// Package cli implements the veilfold command-line interface.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/shenyizg/veilfold/internal/core"
	"github.com/shenyizg/veilfold/internal/seal"
)

const minPasswordLength = 8

// App runs one invocation of the CLI.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// readSecret prompts for a secret without echoing it; nil if there is
	// no terminal.
	readSecret func(prompt string) ([]byte, error)
	// showProgress enables progress lines on Stderr.
	showProgress bool
	ctx          context.Context
	kdf          *seal.KDFParams // tests use cheap parameters

	dataDir       string
	passwordStdin bool
	lines         *bufio.Reader
}

// Main runs the CLI with the process's arguments and returns the exit code.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := &App{
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		readSecret:   terminalSecret,
		showProgress: term.IsTerminal(int(os.Stderr.Fd())),
		ctx:          ctx,
	}
	return a.Run(os.Args[1:])
}

type command struct {
	name    string
	summary string
	run     func(a *App, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"init", "Create a new vault", (*App).cmdInit},
		{"add", "Protect folders (they are locked right away)", (*App).cmdAdd},
		{"list", "List protected folders", (*App).cmdList},
		{"lock", "Lock protected folders", (*App).cmdLock},
		{"unlock", "Unlock protected folders", (*App).cmdUnlock},
		{"remove", "Stop protecting folders (unlocks them first)", (*App).cmdRemove},
		{"mode", "Change how a folder is protected", (*App).cmdMode},
		{"passwd", "Change the vault's password", (*App).cmdPasswd},
		{"recover", "Set a new password using the recovery key", (*App).cmdRecover},
		{"recovery-key", "Replace the vault's recovery key", (*App).cmdRecoveryKey},
		{"destroy", "Delete an empty vault", (*App).cmdDestroy},
		{"info", "Show where Veilfold keeps its data", (*App).cmdInfo},
		{"version", "Print the version", (*App).cmdVersion},
		{"help", "Show help for a command", (*App).cmdHelp},
	}
}

// errReported means the command already printed its errors.
var errReported = errors.New("errors were reported")

