package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/proto"
)

func TestUnknownNodeError(t *testing.T) {
	peers := func(names ...string) func() (*config.Peers, error) {
		return func() (*config.Peers, error) {
			p := &config.Peers{}
			for _, n := range names {
				p.Peer = append(p.Peer, config.Peer{Name: n})
			}
			return p, nil
		}
	}
	cases := []struct {
		node, self string
		load       func() (*config.Peers, error)
		want       string
	}{
		{"", "gx10", peers(), ""},
		{"gx10", "gx10", peers(), ""},
		{"devmac", "gx10", peers("e2e", "devmac"), ""},
		{"devmc", "gx10", peers("e2e", "devmac"), `no machine named "devmc" (linked: devmac, e2e). Link it with: kran link devmc`},
		{"devmac", "gx10", peers(), `no machine named "devmac" (no machines linked yet). Link it with: kran link devmac`},
		{"x", "gx10", func() (*config.Peers, error) { return nil, errors.New("broken") }, ""},
	}
	for _, c := range cases {
		if got := unknownNodeError(c.node, c.self, c.load); got != c.want {
			t.Errorf("%s: %q, want %q", c.node, got, c.want)
		}
	}
}

func TestRunNodePinToUnknownMachineIsAUsageError(t *testing.T) {
	d := t.TempDir()
	t.Setenv("KRAN_CONFIG_DIR", d)
	t.Setenv("KRAN_STATE_DIR", filepath.Join(d, "state"))
	if err := os.WriteFile(filepath.Join(d, "config.toml"), []byte("name = \"gx10\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "peers.toml"), []byte("[[peer]]\nname = \"devmac\"\n\n[[peer]]\nname = \"e2e\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, out, errb := testApp(nil)
	code := a.Execute(context.Background(), []string{"run", "--node", "X", "--", "true"})
	if code != proto.ExitUsage {
		t.Fatalf("exit %d, want 2 (stderr %q)", code, errb)
	}
	want := `kran: no machine named "X" (linked: devmac, e2e). Link it with: kran link X` + "\n"
	if errb.String() != want || out.Len() != 0 {
		t.Fatalf("stderr %q, want %q", errb, want)
	}
}

func TestPinnedNodeAsksTheDaemon(t *testing.T) {
	cl := &stateClient{st: sampleState()}
	for _, n := range []string{"devmac", "gx10", ""} {
		if u, nt := pinnedNode(context.Background(), cl, n); u != "" || nt != "" {
			t.Fatalf("%q: usage %q notice %q", n, u, nt)
		}
	}
	_, got := pinnedNode(context.Background(), cl, "old")
	if !strings.HasPrefix(got, "old is offline: ssh: connection refused; the job waits for it") || !strings.Contains(got, "drop --node") {
		t.Fatalf("offline notice %q", got)
	}
	u, _ := pinnedNode(context.Background(), cl, "devmc")
	if u != `no machine named "devmc" (linked: devmac, old). Link it with: kran link devmc` {
		t.Fatalf("usage %q", u)
	}
	if u, nt := pinnedNode(context.Background(), &fakeClient{}, "old"); u != "" || nt != "" {
		t.Fatalf("no state: %q %q", u, nt)
	}
}

// With a daemon, its view decides (it may know machines this peers.toml
// does not): an unknown pin ends kran run with exit 2 before submitting.
func TestRunNodePinCheckedAgainstTheDaemon(t *testing.T) {
	s := newFakeSession()
	cl := &stateFakeClient{fakeClient: fakeClient{sessions: []*fakeSession{s}}, st: sampleState()}
	h := newHarness(t, nil, "true")
	h.r.dial = func(context.Context) (client.Client, error) { return cl, nil }
	cl.version.API = proto.APIVersion
	h.r.spec.Req.Node = "nope"
	h.r.unknownNode = func(string) string { return "" }
	if code := h.run(t); code != proto.ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), `kran: no machine named "nope" (linked: devmac, old)`) || s.count(proto.FrameSubmit) != 0 {
		t.Fatalf("stderr %q, submitted %d", h.stderr.String(), s.count(proto.FrameSubmit))
	}
}

type stateFakeClient struct {
	fakeClient
	st *proto.State
}

func (c *stateFakeClient) State(context.Context) (*proto.State, error) { return c.st, nil }
