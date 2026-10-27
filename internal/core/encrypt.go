package core

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/pack"
	"github.com/shenyizg/veilfold/internal/seal"
)

// Encrypt mode turns the folder into one encrypted archive in the store:
//
//	lock:   scan → record pending(seal/stage) → rename path → store/<id>.staging
//	        → record pending(seal/write) → archive+encrypt → <id>.veil.partial
//	        → decrypt and verify, re-scan the staging folder for changes
//	        → rename to <id>.veil → record locked + pending(seal/cleanup)
//	        → delete the staging folder → record done
//	        (any failure: record pending(seal/abort) → put the folder back)
//
//	unlock: record pending(unseal/extract) → decrypt+extract → <parent>/.veilfold-<id>.restore
//	        → marker file → record pending(unseal/rename) → rename → target
//	        → record unlocked + pending(unseal/cleanup) → remove marker, apply root metadata,
//	          full sync → delete <id>.veil → record done
//
// At every moment at least one complete copy of the folder exists: the
// plaintext is only deleted once the encrypted copy has been written,
// flushed and verified, and the encrypted copy only once the folder is back
// in place and flushed.

const (
	blobMagic      = "VEIL"
	blobVersion    = 1
	blobHeaderSize = 4 + 1 + 3 + 16 + 32
)

// removeAll deletes staging folders; tests replace it to simulate failures.
var removeAll = fsx.RemoveAll

// newBlobHeader returns a header binding a blob to its item, with a fresh
// salt for the blob's key.
func newBlobHeader(id string) ([]byte, error) {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		return nil, fmt.Errorf("invalid item id %q", id)
	}
	h := make([]byte, blobHeaderSize)
	copy(h, blobMagic)
	h[4] = blobVersion
	copy(h[8:24], raw)
	copy(h[24:], seal.Random(32))
	return h, nil
}

func checkBlobHeader(h []byte, id string) error {
	if len(h) != blobHeaderSize || string(h[:4]) != blobMagic || h[4] != blobVersion {
		return errors.New("not a Veilfold encrypted folder, or an unsupported version")
	}
	if hex.EncodeToString(h[8:24]) != id {
		return errors.New("encrypted folder belongs to a different item")
	}
	return nil
}

func blobSalt(h []byte) string { return hex.EncodeToString(h[24:56]) }

// readBlobSalt returns the salt of the blob at path, or "" if it cannot be
// read.
func readBlobSalt(path, id string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := make([]byte, blobHeaderSize)
	if _, err := io.ReadFull(f, h); err != nil || checkBlobHeader(h, id) != nil {
		return ""
	}
	return blobSalt(h)
}

// blobKey derives the key of one encrypted archive. Every lock uses a new
// salt and therefore a new key.
func (s *Session) blobKey(h []byte) []byte {
	return seal.SubKey(s.mk, h[24:56], "veilfold/blob/v1\x00"+hex.EncodeToString(h[8:24]))
}

// openBlob opens an encrypted folder for reading. If salt is not empty the
// blob must be the one created with that salt.
func (s *Session) openBlob(path, id, salt string) (*os.File, io.Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	h := make([]byte, blobHeaderSize)
	if _, err := io.ReadFull(f, h); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := checkBlobHeader(h, id); err != nil {
		f.Close()
		return nil, nil, err
	}
	if salt != "" && blobSalt(h) != salt {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not the encrypted copy Veilfold made when the folder was last locked (it was replaced); refusing to use it", path)
	}
	key := s.blobKey(h)
	defer seal.Wipe(key)
	r, err := seal.NewReader(f, key, h)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, r, nil
}

// nextStoreError means a store could not be used before anything changed,
// so the next candidate store may be tried.
type nextStoreError struct{ err error }

func (e *nextStoreError) Error() string { return e.err.Error() }
func (e *nextStoreError) Unwrap() error { return e.err }

