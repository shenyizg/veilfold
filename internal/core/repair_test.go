package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crash is the panic value used to simulate the process dying at a
// failpoint. Deferred cleanups in the core must not mask it: real crashes
// do not run them either.
type crash struct{ point string }

func crashAt(t *testing.T, point string) {
	t.Helper()
	failpoint = func(name string) {
		if name == point {
			panic(crash{name})
		}
	}
	t.Cleanup(func() { failpoint = func(string) {} })
}

func expectCrash(t *testing.T, point string, op func() error) {
	t.Helper()
	defer func() {
		r := recover()
		if c, ok := r.(crash); !ok || c.point != point {
			t.Fatalf("expected a crash at %s, got %v", point, r)
		}
	}()
	err := op()
	t.Fatalf("operation returned (%v) without reaching %s", err, point)
}

func TestCrashRecovery(t *testing.T) {
	cases := []struct {
		mode       Mode
		unlock     bool
		point      string
		lockedThen bool // state after the repair
	}{
		{ModeHide, false, "hide:before-rename", false},
		{ModeHide, false, "hide:after-rename", true},
		{ModeHide, true, "unhide:before-rename", true},
		{ModeHide, true, "unhide:after-rename", false},
		{ModeEncrypt, false, "encrypt:before-stage", false},
		{ModeEncrypt, false, "encrypt:after-stage", false},
		{ModeEncrypt, false, "encrypt:written", false},
		{ModeEncrypt, false, "encrypt:sealed", true},
		{ModeEncrypt, false, "encrypt:committed", true},
		{ModeEncrypt, true, "decrypt:extracted", true},
		{ModeEncrypt, true, "decrypt:before-rename", true},
		{ModeEncrypt, true, "decrypt:renamed", false},
		{ModeEncrypt, true, "decrypt:committed", false},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.point, ":", "_"), func(t *testing.T) {
			ctx := context.Background()
			svc := newService(t)
			sess := newVault(t, svc, "pw")
			folder := makeFolder(t, "Crashy")
			want := tree(t, folder)
			it, err := sess.Add(folder, c.mode)
			if err != nil {
				t.Fatal(err)
			}
			if c.unlock {
				if err := sess.Lock(ctx, it.ID, nil); err != nil {
					t.Fatal(err)
				}
			}

			crashAt(t, c.point)
			expectCrash(t, c.point, func() error {
				if c.unlock {
					return sess.Unlock(ctx, it.ID, UnlockOptions{}, nil)
				}
				return sess.Lock(ctx, it.ID, nil)
			})
			failpoint = func(string) {}

			// "Restart": open the vault again, which repairs it.
			sess.Close()
			re, err := svc.Open([]byte("pw"))
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()
			t.Logf("repair notes: %q", re.Notes())
			got := mustItem(t, re, it.ID)
			if got.Busy {
				t.Fatal("operation still pending after repair")
			}
			if got.Locked != c.lockedThen {
				t.Fatalf("locked = %v after repair, want %v (%+v)", got.Locked, c.lockedThen, got)
			}
			if got.Locked == exists(folder) {
				t.Fatalf("locked = %v but folder visible = %v", got.Locked, exists(folder))
			}
			if got.Locked {
				if err := re.Unlock(ctx, it.ID, UnlockOptions{}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if have := tree(t, folder); have != want {
				t.Fatalf("data changed by the crash:\nwant\n%s\nhave\n%s", want, have)
			}
			if names := storeEntries(t, svc); len(names) != 0 {
				t.Fatalf("store not clean: %v", names)
			}
			if stray, _ := filepath.Glob(filepath.Join(filepath.Dir(folder), ".veilfold-*")); len(stray) != 0 {
				t.Fatalf("stray files: %v", stray)
			}
			// The folder can be locked again normally.
			if err := re.Lock(ctx, it.ID, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCrashWithLocationTaken(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Taken")
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)

	crashAt(t, "encrypt:written")
	expectCrash(t, "encrypt:written", func() error { return sess.Lock(ctx, it.ID, nil) })
	failpoint = func(string) {}

	// Meanwhile, something else is created at the original location.
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	re, err := svc.Open([]byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if len(re.Notes()) != 1 || !strings.Contains(re.Notes()[0], "hidden instead") {
		t.Fatalf("notes %q", re.Notes())
	}
	got := mustItem(t, re, it.ID)
	if !got.Locked || got.Storage != StorageHidden {
		t.Fatalf("item after repair: %+v", got)
	}
	alt := folder + " (restored)"
	if err := re.Unlock(ctx, it.ID, UnlockOptions{Target: alt}, nil); err != nil {
		t.Fatal(err)
	}
	if have := tree(t, alt); have != want {
		t.Fatalf("data changed:\nwant\n%s\nhave\n%s", want, have)
	}
	if data, err := os.ReadFile(filepath.Join(folder, "new.txt")); err != nil || string(data) != "new" {
		t.Fatal("the new folder at the original location was touched")
	}
}

func TestInterruptedUnlockKeepsDataWhenUnsure(t *testing.T) {
	// The decrypted folder was moved into place but the marker is gone and
	// the index still says "rename": the repair cannot prove the folder at
	// the target is ours, so it must keep the encrypted copy.
	ctx := context.Background()
	svc := newService(t)
	sess := newVault(t, svc, "pw")
	folder := makeFolder(t, "Unsure")
	want := tree(t, folder)
	it, _ := sess.Add(folder, ModeEncrypt)
	if err := sess.Lock(ctx, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	crashAt(t, "decrypt:renamed")
	expectCrash(t, "decrypt:renamed", func() error { return sess.Unlock(ctx, it.ID, UnlockOptions{}, nil) })
	failpoint = func(string) {}
	if err := os.Remove(filepath.Join(folder, markerName(it.ID))); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	re, err := svc.Open([]byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	got := mustItem(t, re, it.ID)
	if !got.Locked || !exists(filepath.Join(svc.StoreDir(), it.ID+blobExt)) {
		t.Fatalf("encrypted copy not kept: %+v", got)
	}
	// The data is still recoverable from the encrypted copy.
	alt := folder + " (copy)"
	if err := re.Unlock(ctx, it.ID, UnlockOptions{Target: alt}, nil); err != nil {
		t.Fatal(err)
	}
	if have := tree(t, alt); have != want {
		t.Fatal("data differs")
	}
}
