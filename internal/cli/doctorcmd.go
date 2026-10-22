package cli

import "github.com/spf13/cobra"

func (a *App) doctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor [node]", Short: "Check this machine and fix common problems", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return &ExitError{Code: 1, Msg: "doctor: not implemented yet"}
		}}
}
