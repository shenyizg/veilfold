package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/shenyizg/veilfold/internal/core"
	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/seal"
	"github.com/shenyizg/veilfold/internal/version"
)

// App is the backend of the desktop app. Its exported methods are callable
// from the frontend as window.go.main.App.<Method>.
type App struct {
	ctx      context.Context
	svc      *core.Service
	settings *settingsStore
	// emit and pickFolder reach the window. Tests replace them.
	emit       func(name string, data ...any)
	pickFolder func(title string) (string, error)

	mu       sync.Mutex
	sess     *core.Session
	busy     bool
	cancel   context.CancelFunc
	quitting bool
}

// NewApp creates the backend for the default data directory.
func NewApp() (*App, error) {
	svc, err := core.NewService(core.Options{})
	if err != nil {
		return nil, err
	}
	return newApp(svc), nil
}

func newApp(svc *core.Service) *App {
	a := &App{svc: svc, settings: newSettingsStore(svc.DataDir())}
	a.emit = func(name string, data ...any) { wruntime.EventsEmit(a.ctx, name, data...) }
	a.pickFolder = func(title string) (string, error) {
		return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
			Title:                title,
			CanCreateDirectories: true,
			ShowHiddenFiles:      false,
		})
	}
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	if a.sess != nil {
		a.sess.Close()
		a.sess = nil
	}
}

func (a *App) secondInstance(_ interface{}) {
	wruntime.WindowUnminimise(a.ctx)
	wruntime.Show(a.ctx)
}

// beforeClose keeps the window open while an operation runs, and lets the
// user decide what to do with unlocked folders.
func (a *App) beforeClose(context.Context) bool {
	a.mu.Lock()
	quitting, busy, sess := a.quitting, a.busy, a.sess
	a.mu.Unlock()
	if quitting {
		return false
	}
	if busy {
		a.emit("quit:busy")
		return true
	}
	if sess != nil {
		if items, err := sess.Items(); err == nil && unlockedCount(items) > 0 {
			a.emit("quit:confirm", unlockedCount(items))
			return true
		}
	}
	return false
}

func unlockedCount(items []core.Item) int {
	n := 0
	for _, it := range items {
		if !it.Locked {
			n++
		}
	}
	return n
}

// formatError turns errors into a JSON string with a machine-readable code,
// which the frontend parses.
func formatError(err error) any {
	code := "error"
	switch {
	case errors.Is(err, core.ErrWrongPassword):
		code = "wrong_password"
	case errors.Is(err, core.ErrWrongRecoveryKey), errors.Is(err, seal.ErrRecoveryKeyFormat):
		code = "wrong_recovery_key"
	case errors.Is(err, core.ErrPasswordInUse):
		code = "password_in_use"
	case errors.Is(err, core.ErrBusy):
		code = "busy"
	case errors.Is(err, core.ErrTargetExists):
		code = "target_exists"
	case errors.Is(err, core.ErrParentMissing):
		code = "parent_missing"
	case errors.Is(err, core.ErrNotAllowed):
		code = "not_allowed"
	case errors.Is(err, core.ErrInsufficientSpace):
		code = "no_space"
	case errors.Is(err, core.ErrInUse):
		code = "in_use"
	case errors.Is(err, core.ErrDataMissing):
		code = "data_missing"
	case errors.Is(err, core.ErrIncomplete):
		code = "incomplete"
	case errors.Is(err, core.ErrClosed):
		code = "closed"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	}
	b, _ := json.Marshal(map[string]string{"code": code, "message": err.Error()})
	return string(b)
}

// Status describes the app's state for the frontend.
type Status struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	HasVault bool   `json:"hasVault"`
	Open     bool   `json:"open"`
	Recovery bool   `json:"recovery"`
	DataDir  string `json:"dataDir"`
}

