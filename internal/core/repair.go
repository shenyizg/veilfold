package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shenyizg/veilfold/internal/fsx"
)

// repair completes or undoes operations that were interrupted, for example
// by a crash or power loss, and reports data that was left behind. Every
// multi-step operation records its current phase in the index before
// acting, so the state on disk always tells which steps happened. The rule
// throughout: never delete data unless another complete copy is known to
// exist; when in doubt, keep everything and tell the user.
func (s *Session) repair() []string {
	var notes []string
	err := s.update(func(m *manifest) error {
		for _, r := range m.Items {
			if r.Pending == nil {
				continue
			}
			if note := s.repairRecord(m, r); note != "" {
				notes = append(notes, note)
			}
		}
		notes = append(notes, s.leftovers(m)...)
		return nil
	})
	if err != nil && !errors.Is(err, ErrBusy) {
		notes = append(notes, fmt.Sprintf("Checking for interrupted operations failed: %v", err))
	}
	return notes
}

// settle repairs an interrupted operation on r before a new one starts. It
// fails if the repair could not finish, so that nothing overwrites the
// record of where the folder's data is.
func (s *Session) settle(m *manifest, r *record) error {
	if r.Pending == nil {
		return nil
	}
	note := s.repairRecord(m, r)
	if r.Pending == nil {
		return nil
	}
	if note == "" {
		note = "it did not finish"
	}
	return fmt.Errorf("%w: %s", ErrUnresolved, note)
}

// repairRecord resolves r.Pending and returns a note for the user. If it
// cannot, r.Pending stays set and the note says where the data is.
func (s *Session) repairRecord(m *manifest, r *record) string {
	p := r.Pending
	name := r.name()
	var note string
	switch p.Phase {
	case phaseHideMove:
		dst := filepath.Join(p.StoreDir, r.ID)
		if fsx.Exists(dst) {
			r.Concealed = fsx.Conceal(dst, p.Conceal) == nil
			r.Locked, r.Storage, r.StoreDir, r.Conceal = true, StorageHidden, p.StoreDir, p.Conceal
			r.LockedAt = time.Now().UTC()
			note = fmt.Sprintf("Finished hiding %q, which had been interrupted.", name)
		} else {
			note = fmt.Sprintf("Hiding %q had been interrupted; the folder is still in its place.", name)
		}
		r.Pending = nil

	case phaseUnhideMove:
		src := filepath.Join(r.StoreDir, r.ID)
		switch {
		case fsx.Exists(src):
			if r.Concealed {
				_ = fsx.Conceal(src, r.Conceal)
			}
			note = fmt.Sprintf("Unlocking %q had been interrupted; it is still locked.", name)
		case fsx.Exists(p.Target):
			r.Locked, r.Storage, r.StoreDir, r.Conceal, r.Concealed, r.Path = false, "", "", 0, false, p.Target
			note = fmt.Sprintf("Finished unlocking %q, which had been interrupted.", name)
		default:
			note = fmt.Sprintf("The hidden data of %q could not be found.", name)
		}
		r.Pending = nil

	case phaseSealWrite:
		final := filepath.Join(p.StoreDir, r.ID+blobExt)
		if fsx.Exists(final) && fsx.Exists(p.Stage) {
			// The archive only gets its final name after it was verified
			// and compared with the folder, so the lock can be completed.
			r.Locked, r.Storage, r.StoreDir, r.Conceal, r.Concealed = true, StorageSealed, p.StoreDir, 0, false
			r.Size, r.Files, r.LockedAt = p.Size, p.Files, time.Now().UTC()
			r.BlobSalt = readBlobSalt(final, r.ID)
			p.Phase = phaseSealCleanup
			if err := s.cleanupEncrypt(m, r); err != nil {
				return fmt.Sprintf("%q is locked, but its unencrypted copy at %s could not be deleted: %v", name, p.Stage, err)
			}
			return fmt.Sprintf("Finished locking %q, which had been interrupted.", name)
		}
		note = s.rollbackNote(r)

	case phaseSealStage, phaseSealAbort:
		note = s.rollbackNote(r)

	case phaseSealCleanup:
		blob := filepath.Join(r.StoreDir, r.ID+blobExt)
		if !r.Locked || r.Storage != StorageSealed || !fsx.Exists(blob) {
			// The lock was not committed after all: undo it.
			p.Phase = phaseSealAbort
			note = s.rollbackNote(r)
			break
		}
		if err := s.cleanupEncrypt(m, r); err != nil {
			note = fmt.Sprintf("%q is locked, but its unencrypted copy at %s could not be deleted: %v", name, p.Stage, err)
		}

	case phaseUnsealExtract, phaseUnsealRename:
		marker := filepath.Join(p.Target, markerName(r.ID))
		switch {
		case p.Phase == phaseUnsealRename && !fsx.Exists(p.Stage) && fsx.Exists(marker):
			if err := s.finishDecrypt(m, r, p.Target); err != nil {
				note = fmt.Sprintf("Finishing the unlock of %q failed: %v", name, err)
			} else {
				note = fmt.Sprintf("Finished unlocking %q, which had been interrupted.", name)
			}
		case fsx.Exists(p.Stage):
			if err := removeAll(p.Stage); err != nil {
				note = fmt.Sprintf("Unlocking %q had been interrupted; it is still locked, but the partly restored copy at %s could not be deleted: %v",
					name, p.Stage, err)
				break
			}
			r.Pending = nil
			note = fmt.Sprintf("Unlocking %q had been interrupted; it is still locked.", name)
		default:
			// Nothing to clean up, and we cannot tell what is at the
			// target: keep the encrypted copy.
			r.Pending = nil
			note = fmt.Sprintf("Unlocking %q had been interrupted; it is still locked.", name)
		}

	case phaseUnsealCleanup:
		if err := s.cleanupDecrypt(m, r); err != nil {
			note = fmt.Sprintf("%q is unlocked, but cleaning up failed: %v", name, err)
		}

	default:
		// Written by a newer version: leave it alone.
		note = fmt.Sprintf("%q has an unknown interrupted operation (%q); open it with a newer version of Veilfold.", name, p.Phase)
	}
	if err := s.saveManifest(m); err != nil && note == "" {
		note = err.Error()
	}
	return note
}

