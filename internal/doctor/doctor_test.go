package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/link"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
	"github.com/hafencity-dev/kran/internal/version"
)

type fakeClient struct {
	client.Client
	ver proto.VersionInfo
	st  *proto.State
}

func (f *fakeClient) Version(context.Context) (proto.VersionInfo, error) { return f.ver, nil }
func (f *fakeClient) State(context.Context) (*proto.State, error)        { return f.st, nil }
func (f *fakeClient) Close() error                                       { return nil }

type fakeSvc struct {
	st        service.Status
	installed int
	started   int
	restarted int
	refreshed int
}

func (f *fakeSvc) Refresh(context.Context) (bool, error) {
	f.refreshed++
	f.st.Outdated = false
	return true, nil
}

func (f *fakeSvc) Install(context.Context) (bool, error) { f.installed++; return true, nil }
func (f *fakeSvc) Uninstall(context.Context) error       { return nil }
func (f *fakeSvc) Start(context.Context) error           { f.started++; return nil }
func (f *fakeSvc) Stop(context.Context) error            { return nil }
func (f *fakeSvc) Restart(context.Context, service.RestartOptions) error {
	f.restarted++
	return nil
}
func (f *fakeSvc) Status(context.Context) (service.Status, error) { return f.st, nil }
func (f *fakeSvc) Unit() string                                   { return "" }

func baseOpts(t *testing.T) Options {
	t.Helper()
	d := t.TempDir()
	t.Setenv("KRAN_CONFIG_DIR", filepath.Join(d, "cfg"))
	t.Setenv("KRAN_STATE_DIR", filepath.Join(d, "state"))
	t.Setenv("KRAN_RUNTIME_DIR", filepath.Join(d, "rt"))
	return Options{
		Dial:     func(context.Context) (client.Client, error) { return nil, client.ErrNoDaemon },
		LookPath: func(s string) (string, error) { return "/bin/" + s, nil },
		DiskFree: func(string) (int64, int64, error) { return 100 << 30, 500 << 30, nil },
		Exec:     func(context.Context, string, ...string) (string, error) { return "27.0", nil },
		Peers:    func() (*config.Peers, error) { return &config.Peers{}, nil },
		Now:      time.Now,
		GOOS:     "linux",
	}
}

func find(rs []Result, id string) *Result {
	for i := range rs {
		if rs[i].ID == id {
			return &rs[i]
		}
	}
	return nil
}

func TestIdentityFix(t *testing.T) {
	o := baseOpts(t)
	rs := Run(context.Background(), o)
	if r := find(rs, "identity.node"); r == nil || r.Level != Fail || r.Fix == "" {
		t.Fatalf("%+v", r)
	}
	if !Failed(rs) {
		t.Fatal("must report failure")
	}
	o.Fix = true
	rs = Run(context.Background(), o)
	if r := find(rs, "identity.node"); r.Level != OK || !r.Fixed {
		t.Fatalf("%+v", r)
	}
	if r := find(rs, "identity.key"); r.Level != OK || !r.Fixed {
		t.Fatalf("%+v", r)
	}
	if config.NodeID() == "" {
		t.Fatal("node id not created")
	}
}