// Status returns the current state.
func (a *App) Status() (Status, error) {
	n, err := a.svc.VaultCount()
	if err != nil {
		return Status{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Version:  version.Version,
		Platform: runtime.GOOS,
		HasVault: n > 0,
		Open:     a.sess != nil,
		DataDir:  a.svc.DataDir(),
	}
	if a.sess != nil {
		st.Recovery = a.sess.OpenedWithRecoveryKey()
	}
	return st, nil
}

func (a *App) setSession(s *core.Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess != nil {
		a.sess.Close()
	}
	a.sess = s
}

func (a *App) session() (*core.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess == nil {
		return nil, core.ErrClosed
	}
	return a.sess, nil
}

// OpenResult is returned when a vault was opened.
type OpenResult struct {
	Notes []string `json:"notes"`
}

// CreateVault creates a vault, opens it and returns its recovery key ("" if
// none was requested).
func (a *App) CreateVault(password string, recoveryKey bool) (string, error) {
	pw := []byte(password)
	defer seal.Wipe(pw)
	sess, key, err := a.svc.Create(pw, recoveryKey)
	if err != nil {
		return "", err
	}
	a.setSession(sess)
	return key, nil
}

// OpenVault opens the vault the password belongs to.
func (a *App) OpenVault(password string) (OpenResult, error) {
	pw := []byte(password)
	defer seal.Wipe(pw)
	sess, err := a.svc.Open(pw)
	if err != nil {
		return OpenResult{}, err
	}
	a.setSession(sess)
	return OpenResult{Notes: sess.Notes()}, nil
}

// OpenWithRecoveryKey opens a vault with its recovery key; the frontend
// then asks for a new password (ResetPassword).
func (a *App) OpenWithRecoveryKey(key string) (OpenResult, error) {
	sess, err := a.svc.OpenWithRecoveryKey(key)
	if err != nil {
		return OpenResult{}, err
	}
	a.setSession(sess)
	return OpenResult{Notes: sess.Notes()}, nil
}

// ResetPassword sets a new password after opening with the recovery key.
func (a *App) ResetPassword(password string) error {
	sess, err := a.session()
	if err != nil {
		return err
	}
	pw := []byte(password)
	defer seal.Wipe(pw)
	return sess.ResetPassword(pw)
}

// ChangePassword replaces the vault's password.
func (a *App) ChangePassword(current, next string) error {
	sess, err := a.session()
	if err != nil {
		return err
	}
	cur, nxt := []byte(current), []byte(next)
	defer seal.Wipe(cur)
	defer seal.Wipe(nxt)
	return sess.ChangePassword(cur, nxt)
}

// HasRecoveryKey reports whether the open vault has a recovery key.
func (a *App) HasRecoveryKey() (bool, error) {
	sess, err := a.session()
	if err != nil {
		return false, err
	}
	return sess.HasRecoveryKey()
}

// NewRecoveryKey replaces the vault's recovery key.
func (a *App) NewRecoveryKey(password string) (string, error) {
	sess, err := a.session()
	if err != nil {
		return "", err
	}
	pw := []byte(password)
	defer seal.Wipe(pw)
	return sess.NewRecoveryKey(pw)
}

// CloseVault forgets the vault's key.
func (a *App) CloseVault() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return core.ErrBusy
	}
	if a.sess != nil {
		a.sess.Close()
		a.sess = nil
	}
	return nil
}

// Items lists the protected folders.
func (a *App) Items() ([]core.Item, error) {
	sess, err := a.session()
	if err != nil {
		return nil, err
	}
	return sess.Items()
}

// ChooseFolder shows a folder picker and returns "" if it was cancelled.
func (a *App) ChooseFolder(title string) (string, error) {
	return a.pickFolder(title)
}

