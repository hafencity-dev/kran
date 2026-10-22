package execenv

import (
	"errors"
	"syscall"
	"time"
)

// macOS has no cgroups; the supervisor uses process groups, a descendant
// sweep and a memory watchdog instead.
type jobCgroup struct {
	dir string
	fd  int
}

type cgUsage struct {
	mem, peak int64
	cpu       float64
	oomKills  int64
}

func setupJobCgroup(string, int64) (*jobCgroup, error) {
	return nil, errors.New("cgroups are not available on macOS")
}

func (c *jobCgroup) populated() bool       { return false }
func (c *jobCgroup) signal(syscall.Signal) {}
func (c *jobCgroup) kill(time.Duration)    {}
func (c *jobCgroup) usage() cgUsage        { return cgUsage{} }
func (c *jobCgroup) close()                {}
func (c *jobCgroup) pids() []int           { return nil }