func TestPermissionsFix(t *testing.T) {
	o := baseOpts(t)
	o.Fix = true
	Run(context.Background(), o) // creates key
	key := filepath.Join(os.Getenv("KRAN_CONFIG_DIR"), "id_ed25519")
	os.Chmod(key, 0o644)
	o.Fix = false
	rs := Run(context.Background(), o)
	r := find(rs, "files.perms")
	if r == nil || r.Level != Warn || !strings.Contains(r.Detail, "id_ed25519 is 0644") {
		t.Fatalf("%+v", r)
	}
	o.Fix = true
	rs = Run(context.Background(), o)
	if r := find(rs, "files.perms"); r.Level != OK || !r.Fixed {
		t.Fatalf("%+v", r)
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
}

func TestServiceChecks(t *testing.T) {
	o := baseOpts(t)
	f := &fakeSvc{}
	o.Service = func() (service.Manager, error) { return f, nil }
	rs := Run(context.Background(), o)
	if r := find(rs, "service"); r.Level != Fail || r.Fix != "kran service install" {
		t.Fatalf("%+v", r)
	}
	o.Fix = true
	Run(context.Background(), o)
	if f.installed != 1 {
		t.Fatal("fix must install")
	}
	f.st = service.Status{Installed: true}
	o.Fix = false
	if r := find(Run(context.Background(), o), "service"); r.Fix != "kran service start" {
		t.Fatalf("%+v", r)
	}
	no := false
	f.st = service.Status{Installed: true, Running: true, PID: 5, Linger: &no, Manager: "systemd"}
	rs = Run(context.Background(), o)
	if r := find(rs, "service"); r.Level != Fail || !strings.Contains(r.Detail, "does not answer") {
		t.Fatalf("%+v", r)
	}
	if r := find(rs, "linger"); r == nil || r.Level != Warn || !strings.Contains(r.Fix, "enable-linger") {
		t.Fatalf("%+v", r)
	}
}

// A unit written by an older kran (without the daemon's priority settings)
// is reported, and --fix rewrites it.
func TestOutdatedServiceUnit(t *testing.T) {
	o := baseOpts(t)
	f := &fakeSvc{st: service.Status{Installed: true, Running: true, PID: 5, Manager: "systemd", Mode: "systemd", Outdated: true}}
	o.Service = func() (service.Manager, error) { return f, nil }
	r := find(Run(context.Background(), o), "service.unit")
	if r == nil || r.Level != Warn || !strings.Contains(r.Fix, "kran doctor --fix") {
		t.Fatalf("%+v", r)
	}
	o.Fix = true
	r = find(Run(context.Background(), o), "service.unit")
	if f.refreshed != 1 || r == nil || !r.Fixed {
		t.Fatalf("fix: refreshed %d, %+v", f.refreshed, r)
	}
	o.Fix = false
	if r := find(Run(context.Background(), o), "service.unit"); r != nil {
		t.Fatalf("current unit reported: %+v", r)
	}
}

func TestVersionSkew(t *testing.T) {
	o := baseOpts(t)
	f := &fakeSvc{}
	o.Service = func() (service.Manager, error) { return f, nil }
	f.st = service.Status{Installed: true, Running: true}
	st := &proto.State{Nodes: []proto.NodeState{
		{Self: true, Info: proto.NodeInfo{Name: "gx10"}},
		{Info: proto.NodeInfo{Name: "devmac", Version: "0.0.1-other"}, Status: proto.NodeOnline},
	}}
	o.Dial = func(context.Context) (client.Client, error) {
		return &fakeClient{ver: proto.VersionInfo{Version: "0.0.0-old"}, st: st}, nil
	}
	rs := Run(context.Background(), o)
	if r := find(rs, "version.daemon"); r == nil || r.Level != Warn || !strings.Contains(r.Detail, "0.0.0-old") {
		t.Fatalf("%+v", r)
	}
	if r := find(rs, "version.peer"); r == nil || r.Node != "devmac" || r.Fix != "kran upgrade" {
		t.Fatalf("%+v", r)
	}
	o.Fix = true
	Run(context.Background(), o)
	if f.restarted != 1 {
		t.Fatal("fix must restart the daemon")
	}
	_ = version.Version
}

func TestDiskFloor(t *testing.T) {
	o := baseOpts(t)
	o.DiskFree = func(string) (int64, int64, error) { return 5 << 30, 500 << 30, nil }
	if r := find(Run(context.Background(), o), "disk"); r.Level != Warn || !strings.Contains(r.Detail, "floor") {
		t.Fatalf("%+v", r)
	}
	// 5% of a 1 TiB disk (51 GiB) beats the 10 GiB floor.
	o.DiskFree = func(string) (int64, int64, error) { return 30 << 30, 1 << 40, nil }
	if r := find(Run(context.Background(), o), "disk"); r.Level != Warn {
		t.Fatalf("%+v", r)
	}
}

func TestPeerResultHostKeyChange(t *testing.T) {
	p := config.Peer{Name: "devmac"}
	r := peerResult(p, []link.ProbeResult{{Address: "a:22", Error: "host key changed: expected X, got Y"}, {Address: "b:22", Error: "timeout"}})
	if r.Level != Fail || r.ID != "peer.hostkey" || !strings.Contains(r.Fix, "intercepting") {
		t.Fatalf("%+v", r)
	}
	r = peerResult(p, []link.ProbeResult{{Address: "a:22", Error: "timeout"}, {Address: "b:22", OK: true, RTTMs: 4}})
	if r.Level != OK || !strings.Contains(r.Detail, "b:22") || !strings.Contains(r.Detail, "a:22: timeout") {
		t.Fatalf("%+v", r)
	}
	r = peerResult(p, []link.ProbeResult{{Address: "a:22", Error: "refused"}})
	if r.Level != Fail || !strings.Contains(r.Fix, "kran link devmac") {
		t.Fatalf("%+v", r)
	}
}

func TestStateChecks(t *testing.T) {
	o := baseOpts(t)
	now := time.Now()
	c := &checker{o: o, state: &proto.State{Nodes: []proto.NodeState{{
		Info:    proto.NodeInfo{Name: "devmac", DiskTotal: 1 << 40},
		Status:  proto.NodeOnline,
		Metrics: proto.NodeMetrics{Time: now.Add(-42 * time.Second), DiskFree: 8 << 30},
		Link:    proto.LinkInfo{BandwidthBps: 2 << 20, ClockOffsetMillis: -42000, ClockRTTMillis: 4},
	}}}}
	c.o.Now = func() time.Time { return now }
	c.stateChecks("devmac")
	for _, id := range []string{"clock", "disk.peer", "link.speed"} {
		if find(c.results, id) == nil {
			t.Errorf("missing %s: %+v", id, c.results)
		}
	}
	c = &checker{o: o, state: &proto.State{Nodes: []proto.NodeState{{Info: proto.NodeInfo{Name: "devmac"}, Status: proto.NodeOffline, Reason: "ssh timeout"}}}}
	c.stateChecks("devmac")
	if r := find(c.results, "peer.status"); r == nil || !strings.Contains(r.Detail, "ssh timeout") {
		t.Fatalf("%+v", c.results)
	}
}

// The clock check uses the daemon's offset measurement over the live link
// (midpoint of a request, the fastest of several), not the age of the last
// metrics sample, which lags by seconds: devmac and gx10 agreed within 1 s
// while doctor warned about 6 s.
func TestClockSkewFromLinkMeasurement(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		link proto.LinkInfo
		warn bool
	}{
		{"stale metrics, no measurement", proto.LinkInfo{}, false},
		{"in sync", proto.LinkInfo{ClockOffsetMillis: 300, ClockRTTMillis: 5}, false},
		{"6 s off", proto.LinkInfo{ClockOffsetMillis: 6000, ClockRTTMillis: 5}, true},
		{"6 s off but the measurement is too coarse", proto.LinkInfo{ClockOffsetMillis: 6000, ClockRTTMillis: 3000}, false},
	} {
		o := baseOpts(t)
		c := &checker{o: o, state: &proto.State{Nodes: []proto.NodeState{{
			Info: proto.NodeInfo{Name: "devmac"}, Status: proto.NodeOnline,
			Metrics: proto.NodeMetrics{Time: now.Add(-6 * time.Second)}, Link: tc.link,
		}}}}
		c.o.Now = func() time.Time { return now }
		c.stateChecks("devmac")
		if got := find(c.results, "clock") != nil; got != tc.warn {
			t.Errorf("%s: warned %v, want %v (%+v)", tc.name, got, tc.warn, c.results)
		}
	}
}