// Inspect checks a folder before protecting it. Measuring its size stops
// after a moment for very large folders.
func (a *App) Inspect(path string) (core.Inspection, error) {
	sess, err := a.session()
	if err != nil {
		return core.Inspection{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	return sess.Inspect(ctx, path)
}

// OpInfo announces a long-running operation.
type OpInfo struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OpProgress reports the progress of the running operation.
type OpProgress struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// run executes one folder operation at a time, streaming progress events.
func (a *App) run(kind string, it core.Item, fn func(ctx context.Context, sess *core.Session, progress core.ProgressFunc) error) error {
	a.mu.Lock()
	if a.sess == nil {
		a.mu.Unlock()
		return core.ErrClosed
	}
	if a.busy {
		a.mu.Unlock()
		return core.ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.busy, a.cancel = true, cancel
	sess := a.sess
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.busy, a.cancel = false, nil
		a.mu.Unlock()
		a.emit("op:end", it.ID)
	}()

	a.emit("op:start", OpInfo{Kind: kind, ID: it.ID, Name: it.Name})
	var last time.Time
	progress := func(p core.Progress) {
		if time.Since(last) < 80*time.Millisecond && p.Done != p.Total {
			return
		}
		last = time.Now()
		a.emit("op:progress", OpProgress{ID: it.ID, Phase: string(p.Phase), Done: p.Done, Total: p.Total})
	}
	return fn(ctx, sess, progress)
}

func (a *App) find(id string) (core.Item, error) {
	sess, err := a.session()
	if err != nil {
		return core.Item{}, err
	}
	return sess.Find(id)
}

// Protect registers a folder and, if lockNow is set, locks it.
func (a *App) Protect(path, mode string, lockNow bool) (core.Item, error) {
	sess, err := a.session()
	if err != nil {
		return core.Item{}, err
	}
	m, err := core.ParseMode(mode)
	if err != nil {
		return core.Item{}, err
	}
	it, err := sess.Add(path, m)
	if err != nil || !lockNow {
		return it, err
	}
	return it, a.Lock(it.ID)
}

// Lock locks one folder.
func (a *App) Lock(id string) error {
	it, err := a.find(id)
	if err != nil {
		return err
	}
	return a.run("lock", it, func(ctx context.Context, sess *core.Session, p core.ProgressFunc) error {
		return sess.Lock(ctx, id, p)
	})
}

// Unlock restores one folder to its location.
func (a *App) Unlock(id string) error {
	it, err := a.find(id)
	if err != nil {
		return err
	}
	return a.run("unlock", it, func(ctx context.Context, sess *core.Session, p core.ProgressFunc) error {
		return sess.Unlock(ctx, id, core.UnlockOptions{}, p)
	})
}

// UnlockTo asks for a destination folder and restores the folder into it.
// It returns false if the user cancelled the picker.
func (a *App) UnlockTo(id string) (bool, error) {
	it, err := a.find(id)
	if err != nil {
		return false, err
	}
	dir, err := a.ChooseFolder(fmt.Sprintf("Choose where to restore “%s”", it.Name))
	if err != nil || dir == "" {
		return false, err
	}
	target := uniquePath(dir, it.Name)
	return true, a.run("unlock", it, func(ctx context.Context, sess *core.Session, p core.ProgressFunc) error {
		return sess.Unlock(ctx, id, core.UnlockOptions{Target: target}, p)
	})
}

// uniquePath returns dir/name, or dir/"name (2)" and so on if taken.
func uniquePath(dir, name string) string {
	p := joinPath(dir, name)
	for i := 2; fsx.Exists(p) && i < 1000; i++ {
		p = joinPath(dir, fmt.Sprintf("%s (%d)", name, i))
	}
	return p
}

// LockAll locks every unlocked folder, one after the other.
func (a *App) LockAll() error {
	sess, err := a.session()
	if err != nil {
		return err
	}
	items, err := sess.Items()
	if err != nil {
		return err
	}
	var errs []string
	for _, it := range items {
		if it.Locked {
			continue
		}
		if err := a.Lock(it.ID); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", it.Name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

// Remove stops protecting a folder (unlocking it first if needed).
func (a *App) Remove(id string) error {
	it, err := a.find(id)
	if err != nil {
		return err
	}
	return a.run("remove", it, func(ctx context.Context, sess *core.Session, p core.ProgressFunc) error {
		return sess.Remove(ctx, id, core.RemoveOptions{}, p)
	})
}

// Forget drops a locked folder whose data no longer exists.
func (a *App) Forget(id string) error {
	sess, err := a.session()
	if err != nil {
		return err
	}
	return sess.Remove(context.Background(), id, core.RemoveOptions{Forget: true}, nil)
}

// SetMode changes how an unlocked folder will be protected.
func (a *App) SetMode(id, mode string) error {
	sess, err := a.session()
	if err != nil {
		return err
	}
	m, err := core.ParseMode(mode)
	if err != nil {
		return err
	}
	return sess.SetMode(id, m)
}

// Reveal opens an unlocked folder in Finder or Explorer.
func (a *App) Reveal(id string) error {
	it, err := a.find(id)
	if err != nil {
		return err
	}
	if it.Locked {
		return errors.New("unlock the folder first")
	}
	return fsx.OpenInFileManager(it.Path)
}

// Cancel stops the running operation; it rolls back safely.
func (a *App) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// Quit closes the app, after locking every folder if lockFirst is set.
func (a *App) Quit(lockFirst bool) error {
	if lockFirst {
		if err := a.LockAll(); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	wruntime.Quit(a.ctx)
	return nil
}

// Settings returns the app's preferences.
func (a *App) Settings() Settings { return a.settings.get() }

// SaveSettings stores the app's preferences.
func (a *App) SaveSettings(s Settings) error { return a.settings.set(s) }

// OpenProjectPage opens the project's website in the browser.
func (a *App) OpenProjectPage() {
	wruntime.BrowserOpenURL(a.ctx, projectURL)
}
