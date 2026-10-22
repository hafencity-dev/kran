package doctor

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/proto"
)

const slowLinkBps = 10 << 20 // 10 MiB/s: below this, syncing big workspaces hurts

func (c *checker) checkPeers(ctx context.Context) {
	ps, err := c.o.Peers()
	if err != nil {
		c.add(Result{ID: "peers", Title: "peers.toml", Level: Fail, Detail: err.Error()})
		return
	}
	if len(ps.Peer) == 0 {
		if c.o.Node == "" {
			c.add(Result{ID: "peers", Title: "linked machines", Level: Info, Detail: "none yet", Fix: "kran link <host>"})
		}
		return
	}
	signer, serr := c.o.Signer()
	var matched bool
	for _, p := range ps.Peer {
		if c.o.Node != "" && p.Name != c.o.Node {
			continue
		}
		matched = true
		title := "link to " + p.Name
		switch {
		case serr != nil:
			c.add(Result{ID: "peer.reach", Title: title, Level: Fail, Node: p.Name, Detail: serr.Error(), Fix: "kran setup"})
			continue
		case p.Direction == "inbound":
			c.add(Result{ID: "peer.reach", Title: title, Level: Info, Node: p.Name, Detail: "inbound only: " + p.Name + " connects to this machine"})
		case p.HostKey == "":
			c.add(Result{ID: "peer.hostkey", Title: "host key of " + p.Name, Level: Warn, Node: p.Name, Detail: "no host key is pinned, so the connection is not protected against impersonation", Fix: "kran link " + p.Name})
		}
		if p.Direction != "inbound" {
			rs := link.HandshakeAll(ctx, p.Addresses, p.User, signer, p.HostKey, 5*time.Second)
			c.add(peerResult(p, rs))
		}
		c.stateChecks(p.Name)
	}
	if c.o.Node != "" && !matched {
		c.add(Result{ID: "peers", Title: "node " + c.o.Node, Level: Fail, Node: c.o.Node,
			Detail: fmt.Sprintf("no linked machine is named %q", c.o.Node), Fix: "kran status shows linked machines; link one with `kran link <host>`"})
	}
}

func peerResult(p config.Peer, rs []link.ProbeResult) Result {
	title := "link to " + p.Name
	var errs []string
	best := -1
	for i, r := range rs {
		if r.OK && (best < 0 || r.RTTMs < rs[best].RTTMs) {
			best = i
		}
		if !r.OK {
			errs = append(errs, r.Address+": "+r.Error)
		}
	}
	for _, r := range rs {
		if strings.Contains(r.Error, "host key changed") {
			return Result{ID: "peer.hostkey", Title: "host key of " + p.Name, Level: Fail, Node: p.Name,
				Detail: p.Name + " presented a different SSH host key than the one pinned at link time (" + r.Error + ")",
				Fix:    "if you reinstalled " + p.Name + ", run `kran link " + p.Name + "` to pin the new key; if not, someone may be intercepting the connection - do not link again"}
		}
	}
	if best >= 0 {
		d := fmt.Sprintf("reachable via %s in %.0f ms", rs[best].Address, rs[best].RTTMs)
		if len(errs) > 0 {
			d += "; other addresses failed: " + strings.Join(errs, "; ")
		}
		return Result{ID: "peer.reach", Title: title, Level: OK, Node: p.Name, Detail: d}
	}
	return Result{ID: "peer.reach", Title: title, Level: Fail, Node: p.Name, Detail: "cannot connect: " + strings.Join(errs, "; "),
		Fix: "check that " + p.Name + " is on and reachable (`ssh " + p.Name + "`), then `kran link " + p.Name + "` to repair the keys"}
}

// Clock offsets above clockWarnSeconds are reported, when the measurement's
// round trip (twice its error bound) is at most maxClockRTTMillis.
const (
	clockWarnSeconds  = 2.0
	maxClockRTTMillis = 1000.0
)

// stateChecks uses the daemon's live view of the node: clock, disk, bandwidth.
func (c *checker) stateChecks(name string) {
	if c.state == nil {
		return
	}
	var n *proto.NodeState
	for i := range c.state.Nodes {
		if c.state.Nodes[i].Info.Name == name {
			n = &c.state.Nodes[i]
		}
	}
	if n == nil {
		return
	}
	if n.Status != proto.NodeOnline {
		c.add(Result{ID: "peer.status", Title: name + " status", Level: Warn, Node: name, Detail: string(n.Status) + ": " + n.Reason})
		return
	}
	// The daemon measures the offset over the live link (midpoint of a
	// request, fastest of several samples; error at most RTT/2). The age of
	// the last metrics sample is no clock measurement: it lags by seconds.
	if l := n.Link; l.ClockRTTMillis > 0 && l.ClockRTTMillis <= maxClockRTTMillis {
		if off := math.Abs(l.ClockOffsetMillis) / 1000; off > clockWarnSeconds {
			c.add(Result{ID: "clock", Title: "clock of " + name, Level: Warn, Node: name,
				Detail: fmt.Sprintf("differs by about %.1f s from this machine; file mtimes and logs will disagree", off),
				Fix:    "enable network time (Linux: `timedatectl set-ntp true`; macOS: System Settings > General > Date & Time)"})
		}
	}
	if n.Info.DiskTotal > 0 {
		floor := max(int64(10<<30), n.Info.DiskTotal/20)
		if n.Metrics.DiskFree > 0 && n.Metrics.DiskFree < floor {
			c.add(Result{ID: "disk.peer", Title: "disk space on " + name, Level: Warn, Node: name,
				Detail: fmt.Sprintf("%s free, below the floor of %s; %s refuses new jobs", gib(n.Metrics.DiskFree), gib(floor), name),
				Fix:    "free space on " + name})
		}
	}
	if bw := n.Link.BandwidthBps; bw > 0 && bw < slowLinkBps {
		hint := "check whether either machine is on Wi-Fi; Ethernet or a direct Tailscale path (`tailscale ping " + name + "`) is much faster"
		if w := c.wifiInterface(); w != "" {
			hint = "this machine's default route uses Wi-Fi (" + w + "); plug in Ethernet for faster syncs to " + name
		}
		c.add(Result{ID: "link.speed", Title: "speed to " + name, Level: Warn, Node: name,
			Detail: fmt.Sprintf("uploads run at %.1f MiB/s, which makes syncing large workspaces slow", bw/(1<<20)), Fix: hint})
	}
}

// wifiInterface returns the default-route interface when it is wireless (Linux).
func (c *checker) wifiInterface() string {
	if c.o.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) > 3 && f[1] == "00000000" {
			if _, err := os.Stat("/sys/class/net/" + f[0] + "/wireless"); err == nil {
				return f[0]
			}
			return ""
		}
	}
	return ""
}
