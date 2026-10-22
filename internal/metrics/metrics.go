// Package metrics probes static node info and samples live metrics on Linux
// and macOS, including the Docker VM (container runtime capacity) and NVIDIA
// GPUs. SPEC §6.3, proto.NodeInfo/NodeMetrics.
package metrics

import (
	"context"

	"github.com/hafencity-dev/kran/internal/proto"
)

type Prober interface {
	// Info returns static node info (cheap after the first call; Refresh
	// re-probes). Runtimes are filled by the daemon from execenv.
	Info(ctx context.Context) (proto.NodeInfo, error)
	Refresh(ctx context.Context) error
	// Sample returns current live metrics (non-blocking; a background loop
	// started by Run keeps them fresh every 2 s).
	Sample() proto.NodeMetrics
	// JobUsage samples cpu/mem of a process tree / cgroup / container.
	JobUsage(ref UsageRef) (proto.Resources, error)
	Run(ctx context.Context) error
}

// UsageRef identifies what to measure; exactly one field set.
type UsageRef struct {
	CgroupPath string // Linux: /sys/fs/cgroup/... of a scope
	PGID       int    // process group
	Container  string // docker container name
}

type Options struct {
	StateDir string // volume used for disk metrics
	Logf     func(format string, args ...any)
}

func New(opts Options) Prober { return newProber(opts) }
