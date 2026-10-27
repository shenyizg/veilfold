package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shenyizg/veilfold/internal/core"
	"github.com/shenyizg/veilfold/internal/fsx"
	"github.com/shenyizg/veilfold/internal/seal"
)

var testKDF = seal.KDFParams{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

type result struct {
	out, err string
	code     int
}

// run executes the CLI in-process with passwords taken from stdin.
func run(t *testing.T, dataDir, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	a := &App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, kdf: &testKDF}
	code := a.Run(append([]string{"--data-dir", dataDir, "--password-stdin"}, args...))
	return result{out.String(), errb.String(), code}
}

func ok(t *testing.T, r result) result {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.out, r.err)
	}
	return r
}

func setup(t *testing.T) (dataDir, folder string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	t.Cleanup(func() { _ = fsx.RemoveAll(dataDir) })
	folder = filepath.Join(t.TempDir(), "Private Stuff")
	if err := os.MkdirAll(filepath.Join(folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "sub", "secret.txt"), []byte("the secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir, folder
}

func secretContent(t *testing.T, folder string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(folder, "sub", "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var recoveryKeyRE = regexp.MustCompile(`[0-9A-Z]{4}(-[0-9A-Z]{4}){7}`)

func TestWorkflow(t *testing.T) {
	data, folder := setup(t)
	const pw = "correct horse battery"

	r := ok(t, run(t, data, pw+"\n", "init"))
	recovery := recoveryKeyRE.FindString(r.out)
	if !strings.Contains(r.out, "Vault created") || recovery == "" {
		t.Fatalf("init output: %s", r.out)
	}

	r = ok(t, run(t, data, pw+"\n", "add", folder))
	if !strings.Contains(r.out, "Locked") || !strings.Contains(r.out, "encrypted") {
		t.Fatalf("add output: %s", r.out)
	}
	if _, err := os.Stat(folder); err == nil {
		t.Fatal("folder still visible after add")
	}

	r = ok(t, run(t, data, pw+"\n", "list"))
	if !strings.Contains(r.out, "locked") || !strings.Contains(r.out, folder) {
		t.Fatalf("list output: %s", r.out)
	}
	r = ok(t, run(t, data, pw+"\n", "list", "--json"))
	var items []core.Item
	if err := json.Unmarshal([]byte(r.out), &items); err != nil || len(items) != 1 || !items[0].Locked {
		t.Fatalf("list --json: %v %s", err, r.out)
	}

	ok(t, run(t, data, pw+"\n", "unlock", "private stuff"))
	if secretContent(t, folder) != "the secret" {
		t.Fatal("content changed")
	}
	ok(t, run(t, data, pw+"\n", "lock", "--all"))
	if _, err := os.Stat(folder); err == nil {
		t.Fatal("lock --all left the folder visible")
	}
	ok(t, run(t, data, pw+"\n", "unlock", "--all"))

	// Switch to hide mode and lock by ID prefix.
	ok(t, run(t, data, pw+"\n", "mode", folder, "hide"))
	ok(t, run(t, data, pw+"\n", "lock", items[0].ID[:6]))
	r = ok(t, run(t, data, pw+"\n", "list"))
	if !strings.Contains(r.out, "hide") {
		t.Fatalf("list after mode change: %s", r.out)
	}

	// Unlock somewhere else.
	alt := folder + " restored"
	ok(t, run(t, data, pw+"\n", "unlock", "--to", alt, items[0].ID))
	if secretContent(t, alt) != "the secret" {
		t.Fatal("content changed")
	}

	// Wrong password.
	r = run(t, data, "nope nope\n", "list")
	if r.code != 1 || !strings.Contains(r.err, "wrong password") {
		t.Fatalf("wrong password: %+v", r)
	}

	// Change the password, then reset it with the recovery key.
	ok(t, run(t, data, pw+"\nnew password 1\n", "passwd"))
	if r := run(t, data, pw+"\n", "list"); r.code == 0 {
		t.Fatal("old password still works")
	}
	ok(t, run(t, data, recovery+"\nnew password 2\n", "recover"))
	ok(t, run(t, data, "new password 2\n", "list"))

	// A new recovery key replaces the old one.
	r = ok(t, run(t, data, "new password 2\n", "recovery-key"))
	if fresh := recoveryKeyRE.FindString(r.out); fresh == "" || fresh == recovery {
		t.Fatalf("recovery-key output: %s", r.out)
	}
	if r := run(t, data, recovery+"\nnew password 3\n", "recover"); r.code == 0 {
		t.Fatal("old recovery key still works")
	}

	// Lock again, then remove: the folder comes back unprotected.
	ok(t, run(t, data, "new password 2\n", "lock", "--all"))
	ok(t, run(t, data, "new password 2\n", "remove", alt))
	if secretContent(t, alt) != "the secret" {
		t.Fatal("remove lost data")
	}
	r = ok(t, run(t, data, "new password 2\n", "list"))
	if !strings.Contains(r.out, "No protected folders") {
		t.Fatalf("list after remove: %s", r.out)
	}

	// Destroy needs confirmation.
	if r := run(t, data, "new password 2\nno\n", "destroy"); r.code == 0 {
		t.Fatal("destroyed without confirmation")
	}
	ok(t, run(t, data, "new password 2\ndestroy\n", "destroy"))
	r = ok(t, run(t, data, "", "info"))
	if !strings.Contains(r.out, "Vaults:          0") {
		t.Fatalf("info: %s", r.out)
	}
}

func TestAddHideAndNoLock(t *testing.T) {
	data, folder := setup(t)
	ok(t, run(t, data, "password1\n", "init", "--no-recovery-key"))
	r := ok(t, run(t, data, "password1\n", "add", "--mode", "hide", "--no-lock", folder))
	if !strings.Contains(r.out, "Added") {
		t.Fatalf("add --no-lock: %s", r.out)
	}
	if _, err := os.Stat(folder); err != nil {
		t.Fatal("--no-lock locked the folder")
	}
	r = ok(t, run(t, data, "password1\n", "lock", folder))
	if !strings.Contains(r.out, "hidden") {
		t.Fatalf("lock output: %s", r.out)
	}
}

func TestUsageErrors(t *testing.T) {
	data, folder := setup(t)
	cases := [][]string{
		{"frobnicate"},
		{"lock"},
		{"lock", "--all", "x"},
		{"add"},
		{"add", "--mode", "shred", folder},
		{"unlock", "--to", "x", "a", "b"},
		{"init", "extra"},
		{"list", "--bogus"},
	}
	for _, args := range cases {
		if r := run(t, data, "password1\n", args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %s)", args, r.code, r.err)
		}
	}
	if r := run(t, data, "short\n", "init"); r.code != 1 || !strings.Contains(r.err, "at least") {
		t.Fatalf("short password: %+v", r)
	}
	if r := run(t, data, "password1\n", "list"); r.code != 1 || !strings.Contains(r.err, "veilfold init") {
		t.Fatalf("list without vault: %+v", r)
	}
}

func TestHelpAndVersion(t *testing.T) {
	data, _ := setup(t)
	for _, args := range [][]string{{"help"}, {"help", "add"}, {"version"}, {"add", "--help"}} {
		r := ok(t, run(t, data, "", args...))
		if r.out == "" {
			t.Errorf("%v printed nothing", args)
		}
	}
	var out bytes.Buffer
	a := &App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}
	if code := a.Run([]string{"--version"}); code != 0 || !strings.Contains(out.String(), "veilfold") {
		t.Fatalf("--version: %d %s", code, out.String())
	}
}
