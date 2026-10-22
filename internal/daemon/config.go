package daemon

// Minimal readers for config.toml and peers.toml (SPEC §5). Package E owns
// internal/config; until it lands this file keeps the daemon independent.
// Swap: replace loadNodeConfig / filePeerSource with adapters over
// internal/config — the rest of the daemon only sees NodeConfig, PeerConfig
// and PeerSource.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
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

func defaultNodeName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "node"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

type rawConfig struct {
	Name   *string `toml:"name"`
	ID     *string `toml:"id"`
	Accept struct {
		Enabled     *bool    `toml:"enabled"`
		ReserveCPUs *float64 `toml:"reserve_cpus"`
		ReserveMem  any      `toml:"reserve_mem"`
		MaxJobs     *int     `toml:"max_jobs"`
		Nice        *int     `toml:"nice"`
	} `toml:"accept"`
	Submit struct {
		Prefer *string `toml:"prefer"`
	} `toml:"submit"`
	UI struct {
		Listen *string `toml:"listen"`
	} `toml:"ui"`
	Runtime struct {
		Container struct {
			Enabled any `toml:"enabled"`
		} `toml:"container"`
	} `toml:"runtime"`
}

// loadNodeConfig reads config.toml; a missing file yields the defaults.
func loadNodeConfig(path string) (NodeConfig, error) {
	cfg := defaultNodeConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawConfig
	if err := toml.Unmarshal(b, &raw); err != nil {
		return cfg, fmt.Errorf("parse %s: %w (fix it or remove it to use the defaults)", path, err)
	}
	if raw.Name != nil && strings.TrimSpace(*raw.Name) != "" {
		cfg.Name = strings.TrimSpace(*raw.Name)
	}
	if raw.ID != nil {
		cfg.ID = strings.TrimSpace(*raw.ID)
	}
	a := raw.Accept
	if a.Enabled != nil {
		cfg.Accept.Enabled = *a.Enabled
	}
	if a.ReserveCPUs != nil {
		cfg.Accept.ReserveCPUs = *a.ReserveCPUs
	}
	if a.ReserveMem != nil {
		n, err := parseSize(a.ReserveMem)
		if err != nil {
			return cfg, fmt.Errorf("%s: accept.reserve_mem: %w", path, err)
		}
		cfg.Accept.ReserveMem = n
	}
	if a.MaxJobs != nil {
		cfg.Accept.MaxJobs = *a.MaxJobs
	}
	if a.Nice != nil {
		cfg.Accept.Nice = *a.Nice
	}
	if raw.Submit.Prefer != nil {
		cfg.Prefer = *raw.Submit.Prefer
	}
	if raw.UI.Listen != nil {
		cfg.UIListen = strings.TrimSpace(*raw.UI.Listen)
	}
	switch v := raw.Runtime.Container.Enabled.(type) {
	case bool:
		cfg.ContainerEnabled = strconv.FormatBool(v)
	case string:
		cfg.ContainerEnabled = v
	}
	return cfg, nil
}

// parseSize accepts integers (bytes) and strings like "8GiB", "512M", "2g".
func parseSize(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case string:
		return ParseSize(x)
	}
	return 0, fmt.Errorf("unsupported size %v", v)
}

// ParseSize parses "8GiB", "8G", "8gb", "512MiB", "1024" (bytes).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	num, unit := strings.TrimSpace(s[:i]), strings.ToLower(strings.TrimSpace(s[i:]))
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := map[string]float64{
		"": 1, "b": 1,
		"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
		"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
	}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("invalid size unit in %q (use e.g. 8GiB)", s)
	}
	return int64(f * mult), nil
}

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
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.path, err)
	}
	return parsePeers(b)
}

func parsePeers(b []byte) ([]PeerConfig, error) {
	var doc struct {
		Peer []PeerConfig `toml:"peer"`
	}
	if err := toml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse peers.toml: %w (re-link the node: kran link <host>)", err)
	}
	seen := map[string]bool{}
	out := doc.Peer[:0]
	for _, p := range doc.Peer {
		if p.Name == "" || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		if p.Direction == "" {
			p.Direction = "both"
		}
		out = append(out, p)
	}
	return out, nil
}
