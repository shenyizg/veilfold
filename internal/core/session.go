package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/pack"
	"github.com/shenyizg/veilfold/internal/seal"
)

// Phase names the step a long-running operation is in.
type Phase string

// Phases reported through ProgressFunc.
const (
	PhaseScan    Phase = "scan"
	PhaseEncrypt Phase = "encrypt"
	PhaseVerify  Phase = "verify"
	PhaseDecrypt Phase = "decrypt"
	PhaseCleanup Phase = "cleanup"
)

// Progress reports how far an operation got. Done and Total count bytes of
// file content; Total is 0 when unknown.
type Progress struct {
	Phase Phase `json:"phase"`
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// ProgressFunc receives progress reports. It may be nil.
type ProgressFunc func(Progress)

func report(fn ProgressFunc, phase Phase, done, total int64) {
	if fn != nil {
		fn(Progress{Phase: phase, Done: done, Total: total})
	}
}

// failpoint is called at the critical points of every operation. Tests
// replace it to simulate a crash at that exact point.
var failpoint = func(string) {}

// Session is an opened vault. It is safe for concurrent use; operations
// that change folders are serialized across processes.
type Session struct {
	svc         *Service
	id          string
	dir         string
	viaRecovery bool
	notes       []string

	mu sync.RWMutex
	mk []byte // nil once closed
}

// VaultID returns the vault's identifier.
func (s *Session) VaultID() string { return s.id }

// Notes describes repairs of interrupted operations done while opening.
func (s *Session) Notes() []string { return s.notes }

// OpenedWithRecoveryKey reports whether the session was opened with the
// vault's recovery key rather than its password.
func (s *Session) OpenedWithRecoveryKey() bool { return s.viaRecovery }

// Close forgets the vault's key. Locked folders stay locked and unlocked
// folders stay unlocked.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	seal.Wipe(s.mk)
	s.mk = nil
}

// read runs fn with the current index without taking the cross-process lock.
func (s *Session) read(fn func(m *manifest) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mk == nil {
		return ErrClosed
	}
	m, err := s.loadManifest()
	if err != nil {
		return err
	}
	return fn(m)
}

// update runs fn with exclusive access to the vault's index and stores.
func (s *Session) update(fn func(m *manifest) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mk == nil {
		return ErrClosed
	}
	l, err := s.svc.lock()
	if err != nil {
		return err
	}
	defer l.Unlock()
	m, err := s.loadManifest()
	if err != nil {
		return err
	}
	return fn(m)
}

// Items lists the protected folders.
func (s *Session) Items() ([]Item, error) {
	var items []Item
	err := s.read(func(m *manifest) error {
		items = make([]Item, 0, len(m.Items))
		for _, r := range m.Items {
			items = append(items, r.view())
		}
		return nil
	})
	return items, err
}

// Find looks up a protected folder by ID, unique ID prefix, path, or unique
// folder name.
func (s *Session) Find(ref string) (Item, error) {
	items, err := s.Items()
	if err != nil {
		return Item{}, err
	}
	for _, it := range items {
		if it.ID == ref {
			return it, nil
		}
	}
	if abs, err := absPath(ref); err == nil {
		for _, it := range items {
			if samePath(it.Path, abs) {
				return it, nil
			}
		}
	}
	unique := func(match func(Item) bool) (Item, bool, error) {
		var found []Item
		for _, it := range items {
			if match(it) {
				found = append(found, it)
			}
		}
		switch len(found) {
		case 0:
			return Item{}, false, nil
		case 1:
			return found[0], true, nil
		}
		return Item{}, false, fmt.Errorf("%w: %q", ErrAmbiguous, ref)
	}
	if len(ref) >= 4 {
		lower := strings.ToLower(ref)
		if it, ok, err := unique(func(it Item) bool { return strings.HasPrefix(it.ID, lower) }); ok || err != nil {
			return it, err
		}
	}
	if it, ok, err := unique(func(it Item) bool { return strings.EqualFold(it.Name, ref) }); ok || err != nil {
		return it, err
	}
	return Item{}, fmt.Errorf("%w: %q", ErrNotFound, ref)
}

