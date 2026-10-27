package pack

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/shenyizg/veilfold/internal/fsx"
)

// Symbolic links can be archived on Unix only: recreating them on Windows
// requires privileges most users do not have, and junctions and cloud
// placeholders are not files at all.
var symlinksSupported = runtime.GOOS != "windows"

const copyBufferSize = 1 << 20

// deviceOf identifies the file system of an entry; tests replace it.
var deviceOf = fsx.Device

// walk visits root and everything below it in pre-order and fails on the
// first entry that cannot be archived, including anything on another file
// system mounted inside the tree.
func walk(ctx context.Context, root string, fn func(rel, path string, info fs.FileInfo) error) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return &EntryError{Path: root, Err: err}
	}
	rootDev, checkDev := deviceOf(rootInfo)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return &EntryError{Path: path, Err: err}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return &EntryError{Path: path, Err: err}
		}
		if dev, ok := deviceOf(info); checkDev && ok && dev != rootDev {
			return &EntryError{Path: path, Err: ErrMountPoint}
		}
		m := info.Mode()
		supported := m.IsDir() || m.IsRegular() || (m&fs.ModeSymlink != 0 && symlinksSupported)
		if !supported {
			return &EntryError{Path: path, Err: ErrUnsupported}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return &EntryError{Path: path, Err: err}
		}
		return fn(filepath.ToSlash(rel), path, info)
	})
}

// Scan counts what Write would archive and reports the first entry that
// cannot be archived. On error the statistics gathered so far are returned.
func Scan(ctx context.Context, root string) (Stats, error) {
	var st Stats
	dg := newDigester()
	err := walk(ctx, root, func(rel, path string, info fs.FileInfo) error {
		h := &header{path: rel, mode: unixMode(info.Mode()), mtime: info.ModTime().UnixNano()}
		switch m := info.Mode(); {
		case m.IsDir():
			h.kind = kindDir
			st.Dirs++
		case m.IsRegular():
			h.kind = kindFile
			h.size = info.Size()
			st.Files++
			st.Bytes += info.Size()
		default:
			target, err := os.Readlink(path)
			if err != nil {
				return &EntryError{Path: path, Err: err}
			}
			h.kind = kindSymlink
			h.target = target
			st.Symlinks++
		}
		dg.add(h)
		return nil
	})
	st.Digest = dg.sum()
	return st, err
}

// Write archives the tree at root into w. progress, if not nil, receives the
// cumulative number of file content bytes written.
func Write(ctx context.Context, w io.Writer, root string, meta Meta, progress func(done int64)) (Stats, error) {
	bw := bufio.NewWriterSize(w, 256<<10)
	enc := &encoder{w: bw}
	mj, err := json.Marshal(meta)
	if err != nil {
		return Stats{}, err
	}
	enc.raw([]byte(magic))
	enc.u8(version)
	enc.u32(uint32(len(mj)))
	enc.raw(mj)

	var st Stats
	var done int64
	dg := newDigester()
	buf := make([]byte, copyBufferSize)
	report := func(n int64) {
		done += n
		if progress != nil {
			progress(done)
		}
	}
	err = walk(ctx, root, func(rel, path string, info fs.FileInfo) error {
		h := &header{
			path:  rel,
			mode:  unixMode(info.Mode()),
			mtime: info.ModTime().UnixNano(),
			attrs: fsx.FileAttributes(info),
		}
		switch m := info.Mode(); {
		case m.IsDir():
			xs, err := readXattrs(path)
			if err != nil {
				return &EntryError{Path: path, Err: err}
			}
			h.kind = kindDir
			h.xattrs = xs
			enc.entry(h)
			st.Dirs++
		case m.IsRegular():
			n, err := writeFile(ctx, enc, h, path, buf, report)
			if err != nil {
				return &EntryError{Path: path, Err: err}
			}
			st.Files++
			st.Bytes += n
		default:
			target, err := os.Readlink(path)
			if err != nil {
				return &EntryError{Path: path, Err: err}
			}
			h.kind = kindSymlink
			h.target = target
			enc.entry(h)
			st.Symlinks++
		}
		dg.add(h)
		return enc.err
	})
	st.Digest = dg.sum()
	if err != nil {
		return st, err
	}
	enc.u8(kindEnd)
	enc.u64(uint64(st.Dirs))
	enc.u64(uint64(st.Files))
	enc.u64(uint64(st.Symlinks))
	enc.u64(uint64(st.Bytes))
	if enc.err != nil {
		return st, enc.err
	}
	return st, bw.Flush()
}

func writeFile(ctx context.Context, enc *encoder, h *header, path string, buf []byte, report func(int64)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !before.Mode().IsRegular() {
		return 0, ErrChanged
	}
	xs, err := readXattrs(path)
	if err != nil {
		return 0, err
	}
	h.kind = kindFile
	h.size = before.Size()
	h.mode = unixMode(before.Mode())
	h.mtime = before.ModTime().UnixNano()
	h.xattrs = xs
	enc.entry(h)
	if enc.err != nil {
		return 0, enc.err
	}
	n, err := copyN(ctx, enc.w, f, h.size, buf, report)
	if err != nil {
		return n, err
	}
	// Refuse to archive a file that is being modified: what we stored
	// would not match what the user expects to get back.
	after, err := f.Stat()
	if err != nil {
		return n, err
	}
	if n != h.size || after.Size() != h.size || !after.ModTime().Equal(before.ModTime()) {
		return n, ErrChanged
	}
	return n, nil
}
