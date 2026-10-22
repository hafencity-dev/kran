package cli

import "github.com/spf13/cobra"

// addPlatformCommands registers the commands that manage this machine and
// the links to others.
func (a *App) addPlatformCommands(root *cobra.Command) {
	root.AddCommand(
		a.setupCommand(),
		a.serviceCommand(),
		a.agentsCommand(),
		a.linkCommand(),
		a.unlinkCommand(),
		a.doctorCommand(),
		a.upgradeCommand(),
	)
}
