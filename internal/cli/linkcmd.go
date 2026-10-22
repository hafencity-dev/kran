package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/version"
)

type linkFlags struct {
	Host         string
	Name         string
	Tag          string
	RemoteEnv    []string
	RemoteBinDir string
	SSHArgs      []string
	Interactive  bool
	Quiet        bool
}

func (a *App) linkCommand() *cobra.Command {
	var f linkFlags
	cmd := &cobra.Command{
		Use:   "link [host]",
		Short: "Link another machine over SSH (installs kran there if needed)",
		Long: `Link a machine so jobs can move between you. kran uses your normal ssh setup
(config, agent, jump hosts), installs itself on the other machine, and
exchanges dedicated keys so neither side needs anything else. Safe to repeat.

Without a host, kran lists machines from your SSH config, Tailscale and
known_hosts to choose from.`,
		Example: `  kran link devmac
  kran link acme@192.168.1.20
  kran link                         # choose from a list`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeNodes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				f.Host = args[0]
			}
			f.Interactive = isTerminal(a.In) && a.outTTY
			if err := a.linkHost(cmd.Context(), f); err != nil {
				var ue *UsageError
				if errors.As(err, &ue) {
					return err
				}
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Name, "name", "", "name for the other machine (default: its own)")
	fs.StringArrayVar(&f.SSHArgs, "ssh-arg", nil, "extra argument for ssh, e.g. --ssh-arg=-p --ssh-arg=2222 (repeatable)")
	fs.StringVar(&f.Tag, "tag", "", "mark the keys and entry this link adds (for development setups)")
	fs.StringArrayVar(&f.RemoteEnv, "remote-env", nil, "KEY=VALUE exported for every kran command run on the other machine (development)")
	fs.StringVar(&f.RemoteBinDir, "remote-bin-dir", "", "install directory on the other machine (default ~/.local/bin)")
	for _, n := range []string{"tag", "remote-env", "remote-bin-dir"} {
		_ = fs.MarkHidden(n)
	}
	return cmd
}

func parseKV(list []string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(k) { // the key is exported unquoted on the remote
			return nil, usagef("%q must be KEY=VALUE", kv)
		}
		m[k] = v
	}
	return m, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var aliasRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// looksLikeAlias reports whether host is a bare ssh alias (not user@host, an
// FQDN or an address), which is a good node name.
func looksLikeAlias(host string) bool {
	return host != "" && aliasRe.MatchString(host) && host != "localhost"
}

func (a *App) linkOptions(f linkFlags) (link.Options, error) {
	renv, err := parseKV(f.RemoteEnv)
	if err != nil {
		return link.Options{}, err
	}
	for k, v := range renv {
		if strings.HasPrefix(v, "~") || strings.Contains(v, "$") {
			return link.Options{}, usagef("--remote-env %s=%s: use an absolute path; the other machine's home is not expanded", k, v)
		}
	}
	if strings.HasPrefix(f.RemoteBinDir, "$") {
		return link.Options{}, usagef("--remote-bin-dir %s: use an absolute path or ~/…", f.RemoteBinDir)
	}
	if f.Name == "" && looksLikeAlias(f.Host) {
		f.Name = config.SanitizeName(f.Host) // `kran link devmac` names the node devmac on both sides
	}
	self, err := a.Self()
	if err != nil {
		return link.Options{}, fmt.Errorf("cannot find the kran binary: %w", err)
	}
	o := link.Options{
		Host: f.Host, Name: f.Name, Version: version.Version, SelfPath: self, Interactive: f.Interactive,
		SSHArgs: f.SSHArgs, Tag: f.Tag, RemoteEnv: renv, RemoteBinDir: f.RemoteBinDir,
	}
	if !f.Quiet {
		o.Progress = func(step, detail string) {
			if detail != "" {
				fmt.Fprintf(a.Out, "  %s: %s\n", step, detail)
			} else {
				fmt.Fprintf(a.Out, "  %s\n", step)
			}
		}
	}
	return o, nil
}

