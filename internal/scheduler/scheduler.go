// Package scheduler decides where and when jobs run: per-owner fair queue,
// resource admission, placement scoring and human-readable reasons.
// SPEC §6.3. Pure logic: no I/O, no goroutines except those the
// caller drives; deterministic given inputs and the injected clock.
package scheduler

import (
	"context"
	"errors"
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

	// UpdateJobUsage reports a placed job's live cpu/memory (from cgroup,
	// process tree or container sampling). Optional but recommended: it
	// lets admission tell memory a running job already uses (visible in the
	// node's live metrics) from memory it may still grow into, instead of
	// assuming every job uses its full allocation after a short ramp-up.
	UpdateJobUsage(jobID string, live proto.Resources)
	// Admit is the target side of two-phase admission (proto.PeerAdmit): it
	// reserves resources on this node for a job placed here by another
	// origin, or returns an *AdmitError saying why not. Release frees it.
	Admit(job *proto.Job, rt proto.RuntimeKind) (Placement, error)
	// Restore accounts again for a job that kept running across a daemon
	// restart, here or on a peer: it holds pl.Alloc on pl.Node, fitting or
	// not, until Release.
	Restore(job *proto.Job, pl Placement)
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
	//
	// Jobs lists the job IDs counted in Allocated per node (fill
	// proto.NodeState.AllocatedJobs of the self node from Jobs[self]).
	Jobs map[string][]string
	// Unavailable says why the scheduler currently avoids a node although
	// its status is online (failed starts cool-down), node → reason.
	Unavailable map[string]string
	// Placements holds every placed (not yet released) job of this
	// scheduler, job ID → placement.
	Placements map[string]Placement
}

// Options for New.
type Options struct {
	Origin string // this node's name
	// Estimate returns duration and speed information for scoring; may be nil.
	Speed func(fingerprint, node string) float64 // relative duration multiplier (1 = like origin)
	Now   func() time.Time
	Logf  func(format string, args ...any)

	//
	// SyncCost returns the expected seconds to sync the job's workspace to
	// node and run its setup there; nil = learned from Release usage
	// (SyncSeconds+SetupSeconds per repo and node) with a cold-start guess.
	SyncCost func(repoID, node string) float64
	// StarvationAfter is when a queued job gets boosted and may hold
	// resources on a node until it fits (default 2 min, SPEC §6.3).
	StarvationAfter time.Duration
	// MaxRemoteAttempts is how many failed remote placements (Reject) a
	// job gets before it falls back to the origin (default 2).
	MaxRemoteAttempts int
}

// ErrCanceled resolves a ticket whose job was canceled while queued.
var ErrCanceled = errors.New("scheduler: job canceled")

// UnplaceableError resolves a ticket whose job can never be placed with the
// given flags (e.g. --node of an unknown node, --remote after every remote
// node failed). Reason is a user-facing sentence.
type UnplaceableError struct{ Reason string }

func (e *UnplaceableError) Error() string { return e.Reason }

// AdmitError is returned by Admit. Code is machine-readable for
// proto.PeerResponse.Code: "busy", "draining", "not_accepting", "disk_floor",
// "no_runtime", "max_jobs", "unknown".
type AdmitError struct {
	Code   string
	Reason string
}

func (e *AdmitError) Error() string { return e.Reason }

func New(opts Options) Scheduler { return newScheduler(opts) }
