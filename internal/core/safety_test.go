package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/pack"
)

// These tests cover failure modes beyond clean crashes: saves that fail,
// cleanups that fail, folders that change while they are being locked, and
// data that the index does not account for.

func resetHooks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		failpoint = func(string) {}
		saveFault = func() error { return nil }
		removeAll = fsx.RemoveAll
	})
}

func reopen(t *testing.T, svc *Service, old *Session) *Session {
	t.Helper()
	old.Close()
	sess, err := svc.Open([]byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	return sess
}

func hasNote(sess *Session, substr string) bool {
	for _, n := range sess.Notes() {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// The commit fails, and the folder cannot be moved back because its parent
// was renamed in the meantime. Only the abort can be recorded. Nothing may be
// deleted, and the folder must remain restorable.
func TestFailedCommitWithAbortRecorded(t *testing.T) {
	resetHooks(t)
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Thesis")
	parent := filepath.Dir(folder)
	moved := parent + "-moved"
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)

	failpoint = func(name string) {
		if name != "encrypt:sealed" {
			return
		}
		if err := os.Rename(parent, moved); err != nil {
			t.Error(err)
		}
		fails := 1 // only the commit fails; the abort is recorded
		saveFault = func() error {
			if fails > 0 {
				fails--
				return errors.New("no space left on device")
			}
			return nil
		}
	}
	if err := sess.Lock(ctx, it.ID, nil); err == nil {
		t.Fatal("lock succeeded although its commit failed")
	}
	failpoint, saveFault = func(string) {}, func() error { return nil }
	stage := filepath.Join(svc.StoreDir(), it.ID+stageExt)
	if got := tree(t, stage); got != want {
		t.Fatal("staged copy damaged")
	}

	re := reopen(t, svc, sess)
	if got := mustItem(t, re, it.ID); !got.Busy {
		t.Fatalf("unresolved lock was dropped: %+v", got)
	}
	if got := tree(t, stage); got != want {
		t.Fatal("repair touched the staged copy")
	}
	if err := re.Lock(ctx, it.ID, nil); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("lock: got %v, want ErrUnresolved", err)
	}
	if err := re.Remove(ctx, it.ID, RemoveOptions{}, nil); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("remove: got %v, want ErrUnresolved", err)
	}
	alt := filepath.Join(moved, "Thesis (restored)")
	if err := re.Unlock(ctx, it.ID, UnlockOptions{Target: alt}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, alt); got != want {
		t.Fatal("restored folder differs")
	}
	if got := mustItem(t, re, it.ID); got.Locked || got.Busy || got.Path != alt {
		t.Fatalf("item after restoring: %+v", got)
	}
	if names := storeEntries(t, svc); len(names) != 0 {
		t.Fatalf("store not clean: %v", names)
	}
}

// Every save fails after sealing. The verified archive is complete, so
// finishing the lock on the next open is correct; the data must survive.
func TestFailedCommitWithoutAbortRecorded(t *testing.T) {
	resetHooks(t)
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Ledger")
	parent := filepath.Dir(folder)
	moved := parent + "-moved"
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)
	failpoint = func(name string) {
		if name == "encrypt:sealed" {
			if err := os.Rename(parent, moved); err != nil {
				t.Error(err)
			}
			saveFault = func() error { return errors.New("I/O error") }
		}
	}
	if err := sess.Lock(ctx, it.ID, nil); err == nil {
		t.Fatal("lock succeeded although saving failed")
	}
	failpoint, saveFault = func(string) {}, func() error { return nil }

	re := reopen(t, svc, sess)
	got := mustItem(t, re, it.ID)
	if !got.Locked || got.Busy || got.Storage != StorageSealed {
		t.Fatalf("item after repair: %+v", got)
	}
	if err := re.Unlock(ctx, it.ID, UnlockOptions{}, nil); !errors.Is(err, ErrParentMissing) {
		t.Fatalf("unlock without parent: %v", err)
	}
	alt := filepath.Join(moved, "Ledger")
	if err := re.Unlock(ctx, it.ID, UnlockOptions{Target: alt}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, alt); got != want {
		t.Fatal("restored folder differs")
	}
}

func TestUnresolvedRepairBlocksOperations(t *testing.T) {
	resetHooks(t)
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Stuck")
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)
	crashAt(t, "encrypt:committed")
	expectCrash(t, "encrypt:committed", func() error { return sess.Lock(ctx, it.ID, nil) })
	failpoint = func(string) {}

	// The unencrypted staging copy cannot be deleted (a file in use, say).
	removeAll = func(p string) error {
		if strings.HasSuffix(p, stageExt) && exists(p) {
			return errors.New("the file is in use")
		}
		return fsx.RemoveAll(p)
	}
	re := reopen(t, svc, sess)
	if !hasNote(re, "could not be deleted") {
		t.Fatalf("notes %q", re.Notes())
	}
	if got := mustItem(t, re, it.ID); !got.Busy || !got.Locked {
		t.Fatalf("item: %+v", got)
	}
	for name, op := range map[string]func() error{
		"unlock": func() error { return re.Unlock(ctx, it.ID, UnlockOptions{}, nil) },
		"remove": func() error { return re.Remove(ctx, it.ID, RemoveOptions{}, nil) },
		"lock":   func() error { return re.Lock(ctx, it.ID, nil) },
	} {
		if err := op(); !errors.Is(err, ErrUnresolved) {
			t.Errorf("%s: got %v, want ErrUnresolved", name, err)
		}
	}

	removeAll = fsx.RemoveAll
	re = reopen(t, svc, re)
	if got := mustItem(t, re, it.ID); got.Busy || !got.Locked {
		t.Fatalf("item after repair: %+v", got)
	}
	if err := re.Unlock(ctx, it.ID, UnlockOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, folder); got != want {
		t.Fatal("data differs")
	}
}

