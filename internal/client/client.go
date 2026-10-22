// Package client talks to the local daemon over its unix socket. Used by the
// CLI and `kran top`.
package client

import (
	"context"
	"io"

	"github.com/hafencity-dev/kran/internal/proto"
)

type Client interface {
	Version(ctx context.Context) (proto.VersionInfo, error)
	State(ctx context.Context) (*proto.State, error)
	// Events streams events until ctx ends; the first event is EventState.
	// Reconnects transparently (re-sending a fresh EventState).
	Events(ctx context.Context) (<-chan proto.Event, error)
	Jobs(ctx context.Context, limit int) ([]proto.Job, error)
	Job(ctx context.Context, id string) (*proto.Job, error)
	Log(ctx context.Context, id string, follow bool, w io.Writer) error
	Cancel(ctx context.Context, id string) error
	SetPriority(ctx context.Context, id string, p proto.Priority) error
	Drain(ctx context.Context, node string) error
	Resume(ctx context.Context, node string) error
	// Run opens a run session (SPEC §8.2). The returned session is driven
	// by the CLI's run command.
	Run(ctx context.Context) (RunSession, error)
	Close() error
}

// RunSession is a framed bidirectional stream with the daemon.
type RunSession interface {
	Send(t proto.FrameType, payload []byte) error
	SendJSON(t proto.FrameType, v any) error
	// Recv returns the next frame, heartbeats included (they prove the
	// daemon is alive); the payload is valid until the next Recv.
	Recv() (proto.FrameType, []byte, error)
	Close() error
}

// ErrNoDaemon is returned by Dial when no daemon is listening.
var ErrNoDaemon = errNoDaemon

// Dial connects to the local daemon socket (paths.Socket()).
func Dial(ctx context.Context) (Client, error) { return dial(ctx, "") }

// DialSocket connects to a specific socket path (tests, multi-daemon e2e).
func DialSocket(ctx context.Context, socket string) (Client, error) { return dial(ctx, socket) }
