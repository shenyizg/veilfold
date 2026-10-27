//go:build unix

package pack

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func makeSpecialFile(path string) error {
	return unix.Mkfifo(path, 0o600)
}

// setTestXattr tags path with an extended attribute and reports whether the
// file system supports it.
func setTestXattr(t *testing.T, path string) bool {
	t.Helper()
	name := "com.veilfold.test"
	if runtime.GOOS == "linux" {
		name = "user.veilfold.test"
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return false
	}
	if err := unix.Setxattr(path, name, []byte("tagged"), 0); err != nil {
		t.Logf("extended attributes unsupported here: %v", err)
		return false
	}
	return true
}
