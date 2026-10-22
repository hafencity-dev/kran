// Package execenv executes jobs on a target node: slots, manifest apply,
// blob cache, project setup, runtimes (native/container), the per-job
// supervisor, process-tree kill, resource caps and the reaper. SPEC §6.2,
// §6.7, §6.8.
package execenv

import (
	"context"
	"io"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Manager is the target-side execution API. The daemon calls it for jobs
// arriving from peers (remote) — local jobs are executed by the CLI itself.
// All methods are safe for concurrent use.
type Manager interface {
	// Runtimes reports what this node can execute (probed at start and
	// refreshed by Refresh).
	Runtimes(ctx context.Context) []proto.Runtime
	Refresh(ctx context.Context) error

	// AcquireSlot reserves a slot for the repo (preferring the slot last
	// used by worktree `hint`). Blocks until one is free or ctx ends.
	AcquireSlot(ctx context.Context, repoID, hint string, rt proto.RuntimeKind) (proto.SlotRef, error)
	// Diff compares a manifest with the slot and returns the blobs it needs.
	Diff(ctx context.Context, slot proto.SlotRef, m *proto.Manifest) (proto.SyncPlan, error)
	// PutBlob stores one blob (content must hash to `hash`; verified).
	PutBlob(ctx context.Context, hash string, size int64, r io.Reader) error
	// Commit materializes the manifest into the slot (atomic per file,
	// deletes extras except Keep, restores modes and mtimes).
	Commit(ctx context.Context, slot proto.SlotRef, m *proto.Manifest) error
	ReleaseSlot(slot proto.SlotRef)

	// Start runs the job's setup (if needed) and command under a detached
	// supervisor and returns a handle. Setup output goes to the same output
	// stream, preceded by a Phase callback.
	Start(ctx context.Context, req StartRequest) (Process, error)
	// Attach reconnects to a running or finished job (after daemon restart).
	Attach(ctx context.Context, jobID string) (Process, error)
	// Active lists job IDs with live supervisors.
	Active() []string

	// Reap runs one reaper pass (also runs periodically once Run is called).
	Reap(ctx context.Context) error
	// Run starts background loops (reaper, cache trim) until ctx ends.
	Run(ctx context.Context) error
}

type StartRequest struct {
	Spec    proto.JobSpec
	Slot    proto.SlotRef
	Runtime proto.RuntimeKind
	Alloc   proto.Resources // admitted resources; caps derive from it (SPEC §6.8)
	Nice    int
	Origin  string // origin node name (KRAN_ORIGIN)
	// OnPhase is called on setup/running transitions.
	OnPhase func(proto.Phase)
}

// Process is a supervised job. Output is a single ordered stream of frames
// (stdout/stderr) addressed by byte offset so a reattaching reader resumes.
type Process interface {
	JobID() string
	// Output returns a reader of output frames starting at offset; it
	// blocks for new output and returns io.EOF after the job ended and all
	// output was delivered. Each Read yields whole frames encoded with
	// proto.WriteFrame (FrameStdout / FrameStderr).
	Output(offset int64) io.ReadCloser
	// Stdin writes to the job's stdin (when spec.Stdin); CloseStdin sends EOF.
	Stdin() io.WriteCloser
	Resize(proto.TTY) error
	Signal(name string) error
	// Renew extends the lease (SPEC §6.2) by LeaseTTL from now.
	Renew() error
	// Live returns current cpu (cores) and memory of the job tree.
	Live() proto.Resources
	// Wait blocks until the job ended and returns its exit.
	Wait(ctx context.Context) (proto.Exit, error)
}

// LeaseTTL is how long a job survives without a heartbeat.
const LeaseTTL = 15 * time.Second

// Options configure a Manager.
type Options struct {
	StateDir    string // node state dir (slots/, blobs/, jobs/)
	NodeName    string
	Self        string // path of the running kran binary (for `kran internal supervise`)
	LinuxBinary string // path to a linux kran binary of the node arch (container runtime on darwin)
	BlobCap     int64  // bytes; 0 = default
	DiskFloor   int64  // bytes; 0 = default max(10 GiB, 5%)
	// CacheBudget bounds the container runtime's shared caches (bytes;
	// 0 = min(20 GiB, a quarter of the Docker disk)).
	CacheBudget int64
	Logf        func(format string, args ...any)

	// Container is config runtime.container.enabled: "auto" (default:
	// offered when Docker is reachable), "true"/"on" or "false"/"off".
	Container string
	// Namespace prefixes every system resource kran creates: systemd slice
	// and scopes, Docker containers, volumes and the runner image. Default
	// "kran". Tests and side-by-side installs use their own (e.g.
	// "kran-test") so reapers never touch each other's resources.
	Namespace string
}

// New returns the production Manager.
func New(opts Options) (Manager, error) { return newManager(opts) }
