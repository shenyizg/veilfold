package fsx

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRemoveAllLockedFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "locked.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Finder's "Locked" checkbox sets the user-immutable flag.
	if err := unix.Chflags(f, unix.UF_IMMUTABLE); err != nil {
		t.Skip("cannot set flags here:", err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if Exists(root) {
		t.Fatal("tree still exists")
	}
}
