// Package transport carries the peer protocol: SSH dialing with the kran key
// and pinned host keys, the yamux session over the forced-command stdio, the
// `kran serve --stdio` bridge to the local peer socket, and an in-memory
// transport for tests. SPEC §3, §8.3. Owner: A.
package transport

import "context"

// ServeStdio implements `kran serve --stdio`: it bridges stdin/stdout to the
// local daemon's peer socket. Returns the process exit code.
func ServeStdio(ctx context.Context) int { return serveStdio(ctx) }
