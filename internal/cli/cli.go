// Package cli implements every kran command (SPEC §11). Owner: E.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/client"
)

// Hooks are the entry points owned by other packages. cmd/kran fills the ones
// that live outside this package (kran top), the rest default to the real
// implementations wired in wire.go.
type Hooks struct {
	// Top runs `kran top` (internal/tui.Run).
	Top func(ctx context.Context, c client.Client) error
}

// DefaultHooks is set by cmd/kran before Main runs.
var DefaultHooks Hooks

// App carries everything commands need, so tests can inject fakes.
type App struct {
	In     *os.File
	Out    io.Writer
	Err    io.Writer
	Env    func() []string
	Dial   func(ctx context.Context) (client.Client, error)
	Hooks  Hooks
	Self   func() (string, error) // path of the running kran binary
	Args0  string
	outTTY bool
	errTTY bool
}

// ExitError makes a command end with a specific exit code. Msg is printed on
// stderr (prefixed "kran: ") when non-empty.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// UsageError is a mistake in how kran was invoked (exit code 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(format, a...)} }

// NewApp returns an App wired to the real process.
func NewApp() *App {
	a := &App{
		In:    os.Stdin,
		Out:   os.Stdout,
		Err:   os.Stderr,
		Env:   os.Environ,
		Dial:  client.Dial,
		Hooks: DefaultHooks,
		Self:  selfPath,
	}
	a.detectTTY()
	return a
}

func (a *App) detectTTY() {
	if f, ok := a.Out.(*os.File); ok {
		a.outTTY = isTerminal(f)
	}
	if f, ok := a.Err.(*os.File); ok {
		a.errTTY = isTerminal(f)
	}
}

func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return p, nil
}

// Main runs the CLI with os.Args and returns the process exit code.
func Main(args []string) int {
	a := NewApp()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return a.Execute(ctx, args[1:])
}

// Execute runs one command line (without argv[0]) and returns the exit code.
func (a *App) Execute(ctx context.Context, args []string) int {
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	err := root.ExecuteContext(ctx)
	return a.exitCode(err)
}

func (a *App) exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if ee.Msg != "" {
			a.errorf("%s", ee.Msg)
		}
		return ee.Code
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		a.errorf("%s", ue.Msg)
		return 2
	}
	a.errorf("%s", err.Error())
	return 1
}

func (a *App) errorf(format string, args ...any) {
	fmt.Fprintf(a.Err, "kran: "+format+"\n", args...)
}

func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "kran",
		Short:         "Run heavy dev jobs on whichever of your machines has room",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `kran moves tests, builds, docker builds and GPU runs to whichever of your
machines has room, and never makes a job worse than running it locally.

  kran setup                 first-run wizard
  kran link devmac           link another machine over SSH
  kran run -- go test ./...  run where it fits; output and exit code are unchanged
  kran status                the mesh at a glance`,
		Version: versionString(),
	}
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return usagef("%s (see `kran %s --help`)", err.Error(), c.CommandPath()[len("kran "):])
	})
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.HiddenDefaultCmd = false

	root.AddCommand(
		a.runCommand(),
		a.statusCommand(),
		a.jobsCommand(),
		a.logsCommand(),
		a.cancelCommand(),
		a.drainCommand("drain"),
		a.drainCommand("resume"),
		a.uiCommand(),
		a.topCommand(),
		a.versionCommand(),
		a.daemonCommand(),
		a.serveCommand(),
		a.internalCommand(),
		a.hookCommand(),
	)
	a.addPlatformCommands(root)
	return root
}
