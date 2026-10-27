// Package pack serializes a folder tree into a single byte stream and back.
//
// The stream is designed to be encrypted as a whole by package seal, so it
// is simple and sequential rather than seekable. It records directories,
// regular files and (on Unix) symbolic links, together with permission bits,
// modification times, extended attributes (macOS and Linux) and file
// attributes (Windows).
//
// Layout (all integers big endian):
//
//	"VFPK" | version u8 | meta: u32 length + JSON
//	entry*:
//	    kind u8 ('D' directory, 'F' file, 'L' symlink)
//	    path: u32 length + slash-separated path relative to the root ("." is the root)
//	    mode u32 (Unix permission bits including setuid, setgid and sticky)
//	    mtime i64 (Unix nanoseconds)
//	    attrs u32 (Windows file attributes, 0 elsewhere)
//	    xattr count u16, each: name (u16 length + bytes), value (u32 length + bytes)
//	    'F': size u64 followed by the file's contents
//	    'L': target (u32 length + bytes)
//	'E' | dirs u64 | files u64 | symlinks u64 | bytes u64
//
// Entries are written in pre-order (a directory before its contents), and
// the first entry is always the root directory.
package pack

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"time"
)

const (
	magic   = "VFPK"
	version = 1

	kindDir     = 'D'
	kindFile    = 'F'
	kindSymlink = 'L'
	kindEnd     = 'E'

	maxMeta      = 1 << 20
	maxPath      = 64 << 10
	maxXattrName = 4 << 10
	maxXattrSize = 64 << 20
	maxTarget    = 64 << 10
)

var (
	// ErrUnsupported marks entries that cannot be archived, such as sockets,
	// device nodes or (on Windows) links and cloud placeholders.
	ErrUnsupported = errors.New("unsupported file type")
	// ErrChanged is returned when a file is modified while it is archived.
	ErrChanged = errors.New("file changed while it was being read")
	// ErrCorrupt is returned for malformed archives.
	ErrCorrupt = errors.New("archive is malformed")
	// ErrMountPoint marks a directory where another file system (a drive,
	// disk image or network share) is mounted. Archiving it would capture,
	// and later delete, data that does not belong to the folder.
	ErrMountPoint = errors.New("another drive or network share is mounted here")
	// ErrXattrTooLarge marks an extended attribute (for example a resource
	// fork) that exceeds what an archive can hold.
	ErrXattrTooLarge = errors.New("extended attribute is too large to archive")
)

// EntryError attaches the offending path to an error.
type EntryError struct {
	Path string
	Err  error
}

func (e *EntryError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *EntryError) Unwrap() error { return e.Err }

// Stats counts the contents of a tree or an archive. The root counts as a
// directory.
type Stats struct {
	Dirs     int64 `json:"dirs"`
	Files    int64 `json:"files"`
	Symlinks int64 `json:"symlinks"`
	Bytes    int64 `json:"bytes"`
	// Digest fingerprints the names, types, permissions, sizes, times and
	// link targets of all entries. Scan, Write and Verify compute it the
	// same way, so a tree can be checked against an archive cheaply.
	Digest [32]byte `json:"-"`
}

// digester accumulates Stats.Digest in archive order.
type digester struct {
	h hash.Hash
	b [8]byte
}

func newDigester() *digester { return &digester{h: sha256.New()} }

func (d *digester) add(h *header) {
	put32 := func(v uint32) {
		binary.BigEndian.PutUint32(d.b[:4], v)
		d.h.Write(d.b[:4])
	}
	put64 := func(v uint64) {
		binary.BigEndian.PutUint64(d.b[:], v)
		d.h.Write(d.b[:])
	}
	d.h.Write([]byte{h.kind})
	put32(uint32(len(h.path)))
	io.WriteString(d.h, h.path)
	put32(h.mode)
	put64(uint64(h.mtime))
	put64(uint64(h.size))
	put32(uint32(len(h.target)))
	io.WriteString(d.h, h.target)
}

func (d *digester) sum() (out [32]byte) {
	copy(out[:], d.h.Sum(nil))
	return out
}

// Meta is free-form information stored inside the (encrypted) archive. It
// allows an archive to be identified even without the vault index.
type Meta struct {
	Item    string    `json:"item,omitempty"`
	Source  string    `json:"source,omitempty"`
	Created time.Time `json:"created"`
}

// Xattr is an extended attribute.
type Xattr struct {
	Name  string `json:"name"`
	Value []byte `json:"value"`
}

// DirMeta is the metadata of a directory. Extract returns the root's
// metadata instead of applying it, so callers can apply it last.
type DirMeta struct {
	Mode    uint32  `json:"mode"`
	ModTime int64   `json:"mtime"`
	Attrs   uint32  `json:"attrs,omitempty"`
	Xattrs  []Xattr `json:"xattrs,omitempty"`
}

// Apply sets the recorded metadata on the directory at path.
func (m DirMeta) Apply(path string) error {
	setXattrs(path, m.Xattrs)
	return applyMeta(path, m.Mode, m.ModTime, m.Attrs)
}

type header struct {
	kind   byte
	path   string
	mode   uint32
	mtime  int64
	attrs  uint32
	xattrs []Xattr
	size   int64
	target string
}

