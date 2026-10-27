package pack

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/shenyizg/veilfold/internal/fsx"
)

// ExtractOptions configures Extract.
type ExtractOptions struct {
	// Sync flushes every extracted file to stable storage.
	Sync bool
	// Progress, if not nil, receives the cumulative number of file content
	// bytes processed.
	Progress func(done int64)
}

// Result describes a successfully read archive.
type Result struct {
	Meta  Meta
	Root  DirMeta
	Stats Stats
}

// Verify reads a complete archive without writing anything and checks its
// structure and trailer. Together with the authenticated encryption around
// it, a successful Verify proves the archive can be extracted.
func Verify(ctx context.Context, r io.Reader, progress func(done int64)) (Result, error) {
	return process(ctx, r, "", ExtractOptions{Progress: progress})
}

// Extract recreates the archived tree at dest, which must not exist yet but
// whose parent must. The root directory is created with mode 0700 and its
// recorded metadata is returned in Result.Root rather than applied. On
// error, dest may contain a partial tree that the caller should remove.
func Extract(ctx context.Context, r io.Reader, dest string, opts ExtractOptions) (Result, error) {
	if dest == "" {
		return Result{}, errors.New("pack: empty destination")
	}
	return process(ctx, r, dest, opts)
}

type pendingDir struct {
	path string
	meta DirMeta
}

type pendingLink struct {
	path, target string
}

func process(ctx context.Context, r io.Reader, dest string, opts ExtractOptions) (Result, error) {
	var res Result
	d := &decoder{r: bufio.NewReaderSize(r, 256<<10)}

	var head [5]byte
	if err := d.full(head[:]); err != nil {
		return res, err
	}
	if string(head[:4]) != magic {
		return res, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if head[4] != version {
		return res, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, head[4])
	}
	mj, err := d.str32(maxMeta)
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal([]byte(mj), &res.Meta); err != nil {
		return res, fmt.Errorf("%w: metadata: %v", ErrCorrupt, err)
	}

	var (
		st    Stats
		done  int64
		dirs  []pendingDir
		links []pendingLink
		buf   = make([]byte, copyBufferSize)
		dg    = newDigester()
	)
	report := func(n int64) {
		done += n
		if opts.Progress != nil {
			opts.Progress(done)
		}
	}
	for first := true; ; first = false {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		kind, err := d.u8()
		if err != nil {
			return res, err
		}
		if kind == kindEnd {
			if first {
				return res, fmt.Errorf("%w: missing root", ErrCorrupt)
			}
			break
		}
		if kind != kindDir && kind != kindFile && kind != kindSymlink {
			return res, fmt.Errorf("%w: unknown entry kind %q", ErrCorrupt, kind)
		}
		h, err := d.entry(kind)
		if err != nil {
			return res, err
		}
		dg.add(h)
		meta := DirMeta{Mode: h.mode, ModTime: h.mtime, Attrs: h.attrs, Xattrs: h.xattrs}

		if first {
			if kind != kindDir || h.path != "." {
				return res, fmt.Errorf("%w: archive does not start with its root", ErrCorrupt)
			}
			if dest != "" {
				if err := os.Mkdir(dest, 0o700); err != nil {
					return res, err
				}
			}
			res.Root = meta
			st.Dirs++
			continue
		}
		target, err := localPath(dest, h.path)
		if err != nil {
			return res, err
		}
		switch kind {
		case kindDir:
			if dest != "" {
				if err := os.Mkdir(target, 0o700); err != nil {
					return res, err
				}
				dirs = append(dirs, pendingDir{path: target, meta: meta})
			}
			st.Dirs++
		case kindFile:
			if dest == "" {
				n, err := copyN(ctx, io.Discard, d.r, h.size, buf, report)
				if err != nil {
					return res, err
				}
				if n != h.size {
					return res, fmt.Errorf("%w: file data truncated", ErrCorrupt)
				}
			} else if err := extractFile(ctx, d, target, h, opts.Sync, buf, report); err != nil {
				return res, err
			}
			st.Files++
			st.Bytes += h.size
		case kindSymlink:
			if !symlinksSupported {
				return res, &EntryError{Path: h.path, Err: ErrUnsupported}
			}
			if dest != "" {
				links = append(links, pendingLink{path: target, target: h.target})
			}
			st.Symlinks++
		}
	}

	var trailer [4]uint64
	for i := range trailer {
		if trailer[i], err = d.u64(); err != nil {
			return res, err
		}
	}
	st.Digest = dg.sum()
	if trailer != [4]uint64{uint64(st.Dirs), uint64(st.Files), uint64(st.Symlinks), uint64(st.Bytes)} {
		return res, fmt.Errorf("%w: trailer does not match contents", ErrCorrupt)
	}
	if _, err := d.r.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			return res, fmt.Errorf("%w: trailing data", ErrCorrupt)
		}
		return res, err
	}

	// Links are created only after every file and directory exists, so an
	// archive can never make Extract write through a link it created.
	for _, l := range links {
		if err := os.Symlink(l.target, l.path); err != nil {
			return res, err
		}
	}
	// Apply directory metadata bottom-up: children before their parents, so
	// read-only directories do not get in the way.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := dirs[i].meta.Apply(dirs[i].path); err != nil {
			return res, err
		}
	}
	res.Stats = st
	return res, nil
}

// localPath maps an archive path to a path below dest, rejecting anything
// that is not a clean, relative path inside it.
func localPath(dest, rel string) (string, error) {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) || path.Clean(rel) != rel {
		return "", fmt.Errorf("%w: invalid path %q", ErrCorrupt, rel)
	}
	native := filepath.FromSlash(rel)
	if !filepath.IsLocal(native) {
		return "", fmt.Errorf("%w: invalid path %q", ErrCorrupt, rel)
	}
	return filepath.Join(dest, native), nil
}

func extractFile(ctx context.Context, d *decoder, target string, h *header, sync bool, buf []byte, report func(int64)) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := copyN(ctx, f, d.r, h.size, buf, report)
	if err == nil && n != h.size {
		err = fmt.Errorf("%w: file data truncated", ErrCorrupt)
	}
	if err == nil {
		setXattrs(target, h.xattrs)
		if sync {
			err = fsx.SyncFile(f)
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return applyMeta(target, h.mode, h.mtime, h.attrs)
}

func applyMeta(path string, mode uint32, mtime int64, attrs uint32) error {
	if err := os.Chtimes(path, time.Time{}, time.Unix(0, mtime)); err != nil {
		return err
	}
	if attrs != 0 {
		// Windows: the attributes include the read-only flag.
		return fsx.SetFileAttributes(path, attrs)
	}
	return os.Chmod(path, goMode(mode))
}
