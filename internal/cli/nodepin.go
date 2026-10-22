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
// peers.toml. A typo must not silently run the job somewhere else (or
// here): it is a usage error. "" when the name is known or peers.toml
// cannot be read (the daemon then decides).
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
	sort.Strings(names)
	linked := "no machines linked yet"
	if len(names) > 0 {
		linked = "linked: " + strings.Join(names, ", ")
	}
	return fmt.Sprintf("no machine named %q (%s). Link it with: kran link %s", node, linked, node)
}

// pinnedNodeNotice says up front when a --node pin names a machine that is
// linked but cannot take jobs right now; the job then waits for it like any
// queued job (SPEC §6.3), which would otherwise look like a hang.
func pinnedNodeNotice(ctx context.Context, cl client.Client, node string) string {
	if node == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := cl.State(ctx)
	if err != nil || st == nil || node == st.Self {
		return ""
	}
	for _, n := range st.Nodes {
		if n.Info.Name != node {
			continue
		}
		if n.Status == proto.NodeOnline || n.Status == "" {
			return ""
		}
		why := string(n.Status)
		if n.Reason != "" {
			why += ": " + n.Reason
		}
		return fmt.Sprintf("%s is %s; the job waits for it (Ctrl-C to stop, or drop --node to run it wherever there is room)", node, why)
	}
	return ""
}