// usageError is a mistake in the command line.
type usageError struct {
	msg   string
	quiet bool // the flag package already explained it
}

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// Run executes args (without the program name) and returns the exit code.
func (a *App) Run(args []string) int {
	if a.ctx == nil {
		a.ctx = context.Background()
	}
	global := flag.NewFlagSet("veilfold", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	global.StringVar(&a.dataDir, "data-dir", "", "")
	global.BoolVar(&a.passwordStdin, "password-stdin", false, "")
	help := global.Bool("help", false, "")
	global.BoolVar(help, "h", false, "")
	showVersion := global.Bool("version", false, "")
	if err := global.Parse(args); err != nil {
		fmt.Fprintf(a.Stderr, "veilfold: %v\n\n", err)
		a.usage(a.Stderr)
		return 2
	}
	args = global.Args()
	switch {
	case *showVersion:
		args = []string{"version"}
	case *help:
		a.usage(a.Stdout)
		return 0
	case len(args) == 0:
		a.usage(a.Stderr)
		return 2
	}

	var cmd *command
	for i := range commands {
		if commands[i].name == args[0] {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		fmt.Fprintf(a.Stderr, "veilfold: unknown command %q\n\n", args[0])
		a.usage(a.Stderr)
		return 2
	}
	err := cmd.run(a, args[1:])
	var ue *usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errReported):
		return 1
	case errors.As(err, &ue):
		if !ue.quiet {
			fmt.Fprintf(a.Stderr, "veilfold %s: %v\nRun \"veilfold help %s\" for usage.\n", cmd.name, err, cmd.name)
		}
		return 2
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(a.Stderr, "veilfold: cancelled")
		return 1
	default:
		fmt.Fprintf(a.Stderr, "veilfold: %v\n", err)
		return 1
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprint(w, `Veilfold hides and locks private folders behind a password.

Usage:
  veilfold <command> [options] [arguments]

Commands:
`)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-14s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Options for every command:
  --data-dir DIR       use DIR as the data directory
  --password-stdin     read passwords from standard input, one per line

Run "veilfold help <command>" for details about a command.
`)
}

// flags creates the flag set of a command, including the global options.
func (a *App) flags(name, args, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&a.dataDir, "data-dir", a.dataDir, "use `DIR` as the data directory")
	fs.BoolVar(&a.passwordStdin, "password-stdin", a.passwordStdin, "read passwords from standard input, one per line")
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "Usage: veilfold %s [options]", name)
		if args != "" {
			fmt.Fprintf(w, " %s", args)
		}
		fmt.Fprintf(w, "\n\n%s\n\nOptions:\n", help)
		fs.VisitAll(func(f *flag.Flag) {
			arg, usage := flag.UnquoteUsage(f)
			left := "  --" + f.Name
			if arg != "" && arg != "value" {
				left += " " + arg
			}
			fmt.Fprintf(w, "%-24s %s\n", left, usage)
		})
	}
	return fs
}

// parse parses flags anywhere among the arguments and returns the
// positional arguments.
func (a *App) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		// Explicitly requested help goes to stdout, complaints to stderr.
		var msg bytes.Buffer
		fs.SetOutput(&msg)
		err := fs.Parse(args)
		if errors.Is(err, flag.ErrHelp) {
			_, _ = a.Stdout.Write(msg.Bytes())
			return nil, err
		}
		if err != nil {
			_, _ = a.Stderr.Write(msg.Bytes())
			return nil, &usageError{msg: err.Error(), quiet: true}
		}
		rest := fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (a *App) service() (*core.Service, error) {
	return core.NewService(core.Options{DataDir: a.dataDir, KDF: a.kdf})
}

func terminalSecret(prompt string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("standard input is not a terminal; use --password-stdin to pipe passwords")
	}
	fmt.Fprint(os.Stderr, prompt)
	defer fmt.Fprintln(os.Stderr)
	return term.ReadPassword(fd)
}

// line reads one line from standard input.
func (a *App) line() (string, error) {
	if a.lines == nil {
		a.lines = bufio.NewReader(a.Stdin)
	}
	s, err := a.lines.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		return "", errors.New("unexpected end of standard input")
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func (a *App) secret(prompt string) ([]byte, error) {
	if a.passwordStdin {
		s, err := a.line()
		return []byte(s), err
	}
	if a.readSecret == nil {
		return nil, errors.New("no terminal to ask for the password; use --password-stdin")
	}
	return a.readSecret(prompt)
}

func (a *App) newPassword() ([]byte, error) {
	pw, err := a.secret("New password: ")
	if err != nil {
		return nil, err
	}
	if len([]rune(string(pw))) < minPasswordLength {
		return nil, fmt.Errorf("the password must be at least %d characters long", minPasswordLength)
	}
	if !a.passwordStdin {
		again, err := a.secret("Repeat the new password: ")
		if err != nil {
			return nil, err
		}
		defer seal.Wipe(again)
		if !bytes.Equal(pw, again) {
			return nil, errors.New("the passwords do not match")
		}
	}
	return pw, nil
}

// open asks for the password and opens the matching vault. The caller
// must wipe the returned password.
func (a *App) open() (*core.Session, []byte, error) {
	svc, err := a.service()
	if err != nil {
		return nil, nil, err
	}
	if n, err := svc.VaultCount(); err != nil {
		return nil, nil, err
	} else if n == 0 {
		return nil, nil, errors.New(`there is no vault yet; create one with "veilfold init"`)
	}
	pw, err := a.secret("Password: ")
	if err != nil {
		return nil, nil, err
	}
	sess, err := svc.Open(pw)
	if err != nil {
		seal.Wipe(pw)
		return nil, nil, err
	}
	for _, note := range sess.Notes() {
		fmt.Fprintln(a.Stderr, "note:", note)
	}
	return sess, pw, nil
}

// progress returns a ProgressFunc drawing a status line, and a function
// that erases it.
func (a *App) progress(name string) (core.ProgressFunc, func()) {
	if !a.showProgress {
		return nil, func() {}
	}
	var last time.Time
	width := 0
	draw := func(line string) {
		pad := ""
		if n := width - len(line); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		fmt.Fprintf(a.Stderr, "\r%s%s", line, pad)
		width = len(line)
	}
	update := func(p core.Progress) {
		if now := time.Now(); now.Sub(last) >= 100*time.Millisecond || p.Done == p.Total {
			last = now
			line := fmt.Sprintf("  %s: %s", name, phaseLabel(p.Phase))
			if p.Total > 0 {
				line += fmt.Sprintf(" %3d%%  %s of %s", min(100, p.Done*100/p.Total),
					core.HumanBytes(p.Done), core.HumanBytes(p.Total))
			}
			draw(line)
		}
	}
	done := func() {
		if width > 0 {
			draw("")
			fmt.Fprint(a.Stderr, "\r")
		}
	}
	return update, done
}

func phaseLabel(p core.Phase) string {
	switch p {
	case core.PhaseScan:
		return "scanning"
	case core.PhaseEncrypt:
		return "encrypting"
	case core.PhaseVerify:
		return "verifying"
	case core.PhaseDecrypt:
		return "decrypting"
	case core.PhaseCleanup:
		return "cleaning up"
	}
	return string(p)
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }
