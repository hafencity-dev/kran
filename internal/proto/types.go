// Package proto holds the types shared by the CLI, the daemon, peers and the
// web UI. JSON field names are part of the public API (the UI and --json
// output depend on them); change them only together with every consumer.
package proto

import "time"

// APIVersion is the local HTTP API and run-session version. Peers compare
// ProtocolMajor in hello; a mismatch refuses the connection with a reason.
const (
	APIVersion    = "v1"
	ProtocolMajor = 1
	ProtocolMinor = 1
)

// DiskFloor is the free space below which a node takes no remote jobs and
// kran stops filling the disk: 5% of it, at least 10 GiB and at most
// 20 GiB (5% of a large disk that is otherwise fine would keep a node out
// with tens of GiB free).
func DiskFloor(total int64) int64 {
	return max(10<<30, min(total/20, 20<<30))
}

// MemCap is a job's memory cap (SPEC §6.8): twice its admitted memory, at
// most what is free when it starts. An estimated allocation gets at least
// 8 GiB of room: the cap only kills, it reserves nothing, and a first
// compile or link of a large package easily needs more than the estimate
// of its smaller siblings. An explicit --mem gets at least 2 GiB.
func MemCap(alloc, avail int64, explicit bool) int64 {
	if alloc <= 0 {
		return 0
	}
	floor := int64(8 << 30)
	if explicit {
		floor = 2 << 30
	}
	c := max(2*alloc, floor)
	if avail > 0 && c > avail {
		c = max(avail, alloc)
	}
	return c
}

// Process caps on Linux (systemd TasksMax, threads included). A job's scope
// and kran.slice as a whole stop a runaway fork loop at the cap instead of
// letting it take the machine down; a parallel build with threaded test
// binaries stays far below either.
const (
	JobTasksMax   = 8192
	SliceTasksMax = 16384
)

// ---------------------------------------------------------------------------
// Nodes

// NodeStatus is the scheduling status of a node as seen from this daemon.
type NodeStatus string

const (
	NodeOnline   NodeStatus = "online"
	NodeOffline  NodeStatus = "offline"  // not reachable; Reason says why
	NodeDraining NodeStatus = "draining" // finishes running jobs, accepts none
	NodeDegraded NodeStatus = "degraded" // reachable but failing (circuit breaker open) or disk floor hit
)

// RuntimeKind is how a node executes a job.
type RuntimeKind string

const (
	RuntimeNative    RuntimeKind = "native"
	RuntimeContainer RuntimeKind = "container"
)

// NodeInfo is the static description of a node (refreshed on daemon start and
// every 10 minutes).
type NodeInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hostname  string    `json:"hostname"`
	OS        string    `json:"os"`   // GOOS
	Arch      string    `json:"arch"` // GOARCH
	OSVersion string    `json:"os_version"`
	Version   string    `json:"version"` // kran version
	CPUModel  string    `json:"cpu_model"`
	CPUs      int       `json:"cpus"`      // logical CPUs
	PerfCPUs  int       `json:"perf_cpus"` // performance cores where known (big.LITTLE / Apple P-cores), else CPUs
	MemTotal  int64     `json:"mem_total"`
	DiskTotal int64     `json:"disk_total"` // volume holding the state dir
	GPUs      []GPU     `json:"gpus,omitempty"`
	Runtimes  []Runtime `json:"runtimes"`
	// SingleCoreScore is a relative CPU speed score from a short built-in
	// benchmark (higher is faster); used as the initial speed prior.
	SingleCoreScore float64           `json:"single_core_score"`
	Labels          map[string]string `json:"labels,omitempty"`
	StartedAt       time.Time         `json:"started_at"`
}

type GPU struct {
	Vendor   string `json:"vendor"` // nvidia | apple | amd
	Model    string `json:"model"`
	MemTotal int64  `json:"mem_total,omitempty"` // 0 when unified with system memory
	Unified  bool   `json:"unified"`             // shares system memory (GB10, Apple)
}

