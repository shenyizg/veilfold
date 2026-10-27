package pack

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// buildTree creates a representative folder and returns its root.
func buildTree(t *testing.T) string {
	root, _ := buildTreeX(t)
	return root
}

// buildTreeX is buildTree that also reports whether an extended attribute
// was set on notes.txt.
func buildTreeX(t *testing.T) (string, bool) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Private")
	write := func(rel string, data []byte, mode os.FileMode) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), (3*copyBufferSize+12345)/16)
	write("notes.txt", []byte("hello"), 0o644)
	write("empty.bin", nil, 0o600)
	write("big/data.bin", big, 0o640)
	write("中文 文件夹/报告 2026.txt", []byte("机密"), 0o644)
	write("deep/a/b/c/d/leaf.md", []byte("# leaf"), 0o644)
	if runtime.GOOS != "windows" {
		write("bin/run.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("notes.txt", filepath.Join(root, "link-to-notes")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("does/not/exist", filepath.Join(root, "dangling")); err != nil {
			t.Fatal(err)
		}
	}
	tagged := setTestXattr(t, filepath.Join(root, "notes.txt"))

	// A read-only directory with content, and distinctive timestamps.
	ro := filepath.Join(root, "readonly")
	write("readonly/inside.txt", []byte("locked in"), 0o444)
	stamp := time.Date(2021, 3, 4, 5, 6, 7, 800, time.UTC)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink == 0 && p != root {
			_ = os.Chtimes(p, stamp, stamp)
		}
		return nil
	})
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(ro, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(root, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	return root, tagged
}

// digest describes a tree precisely enough to detect any loss of data or
// metadata that pack is supposed to preserve.
func digest(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		line := fmt.Sprintf("%s %v", filepath.ToSlash(rel), info.Mode())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += fmt.Sprintf(" %d %s %d", len(data), hex.EncodeToString(sum[:8]), info.ModTime().UnixNano())
		default:
			if rel != "." {
				line += fmt.Sprintf(" %d", info.ModTime().UnixNano())
			}
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			xs, _ := readXattrs(p)
			for _, x := range xs {
				if strings.Contains(x.Name, "veilfold") {
					line += fmt.Sprintf(" xattr:%s=%s", x.Name, x.Value)
				}
			}
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func archive(t *testing.T, root string) ([]byte, Stats) {
	t.Helper()
	var buf bytes.Buffer
	st, err := Write(context.Background(), &buf, root, Meta{Item: "test", Source: root, Created: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), st
}

func TestRoundTrip(t *testing.T) {
	root, tagged := buildTreeX(t)
	scanned, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var progress int64
	var buf bytes.Buffer
	written, err := Write(context.Background(), &buf, root, Meta{Item: "abc", Source: root}, func(n int64) { progress = n })
	if err != nil {
		t.Fatal(err)
	}
	if scanned != written {
		t.Fatalf("scan %+v != write %+v", scanned, written)
	}
	if progress != written.Bytes {
		t.Fatalf("progress ended at %d, want %d", progress, written.Bytes)
	}

	verified, err := Verify(context.Background(), bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Stats != written || verified.Meta.Item != "abc" {
		t.Fatalf("verify: %+v", verified)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	res, err := Extract(context.Background(), bytes.NewReader(buf.Bytes()), dest, ExtractOptions{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "readonly"), 0o755) })
	if res.Stats != written {
		t.Fatalf("extract stats %+v != %+v", res.Stats, written)
	}
	if err := res.Root.Apply(dest); err != nil {
		t.Fatal(err)
	}
	want, got := digest(t, root), digest(t, dest)
	if tagged && !slices.ContainsFunc(want, func(l string) bool { return strings.Contains(l, "xattr:") }) {
		t.Fatal("extended attribute was set but not observed")
	}
	if !slices.Equal(want, got) {
		t.Fatalf("restored tree differs\nwant:\n%s\n\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	ri, _ := os.Stat(root)
	di, _ := os.Stat(dest)
	if !ri.ModTime().Equal(di.ModTime()) || ri.Mode() != di.Mode() {
		t.Fatalf("root metadata not restored: %v %v vs %v %v", ri.Mode(), ri.ModTime(), di.Mode(), di.ModTime())
	}
}

func TestEmptyFolder(t *testing.T) {
	root := t.TempDir()
	data, st := archive(t, root)
	if st.Dirs != 1 || st.Files != 0 || st.Symlinks != 0 || st.Bytes != 0 {
		t.Fatalf("stats %+v", st)
	}
	dest := filepath.Join(t.TempDir(), "x")
	if _, err := Extract(context.Background(), bytes.NewReader(data), dest, ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRefusesExistingDestination(t *testing.T) {
	data, _ := archive(t, t.TempDir())
	if _, err := Extract(context.Background(), bytes.NewReader(data), t.TempDir(), ExtractOptions{}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("got %v, want ErrExist", err)
	}
}

func TestCorruptArchives(t *testing.T) {
	root := buildTree(t)
	data, _ := archive(t, root)
	cases := map[string][]byte{
		"empty":         {},
		"bad magic":     append([]byte("XXXX"), data[4:]...),
		"bad version":   append(append([]byte(magic), 9), data[5:]...),
		"truncated":     data[:len(data)/2],
		"no trailer":    data[:len(data)-33],
		"trailing data": append(append([]byte(nil), data...), 0),
	}
	for name, c := range cases {
		if _, err := Verify(context.Background(), bytes.NewReader(c), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
		dest := filepath.Join(t.TempDir(), "out")
		if _, err := Extract(context.Background(), bytes.NewReader(c), dest, ExtractOptions{}); err == nil {
			t.Errorf("%s: extracted", name)
		}
	}
}

// craft builds an archive from raw entries, bypassing Write's safety checks.
func craft(entries ...*header) []byte {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	enc := &encoder{w: bw}
	enc.raw([]byte(magic))
	enc.u8(version)
	enc.str32("{}")
	var st Stats
	for _, h := range entries {
		enc.entry(h)
		switch h.kind {
		case kindDir:
			st.Dirs++
		case kindFile:
			enc.raw(bytes.Repeat([]byte{'x'}, int(h.size)))
			st.Files++
			st.Bytes += h.size
		case kindSymlink:
			st.Symlinks++
		}
	}
	enc.u8(kindEnd)
	enc.u64(uint64(st.Dirs))
	enc.u64(uint64(st.Files))
	enc.u64(uint64(st.Symlinks))
	enc.u64(uint64(st.Bytes))
	_ = bw.Flush()
	return buf.Bytes()
}

func TestMaliciousPaths(t *testing.T) {
	root := &header{kind: kindDir, path: ".", mode: 0o755}
	for _, p := range []string{"../escape", "/etc/passwd", "a/../../escape", "a//b", "./a", "", ".", "a/./b", "a\x00b"} {
		data := craft(root, &header{kind: kindFile, path: p, mode: 0o644, size: 1})
		parent := t.TempDir()
		_, err := Extract(context.Background(), bytes.NewReader(data), filepath.Join(parent, "out"), ExtractOptions{})
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("path %q: got %v, want ErrCorrupt", p, err)
		}
		if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
			t.Fatalf("path %q escaped the destination", p)
		}
	}
	// The first entry must be the root, and only once.
	for name, data := range map[string][]byte{
		"file first": craft(&header{kind: kindFile, path: "a", size: 1}),
		"two roots":  craft(root, root),
		"no entries": craft(),
	} {
		if _, err := Verify(context.Background(), bytes.NewReader(data), nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestSymlinkCannotRedirectWrites(t *testing.T) {
	if !symlinksSupported {
		t.Skip("symlinks are not archived on this platform")
	}
	// A crafted archive that creates a link to outside and then writes
	// through it must not touch the link target, because links are only
	// created after all files.
	outside := t.TempDir()
	data := craft(
		&header{kind: kindDir, path: ".", mode: 0o755},
		&header{kind: kindSymlink, path: "link", target: outside},
		&header{kind: kindFile, path: "link/owned.txt", mode: 0o644, size: 3},
	)
	_, err := Extract(context.Background(), bytes.NewReader(data), filepath.Join(t.TempDir(), "out"), ExtractOptions{})
	if err == nil {
		t.Fatal("extraction through a link succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "owned.txt")); err == nil {
		t.Fatal("file was written outside the destination")
	}
}

func TestUnsupportedEntries(t *testing.T) {
	root := t.TempDir()
	if err := makeSpecialFile(filepath.Join(root, "fifo")); err != nil {
		t.Skip("cannot create special files here:", err)
	}
	if _, err := Scan(context.Background(), root); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("scan: got %v", err)
	}
	if _, err := Write(context.Background(), &bytes.Buffer{}, root, Meta{}, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("write: got %v", err)
	}
}

func TestCancellation(t *testing.T) {
	root := buildTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Write(ctx, &bytes.Buffer{}, root, Meta{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("write: got %v", err)
	}
	data, _ := archive(t, root)
	if _, err := Extract(ctx, bytes.NewReader(data), filepath.Join(t.TempDir(), "o"), ExtractOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("extract: got %v", err)
	}
}

func TestDigest(t *testing.T) {
	root := buildTree(t)
	scanned, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	data, written := archive(t, root)
	verified, err := Verify(context.Background(), bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if scanned.Digest == ([32]byte{}) || scanned.Digest != written.Digest || verified.Stats.Digest != written.Digest {
		t.Fatal("scan, write and verify disagree on the digest")
	}
	again, _ := Scan(context.Background(), root)
	if again.Digest != scanned.Digest {
		t.Fatal("digest of an unchanged tree changed")
	}
	// Appending to a file, or adding one, changes the digest.
	f, err := os.OpenFile(filepath.Join(root, "notes.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(" more")
	_ = f.Close()
	changed, _ := Scan(context.Background(), root)
	if changed.Digest == scanned.Digest {
		t.Fatal("digest did not notice an appended file")
	}
	if err := os.WriteFile(filepath.Join(root, "deep", "new.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	added, _ := Scan(context.Background(), root)
	if added.Digest == changed.Digest {
		t.Fatal("digest did not notice a new file")
	}
}

func TestMountPointsRefused(t *testing.T) {
	root := buildTree(t)
	mounted := filepath.Join(root, "deep", "a")
	defer func(orig func(fs.FileInfo) (uint64, bool)) { deviceOf = orig }(deviceOf)
	rootInfo, _ := os.Lstat(root)
	rootDev, _ := deviceOf(rootInfo)
	mountedInfo, _ := os.Lstat(mounted)
	deviceOf = func(fi fs.FileInfo) (uint64, bool) {
		if os.SameFile(fi, mountedInfo) {
			return rootDev + 1, true // pretend another file system is mounted here
		}
		return rootDev, true
	}
	if _, err := Scan(context.Background(), root); !errors.Is(err, ErrMountPoint) {
		t.Fatalf("scan: got %v, want ErrMountPoint", err)
	}
	if _, err := Write(context.Background(), &bytes.Buffer{}, root, Meta{}, nil); !errors.Is(err, ErrMountPoint) {
		t.Fatalf("write: got %v, want ErrMountPoint", err)
	}
}