// rollbackNote undoes an uncommitted lock and describes the outcome.
func (s *Session) rollbackNote(r *record) string {
	name, stage := r.name(), r.Pending.Stage
	if err := s.rollbackEncrypt(r); err != nil {
		return fmt.Sprintf("Locking %q had been interrupted and it could not be put back (%v). Its contents are safe at %s; "+
			"unlock it to another location to restore it.", name, err, stage)
	}
	if r.Locked && r.Storage == StorageHidden {
		return fmt.Sprintf("Locking %q had been interrupted and its location is now taken, so it was hidden instead.", name)
	}
	if r.Locked {
		return fmt.Sprintf("Locking %q had been interrupted; only its encrypted copy was left, so it stays locked.", name)
	}
	return fmt.Sprintf("Locking %q had been interrupted; the folder is back in its place.", name)
}

// leftovers reports data in stores and next to protected folders that the
// index does not account for, such as an unencrypted staging copy whose
// record was lost. It deletes only incomplete encrypted archives, which
// never hold the only copy of anything; everything else is kept.
func (s *Session) leftovers(m *manifest) []string {
	var notes []string
	for _, r := range m.Items {
		if r.Pending != nil {
			continue
		}
		stores := s.svc.storeCandidates(r.Path)
		if r.StoreDir != "" {
			stores = append(stores, r.StoreDir)
		}
		seen := map[string]bool{}
		report := func(p string) {
			if seen[pathKey(p)] || !fsx.Exists(p) {
				return
			}
			seen[pathKey(p)] = true
			notes = append(notes, fmt.Sprintf("Leftover data of %q was found at %s and left untouched. "+
				"Check it, then delete it once you no longer need it.", r.name(), p))
		}
		for _, store := range stores {
			_ = os.Remove(filepath.Join(store, r.ID+blobExt+partialExt))
			report(filepath.Join(store, r.ID+stageExt))
			inUse := r.Locked && samePath(store, r.StoreDir)
			if !(inUse && r.Storage == StorageSealed) {
				report(filepath.Join(store, r.ID+blobExt))
			}
			if !(inUse && r.Storage == StorageHidden) {
				report(filepath.Join(store, r.ID))
			}
		}
		report(filepath.Join(filepath.Dir(r.Path), ".veilfold-"+r.ID+restoreExt))
	}
	return notes
}