// Runtime is one way a node can execute jobs.
type Runtime struct {
	Kind RuntimeKind `json:"kind"`
	OS   string      `json:"os"`   // OS seen by the job (container: "linux")
	Arch string      `json:"arch"` // arch seen by the job
	// CPUs and Mem bound what this runtime can use (container: the Docker
	// VM's resources on macOS, the host's on Linux).
	CPUs      int    `json:"cpus"`
	Mem       int64  `json:"mem"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // why not available
	// Toolchains found on this runtime, e.g. {"go":"1.25.10","node":"24.8.0","mise":"2026.10.1"}.
	Toolchains map[string]string `json:"toolchains,omitempty"`
	Docker     bool              `json:"docker"` // jobs can reach a Docker daemon
	// the container runtime's image is not built yet; the
	// first container job builds it (about 30 s) before it can start.
	FirstUse bool `json:"first_use,omitempty"`
	// Caches: the container runtime's shared caches as its last trim
	// found them (nil until one ran).
	Caches *CacheStatus `json:"caches,omitempty"`
}

// CacheStatus describes the container runtime's shared caches (toolchains,
// module and package caches, the persistent home) after their last trim.
type CacheStatus struct {
	Used      int64 `json:"used"`      // bytes
	Budget    int64 `json:"budget"`    // bytes the caches may use
	DiskFree  int64 `json:"disk_free"` // free bytes on the Docker disk
	DiskTotal int64 `json:"disk_total"`
	// Reserve is the free space kran keeps on that disk, trimming its
	// caches and then idle slots.
	Reserve   int64     `json:"reserve,omitempty"`
	Reclaimed int64     `json:"reclaimed"` // bytes the last trim freed
	At        time.Time `json:"at"`        // when it ran
}

// NodeMetrics are live measurements (sampled every 2 s, published every 2 s).
type NodeMetrics struct {
	Time         time.Time `json:"time"`
	Load1        float64   `json:"load1"`
	CPUBusy      float64   `json:"cpu_busy"`               // 0..1 over the sample window, all CPUs
	CPUPressure  float64   `json:"cpu_pressure,omitempty"` // PSI some avg10 (Linux), 0..100
	MemUsed      int64     `json:"mem_used"`
	MemAvailable int64     `json:"mem_available"`
	MemPressure  float64   `json:"mem_pressure,omitempty"` // PSI some avg10 (Linux) / memorystatus level mapped to 0..100 (macOS)
	SwapUsed     int64     `json:"swap_used"`
	SwapTotal    int64     `json:"swap_total"`
	IOPressure   float64   `json:"io_pressure,omitempty"` // PSI some avg10 (Linux)
	DiskFree     int64     `json:"disk_free"`
	GPUUtil      float64   `json:"gpu_util,omitempty"` // 0..1
	GPUMemUsed   int64     `json:"gpu_mem_used,omitempty"`
	NetRxBps     float64   `json:"net_rx_bps"`
	NetTxBps     float64   `json:"net_tx_bps"`
	// Per-runtime live availability (container runtime on macOS reflects the VM).
	Runtimes map[RuntimeKind]RuntimeMetrics `json:"runtimes,omitempty"`
}

type RuntimeMetrics struct {
	CPUBusy      float64 `json:"cpu_busy"`
	MemAvailable int64   `json:"mem_available"`
}

// Resources is an amount of CPU (cores, fractional) and memory (bytes).
type Resources struct {
	CPUs float64 `json:"cpus"`
	Mem  int64   `json:"mem"`
}

// NodeState is everything the UI and scheduler know about a node.
type NodeState struct {
	Info      NodeInfo    `json:"info"`
	Metrics   NodeMetrics `json:"metrics"`
	Status    NodeStatus  `json:"status"`
	Reason    string      `json:"reason,omitempty"`
	Self      bool        `json:"self"`      // this daemon's own node
	Link      LinkInfo    `json:"link"`      // zero for Self
	Capacity  Resources   `json:"capacity"`  // schedulable after reserve
	Reserve   Resources   `json:"reserve"`   // owner headroom
	Allocated Resources   `json:"allocated"` // Σ admitted kran jobs
	Free      Resources   `json:"free"`      // what the scheduler would admit now
	Running   int         `json:"running"`   // kran jobs running here
	Accepts   bool        `json:"accepts"`   // accept.enabled
	LastSeen  time.Time   `json:"last_seen,omitempty"`
	// config accept.max_jobs (0 = unlimited) so origins
	// respect a peer's job limit when placing.
	MaxJobs int `json:"max_jobs,omitempty"`
	// jobs other origins run on this node.
	Guests []JobRef `json:"guests,omitempty"`
	// IDs of the jobs counted in Allocated (from the node's own
	// scheduler snapshot). Lets an origin tell which of its placements a
	// peer's report already includes, so they are not counted twice.
	AllocatedJobs []string `json:"allocated_jobs,omitempty"`
}

type LinkInfo struct {
	Address   string  `json:"address,omitempty"`   // address in use
	Direction string  `json:"direction,omitempty"` // both | outbound | inbound
	RTTMillis float64 `json:"rtt_ms,omitempty"`
	// BandwidthBps is the EWMA of measured upload throughput to this node.
	BandwidthBps float64 `json:"bandwidth_bps,omitempty"`
	Connected    bool    `json:"connected"`
	// the node's clock minus this node's, measured over the
	// link (midpoint of a request, fastest of several samples); the error is
	// at most ClockRTTMillis/2. ClockRTTMillis is 0 when not measured.
	ClockOffsetMillis float64 `json:"clock_offset_ms,omitempty"`
	ClockRTTMillis    float64 `json:"clock_rtt_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// Jobs

type JobState string

const (
	JobQueued    JobState = "queued"
	JobSyncing   JobState = "syncing"
	JobSetup     JobState = "setup"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded" // exit 0
	JobFailed    JobState = "failed"    // non-zero exit of the command
	JobCanceled  JobState = "canceled"
	JobInfraErr  JobState = "infra_error" // kran failed after the job started
	JobLost      JobState = "lost"        // lease expired; target lost contact
)

// Starting reports whether a job is between placement and its command's
// start (syncing, setup). A daemon restart is safest outside this window:
// queued and running jobs carry over a restart (they are restored and
// reattached), a starting one is settled with its target first.
func (s JobState) Starting() bool { return s == JobSyncing || s == JobSetup }

// Terminal reports whether no further transitions happen.
func (s JobState) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCanceled, JobInfraErr, JobLost:
		return true
	}
	return false
}

