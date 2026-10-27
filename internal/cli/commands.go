package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/shenyizg/veilfold/internal/core"
	"github.com/shenyizg/veilfold/internal/seal"
	"github.com/shenyizg/veilfold/internal/version"
)

func (a *App) cmdInit(args []string) error {
	fs := a.flags("init", "", "Create a new vault protected by a password.\n\n"+
		"A computer can hold several vaults. Each password opens its own vault, and\n"+
		"nobody can tell which folders another vault protects.")
	noRecovery := fs.Bool("no-recovery-key", false, "do not create a recovery key")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	svc, err := a.service()
	if err != nil {
		return err
	}
	pw, err := a.newPassword()
	if err != nil {
		return err
	}
	defer seal.Wipe(pw)
	sess, recovery, err := svc.Create(pw, !*noRecovery)
	if err != nil {
		return err
	}
	defer sess.Close()
	fmt.Fprintln(a.Stdout, "Vault created.")
	if recovery != "" {
		a.printRecoveryKey(recovery)
	}
	fmt.Fprintln(a.Stdout, "Protect a folder with: veilfold add <folder>")
	return nil
}

func (a *App) printRecoveryKey(key string) {
	fmt.Fprintf(a.Stdout, "\nRecovery key:\n\n    %s\n\n"+
		"Write it down and keep it somewhere safe. It is the only way back in if\n"+
		"you forget your password, and it will not be shown again.\n\n", key)
}

func (a *App) cmdAdd(args []string) error {
	fs := a.flags("add", "<folder>...", "Protect folders. They are locked right away unless --no-lock is given.\n\n"+
		"Modes:\n"+
		"  encrypt  contents and names are encrypted; takes time and free space\n"+
		"           proportional to the folder's size (default)\n"+
		"  hide     the folder is moved to a concealed place; instant, but its\n"+
		"           contents are not encrypted")
	mode := fs.String("mode", string(core.ModeEncrypt), "protection `mode`: encrypt or hide")
	noLock := fs.Bool("no-lock", false, "only register the folders, do not lock them yet")
	folders, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		return usagef("no folder given")
	}
	m, err := core.ParseMode(*mode)
	if err != nil {
		return usagef("%v", err)
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	failed := false
	for _, f := range folders {
		it, err := sess.Add(f, m)
		if err != nil {
			fmt.Fprintf(a.Stderr, "veilfold: %v\n", err)
			failed = true
			continue
		}
		if *noLock {
			fmt.Fprintf(a.Stdout, "Added    %s (%s)\n", it.Path, it.Mode)
			continue
		}
		if err := a.lock(sess, it); err != nil {
			failed = true
		}
	}
	if failed {
		return errReported
	}
	return nil
}

// lock locks one item and reports the outcome.
func (a *App) lock(sess *core.Session, it core.Item) error {
	progress, clear := a.progress(it.Name)
	err := sess.Lock(a.ctx, it.ID, progress)
	clear()
	if err != nil {
		fmt.Fprintf(a.Stderr, "veilfold: locking %s: %v\n", it.Path, err)
		a.hint(err)
		return err
	}
	if it, err = sess.Find(it.ID); err == nil {
		fmt.Fprintf(a.Stdout, "Locked   %s (%s)\n", it.Path, describe(it))
	}
	return nil
}

func describe(it core.Item) string {
	if it.Storage == core.StorageSealed || (!it.Locked && it.Mode == core.ModeEncrypt) {
		if it.Locked {
			return fmt.Sprintf("encrypted, %d files, %s", it.Files, core.HumanBytes(it.Size))
		}
		return "encrypt mode"
	}
	if it.Locked {
		return "hidden"
	}
	return "hide mode"
}

func (a *App) cmdList(args []string) error {
	fs := a.flags("list", "", "List the folders protected by the vault.")
	asJSON := fs.Bool("json", false, "print the list as JSON")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	items, err := sess.Items()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.Stdout, "No protected folders yet. Protect one with: veilfold add <folder>")
		return nil
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tMODE\tSIZE\tPATH")
	for _, it := range items {
		state, size := "unlocked", "-"
		if it.Locked {
			state = "locked"
		}
		if it.Locked && it.Storage == core.StorageSealed {
			size = core.HumanBytes(it.Size)
		}
		mode := string(it.Mode)
		if it.Locked && it.Storage == core.StorageHidden && it.Mode == core.ModeEncrypt {
			mode = "hide*" // an interrupted lock fell back to hiding
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.ID[:8], state, mode, size, it.Path)
		if it.Problem != "" {
			fmt.Fprintf(tw, "\t\t\t\t  ! %s\n", it.Problem)
		}
	}
	return tw.Flush()
}

