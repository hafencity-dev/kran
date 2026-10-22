package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

func TestBusDropsSlowConsumer(t *testing.T) {
	b := newBus(time.Now)
	b.bufSize = 4
	slow, _ := b.subscribe()
	fast, _ := b.subscribe()
	done := make(chan int)
	go func() {
		n := 0
		for range fast.ch {
			n++
		}
		done <- n
	}()
	for i := 0; i < 10; i++ {
		b.publish(proto.EventJob, i) // never blocks
	}
	n := 0
	for range slow.ch {
		n++
	}
	if !slow.dropped || n != 4 {
		t.Fatalf("slow: dropped=%v got %d", slow.dropped, n)
	}
	b.unsubscribe(fast)
	if got := <-done; got < 4 {
		t.Fatalf("fast got %d", got)
	}
}

func TestBusThrottlesMetrics(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newBus(func() time.Time { return now })
	s, _ := b.subscribe()
	b.publishMetrics("a", proto.NodeMetrics{})
	b.publishMetrics("a", proto.NodeMetrics{}) // dropped: same second
	b.publishMetrics("b", proto.NodeMetrics{})
	now = now.Add(1100 * time.Millisecond)
	b.publishMetrics("a", proto.NodeMetrics{})
	b.unsubscribe(s)
	var names []string
	var seqs []uint64
	for ev := range s.ch {
		names = append(names, ev.Data.(proto.NodeMetricsEvent).Name)
		seqs = append(seqs, ev.Seq)
	}
	if strings.Join(names, ",") != "a,b,a" {
		t.Fatalf("events %v", names)
	}
	if seqs[0] != 1 || seqs[2] != 3 {
		t.Fatalf("sequence numbers must stay contiguous: %v", seqs)
	}
}

func TestTCPAuth(t *testing.T) {
	d := &Daemon{token: strings.Repeat("ab", 32)}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	cases := []struct {
		name     string
		loopback bool
		method   string
		url      string
		host     string
		token    string
		want     int
	}{
		// Loopback TCP is open to every local account, not just the owner:
		// job output and argv must not be readable without the token.
		{"loopback read needs token", true, "GET", "/api/v1/jobs/x/log", "127.0.0.1:7270", "", 401},
		{"loopback read", true, "GET", "/api/v1/state", "127.0.0.1:7270", "hdr", 200},
		{"loopback sse query token", true, "GET", "/api/v1/events?token=" + d.token, "[::1]:7270", "", 200},
		{"loopback ui", true, "GET", "/", "localhost:7270", "", 200},
		{"loopback post needs token", true, "POST", "/api/v1/jobs/x/cancel", "127.0.0.1:7270", "", 401},
		{"loopback post with token", true, "POST", "/api/v1/jobs/x/cancel", "127.0.0.1:7270", "hdr", 200},
		{"loopback post query token refused", true, "POST", "/api/v1/jobs/x/cancel?token=" + d.token, "127.0.0.1:7270", "", 401},
		{"dns rebinding", true, "GET", "/api/v1/state", "evil.example:7270", "", 403},
		{"remote api needs token", false, "GET", "/api/v1/state", "100.1.2.3:7270", "", 401},
		{"remote api header", false, "GET", "/api/v1/state", "100.1.2.3:7270", "hdr", 200},
		{"remote sse query token", false, "GET", "/api/v1/events?token=" + d.token, "100.1.2.3:7270", "", 200},
		{"remote wrong token", false, "GET", "/api/v1/events?token=nope", "100.1.2.3:7270", "", 401},
		{"remote assets open", false, "GET", "/index.js", "100.1.2.3:7270", "", 200},
		{"remote post needs header", false, "POST", "/api/v1/nodes/a/drain", "100.1.2.3:7270", "", 401},
		{"remote foreign host", false, "GET", "/", "evil.example:7270", "", 403},
		{"remote foreign host with token", false, "GET", "/api/v1/state", "evil.example:7270", "hdr", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.url, nil)
			r.Host = c.host
			if c.token == "hdr" {
				r.Header.Set("X-Kran-Token", d.token)
			}
			w := httptest.NewRecorder()
			d.tcpAuth(ok, c.loopback, "100.1.2.3").ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
			// A cross-origin <script src> must not be able to read API output.
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
				t.Fatalf("missing nosniff/CORP headers: %v", w.Header())
			}
		})
	}
}

func TestSpoolOffsetsAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	sp, err := openSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	sp.append(proto.FrameDecision, []byte(`{}`))
	sp.append(proto.FrameStdout, []byte("hello "))
	sp.append(proto.FrameStderr, []byte("world"))
	if sp.outputBytes() != 11 {
		t.Fatalf("out %d", sp.outputBytes())
	}
	read := func(off int64, reattach bool) (string, []proto.FrameType) {
		rd, err := sp.reader(off, reattach)
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		var out strings.Builder
		var types []proto.FrameType
		for {
			ft, p, err := rd.NextNoWait()
			if err != nil {
				break
			}
			types = append(types, ft)
			if isOutput(ft) {
				out.Write(p)
			}
		}
		return out.String(), types
	}
	if out, types := read(0, false); out != "hello world" || types[0] != proto.FrameDecision {
		t.Fatalf("%q %v", out, types)
	}
	if out, types := read(3, true); out != "lo world" || types[0] != proto.FrameStdout {
		t.Fatalf("reattach: %q %v", out, types)
	}
	// Live follow: a reader blocks until new frames arrive.
	rd, _ := sp.reader(11, true)
	got := make(chan string, 1)
	go func() {
		_, p, err := rd.Next(context.Background())
		if err != nil {
			got <- err.Error()
			return
		}
		got <- string(p)
	}()
	time.Sleep(50 * time.Millisecond)
	sp.append(proto.FrameStdout, []byte("!"))
	if s := <-got; s != "!" {
		t.Fatalf("follow got %q", s)
	}
	rd.Close()
	sp.close()

	// A torn frame at the end (crash mid-write) is dropped on reopen.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{byte(proto.FrameStdout), 0, 0, 0, 9, 'x'})
	f.Close()
	sp, err = openSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	if sp.outputBytes() != 12 {
		t.Fatalf("after recovery out = %d", sp.outputBytes())
	}
	sp.append(proto.FrameExit, []byte(`{"code":0}`))
	if out, types := read(0, false); out != "hello world!" || types[len(types)-1] != proto.FrameExit {
		t.Fatalf("%q %v", out, types)
	}
	if err := sp.append(proto.FrameStdout, []byte("late")); err == nil {
		t.Fatal("append after exit accepted")
	}
	sp.close()
}

func TestIDsSortableAndUnique(t *testing.T) {
	var g idGen
	now := time.Now()
	prev := ""
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := g.next(now)
		if len(id) != 10 || !validID(id) || seen[id] || id <= prev {
			t.Fatalf("bad id %q after %q", id, prev)
		}
		seen[id] = true
		prev = id
	}
	later := g.next(now.Add(time.Hour))
	if later <= prev {
		t.Fatal("ids not time-sortable")
	}
	for _, bad := range []string{"", "../x", "a/b", strings.Repeat("a", 65)} {
		if validID(bad) {
			t.Fatalf("validID(%q)", bad)
		}
	}
}

func TestRotator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	r, err := newRotator(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n"
	for i := 0; i < 20; i++ {
		r.Write([]byte(line))
	}
	r.Close()
	for _, p := range []string{path, path + ".1", path + ".2"} {
		st, err := os.Stat(p)
		if err != nil || st.Size() > 100 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", p, st.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("kept more than 3 files")
	}
}

func TestConfigAdapters(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`name = "gx10"
[accept]
reserve_cpus = 4
reserve_mem = "16GiB"
nice = 5
[ui]
listen = ""
`), 0o600)
	os.WriteFile(filepath.Join(dir, "node_id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o600)
	c, err := loadNodeConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "gx10" || c.Accept.ReserveCPUs != 4 || c.Accept.ReserveMem != 16<<30 || c.Accept.Nice != 5 || c.UIListen != "" || !c.Accept.Enabled || c.ID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("%+v", c)
	}
	peers := filepath.Join(dir, "peers.toml")
	src := filePeerSource{path: peers}
	if ps, err := src.Load(); err != nil || len(ps) != 0 {
		t.Fatalf("missing file: %v %v", ps, err)
	}
	stamp := src.Stamp()
	os.WriteFile(peers, []byte(`[[peer]]
name = "devmac"
id = "x"
addresses = ["100.64.0.2:22", "192.168.1.20:22"]
user = "acme"
host_key = "ssh-ed25519 AAAA"
direction = "both"
`), 0o600)
	if src.Stamp() == stamp {
		t.Fatal("stamp did not change")
	}
	ps, err := src.Load()
	if err != nil || len(ps) != 1 || ps[0].Name != "devmac" || len(ps[0].Addresses) != 2 {
		t.Fatalf("%+v %v", ps, err)
	}
	if err := src.savePreferred("devmac", "192.168.1.20:22"); err != nil {
		t.Fatal(err)
	}
	ps, _ = src.Load()
	if ps[0].PreferredAddress != "192.168.1.20:22" || !ps[0].sameConnection(PeerConfig{Name: "devmac", ID: "x", Addresses: ps[0].Addresses, User: "acme", HostKey: "ssh-ed25519 AAAA", Direction: "both"}) {
		t.Fatalf("%+v", ps[0])
	}
	if got := orderAddresses(ps[0].Addresses, ps[0].PreferredAddress); got[0] != "192.168.1.20:22" || len(got) != 2 {
		t.Fatalf("order %v", got)
	}
}

