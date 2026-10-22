package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/proto"
)

// unknownNodeError checks a --node pin against this machine's name and
// peers.toml, for when no daemon answers. A typo must not silently run the
// job somewhere else (or here): it is a usage error. "" when the name is
// known or peers.toml cannot be read.
func unknownNodeError(node, self string, loadPeers func() (*config.Peers, error)) string {
	if node == "" || node == self {
		return ""
	}
	ps, err := loadPeers()
	if err != nil {
		return ""
	}
	var names []string
	for _, p := range ps.Peer {
		if p.Name == node {
			return ""
		}
		names = append(names, p.Name)
	}
	return noMachine(node, names)
}

// pinnedNode checks a --node pin against the daemon's view of the mesh
// (the authority: it may know peers from elsewhere than this peers.toml).
// usage is set for a name the daemon does not know; notice when the
// machine is linked but cannot take jobs right now, since the job then
// waits for it like any queued job (SPEC §6.3), which would otherwise look
// like a hang. Both are "" when the state cannot be read.
func pinnedNode(ctx context.Context, cl client.Client, node string) (usage, notice string) {
	if node == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := cl.State(ctx)
	if err != nil || st == nil || node == st.Self {
		return "", ""
	}
	var names []string
	for _, n := range st.Nodes {
		if n.Self {
			continue
		}
		names = append(names, n.Info.Name)
		if n.Info.Name != node {
			continue
		}
		if n.Status == proto.NodeOnline || n.Status == "" {
			return "", ""
		}
		why := string(n.Status)
		if n.Reason != "" {
			why += ": " + n.Reason
		}
		return "", fmt.Sprintf("%s is %s; the job waits for it (Ctrl-C to stop, or drop --node to run it wherever there is room)", node, why)
	}
	return noMachine(node, names), ""
}

func noMachine(node string, linked []string) string {
	sort.Strings(linked)
	list := "no machines linked yet"
	if len(linked) > 0 {
		list = "linked: " + strings.Join(linked, ", ")
	}
	return fmt.Sprintf("no machine named %q (%s). Link it with: kran link %s", node, list, node)
}
