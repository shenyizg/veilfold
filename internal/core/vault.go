// Package core implements Veilfold: password-protected vaults that lock
// folders away from their original location, either by encrypting them
// (encrypt mode) or by moving them into a concealed store (hide mode).
//
// A data directory can hold several independent vaults. A vault is selected
// by its password alone, so people sharing one computer account can each
// keep their own folders without seeing that the others exist.
package core

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/seal"
)

const (
	headerFormat  = "veilfold-vault"
	headerVersion = 1
	slotPassword  = "password"
	slotRecovery  = "recovery"
)

// vaultHeader is stored unencrypted in vault.json. It contains nothing but
// the master key, wrapped once under the password and optionally once under
// the recovery key.
type vaultHeader struct {
	Format   string   `json:"format"`
	Version  int      `json:"version"`
	ID       string   `json:"id"`
	Password keySlot  `json:"password"`
	Recovery *keySlot `json:"recovery,omitempty"`
}

type keySlot struct {
	KDF     string          `json:"kdf"`
	Params  *seal.KDFParams `json:"params,omitempty"`
	Salt    []byte          `json:"salt"`
	Wrapped []byte          `json:"wrapped_key"`
}

func slotAD(vaultID, slot string) []byte {
	return []byte("veilfold vault key v1\x00" + vaultID + "\x00" + slot)
}

func newPasswordSlot(vaultID string, mk, password []byte, p seal.KDFParams) (keySlot, error) {
	salt := seal.Random(16)
	kek, err := seal.DeriveKey(password, salt, p)
	if err != nil {
		return keySlot{}, err
	}
	defer seal.Wipe(kek)
	wrapped, err := seal.Seal(kek, mk, slotAD(vaultID, slotPassword))
	if err != nil {
		return keySlot{}, err
	}
	return keySlot{KDF: "argon2id", Params: &p, Salt: salt, Wrapped: wrapped}, nil
}

func newRecoverySlot(vaultID string, mk []byte) (keySlot, string, error) {
	key, text := seal.NewRecoveryKey()
	defer seal.Wipe(key)
	salt := seal.Random(32)
	kek := seal.SubKey(key, salt, "veilfold/recovery/v1")
	defer seal.Wipe(kek)
	wrapped, err := seal.Seal(kek, mk, slotAD(vaultID, slotRecovery))
	if err != nil {
		return keySlot{}, "", err
	}
	return keySlot{KDF: "hkdf-sha256", Salt: salt, Wrapped: wrapped}, text, nil
}

func (h *vaultHeader) unwrapPassword(password []byte) ([]byte, error) {
	s := h.Password
	if s.KDF != "argon2id" || s.Params == nil {
		return nil, fmt.Errorf("vault %s: unsupported key derivation %q", h.ID, s.KDF)
	}
	kek, err := seal.DeriveKey(password, s.Salt, *s.Params)
	if err != nil {
		return nil, err
	}
	defer seal.Wipe(kek)
	mk, err := seal.Open(kek, s.Wrapped, slotAD(h.ID, slotPassword))
	if err != nil || len(mk) != seal.KeySize {
		return nil, ErrWrongPassword
	}
	return mk, nil
}

func (h *vaultHeader) unwrapRecovery(key []byte) ([]byte, error) {
	s := h.Recovery
	if s == nil || s.KDF != "hkdf-sha256" {
		return nil, ErrWrongRecoveryKey
	}
	kek := seal.SubKey(key, s.Salt, "veilfold/recovery/v1")
	defer seal.Wipe(kek)
	mk, err := seal.Open(kek, s.Wrapped, slotAD(h.ID, slotRecovery))
	if err != nil || len(mk) != seal.KeySize {
		return nil, ErrWrongRecoveryKey
	}
	return mk, nil
}

func readHeader(dir string) (*vaultHeader, error) {
	data, err := fsx.ReadFile(filepath.Join(dir, headerFileName))
	if err != nil {
		return nil, err
	}
	var h vaultHeader
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if h.Format != headerFormat || h.Version != headerVersion || h.ID != filepath.Base(dir) {
		return nil, fmt.Errorf("%s: not a supported vault", dir)
	}
	return &h, nil
}

func writeHeader(dir string, h *vaultHeader) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(dir, headerFileName), append(data, '\n'), 0o600)
}

func newID() string { return hex.EncodeToString(seal.Random(16)) }

// Options configures a Service.
type Options struct {
	// DataDir holds the vaults and the default store. Empty means
	// DefaultDataDir().
	DataDir string
	// KDF are the Argon2id parameters for new passwords. Nil means
	// seal.DefaultKDF.
	KDF *seal.KDFParams
}

// Service manages the vaults in one data directory.
type Service struct {
	dataDir string
	kdf     seal.KDFParams
}

