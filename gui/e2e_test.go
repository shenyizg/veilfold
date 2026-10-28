package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shenyizg/veilfold/internal/core"
	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/seal"
)

// TestDesktopUI drives the real frontend in headless Chrome against the real
// backend, clicking through every flow a user can take: creating a vault,
// protecting, locking, unlocking, recovering, cancelling. After each step
// that touches files, it checks them on disk: nothing may be lost, changed
// or left readable.
func TestDesktopUI(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a browser")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("Chrome or Chromium not found; set VEILFOLD_CHROME to its path to run this test")
	}

	root := t.TempDir()
	// Hidden folders cannot be removed as they are if the test fails.
	t.Cleanup(func() { _ = fsx.RemoveAll(root) })
	fx := newFixture(t, root)
	svc, err := core.NewService(core.Options{
		DataDir: fx.path("data"),
		KDF:     &seal.KDFParams{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, newApp(svc), fx)
	srv := httptest.NewServer(h)
	defer srv.Close()

	profile, err := os.MkdirTemp("", "veilfold-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	cmd := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--disable-background-networking", "--disable-sync", "--window-size=980,680",
		"--user-data-dir="+profile, srv.URL+"/")
	var chromeLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &chromeLog, &chromeLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	select {
	case rep := <-h.done:
		if !rep.OK {
			t.Errorf("step %q failed: %s\ntoasts: %q\nJavaScript errors: %q", rep.Step, rep.Error, rep.Toasts, rep.Errors)
		}
	case <-time.After(5 * time.Minute):
		t.Errorf("the UI test did not finish in time\nChrome output:\n%s", chromeLog.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.errs {
		t.Error(e)
	}
}

func findChrome() string {
	if p := os.Getenv("VEILFOLD_CHROME"); p != "" {
		return p
	}
	var paths []string
	switch runtime.GOOS {
	case "darwin":
		paths = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if dir := os.Getenv(env); dir != "" {
				paths = append(paths, filepath.Join(dir, `Google\Chrome\Application\chrome.exe`))
			}
		}
	}
	for _, p := range paths {
		if fsx.Exists(p) {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// ---------------------------------------------------------------- harness

type report struct {
	OK     bool     `json:"ok"`
	Step   string   `json:"step"`
	Error  string   `json:"error"`
	Toasts []string `json:"toasts"`
	Errors []string `json:"errors"`
}

// harness serves the frontend with a stand-in for the Wails runtime that
// forwards calls to the real App.
type harness struct {
	t    *testing.T
	app  *App
	fx   *fixture
	mux  *http.ServeMux
	done chan report

	mu     sync.Mutex
	events []json.RawMessage
	wake   chan struct{}
	picks  []string // answers for the next folder pickers
	errs   []string
	// pause holds the next lock at its first encryption progress until
	// the UI cancels it.
	pause     bool
	paused    string // phase at which the lock was held, once it was
	cancelled chan struct{}
}

func newHarness(t *testing.T, a *App, fx *fixture) *harness {
	h := &harness{t: t, app: a, fx: fx, mux: http.NewServeMux(), done: make(chan report, 1), wake: make(chan struct{})}
	a.emit = h.emit
	a.pickFolder = func(string) (string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.picks) == 0 {
			return "", nil // the user cancelled the picker
		}
		p := h.picks[0]
		h.picks = h.picks[1:]
		return p, nil
	}

	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	const app = `<script src="app.js"></script>`
	if !bytes.Contains(index, []byte(app)) {
		t.Fatal("index.html does not load app.js as expected")
	}
	index = bytes.Replace(index, []byte(app),
		[]byte(`<script src="/test/bridge.js"></script>`+app+`<script src="/test/driver.js"></script>`), 1)

	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
	h.mux.Handle("GET /", http.FileServerFS(dist))
	h.mux.HandleFunc("GET /test/{script}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		http.ServeFile(w, r, filepath.Join("testdata", "e2e", filepath.Base(r.PathValue("script"))))
	})
	h.mux.HandleFunc("POST /test/call", h.call)
	h.mux.HandleFunc("GET /test/events", h.stream)
	h.mux.HandleFunc("POST /test/check", h.serveCheck)
	h.mux.HandleFunc("POST /test/log", func(w http.ResponseWriter, r *http.Request) {
		var msg struct{ Line string }
		_ = json.NewDecoder(r.Body).Decode(&msg)
		t.Log(msg.Line)
		_, _ = io.WriteString(w, "{}")
	})
	h.mux.HandleFunc("POST /test/done", func(w http.ResponseWriter, r *http.Request) {
		var rep report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			rep = report{Error: err.Error()}
		}
		_, _ = io.WriteString(w, "{}")
		select {
		case h.done <- rep:
		default:
		}
	})
	return h
}

func (h *harness) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *harness) fail(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, fmt.Sprintf(format, args...))
}