func (s *Session) encrypt(ctx context.Context, m *manifest, r *record, progress ProgressFunc) error {
	report(progress, PhaseScan, 0, 0)
	scan, err := pack.Scan(ctx, r.Path)
	if err != nil {
		return scanError(err)
	}
	var lastErr error
	for _, store := range s.svc.storeCandidates(r.Path) {
		err := s.encryptInto(ctx, m, r, store, scan, progress)
		var next *nextStoreError
		if errors.As(err, &next) {
			lastErr = next.err
			continue
		}
		return err
	}
	return fmt.Errorf("no hidden store can be created on the drive of %s: %w", r.Path, lastErr)
}

func (s *Session) encryptInto(ctx context.Context, m *manifest, r *record, store string, scan pack.Stats, progress ProgressFunc) error {
	created, err := s.svc.ensureStore(store)
	if err != nil {
		return &nextStoreError{err}
	}
	if free, err := fsx.FreeSpace(store); err == nil && free < requiredSpace(scan.Bytes) {
		s.svc.dropStore(store, created)
		return fmt.Errorf("%w: encrypting %s needs about %s but only %s is free (hiding it needs no extra space)",
			ErrInsufficientSpace, r.name(), HumanBytes(int64(requiredSpace(scan.Bytes))), HumanBytes(int64(free)))
	}
	stage := filepath.Join(store, r.ID+stageExt)
	for _, p := range []string{stage, filepath.Join(store, r.ID), filepath.Join(store, r.ID+blobExt)} {
		if fsx.Exists(p) {
			return fmt.Errorf("unexpected leftover data at %s; refusing to overwrite it", p)
		}
	}

	r.Pending = &pending{Phase: phaseSealStage, StoreDir: store, Stage: stage, Size: scan.Bytes, Files: scan.Files}
	if err := s.saveManifest(m); err != nil {
		r.Pending = nil
		s.svc.dropStore(store, created)
		return err
	}
	failpoint("encrypt:before-stage")
	if err := fsx.Rename(r.Path, stage); err != nil {
		r.Pending = nil
		serr := s.saveManifest(m)
		s.svc.dropStore(store, created)
		if fsx.IsCrossDevice(err) && serr == nil {
			return &nextStoreError{err}
		}
		return errors.Join(renameError(err, r.Path), serr)
	}
	// From here on the folder is out of sight. Every failure puts it back.
	failpoint("encrypt:after-stage")
	r.Pending.Phase = phaseSealWrite
	if err := s.saveManifest(m); err != nil {
		return s.abortEncrypt(m, r, err)
	}
	stats, salt, err := s.sealStage(ctx, r, store, stage, scan, progress)
	if err != nil {
		return s.abortEncrypt(m, r, err)
	}
	failpoint("encrypt:sealed")

	// Commit. Keep a deep copy so that a failed save can be undone exactly.
	before, pend := *r, *r.Pending
	before.Pending = &pend
	r.Locked, r.Storage, r.StoreDir, r.Conceal, r.Concealed = true, StorageSealed, store, 0, false
	r.Size, r.Files, r.LockedAt, r.BlobSalt = stats.Bytes, stats.Files, time.Now().UTC(), salt
	r.Pending.Phase = phaseSealCleanup
	if err := s.saveManifest(m); err != nil {
		*r = before
		return s.abortEncrypt(m, r, err)
	}
	failpoint("encrypt:committed")
	report(progress, PhaseCleanup, 0, 0)
	return s.cleanupEncrypt(m, r)
}

// cleanupEncrypt deletes the unencrypted staging folder of a committed lock.
// It refuses unless the record says the folder is sealed and the encrypted
// copy exists.
func (s *Session) cleanupEncrypt(m *manifest, r *record) error {
	p := r.Pending
	blob := filepath.Join(r.StoreDir, r.ID+blobExt)
	if !r.Locked || r.Storage != StorageSealed || !fsx.Exists(blob) {
		return fmt.Errorf("%w: refusing to remove %s without an encrypted copy", ErrUnresolved, p.Stage)
	}
	if err := removeAll(p.Stage); err != nil {
		return fmt.Errorf("%w: removing the unencrypted copy at %s failed (%v); this is retried the next time the vault is opened",
			ErrIncomplete, p.Stage, err)
	}
	r.Pending = nil
	return s.saveManifest(m)
}