func (a *App) linkHost(ctx context.Context, f linkFlags) error {
	if _, _, err := config.EnsureNodeID(); err != nil {
		return err
	}
	if f.Host == "" {
		h, err := a.chooseHost(ctx)
		if err != nil {
			return err
		}
		f.Host = h
	}
	o, err := a.linkOptions(f)
	if err != nil {
		return err
	}
	if !f.Quiet {
		fmt.Fprintf(a.Out, "Linking %s\n", f.Host)
	}
	res, err := link.Link(ctx, o)
	if err != nil {
		return fmt.Errorf("could not link %s: %w", f.Host, err)
	}
	name := res.Peer.Name
	dir := res.Direction
	if dir == "both" {
		dir = "both directions"
	} else {
		dir += " only"
	}
	verb := "Linked"
	if res.Already {
		verb = "Already linked to"
	}
	line := fmt.Sprintf("%s %s (%s)", verb, name, dir)
	var meas []string
	if res.RTT > 0 {
		meas = append(meas, fmt.Sprintf("rtt %s", humanDuration(res.RTT.Round(time.Millisecond/10))))
	}
	if res.Bandwidth > 0 {
		meas = append(meas, humanBytes(int64(res.Bandwidth))+"/s")
	}
	if len(meas) > 0 {
		line += ": " + strings.Join(meas, ", ")
	}
	fmt.Fprintln(a.Out, a.styleOut().Green(line)+".")
	if res.ReverseNote != "" {
		fmt.Fprintln(a.Out, res.ReverseNote)
	}
	for _, n := range res.Notes {
		fmt.Fprintln(a.Out, n)
	}
	return nil
}

// chooseHost shows linkable machines and returns the chosen ssh destination.
func (a *App) chooseHost(ctx context.Context) (string, error) {
	cands := link.Candidates(ctx, link.ExecRunner{})
	var items []string
	var hosts []string
	for _, c := range cands {
		if c.Linked || unsupportedPlatform(c.Detail) {
			continue
		}
		label := c.Host
		if c.Detail != "" {
			label += "  —  " + c.Detail
		}
		label += "  [" + c.Source + "]"
		items = append(items, label)
		hosts = append(hosts, c.Host)
	}
	if len(items) == 0 {
		return "", &UsageError{Msg: "no machines found in your SSH config, Tailscale or known_hosts; name one: kran link user@host"}
	}
	if !isTerminal(a.In) {
		return "", &UsageError{Msg: "name the machine to link: kran link <host> (candidates: " + strings.Join(hosts[:min(5, len(hosts))], ", ") + ")"}
	}
	i, err := pick(a.In, a.Out, "Link which machine?", items)
	if err != nil {
		return "", &ExitError{Code: 1, Msg: "nothing linked"}
	}
	return hosts[i], nil
}

func (a *App) unlinkCommand() *cobra.Command {
	var f linkFlags
	cmd := &cobra.Command{
		Use:               "unlink <machine>",
		Short:             "Undo a link: removes the keys and entries `kran link` added, here and there",
		Example:           "  kran unlink devmac",
		Args:              oneArg("machine", "linked machines are in `kran status`"),
		ValidArgsFunction: completeNodes,
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := a.linkOptions(f)
			if err != nil {
				return err
			}
			o.Host = ""
			res, err := link.UnlinkDetailed(cmd.Context(), args[0], o)
			if err != nil {
				return &ExitError{Code: 1, Msg: err.Error()}
			}
			fmt.Fprintf(a.Out, "Unlinked %s.\n", args[0])
			for _, l := range res.Leftover {
				fmt.Fprintln(a.Out, l)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&f.SSHArgs, "ssh-arg", nil, "extra argument for ssh (repeatable)")
	return cmd
}

func (a *App) addInternalLink(internal *cobra.Command) {
	for _, name := range []string{"link-info", "link-accept", "link-forget", "link-probe"} {
		name := name
		internal.AddCommand(&cobra.Command{
			Use: name, Hidden: true, DisableFlagParsing: true,
			RunE: func(c *cobra.Command, args []string) error {
				if code := link.InternalMain(append([]string{name}, args...), a.In, a.Out, a.Err); code != 0 {
					return &ExitError{Code: code}
				}
				return nil
			},
		})
	}
}

// unsupportedPlatform hides Tailscale devices kran cannot run on.
func unsupportedPlatform(detail string) bool {
	d := strings.ToLower(detail)
	for _, os := range []string{"ios", "android", "windows", "tvos", "ipados"} {
		if strings.HasPrefix(d, os+",") || strings.HasPrefix(d, os+" ") || d == os {
			return true
		}
	}
	return false
}
