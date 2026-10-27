package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "index")
	for _, content := range []string{"first", "second, longer content"} {
		if err := WriteFileAtomic(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != content {
			t.Fatalf("read %q, %v; want %q", got, err, content)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestRemoveAllReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	sub := filepath.Join(root, "locked")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "readonly.txt")
	if err := os.WriteFile(f, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if Exists(root) {
		t.Fatal("tree still exists")
	}
}

func TestTryLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	l, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: got %v, want ErrLocked", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	l2, err := TryLock(p)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	_ = l2.Unlock()
}

func TestVolumes(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	_ = os.Mkdir(a, 0o755)
	_ = os.Mkdir(b, 0o755)
	va, err := VolumeID(a)
	if err != nil {
		t.Fatal(err)
	}
	if vb, _ := VolumeID(b); va != vb {
		t.Fatalf("siblings on different volumes: %q vs %q", va, vb)
	}
	mp, err := MountPoint(a)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(a)
	clean := filepath.Clean(mp)
	if !strings.HasPrefix(strings.ToLower(real), strings.ToLower(strings.TrimSuffix(clean, string(filepath.Separator)))) {
		t.Fatalf("mount point %q is not above %q", mp, real)
	}
	if free, err := FreeSpace(a); err != nil || free == 0 {
		t.Fatalf("free space %d, %v", free, err)
	}
}

func TestConcealReveal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secret")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Lstat(dir)
	state, err := ConcealState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Conceal(dir, state); err != nil {
		t.Fatal(err)
	}
	concealed, _ := os.Lstat(dir)
	switch runtime.GOOS {
	case "windows":
		const hidden, system = 0x2, 0x4
		if a := FileAttributes(concealed); a&hidden == 0 || a&system == 0 {
			t.Fatalf("attributes %#x after conceal", a)
		}
	default:
		if concealed.Mode().Perm() != 0 {
			t.Fatalf("mode %v after conceal", concealed.Mode())
		}
	}
	if err := Reveal(dir, state); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Lstat(dir)
	if after.Mode() != before.Mode() || FileAttributes(after) != FileAttributes(before) {
		t.Fatalf("not restored: %v/%#x, want %v/%#x", after.Mode(), FileAttributes(after), before.Mode(), FileAttributes(before))
	}
}

func TestHideDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := HideDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := HideDir(dir); err != nil { // idempotent
		t.Fatal(err)
	}
	if !Exists(dir) {
		t.Fatal("hidden directory vanished")
	}
}

func TestIsCrossDevice(t *testing.T) {
	if IsCrossDevice(errors.New("other")) || IsInUse(errors.New("other")) {
		t.Fatal("unrelated errors classified")
	}
}

func TestRemoveAllStopsAtMountPoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stage")
	mounted := filepath.Join(root, "sub", "share")
	if err := os.MkdirAll(mounted, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(mounted, "precious.txt")
	if err := os.WriteFile(precious, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(orig func(fs.FileInfo) (uint64, bool)) { deviceOf = orig }(deviceOf)
	mountedInfo, _ := os.Lstat(mounted)
	deviceOf = func(fi fs.FileInfo) (uint64, bool) {
		if os.SameFile(fi, mountedInfo) {
			return 2, true // another file system is mounted here
		}
		return 1, true
	}
	if err := RemoveAll(root); !errors.Is(err, ErrMountPoint) {
		t.Fatalf("got %v, want ErrMountPoint", err)
	}
	if data, err := os.ReadFile(precious); err != nil || string(data) != "not ours" {
		t.Fatal("RemoveAll deleted data on the mounted file system")
	}
}
