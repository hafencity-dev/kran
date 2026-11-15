package execenv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProxyCreateRefusesOtherSpellingsInvariant (review delta 4, B1): a
// create Docker could read differently than kran is refused, whatever the
// job's score: other spellings of HostConfig, OomScoreAdj, OomKillDisable
// under Docker's case folding (ASCII, the long s, the Kelvin sign), a name
// given twice (either order), anything but exactly one JSON object.
// Without a score (rootless Docker) kran passes such a body on as it is.
func TestProxyCreateRefusesOtherSpellingsInvariant(t *testing.T) {
	refused := map[string]string{
		"lower-case score key":        `{"Image":"x","HostConfig":{"oomscoreadj":-1000}}`,
		"lower-case HostConfig":       `{"Image":"x","hostconfig":{"OomScoreAdj":-1000}}`,
		"long s":                      `{"Image":"x","HostConfig":{"OomſcoreAdj":-1000}}`,
		"both spellings":              `{"Image":"x","HostConfig":{"OomScoreAdj":1000,"oomscoreadj":-1000}}`,
		"lower-case OomKillDisable":   `{"Image":"x","HostConfig":{"oomkilldisable":true}}`,
		"Kelvin sign":                 `{"Image":"x","HostConfig":{"OomKillDisable":false,"Oom` + "K" + `illDisable":true}}`,
		"score twice, -1000 last":     `{"Image":"x","HostConfig":{"OomScoreAdj":1000,"OomScoreAdj":-1000}}`,
		"score twice, -1000 first":    `{"Image":"x","HostConfig":{"OomScoreAdj":-1000,"OomScoreAdj":1000}}`,
		"HostConfig twice, bad first": `{"Image":"x","HostConfig":{"OomKillDisable":true},"HostConfig":{}}`,
		"HostConfig twice, bad last":  `{"Image":"x","HostConfig":{},"HostConfig":{"OomScoreAdj":-1000}}`,
		"trailing brace":              `{"Image":"x","HostConfig":{"OomScoreAdj":-1000}}}`,
		"trailing bracket":            `{"Image":"x","HostConfig":{"OomScoreAdj":-1000}}]`,
		"second object":               `{"Image":"x"}{"HostConfig":{"OomScoreAdj":-1000}}`,
		"null body":                   `null`,
		"array body":                  `[{"Image":"x"}]`,
		"string body":                 `"x"`,
		"empty body":                  ``,
		"HostConfig a string":         `{"Image":"x","HostConfig":"x"}`,
		"HostConfig an array":         `{"Image":"x","HostConfig":[]}`,
	}
	for name, body := range refused {
		for _, score := range []int{ctrOOMScoreDefault, ctrOOMScoreExplicit} {
			if out, _, err := rewriteCreateContainer(nil, score, []byte(body)); err == nil {
				t.Errorf("score %d, %s: passed as %s", score, name, out)
			}
		}
		if out, changed, _ := rewriteCreateContainer(nil, 0, []byte(body)); changed || (len(out) > 0 && string(out) != body) {
			t.Errorf("no score, %s: changed to %s", name, out)
		}
	}
	// Docker's own spelling written with escapes is Docker's spelling.
	out, _, err := rewriteCreateContainer(nil, 500, []byte(`{"Image":"x","HostConfig":{"OomScoreAdj":-1000}}`))
	if err != nil || !strings.Contains(string(out), `"OomScoreAdj":500`) || strings.Contains(string(out), "-1000") {
		t.Errorf("escaped canonical key: %s %v", out, err)
	}
	// A null HostConfig is none: the job's score.
	if out, _, err := rewriteCreateContainer(nil, 500, []byte(`{"Image":"x","HostConfig":null}`)); err != nil || string(out) != `{"HostConfig":{"OomScoreAdj":500},"Image":"x"}` {
		t.Errorf("null HostConfig: %s %v", out, err)
	}
}

