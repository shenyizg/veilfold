//go:build unix && !darwin

package fsx

// HideDir is a no-op on Linux and other Unix systems, where Veilfold relies
// on dot-prefixed directory names (which file managers hide by default).
func HideDir(string) error { return nil }

func clearFlags(string) {}
