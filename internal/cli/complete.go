package cli

import (
	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/config"
)

// completeNodes completes linked node names from peers.toml.
func completeNodes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ps, err := config.LoadPeers("")
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, p := range ps.Peer {
		names = append(names, p.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
