package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/hafencity-dev/kran/internal/paths"
)

// Peer is one linked machine (SPEC §5). Written by `kran link`, never by hand.
type Peer struct {
	Name      string    `toml:"name"`
	ID        string    `toml:"id"`
	Addresses []string  `toml:"addresses"` // host:port, tried in order
	User      string    `toml:"user"`
	HostKey   string    `toml:"host_key"` // "ssh-ed25519 AAAA…"
	KranPath  string    `toml:"kran_path"`
	Direction string    `toml:"direction"` // both | outbound | inbound
	LinkedAt  time.Time `toml:"linked_at"`
	// PreferredAddress is the last address that connected fastest.
	PreferredAddress string `toml:"preferred_address,omitempty"`
	// RemoteConfigDir is set when the peer runs with a non-default config dir
	// (development setups); `kran unlink` and `doctor` use it.
	RemoteConfigDir string `toml:"remote_config_dir,omitempty"`
	// OS and Arch of the peer as seen at link time.
	OS   string `toml:"os,omitempty"`
	Arch string `toml:"arch,omitempty"`
	// Tag marks authorized_keys lines this link added, e.g. "kran-dev".
	Tag string `toml:"tag,omitempty"`
}

// Peers is peers.toml.
type Peers struct {
	Peer []Peer `toml:"peer"`
}

// Find returns the peer with the given name.
func (p *Peers) Find(name string) (*Peer, bool) {
	for i := range p.Peer {
		if p.Peer[i].Name == name {
			return &p.Peer[i], true
		}
	}
	return nil, false
}

// Upsert replaces the peer with the same ID or name, else appends.
func (p *Peers) Upsert(n Peer) {
	for i := range p.Peer {
		if (n.ID != "" && p.Peer[i].ID == n.ID) || p.Peer[i].Name == n.Name {
			p.Peer[i] = n
			return
		}
	}
	p.Peer = append(p.Peer, n)
}

// Remove deletes the named peer and reports whether it existed.
func (p *Peers) Remove(name string) bool {
	for i := range p.Peer {
		if p.Peer[i].Name == name {
			p.Peer = append(p.Peer[:i], p.Peer[i+1:]...)
			return true
		}
	}
	return false
}

// sshWord reports whether s can be part of an ssh destination: no leading
// "-" (ssh would read an option), no whitespace or control characters.
func sshWord(s string) bool {
	return !strings.HasPrefix(s, "-") && strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0
}

// Validate checks a peer entry.
func (n *Peer) Validate() error {
	var errs []error
	if !ValidName(n.Name) {
		errs = append(errs, fmt.Errorf("peer name %q is not valid (use a-z, 0-9, - and _)", n.Name))
	}
	if len(n.Addresses) == 0 {
		errs = append(errs, fmt.Errorf("peer %s has no addresses; run `kran link %s` again", n.Name, n.Name))
	}
	for _, a := range n.Addresses {
		if strings.TrimSpace(a) == "" || !strings.Contains(a, ":") && !strings.HasPrefix(a, "[") || !sshWord(a) {
			errs = append(errs, fmt.Errorf("peer %s: address %q must be host:port", n.Name, a))
		}
	}
	if n.User != "" && !sshWord(n.User) {
		errs = append(errs, fmt.Errorf("peer %s: user %q is not a valid user name", n.Name, n.User))
	}
	switch n.Direction {
	case "both", "outbound", "inbound":
	default:
		errs = append(errs, fmt.Errorf("peer %s: direction %q must be both, outbound or inbound", n.Name, n.Direction))
	}
	return errors.Join(errs...)
}

// LoadPeers reads peers.toml (default paths.PeersFile()); missing = empty.
func LoadPeers(path string) (*Peers, error) {
	if path == "" {
		path = paths.PeersFile()
	}
	var p Peers
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, &p); err != nil {
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return nil, fmt.Errorf("%s:%d:%d: %s (re-run `kran link` for the affected machines to rewrite it)", path, row, col, de.Error())
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// SavePeers validates and writes peers.toml atomically with mode 0600.
func SavePeers(path string, p *Peers) error {
	if path == "" {
		path = paths.PeersFile()
	}
	for i := range p.Peer {
		if err := p.Peer[i].Validate(); err != nil {
			return err
		}
	}
	data, err := toml.Marshal(p)
	if err != nil {
		return err
	}
	header := "# Written by `kran link`; use `kran link` / `kran unlink` instead of editing.\n"
	return WriteFileAtomic(path, append([]byte(header), data...), 0o600)
}

// UpdatePeers runs a locked read-modify-write on peers.toml.
func UpdatePeers(path string, fn func(*Peers) error) error {
	if path == "" {
		path = paths.PeersFile()
	}
	return WithLock(path, func() error {
		p, err := LoadPeers(path)
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
		return SavePeers(path, p)
	})
}
