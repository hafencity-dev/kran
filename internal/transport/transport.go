// Package transport carries the peer protocol: SSH dialing with the kran key
// and pinned host keys, the yamux session over the forced-command stdio, the
// `kran serve --stdio` bridge to the local peer socket, and an in-memory
// transport for tests. SPEC §3, §8.3.
package transport

import (
	"context"
	"errors"
	"net"
	"time"
)

// ServeStdio implements `kran serve --stdio`: it bridges stdin/stdout to the
// local daemon's peer socket. Returns the process exit code.
func ServeStdio(ctx context.Context) int { return serveStdio(ctx) }

// Session is one multiplexed connection to a peer daemon. Either side may
// open streams; each stream is an independent net.Conn.
type Session interface {
	// Open starts a new stream to the other side.
	Open(ctx context.Context) (net.Conn, error)
	// Accept waits for a stream opened by the other side.
	Accept() (net.Conn, error)
	// Close tears the session down (all streams fail).
	Close() error
	// Done is closed once the session has ended for any reason.
	Done() <-chan struct{}
	// Err says why the session ended (nil while it is alive).
	Err() error
	// RTT is the last measured round-trip time (0 before the first ping).
	RTT() time.Duration
	// RemoteAddr names the address in use ("mem" for in-memory sessions).
	RemoteAddr() string
}

// Errors callers may test with errors.Is.
var (
	// ErrDaemonDown: the SSH side worked but no kran daemon listens on the peer.
	ErrDaemonDown = errors.New("kran daemon is not running")
	// ErrHostKey: the peer presented a host key other than the pinned one.
	ErrHostKey = errors.New("host key mismatch")
	// ErrAuth: the peer refused the kran key.
	ErrAuth = errors.New("kran key refused")
	// ErrKeepalive: nothing arrived from the peer for longer than the link tolerates.
	ErrKeepalive = errors.New("peer stopped answering")
	// ErrAlreadyRunning: another process serves the socket.
	ErrAlreadyRunning = errors.New("another kran daemon is already running")
)

// ExitDaemonDown is the exit code of `kran serve --stdio` when the local
// daemon is not running (EX_UNAVAILABLE). The dialer maps it to ErrDaemonDown.
const ExitDaemonDown = 69

// ExitSocketDenied is the exit code when the peer socket exists but cannot be
// opened (EX_NOPERM).
const ExitSocketDenied = 77