type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

type OwnerKind string

const (
	OwnerHuman  OwnerKind = "human"
	OwnerClaude OwnerKind = "claude"
	OwnerCodex  OwnerKind = "codex"
	OwnerAgent  OwnerKind = "agent" // other coding agent / IDE
	OwnerCI     OwnerKind = "ci"
)

type Owner struct {
	Kind OwnerKind `json:"kind"`
	ID   string    `json:"id"`   // stable within a session: session id or ancestor pid
	Name string    `json:"name"` // display name, e.g. "claude · acme-a1"
}

// OSPolicy controls where a job may run relative to the origin OS.
type OSPolicy string

const (
	OSSame OSPolicy = "same"
	OSAny  OSPolicy = "any"
)

// Placement preference from flags.
type Prefer string

const (
	PreferAuto   Prefer = "auto"
	PreferLocal  Prefer = "local"  // --local: origin only
	PreferRemote Prefer = "remote" // --remote: never origin; no fallback
)

type Requirements struct {
	CPUs   float64  `json:"cpus,omitempty"` // explicit --cpus (0 = estimate)
	Mem    int64    `json:"mem,omitempty"`  // explicit --mem (0 = estimate)
	GPU    bool     `json:"gpu,omitempty"`
	OS     OSPolicy `json:"os"`
	Node   string   `json:"node,omitempty"` // --node pin
	Prefer Prefer   `json:"prefer"`
	// OriginOnly is set by the CLI when the job cannot leave the origin
	// (cwd outside workspace, localhost references, rule); Reason explains.
	OriginOnly       bool   `json:"origin_only,omitempty"`
	OriginOnlyReason string `json:"origin_only_reason,omitempty"`
	// Toolchains required by the workspace, e.g. {"go":"1.25.10","node":"24"}.
	Toolchains map[string]string `json:"toolchains,omitempty"`
	// NoDocker keeps the Docker socket out of a container job (.kran.toml
	// docker = false, --no-docker). The socket is root-equivalent on
	// rootful Docker.
	NoDocker bool `json:"no_docker,omitempty"`
}

type Workspace struct {
	Root   string `json:"root"`    // absolute path on origin
	RepoID string `json:"repo_id"` // see SPEC §6.7
	Rel    string `json:"rel"`     // cwd relative to Root ("" = root); "../.." never
	Git    bool   `json:"git"`
	Branch string `json:"branch,omitempty"`
	Name   string `json:"name"` // display: worktree dir name
}

