//go:build unix

package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func device(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	return uint64(st.Dev), nil // Dev is not uint64 on every platform
}

// VolumeID identifies the file system that contains path. Two paths with the
// same VolumeID can be renamed into each other.
func VolumeID(path string) (string, error) {
	dev, err := device(path)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(dev, 10), nil
}

// MountPoint returns the top-most directory above path on the same file
// system.
func MountPoint(path string) (string, error) {
	cur, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dev, err := device(cur)
	if err != nil {
		return "", err
	}
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur, nil
		}
		pdev, err := device(parent)
		if err != nil || pdev != dev {
			return cur, nil
		}
		cur = parent
	}
}

// FreeSpace returns the number of bytes available to the current user on
// the file system containing path.
func FreeSpace(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // field types vary by platform
}

// SyncDir flushes a directory's entries (for example after a rename) to
// stable storage.
func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	return nil
}

// IsInUse reports whether err means that a file is busy.
func IsInUse(err error) bool {
	return errors.Is(err, unix.EBUSY) || errors.Is(err, unix.ETXTBSY)
}

// IsCrossDevice reports whether err means that a rename crossed file systems.
func IsCrossDevice(err error) bool {
	return errors.Is(err, unix.EXDEV)
}

const permBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// ConcealState records what Conceal changes so that Reveal can restore it.
// On Unix this is the directory's permission bits.
func ConcealState(path string) (uint64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return uint64(fi.Mode() & permBits), nil
}

// Conceal makes a stored folder unreadable for casual inspection: on Unix it
// removes all permissions from the top-level directory, so listing it or
// indexing its contents fails until Reveal restores the original mode.
func Conceal(path string, _ uint64) error {
	return os.Chmod(path, 0)
}

// Reveal undoes Conceal.
func Reveal(path string, state uint64) error {
	return os.Chmod(path, os.FileMode(state)&permBits)
}

// TryLock acquires an exclusive lock on path, creating the file if needed.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// UserTag is a short, stable, file-name-safe identifier of the current
// operating system user, used to separate users' stores on shared drives.
func UserTag() string {
	h := sha256.Sum256([]byte("veilfold user " + strconv.Itoa(os.Getuid())))
	return hex.EncodeToString(h[:6])
}

// SyncFile flushes a file's data to the storage device. On macOS it issues a
// plain fsync rather than the much slower F_FULLFSYNC; callers that need a
// full barrier follow up with SyncDir, which uses F_FULLFSYNC and therefore
// also flushes the drive's cache for everything written before.
func SyncFile(f *os.File) error {
	return unix.Fsync(int(f.Fd()))
}

// FileAttributes returns platform specific file attributes (none on Unix).
func FileAttributes(fs.FileInfo) uint32 { return 0 }

// SetFileAttributes applies attributes returned by FileAttributes.
func SetFileAttributes(string, uint32) error { return nil }

// Device returns the identifier of the file system an entry lives on.
func Device(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true // Dev is not uint64 on every platform
}

// Rename renames a file or directory.
func Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// ReadFile reads a whole file.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// CheckPrivateDir verifies that path is a real directory (not a link)
// owned by the current user, and restricts it to mode 0700. Stores are
// only used after this check, so another account cannot plant a directory
// or link at a predictable location and receive the data.
func CheckPrivateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user", path)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(path, 0o700)
	}
	return nil
}

// CheckSharedDir verifies that a directory shared by several accounts is a
// real directory and, unless the current user owns it, has the sticky bit
// so that nobody can rename or delete other users' entries.
func CheckSharedDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if ok && int(st.Uid) != os.Getuid() && fi.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s belongs to another user and is not protected by the sticky bit", path)
	}
	return nil
}