// checkNew validates that path can be protected by this vault.
func (s *Session) checkNew(m *manifest, path, self string) error {
	if err := s.svc.checkPath(path); err != nil {
		return err
	}
	return checkOverlap(m, path, self)
}

func checkOverlap(m *manifest, path, self string) error {
	real := resolve(path)
	for _, r := range m.Items {
		if r.ID == self {
			continue
		}
		other := resolve(r.Path)
		if within(real, other) || within(other, real) {
			return notAllowed(path, fmt.Sprintf("it overlaps with the protected folder %s", r.Path))
		}
	}
	return nil
}

// checkTarget validates where a locked folder is to be restored.
func (s *Session) checkTarget(m *manifest, r *record, target string) error {
	if fsx.Exists(target) {
		return fmt.Errorf("%w: %s", ErrTargetExists, target)
	}
	parent := filepath.Dir(target)
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: %s", ErrParentMissing, parent)
	}
	if samePath(target, r.Path) {
		return nil
	}
	if within(resolve(target), resolve(s.svc.dataDir)) {
		return notAllowed(target, "it belongs to Veilfold")
	}
	return checkOverlap(m, target, r.ID)
}

// Inspection describes a folder that is about to be protected.
type Inspection struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Error explains why the folder cannot be protected at all.
	Error string `json:"error,omitempty"`
	// EncryptError explains why only hide mode is possible.
	EncryptError string     `json:"encrypt_error,omitempty"`
	Stats        pack.Stats `json:"stats"`
	// Complete is false if counting stopped early (see ctx).
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings,omitempty"`
}

// Inspect checks whether path can be protected and measures it. Measuring
// stops when ctx is done, so callers can bound the time it takes.
func (s *Session) Inspect(ctx context.Context, path string) (Inspection, error) {
	abs, err := absPath(path)
	if err != nil {
		return Inspection{}, err
	}
	in := Inspection{Path: abs, Name: filepath.Base(abs)}
	err = s.read(func(m *manifest) error {
		if err := s.checkNew(m, abs, ""); err != nil {
			in.Error = err.Error()
		}
		return nil
	})
	if err != nil || in.Error != "" {
		return in, err
	}
	in.Stats, err = pack.Scan(ctx, abs)
	switch {
	case err == nil:
		in.Complete = true
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
	default:
		in.EncryptError = scanError(err).Error()
	}
	in.Warnings = warnings(abs, in.Stats)
	return in, nil
}

// Add registers a folder for protection without locking it.
func (s *Session) Add(path string, mode Mode) (Item, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return Item{}, err
	}
	abs, err := absPath(path)
	if err != nil {
		return Item{}, err
	}
	var it Item
	err = s.update(func(m *manifest) error {
		if err := s.checkNew(m, abs, ""); err != nil {
			return err
		}
		r := &record{ID: newID(), Path: abs, Mode: mode, AddedAt: time.Now().UTC()}
		m.Items = append(m.Items, r)
		if err := s.saveManifest(m); err != nil {
			return err
		}
		it = r.view()
		return nil
	})
	return it, err
}

// SetMode changes how an unlocked folder will be protected the next time
// it is locked.
func (s *Session) SetMode(id string, mode Mode) error {
	if _, err := ParseMode(string(mode)); err != nil {
		return err
	}
	return s.update(func(m *manifest) error {
		r := m.find(id)
		if r == nil {
			return ErrNotFound
		}
		if r.Locked {
			return errors.New("unlock the folder before changing how it is protected")
		}
		r.Mode = mode
		return s.saveManifest(m)
	})
}

// Lock locks a protected folder: in encrypt mode its contents are
// encrypted into the store, in hide mode it is moved into the store. In
// both cases nothing remains at the original location. Locking a locked
// folder does nothing.
func (s *Session) Lock(ctx context.Context, id string, progress ProgressFunc) error {
	return s.update(func(m *manifest) error {
		r := m.find(id)
		if r == nil {
			return ErrNotFound
		}
		if err := s.settle(m, r); err != nil {
			return err
		}
		if r.Locked {
			return nil
		}
		if err := s.checkNew(m, r.Path, r.ID); err != nil {
			return err
		}
		if r.Mode == ModeHide {
			return s.hide(m, r)
		}
		return s.encrypt(ctx, m, r, progress)
	})
}