// errChangedDuringLock means programs wrote to the folder while it was
// being encrypted.
var errChangedDuringLock = fmt.Errorf("%w: files in the folder changed while it was being locked; close the programs that use it and try again", pack.ErrChanged)

// sealStage archives and encrypts the staged folder and verifies the result
// before giving it its final name. It returns the blob's salt.
func (s *Session) sealStage(ctx context.Context, r *record, store, stage string, scan pack.Stats, progress ProgressFunc) (pack.Stats, string, error) {
	partial := filepath.Join(store, r.ID+blobExt+partialExt)
	final := filepath.Join(store, r.ID+blobExt)
	h, err := newBlobHeader(r.ID)
	if err != nil {
		return pack.Stats{}, "", err
	}
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return pack.Stats{}, "", err
	}
	stats, err := func() (pack.Stats, error) {
		if _, err := f.Write(h); err != nil {
			return pack.Stats{}, err
		}
		key := s.blobKey(h)
		defer seal.Wipe(key)
		w, err := seal.NewWriter(f, key, h)
		if err != nil {
			return pack.Stats{}, err
		}
		meta := pack.Meta{Item: r.ID, Source: r.Path, Created: time.Now().UTC()}
		stats, err := pack.Write(ctx, w, stage, meta, func(done int64) {
			report(progress, PhaseEncrypt, done, scan.Bytes)
		})
		if err != nil {
			return stats, scanError(err)
		}
		if err := w.Close(); err != nil {
			return stats, err
		}
		return stats, f.Sync()
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(partial)
		return stats, "", err
	}
	failpoint("encrypt:written")
	if err := s.verifyBlob(ctx, partial, r.ID, blobSalt(h), stats, progress); err != nil {
		_ = os.Remove(partial)
		return stats, "", fmt.Errorf("verifying the encrypted copy failed: %w", err)
	}
	// Programs that still had files open, or a working directory inside
	// the folder, can have written to it after it was read. Compare it
	// with what was archived before the archive becomes authoritative.
	if now, err := pack.Scan(ctx, stage); err != nil || now.Digest != stats.Digest {
		_ = os.Remove(partial)
		if err != nil {
			return stats, "", scanError(err)
		}
		return stats, "", errChangedDuringLock
	}
	if err := fsx.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return stats, "", err
	}
	if err := fsx.SyncDir(store); err != nil {
		_ = os.Remove(final)
		return stats, "", err
	}
	return stats, blobSalt(h), nil
}

// verifyBlob decrypts a whole archive and checks that it describes exactly
// what was written.
func (s *Session) verifyBlob(ctx context.Context, path, id, salt string, want pack.Stats, progress ProgressFunc) error {
	f, rd, err := s.openBlob(path, id, salt)
	if err != nil {
		return err
	}
	defer f.Close()
	res, err := pack.Verify(ctx, rd, func(done int64) { report(progress, PhaseVerify, done, want.Bytes) })
	if err != nil {
		return err
	}
	if res.Stats != want || res.Meta.Item != id {
		return fmt.Errorf("archive contents differ from the folder (%+v, want %+v)", res.Stats, want)
	}
	return nil
}

// isLockPhase reports whether a pending phase belongs to a lock that has
// not been committed, which is always undone rather than completed.
func isLockPhase(phase string) bool {
	return phase == phaseSealStage || phase == phaseSealWrite || phase == phaseSealAbort
}

// abortEncrypt rolls a failed lock back and returns cause. The abort is
// recorded first, so that an interruption from here on is never completed
// as a lock: the staging folder may hold changes the archive lacks.
func (s *Session) abortEncrypt(m *manifest, r *record, cause error) error {
	r.Pending.Phase = phaseSealAbort
	_ = s.saveManifest(m)
	stage := r.Pending.Stage
	if err := s.rollbackEncrypt(r); err != nil {
		_ = s.saveManifest(m)
		return fmt.Errorf("%w; moving the folder back also failed (%v). It is safe at %s: open the vault again to retry, or restore it to another location",
			cause, err, stage)
	}
	return errors.Join(cause, s.saveManifest(m))
}

