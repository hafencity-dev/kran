package cli

import (
	"context"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/workspace"
)

func (a *App) runCommand() *cobra.Command {
	var f RunFlags
	cmd := &cobra.Command{
		Use:     "run [flags] [--] command [args…]",
		Aliases: []string{"r"},
		Short:   "Run a command where it fits (here or on a linked machine)",
		Long: `Run a command on whichever of your machines has room. Output, colors and the
exit code are the same as running it directly. If kran cannot place the job it
runs it here, so it is never worse than running it yourself.

Everything after the first non-flag argument (or after --) is the command.
kran says something on stderr only when it adds information, as one line
starting with "kran:".

A job that runs on another machine works on a synced copy of your files: files
it creates stay there. Use --local when you need them here.

Exit codes: the command's own; 125 kran failed after the command started (or
--remote/--no-fallback could not be honored); 126 not executable; 127 not
found; 128+n killed by signal n; 2 usage error.`,
		Example: `  kran run -- go test ./...
  kran run go build ./...                 # -- is optional
  kran run --sh 'pnpm install && pnpm build | tail -n 20'
  kran run --mem 12G --cpus 8 -- cargo test
  kran run -n devmac -- docker build .    # pin a machine
  kran run --local -- ./build.sh          # keep the output files here
  kran run --json-events -- make test     # progress as JSON lines on stderr`,
		Args:                  cobra.ArbitraryArgs,
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
		RunE: func(cmd *cobra.Command, args []string) error {
			code := a.doRun(cmd.Context(), f, args)
			if code == 0 {
				return nil
			}
			return &ExitError{Code: code}
		},
	}
	fs := cmd.Flags()
	fs.SetInterspersed(false) // flags after the command belong to the command
	fs.BoolVarP(&f.Local, "local", "l", false, "run on this machine only")
	fs.BoolVarP(&f.Remote, "remote", "r", false, "never run here: exit 125 if no other machine can take the job")
	fs.StringVarP(&f.Node, "node", "n", "", "run on this machine (a name from kran status)")
	fs.BoolVar(&f.GPU, "gpu", false, "needs a GPU")
	fs.Float64Var(&f.CPUs, "cpus", 0, "CPU cores to reserve (default: learned estimate)")
	fs.StringVar(&f.Mem, "mem", "", "memory to reserve, e.g. 6G (default: learned estimate)")
	fs.BoolVar(&f.AnyOS, "any-os", false, "allow running natively on a different OS than this one")
	fs.BoolVarP(&f.Stdin, "stdin", "i", false, "forward stdin to a remote job")
	fs.BoolVarP(&f.Quiet, "quiet", "q", false, "hide kran's informational messages (errors still show)")
	fs.BoolVar(&f.JSONEvents, "json-events", false, "print progress as JSON lines on stderr")
	fs.StringVar(&f.Priority, "priority", "", "high, normal or low (default: high on a terminal, else normal)")
	fs.StringArrayVar(&f.Labels, "label", nil, "label the job, key=value (repeatable)")
	fs.BoolVar(&f.NoFallback, "no-fallback", false, "exit 125 instead of running here when kran cannot place the job")
	fs.BoolVar(&f.NoDocker, "no-docker", false, "do not give a container job the Docker socket (root-equivalent on rootful Docker)")
	fs.StringArrayVar(&f.ExtraEnv, "env", nil, "also forward this environment variable, even if it looks secret (repeatable)")
	fs.StringVar(&f.Sh, "sh", "", "run this string with your shell instead of a command")
	return cmd
}

func (a *App) doRun(ctx context.Context, f RunFlags, args []string) int {
	if f.Sh != "" && len(args) > 0 {
		a.errorf("give either a command or --sh, not both (got --sh and %q); quote the whole string: kran run --sh '%s'", args[0], f.Sh)
		return proto.ExitUsage
	}
	cwd, err := os.Getwd()
	if err != nil {
		a.errorf("cannot determine the working directory: %v", err)
		return proto.ExitInfra
	}
	home, _ := os.UserHomeDir()
	env := a.Env()
	termSize := func() *proto.TTY {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			return nil
		}
		return &proto.TTY{Rows: uint16(h), Cols: uint16(w)}
	}
	tty := jobTTY(a.outTTY, a.errTTY, termSize)
	cfg, cerr := config.Load("")
	if cerr != nil {
		// A broken config must not block a job: warn and use defaults.
		if !f.Quiet {
			a.errorf("ignoring config: %v", cerr)
		}
		d := config.Defaults()
		cfg = &d
	}
	var envSet map[string]string
	spec, err := BuildSpec(ctx, SpecInput{
		Project: func(root string) (*workspace.ProjectConfig, error) {
			pc, err := workspace.LoadProjectConfig(root)
			if pc != nil {
				envSet = pc.Env.Set
			}
			return pc, err
		},
		Flags: f, Argv: args, Cwd: cwd, Env: env, Home: home, TTY: tty, Now: time.Now(), PID: os.Getpid(),
		Owner:  DetectOwner(OwnerInput{Env: envMap(env), PID: os.Getpid(), User: userName(), TTY: ttyName(), Procs: newProcReader()}),
		Config: cfg,
		Notice: func(msg string) { a.errorf("%s", msg) },
	})
	if err != nil {
		var ue *UsageError
		if asUsage(err, &ue) {
			a.errorf("%s", ue.Msg)
			return proto.ExitUsage
		}
		a.errorf("%v", err)
		return proto.ExitInfra
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	r := &runner{
		spec: spec, flags: f, env: env, envSet: envSet, dial: a.Dial, stdin: a.In, stdout: a.Out,
		stderr: &lineWriter{w: a.Err}, stdio: localStdio{In: a.In, Out: asFile(a.Out, os.Stdout), Err: asFile(a.Err, os.Stderr)},
		signals: sigs, winch: winch, tm: defaultTiming(), start: startLocal, color: a.styleErr(),
		unknownNode: func(n string) string {
			return unknownNodeError(n, cfg.Name, func() (*config.Peers, error) { return config.LoadPeers("") })
		},
		size: termSize,
	}
	return r.Run(ctx)
}

// jobTTY is the terminal a remote job gets. A remote terminal carries
// stdout and stderr as one stream, so both must be terminals here: with
// `2>err.log` the job keeps separate pipes and its errors land in the file
// as they would locally.
func jobTTY(outTTY, errTTY bool, size func() *proto.TTY) *proto.TTY {
	if !outTTY || !errTTY {
		return nil
	}
	return size()
}

func asFile(w interface{}, def *os.File) *os.File {
	if f, ok := w.(*os.File); ok {
		return f
	}
	return def
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return m
}

func userName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "user"
}