// resolve turns folder references (or --all) into items.
func (a *App) resolve(sess *core.Session, refs []string, all bool, want func(core.Item) bool) ([]core.Item, error) {
	if all == (len(refs) > 0) {
		return nil, usagef("name folders or use --all, not both or neither")
	}
	if all {
		items, err := sess.Items()
		if err != nil {
			return nil, err
		}
		var out []core.Item
		for _, it := range items {
			if want(it) {
				out = append(out, it)
			}
		}
		return out, nil
	}
	var out []core.Item
	for _, r := range refs {
		it, err := sess.Find(r)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

func (a *App) cmdLock(args []string) error {
	fs := a.flags("lock", "<folder>... | --all", "Lock protected folders. A folder can be given by its path, its name or\nthe start of its ID (see \"veilfold list\").")
	all := fs.Bool("all", false, "lock every unlocked folder")
	refs, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if *all == (len(refs) > 0) {
		return usagef("name folders or use --all, not both or neither")
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	items, err := a.resolve(sess, refs, *all, func(it core.Item) bool { return !it.Locked })
	if err != nil {
		return err
	}
	if *all && len(items) == 0 {
		fmt.Fprintln(a.Stdout, "All folders are already locked.")
	}
	failed := false
	for _, it := range items {
		if it.Locked {
			fmt.Fprintf(a.Stdout, "Already locked: %s\n", it.Path)
			continue
		}
		if err := a.lock(sess, it); err != nil {
			failed = true
		}
	}
	if failed {
		return errReported
	}
	return nil
}

func (a *App) cmdUnlock(args []string) error {
	fs := a.flags("unlock", "<folder>... | --all", "Unlock protected folders, restoring them to where they were.")
	all := fs.Bool("all", false, "unlock every locked folder")
	to := fs.String("to", "", "restore the folder to `PATH` instead (one folder only)")
	refs, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if *all == (len(refs) > 0) {
		return usagef("name folders or use --all, not both or neither")
	}
	if *to != "" && len(refs) != 1 {
		return usagef("--to needs exactly one folder")
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	items, err := a.resolve(sess, refs, *all, func(it core.Item) bool { return it.Locked })
	if err != nil {
		return err
	}
	if *all && len(items) == 0 {
		fmt.Fprintln(a.Stdout, "No folder is locked.")
	}
	failed := false
	for _, it := range items {
		if !it.Locked {
			fmt.Fprintf(a.Stdout, "Already unlocked: %s\n", it.Path)
			continue
		}
		progress, clear := a.progress(it.Name)
		err := sess.Unlock(a.ctx, it.ID, core.UnlockOptions{Target: *to}, progress)
		clear()
		if err != nil {
			fmt.Fprintf(a.Stderr, "veilfold: unlocking %s: %v\n", it.Path, err)
			if errors.Is(err, core.ErrTargetExists) {
				fmt.Fprintln(a.Stderr, "  (restore it elsewhere with --to <path>)")
			}
			a.hint(err)
			failed = true
			continue
		}
		if it, err := sess.Find(it.ID); err == nil {
			fmt.Fprintf(a.Stdout, "Unlocked %s\n", it.Path)
		}
	}
	if failed {
		return errReported
	}
	return nil
}

func (a *App) cmdRemove(args []string) error {
	fs := a.flags("remove", "<folder>...", "Stop protecting folders. Locked folders are unlocked first.")
	forget := fs.Bool("forget", false, "drop locked folders whose data can no longer be found")
	refs, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return usagef("no folder given")
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	failed := false
	for _, r := range refs {
		it, err := sess.Find(r)
		if err == nil {
			progress, clear := a.progress(it.Name)
			err = sess.Remove(a.ctx, it.ID, core.RemoveOptions{Forget: *forget}, progress)
			clear()
		}
		if err != nil {
			fmt.Fprintf(a.Stderr, "veilfold: %s: %v\n", r, err)
			a.hint(err)
			failed = true
			continue
		}
		fmt.Fprintf(a.Stdout, "Removed  %s (no longer protected)\n", it.Path)
	}
	if failed {
		return errReported
	}
	return nil
}

func (a *App) cmdMode(args []string) error {
	fs := a.flags("mode", "<folder> encrypt|hide", "Change how an unlocked folder is protected the next time it is locked.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("expected a folder and a mode")
	}
	m, err := core.ParseMode(pos[1])
	if err != nil {
		return usagef("%v", err)
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	it, err := sess.Find(pos[0])
	if err != nil {
		return err
	}
	if err := sess.SetMode(it.ID, m); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "%s will be protected in %s mode.\n", it.Path, m)
	return nil
}

func (a *App) cmdPasswd(args []string) error {
	fs := a.flags("passwd", "", "Change the vault's password. The recovery key stays valid.")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	defer seal.Wipe(pw)
	defer sess.Close()
	next, err := a.newPassword()
	if err != nil {
		return err
	}
	defer seal.Wipe(next)
	if err := sess.ChangePassword(pw, next); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "Password changed.")
	return nil
}

func (a *App) cmdRecover(args []string) error {
	fs := a.flags("recover", "", "Open a vault with its recovery key and set a new password.")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	svc, err := a.service()
	if err != nil {
		return err
	}
	key, err := a.secret("Recovery key: ")
	if err != nil {
		return err
	}
	defer seal.Wipe(key)
	sess, err := svc.OpenWithRecoveryKey(string(key))
	if err != nil {
		return err
	}
	defer sess.Close()
	for _, note := range sess.Notes() {
		fmt.Fprintln(a.Stderr, "note:", note)
	}
	next, err := a.newPassword()
	if err != nil {
		return err
	}
	defer seal.Wipe(next)
	if err := sess.ResetPassword(next); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "Password changed. Your recovery key is still valid; replace it with \"veilfold recovery-key\" if it may have been seen by someone else.")
	return nil
}

func (a *App) cmdRecoveryKey(args []string) error {
	fs := a.flags("recovery-key", "", "Create a new recovery key. The previous one stops working.")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	defer seal.Wipe(pw)
	defer sess.Close()
	key, err := sess.NewRecoveryKey(pw)
	if err != nil {
		return err
	}
	a.printRecoveryKey(key)
	return nil
}

func (a *App) cmdDestroy(args []string) error {
	fs := a.flags("destroy", "", "Delete a vault. Only possible once it protects no folders.")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	sess, pw, err := a.open()
	if err != nil {
		return err
	}
	seal.Wipe(pw)
	defer sess.Close()
	if !*yes {
		fmt.Fprint(a.Stderr, "Type \"destroy\" to delete this vault: ")
		answer, err := a.line()
		if err != nil {
			return err
		}
		if strings.TrimSpace(answer) != "destroy" {
			return errors.New("not confirmed; nothing was deleted")
		}
	}
	if err := sess.Destroy(); err != nil {
		if errors.Is(err, core.ErrNotEmpty) {
			return fmt.Errorf("%w; remove them first with \"veilfold remove\"", err)
		}
		return err
	}
	fmt.Fprintln(a.Stdout, "Vault deleted.")
	return nil
}

func (a *App) cmdInfo(args []string) error {
	fs := a.flags("info", "", "Show where Veilfold keeps its data. Does not need a password.")
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) > 0 {
		return usagef("unexpected argument %q", rest[0])
	}
	svc, err := a.service()
	if err != nil {
		return err
	}
	n, err := svc.VaultCount()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Version:         %s (%s)\n", version.Version, platform())
	fmt.Fprintf(a.Stdout, "Data directory:  %s\n", svc.DataDir())
	fmt.Fprintf(a.Stdout, "Default store:   %s\n", svc.StoreDir())
	fmt.Fprintf(a.Stdout, "Vaults:          %d\n", n)
	return nil
}

func (a *App) cmdVersion(args []string) error {
	fs := a.flags("version", "", "Print the version.")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "veilfold %s (%s)\n", version.Version, platform())
	return nil
}

func (a *App) cmdHelp(args []string) error {
	if len(args) == 0 {
		a.usage(a.Stdout)
		return nil
	}
	for _, c := range commands {
		if c.name == args[0] && c.name != "help" {
			return c.run(a, []string{"-h"})
		}
	}
	return usagef("unknown command %q", args[0])
}

// hint suggests a way out of errors that have one.
func (a *App) hint(err error) {
	if errors.Is(err, core.ErrUnresolved) {
		fmt.Fprintln(a.Stderr, "  (restore the folder to another location with: veilfold unlock --to <path> <folder>)")
	}
}
