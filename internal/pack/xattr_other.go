//go:build !darwin && !linux

package pack

// Extended attributes are only preserved on macOS and Linux.

func readXattrs(string) ([]Xattr, error) { return nil, nil }

func setXattrs(string, []Xattr) {}