// rollbackEncrypt undoes an uncommitted lock. The staged folder goes back
// to its location or, if something else took that location meanwhile, is
// kept as a hidden folder. It never deletes the staged folder, and removes
// the encrypted copy only once the staged folder is safely in its place.
// On error r.Pending is left as it is.
func (s *Session) rollbackEncrypt(r *record) error {
	p := r.Pending
	final := filepath.Join(p.StoreDir, r.ID+blobExt)
	_ = os.Remove(filepath.Join(p.StoreDir, r.ID+blobExt+partialExt)) // incomplete, never needed
	if fsx.Exists(p.Stage) {
		if !fsx.Exists(r.Path) {
			if err := fsx.Rename(p.Stage, r.Path); err != nil {
				return err
			}
			r.Locked, r.Storage, r.StoreDir, r.BlobSalt = false, "", "", ""
		} else {
			dst := filepath.Join(p.StoreDir, r.ID)
			if fsx.Exists(dst) {
				return fmt.Errorf("unexpected leftover data at %s", dst)
			}
			state, err := fsx.ConcealState(p.Stage)
			if err != nil {
				return err
			}
			if err := fsx.Rename(p.Stage, dst); err != nil {
				return err
			}
			r.Concealed = fsx.Conceal(dst, state) == nil
			r.Locked, r.Storage, r.StoreDir, r.Conceal, r.BlobSalt = true, StorageHidden, p.StoreDir, state, ""
			r.LockedAt = time.Now().UTC()
		}
		_ = os.Remove(final) // the folder is safe in its place now
		r.Pending = nil
		return nil
	}
	// The staging folder is gone: work out where the data is, and never
	// delete anything.
	switch {
	case p.Target != "" && fsx.Exists(p.Target):
		r.Path, r.Locked, r.Storage, r.StoreDir, r.BlobSalt = p.Target, false, "", "", ""
	case fsx.Exists(r.Path):
		r.Locked, r.Storage, r.StoreDir, r.BlobSalt = false, "", "", ""
	case fsx.Exists(final):
		r.Locked, r.Storage, r.StoreDir = true, StorageSealed, p.StoreDir
		r.BlobSalt, r.LockedAt = readBlobSalt(final, r.ID), time.Now().UTC()
	default:
		return fmt.Errorf("%w: %s", ErrDataMissing, p.Stage)
	}
	r.Pending = nil
	return nil
}

// restoreStageTo moves the staging folder of a lock that could not be
// undone (typically because its original location disappeared) to target.
func (s *Session) restoreStageTo(m *manifest, r *record, target string) error {
	if err := s.checkTarget(m, r, target); err != nil {
		return err
	}
	p := r.Pending
	p.Target = target
	if err := s.saveManifest(m); err != nil {
		return err
	}
	if err := fsx.Rename(p.Stage, target); err != nil {
		return renameError(err, target)
	}
	_ = os.Remove(filepath.Join(p.StoreDir, r.ID+blobExt+partialExt))
	_ = os.Remove(filepath.Join(p.StoreDir, r.ID+blobExt))
	r.Path, r.Locked, r.Storage, r.StoreDir, r.BlobSalt, r.Pending = target, false, "", "", "", nil
	return s.saveManifest(m)
}

func markerName(id string) string { return ".veilfold-" + id + ".restoring" }

