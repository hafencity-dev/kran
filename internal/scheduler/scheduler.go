// Package scheduler decides where and when jobs run: per-owner fair queue,
// resource admission, placement scoring and human-readable reasons.
// SPEC §6.3. Owner: C. Pure logic: no I/O, no goroutines except those the
// caller drives; deterministic given inputs and the injected clock.
package scheduler

import (
	"context"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Scheduler is driven by the daemon. Nodes are pushed in via UpdateNode;
// jobs via Submit. Placements are delivered through Ticket.
type Scheduler interface {
	// Submit enqueues a job with its estimate. The returned ticket resolves
	// when the job is placed (or ctx/cancel ends it).
	Submit(job *proto.Job) Ticket
	// Cancel removes a queued job (no-op if already placed).
	Cancel(jobID string)
	// Release frees a placed job's allocation (job ended or placement
	// abandoned). usage may be nil when the job never ran.
	Release(jobID string, usage *proto.Usage)
	// Reject reports that a placement failed on its node before start
	// (sync/setup/admit error) so the scheduler can re-place it elsewhere or
	// fall back to origin; failing nodes trip the circuit breaker.
	Reject(jobID string, node string, err error) Ticket
	// UpdateNode replaces the node's state (metrics, status, capacity).
	UpdateNode(n proto.NodeState)
	RemoveNode(name string)
	// SetPriority changes a queued job's priority.
	SetPriority(jobID string, p proto.Priority)
	// Snapshot returns queue order, per-job reasons and per-node allocations.
	Snapshot() Snapshot
	// Kick re-evaluates the queue now (also happens on every mutation and
	// every Tick).
	Kick()
	// Tick runs periodic work (starvation boost, breaker cool-down).
	Tick(now time.Time)
}

type Ticket interface {
	JobID() string
	// Wait blocks until placement or ctx end.
	Wait(ctx context.Context) (Placement, error)
	// Updates delivers queue-reason changes (coalesced; never blocks the scheduler).
	Updates() <-chan string
}

type Placement struct {
	JobID   string
	Node    string
	Runtime proto.RuntimeKind
	Local   bool // Node == origin
	Alloc   proto.Resources
	Reason  string // rationale shown to the user
	// Fallback is true when this placement is the origin after remote
	// placements failed.
	Fallback bool
}

type Snapshot struct {
	Queue     []string                   // job IDs in dispatch order
	Reasons   map[string]string          // job ID → why waiting
	Allocated map[string]proto.Resources // node → Σ allocations
	Free      map[string]proto.Resources // node → admissible now
	Running   map[string]int             // node → jobs
}

// Options for New.
type Options struct {
	Origin string // this node's name
	// Estimate returns duration and speed information for scoring; may be nil.
	Speed func(fingerprint, node string) float64 // relative duration multiplier (1 = like origin)
	Now   func() time.Time
	Logf  func(format string, args ...any)
}

func New(opts Options) Scheduler { return newScheduler(opts) }
