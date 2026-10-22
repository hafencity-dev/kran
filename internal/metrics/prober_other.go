//go:build !linux && !darwin

package metrics

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Unsupported OSes report static basics only, so the package still builds.

type otherPlatform struct{}

func newPlatform(p *prober) platform { return otherPlatform{} }

func (otherPlatform) staticInfo(ctx context.Context, info *proto.NodeInfo)            {}
func (otherPlatform) sample(ctx context.Context, m *proto.NodeMetrics, now time.Time) {}
func (otherPlatform) start(ctx context.Context, wg *sync.WaitGroup)                   {}
func (otherPlatform) pinBench()                                                       {}
func (otherPlatform) pgidUsage(ctx context.Context, pgid int, now time.Time) (proto.Resources, error) {
	return proto.Resources{}, errors.New("process group usage is not supported on this OS")
}

func statfs(path string) (total, free int64, err error) {
	return 0, 0, errors.New("statfs is not supported on this OS")
}

func ctimeOf(fi os.FileInfo) time.Time { return time.Time{} }
