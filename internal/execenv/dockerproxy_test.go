package execenv

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testMaps = parseDockerMaps(encodeDockerMaps([]dockerMap{
	{Ctr: "/home/alice", Host: "/var/lib/docker/volumes/kran-cache/_data/home"},
	{Ctr: "/home/alice/src/app", Host: "/var/lib/docker/volumes/kran-slots/_data/r1/0"},
	{Ctr: "/", Host: "/evil"},            // never: everything would map
	{Ctr: "relative", Host: "/x"},        // not absolute
	{Ctr: "/kran/cache", Host: "x/../y"}, // host not clean
}))

func TestDockerMapPaths(t *testing.T) {
	if len(testMaps) != 2 || testMaps[0].Ctr != "/home/alice/src/app" {
		t.Fatalf("maps: %+v", testMaps)
	}
	for _, tc := range []struct{ in, want string }{
		{"/home/alice/src/app", "/var/lib/docker/volumes/kran-slots/_data/r1/0"},
		{"/home/alice/src/app/", "/var/lib/docker/volumes/kran-slots/_data/r1/0"},
		{"/home/alice/src/app/sub/../conf/x.yml", "/var/lib/docker/volumes/kran-slots/_data/r1/0/conf/x.yml"},
		{"/home/alice/.aws", "/var/lib/docker/volumes/kran-cache/_data/home/.aws"},
		{"/home/alice/src/application", "/var/lib/docker/volumes/kran-cache/_data/home/src/application"},
		{"/home/alicex", "/home/alicex"},
		{"/etc/hosts", "/etc/hosts"},
		{"named-volume", "named-volume"},
		{"/home/alice/src/app/../../../../etc", "/home/alice/src/app/../../../../etc"}, // cleans to /etc: not ours
	} {
		got, _ := mapHostPath(testMaps, tc.in)
		if got != tc.want {
			t.Errorf("map %q = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"/home/alice/src/app:/src", "/var/lib/docker/volumes/kran-slots/_data/r1/0:/src"},
		{"/home/alice/src/app/data:/data:ro,z", "/var/lib/docker/volumes/kran-slots/_data/r1/0/data:/data:ro,z"},
		{"cachevol:/root/.cache", "cachevol:/root/.cache"},
		{"/anon", "/anon"},
		{"/var/run/docker.sock:/var/run/docker.sock", "/var/run/docker.sock:/var/run/docker.sock"},
	} {
		if got := rewriteBind(testMaps, tc.in); got != tc.want {
			t.Errorf("bind %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDockerRewriteCreate(t *testing.T) {
	body := `{"Image":"alpine","Cmd":["cat","/w/f"],"Labels":{"a":"<&>"},"HostConfig":{"Memory":9007199254740993,
		"Binds":["/home/alice/src/app:/w","vol:/v"],
		"Mounts":[{"Type":"bind","Source":"/home/alice/src/app/conf","Target":"/c","ReadOnly":true},{"Type":"volume","Source":"/home/alice/src/app","Target":"/x"}]},
		"Unknown":{"keep":[1,2]}}`
	out, changed, err := rewriteCreateContainer(testMaps, []byte(body))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var got struct {
		Image      string
		Labels     map[string]string
		Unknown    map[string][]int
		HostConfig struct {
			Memory json.Number
			Binds  []string
			Mounts []struct {
				Type, Source, Target string
				ReadOnly             bool
			}
		}
	}
	d := json.NewDecoder(strings.NewReader(string(out)))
	d.UseNumber()
	if err := d.Decode(&got); err != nil {
		t.Fatal(err)
	}
	hc := got.HostConfig
	if got.Image != "alpine" || got.Labels["a"] != "<&>" || len(got.Unknown["keep"]) != 2 || hc.Memory.String() != "9007199254740993" {
		t.Errorf("other fields changed: %s", out)
	}
	if hc.Binds[0] != "/var/lib/docker/volumes/kran-slots/_data/r1/0:/w" || hc.Binds[1] != "vol:/v" {
		t.Errorf("binds: %v", hc.Binds)
	}
	if hc.Mounts[0].Source != "/var/lib/docker/volumes/kran-slots/_data/r1/0/conf" || !hc.Mounts[0].ReadOnly || hc.Mounts[1].Source != "/home/alice/src/app" {
		t.Errorf("mounts: %+v", hc.Mounts)
	}
	// Nothing to map: the body goes through byte for byte.
	plain := `{"Image":"alpine","HostConfig":{"Binds":["/etc:/e"]}}`
	if out, changed, _ := rewriteCreateContainer(testMaps, []byte(plain)); changed || string(out) != plain {
		t.Errorf("unrelated body rewritten: %s", out)
	}
	vol := `{"Name":"data","Driver":"local","DriverOpts":{"type":"none","o":"bind","device":"/home/alice/src/app/data"}}`
	out, changed, err = rewriteCreateVolume(testMaps, []byte(vol))
	if err != nil || !changed || !strings.Contains(string(out), `"device":"/var/lib/docker/volumes/kran-slots/_data/r1/0/data"`) {
		t.Errorf("volume: %s %v %v", out, changed, err)
	}
	nfs := `{"Name":"n","DriverOpts":{"type":"nfs","o":"addr=1.2.3.4","device":"/home/alice/src/app"}}`
	if _, changed, _ := rewriteCreateVolume(testMaps, []byte(nfs)); changed {
		t.Error("non-bind volume rewritten")
	}
}

// fakeDockerUpstream is an upstream that records requests: /containers/create
// echoes the body, /attach upgrades and echoes raw bytes, /logs streams.
func fakeDockerUpstream(t *testing.T, sock string) chan string {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 16)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- r.Method + " " + r.URL.Path + " " + string(body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/attach"):
			hj, _ := w.(http.Hijacker)
			c, brw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			brw.Flush()
			line, _ := brw.ReadString('\n')
			c.Write([]byte("echo:" + line))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			for i := 0; i < 3; i++ {
				io.WriteString(w, "tick\n")
				w.(http.Flusher).Flush()
				time.Sleep(20 * time.Millisecond)
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return seen
}

func TestDockerProxyServes(t *testing.T) {
	dir, err := os.MkdirTemp("", "kdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	upSock, proxySock := filepath.Join(dir, "up.sock"), filepath.Join(dir, "p.sock")
	seen := fakeDockerUpstream(t, upSock)
	ln, err := net.Listen("unix", proxySock)
	if err != nil {
		t.Fatal(err)
	}
	go serveDockerProxy(ln, upSock, testMaps)
	t.Cleanup(func() { ln.Close() })

	// A client like the docker CLI: keep-alive, several requests.
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", proxySock)
	}}}
	for i := 0; i < 3; i++ {
		resp, err := cl.Post("http://docker/v1.47/containers/create?name=x", "application/json",
			strings.NewReader(`{"Image":"alpine","HostConfig":{"Binds":["/home/alice/src/app:/w"]}}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), `/var/lib/docker/volumes/kran-slots/_data/r1/0:/w`) {
			t.Fatalf("request %d not rewritten: %s", i, b)
		}
		if got := <-seen; !strings.HasPrefix(got, "POST /v1.47/containers/create ") {
			t.Fatalf("upstream saw %q", got)
		}
	}
	resp, err := cl.Get("http://docker/v1.47/containers/x/logs?follow=1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "tick\ntick\ntick\n" {
		t.Errorf("streamed body %q", b)
	}
	<-seen

	// An upgraded connection (docker attach, exec) carries raw bytes both ways.
	c, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "POST /v1.47/containers/x/attach?stream=1&stdin=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status %q", status)
	}
	for {
		l, _ := br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	io.WriteString(c, "hello\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := br.ReadString('\n')
	if got != "echo:hello\n" {
		t.Errorf("hijacked echo %q", got)
	}
}
