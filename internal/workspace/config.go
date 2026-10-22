package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/hafencity-dev/kran/internal/proto"
)

// ProjectConfigName is the optional per-repo config file.
const ProjectConfigName = ".kran.toml"

func loadProjectConfig(root string) (*ProjectConfig, error) {
	file := filepath.Join(root, ProjectConfigName)
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return &ProjectConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	var cfg ProjectConfig
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return nil, fmt.Errorf("%s: unknown setting: %s; see the .kran.toml section of the kran docs", file, strings.TrimSpace(sm.String()))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return nil, fmt.Errorf("%s:%d:%d: %s", file, row, col, de.Error())
		}
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	cfg.Path = file
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &cfg, nil
}

func (c *ProjectConfig) validate() error {
	okOS := func(p proto.OSPolicy) bool { return p == "" || p == proto.OSSame || p == proto.OSAny }
	if !okOS(c.OS) {
		return fmt.Errorf(`os = %q: use "same" or "any"`, c.OS)
	}
	if c.Setup != nil && strings.TrimSpace(c.Setup.Run) == "" {
		return errors.New("[setup] needs a run command")
	}
	for i, r := range c.Rules {
		if strings.TrimSpace(r.Match) == "" {
			return fmt.Errorf("rule %d: match is empty", i+1)
		}
		if !okOS(r.OS) {
			return fmt.Errorf(`rule %d: os = %q: use "same" or "any"`, i+1, r.OS)
		}
		switch r.Priority {
		case "", proto.PriorityHigh, proto.PriorityNormal, proto.PriorityLow:
		default:
			return fmt.Errorf(`rule %d: priority = %q: use "high", "normal" or "low"`, i+1, r.Priority)
		}
		if r.CPUs < 0 {
			return fmt.Errorf("rule %d: cpus must not be negative", i+1)
		}
	}
	return nil
}
