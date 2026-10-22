package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/agents"
	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/service"
)

type setupFlags struct {
	Yes, Quiet, NoService, NoAgents, Agents, NoLink bool
	Name                                            string
	Link                                            []string
}

func (a *App) setupCommand() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First-run wizard: key, service, agent integration, optional link",
		Long: `Prepare this machine for kran: create its directories, node ID and SSH key,
install and start the background service, and offer to link other machines and
to teach your coding agents about kran. Safe to run again; it reports what was
already done.`,
		Example: `  kran setup
  kran setup --yes --link devmac     # no questions, then link devmac
  kran setup --yes --quiet           # what "kran link" runs on the other machine`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSetup(cmd.Context(), f)
		},
	}
	fs := cmd.Flags()
	fs.BoolVarP(&f.Yes, "yes", "y", false, "accept the defaults without asking")
	fs.BoolVarP(&f.Quiet, "quiet", "q", false, "print only errors")
	fs.StringVar(&f.Name, "name", "", "name for this machine (default: short hostname)")
	fs.BoolVar(&f.NoService, "no-service", false, "do not install the background service")
	fs.BoolVar(&f.Agents, "agents", false, "install the agent integration without asking")
	fs.BoolVar(&f.NoAgents, "no-agents", false, "skip the agent integration")
	fs.BoolVar(&f.NoLink, "no-link", false, "do not offer to link machines")
	fs.StringArrayVar(&f.Link, "link", nil, "link this host after setup (repeatable)")
	return cmd
}

// prompter asks yes/no questions when a human is there to answer.
type prompter struct {
	r     *bufio.Reader
	w     func(string)
	ask   bool
	yesAs bool
}

func (p *prompter) confirm(q string, def bool) bool {
	if !p.ask {
		return def
	}
	suffix := " [Y/n] "
	if !def {
		suffix = " [y/N] "
	}
	p.w(q + suffix)
	line, _ := p.r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

func (a *App) runSetup(ctx context.Context, f setupFlags) error {
	say := func(format string, args ...any) {
		if !f.Quiet {
			fmt.Fprintf(a.Out, format+"\n", args...)
		}
	}
	if os.Getenv("KRAN_NO_SERVICE") != "" {
		f.NoService = true // set by development/CI setups that must not install a service
	}
	interactive := !f.Yes && a.In != nil && isTerminal(a.In) && a.outTTY
	pr := &prompter{r: bufio.NewReader(a.In), ask: interactive, w: func(s string) { fmt.Fprint(a.Out, s) }}

	// 1. directories, name, node id, key
	if err := paths.EnsureDirs(); err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("cannot create kran's directories: %v (check permissions of %s and %s)", err, paths.ConfigDir(), paths.StateDir())}
	}
	cfg, err := config.Load("")
	if err != nil {
		return &ExitError{Code: 1, Msg: err.Error()}
	}
	if f.Name != "" && f.Name != cfg.Name {
		if err := config.SetName("", f.Name); err != nil {
			return &ExitError{Code: 2, Msg: err.Error()}
		}
		cfg.Name = f.Name
		say("this machine is now called %s", f.Name)
	}
	id, created, err := config.EnsureNodeID()
	if err != nil {
		return &ExitError{Code: 1, Msg: err.Error()}
	}
	if created {
		say("this machine is %s (id %s)", cfg.Name, id[:8])
	} else {
		say("this machine is %s (id %s)", cfg.Name, id[:8])
	}
	_, keyNew, err := config.EnsureKey(cfg.Name)
	if err != nil {
		return &ExitError{Code: 1, Msg: err.Error()}
	}
	if keyNew {
		say("created kran's SSH key (%s)", paths.KeyFile())
	} else {
		say("SSH key present")
	}

	// 2. service
	if !f.NoService {
		if err := a.setupService(ctx, say); err != nil {
			a.errorf("%v", err)
			say("continuing without the service; run `kran daemon` in a terminal or fix the above and run `kran service install`")
		}
	}

	// 3. agents
	if !f.NoAgents {
		if f.Agents || pr.confirm("Teach Claude Code and Codex to use kran (skill, CLAUDE.md block, hook)?", agentToolsPresent()) {
			if f.Yes && !f.Agents && !agentToolsPresent() {
				say("agent integration skipped (no Claude Code or Codex found; add it later with `kran agents install`)")
			} else {
				self, _ := a.Self()
				rep, err := agents.Install(agents.Options{KranPath: self})
				if err != nil {
					a.errorf("agent integration: %v", err)
				} else if !f.Quiet {
					for _, it := range rep.Items {
						say("agents: %s %s", it.Name, it.State)
					}
				}
			}
		}
	}

	// 4. link
	if !f.NoLink {
		hosts := f.Link
		if len(hosts) == 0 && interactive {
			n := linkableCandidates(link.Candidates(ctx, link.ExecRunner{}))
			q := "Link another machine now?"
			if n > 0 {
				q = fmt.Sprintf("Link another machine now? (%d found in your SSH config and Tailscale)", n)
			}
			if pr.confirm(q, n > 0) {
				hosts = []string{""}
			}
		}
		for _, h := range hosts {
			if err := a.linkHost(ctx, linkFlags{Host: h, Interactive: interactive, Quiet: f.Quiet}); err != nil {
				a.errorf("%v", err)
			}
		}
	}

	if !f.Quiet {
		fmt.Fprintln(a.Out)
		if ps, err := config.LoadPeers(""); err == nil && len(ps.Peer) > 0 {
			fmt.Fprintln(a.Out, "kran is ready. Try: kran run -- <command>   or   kran status")
		} else {
			fmt.Fprintln(a.Out, "kran is ready on this machine. Next: kran link <host> to add another machine, then kran run -- <command>.")
		}
	}
	return nil
}

// linkableCandidates counts machines worth offering for a link right away:
// SSH config hosts and Tailscale peers that are not linked yet. known_hosts
// entries alone are too weak a hint to default to yes.
func linkableCandidates(cands []link.Candidate) int {
	n := 0
	for _, c := range cands {
		if c.Linked || unsupportedPlatform(c.Detail) {
			continue
		}
		if c.Source == "ssh-config" || c.Source == "tailscale" {
			n++
		}
	}
	return n
}

func agentToolsPresent() bool {
	h, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, d := range []string{".claude", ".codex"} {
		if st, err := os.Stat(h + "/" + d); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func (a *App) setupService(ctx context.Context, say func(string, ...any)) error {
	switch {
	case service.Mode(service.Options{}) == "launchd-system":
		say("service already installed as a launchd system daemon")
		return nil
	case runtime.GOOS == "darwin" && !service.HasGUISession(ctx, service.Options{}):
		// Not an error: incoming connections start the daemon on demand.
		say("%s", noGUIMessage)
		return nil
	}
	m, err := a.serviceManager()
	if err != nil {
		return err
	}
	release, err := a.handOver(ctx, m, false)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	defer release()
	changed, err := m.Install(ctx)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if changed {
		say("service installed")
	} else {
		say("service already installed")
	}
	v, err := a.waitDaemon(ctx, 10*time.Second)
	if err != nil {
		return fmt.Errorf("the service started but the daemon does not answer yet (%v); see `kran doctor`", err)
	}
	say("daemon running (kran %s)", v.Version)
	if st, err := m.Status(ctx); err == nil {
		for _, adv := range st.Advice {
			say("%s", adv)
		}
	}
	return nil
}