func unixMode(m os.FileMode) uint32 {
	u := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		u |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		u |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		u |= 0o1000
	}
	return u
}

func goMode(u uint32) os.FileMode {
	m := os.FileMode(u & 0o777)
	if u&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if u&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if u&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// encoder writes primitive values and remembers the first error.
type encoder struct {
	w   *bufio.Writer
	err error
	b   [8]byte
}

func (e *encoder) raw(p []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(p)
	}
}

func (e *encoder) u8(v byte) {
	if e.err == nil {
		e.err = e.w.WriteByte(v)
	}
}

func (e *encoder) u16(v uint16) {
	binary.BigEndian.PutUint16(e.b[:2], v)
	e.raw(e.b[:2])
}

func (e *encoder) u32(v uint32) {
	binary.BigEndian.PutUint32(e.b[:4], v)
	e.raw(e.b[:4])
}

func (e *encoder) u64(v uint64) {
	binary.BigEndian.PutUint64(e.b[:8], v)
	e.raw(e.b[:8])
}

func (e *encoder) str32(s string) {
	e.u32(uint32(len(s)))
	if e.err == nil {
		_, e.err = e.w.WriteString(s)
	}
}

func (e *encoder) entry(h *header) {
	e.u8(h.kind)
	e.str32(h.path)
	e.u32(h.mode)
	e.u64(uint64(h.mtime))
	e.u32(h.attrs)
	e.u16(uint16(len(h.xattrs)))
	for _, x := range h.xattrs {
		e.u16(uint16(len(x.Name)))
		e.raw([]byte(x.Name))
		e.u32(uint32(len(x.Value)))
		e.raw(x.Value)
	}
	switch h.kind {
	case kindFile:
		e.u64(uint64(h.size))
	case kindSymlink:
		e.str32(h.target)
	}
}

// decoder reads primitive values. Running out of input inside a structure
// is reported as ErrCorrupt; errors of the underlying reader (for example an
// authentication failure) are passed through unchanged.
type decoder struct {
	r *bufio.Reader
	b [8]byte
}

func corrupt(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: unexpected end of data", ErrCorrupt)
	}
	return err
}

func (d *decoder) full(p []byte) error {
	_, err := io.ReadFull(d.r, p)
	return corrupt(err)
}

func (d *decoder) u8() (byte, error) {
	b, err := d.r.ReadByte()
	return b, corrupt(err)
}

func (d *decoder) u16() (uint16, error) {
	err := d.full(d.b[:2])
	return binary.BigEndian.Uint16(d.b[:2]), err
}

func (d *decoder) u32() (uint32, error) {
	err := d.full(d.b[:4])
	return binary.BigEndian.Uint32(d.b[:4]), err
}

func (d *decoder) u64() (uint64, error) {
	err := d.full(d.b[:8])
	return binary.BigEndian.Uint64(d.b[:8]), err
}

func (d *decoder) bytes(n uint32, limit uint32) ([]byte, error) {
	if n > limit {
		return nil, fmt.Errorf("%w: field of %d bytes exceeds limit", ErrCorrupt, n)
	}
	p := make([]byte, n)
	return p, d.full(p)
}

func (d *decoder) str32(limit uint32) (string, error) {
	n, err := d.u32()
	if err != nil {
		return "", err
	}
	p, err := d.bytes(n, limit)
	return string(p), err
}

func (d *decoder) entry(kind byte) (*header, error) {
	h := &header{kind: kind}
	var err error
	if h.path, err = d.str32(maxPath); err != nil {
		return nil, err
	}
	if h.mode, err = d.u32(); err != nil {
		return nil, err
	}
	mtime, err := d.u64()
	if err != nil {
		return nil, err
	}
	h.mtime = int64(mtime)
	if h.attrs, err = d.u32(); err != nil {
		return nil, err
	}
	count, err := d.u16()
	if err != nil {
		return nil, err
	}
	for range count {
		nl, err := d.u16()
		if err != nil {
			return nil, err
		}
		name, err := d.bytes(uint32(nl), maxXattrName)
		if err != nil {
			return nil, err
		}
		vl, err := d.u32()
		if err != nil {
			return nil, err
		}
		value, err := d.bytes(vl, maxXattrSize)
		if err != nil {
			return nil, err
		}
		h.xattrs = append(h.xattrs, Xattr{Name: string(name), Value: value})
	}
	switch kind {
	case kindFile:
		size, err := d.u64()
		if err != nil {
			return nil, err
		}
		if size > 1<<62 {
			return nil, fmt.Errorf("%w: invalid file size", ErrCorrupt)
		}
		h.size = int64(size)
	case kindSymlink:
		if h.target, err = d.str32(maxTarget); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// copyN copies exactly n bytes unless src ends early, checking ctx between
// blocks and reporting each block to progress.
func copyN(ctx context.Context, dst io.Writer, src io.Reader, n int64, buf []byte, progress func(int64)) (int64, error) {
	var written int64
	for written < n {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		chunk := min(int64(len(buf)), n-written)
		k, err := io.ReadFull(src, buf[:chunk])
		if k > 0 {
			if _, werr := dst.Write(buf[:k]); werr != nil {
				return written, werr
			}
			written += int64(k)
			if progress != nil {
				progress(int64(k))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return written, nil
			}
			return written, err
		}
	}
	return written, nil
}
