package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

type stateClient struct {
	fakeClient
	st   *proto.State
	jobs []proto.Job
	log  string
	got  []string
}

func (c *stateClient) State(context.Context) (*proto.State, error) { return c.st, nil }
func (c *stateClient) Jobs(context.Context, int) ([]proto.Job, error) {
	return c.jobs, nil
}
func (c *stateClient) Log(_ context.Context, id string, f bool, w io.Writer) error {
	c.got = append(c.got, "log:"+id)
	_, err := io.WriteString(w, c.log)
	return err
}
func (c *stateClient) Cancel(_ context.Context, id string) error {
	c.got = append(c.got, "cancel:"+id)
	return nil
}
func (c *stateClient) Drain(_ context.Context, n string) error {
	c.got = append(c.got, "drain:"+n)
	return nil
}
func (c *stateClient) Close() error { return nil }

func testApp(cl client.Client) (*App, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	a := &App{Out: out, Err: errb, Env: func() []string { return nil }, In: nil,
		Dial: func(context.Context) (client.Client, error) {
			if cl == nil {
				return nil, client.ErrNoDaemon
			}
			return cl, nil
		},
		Self: func() (string, error) { return "/usr/bin/kran", nil }}
	return a, out, errb
}

func sampleState() *proto.State {
	return &proto.State{
		Self: "gx10",
		Nodes: []proto.NodeState{
			{Self: true, Status: proto.NodeOnline, Accepts: true, Info: proto.NodeInfo{Name: "gx10", CPUs: 20, MemTotal: 119 << 30},
				Free: proto.Resources{CPUs: 18, Mem: 96 << 30}, Running: 1},
			{Status: proto.NodeOnline, Accepts: true, Info: proto.NodeInfo{Name: "devmac", CPUs: 32, MemTotal: 256 << 30},
				Free: proto.Resources{CPUs: 28, Mem: 200 << 30}, Link: proto.LinkInfo{Connected: true, RTTMillis: 3, BandwidthBps: 480 << 20}},
			{Status: proto.NodeOffline, Reason: "ssh: connection refused", Info: proto.NodeInfo{Name: "old"}},
		},
		Jobs: []proto.Job{
			{Spec: proto.JobSpec{ID: "j1", Argv: []string{"go", "test", "./..."}}, State: proto.JobRunning, Node: "devmac", QueuedAt: time.Now().Add(-30 * time.Second)},
			{Spec: proto.JobSpec{ID: "j2", Shell: "make test"}, State: proto.JobQueued, Reason: "needs 6 GiB, devmac has 3.1 GiB free", QueuedAt: time.Now()},
			{Spec: proto.JobSpec{ID: "j0"}, State: proto.JobSucceeded},
		},
		Stats: proto.Stats{JobsToday: 14, OffloadedToday: 9, FallbacksToday: 1, SavedSecondsToday: 125},
	}
}

func TestStatusHuman(t *testing.T) {
	a, out, _ := testApp(&stateClient{st: sampleState()})
	if code := a.Execute(context.Background(), []string{"status"}); code != 0 {
		t.Fatal(code)
	}
	s := out.String()
	for _, want := range []string{"gx10 (this machine)", "18 of 20 cpus free", "devmac", "rtt 3 ms", "480.0 MiB/s", "ssh: connection refused",
		"go test ./...", "waiting: needs 6 GiB", "make test", "14 jobs, 9 offloaded", "2m05s saved"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "j0") || strings.Contains(s, "\x1b[") {
		t.Errorf("finished jobs and color codes must not show:\n%s", s)
	}
}

func TestStatusJSONAndNoDaemon(t *testing.T) {
	a, out, _ := testApp(&stateClient{st: sampleState()})
	if code := a.Execute(context.Background(), []string{"status", "--json"}); code != 0 || !strings.Contains(out.String(), `"self": "gx10"`) {
		t.Fatal(code, out.String())
	}
	a, _, errb := testApp(nil)
	if code := a.Execute(context.Background(), []string{"status"}); code != 1 || !strings.Contains(errb.String(), "kran service start") {
		t.Fatal(code, errb.String())
	}
}

