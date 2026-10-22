package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
)

// Every linked key runs the same `kran serve --stdio`, so a peer's hello
// alone cannot tell linked machines apart. The forced command written at
// link time carries the name the key was linked as (`--peer <name>`); the
// bridge sends it to the daemon as one line ahead of the yamux bytes. Only
// processes of this user can reach the peer socket, so the line is trusted.

const peerNamePrefix = "kran-peer "

// peerNameRe mirrors config.ValidName (node names).
var peerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidPeerName reports whether name can travel in the forced command.
func ValidPeerName(name string) bool { return peerNameRe.MatchString(name) }

// ServeStdioAs is ServeStdio for a key linked as peer ("" = unknown, from a
// forced command written by an older kran).
func ServeStdioAs(ctx context.Context, peer string) int { return serveStdioAs(ctx, peer) }

// BridgeAs is Bridge that first tells the daemon which peer this is.
func BridgeAs(ctx context.Context, in io.Reader, out io.Writer, errOut io.Writer, socket, peer string) int {
	if peer == "" {
		return Bridge(ctx, in, out, errOut, socket)
	}
	if !ValidPeerName(peer) {
		fmt.Fprintf(errOut, "kran: invalid peer name %q in the forced command; run kran link again\n", peer)
		return 2
	}
	return Bridge(ctx, io.MultiReader(strings.NewReader(peerNamePrefix+peer+"\n"), in), out, errOut, socket)
}

// ReadPeerName reads the bridge's peer line, if any, from a connection
// accepted on the peer socket. It returns the name ("" when the bridge sent
// none: yamux bytes start with version 0) and a conn that yields the rest.
func ReadPeerName(c net.Conn) (string, net.Conn, error) {
	br := bufio.NewReaderSize(c, 128)
	b, err := br.Peek(1)
	if err != nil {
		return "", nil, err
	}
	rc := &bufConn{Conn: c, r: br}
	if b[0] != peerNamePrefix[0] {
		return "", rc, nil
	}
	line, err := br.ReadSlice('\n')
	if err != nil {
		return "", nil, errors.New("peer name line too long or cut off")
	}
	name, ok := strings.CutPrefix(strings.TrimSuffix(string(line), "\n"), peerNamePrefix)
	if !ok || !ValidPeerName(name) {
		return "", nil, fmt.Errorf("invalid peer name line %q", line)
	}
	return name, rc, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