func TestResolveUIListen(t *testing.T) {
	cases := []struct {
		in       string
		addr     string
		loopback bool
	}{
		{"127.0.0.1:7270", "127.0.0.1:7270", true},
		{"localhost:9000", "localhost:9000", true},
		{"[::1]:7270", "[::1]:7270", true},
		{"0.0.0.0:7270", "0.0.0.0:7270", false},
		{"", "", false},
	}
	for _, c := range cases {
		addr, lb, err := resolveUIListen(c.in)
		if err != nil || addr != c.addr || lb != c.loopback {
			t.Errorf("%q → %q %v %v", c.in, addr, lb, err)
		}
	}
	if _, _, err := resolveUIListen("nonsense"); err == nil {
		t.Error("nonsense accepted")
	}
}

func TestAPIEventsStateFirstAndUIListener(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.cfg.UIListen = "127.0.0.1:0"
	d := a.start()
	ctx, cancel := testContext(10 * time.Second)
	defer cancel()
	cl, err := client.DialSocket(ctx, a.sock())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	evs, err := cl.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := <-evs
	st, ok := first.Data.(proto.State)
	if first.Type != proto.EventState || !ok || st.Self != "a" || len(st.Nodes) != 1 || !st.Nodes[0].Self {
		t.Fatalf("first event %+v", first)
	}
	ws := workspaceDir(t, map[string]string{"f": "x"})
	spec := shSpec(ws, "true")
	spec.Env = map[string]string{"SECRETISH": "v"}
	go runCLI(ctx, a.sock(), spec, cliOpts{})
	for ev := range evs {
		if ev.Type == proto.EventJob {
			j := ev.Data.(proto.Job)
			if j.Spec.Env != nil {
				t.Fatal("job env shipped to the API")
			}
			if j.State == proto.JobSucceeded {
				if j.PlacedAt.IsZero() || j.RunningAt.IsZero() {
					t.Fatalf("timestamps %+v", j)
				}
				break
			}
		}
	}

	// Token file written with 0600.
	stTok, err := os.Stat(filepath.Join(a.dirs.State, "ui.token"))
	if err != nil || stTok.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v %v", stTok, err)
	}
	// UI listener: reads need the token (header or query), run sessions are
	// refused, mutations need the token header.
	base := "http://" + d.UIAddr()
	resp, err := http.Get(base + "/api/v1/state")
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("GET state over tcp without token: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get(base + "/api/v1/state?token=" + d.Token())
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET state over tcp: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, _ = http.Post(base+"/api/v1/nodes/a/drain", "application/json", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("drain without token: %d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ := http.NewRequest("POST", base+"/api/v1/run", nil)
	req.Header.Set("X-Kran-Token", d.Token())
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("run over tcp: %d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("POST", base+"/api/v1/nodes/a/drain", nil)
	req.Header.Set("X-Kran-Token", d.Token())
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("drain with token: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	waitFor(t, 2*time.Second, "self draining", func() bool {
		ns, _ := a.scheduler().node("a")
		return ns.Status == proto.NodeDraining
	})
	resp, err = http.Get(base + "/api/v1/nodes/a/metrics?token=" + d.Token())
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("metrics history: %v %v", resp, err)
	}
	var hist []proto.NodeMetrics
	json.NewDecoder(resp.Body).Decode(&hist)
	resp.Body.Close()
	if len(hist) == 0 {
		t.Fatal("no metrics history")
	}
	if err := cl.Resume(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Drain(ctx, "nope"); !client.IsNotFound(err) {
		t.Fatalf("drain unknown node: %v", err)
	}
}

func TestUTF8Complete(t *testing.T) {
	euro := []byte("€") // 3 bytes
	cases := []struct {
		in   []byte
		want int
	}{
		{[]byte("abc"), 3},
		{append([]byte("a"), euro...), 4},
		{append([]byte("a"), euro[:1]...), 1},
		{append([]byte("a"), euro[:2]...), 1},
		{[]byte{0xF0, 0x9F, 0x98}, 0},
		{[]byte{0xF0, 0x9F, 0x98, 0x80}, 4},
		{[]byte{0xff}, 1}, // invalid byte: pass through
	}
	for _, c := range cases {
		if got := utf8Complete(c.in); got != c.want {
			t.Errorf("utf8Complete(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestKeepaliveFromEnv(t *testing.T) {
	ka, err := keepaliveFromEnv("1s,8s,20s")
	if err != nil || ka == nil || ka.Interval != time.Second || ka.DeadAfter != 8*time.Second || ka.MaxDeadAfter != 20*time.Second {
		t.Fatalf("%+v %v", ka, err)
	}
	if ka, err := keepaliveFromEnv(""); ka != nil || err != nil {
		t.Fatalf("empty: %+v %v", ka, err)
	}
	for _, bad := range []string{"1s", "1s,x,2s", "1s,8s,4s", "0s,1s,2s"} {
		if _, err := keepaliveFromEnv(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
