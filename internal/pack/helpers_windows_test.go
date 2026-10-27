package pack

import (
	"errors"
	"testing"
)

func makeSpecialFile(string) error {
	return errors.New("no special files on Windows")
}

func setTestXattr(*testing.T, string) bool { return false }
