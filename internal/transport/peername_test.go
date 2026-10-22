package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// The forced command names the peer the key was linked as; the bridge hands
// that name to the daemon ahead of the yamux bytes, so a linked machine
// cannot claim to be another one in its hello.
func TestBridgeSendsPeerName(t *testing.T) {
	for _, name := range []string{"devmac", ""} {
		sock := filepath.Join(shortDir(t), "p.sock")
		l, err := ListenUnix(sock)
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan [2]string, 1)
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			peer, rc, err := ReadPeerName(c)
			if err != nil {
				got <- [2]string{"error", err.Error()}
				return
			}
			b := make([]byte, 4)
			io.ReadFull(rc, b)
			got <- [2]string{peer, string(b)}
		}()
		inR, inW := io.Pipe()
		go BridgeAs(context.Background(), inR, io.Discard, io.Discard, sock, name)
		inW.Write([]byte("\x00abc")) // yamux frames start with version 0
		select {
		case g := <-got:
			if g[0] != name || g[1] != "\x00abc" {
				t.Fatalf("name %q: got %q", name, g)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timeout")
		}
		inW.Close()
		l.Close()
	}
}

func TestReadPeerNameRejectsGarbage(t *testing.T) {
	for _, pre := range []string{"kran-peer ../x\n", "kran-peer " + string(bytes.Repeat([]byte("a"), 200)) + "\n", "kran-peer a b\n"} {
		a, b := net.Pipe()
		go func() { a.Write([]byte(pre)); a.Close() }()
		b.SetDeadline(time.Now().Add(time.Second))
		if name, _, err := ReadPeerName(b); err == nil {
			t.Errorf("%q accepted as %q", pre, name)
		}
		b.Close()
	}
}
