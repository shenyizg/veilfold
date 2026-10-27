// Package fsx wraps the file system operations Veilfold needs and hides the
// differences between macOS, Linux and Windows: atomic writes, durable
// renames, volume detection, free space, advisory locks and concealment
// attributes.
package fsx

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ErrLocked is returned by TryLock when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock is an exclusive advisory lock on a file. The operating system
// releases it automatically if the process dies.
type Lock struct {
	f *os.File
}

// WriteFileAtomic replaces path with data so that readers and crashes only
// ever observe either the old or the new content.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return SyncDir(dir)
}

// ErrMountPoint is returned by RemoveAll instead of descending into another
// file system mounted inside the tree.
var ErrMountPoint = errors.New("another drive or network share is mounted inside")

// RemoveAll deletes path recursively. Unlike os.RemoveAll it
//
//   - never crosses into another file system mounted inside the tree, so a
//     mounted drive or network share is not emptied by accident, and
//   - also removes read-only files and directories (and, on macOS, files
//     with the "locked" flag), as produced by concealment or by a user's
//     own restrictive permissions.
func RemoveAll(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return removeEntry(path)
	}
	dev, ok := deviceOf(fi)
	return removeTree(path, dev, ok)
}

// deviceOf identifies the file system of an entry; tests replace it.
var deviceOf = Device

func removeTree(dir string, dev uint64, checkDev bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A directory without permissions (for example a concealed one).
		clearFlags(dir)
		_ = os.Chmod(dir, 0o700)
		if entries, err = os.ReadDir(dir); err != nil {
			return err
		}
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			if err := removeEntry(p); err != nil {
				return err
			}
			continue
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if d, ok := deviceOf(info); checkDev && ok && d != dev {
			return &fs.PathError{Op: "remove", Path: p, Err: ErrMountPoint}
		}
		if err := removeTree(p, dev, checkDev); err != nil {
			return err
		}
	}
	return removeEntry(dir)
}

// removeEntry removes a file, link or empty directory, making it and its
// parent writable if that is what stands in the way.
func removeEntry(p string) error {
	err := os.Remove(p)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	clearFlags(p)
	if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&fs.ModeSymlink == 0 {
		_ = os.Chmod(p, 0o700)
	}
	_ = os.Chmod(filepath.Dir(p), 0o700)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Exists reports whether path exists without following a final symlink.
// Errors other than "does not exist" are treated as existing so callers
// err on the side of not overwriting anything.
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// OpenInFileManager shows a folder in Finder, Explorer or the desktop's
// default file manager.
func OpenInFileManager(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer.exe", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