// TestProxyCreateReencodesLossless: with a score kran sends its own
// encoding of the body; what it does not rewrite arrives with the same
// value: int64 limits digit for digit, null as null, absent as absent.
func TestProxyCreateReencodesLossless(t *testing.T) {
	in := `{"Image":"postgres:17","Env":null,"Labels":{"a":"<b>&c"},"HostConfig":{"Memory":9007199254740993,` +
		`"NanoCpus":9223372036854775807,"PidsLimit":null,"MemorySwap":-1,"Binds":["/data:/d"],"OomScoreAdj":-5},"X-Unknown":[1,{"y":2}]}`
	out, changed, err := rewriteCreateContainer(nil, 1000, []byte(in))
	if err != nil || !changed {
		t.Fatal(out, changed, err)
	}
	dec := func(b []byte) map[string]any {
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var m map[string]any
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := dec([]byte(in)), dec(out)
	a["HostConfig"].(map[string]any)["OomScoreAdj"] = json.Number("1000")
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("re-encoded body differs:\n in  %v\n out %v", a, b)
	}
	for _, digits := range []string{"9007199254740993", "9223372036854775807", `"PidsLimit":null`, `"Env":null`} {
		if !strings.Contains(string(out), digits) {
			t.Errorf("%s lost: %s", digits, out)
		}
	}
	if strings.Contains(string(out), "Cmd") {
		t.Errorf("an absent field appeared: %s", out)
	}
	// Nothing to raise: still kran's own encoding, never the job's bytes.
	keep := `{ "Image" : "x", "HostConfig" : { "OomScoreAdj" : 700 } }`
	if out, changed, err := rewriteCreateContainer(nil, 500, []byte(keep)); err != nil || !changed || string(out) != `{"HostConfig":{"OomScoreAdj":700},"Image":"x"}` {
		t.Errorf("unchanged score: sent %s (changed %v, %v)", out, changed, err)
	}
}

// proxyConn starts kran's proxy (score 500) in front of a fake Docker and
// returns its socket and what Docker saw.
func proxyConn(t *testing.T) (string, chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "kph")
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
	go serveDockerProxy(ln, upSock, testMaps, ctrOOMScoreExplicit)
	t.Cleanup(func() { ln.Close() })
	return proxySock, seen
}

// raw sends raw bytes on one connection and returns everything the proxy
// answers until it closes.
func raw(t *testing.T, sock, req string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, req)
	// Done sending: the proxy passes the end on, so Docker (and the
	// proxy) close the connection whether or not they read on.
	c.(*net.UnixConn).CloseWrite()
	b, _ := io.ReadAll(c)
	return string(b)
}

func post(path, body string) string {
	return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", path, len(body), body)
}

