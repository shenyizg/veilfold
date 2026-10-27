package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/pack"
	"github.com/shenyizg/veilfold/internal/seal"
)

// The vault index ("manifest") lists the protected folders. It is stored
// encrypted, so neither the names nor the locations of protected folders
// can be learned without the password.

const (
	manifestMagic   = "VFI1"
	manifestVersion = 1
)

// Mode selects how a folder is protected when it is locked.
type Mode string

const (
	// ModeEncrypt encrypts the folder's contents and names into the store.
	ModeEncrypt Mode = "encrypt"
	// ModeHide moves the folder into the concealed store unchanged.
	ModeHide Mode = "hide"
)

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeEncrypt, ModeHide:
		return Mode(s), nil
	}
	return "", fmt.Errorf("unknown mode %q (use %q or %q)", s, ModeEncrypt, ModeHide)
}

// Storage describes how a locked folder is currently stored.
type Storage string

const (
	// StorageSealed means the folder is an encrypted archive in the store.
	StorageSealed Storage = "sealed"
	// StorageHidden means the folder sits concealed in the store.
	StorageHidden Storage = "hidden"
)

// Phases of multi-step operations, recorded before each step so that an
// interrupted operation can be completed or undone (see repair.go).
const (
	phaseHideMove      = "hide/move"
	phaseUnhideMove    = "unhide/move"
	phaseSealStage     = "seal/stage"
	phaseSealWrite     = "seal/write"
	phaseSealCleanup   = "seal/cleanup"
	phaseSealAbort     = "seal/abort"
	phaseUnsealExtract = "unseal/extract"
	phaseUnsealRename  = "unseal/rename"
	phaseUnsealCleanup = "unseal/cleanup"
)

type pending struct {
	Phase    string        `json:"phase"`
	StoreDir string        `json:"store_dir,omitempty"`
	Stage    string        `json:"stage,omitempty"`
	Target   string        `json:"target,omitempty"`
	Conceal  uint64        `json:"conceal,omitempty"`
	Root     *pack.DirMeta `json:"root,omitempty"`
	Size     int64         `json:"size,omitempty"`
	Files    int64         `json:"files,omitempty"`
}

type record struct {
	ID       string  `json:"id"`
	Path     string  `json:"path"`
	Mode     Mode    `json:"mode"`
	Locked   bool    `json:"locked"`
	Storage  Storage `json:"storage,omitempty"`
	StoreDir string  `json:"store_dir,omitempty"`
	Conceal  uint64  `json:"conceal,omitempty"`
	// Concealed records whether concealment was applied (it is not, for
	// example, on FAT drives or for folders owned by another account).
	Concealed bool `json:"concealed,omitempty"`
	// BlobSalt binds the record to the archive created by the last lock, so
	// an older archive of the same folder cannot be swapped in.
	BlobSalt string    `json:"blob_salt,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Files    int64     `json:"files,omitempty"`
	AddedAt  time.Time `json:"added_at"`
	LockedAt time.Time `json:"locked_at,omitzero"`
	Pending  *pending  `json:"pending,omitempty"`
}

type manifest struct {
	Version int       `json:"version"`
	Items   []*record `json:"items"`
}

func (m *manifest) find(id string) *record {
	for _, r := range m.Items {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (m *manifest) remove(id string) {
	for i, r := range m.Items {
		if r.ID == id {
			m.Items = append(m.Items[:i], m.Items[i+1:]...)
			return
		}
	}
}

// dataPath is where a locked record's data lives.
func (r *record) dataPath() string {
	switch r.Storage {
	case StorageSealed:
		return filepath.Join(r.StoreDir, r.ID+blobExt)
	case StorageHidden:
		return filepath.Join(r.StoreDir, r.ID)
	}
	return ""
}

func (r *record) name() string { return filepath.Base(r.Path) }

// Item is the public view of a protected folder.
type Item struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Mode     Mode      `json:"mode"`
	Locked   bool      `json:"locked"`
	Storage  Storage   `json:"storage,omitempty"`
	Size     int64     `json:"size"`
	Files    int64     `json:"files"`
	AddedAt  time.Time `json:"added_at"`
	LockedAt time.Time `json:"locked_at,omitzero"`
	// Busy is set while an operation on the folder is in progress.
	Busy bool `json:"busy,omitempty"`
	// Problem describes why the folder cannot currently be locked or
	// unlocked, if it cannot.
	Problem string `json:"problem,omitempty"`
}

func (r *record) view() Item {
	it := Item{
		ID: r.ID, Name: r.name(), Path: r.Path, Mode: r.Mode, Locked: r.Locked, Storage: r.Storage,
		Size: r.Size, Files: r.Files, AddedAt: r.AddedAt, LockedAt: r.LockedAt, Busy: r.Pending != nil,
	}
	switch {
	case r.Pending != nil:
		it.Problem = "An operation on this folder is in progress or did not finish."
	case !r.Locked && !fsx.Exists(r.Path):
		it.Problem = "The folder is no longer at its location."
	case r.Locked && !fsx.Exists(r.dataPath()):
		it.Problem = "The protected data was not found. If it is on a removable drive, connect the drive."
	}
	return it
}

func (s *Session) manifestPath() string { return filepath.Join(s.dir, manifestFileName) }

func (s *Session) manifestAD() []byte { return []byte("veilfold index v1\x00" + s.id) }

func (s *Session) readManifest(path string) (*manifest, error) {
	data, err := fsx.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < len(manifestMagic) || string(data[:len(manifestMagic)]) != manifestMagic {
		return nil, errors.New("not a vault index")
	}
	key := seal.SubKey(s.mk, nil, "veilfold/index/v1")
	defer seal.Wipe(key)
	pt, err := seal.Open(key, data[len(manifestMagic):], s.manifestAD())
	if err != nil {
		return nil, err
	}
	defer seal.Wipe(pt)
	var m manifest
	if err := json.Unmarshal(pt, &m); err != nil {
		return nil, err
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("unsupported vault index version %d", m.Version)
	}
	return &m, nil
}

// loadManifest reads the index, falling back to its backup copy if the
// index cannot be read.
func (s *Session) loadManifest() (*manifest, error) {
	m, err := s.readManifest(s.manifestPath())
	if err == nil {
		return m, nil
	}
	if b, berr := s.readManifest(s.manifestPath() + ".bak"); berr == nil {
		return b, nil
	}
	return nil, fmt.Errorf("reading the vault index: %w", err)
}

// saveFault lets tests make saving the index fail.
var saveFault = func() error { return nil }

// saveManifest writes the index and its backup copy. The backup is written
// first, so it is never older than the index itself: whichever of the two
// is read later describes either the state before or after this save, and
// both are consistent with the files on disk, because every operation only
// acts once the save has succeeded.
func (s *Session) saveManifest(m *manifest) error {
	if err := saveFault(); err != nil {
		return fmt.Errorf("saving the vault index: %w", err)
	}
	pt, err := json.Marshal(m)
	if err != nil {
		return err
	}
	defer seal.Wipe(pt)
	key := seal.SubKey(s.mk, nil, "veilfold/index/v1")
	defer seal.Wipe(key)
	ct, err := seal.Seal(key, pt, s.manifestAD())
	if err != nil {
		return err
	}
	data := append([]byte(manifestMagic), ct...)
	for _, path := range []string{s.manifestPath() + ".bak", s.manifestPath()} {
		if err := fsx.WriteFileAtomic(path, data, 0o600); err != nil {
			return fmt.Errorf("saving the vault index: %w", err)
		}
	}
	return nil
}
