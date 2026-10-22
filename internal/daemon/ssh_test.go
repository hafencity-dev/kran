package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// TestRemoteRunOverSSH runs the real stack: SSH dialer with a pinned host
// key → in-process sshd with the forced-command bridge → b's peer socket.
func TestRemoteRunOverSSH(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	a.place = preferRemote("b")
	b.peers.set([]PeerConfig{{Name: "a", ID: "a-id", Addresses: []string{"mem"}, Direction: "inbound"}})
	b.start()

	key, err := transport.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := transport.NewSSHServer(transport.SSHServerConfig{
		AuthorizedKeys: []ssh.PublicKey{key.PublicKey()},
		Socket:         b.dirs.peerSocket(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	td := &transport.Dialer{Signer: key, Session: testSessionConfig()}
	a.dialer = sshDialer{d: td, logf: t.Logf}
	// a reaches b over SSH only; b does not dial back (inbound only).
	a.peers.set([]PeerConfig{{Name: "b", ID: "b-id", Addresses: []string{"127.0.0.1:1", srv.Addr()},
		User: "kran", HostKey: srv.HostKey(), Direction: "outbound"}})
	a.start()
	c.waitOnline("a", "b")

	ws := workspaceDir(t, map[string]string{"src/main.txt": "over ssh\n"})
	ctx, cancel := testContext(30 * time.Second)
	defer cancel()
	res, err := runCLI(ctx, a.sock(), shSpec(ws, "cat src/main.txt; exit 9"), cliOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.out() != "over ssh\n" || res.exit.Code != 9 || res.exit.Node != "b" {
		t.Fatalf("out %q exit %+v notices %+v", res.out(), res.exit, res.notices)
	}
	st := a.daemon().State()
	for _, n := range st.Nodes {
		if n.Info.Name == "b" && (n.Link.Address != srv.Addr() || !n.Link.Connected) {
			t.Fatalf("link %+v", n.Link)
		}
	}
	// The connection drops (sshd restart): a reconnects by itself. Wait for
	// a new session, not for "connected": right after the drop a still
	// reports the old one until it notices.
	old := a.daemon().peers.get("b").active()
	srv.DropConnections()
	waitFor(t, 10*time.Second, "b reconnected", func() bool {
		s := a.daemon().peers.get("b").active()
		ns, ok := a.scheduler().node("b")
		return s != nil && s != old && ok && ns.Status == proto.NodeOnline && ns.Link.Connected
	})
	res, err = runCLI(ctx, a.sock(), shSpec(ws, "echo again"), cliOpts{})
	if err != nil || res.out() != "again\n" || res.exit.Node != "b" {
		t.Fatalf("after reconnect: %v %q %+v", err, res.out(), res.exit)
	}

	// The SSH connection dies while the job streams: a reattaches.
	script, want := counterScript(30, "0.05")
	var once sync.Once
	res, err = runCLI(ctx, a.sock(), shSpec(ws, script), cliOpts{
		hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
			if ft == proto.FrameStdout && reachedLine(r, p, "4") {
				once.Do(srv.DropConnections)
			}
			return nil
		}})
	if err != nil || res.out() != want || res.exit.Code != 0 || res.exit.Node != "b" {
		t.Fatalf("drop mid-job: %v %q %+v", err, res.out(), res.exit)
	}
}

// TestPeersFileWatch: editing peers.toml connects and disconnects nodes
// without a restart.
func TestPeersFileWatch(t *testing.T) {
	c := newCluster(t, "a", "b")
	a, b := c.nodes["a"], c.nodes["b"]
	b.start()
	peers := filepath.Join(a.dirs.Config, "peers.toml")
	os.MkdirAll(a.dirs.Config, 0o700)
	a.src = filePeerSource{path: peers}
	a.start()
	time.Sleep(200 * time.Millisecond)
	if _, ok := a.scheduler().node("b"); ok {
		t.Fatal("b known before it was linked")
	}
	writeFileAtomic(peers, []byte(`[[peer]]
name = "b"
id = "b-id"
addresses = ["mem:22"]
user = "u"
host_key = "ssh-ed25519 AAAA"
direction = "both"
`), 0o600)
	c.waitOnline("a", "b")
	writeFileAtomic(peers, []byte("# unlinked\n"), 0o600)
	waitFor(t, 5*time.Second, "b removed", func() bool {
		_, ok := a.scheduler().node("b")
		return !ok && len(a.daemon().State().Nodes) == 1
	})
	// A broken file keeps the current peers.
	writeFileAtomic(peers, []byte(`[[peer]]
name = "b"
id = "b-id"
addresses = ["mem:22"]
user = "u"
host_key = "k"
direction = "both"
`), 0o600)
	c.waitOnline("a", "b")
	writeFileAtomic(peers, []byte("[[peer]\nbroken"), 0o600)
	time.Sleep(300 * time.Millisecond)
	if ns, ok := a.scheduler().node("b"); !ok || ns.Status != proto.NodeOnline {
		t.Fatal("a broken peers.toml disconnected b")
	}
}

// writeFileAtomic replaces a file the way kran link does (temp + rename),
// so a watcher never sees it half-written: on a loaded machine the gap
// between os.WriteFile's truncate and write can span several polls.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