// UnlockOptions configures Unlock.
type UnlockOptions struct {
	// Target restores the folder to this path instead of its original
	// location. The folder is then protected at the new location.
	Target string
}

// Unlock restores a locked folder. Unlocking an unlocked folder does
// nothing.
func (s *Session) Unlock(ctx context.Context, id string, opts UnlockOptions, progress ProgressFunc) error {
	return s.update(func(m *manifest) error {
		r := m.find(id)
		if r == nil {
			return ErrNotFound
		}
		target := r.Path
		if opts.Target != "" {
			t, err := absPath(opts.Target)
			if err != nil {
				return err
			}
			target = t
		}
		if err := s.settle(m, r); err != nil {
			// A lock that could not be undone (say, because the folder's
			// original location disappeared) can still be restored to
			// another location.
			if opts.Target != "" && r.Pending != nil && isLockPhase(r.Pending.Phase) && fsx.Exists(r.Pending.Stage) {
				return s.restoreStageTo(m, r, target)
			}
			return err
		}
		if !r.Locked {
			if opts.Target != "" {
				return ErrNotLocked
			}
			return nil
		}
		return s.unlock(ctx, m, r, target, progress)
	})
}

func (s *Session) unlock(ctx context.Context, m *manifest, r *record, target string, progress ProgressFunc) error {
	if err := s.checkTarget(m, r, target); err != nil {
		return err
	}
	switch r.Storage {
	case StorageHidden:
		return s.unhide(m, r, target)
	case StorageSealed:
		return s.decrypt(ctx, m, r, target, progress)
	}
	return fmt.Errorf("unknown storage %q", r.Storage)
}

// RemoveOptions configures Remove.
type RemoveOptions struct {
	// Forget drops a locked folder whose data can no longer be found
	// instead of failing. It never discards data that still exists.
	Forget bool
}

// Remove stops protecting a folder. A locked folder is unlocked first.
func (s *Session) Remove(ctx context.Context, id string, opts RemoveOptions, progress ProgressFunc) error {
	return s.update(func(m *manifest) error {
		r := m.find(id)
		if r == nil {
			return ErrNotFound
		}
		if err := s.settle(m, r); err != nil {
			return err
		}
		if r.Locked {
			if opts.Forget && !fsx.Exists(r.dataPath()) {
				m.remove(r.ID)
				return s.saveManifest(m)
			}
			if err := s.unlock(ctx, m, r, r.Path, progress); err != nil {
				return err
			}
		}
		m.remove(r.ID)
		return s.saveManifest(m)
	})
}

// ChangePassword replaces the vault's password.
func (s *Session) ChangePassword(current, next []byte) error {
	return s.rewrapPassword(current, next)
}

// ResetPassword sets a new password without knowing the old one. It is
// only available after opening the vault with its recovery key.
func (s *Session) ResetPassword(next []byte) error {
	if !s.viaRecovery {
		return ErrRecoveryOnly
	}
	return s.rewrapPassword(nil, next)
}

func (s *Session) rewrapPassword(current, next []byte) error {
	if len(next) == 0 {
		return errors.New("the password must not be empty")
	}
	return s.update(func(*manifest) error {
		hdr, err := readHeader(s.dir)
		if err != nil {
			return err
		}
		if current != nil {
			mk, err := hdr.unwrapPassword(current)
			if err != nil {
				return err
			}
			seal.Wipe(mk)
		}
		vs, err := s.svc.vaults()
		if err != nil {
			return err
		}
		others := vs[:0]
		for _, v := range vs {
			if v.hdr.ID != s.id {
				others = append(others, v)
			}
		}
		if _, mk, err := s.svc.match(others, next); err == nil {
			seal.Wipe(mk)
			return ErrPasswordInUse
		}
		if hdr.Password, err = newPasswordSlot(s.id, s.mk, next, s.svc.kdf); err != nil {
			return err
		}
		return writeHeader(s.dir, hdr)
	})
}

