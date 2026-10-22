package link

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hafencity-dev/kran/internal/config"
)

// Candidate is a machine `kran link` can offer.
type Candidate struct {
	Source string // ssh-config | tailscale | known-hosts
	Host   string // what to pass to ssh
	Detail string // e.g. "macOS, online"
	Online *bool
	Linked bool
}

// Candidates lists machines worth offering: ~/.ssh/config hosts, Tailscale
// peers and known_hosts names. Already linked machines are marked, this
// machine is skipped.
func Candidates(ctx context.Context, r Runner) []Candidate {
	home, _ := os.UserHomeDir()
	peers, _ := config.LoadPeers("")
	self, _ := os.Hostname()
	return candidates(ctx, r, home, peers, self)
}

func shortLabel(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if net.ParseIP(h) != nil {
		return h
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
}

func candidates(ctx context.Context, r Runner, home string, peers *config.Peers, selfHost string) []Candidate {
	self := shortLabel(selfHost)
	byKey := map[string]*Candidate{}
	var order []string
	add := func(c Candidate, keys ...string) *Candidate {
		for _, k := range keys {
			if ex, ok := byKey[k]; ok {
				if ex.Source != "ssh-config" && c.Source == "ssh-config" {
					ex.Host, ex.Source = c.Host, c.Source
				}
				if c.Detail != "" && !strings.Contains(ex.Detail, c.Detail) {
					if ex.Detail != "" {
						ex.Detail += ", "
					}
					ex.Detail += c.Detail
				}
				if c.Online != nil {
					ex.Online = c.Online
				}
				for _, k2 := range keys {
					byKey[k2] = ex
				}
				return ex
			}
		}
		cc := c
		order = append(order, keys[0])
		for _, k := range keys {
			byKey[k] = &cc
		}
		return &cc
	}

	// ssh config
	for _, h := range readSSHConfigHosts(filepath.Join(home, ".ssh", "config"), home, 0) {
		keys := []string{shortLabel(h.Alias)}
		d := h.HostName
		if h.User != "" && d != "" {
			d = h.User + "@" + d
		}
		if h.HostName != "" {
			keys = append(keys, shortLabel(h.HostName))
		}
		add(Candidate{Source: "ssh-config", Host: h.Alias, Detail: d}, keys...)
	}

	// tailscale
	if r != nil {
		if out, err := r.Run(ctx, Cmd{Name: "tailscale", Args: []string{"status", "--json"}}); err == nil {
			for _, t := range parseTailscale(out.Stdout) {
				on := t.Online
				d := t.OS
				if t.OS == "macOS" || t.OS == "linux" || t.OS == "iOS" || t.OS == "windows" {
					d = map[string]string{"macOS": "macOS", "linux": "Linux", "iOS": "iOS", "windows": "Windows"}[t.OS]
				}
				if on {
					d += ", online"
				} else {
					d += ", offline"
				}
				host := t.DNSName
				if host == "" {
					host = t.HostName
				}
				add(Candidate{Source: "tailscale", Host: host, Detail: strings.TrimPrefix(d, ", "), Online: &on},
					shortLabel(t.HostName), shortLabel(host))
			}
		}
	}

	// known_hosts (plain names only)
	if b, err := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(ln)
			if len(f) < 3 || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "@") || strings.HasPrefix(f[0], "|") {
				continue
			}
			for _, h := range strings.Split(f[0], ",") {
				h = strings.Trim(h, "[]")
				if i := strings.LastIndex(h, "]:"); i >= 0 {
					h = h[:i]
				}
				if strings.ContainsAny(h, "*?!") || net.ParseIP(h) != nil || h == "" {
					continue
				}
				add(Candidate{Source: "known-hosts", Host: h}, shortLabel(h))
			}
		}
	}

	var out []Candidate
	for _, k := range order {
		c := *byKey[k]
		if k == self {
			continue
		}
		if linked(peers, k, c.Host) {
			c.Linked = true
		}
		out = append(out, c)
	}
	rank := func(c Candidate) int {
		switch {
		case c.Source == "tailscale" && c.Online != nil && *c.Online:
			return 0
		case c.Online != nil && *c.Online:
			return 0
		case c.Source == "ssh-config":
			return 1
		case c.Source == "tailscale":
			return 2
		}
		return 3
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Linked != out[j].Linked {
			return !out[i].Linked
		}
		return rank(out[i]) < rank(out[j])
	})
	return out
}

func linked(ps *config.Peers, label, host string) bool {
	if ps == nil {
		return false
	}
	for _, p := range ps.Peer {
		if p.Name == label {
			return true
		}
		for _, a := range p.Addresses {
			h, _, err := net.SplitHostPort(a)
			if err != nil {
				h = a
			}
			if strings.EqualFold(h, host) || shortLabel(h) == label {
				return true
			}
		}
	}
	return false
}

type tsPeer struct {
	HostName, DNSName, OS string
	Online                bool
	IPs                   []string
}

func parseTailscale(b []byte) []tsPeer {
	var st struct {
		Peer map[string]struct {
			HostName     string
			DNSName      string
			OS           string
			Online       bool
			TailscaleIPs []string
		}
	}
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	var out []tsPeer
	for _, p := range st.Peer {
		out = append(out, tsPeer{HostName: p.HostName, DNSName: strings.TrimSuffix(p.DNSName, "."), OS: p.OS, Online: p.Online, IPs: p.TailscaleIPs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostName < out[j].HostName })
	return out
}