func TestUnknownNode(t *testing.T) {
	o := baseOpts(t)
	o.Node = "nope"
	o.Peers = func() (*config.Peers, error) {
		return &config.Peers{Peer: []config.Peer{{Name: "devmac", Addresses: []string{"h:22"}, Direction: "both"}}}, nil
	}
	rs := Run(context.Background(), o)
	if r := find(rs, "peers"); r == nil || r.Level != Fail || !strings.Contains(r.Detail, `"nope"`) {
		t.Fatalf("%+v", rs)
	}
}

func TestDockerProbeWithoutDaemon(t *testing.T) {
	o := baseOpts(t)
	o.Exec = func(context.Context, string, ...string) (string, error) {
		return "Cannot connect to the Docker daemon", errors.New("exit 1")
	}
	o.GOOS = "darwin"
	r := find(Run(context.Background(), o), "runtime.docker")
	if r == nil || r.Level != Warn || !strings.Contains(r.Fix, "colima") {
		t.Fatalf("%+v", r)
	}
}

func TestOnDemandModeIsOK(t *testing.T) {
	o := baseOpts(t)
	f := &fakeSvc{st: service.Status{Manager: "launchd", Mode: "launchd-agent", NoGUI: true}}
	o.Service = func() (service.Manager, error) { return f, nil }
	r := find(Run(context.Background(), o), "service")
	if r.Level != OK || !strings.Contains(r.Detail, "on-demand") || !strings.Contains(r.Detail, "kran service install --system") {
		t.Fatalf("%+v", r)
	}
	// Installed agent that cannot load without a GUI: same mode.
	f.st.Installed = true
	if r := find(Run(context.Background(), o), "service"); r.Level != OK {
		t.Fatalf("%+v", r)
	}
	// With a GUI session the same state is a real failure.
	f.st.NoGUI = false
	if r := find(Run(context.Background(), o), "service"); r.Level != Fail {
		t.Fatalf("%+v", r)
	}
}

func TestSystemDaemonModeReported(t *testing.T) {
	o := baseOpts(t)
	f := &fakeSvc{st: service.Status{Manager: "launchd", Mode: "launchd-system", Installed: true, Running: true, PID: 7}}
	o.Service = func() (service.Manager, error) { return f, nil }
	o.Dial = func(context.Context) (client.Client, error) {
		return &fakeClient{ver: proto.VersionInfo{Version: version.Version}, st: &proto.State{}}, nil
	}
	r := find(Run(context.Background(), o), "service")
	if r.Level != OK || !strings.Contains(r.Detail, "launchd system daemon") {
		t.Fatalf("%+v", r)
	}
}

func TestCacheResult(t *testing.T) {
	ok := cacheResult("devmac", &proto.CacheStatus{Used: 12 << 30, Budget: 20 << 30, DiskFree: 40 << 30, DiskTotal: 100 << 30, Reserve: 25 << 30, Reclaimed: 3 << 30})
	if ok.Level != OK || !strings.Contains(ok.Detail, "12.0 GiB of 20.0 GiB budget") || !strings.Contains(ok.Detail, "reclaimed 3.0 GiB") || !strings.Contains(ok.Detail, "keeps 25.0 GiB free") {
		t.Errorf("%+v", ok)
	}
	low := cacheResult("devmac", &proto.CacheStatus{Used: 12 << 30, Budget: 20 << 30, DiskFree: 5 << 30, DiskTotal: 100 << 30})
	if low.Level != Warn || low.Fix == "" {
		t.Errorf("%+v", low)
	}
}