func (s *Session) decrypt(ctx context.Context, m *manifest, r *record, target string, progress ProgressFunc) error {
	blob := r.dataPath()
	bi, err := os.Stat(blob)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrDataMissing, blob)
	}
	if err != nil {
		return err
	}
	parent := filepath.Dir(target)
	if free, err := fsx.FreeSpace(parent); err == nil && free < requiredSpace(bi.Size()) {
		return fmt.Errorf("%w: unlocking %s needs about %s but only %s is free",
			ErrInsufficientSpace, r.name(), HumanBytes(int64(requiredSpace(bi.Size()))), HumanBytes(int64(free)))
	}
	// The encrypted copy stays intact until the very end, so a leftover
	// restore folder from an earlier attempt holds nothing unique.
	stage := filepath.Join(parent, ".veilfold-"+r.ID+restoreExt)
	if err := removeAll(stage); err != nil {
		return err
	}

	r.Pending = &pending{Phase: phaseUnsealExtract, Target: target, Stage: stage}
	if err := s.saveManifest(m); err != nil {
		r.Pending = nil
		return err
	}
	// fail undoes a failed unlock. The partial plaintext copy stays tracked
	// in the index until it has really been deleted.
	fail := func(cause error) error {
		if err := removeAll(stage); err != nil {
			r.Pending.Phase = phaseUnsealExtract
			_ = s.saveManifest(m)
			return fmt.Errorf("%w; the partly restored copy at %s could not be deleted (%v) and is deleted the next time the vault is opened",
				cause, stage, err)
		}
		r.Pending = nil
		return errors.Join(cause, s.saveManifest(m))
	}
	f, rd, err := s.openBlob(blob, r.ID, r.BlobSalt)
	if err != nil {
		return fail(err)
	}
	res, err := pack.Extract(ctx, rd, stage, pack.ExtractOptions{
		Sync:     true,
		Progress: func(done int64) { report(progress, PhaseDecrypt, done, r.Size) },
	})
	f.Close()
	if err != nil {
		return fail(fmt.Errorf("decrypting %s failed: %w", r.name(), err))
	}
	failpoint("decrypt:extracted")
	marker := filepath.Join(stage, markerName(r.ID))
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return fail(err)
	}
	if err := fsx.SyncDir(stage); err != nil {
		return fail(err)
	}
	r.Pending.Phase = phaseUnsealRename
	r.Pending.Root = &res.Root
	if err := s.saveManifest(m); err != nil {
		return fail(err)
	}
	failpoint("decrypt:before-rename")
	if err := fsx.Rename(stage, target); err != nil {
		return fail(renameError(err, target))
	}
	_ = fsx.SyncDir(parent)
	failpoint("decrypt:renamed")
	return s.finishDecrypt(m, r, target)
}

// finishDecrypt records that the decrypted folder is in place, then cleans
// up.
func (s *Session) finishDecrypt(m *manifest, r *record, target string) error {
	r.Locked, r.Storage, r.Path = false, "", target
	r.Pending.Phase = phaseUnsealCleanup
	r.Pending.Stage = ""
	if err := s.saveManifest(m); err != nil {
		return fmt.Errorf("%w: the folder is back, but %v; this is repaired the next time the vault is opened", ErrIncomplete, err)
	}
	failpoint("decrypt:committed")
	return s.cleanupDecrypt(m, r)
}

// cleanupDecrypt removes the restore marker, applies the root folder's
// metadata and, once everything is flushed, deletes the encrypted copy.
func (s *Session) cleanupDecrypt(m *manifest, r *record) error {
	p := r.Pending
	_ = os.Remove(filepath.Join(r.Path, markerName(r.ID)))
	if p.Root != nil {
		_ = p.Root.Apply(r.Path)
	}
	blob := filepath.Join(r.StoreDir, r.ID+blobExt)
	if fsx.Exists(blob) {
		// A full flush (F_FULLFSYNC on macOS) before the only other copy
		// of the data goes away.
		if err := fsx.SyncDir(r.Path); err != nil {
			return fmt.Errorf("%w: flushing %s failed (%v); the encrypted copy was kept", ErrIncomplete, r.Path, err)
		}
		if err := os.Remove(blob); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: removing the encrypted copy failed (%v); this is retried the next time the vault is opened",
				ErrIncomplete, err)
		}
		_ = fsx.SyncDir(r.StoreDir)
	}
	r.StoreDir, r.BlobSalt, r.Pending = "", "", nil
	return s.saveManifest(m)
}