// HasRecoveryKey reports whether the vault has a recovery key.
func (s *Session) HasRecoveryKey() (bool, error) {
	hdr, err := readHeader(s.dir)
	if err != nil {
		return false, err
	}
	return hdr.Recovery != nil, nil
}

// NewRecoveryKey replaces the vault's recovery key (or creates one) and
// returns it. The previous recovery key stops working.
func (s *Session) NewRecoveryKey(password []byte) (string, error) {
	var text string
	err := s.update(func(*manifest) error {
		hdr, err := readHeader(s.dir)
		if err != nil {
			return err
		}
		if !s.viaRecovery {
			mk, err := hdr.unwrapPassword(password)
			if err != nil {
				return err
			}
			seal.Wipe(mk)
		}
		slot, t, err := newRecoverySlot(s.id, s.mk)
		if err != nil {
			return err
		}
		hdr.Recovery, text = &slot, t
		return writeHeader(s.dir, hdr)
	})
	return text, err
}

// Destroy deletes the vault. It refuses while the vault protects folders.
func (s *Session) Destroy() error {
	err := s.update(func(m *manifest) error {
		if len(m.Items) > 0 {
			return ErrNotEmpty
		}
		return fsx.RemoveAll(s.dir)
	})
	if err == nil {
		s.Close()
	}
	return err
}

// storeCandidates lists stores on the same drive as path, best first.
func (s *Service) storeCandidates(path string) []string {
	parent := filepath.Dir(path)
	var out []string
	add := func(p string) {
		for _, e := range out {
			if samePath(e, p) {
				return
			}
		}
		out = append(out, p)
	}
	if pv, err := fsx.VolumeID(parent); err == nil {
		if dv, err := fsx.VolumeID(s.dataDir); err == nil && pv == dv {
			add(s.StoreDir())
		}
	}
	if mp, err := fsx.MountPoint(parent); err == nil {
		add(filepath.Join(mp, volumeStoreName, fsx.UserTag()))
	}
	add(filepath.Join(parent, volumeStoreName, fsx.UserTag()))
	return out
}

// ensureStore creates a store directory if needed and reports whether it
// did. Existing directories are only used if they are real directories
// owned by the current user (see fsx.CheckPrivateDir), so nobody can plant
// a directory or link where Veilfold would put the user's data.
func (s *Service) ensureStore(store string) (bool, error) {
	created := false
	if _, err := os.Lstat(store); errors.Is(err, fs.ErrNotExist) {
		shared := filepath.Dir(store)
		if filepath.Base(shared) == volumeStoreName {
			// Every account on the computer gets its own directory below a
			// shared one which, like /tmp, is world-writable with the
			// sticky bit.
			if !fsx.Exists(shared) {
				if err := os.Mkdir(shared, 0o700); err != nil {
					return false, err
				}
				_ = os.Chmod(shared, 0o777|os.ModeSticky)
				_ = fsx.HideDir(shared)
			}
			if err := fsx.CheckSharedDir(shared); err != nil {
				return false, err
			}
			if err := os.Mkdir(store, 0o700); err != nil {
				return false, err
			}
		} else if err := os.MkdirAll(store, 0o700); err != nil {
			return false, err
		}
		created = true
		_ = fsx.HideDir(store)
	} else if err != nil {
		return false, err
	}
	if err := fsx.CheckPrivateDir(store); err != nil {
		return created, fmt.Errorf("cannot use %s as a store: %w", store, err)
	}
	return created, nil
}

// dropStore removes a store directory created by ensureStore if it is
// still empty.
func (s *Service) dropStore(store string, created bool) {
	if !created || samePath(store, s.StoreDir()) {
		return
	}
	if os.Remove(store) == nil && filepath.Base(filepath.Dir(store)) == volumeStoreName {
		_ = os.Remove(filepath.Dir(store))
	}
}

// requiredSpace estimates the free space needed to store n bytes of data
// in a new copy, with a safety margin.
func requiredSpace(n int64) uint64 {
	return uint64(n) + uint64(n)/100 + 64<<20
}
