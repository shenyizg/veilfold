package core

import (
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

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/seal"
)

var testKDF = seal.KDFParams{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

func newService(t *testing.T) *Service {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	svc, err := NewService(Options{DataDir: dir, KDF: &testKDF})
	if err != nil {
		t.Fatal(err)
	}
	// Hidden folders have no permissions; remove them before TempDir's
	// cleanup trips over them.
	t.Cleanup(func() { _ = fsx.RemoveAll(dir) })
	return svc
}

func newVault(t *testing.T, svc *Service, password string) *Session {
	t.Helper()
	sess, _, err := svc.Create([]byte(password), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	return sess
}

// makeFolder creates a folder with assorted content below a fresh temp dir.
func makeFolder(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	files := map[string]string{
		"todo.txt":                 "buy milk",
		"photos/2026/beach.jpg":    strings.Repeat("JPEG", 100000),
		"文档/合同.pdf":                "PDF-1.7 机密",
		"nested/deeper/empty.file": "",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "empty dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("todo.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// tree describes the contents of a folder, excluding the root's mtime
// (moving a directory may change it on some systems).
func tree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
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
			line += " " + hex.EncodeToString(sum[:8]) + fmt.Sprint(" ", info.ModTime().UnixNano())
		case rel != ".":
			line += fmt.Sprint(" ", info.ModTime().UnixNano())
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func mustItem(t *testing.T, sess *Session, id string) Item {
	t.Helper()
	it, err := sess.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// storeEntries lists the names in the default store.
func storeEntries(t *testing.T, svc *Service) []string {
	t.Helper()
	entries, err := os.ReadDir(svc.StoreDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestVaultLifecycle(t *testing.T) {
	svc := newService(t)
	if n, _ := svc.VaultCount(); n != 0 {
		t.Fatalf("fresh data dir has %d vaults", n)
	}
	if _, err := svc.Open([]byte("x")); !errors.Is(err, ErrNoVault) {
		t.Fatalf("open without vaults: %v", err)
	}
	sess, recovery, err := svc.Create([]byte("alice password"), true)
	if err != nil {
		t.Fatal(err)
	}
	if recovery == "" {
		t.Fatal("no recovery key returned")
	}
	sess.Close()
	if _, err := sess.Items(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed session still usable: %v", err)
	}

	if _, err := svc.Open([]byte("wrong")); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	sess, err = svc.Open([]byte("alice password"))
	if err != nil {
		t.Fatal(err)
	}

	// Change the password.
	if err := sess.ChangePassword([]byte("wrong"), []byte("new")); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("change with wrong current password: %v", err)
	}
	if err := sess.ResetPassword([]byte("new")); !errors.Is(err, ErrRecoveryOnly) {
		t.Fatalf("reset without recovery key: %v", err)
	}
	if err := sess.ChangePassword([]byte("alice password"), []byte("alice new")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open([]byte("alice password")); !errors.Is(err, ErrWrongPassword) {
		t.Fatal("old password still works")
	}

	// Recover with the recovery key and reset the password.
	rs, err := svc.OpenWithRecoveryKey(strings.ToLower(recovery))
	if err != nil {
		t.Fatal(err)
	}
	if rs.VaultID() != sess.VaultID() || !rs.OpenedWithRecoveryKey() {
		t.Fatal("recovery key opened the wrong vault")
	}
	if err := rs.ResetPassword([]byte("alice reset")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open([]byte("alice reset")); err != nil {
		t.Fatal(err)
	}

	// A new recovery key replaces the old one.
	fresh, err := sess.NewRecoveryKey([]byte("alice reset"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenWithRecoveryKey(recovery); !errors.Is(err, ErrWrongRecoveryKey) {
		t.Fatal("old recovery key still works")
	}
	if _, err := svc.OpenWithRecoveryKey(fresh); err != nil {
		t.Fatal(err)
	}

	// A vault can only be destroyed when it is empty.
	if _, err := sess.Add(makeFolder(t, "Keep"), ModeHide); err != nil {
		t.Fatal(err)
	}
	if err := sess.Destroy(); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("destroyed a non-empty vault: %v", err)
	}
	items, _ := sess.Items()
	if err := sess.Remove(context.Background(), items[0].ID, RemoveOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sess.Destroy(); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.VaultCount(); n != 0 {
		t.Fatalf("%d vaults left after destroy", n)
	}
}

func TestVaultsAreIsolated(t *testing.T) {
	svc := newService(t)
	alice := newVault(t, svc, "alice")
	bob := newVault(t, svc, "bob")
	if _, _, err := svc.Create([]byte("alice"), false); !errors.Is(err, ErrPasswordInUse) {
		t.Fatalf("duplicate password: %v", err)
	}
	if err := bob.ChangePassword([]byte("bob"), []byte("alice")); !errors.Is(err, ErrPasswordInUse) {
		t.Fatalf("changing to another vault's password: %v", err)
	}
	folder := makeFolder(t, "AliceOnly")
	if _, err := alice.Add(folder, ModeEncrypt); err != nil {
		t.Fatal(err)
	}
	if items, _ := bob.Items(); len(items) != 0 {
		t.Fatalf("bob sees %d of alice's folders", len(items))
	}
	reopened, err := svc.Open([]byte("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.VaultID() != bob.VaultID() {
		t.Fatal("password opened the wrong vault")
	}
}

func TestEncryptLockUnlock(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Taxes 2026")
	want := tree(t, folder)

	it, err := sess.Add(folder, ModeEncrypt)
	if err != nil {
		t.Fatal(err)
	}
	var phases []Phase
	if err := sess.Lock(context.Background(), it.ID, func(p Progress) {
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(phases, []Phase{PhaseScan, PhaseEncrypt, PhaseVerify, PhaseCleanup}) {
		t.Fatalf("phases %v", phases)
	}
	if exists(folder) {
		t.Fatal("folder still visible after locking")
	}
	got := mustItem(t, sess, it.ID)
	if !got.Locked || got.Storage != StorageSealed || got.Files != 4 || got.Problem != "" {
		t.Fatalf("item after lock: %+v", got)
	}
	// Only one opaque blob is stored; neither names nor contents leak.
	names := storeEntries(t, svc)
	if !slices.Equal(names, []string{it.ID + blobExt}) {
		t.Fatalf("store contains %v", names)
	}
	blob, err := os.ReadFile(filepath.Join(svc.StoreDir(), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Taxes", "todo", "buy milk", "JPEGJPEG", "合同"} {
		if bytes.Contains(blob, []byte(secret)) {
			t.Fatalf("encrypted store leaks %q", secret)
		}
	}
	// Locking twice is harmless.
	if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
		t.Fatal(err)
	}

	if err := sess.Unlock(context.Background(), it.ID, UnlockOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, folder); got != want {
		t.Fatalf("restored folder differs:\nwant\n%s\ngot\n%s", want, got)
	}
	if names := storeEntries(t, svc); len(names) != 0 {
		t.Fatalf("store not empty after unlock: %v", names)
	}
	if exists(filepath.Join(folder, markerName(it.ID))) {
		t.Fatal("restore marker left behind")
	}
	if it := mustItem(t, sess, it.ID); it.Locked {
		t.Fatal("item still locked")
	}
}

func TestHideLockUnlock(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Diary")
	if err := os.Chmod(folder, 0o750); err != nil {
		t.Fatal(err)
	}
	want := tree(t, folder)
	it, err := sess.Add(folder, ModeHide)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if exists(folder) {
		t.Fatal("folder still visible after hiding")
	}
	hidden := filepath.Join(svc.StoreDir(), it.ID)
	fi, err := os.Lstat(hidden)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0 {
		t.Fatalf("hidden folder has mode %v, want no permissions", fi.Mode())
	}
	if err := sess.Unlock(context.Background(), it.ID, UnlockOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, folder); got != want {
		t.Fatalf("restored folder differs:\nwant\n%s\ngot\n%s", want, got)
	}
}

func TestUnlockTargets(t *testing.T) {
	for _, mode := range []Mode{ModeEncrypt, ModeHide} {
		t.Run(string(mode), func(t *testing.T) {
			svc := newService(t)
			sess := newVault(t, svc, "pw")
			folder := makeFolder(t, "Work")
			want := tree(t, folder)
			it, _ := sess.Add(folder, mode)
			if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
				t.Fatal(err)
			}
			// Something new appeared at the original location.
			if err := os.Mkdir(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := sess.Unlock(context.Background(), it.ID, UnlockOptions{}, nil); !errors.Is(err, ErrTargetExists) {
				t.Fatalf("unlock over existing folder: %v", err)
			}
			if err := sess.Unlock(context.Background(), it.ID, UnlockOptions{Target: filepath.Join(folder, "missing", "x")}, nil); !errors.Is(err, ErrParentMissing) {
				t.Fatalf("unlock below missing parent: %v", err)
			}
			alt := filepath.Join(filepath.Dir(folder), "Work (restored)")
			if err := sess.Unlock(context.Background(), it.ID, UnlockOptions{Target: alt}, nil); err != nil {
				t.Fatal(err)
			}
			if got := tree(t, alt); got != want {
				t.Fatalf("restored folder differs:\nwant\n%s\ngot\n%s", want, got)
			}
			if it := mustItem(t, sess, it.ID); it.Path != alt || it.Locked {
				t.Fatalf("item after unlock elsewhere: %+v", it)
			}
		})
	}
}

func TestProtectionRules(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	home, _ := os.UserHomeDir()
	parent := t.TempDir()
	file := filepath.Join(parent, "file.txt")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	folder := makeFolder(t, "Outer")
	inner := filepath.Join(folder, "photos")

	refused := map[string]string{
		"home":      home,
		"data dir":  svc.DataDir(),
		"file":      file,
		"missing":   filepath.Join(parent, "nope"),
		"temp root": os.TempDir(),
	}
	if runtime.GOOS != "windows" {
		refused["root"] = "/"
		refused["system"] = "/usr/bin"
		link := filepath.Join(parent, "link")
		_ = os.Symlink(folder, link)
		refused["symlink"] = link
	}
	for name, p := range refused {
		if _, err := sess.Add(p, ModeHide); err == nil {
			t.Errorf("%s (%s) was accepted", name, p)
		}
	}
	if _, err := sess.Add(folder, ModeHide); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Add(inner, ModeHide); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("nested folder: %v", err)
	}
	if _, err := sess.Add(filepath.Dir(folder), ModeHide); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("enclosing folder: %v", err)
	}
	if _, err := sess.Add(folder, "bogus"); err == nil {
		t.Fatal("bogus mode accepted")
	}
}

func TestFindAndProblems(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	a, _ := sess.Add(makeFolder(t, "Alpha"), ModeHide)
	b, _ := sess.Add(makeFolder(t, "Beta"), ModeEncrypt)
	for ref, want := range map[string]string{a.ID: a.ID, b.ID[:6]: b.ID, "beta": b.ID, a.Path: a.ID} {
		if it, err := sess.Find(ref); err != nil || it.ID != want {
			t.Errorf("Find(%q) = %v, %v", ref, it.ID, err)
		}
	}
	if _, err := sess.Find("gamma"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find unknown: %v", err)
	}
	if err := os.RemoveAll(a.Path); err != nil {
		t.Fatal(err)
	}
	if it := mustItem(t, sess, a.ID); it.Problem == "" {
		t.Fatal("missing folder not reported")
	}
	if err := sess.Lock(context.Background(), a.ID, nil); !errors.Is(err, ErrMissing) {
		t.Fatalf("locking a missing folder: %v", err)
	}
}

func TestRemoveAndForget(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Old")
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)
	if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := sess.Remove(context.Background(), it.ID, RemoveOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, folder); got != want {
		t.Fatal("remove did not restore the folder")
	}
	if items, _ := sess.Items(); len(items) != 0 {
		t.Fatal("item not removed")
	}

	// Forget only drops items whose data is really gone.
	it, _ = sess.Add(folder, ModeHide)
	if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sess.Remove(context.Background(), it.ID, RemoveOptions{Forget: true}, nil); !errors.Is(err, ErrTargetExists) {
		t.Fatalf("forget with data present: %v", err)
	}
	hidden := filepath.Join(svc.StoreDir(), it.ID)
	_ = os.Chmod(hidden, 0o700)
	if err := os.RemoveAll(hidden); err != nil {
		t.Fatal(err)
	}
	if err := sess.Remove(context.Background(), it.ID, RemoveOptions{Forget: true}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSetMode(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	it, _ := sess.Add(makeFolder(t, "Flip"), ModeEncrypt)
	if err := sess.SetMode(it.ID, ModeHide); err != nil {
		t.Fatal(err)
	}
	if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustItem(t, sess, it.ID); got.Storage != StorageHidden {
		t.Fatalf("storage %q after switching to hide mode", got.Storage)
	}
	if err := sess.SetMode(it.ID, ModeEncrypt); err == nil {
		t.Fatal("changed the mode of a locked folder")
	}
}

func TestBusy(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	it, _ := sess.Add(makeFolder(t, "Busy"), ModeHide)
	l, err := svc.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Lock(context.Background(), it.ID, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("lock while busy: %v", err)
	}
	_ = l.Unlock()
	if err := sess.Lock(context.Background(), it.ID, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCancelRollsBack(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Cancelled")
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)
	ctx, cancel := context.WithCancel(context.Background())
	err := sess.Lock(ctx, it.ID, func(p Progress) {
		if p.Phase == PhaseEncrypt && p.Done > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if got := tree(t, folder); got != want {
		t.Fatal("folder not restored after cancellation")
	}
	if names := storeEntries(t, svc); len(names) != 0 {
		t.Fatalf("store not cleaned up: %v", names)
	}
	if it := mustItem(t, sess, it.ID); it.Locked || it.Busy {
		t.Fatalf("item after cancel: %+v", it)
	}
}

func TestIndexBackup(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	if _, err := sess.Add(makeFolder(t, "One"), ModeHide); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Add(makeFolder(t, "Two"), ModeHide); err != nil {
		t.Fatal(err)
	}
	// Damage the current index: the backup, which is written first on
	// every save, holds the same state.
	if err := os.WriteFile(sess.manifestPath(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := sess.Items()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("backup index has %d items, want 2", len(items))
	}
	if err := os.WriteFile(sess.manifestPath()+".bak", []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Items(); err == nil {
		t.Fatal("unreadable index and backup were not reported")
	}
}

func TestInspect(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Look")
	in, err := sess.Inspect(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if in.Error != "" || !in.Complete || in.Stats.Files != 4 {
		t.Fatalf("inspection %+v", in)
	}
	home, _ := os.UserHomeDir()
	if in, _ := sess.Inspect(context.Background(), home); in.Error == "" {
		t.Fatal("home folder not refused")
	}
	cloud := filepath.Join(t.TempDir(), "OneDrive", "Stuff")
	_ = os.MkdirAll(cloud, 0o755)
	if in, _ := sess.Inspect(context.Background(), cloud); len(in.Warnings) == 0 {
		t.Fatal("no warning for a cloud-synced folder")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if in, _ := sess.Inspect(ctx, folder); in.Complete {
		t.Fatal("inspection with expired deadline reported complete")
	}
}
