// Package config loads and saves config.toml and peers.toml (SPEC §5).
// Writes are atomic (temp+rename, 0600) and read-modify-write cycles take a
// file lock, so the daemon, `kran link` and `kran setup` can overlap safely.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/proto"
)

// Config is config.toml. Every field is optional; Defaults() is the baseline.
type Config struct {
	Name    string        `toml:"name"`
	Accept  AcceptConfig  `toml:"accept"`
	Submit  SubmitConfig  `toml:"submit"`
	UI      UIConfig      `toml:"ui"`
	Runtime RuntimeConfig `toml:"runtime"`
}

type AcceptConfig struct {
	Enabled     bool   `toml:"enabled"`
	ReserveCPUs int    `toml:"reserve_cpus"`
	ReserveMem  string `toml:"reserve_mem"`
	MaxJobs     int    `toml:"max_jobs"`
	Nice        int    `toml:"nice"`
}

type SubmitConfig struct {
	Prefer string `toml:"prefer"` // auto | local | remote
}

type UIConfig struct {
	// Listen is host:port, "" (disabled) or "tailscale".
	Listen string `toml:"listen"`
}

type RuntimeConfig struct {
	Container struct {
		Enabled string `toml:"enabled"` // auto | true | false
		// CacheBudget bounds the shared caches of container jobs
		// (toolchains, module and package caches), e.g. "20GiB";
		// "" = min(20 GiB, a quarter of the Docker disk).
		CacheBudget string `toml:"cache_budget"`
	} `toml:"container"`
}

// Defaults returns the configuration used when no file exists.
func Defaults() Config {
	c := Config{
		Name:   DefaultName(),
		Accept: AcceptConfig{Enabled: true, ReserveCPUs: 2, ReserveMem: "8GiB", Nice: 10},
		Submit: SubmitConfig{Prefer: "auto"},
		UI:     UIConfig{Listen: "127.0.0.1:7270"},
	}
	c.Runtime.Container.Enabled = "auto"
	return c
}

// DefaultName is the short lowercase hostname.
func DefaultName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "node"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return SanitizeName(h)
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// SanitizeName lowercases and replaces characters not allowed in node names.
func SanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		return "node"
	}
	return out
}

// ValidName reports whether s is usable as a node name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Validate checks values and returns an error that says how to fix them.
func (c *Config) Validate() error {
	var errs []error
	if !ValidName(c.Name) {
		errs = append(errs, fmt.Errorf("name %q is not valid: use 1-32 chars of a-z, 0-9, - and _ (try %q)", c.Name, SanitizeName(c.Name)))
	}
	if c.Accept.ReserveCPUs < 0 {
		errs = append(errs, errors.New("accept.reserve_cpus must be 0 or more"))
	}
	if c.Accept.MaxJobs < 0 {
		errs = append(errs, errors.New("accept.max_jobs must be 0 (unlimited) or more"))
	}
	if c.Accept.Nice < 0 || c.Accept.Nice > 19 {
		errs = append(errs, errors.New("accept.nice must be between 0 and 19"))
	}
	if _, err := ParseSize(c.Accept.ReserveMem); err != nil {
		errs = append(errs, fmt.Errorf("accept.reserve_mem: %w", err))
	}
	switch c.Submit.Prefer {
	case "auto", "local", "remote":
	default:
		errs = append(errs, fmt.Errorf("submit.prefer = %q; use auto, local or remote", c.Submit.Prefer))
	}
	if b := c.Runtime.Container.CacheBudget; b != "" {
		if _, err := ParseSize(b); err != nil {
			errs = append(errs, fmt.Errorf("runtime.container.cache_budget: %w", err))
		}
	}
	switch c.Runtime.Container.Enabled {
	case "auto", "true", "false":
	default:
		errs = append(errs, fmt.Errorf("runtime.container.enabled = %q; use auto, true or false", c.Runtime.Container.Enabled))
	}
	return errors.Join(errs...)
}

// ReserveMemBytes returns accept.reserve_mem in bytes (0 when invalid).
func (c *Config) ReserveMemBytes() int64 {
	n, _ := ParseSize(c.Accept.ReserveMem)
	return n
}

// PreferMode maps submit.prefer to the proto type.
func (c *Config) PreferMode() proto.Prefer { return proto.Prefer(c.Submit.Prefer) }

// Load reads path (default paths.ConfigFile()). A missing file yields the
// defaults; a broken file yields an error naming the file and line.
func Load(path string) (*Config, error) {
	if path == "" {
		path = paths.ConfigFile()
	}
	c := Defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return nil, fmt.Errorf("%s:%d:%d: %s (fix the file or delete it to use defaults)", path, row, col, de.Error())
		}
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return nil, fmt.Errorf("%s: unknown setting:\n%s", path, sm.String())
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Name == "" {
		c.Name = DefaultName()
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save validates and writes the config atomically with mode 0600.
func Save(path string, c *Config) error {
	if path == "" {
		path = paths.ConfigFile()
	}
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# kran configuration. Every setting is optional; see docs/ or `kran status`.\n"
	return WriteFileAtomic(path, append([]byte(header), data...), 0o600)
}

// Update runs a locked read-modify-write on config.toml.
func Update(path string, fn func(*Config) error) error {
	if path == "" {
		path = paths.ConfigFile()
	}
	return WithLock(path, func() error {
		c, err := Load(path)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
		return Save(path, c)
	})
}

// SetName writes only the node name into config.toml, keeping every other
// key the user set (defaults stay implicit so they can improve later).
func SetName(path, name string) error {
	if path == "" {
		path = paths.ConfigFile()
	}
	if !ValidName(name) {
		return fmt.Errorf("name %q is not valid: use 1-32 chars of a-z, 0-9, - and _ (try %q)", name, SanitizeName(name))
	}
	return WithLock(path, func() error {
		raw := map[string]any{}
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("read %s: %w", path, err)
		default:
			if err := toml.Unmarshal(data, &raw); err != nil {
				return fmt.Errorf("%s is not valid TOML (%v); fix or delete it, then retry", path, err)
			}
		}
		raw["name"] = name
		out, err := toml.Marshal(raw)
		if err != nil {
			return err
		}
		return WriteFileAtomic(path, out, 0o600)
	})
}
