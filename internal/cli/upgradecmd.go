package cli

import "github.com/spf13/cobra"

func (a *App) upgradeCommand() *cobra.Command {
	return &cobra.Command{Use: "upgrade", Short: "Update kran here and on all linked machines", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return &ExitError{Code: 1, Msg: "upgrade: not implemented yet"}
		}}
}
