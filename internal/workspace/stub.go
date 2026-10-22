package workspace

import (
	"context"
	"errors"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Replaced by the real implementation (owner B).
func detect(ctx context.Context, cwd string) (proto.Workspace, error) {
	return proto.Workspace{}, errors.New("workspace: not implemented")
}
func newScanner(cacheDir string) (Scanner, error)            { return nil, errors.New("workspace: not implemented") }
func toolchains(root string) map[string]string               { return nil }
func loadProjectConfig(root string) (*ProjectConfig, error)  { return &ProjectConfig{}, nil }
func defaultSetup(root string, argv []string) *proto.SetupSpec { return nil }