// NewService returns a Service for opts. It does not touch the disk.
func NewService(opts Options) (*Service, error) {
	dir := opts.DataDir
	if dir == "" {
		d, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	dir, err := absPath(dir)
	if err != nil {
		return nil, err
	}
	kdf := seal.DefaultKDF
	if opts.KDF != nil {
		kdf = *opts.KDF
	}
	if err := kdf.Validate(); err != nil {
		return nil, err
	}
	return &Service{dataDir: dir, kdf: kdf}, nil
}

// DataDir returns the data directory.
func (s *Service) DataDir() string { return s.dataDir }

// StoreDir returns the default store, used for folders on the same drive
// as the data directory.
func (s *Service) StoreDir() string { return filepath.Join(s.dataDir, storeDirName) }

func (s *Service) vaultsDir() string { return filepath.Join(s.dataDir, vaultsDirName) }

func (s *Service) ensureLayout() error {
	if err := os.MkdirAll(s.vaultsDir(), 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(s.dataDir, 0o700)
	}
	return nil
}

// lock takes the data directory's lock, which serializes all changes to
// vaults and stores across processes.
func (s *Service) lock() (*fsx.Lock, error) {
	if err := s.ensureLayout(); err != nil {
		return nil, err
	}
	l, err := fsx.TryLock(filepath.Join(s.dataDir, lockFileName))
	if errors.Is(err, fsx.ErrLocked) {
		return nil, ErrBusy
	}
	return l, err
}

type vaultRef struct {
	dir string
	hdr *vaultHeader
}

func (s *Service) vaults() ([]vaultRef, error) {
	entries, err := os.ReadDir(s.vaultsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []vaultRef
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.vaultsDir(), e.Name())
		if h, err := readHeader(dir); err == nil {
			out = append(out, vaultRef{dir: dir, hdr: h})
		}
	}
	return out, nil
}

// VaultCount returns the number of vaults in the data directory.
func (s *Service) VaultCount() (int, error) {
	vs, err := s.vaults()
	return len(vs), err
}

// match finds the vault that password opens and returns its master key.
// Key derivation is deliberately slow, so vaults are tried in parallel.
func (s *Service) match(vs []vaultRef, password []byte) (vaultRef, []byte, error) {
	keys := make([][]byte, len(vs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range vs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if mk, err := vs[i].hdr.unwrapPassword(password); err == nil {
				keys[i] = mk
			}
		})
	}
	wg.Wait()
	found := -1
	for i, k := range keys {
		if k != nil && found < 0 {
			found = i
		} else if k != nil {
			seal.Wipe(k)
		}
	}
	if found < 0 {
		return vaultRef{}, nil, ErrWrongPassword
	}
	return vs[found], keys[found], nil
}

// Create makes a new vault protected by password. If withRecoveryKey is
// set it also returns a recovery key that can reset the password; it is
// shown to the user once and never stored.
func (s *Service) Create(password []byte, withRecoveryKey bool) (*Session, string, error) {
	if len(password) == 0 {
		return nil, "", errors.New("the password must not be empty")
	}
	l, err := s.lock()
	if err != nil {
		return nil, "", err
	}
	defer l.Unlock()
	vs, err := s.vaults()
	if err != nil {
		return nil, "", err
	}
	if _, mk, err := s.match(vs, password); err == nil {
		seal.Wipe(mk)
		return nil, "", ErrPasswordInUse
	}

	id := newID()
	mk := seal.Random(seal.KeySize)
	hdr := &vaultHeader{Format: headerFormat, Version: headerVersion, ID: id}
	if hdr.Password, err = newPasswordSlot(id, mk, password, s.kdf); err != nil {
		return nil, "", err
	}
	var recovery string
	if withRecoveryKey {
		slot, text, err := newRecoverySlot(id, mk)
		if err != nil {
			return nil, "", err
		}
		hdr.Recovery, recovery = &slot, text
	}
	dir := filepath.Join(s.vaultsDir(), id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, "", err
	}
	sess := &Session{svc: s, id: id, dir: dir, mk: mk}
	// The index is written before the header: a vault without a header is
	// ignored, a header without an index would be broken.
	if err := sess.saveManifest(&manifest{Version: manifestVersion}); err != nil {
		_ = fsx.RemoveAll(dir)
		return nil, "", err
	}
	if err := writeHeader(dir, hdr); err != nil {
		_ = fsx.RemoveAll(dir)
		return nil, "", err
	}
	return sess, recovery, nil
}

// Open opens the vault that password belongs to and completes any
// operation that an earlier crash interrupted. The repairs performed are
// available from Session.Notes.
func (s *Service) Open(password []byte) (*Session, error) {
	vs, err := s.vaults()
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, ErrNoVault
	}
	v, mk, err := s.match(vs, password)
	if err != nil {
		return nil, err
	}
	sess := &Session{svc: s, id: v.hdr.ID, dir: v.dir, mk: mk}
	sess.notes = sess.repair()
	return sess, nil
}

// OpenWithRecoveryKey opens the vault a recovery key belongs to. The
// session may then reset the vault's password with ResetPassword.
func (s *Service) OpenWithRecoveryKey(text string) (*Session, error) {
	key, err := seal.ParseRecoveryKey(text)
	if err != nil {
		return nil, err
	}
	defer seal.Wipe(key)
	vs, err := s.vaults()
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		if mk, err := v.hdr.unwrapRecovery(key); err == nil {
			sess := &Session{svc: s, id: v.hdr.ID, dir: v.dir, mk: mk, viaRecovery: true}
			sess.notes = sess.repair()
			return sess, nil
		}
	}
	return nil, ErrWrongRecoveryKey
}
