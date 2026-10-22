package execenv

import (
	"encoding/json"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// superSpec is the supervisor's complete input (jobs/<id>/spec.json). The
// manager resolves everything (env, toolchains, docker arguments) up front
// so the supervisor only executes and never needs the daemon again.
type superSpec struct {
	JobID     string            `json:"job_id"`
	Node      string            `json:"node"`
	Namespace string            `json:"namespace"`
	Runtime   proto.RuntimeKind `json:"runtime"`

	Argv  []string   `json:"argv"`
	Env   []string   `json:"env"`
	Dir   string     `json:"dir"`
	TTY   *proto.TTY `json:"tty,omitempty"`
	Stdin bool       `json:"stdin"`

	MemMax int64   `json:"mem_max"` // 0 = no cap
	CPUs   float64 `json:"cpus"`
	Nice   int     `json:"nice"`

	LeaseTTL    time.Duration `json:"lease_ttl"`
	KillGrace   time.Duration `json:"kill_grace"`
	SampleEvery time.Duration `json:"sample_every"`

	Setup *setupStep `json:"setup,omitempty"`
	// Cgroup: the supervisor runs in its own delegated systemd scope named
	// Scope and puts the job into a child cgroup.
	Cgroup bool   `json:"cgroup"`
	Scope  string `json:"scope,omitempty"`
	// CtlPath is the control socket (stdin, resize, signals).
	CtlPath string      `json:"ctl_path"`
	Docker  *dockerStep `json:"docker,omitempty"`
	// Slot is recorded so a restarted daemon knows which slot the job holds.
	Slot proto.SlotRef `json:"slot"`
	// ToolchainNote explains a toolchain substitution, printed nowhere but
	// kept for `kran doctor` and debugging.
	ToolchainNote string `json:"toolchain_note,omitempty"`
}

type setupStep struct {
	Run     string   `json:"run"`  // for messages
	Argv    []string `json:"argv"` // native: [/bin/sh -c run]
	Dir     string   `json:"dir"`
	KeyFile string   `json:"key_file,omitempty"` // receives Key after success
	Key     string   `json:"key,omitempty"`
}

// dockerStep runs the job as `docker run` (container runtime).
type dockerStep struct {
	CLI       string   `json:"cli"`
	Env       []string `json:"env"` // environment of the docker CLI process
	Name      string   `json:"name"`
	RunArgs   []string `json:"run_args"`   // argv after the binary for the job container
	SetupName string   `json:"setup_name"` // container name of the setup step
	SetupArgs []string `json:"setup_args,omitempty"`
	API       string   `json:"api,omitempty"` // docker API unix socket for stats/inspect
}

// event is one line of jobs/<id>/events.
type event struct {
	Ev        string         `json:"ev"` // ready | phase | exit
	Time      time.Time      `json:"t"`
	PID       int            `json:"pid,omitempty"`
	State     proto.JobState `json:"state,omitempty"`
	Message   string         `json:"msg,omitempty"`
	Cgroup    string         `json:"cgroup,omitempty"`
	Container string         `json:"container,omitempty"`
}

type liveSample struct {
	Time time.Time `json:"t"`
	CPUs float64   `json:"cpus"`
	Mem  int64     `json:"mem"`
}

func (s *superSpec) marshal() ([]byte, error) { return json.MarshalIndent(s, "", "  ") }
