package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/doctor"
	"github.com/hafencity-dev/kran/internal/service"
)

func (a *App) doctorCommand() *cobra.Command {
	var fix, asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor [machine]",
		Short: "Check this machine and its links; fix common problems",
		Long: `Check the service, keys, permissions, linked machines, versions, disk, container
runtime and clocks. Every finding says what to do. With --fix, kran repairs what
is safe to repair (permissions, missing key, service install/start/restart).`,
		Example: "  kran doctor\n  kran doctor --fix\n  kran doctor devmac",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o := doctor.Options{Fix: fix, Dial: a.Dial}
			if len(args) == 1 {
				o.Node = args[0]
			}
			o.Service = func() (service.Manager, error) { return a.serviceManager() }
			rs := doctor.Run(cmd.Context(), o)
			if asJSON {
				enc := json.NewEncoder(a.Out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rs); err != nil {
					return err
				}
			} else {
				a.printDoctor(rs)
			}
			if doctor.Failed(rs) {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "repair what can be repaired safely")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.ValidArgsFunction = completeNodes
	return cmd
}

func (a *App) printDoctor(rs []doctor.Result) {
	s := a.styleOut()
	var warn, fail, fixed int
	for _, r := range rs {
		tag := ""
		switch r.Level {
		case doctor.OK:
			tag = s.Green("ok  ")
		case doctor.Info:
			tag = s.Dim("info")
		case doctor.Warn:
			tag = s.Yellow("warn")
			warn++
		case doctor.Fail:
			tag = s.Red("FAIL")
			fail++
		}
		line := fmt.Sprintf("%s  %s", tag, r.Title)
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		if r.Fixed {
			line += " (fixed)"
			fixed++
		}
		fmt.Fprintln(a.Out, line)
		if r.Fix != "" && r.Level != doctor.OK {
			fmt.Fprintf(a.Out, "        -> %s\n", r.Fix)
		}
	}
	fmt.Fprintln(a.Out)
	switch {
	case fail > 0:
		fmt.Fprintf(a.Out, "%d problem(s) need attention. Try `kran doctor --fix` for the ones marked fixable.\n", fail)
	case warn > 0:
		fmt.Fprintf(a.Out, "kran works, with %d warning(s).\n", warn)
	default:
		fmt.Fprintln(a.Out, "everything looks good.")
	}
	if fixed > 0 {
		fmt.Fprintf(a.Out, "fixed %d thing(s).\n", fixed)
	}
}
