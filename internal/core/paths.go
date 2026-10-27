package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/pack"
)

// File and directory names used by Veilfold.
const (
	vaultsDirName    = "vaults"
	lockFileName     = "veilfold.lock"
	headerFileName   = "vault.json"
	manifestFileName = "index.vfi"
	blobExt          = ".veil"
	partialExt       = ".partial"
	stageExt         = ".staging"
	restoreExt       = ".restore"
)

// storeDirName is the store inside the data directory and volumeStoreName
// the store at the root of other drives. On macOS the ".noindex" suffix
// keeps Spotlight from indexing what is stored there.
var (
	storeDirName    = pick("store.noindex", "store")
	volumeStoreName = pick(".veilfold.noindex", ".veilfold")
)

func pick(darwin, other string) string {
	if runtime.GOOS == "darwin" {
		return darwin
	}
	return other
}

// DefaultDataDir returns the per-user directory holding vaults and the
// default store. VEILFOLD_DATA_DIR overrides it.
func DefaultDataDir() (string, error) {
	if d := os.Getenv("VEILFOLD_DATA_DIR"); d != "" {
		return filepath.Abs(d)
	}
	switch runtime.GOOS {
	case "windows":
		// Local, not Roaming: stored folders must never be synced around.
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "Veilfold"), nil
		}
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "Veilfold"), nil
	case "darwin":
		base, err := os.UserConfigDir() // ~/Library/Application Support
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "Veilfold"), nil
	default:
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" || !filepath.IsAbs(base) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(base, "veilfold"), nil
	}
}

func absPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("empty path")
	}
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(a), nil
}

// caseInsensitive is true where the default file systems ignore case.
var caseInsensitive = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func pathKey(p string) string {
	p = filepath.Clean(p)
	if caseInsensitive {
		p = strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool { return pathKey(a) == pathKey(b) }

// within reports whether p equals base or lies inside it.
func within(p, base string) bool {
	p, base = pathKey(p), pathKey(base)
	if p == base {
		return true
	}
	if !strings.HasSuffix(base, string(filepath.Separator)) {
		base += string(filepath.Separator)
	}
	return strings.HasPrefix(p, base)
}

// resolve follows symbolic links in p. For paths that do not exist it
// resolves the deepest existing ancestor.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	return filepath.Join(resolve(dir), filepath.Base(p))
}

// systemTrees are folders that may neither be protected nor contain a
// protected folder.
func systemTrees() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/System", "/Library", "/Applications", "/bin", "/sbin", "/usr", "/etc",
			"/private/etc", "/dev", "/cores", "/opt", "/private/var/db"}
	case "windows":
		var out []string
		for _, env := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData"} {
			if v := os.Getenv(env); v != "" {
				out = append(out, v)
			}
		}
		if d := os.Getenv("SystemDrive"); d != "" {
			for _, n := range []string{"Windows", "$Recycle.Bin", "System Volume Information", "Recovery"} {
				out = append(out, d+`\`+n)
			}
		}
		return out
	default:
		return []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/opt",
			"/proc", "/run", "/sbin", "/snap", "/sys", "/usr", "/var/lib", "/var/log", "/var/cache"}
	}
}

// systemContainers are folders that hold other users' or the system's data
// and may not be protected themselves (their subfolders may).
func systemContainers() []string {
	out := []string{os.TempDir()}
	switch runtime.GOOS {
	case "darwin":
		out = append(out, "/", "/Users", "/Users/Shared", "/Volumes", "/Network", "/private", "/private/var",
			"/var", "/tmp", "/private/tmp")
	case "windows":
		if d := os.Getenv("SystemDrive"); d != "" {
			out = append(out, d+`\`, d+`\Users`)
		}
		if p := os.Getenv("PUBLIC"); p != "" {
			out = append(out, p)
		}
	default:
		out = append(out, "/", "/home", "/media", "/mnt", "/run/media", "/var", "/tmp", "/srv", "/root")
	}
	return out
}

// criticalPaths may not be protected, nor any folder containing them.
func (s *Service) criticalPaths() []string {
	out := []string{s.dataDir}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, home)
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Dir(exe))
	}
	return out
}

// checkPath validates that path is a folder Veilfold may protect.
func (s *Service) checkPath(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrMissing, path)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return notAllowed(path, "it is a symbolic link; protect the folder it points to instead")
	}
	if !fi.IsDir() {
		return notAllowed(path, "it is not a folder")
	}
	real := resolve(path)
	if mp, err := fsx.MountPoint(real); err == nil && samePath(mp, real) {
		return notAllowed(path, "it is the root of a drive")
	}
	for _, c := range s.criticalPaths() {
		if within(resolve(c), real) {
			return notAllowed(path, fmt.Sprintf("it contains %s", c))
		}
	}
	for _, t := range systemTrees() {
		if within(real, resolve(t)) {
			return notAllowed(path, "it is part of the operating system or installed programs")
		}
	}
	for _, c := range systemContainers() {
		if c != "" && samePath(real, resolve(c)) {
			return notAllowed(path, "it is a system folder shared by other users or programs")
		}
	}
	if within(real, resolve(s.dataDir)) {
		return notAllowed(path, "it belongs to Veilfold")
	}
	for _, part := range strings.Split(real, string(filepath.Separator)) {
		if strings.EqualFold(part, volumeStoreName) {
			return notAllowed(path, "it belongs to Veilfold")
		}
	}
	if fsx.Exists(filepath.Join(real, volumeStoreName)) {
		return notAllowed(path, "it contains a hidden Veilfold store; protect its subfolders instead")
	}
	return nil
}

var cloudMarkers = []string{
	"/onedrive", "/dropbox", "/google drive", "/googledrive", "/icloud drive",
	"/mobile documents/", "/library/cloudstorage/",
}

// warnings returns advice about protecting path.
func warnings(path string, st pack.Stats) []string {
	var out []string
	p := strings.ToLower(filepath.ToSlash(path)) + "/"
	for _, m := range cloudMarkers {
		if strings.Contains(p, m) {
			out = append(out, "This folder seems to be inside a cloud-synced folder. While it is locked, "+
				"the sync service sees it as deleted and may remove it from your other devices.")
			break
		}
	}
	if st.Bytes > 4<<30 {
		out = append(out, fmt.Sprintf("This folder is large (%s). Encrypting it takes a while and temporarily "+
			"needs that much free space; hiding it is instant.", HumanBytes(st.Bytes)))
	}
	return out
}

// HumanBytes formats a byte count for people.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