// TestProxyRefusedCreateAndForms: through the running proxy, a create it
// cannot read gets a 400 naming kran and never reaches Docker, the proxy
// survives a null body, a body over the bound is refused, a chunked create
// and every API path form are rewritten.
func TestProxyRefusedCreateAndForms(t *testing.T) {
	sock, seen := proxyConn(t)
	// next proves what reached Docker before it: a plain request after.
	next := func() string {
		raw(t, sock, "GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
		return <-seen
	}
	got := raw(t, sock, post("/v1.47/containers/create?name=pg", `{"Image":"x","HostConfig":{"oomscoreadj":-1000}}`))
	want := `{"message":"kran: the Docker proxy refused this container create (\"oomscoreadj\" is not Docker's spelling of OomScoreAdj; write OomScoreAdj); kran gives the containers a job creates the job's place in the kernel's kill order and passes on only a create it can read"}`
	if !strings.HasPrefix(got, "HTTP/1.1 400 Bad Request\r\n") || !strings.HasSuffix(got, want) {
		t.Errorf("refused create answered:\n%s\nwant 400 with\n%s", got, want)
	}
	if s := next(); !strings.HasPrefix(s, "GET /_ping") {
		t.Errorf("Docker saw the refused create: %s", s)
	}
	if got := raw(t, sock, post("/containers/create", `null`)); !strings.HasPrefix(got, "HTTP/1.1 400 ") {
		t.Errorf("null body: %q", got)
	}
	if s := next(); !strings.HasPrefix(s, "GET /_ping") {
		t.Errorf("after a null body: %s", s)
	}
	big := `{"Image":"x","Labels":{"pad":"` + strings.Repeat("a", maxRewriteBody) + `"}}`
	if got := raw(t, sock, post("/containers/create", big)); !strings.HasPrefix(got, "HTTP/1.1 400 ") || !strings.Contains(got, "too large") {
		t.Errorf("over the bound: %.200q", got)
	}
	if s := next(); !strings.HasPrefix(s, "GET /_ping") {
		t.Errorf("Docker saw the oversized create: %.100s", s)
	}
	body := `{"Image":"x","HostConfig":{"OomScoreAdj":-1000}}`
	chunked := fmt.Sprintf("POST /v1.53/containers/create HTTP/1.1\r\nHost: docker\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	raw(t, sock, chunked)
	if s := <-seen; s != `POST /v1.53/containers/create {"HostConfig":{"OomScoreAdj":500},"Image":"x"}` {
		t.Errorf("chunked create reached Docker as %s", s)
	}
	for _, p := range []string{"/containers/create", "/containers/create?name=pg", "/v1.41/containers/create", "/v1.53/containers/create?name=pg&platform=linux"} {
		raw(t, sock, post(p, `{"Image":"x"}`))
		if s := <-seen; !strings.HasSuffix(s, ` {"HostConfig":{"OomScoreAdj":500},"Image":"x"}`) {
			t.Errorf("%s reached Docker as %s", p, s)
		}
	}
}

// TestProxySecondRequestAfterUpgradeHeader (review delta 4, B2): a create
// pipelined on a connection whose first request carries an Upgrade header
// Docker does not honour must never reach Docker unread.
func TestProxySecondRequestAfterUpgradeHeader(t *testing.T) {
	sock, seen := proxyConn(t)
	first := "GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: x\r\n\r\n"
	raw(t, sock, first+post("/v1.47/containers/create", `{"Image":"x","HostConfig":{"OomScoreAdj":-1000}}`))
	if s := <-seen; !strings.HasPrefix(s, "GET /_ping") {
		t.Fatalf("first: %s", s)
	}
	raw(t, sock, "GET /version HTTP/1.1\r\nHost: docker\r\n\r\n")
	if s := <-seen; !strings.HasPrefix(s, "GET /version") {
		t.Errorf("the pipelined create reached Docker unread: %s", s)
	}
}

// TestProxyRealDockerFlows (KRAN_TEST_DOCKER=1): kran's proxy (score 1000)
// in front of this machine's real Docker; the docker CLI through it: run
// with stdin attached, an interactive exec, logs follow, and a BuildKit
// build (whose RUN step reports the score BuildKit gives it).
func TestProxyRealDockerFlows(t *testing.T) {
	dockerTests(t)
	const image = "alpine:3.24.3"
	cli := dockerCLIForTests()
	if exec.Command(cli, "image", "inspect", image).Run() != nil {
		t.Skipf("image %s not present (no pull in tests)", image)
	}
	dir, err := os.MkdirTemp("", "kdf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go serveDockerProxy(ln, "/var/run/docker.sock", nil, ctrOOMScoreDefault)
	t.Cleanup(func() { ln.Close() })
	docker := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(cli, append([]string{"--host", "unix://" + sock}, args...)...)
		cmd.Env = append(os.Environ(), "DOCKER_CONTEXT=")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if out := docker("through stdin\n", "run", "-i", "--rm", "--pull", "never", "--label", "kran.ns=kran-test", image, "cat"); out != "through stdin\n" {
		t.Errorf("run -i: %q", out)
	}
	name := fmt.Sprintf("kran-test-flows-%d", time.Now().UnixNano())
	t.Cleanup(func() { exec.Command(cli, "rm", "--force", name).Run() })
	docker("", "run", "-d", "--name", name, "--pull", "never", "--label", "kran.ns=kran-test", image, "sh", "-c", "echo one; echo two; sleep 30")
	if out := docker("exec stdin\n", "exec", "-i", name, "cat"); out != "exec stdin\n" {
		t.Errorf("exec -i: %q", out)
	}
	if out := docker("", "exec", name, "cat", "/proc/self/oom_score_adj"); strings.TrimSpace(out) != "1000" {
		t.Errorf("score inside: %q", out)
	}
	docker("", "kill", name)
	if out := docker("", "logs", "--follow", name); out != "one\ntwo\n" {
		t.Errorf("logs --follow: %q", out)
	}
	ctx := filepath.Join(dir, "ctx")
	os.MkdirAll(ctx, 0o755)
	os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM "+image+"\nRUN echo build-step-score=$(cat /proc/self/oom_score_adj)\n"), 0o644)
	iid := filepath.Join(dir, "iid")
	out := docker("", "build", "--no-cache", "--progress", "plain", "--iidfile", iid, ctx)
	if id, _ := os.ReadFile(iid); len(id) > 0 {
		t.Cleanup(func() { exec.Command(cli, "rmi", "--force", strings.TrimSpace(string(id))).Run() })
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	step := ""
	for sc.Scan() {
		if i := strings.Index(sc.Text(), "build-step-score="); i >= 0 && !strings.Contains(sc.Text(), "RUN") {
			step = sc.Text()[i:]
		}
	}
	if step == "" {
		t.Errorf("build through the proxy did not run its step:\n%s", out)
	}
	t.Logf("BuildKit build through the proxy: %s (BuildKit's own score; the proxy cannot set it)", step)
}
