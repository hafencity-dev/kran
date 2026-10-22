package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/agents"
)

func (a *App) agentsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Teach coding agents (Claude Code, Codex) to use kran",
		Long: `Install the kran skill, a short block in ~/.claude/CLAUDE.md and ~/.codex/AGENTS.md,
and a Claude Code hook that runs heavy commands (go test, cargo build, next build, …)
through kran automatically. Everything is reversible with "kran agents uninstall".`,
	}
	run := func(use, short string, fn func(agents.Options) (agents.Report, error)) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(c *cobra.Command, args []string) error {
			self, _ := a.Self()
			rep, err := fn(agents.Options{KranPath: self})
			printAgentReport(a.Out, rep)
			if use == "status" {
				for _, it := range rep.Items {
					if it.State != agents.StateInstalled {
						fmt.Fprintln(a.Out, "\nInstall or update what is missing with `kran agents install`.")
						break
					}
				}
			}
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			return nil
		}}
	}
	cmd.AddCommand(
		run("install", "Install the skill, the CLAUDE.md/AGENTS.md blocks and the Claude Code hook", agents.Install),
		run("uninstall", "Remove exactly what install added", agents.Uninstall),
		run("status", "Show what is installed", agents.Status),
	)
	return cmd
}

func printAgentReport(w io.Writer, rep agents.Report) {
	home, _ := os.UserHomeDir()
	for _, it := range rep.Items {
		path := it.Path
		if home != "" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
			path = "~" + path[len(home):]
		}
		line := fmt.Sprintf("%-16s %-10s %s", it.Name, it.State, path)
		if it.Detail != "" {
			line += "  (" + it.Detail + ")"
		}
		fmt.Fprintln(w, line)
	}
}
