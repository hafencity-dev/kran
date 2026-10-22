// Package cli implements every kran command (SPEC §11).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/selfexe"
	"github.com/hafencity-dev/kran/internal/service"
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
	In    *os.File
	Out   io.Writer
	Err   io.Writer
	Env   func() []string
	Dial  func(ctx context.Context) (client.Client, error)
	Hooks Hooks
	// ServiceManager builds the service manager (nil: the real one).
	ServiceManager func(system bool) (service.Manager, error)
	Self           func() (string, error) // path of the running kran binary
	Args0          string
	outTTY         bool
	errTTY         bool
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

func selfPath() (string, error) { return selfexe.Path() }

// Main runs the CLI with os.Args and returns the process exit code.
func Main(args []string) int {
	a := NewApp()
	return a.Execute(context.Background(), args[1:])
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

	root.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 {
			msg := fmt.Sprintf("unknown command %q", args[0])
			if sug := c.SuggestionsFor(args[0]); len(sug) > 0 {
				msg += "; did you mean `kran " + sug[0] + "`?"
			} else {
				msg += "; see `kran --help`"
			}
			return usagef("%s", msg)
		}
		return c.Help()
	}
	root.AddGroup(
		&cobra.Group{ID: "jobs", Title: "Run and watch jobs:"},
		&cobra.Group{ID: "machines", Title: "Your machines:"},
		&cobra.Group{ID: "here", Title: "This machine:"},
	)
	root.SuggestionsMinimumDistance = 2
	cobra.EnableCommandSorting = false // the help lists commands in the order we add them
	root.Args = cobra.ArbitraryArgs    // RunE below reports unknown commands, with a suggestion
	root.AddCommand(
		a.runCommand(),
		a.statusCommand(),
		a.jobsCommand(),
		a.logsCommand(),
		a.cancelCommand(),
		a.topCommand(),
		a.uiCommand(),
		a.linkCommand(),
		a.unlinkCommand(),
		a.drainCommand("drain"),
		a.drainCommand("resume"),
		a.upgradeCommand(),
		a.setupCommand(),
		a.doctorCommand(),
		a.serviceCommand(),
		a.agentsCommand(),
		a.daemonCommand(),
		a.versionCommand(),
		a.serveCommand(),
		a.internalCommand(),
		a.hookCommand(),
	)
	for id, names := range map[string][]string{
		"jobs":     {"run", "status", "jobs", "logs", "cancel", "top", "ui"},
		"machines": {"link", "unlink", "drain", "resume", "upgrade"},
		"here":     {"setup", "doctor", "service", "agents", "daemon", "version"},
	} {
		for _, n := range names {
			if c, _, err := root.Find([]string{n}); err == nil && c != root {
				c.GroupID = id
			}
		}
	}
	root.SetHelpCommandGroupID("here")
	root.SetCompletionCommandGroupID("here")
	usageArgs(root)
	return root
}

// usageArgs turns cobra's argument-count errors into usage errors (exit 2)
// that say how to get help.
func usageArgs(c *cobra.Command) {
	if v := c.Args; v != nil && c.Parent() != nil {
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := v(cmd, args); err != nil {
				var ue *UsageError
				if errors.As(err, &ue) {
					return err
				}
				return usagef("%s (see `%s --help`)", err.Error(), cmd.CommandPath())
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		usageArgs(sub)
	}
}

// oneArg requires exactly one argument and says what it is and where to find
// it, instead of cobra's "accepts 1 arg(s), received 0".
func oneArg(what, hint string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return usagef("%s needs a %s (%s)", cmd.Name(), what, hint)
		case len(args) > 1:
			return usagef("%s takes one %s, got %d (see `%s --help`)", cmd.Name(), what, len(args), cmd.CommandPath())
		}
		return nil
	}
}