type TTY struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// JobSpec is what the CLI submits.
type JobSpec struct {
	ID         string            `json:"id"` // assigned by the daemon if empty
	Argv       []string          `json:"argv"`
	Shell      string            `json:"shell,omitempty"` // --sh string; Argv then is [$SHELL,"-c",Shell]
	Cwd        string            `json:"cwd"`             // absolute on origin
	Workspace  Workspace         `json:"workspace"`
	Env        map[string]string `json:"env"` // already filtered (SPEC §6.5)
	Req        Requirements      `json:"req"`
	TTY        *TTY              `json:"tty,omitempty"` // nil = not a terminal
	Stdin      bool              `json:"stdin"`         // forward stdin
	Owner      Owner             `json:"owner"`
	Priority   Priority          `json:"priority"`
	Labels     map[string]string `json:"labels,omitempty"`
	NoFallback bool              `json:"no_fallback,omitempty"`
	// Setup overrides auto-detected setup (from .kran.toml).
	Setup *SetupSpec `json:"setup,omitempty"`
	// Sync include/exclude from .kran.toml.
	SyncInclude []string  `json:"sync_include,omitempty"`
	SyncExclude []string  `json:"sync_exclude,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
	ClientPID   int       `json:"client_pid"`
	// OriginEnv describes the origin's user and tools so a container job
	// can match them (SPEC §6.8). Filled by the origin daemon.
	OriginEnv *OriginEnv `json:"origin_env,omitempty"`
}

// OriginEnv is what a container job needs to look like the origin.
type OriginEnv struct {
	User string `json:"user,omitempty"` // login name
	Home string `json:"home,omitempty"` // home directory
	TZ   string `json:"tz,omitempty"`   // IANA time zone, e.g. "Europe/Berlin"
	// Toolchains are the origin's native versions, e.g. {"go":"1.25.3",
	// "node":"24.8.0"}: the defaults for tools the workspace does not pin.
	Toolchains map[string]string `json:"toolchains,omitempty"`
}

type SetupSpec struct {
	Run string   `json:"run"`
	Key []string `json:"key"` // files whose content keys the setup
}

// Estimate is the scheduler's expectation for a job.
type Estimate struct {
	CPUs     float64 `json:"cpus"`
	Mem      int64   `json:"mem"`
	Duration float64 `json:"duration_s"`
	Samples  int     `json:"samples"` // runs of this fingerprint behind it; 0 = none
	// Source is where cpu/mem came from: "learned" (this fingerprint's
	// runs), "repo:go-test" (runs of the family in this repo),
	// "family:go-test" (static default), "flag" or "rule".
	Source string `json:"source"`
	// DurationSource is where Duration came from ("learned",
	// "repo:go-test", "family:go-test"): flags and rules set only cpu/mem.
	DurationSource string `json:"duration_source,omitempty"`
	// RepoSamples counts the runs of the job's family in its repo that a
	// "repo:" estimate (or duration) is built from.
	RepoSamples int `json:"repo_samples,omitempty"`
	// ResultOnly: the command is a check (tests, vet, type check, lint)
	// whose result is its output and exit code; it writes no file the
	// caller would need afterwards, so where it runs costs nothing but time.
	ResultOnly bool `json:"result_only,omitempty"`
}

type Usage struct {
	CPUSeconds   float64 `json:"cpu_seconds"`
	PeakMem      int64   `json:"peak_mem"`
	WallSeconds  float64 `json:"wall_seconds"`
	SyncBytes    int64   `json:"sync_bytes,omitempty"`
	SyncSeconds  float64 `json:"sync_seconds,omitempty"`
	SetupSeconds float64 `json:"setup_seconds,omitempty"`
}

// Job is the daemon's view of a job (live and history).
type Job struct {
	Spec     JobSpec     `json:"spec"`
	State    JobState    `json:"state"`
	Node     string      `json:"node,omitempty"` // target node name once placed
	Runtime  RuntimeKind `json:"runtime,omitempty"`
	Local    bool        `json:"local"`    // ran on origin
	Fallback bool        `json:"fallback"` // ran locally after a remote failure
	// DiskShort: it failed while a disk it wrote to was full (Exit.DiskShort).
	DiskShort bool `json:"disk_short,omitempty"`
	// Reason is why the job waits (queued), what it is doing when that
	// says more (syncing, setup, reconnecting), and once it ended: why it
	// failed (infra error, memory cap, canceled) or else why it ran where
	// it ran (PlaceReason).
	Reason string `json:"reason,omitempty"`
	// PlaceReason is the scheduler's rationale for the placement ("devmac:
	// 12 CPUs free, est. 1m vs 3m here"); set once placed, kept for good.
	PlaceReason string     `json:"place_reason,omitempty"`
	Estimate    Estimate   `json:"estimate"`
	Alloc       Resources  `json:"alloc"`
	Usage       Usage      `json:"usage"`
	Live        *Resources `json:"live,omitempty"` // current cpu/mem while running
	ExitCode    *int       `json:"exit_code,omitempty"`
	Signal      string     `json:"signal,omitempty"`
	Fingerprint string     `json:"fingerprint"`
	QueuedAt    time.Time  `json:"queued_at"`
	StartedAt   time.Time  `json:"started_at,omitempty"`
	EndedAt     time.Time  `json:"ended_at,omitempty"`
	Origin      string     `json:"origin"` // origin node name
	QueuePos    int        `json:"queue_pos,omitempty"`
	// when the job was placed, when the user's
	// command started (same as StartedAt), and the last phase report
	// (sync/setup progress) while it is not running yet.
	PlacedAt  time.Time `json:"placed_at,omitempty"`
	RunningAt time.Time `json:"running_at,omitempty"`
	Phase     *Phase    `json:"phase,omitempty"`
}

// ---------------------------------------------------------------------------
// State and events (local API, SSE)

type Stats struct {
	JobsToday        int `json:"jobs_today"`
	OffloadedToday   int `json:"offloaded_today"`
	FallbacksToday   int `json:"fallbacks_today"`
	FailedInfraToday int `json:"failed_infra_today"`
	// SavedSeconds estimates origin wall time avoided by offloading today.
	SavedSecondsToday float64            `json:"saved_seconds_today"`
	CPUSecondsByNode  map[string]float64 `json:"cpu_seconds_by_node"`
	// start of "today" (local midnight on the daemon).
	Since time.Time `json:"since"`
}

type State struct {
	Version VersionInfo `json:"version"`
	Self    string      `json:"self"` // this node's name
	Nodes   []NodeState `json:"nodes"`
	Jobs    []Job       `json:"jobs"`  // active jobs (queued..running) then most recent 100 finished
	Queue   []string    `json:"queue"` // job IDs in dispatch order
	Owners  []OwnerStat `json:"owners"`
	Stats   Stats       `json:"stats"`
}

type OwnerStat struct {
	Owner   Owner `json:"owner"`
	Running int   `json:"running"`
	Queued  int   `json:"queued"`
}

type EventType string

const (
	EventState       EventType = "state"        // full State (first event, and after resync)
	EventNode        EventType = "node"         // NodeState
	EventNodeMetrics EventType = "node.metrics" // {name, metrics}
	EventJob         EventType = "job"          // Job
	EventQueue       EventType = "queue"        // {queue:[ids]}
	EventStats       EventType = "stats"        // Stats
	EventLog         EventType = "log"          // daemon notable log line {level,msg,time}
)

type Event struct {
	Type EventType `json:"type"`
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Data any       `json:"data"`
}

type NodeMetricsEvent struct {
	Name    string      `json:"name"`
	Metrics NodeMetrics `json:"metrics"`
}

type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	API       string `json:"api"`
	Protocol  string `json:"protocol"` // "1.0"
}

// DaemonInfo says which process serves the local API, so
// `kran service` can tell a daemon its service manager runs from one started
// on demand that must hand over (GET /api/v1/daemon).
type DaemonInfo struct {
	PID int `json:"pid"`
	// Supervisor is the service manager that started it ("systemd",
	// "launchd"), "" when started on demand or by hand.
	Supervisor string    `json:"supervisor,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

// APIError is the JSON body of every non-2xx API response.
type APIError struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"` // what to do next
	// RunLocally (run sessions, FrameError): nothing of the job ran or
	// can still run anywhere, so kran run may run it itself. After a
	// remote decision kran run falls back only on such an error.
	RunLocally bool `json:"run_locally,omitempty"`
}

