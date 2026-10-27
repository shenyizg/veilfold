package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/shenyizg/veilfold/internal/fsx"
)

// Hide mode moves the folder into a store on the same drive with a single
// atomic rename and conceals it there. It is instant and preserves the
// folder exactly, but does not encrypt it.
//
//	hide:   record pending(hide/move) → rename path → store/<id> → conceal → record locked
//	unhide: record pending(unhide/move) → reveal → rename store/<id> → target → record unlocked

func (s *Session) hide(m *manifest, r *record) error {
	state, err := fsx.ConcealState(r.Path)
	if err != nil {
		return err
	}
	var lastErr error
	for _, store := range s.svc.storeCandidates(r.Path) {
		created, err := s.svc.ensureStore(store)
		if err != nil {
			lastErr = err
			continue
		}
		dst := filepath.Join(store, r.ID)
		if fsx.Exists(dst) {
			return fmt.Errorf("unexpected leftover data at %s; refusing to overwrite it", dst)
		}
		r.Pending = &pending{Phase: phaseHideMove, StoreDir: store, Conceal: state}
		if err := s.saveManifest(m); err != nil {
			r.Pending = nil
			s.svc.dropStore(store, created)
			return err
		}
		failpoint("hide:before-rename")
		if err := fsx.Rename(r.Path, dst); err != nil {
			r.Pending = nil
			serr := s.saveManifest(m)
			s.svc.dropStore(store, created)
			if fsx.IsCrossDevice(err) && serr == nil {
				lastErr = err
				continue
			}
			return errors.Join(renameError(err, r.Path), serr)
		}
		failpoint("hide:after-rename")
		r.Concealed = fsx.Conceal(dst, state) == nil
		r.Locked, r.Storage, r.StoreDir, r.Conceal = true, StorageHidden, store, state
		r.Size, r.Files, r.LockedAt, r.Pending = 0, 0, time.Now().UTC(), nil
		if err := s.saveManifest(m); err != nil {
			return fmt.Errorf("%w: the folder is hidden, but %v; this is repaired the next time the vault is opened", ErrIncomplete, err)
		}
		return nil
	}
	return fmt.Errorf("no hidden store can be created on the drive of %s: %w", r.Path, lastErr)
}

func (s *Session) unhide(m *manifest, r *record, target string) error {
	src := r.dataPath()
	if !fsx.Exists(src) {
		return fmt.Errorf("%w: %s", ErrDataMissing, src)
	}
	if sv, err := fsx.VolumeID(r.StoreDir); err == nil {
		if tv, err := fsx.VolumeID(filepath.Dir(target)); err == nil && sv != tv {
			return ErrDifferentVolume
		}
	}
	r.Pending = &pending{Phase: phaseUnhideMove, Target: target}
	if err := s.saveManifest(m); err != nil {
		r.Pending = nil
		return err
	}
	if r.Concealed {
		if err := fsx.Reveal(src, r.Conceal); err != nil {
			r.Pending = nil
			return errors.Join(err, s.saveManifest(m))
		}
	}
	failpoint("unhide:before-rename")
	if err := fsx.Rename(src, target); err != nil {
		if r.Concealed {
			_ = fsx.Conceal(src, r.Conceal)
		}
		r.Pending = nil
		return errors.Join(renameError(err, target), s.saveManifest(m))
	}
	failpoint("unhide:after-rename")
	r.Locked, r.Storage, r.StoreDir, r.Conceal, r.Concealed = false, "", "", 0, false
	r.Path, r.Pending = target, nil
	if err := s.saveManifest(m); err != nil {
		return fmt.Errorf("%w: the folder is back, but %v; this is repaired the next time the vault is opened", ErrIncomplete, err)
	}
	return nil
}
