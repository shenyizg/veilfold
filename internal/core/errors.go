package core

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/pack"
)

// Errors returned by the core. Callers should test with errors.Is; the
// returned errors usually wrap these with details.
var (
	ErrNoVault           = errors.New("no vault exists yet")
	ErrWrongPassword     = errors.New("wrong password")
	ErrWrongRecoveryKey  = errors.New("this recovery key does not belong to any vault on this computer")
	ErrPasswordInUse     = errors.New("this password is already used by another vault on this computer")
	ErrBusy              = errors.New("another Veilfold operation is in progress")
	ErrClosed            = errors.New("the vault is closed")
	ErrNotFound          = errors.New("no such protected folder")
	ErrAmbiguous         = errors.New("more than one protected folder matches")
	ErrNotAllowed        = errors.New("this folder cannot be protected")
	ErrMissing           = errors.New("the folder does not exist")
	ErrTargetExists      = errors.New("something already exists at the destination")
	ErrParentMissing     = errors.New("the destination's parent folder does not exist")
	ErrDataMissing       = errors.New("the protected data could not be found")
	ErrDifferentVolume   = errors.New("a hidden folder can only be restored on the drive it was hidden on")
	ErrInsufficientSpace = errors.New("not enough free disk space")
	ErrInUse             = errors.New("the folder is in use by another program")
	ErrNotEmpty          = errors.New("the vault still protects folders")
	ErrRecoveryOnly      = errors.New("only a vault opened with its recovery key can reset its password")
	ErrIncomplete        = errors.New("the operation succeeded, but cleaning up afterwards did not")
	ErrNotLocked         = errors.New("the folder is not locked")
	ErrUnresolved        = errors.New("an earlier operation on this folder could not be completed")
)

// NotAllowedError explains why a folder cannot be protected.
type NotAllowedError struct {
	Path   string
	Reason string
}

func (e *NotAllowedError) Error() string {
	return fmt.Sprintf("%s cannot be protected: %s", e.Path, e.Reason)
}

func (e *NotAllowedError) Unwrap() error { return ErrNotAllowed }

func notAllowed(path, reason string) error {
	return &NotAllowedError{Path: path, Reason: reason}
}

// renameError adds advice to errors from moving a user's folder.
func renameError(err error, path string) error {
	switch {
	case fsx.IsCrossDevice(err):
		return fmt.Errorf("%w: %v", ErrDifferentVolume, err)
	case fsx.IsInUse(err):
		return fmt.Errorf("%w: close any program using %s and try again (%v)", ErrInUse, path, err)
	}
	return err
}

// scanError explains why encrypt mode cannot archive a folder.
func scanError(err error) error {
	var ee *pack.EntryError
	if errors.Is(err, pack.ErrMountPoint) && errors.As(err, &ee) {
		return fmt.Errorf("%s: another drive or network share is mounted inside the folder; unmount it first", ee.Path)
	}
	if errors.Is(err, pack.ErrXattrTooLarge) && errors.As(err, &ee) {
		return fmt.Errorf("%s: %v; use hide mode for this folder", ee.Path, ee.Err)
	}
	if errors.Is(err, pack.ErrUnsupported) && errors.As(err, &ee) {
		what := "only files, folders and symbolic links can be encrypted"
		if runtime.GOOS == "windows" {
			what = "links, junctions and cloud-only files cannot be encrypted"
		}
		return fmt.Errorf("%s: %s; use hide mode for this folder", ee.Path, what)
	}
	return err
}