// QueueEvent is the Data of an EventQueue event.
// typed payload so the client, TUI and UI decode the same shape.
type QueueEvent struct {
	Queue []string `json:"queue"`
}

// LogEvent is the Data of an EventLog event.
// typed payload for notable daemon log lines.
type LogEvent struct {
	Level string    `json:"level"` // "info" | "warn" | "error"
	Msg   string    `json:"msg"`
	Time  time.Time `json:"time"`
	Node  string    `json:"node,omitempty"`
	JobID string    `json:"job_id,omitempty"`
}

// OutputChunk is one message of the followed job log (GET
// /api/v1/jobs/{id}/log?follow=1, unnamed SSE messages).
// binary-safe log streaming for the CLI and the UI.
type OutputChunk struct {
	Offset int64  `json:"offset"`           // payload offset after this chunk
	Data   string `json:"data"`             // the bytes as UTF-8 text (never splits a character)
	Stream string `json:"stream,omitempty"` // "stdout" | "stderr"
	B64    string `json:"b64,omitempty"`    // the same bytes, base64 (exact, for non-UTF-8 output)
}

// JobRef is a job another origin runs on a node (NodeState.Guests).
// show jobs from other machines.
type JobRef struct {
	ID     string     `json:"id"`
	Origin string     `json:"origin"`
	Owner  Owner      `json:"owner"`
	Argv   []string   `json:"argv,omitempty"`
	Alloc  Resources  `json:"alloc"`
	Live   *Resources `json:"live,omitempty"`
}
