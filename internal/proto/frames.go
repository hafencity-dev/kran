package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Run-session framing (SPEC §8.2): type:u8 | len:u32be | payload.
// Control frames carry JSON; stdin/stdout/stderr carry raw bytes.

type FrameType uint8

const (
	FrameSubmit    FrameType = 1  // client→daemon  JSON JobSpec
	FrameNotice    FrameType = 2  // daemon→client  JSON Notice
	FrameDecision  FrameType = 3  // daemon→client  JSON Decision
	FrameStarted   FrameType = 4  // both           JSON Started
	FrameStdin     FrameType = 5  // client→daemon  raw
	FrameStdinEOF  FrameType = 6  // client→daemon  empty
	FrameStdout    FrameType = 7  // both           raw (client→daemon: a local job's output for the history)
	FrameStderr    FrameType = 8  // both           raw (as FrameStdout)
	FrameResize    FrameType = 9  // client→daemon  JSON TTY
	FrameSignal    FrameType = 10 // client→daemon  JSON Signal
	FrameExit      FrameType = 11 // both           JSON Exit (client sends it for local jobs)
	FrameHeartbeat FrameType = 12 // both           empty; every 2 s each way
	FrameReattach  FrameType = 13 // client→daemon  JSON Reattach
	FrameError     FrameType = 14 // daemon→client  JSON APIError; session ends
	FramePhase     FrameType = 15 // daemon→client  JSON Phase (syncing/setup/running + progress)
	// stdin flow control. kran run forwards input only below
	// the credit's limit and keeps what the job has not consumed, so input
	// lost on a broken connection is sent again from Resume.
	FrameStdinCredit FrameType = 16 // daemon→client  JSON StdinCredit
	FrameStdinResume FrameType = 17 // client→daemon  JSON StdinResume (answers a credit with Resume)
)

func (t FrameType) String() string {
	switch t {
	case FrameSubmit:
		return "submit"
	case FrameNotice:
		return "notice"
	case FrameDecision:
		return "decision"
	case FrameStarted:
		return "started"
	case FrameStdin:
		return "stdin"
	case FrameStdinEOF:
		return "stdin_eof"
	case FrameStdout:
		return "stdout"
	case FrameStderr:
		return "stderr"
	case FrameResize:
		return "resize"
	case FrameSignal:
		return "signal"
	case FrameExit:
		return "exit"
	case FrameHeartbeat:
		return "heartbeat"
	case FrameReattach:
		return "reattach"
	case FrameError:
		return "error"
	case FramePhase:
		return "phase"
	case FrameStdinCredit:
		return "stdin_credit"
	case FrameStdinResume:
		return "stdin_resume"
	}
	return fmt.Sprintf("frame(%d)", uint8(t))
}

// MaxFramePayload bounds a single frame; data is chunked below it.
const MaxFramePayload = 1 << 20

type Notice struct {
	Kind    string `json:"kind"`    // "queued" | "placed" | "fallback" | "info" | "warn"
	Message string `json:"message"` // one human line, no "kran:" prefix
	JobID   string `json:"job_id"`
}

type Decision struct {
	JobID   string      `json:"job_id"`
	Mode    string      `json:"mode"` // "local" | "remote"
	Node    string      `json:"node"`
	Runtime RuntimeKind `json:"runtime"`
	Reason  string      `json:"reason"` // placement rationale, e.g. "devmac: 22 cores free, est. 41s vs 96s here"
	// Envelope applies to local execution by the CLI.
	Envelope *LocalEnvelope `json:"envelope,omitempty"`
	Estimate Estimate       `json:"estimate"`
	// for a remote job, input may be forwarded below this offset
	// until a FrameStdinCredit says more (0: no flow control, old daemon).
	StdinLimit int64 `json:"stdin_limit,omitempty"`
}

// StdinCredit tells kran run how much input it may forward.
// Offsets count input bytes from the start of the job.
type StdinCredit struct {
	Consumed int64 `json:"consumed"` // the job has taken input up to here; earlier bytes may be forgotten
	Limit    int64 `json:"limit"`    // forward input only below this offset; -1: no limit
	// Resume asks kran run to send its input again from this offset (what
	// came after it was lost) and to answer with FrameStdinResume.
	Resume *int64 `json:"resume,omitempty"`
}

// StdinResume answers a credit's Resume: input follows from
// Offset. Lost means kran run no longer has that input.
type StdinResume struct {
	Offset int64 `json:"offset"`
	Lost   bool  `json:"lost,omitempty"`
}

type LocalEnvelope struct {
	MemMax int64   `json:"mem_max"` // 0 = no cap
	CPUs   float64 `json:"cpus"`
	Nice   int     `json:"nice"`
	// Slice is the systemd slice to run the scope in (Linux), e.g. "kran.slice".
	Slice string `json:"slice,omitempty"`
	// Env is merged into the child's environment (KRAN_JOB_ID etc.).
	Env map[string]string `json:"env,omitempty"`
}

type Started struct {
	JobID string `json:"job_id"`
	PID   int    `json:"pid,omitempty"`   // local jobs
	Scope string `json:"scope,omitempty"` // systemd scope unit (Linux local jobs)
	PGID  int    `json:"pgid,omitempty"`
}

type Phase struct {
	State    JobState `json:"state"`
	Message  string   `json:"message,omitempty"`  // e.g. "sending 214 files, 3.1 MB"
	Progress float64  `json:"progress,omitempty"` // 0..1 when known
}

type Signal struct {
	Name string `json:"name"` // "INT" | "TERM" | "KILL" | "HUP" | "QUIT"
}

type Exit struct {
	JobID    string   `json:"job_id"`
	Code     int      `json:"code"`             // what kran run exits with
	Signal   string   `json:"signal,omitempty"` // when killed by a signal
	State    JobState `json:"state"`
	Reason   string   `json:"reason,omitempty"` // shown on stderr for infra errors / caps
	Usage    Usage    `json:"usage"`
	Node     string   `json:"node"`
	Fallback bool     `json:"fallback,omitempty"`
	// set when the job ended before the user's command started
	// (setup failed, toolchain missing, runtime not ready). Nothing of the
	// command ran, so the origin may still fall back to running it locally.
	BeforeStart bool `json:"before_start,omitempty"`
}

type Reattach struct {
	JobID  string `json:"job_id"`
	Offset int64  `json:"offset"` // output bytes already received (stdout+stderr frames combined)
}

// Exit codes owned by kran (SPEC §11).
const (
	ExitInfra      = 125
	ExitUsage      = 2
	ExitNotExec    = 126
	ExitNotFound   = 127
	ExitSignalBase = 128
)

// ErrFrameTooLarge is returned for frames above MaxFramePayload.
var ErrFrameTooLarge = errors.New("kran: frame too large")

// WriteFrame writes one frame. Not safe for concurrent use; callers serialize.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSONFrame marshals v and writes it as one frame.
func WriteJSONFrame(w io.Writer, t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// ReadFrame reads one frame. buf is reused when large enough.
func ReadFrame(r io.Reader, buf []byte) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFramePayload {
		return 0, nil, ErrFrameTooLarge
	}
	if cap(buf) < int(n) {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return FrameType(hdr[0]), buf, nil
}
