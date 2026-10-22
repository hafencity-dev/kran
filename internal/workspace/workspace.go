// Package workspace is the origin-side view of a job's files: repo root and
// ID detection, the file scan with a persistent hash cache, toolchain
// detection and the optional project config `.kran.toml`. SPEC §5, §6.7.
package workspace

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Detect resolves the workspace for cwd (git toplevel, else cwd).
func Detect(ctx context.Context, cwd string) (proto.Workspace, error) { return detect(ctx, cwd) }

// Scanner builds manifests. It keeps a hash cache (sqlite table in the state
// db, or a file under the state dir) so unchanged files are not rehashed.
type Scanner interface {
	Scan(ctx context.Context, ws proto.Workspace, include, exclude []string) (*proto.Manifest, error)
	// Open returns the content of a blob by hash from the last scan of root.
	Open(hash string) (ReadSeekCloser, int64, error)
	Close() error
}

type ReadSeekCloser interface {
	Read(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
	Close() error
}

// NewScanner returns a Scanner whose cache lives in cacheDir.
func NewScanner(cacheDir string) (Scanner, error) { return newScanner(cacheDir) }

// Toolchains detects pinned toolchain versions in the workspace
// (go.mod go/toolchain, .nvmrc, .node-version, package.json engines and
// packageManager, .python-version, .tool-versions, mise.toml, rust-toolchain).
func Toolchains(root string) map[string]string { return toolchains(root) }

// ToolchainsAt is Toolchains for a command run in dir below root: every
// directory from root down to dir is read, and per tool the pin nearest dir
// wins (a monorepo's apps/web/.nvmrc or package.json over the root's).
// A dir outside root reads root alone.
func ToolchainsAt(root, dir string) map[string]string {
	rel, err := filepath.Rel(root, dir)
	if err == nil && strings.HasPrefix(rel, "..") {
		// root may be the resolved path of a symlinked checkout.
		if real, e := filepath.EvalSymlinks(dir); e == nil {
			rel, err = filepath.Rel(root, real)
		}
	}
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return toolchains(root)
	}
	var out map[string]string
	cur := root
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		cur = filepath.Join(cur, part)
		for k, v := range toolchains(cur) {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// ProjectConfig is `.kran.toml` (all fields optional).
type ProjectConfig struct {
	OS proto.OSPolicy `toml:"os"`
	// Docker = false keeps the Docker socket out of container jobs (it is
	// root-equivalent on rootful Docker). Default: mounted.
	Docker *bool `toml:"docker"`
	Sync   struct {
		Include []string `toml:"include"`
		Exclude []string `toml:"exclude"`
	} `toml:"sync"`
	Env struct {
		Forward []string          `toml:"forward"`
		Set     map[string]string `toml:"set"`
	} `toml:"env"`
	Setup *struct {
		Run string   `toml:"run"`
		Key []string `toml:"key"`
	} `toml:"setup"`
	Rules []Rule `toml:"rule"`
	Path  string `toml:"-"` // file it was loaded from ("" = none)
}

type Rule struct {
	Match    string         `toml:"match"` // glob on the command line, e.g. "go test*"
	CPUs     float64        `toml:"cpus"`
	Mem      string         `toml:"mem"` // "6GiB"
	Local    bool           `toml:"local"`
	OS       proto.OSPolicy `toml:"os"`
	GPU      bool           `toml:"gpu"`
	Priority proto.Priority `toml:"priority"`
}

// LoadProjectConfig reads `.kran.toml` from root (missing file = zero config).
func LoadProjectConfig(root string) (*ProjectConfig, error) { return loadProjectConfig(root) }

// DefaultSetup returns the auto-detected setup for argv in root, or nil.
func DefaultSetup(root string, argv []string) *proto.SetupSpec { return defaultSetup(root, argv) }