func firstArg(data []any) any {
	if len(data) == 0 {
		return nil
	}
	return data[0]
}

func (h *harness) emit(name string, data ...any) {
	b, err := json.Marshal(map[string]any{"name": name, "data": data})
	if err != nil {
		h.fail("event %s: %v", name, err)
		return
	}
	h.mu.Lock()
	h.events = append(h.events, b)
	close(h.wake)
	h.wake = make(chan struct{})
	var wait chan struct{}
	if p, ok := firstArg(data).(OpProgress); ok && h.pause &&
		(p.Phase == string(core.PhaseEncrypt) || p.Phase == string(core.PhaseVerify)) {
		h.pause, h.paused, wait = false, p.Phase, h.cancelled
	}
	h.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(20 * time.Second):
			h.fail("the paused lock was never cancelled")
		}
	}
}

// stream sends backend events to the page as server-sent events.
func (h *harness) stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	sent := 0
	for {
		h.mu.Lock()
		pending, wake := h.events[sent:], h.wake
		h.mu.Unlock()
		for _, e := range pending {
			fmt.Fprintf(w, "data: %s\n\n", e)
			sent++
		}
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		}
	}
}

// call invokes an App method the way the Wails bindings do.
func (h *harness) call(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Args   []json.RawMessage `json:"args"`
	}
	reply := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply["error"] = err.Error()
	} else if m := reflect.ValueOf(h.app).MethodByName(req.Method); !m.IsValid() || m.Type().NumIn() != len(req.Args) {
		reply["error"] = fmt.Sprintf("method %s with %d arguments is not bound", req.Method, len(req.Args))
	} else {
		in := make([]reflect.Value, len(req.Args))
		for i, raw := range req.Args {
			v := reflect.New(m.Type().In(i))
			if err := json.Unmarshal(raw, v.Interface()); err != nil {
				h.fail("%s: argument %d: %v", req.Method, i, err)
			}
			in[i] = v.Elem()
		}
		out := m.Call(in)
		if n := len(out); n > 0 && out[n-1].Type() == reflect.TypeFor[error]() {
			if err, _ := out[n-1].Interface().(error); err != nil {
				reply["error"] = formatError(err)
			}
			out = out[:n-1]
		}
		if _, failed := reply["error"]; !failed && len(out) > 0 {
			reply["result"] = out[0].Interface()
		}
		if req.Method == "Cancel" {
			h.mu.Lock()
			if h.cancelled != nil {
				select {
				case <-h.cancelled:
				default:
					close(h.cancelled)
				}
			}
			h.mu.Unlock()
		}
	}
	h.mu.Lock()
	reply["seq"] = len(h.events)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (h *harness) serveCheck(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Arg string }
	reply := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply["error"] = err.Error()
	} else if v, err := h.check(req.Name, req.Arg); err != nil {
		reply["error"] = err.Error()
	} else {
		reply["value"] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// ---------------------------------------------------------------- checks

// check verifies the state on disk (and in the backend) after a UI step.
func (h *harness) check(name, arg string) (any, error) {
	fx := h.fx
	switch name {
	case "path":
		return fx.path(arg), nil
	case "pick":
		h.mu.Lock()
		h.picks = append(h.picks, fx.path(arg))
		h.mu.Unlock()
		return nil, nil
	case "pause-next-lock":
		h.mu.Lock()
		h.pause, h.paused, h.cancelled = true, "", make(chan struct{})
		h.mu.Unlock()
		return nil, nil
	case "vault-count":
		n, err := h.app.svc.VaultCount()
		if err == nil && fmt.Sprint(n) != arg {
			err = fmt.Errorf("%d vaults, want %s", n, arg)
		}
		return nil, err
	case "default-mode":
		if got := h.app.Settings().DefaultMode; got != arg {
			return nil, fmt.Errorf("default mode is %q", got)
		}
		return nil, nil
	case "closed":
		st, err := h.app.Status()
		if err == nil && st.Open {
			err = errors.New("the vault is still open")
		}
		return nil, err
	case "occupy":
		// Something new now lives where the folder used to be.
		dir := fx.path("home/" + arg)
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("created while the folder was locked\n"), 0o644); err != nil {
			return nil, err
		}
		tree, err := fingerprint(dir)
		fx.occupant = tree
		return nil, err
	}

	orig := fx.path("home/" + arg)
	var it core.Item
	var err error
	if slices.Contains([]string{"unlocked", "hidden", "locked", "unlisted", "restored-elsewhere", "cancelled"}, name) {
		if it, err = h.item(arg); err != nil && name != "unlisted" {
			return nil, err
		}
	}
	switch name {
	case "intact":
		return nil, fx.same(arg, orig)
	case "unlocked":
		if it.Locked || it.Path != orig {
			return nil, fmt.Errorf("item is %+v", it)
		}
		return nil, fx.same(arg, orig)
	case "gone":
		if fsx.Exists(orig) {
			return nil, fmt.Errorf("%s is still visible", orig)
		}
		return nil, nil
	case "hidden", "locked":
		want := core.StorageHidden
		if name == "locked" {
			want = core.StorageSealed
		}
		if !it.Locked || it.Storage != want {
			return nil, fmt.Errorf("item is %+v, want locked with storage %s", it, want)
		}
		if fsx.Exists(orig) {
			return nil, fmt.Errorf("%s is still visible", orig)
		}
		if name == "locked" {
			return nil, fx.noPlaintext(arg)
		}
		return nil, nil
	case "unlisted":
		if err == nil {
			return nil, fmt.Errorf("%s is still listed: %+v", arg, it)
		}
		return nil, nil
	case "restored-elsewhere":
		target := fx.path("restore/" + arg)
		if it.Locked || it.Path != target {
			return nil, fmt.Errorf("item is %+v, want unlocked at %s", it, target)
		}
		if err := fx.same(arg, target); err != nil {
			return nil, err
		}
		return nil, fx.sameTree(fx.occupant, orig, "the folder that took its place")
	case "cancelled":
		h.mu.Lock()
		paused := h.paused
		h.mu.Unlock()
		if paused == "" {
			return nil, errors.New("the lock finished or stopped before it was held half-way")
		}
		h.t.Logf("cancelled the lock during its %s phase", paused)
		if it.Locked || it.Busy || it.Problem != "" {
			return nil, fmt.Errorf("item is %+v, want unlocked and idle", it)
		}
		if err := fx.same(arg, orig); err != nil {
			return nil, err
		}
		return nil, fx.noLeftovers(it.ID)
	case "final":
		items, err := h.app.Items()
		if err != nil || len(items) > 0 {
			return nil, fmt.Errorf("items %+v, %v; want none", items, err)
		}
		for name, dir := range map[string]string{"Photos": "home/Photos", "Notes": "home/Notes", "Big": "home/Big", "Taxes": "restore/Taxes"} {
			if err := fx.same(name, fx.path(dir)); err != nil {
				return nil, err
			}
		}
		if err := fx.sameTree(fx.occupant, fx.path("home/Taxes"), "the folder that took the place of Taxes"); err != nil {
			return nil, err
		}
		return nil, fx.noLeftovers("")
	}
	return nil, fmt.Errorf("unknown check %q", name)
}