func TestCancelResolvesPrefixAndDrain(t *testing.T) {
	cl := &stateClient{jobs: []proto.Job{{Spec: proto.JobSpec{ID: "01abc"}}, {Spec: proto.JobSpec{ID: "01abd"}}, {Spec: proto.JobSpec{ID: "02zzz"}}}}
	a, out, errb := testApp(cl)
	if code := a.Execute(context.Background(), []string{"cancel", "02"}); code != 0 || cl.got[0] != "cancel:02zzz" {
		t.Fatal(code, cl.got, errb.String())
	}
	if code := a.Execute(context.Background(), []string{"cancel", "01"}); code != 1 || !strings.Contains(errb.String(), "matches 2 jobs") {
		t.Fatal(code, errb.String())
	}
	if code := a.Execute(context.Background(), []string{"cancel", "zz9"}); code != 1 || !strings.Contains(errb.String(), "no job matches") {
		t.Fatal(code, errb.String())
	}
	if code := a.Execute(context.Background(), []string{"drain", "devmac"}); code != 0 || cl.got[len(cl.got)-1] != "drain:devmac" || !strings.Contains(out.String(), "kran resume devmac") {
		t.Fatal(code, cl.got)
	}
}

func TestLogsDefaultsToLatest(t *testing.T) {
	now := time.Now()
	cl := &stateClient{log: "hello\n", jobs: []proto.Job{{Spec: proto.JobSpec{ID: "old"}, QueuedAt: now.Add(-time.Hour)}, {Spec: proto.JobSpec{ID: "new"}, QueuedAt: now}}}
	a, out, _ := testApp(cl)
	if code := a.Execute(context.Background(), []string{"logs"}); code != 0 || cl.got[0] != "log:new" || out.String() != "hello\n" {
		t.Fatal(code, cl.got, out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	a, _, errb := testApp(nil)
	for _, args := range [][]string{{"run", "--local", "--remote", "x"}, {"run", "--sh", "a", "b"}, {"status", "--nope"}, {"cancel"}, {"serve"}} {
		errb.Reset()
		if code := a.Execute(context.Background(), args); code != 2 {
			t.Errorf("%v: code %d (%s)", args, code, errb.String())
		}
		if !strings.HasPrefix(errb.String(), "kran: ") {
			t.Errorf("%v: %q", args, errb.String())
		}
	}
}

func TestRunParsingKeepsCommandFlags(t *testing.T) {
	// Flags after the command belong to the command: `kran run go test -v`.
	a, _, _ := testApp(nil)
	a.Env = os.Environ
	cmd := a.runCommand()
	cmd.SetArgs([]string{"-q", "go", "test", "-v", "--", "-x"})
	var got []string
	cmd.RunE = func(c *cobra.Command, args []string) error { got = args; return nil }
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "go test -v -- -x" {
		t.Fatalf("%v", got)
	}
}

func TestUIURL(t *testing.T) {
	tok := func() string { return "sekret" }
	ts := func() string { return "100.64.0.1" }
	for listen, want := range map[string]string{
		"127.0.0.1:7270": "http://127.0.0.1:7270/#token=sekret",
		"localhost:9000": "http://localhost:9000/#token=sekret",
		"tailscale":      "http://100.64.0.1:7270/#token=sekret",
		"0.0.0.0:7270":   "http://0.0.0.0:7270/#token=sekret",
	} {
		got, err := uiURL(listen, tok, ts)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", listen, got, err)
		}
	}
	if _, err := uiURL("", tok, ts); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal(err)
	}
	if _, err := uiURL("tailscale", tok, func() string { return "" }); err == nil {
		t.Fatal("expected error")
	}
}

func TestTopNeedsHook(t *testing.T) {
	a, _, errb := testApp(&stateClient{})
	if code := a.Execute(context.Background(), []string{"top"}); code != 1 || !strings.Contains(errb.String(), "not available") {
		t.Fatal(code, errb.String())
	}
	called := false
	a.Hooks.Top = func(ctx context.Context, c client.Client) error { called = true; return nil }
	if code := a.Execute(context.Background(), []string{"top"}); code != 0 || !called {
		t.Fatal(code)
	}
	a.Hooks.Top = func(ctx context.Context, c client.Client) error { return errors.New("boom") }
	if code := a.Execute(context.Background(), []string{"top"}); code != 1 {
		t.Fatal(code)
	}
}