func TestFailedUnlockKeepsTrackOfRestoreFolder(t *testing.T) {
	resetHooks(t)
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Restore")
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)
	if err := sess.Lock(ctx, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	failpoint = func(name string) {
		if name == "decrypt:before-rename" {
			// Something appears at the destination, so the rename fails.
			if err := os.MkdirAll(filepath.Join(folder, "new"), 0o755); err != nil {
				t.Error(err)
			}
		}
	}
	removeAll = func(p string) error {
		if strings.HasSuffix(p, restoreExt) && exists(p) {
			return errors.New("the file is in use")
		}
		return fsx.RemoveAll(p)
	}
	err := sess.Unlock(ctx, it.ID, UnlockOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "partly restored copy") {
		t.Fatalf("unlock: %v", err)
	}
	restore := filepath.Join(filepath.Dir(folder), ".veilfold-"+it.ID+restoreExt)
	if !exists(restore) {
		t.Fatal("restore folder vanished")
	}
	if got := mustItem(t, sess, it.ID); !got.Busy || !got.Locked {
		t.Fatalf("restore folder no longer tracked: %+v", got)
	}

	failpoint, removeAll = func(string) {}, fsx.RemoveAll
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	re := reopen(t, svc, sess)
	if exists(restore) {
		t.Fatal("repair did not delete the restore folder")
	}
	if err := re.Unlock(ctx, it.ID, UnlockOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, folder); got != want {
		t.Fatal("data differs")
	}
}

func TestChangeDuringLockIsNotLost(t *testing.T) {
	resetHooks(t)
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Live")
	it, _ := sess.Add(folder, ModeEncrypt)
	failpoint = func(name string) {
		if name != "encrypt:written" {
			return
		}
		// A program that kept the file open appends to it after it was
		// archived.
		stage := filepath.Join(svc.StoreDir(), it.ID+stageExt)
		f, err := os.OpenFile(filepath.Join(stage, "todo.txt"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.WriteString(" and eggs")
		_ = f.Close()
	}
	if err := sess.Lock(context.Background(), it.ID, nil); !errors.Is(err, pack.ErrChanged) {
		t.Fatalf("lock: got %v, want ErrChanged", err)
	}
	data, err := os.ReadFile(filepath.Join(folder, "todo.txt"))
	if err != nil || string(data) != "buy milk and eggs" {
		t.Fatalf("the late change was lost: %q, %v", data, err)
	}
	if got := mustItem(t, sess, it.ID); got.Locked || got.Busy {
		t.Fatalf("item: %+v", got)
	}
	if names := storeEntries(t, svc); len(names) != 0 {
		t.Fatalf("store not clean: %v", names)
	}
}

func TestLeftoversAreReported(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	it, _ := sess.Add(makeFolder(t, "Orphan"), ModeHide)
	stray := filepath.Join(svc.StoreDir(), it.ID+stageExt)
	if err := os.MkdirAll(filepath.Join(stray, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(svc.StoreDir(), it.ID+blobExt+partialExt)
	if err := os.WriteFile(partial, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	re := reopen(t, svc, sess)
	if !hasNote(re, stray) {
		t.Fatalf("leftover not reported: %q", re.Notes())
	}
	if !exists(stray) {
		t.Fatal("leftover data was deleted")
	}
	if exists(partial) {
		t.Fatal("incomplete archive was kept")
	}
}

func TestStoreDirectoriesAreVerified(t *testing.T) {
	svc := newService(t)
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "planted")
	if err := os.Symlink(elsewhere, link); err == nil {
		if _, err := svc.ensureStore(link); err == nil {
			t.Fatal("a symbolic link was accepted as a store")
		}
	}
	if runtime.GOOS != "windows" {
		loose := filepath.Join(base, "loose")
		if err := os.Mkdir(loose, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ensureStore(loose); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o700 {
			t.Fatalf("store mode %v, want 0700", fi.Mode().Perm())
		}
	}
}

func TestReplacedArchiveIsRejected(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Swap")
	it, _ := sess.Add(folder, ModeEncrypt)
	if err := sess.Lock(ctx, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(svc.StoreDir(), it.ID+blobExt)
	old, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Unlock(ctx, it.ID, UnlockOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sess.Lock(ctx, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, old, 0o600); err != nil {
		t.Fatal(err)
	}
	err = sess.Unlock(ctx, it.ID, UnlockOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("an older archive was accepted: %v", err)
	}
	if got := mustItem(t, sess, it.ID); !got.Locked || got.Busy || exists(folder) {
		t.Fatalf("item after refusal: %+v", got)
	}
}

func TestFolderContainingStoreIsRefused(t *testing.T) {
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Container")
	if err := os.Mkdir(filepath.Join(folder, volumeStoreName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Add(folder, ModeHide); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v, want ErrNotAllowed", err)
	}
}