func (h *harness) item(name string) (core.Item, error) {
	items, err := h.app.Items()
	if err != nil {
		return core.Item{}, err
	}
	for _, it := range items {
		if it.Name == name {
			return it, nil
		}
	}
	return core.Item{}, fmt.Errorf("%s is not listed", name)
}

// ---------------------------------------------------------------- fixture

// fixture holds the folders the test protects and their original state.
type fixture struct {
	t        *testing.T
	root     string
	orig     map[string]map[string]string
	occupant map[string]string
}

// marker is written into every encrypted folder; it must never appear in
// the clear anywhere else.
func marker(name string) []byte { return []byte("VEILFOLD-E2E-PLAINTEXT-" + strings.ToUpper(name)) }

type entry struct {
	path string
	size int
	mode fs.FileMode
	link string
	dir  bool
}

func newFixture(t *testing.T, root string) *fixture {
	fx := &fixture{t: t, root: root, orig: map[string]map[string]string{}}
	for _, dir := range []string{"data", "home", "restore"} {
		if err := os.MkdirAll(fx.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx.build("Taxes", []entry{
		{path: "2026/报税 单.pdf", size: 100<<10 + 7},
		{path: "2026/receipts/r1.jpg", size: 64 << 10},
		{path: "2026/receipts/r2.jpg", size: 64<<10 + 1},
		{path: "notes.txt", size: 300},
		{path: "empty.txt"},
		{path: "big.bin", size: 3<<20 + 11},
		{path: "tools/run.sh", size: 300, mode: 0o755},
		{path: "private.key", size: 1200, mode: 0o600},
		{path: ".hidden", size: 10},
		{path: "Archive/old", dir: true},
		{path: "latest", link: "2026"},
	})
	fx.build("Photos", []entry{
		{path: "2025/IMG_0001.HEIC", size: 200 << 10},
		{path: "2025/IMG_0002.HEIC", size: 150 << 10},
		{path: "album.json", size: 2 << 10},
	})
	fx.build("Notes", []entry{
		{path: "todo.md", size: 1 << 10},
		{path: "ideas/one.md", size: 3 << 10},
	})
	var big []entry
	for i := 1; i <= 6; i++ {
		big = append(big, entry{path: fmt.Sprintf("part-%d.bin", i), size: 8 << 20})
	}
	fx.build("Big", big)
	return fx
}

func (fx *fixture) path(rel string) string { return filepath.Join(fx.root, filepath.FromSlash(rel)) }

func (fx *fixture) build(name string, entries []entry) {
	t := fx.t
	dir := fx.path("home/" + name)
	rnd := rand.NewChaCha8([32]byte([]byte(fmt.Sprintf("%-32s", name))))
	entries = append(entries, entry{path: "marker.txt", size: 64})
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.dir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		case e.link != "":
			if runtime.GOOS == "windows" {
				continue // creating symlinks needs extra rights there
			}
			if err := os.Symlink(e.link, p); err != nil {
				t.Fatal(err)
			}
		default:
			data := make([]byte, e.size)
			_, _ = rnd.Read(data)
			if e.path == "marker.txt" || e.path == "notes.txt" {
				data = append(marker(name), '\n')
			}
			mode := e.mode
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(p, data, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Fixed times, set after the contents, deepest first.
	when := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	var paths []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink == 0 {
			paths = append(paths, p)
		}
		return err
	})
	slices.Reverse(paths)
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	fx.orig[name] = tree
}

// fingerprint describes every entry of a tree: type, permissions, size,
// modification time and content.
func fingerprint(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch mt := info.ModTime().Unix(); {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link → " + filepath.ToSlash(target)
		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %v mtime %d", info.Mode().Perm(), mt)
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = fmt.Sprintf("file %v size %d mtime %d sha256 %s", info.Mode().Perm(), info.Size(), mt, hex.EncodeToString(sum[:8]))
		}
		return nil
	})
	return out, err
}

