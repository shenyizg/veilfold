package fsx

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// HideDir sets the Finder "hidden" flag on a directory, in addition to its
// dot-prefixed or ~/Library location.
func HideDir(path string) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	if st.Flags&unix.UF_HIDDEN != 0 {
		return nil
	}
	if err := unix.Chflags(path, int(st.Flags|unix.UF_HIDDEN)); err != nil {
		return &fs.PathError{Op: "chflags", Path: path, Err: err}
	}
	return nil
}

// clearFlags removes BSD file flags such as "locked" (uchg), which would
// otherwise prevent deletion.
func clearFlags(path string) {
	_ = unix.Chflags(path, 0)
}
