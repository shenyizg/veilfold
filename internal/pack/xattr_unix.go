//go:build darwin || linux

package pack

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// keepXattr selects which extended attributes are preserved. On Linux only
// the user namespace is portable between processes of the same user; the
// security, system and trusted namespaces need privileges to restore.
func keepXattr(name string) bool {
	if runtime.GOOS == "linux" {
		return strings.HasPrefix(name, "user.")
	}
	return true
}

// errSkipXattr marks an attribute that cannot be read and is skipped.
var errSkipXattr = errors.New("unreadable extended attribute")

// readXattrs returns the extended attributes of a file or directory.
// Attributes that cannot be read are skipped; an attribute too large to
// archive is an error, because dropping it (a resource fork, say) would
// lose data.
func readXattrs(path string) ([]Xattr, error) {
	var out []Xattr
	for _, name := range listXattrs(path) {
		if !keepXattr(name) {
			continue
		}
		v, err := getXattr(path, name)
		if errors.Is(err, errSkipXattr) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Xattr{Name: name, Value: v})
	}
	return out, nil
}

func listXattrs(path string) []string {
	for range 4 {
		sz, err := unix.Listxattr(path, nil)
		if err != nil || sz <= 0 {
			return nil
		}
		buf := make([]byte, sz)
		n, err := unix.Listxattr(path, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // the list grew in between; retry
		}
		if err != nil {
			return nil
		}
		var names []string
		for _, name := range bytes.Split(buf[:n], []byte{0}) {
			if len(name) > 0 && len(name) <= maxXattrName {
				names = append(names, string(name))
			}
		}
		return names
	}
	return nil
}

func getXattr(path, name string) ([]byte, error) {
	for range 4 {
		sz, err := unix.Getxattr(path, name, nil)
		if err != nil || sz < 0 {
			return nil, errSkipXattr
		}
		if sz > maxXattrSize {
			return nil, fmt.Errorf("%w: %s (%d bytes)", ErrXattrTooLarge, name, sz)
		}
		buf := make([]byte, sz)
		n, err := unix.Getxattr(path, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, errSkipXattr
		}
		return buf[:n], nil
	}
	return nil, errSkipXattr
}

// setXattrs restores extended attributes, ignoring the ones the system does
// not allow us to set (for example com.apple.provenance on macOS).
func setXattrs(path string, xs []Xattr) {
	for _, x := range xs {
		_ = unix.Setxattr(path, x.Name, x.Value, 0)
	}
}
