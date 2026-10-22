package daemon

// Adapters over internal/config: the daemon only sees NodeConfig,
// PeerConfig and PeerSource, so tests can feed them directly.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
)

// NodeConfig is this node's config.toml with defaults applied.
type NodeConfig struct {
	Name   string
	ID     string
	Accept AcceptConfig
	Prefer string // auto | local | remote
	// UIListen is the UI listener address ("" = disabled, "tailscale" =
	// the tailscale IP on port 7270).
	UIListen         string
	ContainerEnabled string // auto | true | false
	CacheBudget      int64  // runtime.container.cache_budget in bytes (0 = automatic)
	DiskReserve      int64  // accept.disk_reserve in bytes (0 = automatic)
}

type AcceptConfig struct {
	Enabled     bool
	ReserveCPUs float64
	ReserveMem  int64
	MaxJobs     int
	Nice        int
}

// DefaultUIListen is the default UI address.
const DefaultUIListen = "127.0.0.1:7270"

func defaultNodeConfig() NodeConfig {
	return NodeConfig{
		Name:             defaultNodeName(),
		Accept:           AcceptConfig{Enabled: true, ReserveCPUs: 2, ReserveMem: 8 << 30, Nice: 10},
		Prefer:           "auto",
		UIListen:         DefaultUIListen,
		ContainerEnabled: "auto",
	}
}

func defaultNodeName() string { return config.DefaultName() }

// loadNodeConfig reads config.toml (missing file = defaults) and the node
// ID from <config dir>/node_id.
func loadNodeConfig(dir string) (NodeConfig, error) {
	c, err := config.Load(filepath.Join(dir, "config.toml"))
	if err != nil {
		return defaultNodeConfig(), err
	}
	nc := NodeConfig{
		Name: c.Name,
		Accept: AcceptConfig{
			Enabled:     c.Accept.Enabled,
			ReserveCPUs: float64(c.Accept.ReserveCPUs),
			ReserveMem:  c.ReserveMemBytes(),
			MaxJobs:     c.Accept.MaxJobs,
			Nice:        c.Accept.Nice,
		},
		Prefer:           c.Submit.Prefer,
		UIListen:         c.UI.Listen,
		ContainerEnabled: c.Runtime.Container.Enabled,
	}
	if b := c.Runtime.Container.CacheBudget; b != "" {
		nc.CacheBudget, _ = config.ParseSize(b)
	}
	if b := c.Accept.DiskReserve; b != "" {
		nc.DiskReserve, _ = config.ParseSize(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "node_id")); err == nil {
		nc.ID = strings.TrimSpace(string(b))
	}
	return nc, nil
}

// ParseSize parses "8GiB", "512MiB", … (config.ParseSize).
func ParseSize(s string) (int64, error) { return config.ParseSize(s) }

// PeerConfig is one [[peer]] of peers.toml.
type PeerConfig struct {
	Name      string    `toml:"name"`
	ID        string    `toml:"id"`
	Addresses []string  `toml:"addresses"`
	User      string    `toml:"user"`
	HostKey   string    `toml:"host_key"`
	KranPath  string    `toml:"kran_path"`
	Direction string    `toml:"direction"` // both | outbound | inbound
	LinkedAt  time.Time `toml:"linked_at"`
	// PreferredAddress won the last address race; tried first.
	PreferredAddress string `toml:"preferred_address"`
}

func (p PeerConfig) dials() bool { return p.Direction != "inbound" }

func (p PeerConfig) sameConnection(o PeerConfig) bool {
	return p.ID == o.ID && p.User == o.User && p.HostKey == o.HostKey &&
		p.Direction == o.Direction && strings.Join(p.Addresses, ",") == strings.Join(o.Addresses, ",")
}

// PeerSource supplies the peer list. Stamp is cheap and changes whenever
// Load would return something different (file mtime and size).
type PeerSource interface {
	Stamp() string
	Load() ([]PeerConfig, error)
}

type filePeerSource struct{ path string }

func (f filePeerSource) Stamp() string {
	st, err := os.Stat(f.path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
}

func (f filePeerSource) Load() ([]PeerConfig, error) {
	ps, err := config.LoadPeers(f.path)
	if err != nil {
		return nil, err
	}
	var out []PeerConfig
	seen := map[string]bool{}
	for _, p := range ps.Peer {
		if p.Name == "" || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		dir := p.Direction
		if dir == "" {
			dir = "both"
		}
		out = append(out, PeerConfig{Name: p.Name, ID: p.ID, Addresses: p.Addresses, User: p.User, HostKey: p.HostKey,
			KranPath: p.KranPath, Direction: dir, LinkedAt: p.LinkedAt, PreferredAddress: p.PreferredAddress})
	}
	return out, nil
}

// savePreferred records the address that won the race in peers.toml.
func (f filePeerSource) savePreferred(name, addr string) error {
	return config.UpdatePeers(f.path, func(ps *config.Peers) error {
		if p, ok := ps.Find(name); ok {
			p.PreferredAddress = addr
		}
		return nil
	})
}