func (fx *fixture) same(name, dir string) error {
	return fx.sameTree(fx.orig[name], dir, name)
}

func (fx *fixture) sameTree(want map[string]string, dir, what string) error {
	got, err := fingerprint(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	var diffs []string
	for k, v := range want {
		if g, ok := got[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("missing %s (%s)", k, v))
		} else if g != v {
			diffs = append(diffs, fmt.Sprintf("%s: %s, want %s", k, g, v))
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("unexpected %s (%s)", k, v))
		}
	}
	if len(diffs) > 0 {
		slices.Sort(diffs)
		return fmt.Errorf("%s at %s differs from the original:\n  %s", what, dir, strings.Join(diffs, "\n  "))
	}
	return nil
}

// noPlaintext checks that no file outside the visible folders contains an
// encrypted folder's marker, and no name gives its files away.
func (fx *fixture) noPlaintext(name string) error {
	visible := []string{}
	for _, d := range []string{"home/Taxes", "home/Photos", "home/Notes", "home/Big", "restore/Taxes"} {
		if fsx.Exists(fx.path(d)) {
			visible = append(visible, fx.path(d))
		}
	}
	names := map[string]bool{}
	for rel := range fx.orig[name] {
		if base := filepath.Base(rel); base != "." && base != "marker.txt" {
			names[base] = true
		}
	}
	var found []string
	err := filepath.WalkDir(fx.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return fs.SkipDir // a concealed (hidden, not encrypted) folder
			}
			return err
		}
		if slices.Contains(visible, p) {
			return fs.SkipDir
		}
		if names[d.Name()] {
			found = append(found, "file name "+p)
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil && !errors.Is(err, fs.ErrPermission) {
				return err
			}
			if bytes.Contains(data, marker(name)) {
				found = append(found, "contents of "+p)
			}
		}
		return nil
	})
	if err == nil && len(found) > 0 {
		err = fmt.Errorf("%s is readable without the password: %s", name, strings.Join(found, ", "))
	}
	return err
}

// noLeftovers checks that no file in the stores belongs to the item (or,
// with id "", that the stores hold no files at all).
func (fx *fixture) noLeftovers(id string) error {
	var found []string
	for _, store := range []string{fx.path("data/store"), fx.path("home/.veilfold")} {
		err := filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if p != store && ((id != "" && strings.HasPrefix(d.Name(), id)) || (id == "" && !d.IsDir())) {
				found = append(found, p)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("leftover data in the store: %s", strings.Join(found, ", "))
	}
	return nil
}
