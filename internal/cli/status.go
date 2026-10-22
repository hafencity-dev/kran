package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

func (a *App) statusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show your machines at a glance: free CPU and memory, running and queued jobs",
		Example: `  kran status
  kran status --json | jq '.nodes[] | {name: .info.name, free: .free}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd.Context(), func(cl client.Client) error {
				st, err := cl.State(cmd.Context())
				if err != nil {
					return &ExitError{Code: 1, Msg: "cannot read state: " + err.Error()}
				}
				if asJSON {
					enc := json.NewEncoder(a.Out)
					enc.SetIndent("", "  ")
					return enc.Encode(st)
				}
				a.printStatus(st, time.Now())
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full state as JSON")
	return cmd
}

func (a *App) printStatus(st *proto.State, now time.Time) {
	s := a.styleOut()
	w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	for _, n := range st.Nodes {
		name := n.Info.Name
		if n.Self {
			name += " (this machine)"
		}
		status := string(n.Status)
		switch n.Status {
		case proto.NodeOnline:
			status = s.Green(status)
		case proto.NodeOffline, proto.NodeDegraded:
			status = s.Red(status)
		case proto.NodeDraining:
			status = s.Yellow(status)
		}
		detail := ""
		if n.Status == proto.NodeOnline || n.Status == proto.NodeDraining || n.Status == proto.NodeDegraded {
			detail = fmt.Sprintf("%.0f of %d cpus free · %s of %s mem free · %d running",
				n.Free.CPUs, n.Info.CPUs, humanBytes(n.Free.Mem), humanBytes(n.Info.MemTotal), n.Running)
			if !n.Self && n.Link.Connected {
				detail += fmt.Sprintf(" · rtt %.0f ms", n.Link.RTTMillis)
				if n.Link.BandwidthBps > 0 {
					detail += " · " + humanBytes(int64(n.Link.BandwidthBps)) + "/s"
				}
			}
		}
		if n.Reason != "" {
			if detail != "" {
				detail += " · "
			}
			detail += n.Reason
		}
		if !n.Accepts && !n.Self {
			detail += " · not accepting jobs"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Bold(name), status, detail)
	}
	w.Flush()

	var active []proto.Job
	for _, j := range st.Jobs {
		if !j.State.Terminal() {
			active = append(active, j)
		}
	}
	if len(active) > 0 {
		fmt.Fprintln(a.Out)
		w = tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
		for _, j := range active {
			where := j.Node
			if where == "" {
				where = "-"
			}
			age := ""
			if !j.QueuedAt.IsZero() {
				age = humanDuration(now.Sub(j.QueuedAt).Truncate(time.Second))
			}
			reason := ""
			if j.State == proto.JobQueued && j.Reason != "" {
				reason = j.Reason
				if !strings.HasPrefix(reason, "waiting") { // the scheduler's reasons already start with it
					reason = "waiting: " + reason
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.Spec.ID, j.State, where, age, truncate(jobCommand(j.Spec), 48), reason)
		}
		w.Flush()
	}
	fmt.Fprintln(a.Out)
	sts := st.Stats
	if sts.JobsToday == 0 {
		fmt.Fprintln(a.Out, "today: no jobs yet")
	} else {
		parts := []string{fmt.Sprintf("%d %s", sts.JobsToday, plural(sts.JobsToday, "job", "jobs"))}
		if sts.OffloadedToday > 0 || len(st.Nodes) > 1 {
			parts = append(parts, fmt.Sprintf("%d offloaded", sts.OffloadedToday))
		}
		if sts.FallbacksToday > 0 {
			parts = append(parts, fmt.Sprintf("%d ran here after a remote attempt failed", sts.FallbacksToday))
		}
		if sts.FailedInfraToday > 0 {
			parts = append(parts, fmt.Sprintf("%d failed on kran's side (kran doctor)", sts.FailedInfraToday))
		}
		if sts.SavedSecondsToday >= 1 {
			parts = append(parts, "about "+humanDuration(time.Duration(sts.SavedSecondsToday*float64(time.Second)).Truncate(time.Second))+" saved")
		}
		fmt.Fprintln(a.Out, "today: "+strings.Join(parts, ", "))
	}
	if len(st.Nodes) == 1 {
		fmt.Fprintln(a.Out, "only this machine so far; add another with `kran link <host>`")
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func jobCommand(sp proto.JobSpec) string {
	if sp.Shell != "" {
		return sp.Shell
	}
	return strings.Join(sp.Argv, " ")
}

func (a *App) jobsCommand() *cobra.Command {
	var asJSON, all, why bool
	var limit int
	cmd := &cobra.Command{
		Use:     "jobs",
		Short:   "List recent jobs",
		Example: "  kran jobs\n  kran jobs --why           # why each job ran where it did\n  kran jobs -a --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd.Context(), func(cl client.Client) error {
				n := limit
				if all {
					n = 1000
				}
				jobs, err := cl.Jobs(cmd.Context(), n)
				if err != nil {
					return &ExitError{Code: 1, Msg: "cannot list jobs: " + err.Error()}
				}
				if asJSON {
					enc := json.NewEncoder(a.Out)
					enc.SetIndent("", "  ")
					return enc.Encode(jobs)
				}
				if len(jobs) == 0 {
					fmt.Fprintln(a.Out, "no jobs yet; try `kran run -- <command>`")
					return nil
				}
				w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
				head := "ID\tSTATE\tNODE\tTOOK\tOWNER\tCOMMAND"
				if why {
					head += "\tWHY"
				}
				fmt.Fprintln(w, head)
				for _, j := range jobs {
					node := j.Node
					if node == "" {
						node = "-"
					}
					dur := ""
					if !j.StartedAt.IsZero() && !j.EndedAt.IsZero() {
						dur = humanDuration(j.EndedAt.Sub(j.StartedAt).Truncate(100 * time.Millisecond))
					}
					state := string(j.State)
					if j.ExitCode != nil && j.State == proto.JobFailed {
						state = fmt.Sprintf("failed(%d)", *j.ExitCode)
					}
					row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", j.Spec.ID, state, node, dur, truncate(j.Spec.Owner.Name, 22), truncate(jobCommand(j.Spec), 56))
					if why {
						row += "\t" + j.Reason
					}
					fmt.Fprintln(w, row)
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&why, "why", false, "add a column with the scheduler's reason for where each job ran")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "show up to 1000 jobs instead of the most recent")
	cmd.Flags().IntVar(&limit, "limit", 20, "number of jobs to show")
	return cmd
}

// resolveJob maps a job ID prefix (or "" = most recent) to a full ID.
func (a *App) resolveJob(cmd *cobra.Command, cl client.Client, prefix string) (string, error) {
	jobs, err := cl.Jobs(cmd.Context(), 1000)
	if err != nil {
		return "", &ExitError{Code: 1, Msg: "cannot list jobs: " + err.Error()}
	}
	if prefix == "" {
		if len(jobs) == 0 {
			return "", &ExitError{Code: 1, Msg: "no jobs yet; try `kran run -- <command>`"}
		}
		sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].QueuedAt.After(jobs[j].QueuedAt) })
		return jobs[0].Spec.ID, nil
	}
	var match []string
	for _, j := range jobs {
		if j.Spec.ID == prefix {
			return prefix, nil
		}
		if strings.HasPrefix(j.Spec.ID, prefix) {
			match = append(match, j.Spec.ID)
		}
	}
	switch len(match) {
	case 0:
		return "", &ExitError{Code: 1, Msg: fmt.Sprintf("no job matches %q; list jobs with `kran jobs -a`", prefix)}
	case 1:
		return match[0], nil
	}
	return "", &ExitError{Code: 1, Msg: fmt.Sprintf("%q matches %d jobs (%s…); give more characters", prefix, len(match), strings.Join(match[:min(3, len(match))], ", "))}
}

func (a *App) logsCommand() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:     "logs [job]",
		Short:   "Show a job's output (default: the most recent job)",
		Example: "  kran logs\n  kran logs -f 01hx2",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd.Context(), func(cl client.Client) error {
				prefix := ""
				if len(args) == 1 {
					prefix = args[0]
				}
				id, err := a.resolveJob(cmd, cl, prefix)
				if err != nil {
					return err
				}
				cw := &countWriter{w: a.Out}
				if err := cl.Log(cmd.Context(), id, follow, cw); err != nil {
					return &ExitError{Code: 1, Msg: "cannot read the log of " + id + ": " + err.Error()}
				}
				if cw.n == 0 {
					a.explainEmptyLog(cmd, cl, id)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the job ends")
	return cmd
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// explainEmptyLog says why there is nothing to show for a local job (whose
// output kran records since its CLI copies it to the daemon).
func (a *App) explainEmptyLog(cmd *cobra.Command, cl client.Client, id string) {
	jobs, err := cl.Jobs(cmd.Context(), 1000)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.Spec.ID != id {
			continue
		}
		if j.Local {
			a.errorf("%s ran on this machine and printed nothing kran recorded (a job started by an older kran, or while the daemon was down, has no recorded output)", id)
		}
		return
	}
}

func (a *App) cancelCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "cancel <job>",
		Short:   "Cancel a queued or running job",
		Example: "  kran cancel 01hx2",
		Args:    oneArg("job", "list jobs with `kran jobs`"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd.Context(), func(cl client.Client) error {
				id, err := a.resolveJob(cmd, cl, args[0])
				if err != nil {
					return err
				}
				if err := cl.Cancel(cmd.Context(), id); err != nil {
					return &ExitError{Code: 1, Msg: "cannot cancel " + id + ": " + err.Error()}
				}
				fmt.Fprintf(a.Out, "canceled %s\n", id)
				return nil
			})
		},
	}
}

func (a *App) drainCommand(verb string) *cobra.Command {
	short := "Stop sending new jobs to a machine (running jobs finish)"
	if verb == "resume" {
		short = "Let a drained machine accept jobs again"
	}
	return &cobra.Command{
		Use:               verb + " <machine>",
		Short:             short,
		Example:           "  kran " + verb + " devmac",
		Args:              oneArg("machine", "machine names are in `kran status`"),
		ValidArgsFunction: completeNodes,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withClient(cmd.Context(), func(cl client.Client) error {
				var err error
				if verb == "drain" {
					err = cl.Drain(cmd.Context(), args[0])
				} else {
					err = cl.Resume(cmd.Context(), args[0])
				}
				if err != nil {
					return &ExitError{Code: 1, Msg: fmt.Sprintf("cannot %s %s: %v", verb, args[0], err)}
				}
				if verb == "drain" {
					fmt.Fprintf(a.Out, "%s is draining: running jobs finish, nothing new starts there. Undo with `kran resume %s`.\n", args[0], args[0])
				} else {
					fmt.Fprintf(a.Out, "%s accepts jobs again.\n", args[0])
				}
				return nil
			})
		},
	}
}