func TestColorOnlyOnTTYWithoutNoColor(t *testing.T) {
	if colorAllowed([]string{"NO_COLOR=1"}) || colorAllowed([]string{"TERM=dumb"}) || !colorAllowed([]string{"NO_COLOR="}) {
		t.Fatal("color policy")
	}
	a, _, _ := testApp(nil)
	if a.styleOut().Red("x") != "x" {
		t.Fatal("no color off a tty")
	}
	a.outTTY = true
	a.Env = func() []string { return nil }
	if a.styleOut().Red("x") == "x" {
		t.Fatal("color on a tty")
	}
}

func TestServiceInstallSystemOnLinuxExplains(t *testing.T) {
	if runtimeGOOS() != "linux" {
		t.Skip("linux-only message")
	}
	a, _, errb := testApp(nil)
	if code := a.Execute(context.Background(), []string{"service", "install", "--system"}); code != 2 || !strings.Contains(errb.String(), "enable-linger") {
		t.Fatal(code, errb.String())
	}
}

func TestServiceInstallDryRunChangesNothing(t *testing.T) {
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", d+"/xdg")
	t.Setenv("KRAN_STATE_DIR", d+"/state")
	a, out, _ := testApp(nil)
	if code := a.Execute(context.Background(), []string{"service", "install", "--dry-run"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "daemon") || !strings.Contains(out.String(), "Dry run: nothing was changed") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(d + "/xdg"); err == nil {
		t.Fatal("dry run wrote files")
	}
}

func TestNoGUIMessageNamesTheCommand(t *testing.T) {
	for _, want := range []string{"on demand", "kran service install --system", "password"} {
		if !strings.Contains(noGUIMessage, want) {
			t.Errorf("message lacks %q", want)
		}
	}
}

func TestParseKVNames(t *testing.T) {
	if m, err := parseKV([]string{"KRAN_CONFIG_DIR=/a b"}); err != nil || m["KRAN_CONFIG_DIR"] != "/a b" {
		t.Fatal(m, err)
	}
	for _, bad := range []string{"=x", "A;rm -rf ~;B=x", "A B=x", "1A=x"} {
		if _, err := parseKV([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The token must not travel on the browser's command line (visible to every
// user in ps): kran ui opens a private file that redirects to the URL.
func TestOpenUIKeepsTokenOffTheCommandLine(t *testing.T) {
	url := "http://127.0.0.1:7270/#token=" + strings.Repeat("ab", 32)
	var opened string
	var mode os.FileMode
	var content []byte
	open := func(target string) error {
		opened = target
		st, err := os.Stat(strings.TrimPrefix(target, "file://"))
		if err != nil {
			return err
		}
		mode = st.Mode().Perm()
		content, _ = os.ReadFile(strings.TrimPrefix(target, "file://"))
		return nil
	}
	if err := openUI(url, open, func() {}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opened, "token") || strings.Contains(opened, "abab") {
		t.Fatalf("token on the command line: %q", opened)
	}
	if mode != 0o600 || !strings.Contains(string(content), url) || !strings.Contains(string(content), "no-referrer") {
		t.Fatalf("mode %v content %s", mode, content)
	}
	if _, err := os.Stat(strings.TrimPrefix(opened, "file://")); !os.IsNotExist(err) {
		t.Fatal("redirect file not removed")
	}
}

// SPEC §9: jobs never run as root, and the daemon runs every job.
func TestDaemonRefusesRoot(t *testing.T) {
	old := getuid
	getuid = func() int { return 0 }
	defer func() { getuid = old }()
	a, _, errb := testApp(nil)
	if code := a.Execute(context.Background(), []string{"daemon"}); code != 1 || !strings.Contains(errb.String(), "--allow-root") {
		t.Fatalf("code %d: %s", code, errb.String())
	}
}
