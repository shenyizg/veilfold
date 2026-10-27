package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func volumePath(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", &fs.PathError{Op: "GetVolumePathName", Path: path, Err: err}
	}
	return windows.UTF16ToString(buf), nil
}

// VolumeID identifies the volume that contains path. Two paths with the same
// VolumeID can be renamed into each other.
func VolumeID(path string) (string, error) {
	v, err := volumePath(path)
	if err != nil {
		return "", err
	}
	return strings.ToLower(v), nil
}

// MountPoint returns the root of the volume containing path, such as `D:\`.
func MountPoint(path string) (string, error) {
	return volumePath(path)
}

// FreeSpace returns the number of bytes available to the current user on
// the volume containing path.
func FreeSpace(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
	}
	return free, nil
}

// SyncDir is a no-op on Windows: NTFS journals directory changes and
// directories cannot be flushed through a regular file handle.
func SyncDir(string) error { return nil }

// IsInUse reports whether err means that a file is open in another program.
func IsInUse(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// IsCrossDevice reports whether err means that a rename crossed volumes.
func IsCrossDevice(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}

const settableAttrs = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN |
	windows.FILE_ATTRIBUTE_SYSTEM | windows.FILE_ATTRIBUTE_ARCHIVE |
	windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED

const concealAttrs = windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM |
	windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED

func getAttrs(path string) (uint32, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	a, err := windows.GetFileAttributes(p)
	if err != nil {
		return 0, &fs.PathError{Op: "GetFileAttributes", Path: path, Err: err}
	}
	return a, nil
}

func setAttrs(path string, attrs uint32) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs &= settableAttrs
	if attrs == 0 {
		attrs = windows.FILE_ATTRIBUTE_NORMAL
	}
	if err := windows.SetFileAttributes(p, attrs); err != nil {
		return &fs.PathError{Op: "SetFileAttributes", Path: path, Err: err}
	}
	return nil
}

// HideDir marks a directory as hidden and as a protected system item, which
// Explorer does not show even when "show hidden files" is enabled.
func HideDir(path string) error {
	a, err := getAttrs(path)
	if err != nil {
		return err
	}
	return setAttrs(path, a|concealAttrs)
}

// ConcealState records the folder's original attributes.
func ConcealState(path string) (uint64, error) {
	a, err := getAttrs(path)
	return uint64(a), err
}

// Conceal marks a stored folder as a hidden, non-indexed system item.
func Conceal(path string, state uint64) error {
	return setAttrs(path, uint32(state)|concealAttrs)
}

// Reveal restores the folder's original attributes.
func Reveal(path string, state uint64) error {
	return setAttrs(path, uint32(state))
}

// TryLock acquires an exclusive lock on path, creating the file if needed.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, ol)
	return l.f.Close()
}

// UserTag is a short, stable, file-name-safe identifier of the current
// Windows account, used to separate users' stores on shared drives.
func UserTag() string {
	id := os.Getenv("USERNAME")
	if u, err := user.Current(); err == nil {
		id = u.Uid // the account's SID
	}
	h := sha256.Sum256([]byte("veilfold user " + id))
	return hex.EncodeToString(h[:6])
}

// SyncFile flushes a file's data to the storage device.
func SyncFile(f *os.File) error {
	return f.Sync()
}

// FileAttributes returns the Windows file attributes recorded in fi.
func FileAttributes(fi fs.FileInfo) uint32 {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes
	}
	return 0
}

// SetFileAttributes applies attributes returned by FileAttributes.
func SetFileAttributes(path string, attrs uint32) error {
	return setAttrs(path, attrs)
}

// Device is not used on Windows, where junctions and mounted folders are
// reparse points that are never descended into.
func Device(fs.FileInfo) (uint64, bool) { return 0, false }

func clearFlags(string) {}

// transient reports errors that Windows returns while another process
// (often an antivirus scanner or a reader without FILE_SHARE_DELETE) has
// the file open for a moment.
func transient(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// retry runs op until it succeeds, fails permanently, or about two seconds
// have passed.
func retry(op func() error) error {
	delay := 5 * time.Millisecond
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := op()
		if err == nil || !transient(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(delay)
		delay = min(2*delay, 200*time.Millisecond)
	}
}

// Rename renames a file or directory, retrying while another process
// briefly holds it open.
func Rename(oldpath, newpath string) error {
	return retry(func() error { return os.Rename(oldpath, newpath) })
}

// ReadFile reads a whole file, retrying while it is briefly locked.
func ReadFile(path string) ([]byte, error) {
	var data []byte
	err := retry(func() error {
		var err error
		data, err = os.ReadFile(path)
		return err
	})
	return data, err
}

// CheckPrivateDir verifies that path is a real directory and not a link or
// junction. Per-user access on Windows is governed by the folder's ACL.
func CheckPrivateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// CheckSharedDir verifies that path is a real directory.
func CheckSharedDir(path string) error { return CheckPrivateDir(path) }
