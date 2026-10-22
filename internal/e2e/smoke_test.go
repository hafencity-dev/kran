//go:build smoke

package e2e

// Real-machine smoke test (make smoke NODE=devmac). This machine and the
// node each get a development kran install in its own dirs
// (KRAN_*_DIR, namespace kran-dev, ~/kran-dev/t1 on the node), linked with
// the kran-dev tag. It runs a Go test of this repo on the node (container
// runtime when the node is a Mac), checks status and the UI, unlinks and
// verifies that both authorized_keys files are byte-identical to before
// and nothing of the dev install is left. A real kran install on either
// machine is never touched: other dirs, other namespace, other ports.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

type smoke struct {
	t       *testing.T
	node    string // ssh host
	base    string // local dev install
	rhome   string
	rdir    string // remote dev install
	env     []string
	daemon  *exec.Cmd
	timings [][2]string
}

func (s *smoke) ssh(cmd string) (string, error) {
	out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", s.node, cmd).CombinedOutput()
	return string(out), err
}

func (s *smoke) mustSSH(cmd string) string {
	s.t.Helper()
	out, err := s.ssh(cmd)
	if err != nil {
		s.t.Fatalf("ssh %s %q: %v\n%s", s.node, cmd, err, out)
	}
	return out
}

func (s *smoke) timed(name string, fn func()) {
	t0 := time.Now()
	fn()
	s.timings = append(s.timings, [2]string{name, time.Since(t0).Round(100 * time.Millisecond).String()})
	s.t.Logf("%s: %v", name, time.Since(t0).Round(100*time.Millisecond))
}

func (s *smoke) kran(timeout time.Duration, dir string, args ...string) result {
	s.t.Helper()
	cmd := exec.Command(kranBin, args...)
	cmd.Env, cmd.Dir = s.env, dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		err = <-done
	}
	return result{code: exitCode(cmd, err), stdout: out.Bytes(), stderr: errb.Bytes(), dur: time.Since(t0)}
}

const smokeNS = "kran-dev"

func TestSmoke(t *testing.T) {
	node := os.Getenv("KRAN_SMOKE_NODE")
	if node == "" {
		t.Skip("set KRAN_SMOKE_NODE (make smoke NODE=devmac)")
	}
	s := &smoke{t: t, node: node}
	s.rhome = strings.TrimSpace(s.mustSSH("echo $HOME"))
	s.rdir = s.rhome + "/kran-dev/t1"
	rOS := strings.ToLower(strings.TrimSpace(s.mustSSH("uname -s")))
	rArch := map[string]string{"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}[strings.TrimSpace(s.mustSSH("uname -m"))]

	// --- before: what must be identical afterwards
	home, _ := os.UserHomeDir()
	localAK := filepath.Join(home, ".ssh", "authorized_keys")
	localBefore, _ := os.ReadFile(localAK)
	remoteBefore := s.mustSSH("cat ~/.ssh/authorized_keys 2>/dev/null; true")
	realBefore := s.realKran()
	if left := s.devLeftovers(); left != "" {
		t.Fatalf("%s already has kran-dev leftovers; clean them first:\n%s", node, left)
	}

	// --- local dev install
	var err error
	s.base, err = os.MkdirTemp("/tmp", "kran-smoke-")
	must(t, err)
	cfgDir := filepath.Join(s.base, "cfg")
	for _, d := range []string{"cfg", "st", "run", "cache"} {
		must(t, os.MkdirAll(filepath.Join(s.base, d), 0o700))
	}
	s.env = []string{"KRAN_CONFIG_DIR=" + cfgDir, "KRAN_STATE_DIR=" + filepath.Join(s.base, "st"),
		"KRAN_RUNTIME_DIR=" + filepath.Join(s.base, "run"), "KRAN_CACHE_DIR=" + filepath.Join(s.base, "cache"),
		"KRAN_NAMESPACE=" + smokeNS, "KRAN_NO_SERVICE=1", "KRAN_NO_HOOK=1", "KRAN_OWNER=human:smoke"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KRAN_") && !strings.HasPrefix(kv, "GORACE=") {
			s.env = append(s.env, kv)
		}
	}
	must(t, os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(`name = "t1-gx10"
[accept]
enabled = true
reserve_cpus = 2
reserve_mem = "8GiB"
nice = 10
[ui]
listen = "127.0.0.1:0"
`), 0o600))
	t.Cleanup(func() { s.cleanup(localAK, localBefore, remoteBefore, realBefore) })

	// Binaries for the node: kran for its OS and, for a Mac, the Linux kran
	// its container runtime needs; static builds of this checkout.
	s.timed("build binaries", func() {
		for _, p := range [][2]string{{rOS, rArch}, {"linux", rArch}} {
			out := filepath.Join(s.base, "cache", "bin", "dev", p[0]+"-"+p[1], "kran")
			cmd := exec.Command("go", "build", "-trimpath", "-o", out, "github.com/hafencity-dev/kran/cmd/kran")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+p[0], "GOARCH="+p[1])
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build %v: %v\n%s", p, err, b)
			}
		}
	})
	if r := s.kran(time.Minute, s.base, "setup", "--yes", "--quiet", "--name", "t1-gx10", "--no-service", "--no-agents", "--no-link"); r.code != 0 {
		t.Fatalf("local setup: %s", r)
	}
	s.startDaemon()

	// The node's dev install: config before link sets it up (UI on a free
	// port, never the real 7270).
	s.mustSSH(fmt.Sprintf(`mkdir -p %[1]s/cfg && chmod 700 %[1]s %[1]s/cfg && cat > %[1]s/cfg/config.toml <<'EOF'
name = "t1-devmac"
[accept]
enabled = true
reserve_cpus = 2
reserve_mem = "8GiB"
nice = 10
[ui]
listen = "127.0.0.1:0"
[runtime.container]
enabled = "true"
EOF`, s.rdir))

	var linkOut result
	s.timed("kran link", func() {
		args := []string{"link", node, "--name", "t1-devmac", "--tag", "kran-dev", "--remote-bin-dir", s.rdir + "/bin"}
		for _, kv := range []string{"KRAN_CONFIG_DIR=" + s.rdir + "/cfg", "KRAN_STATE_DIR=" + s.rdir + "/st",
			"KRAN_RUNTIME_DIR=" + s.rdir + "/run", "KRAN_CACHE_DIR=" + s.rdir + "/cache",
			"KRAN_NAMESPACE=" + smokeNS, "KRAN_NO_SERVICE=1"} {
			args = append(args, "--remote-env", kv)
		}
		linkOut = s.kran(10*time.Minute, s.base, args...)
	})
	if linkOut.code != 0 {
		t.Fatalf("kran link: %s", linkOut)
	}
	t.Logf("link output:\n%s", linkOut.stdout)
	// The new forced commands are tagged and pinned to the dev install.
	for _, ak := range []string{readFileString(localAK), s.mustSSH("cat ~/.ssh/authorized_keys")} {
		if !strings.Contains(ak, "kran-dev") || !strings.Contains(ak, "env KRAN_CONFIG_DIR=") {
			t.Errorf("dev authorized_keys line missing or not pinned:\n%s", ak)
		}
	}

	s.timed("mesh online (incl. container runtime)", func() { s.waitPeer("t1-devmac", 10*time.Minute) })

	repo := moduleRoot(t)
	var r result
	s.timed("remote go test (first run)", func() {
		r = s.kran(30*time.Minute, repo, "run", "--json-events", "-n", "t1-devmac", "--no-fallback", "--",
			"go", "test", "-count=1", "./internal/proto/", "./internal/config/")
	})
	if r.code != 0 || !strings.Contains(string(r.stdout), "ok  \tgithub.com/hafencity-dev/kran/internal/config") {
		t.Fatalf("remote go test: %s", r)
	}
	dec := event(r.events(), "decision")
	t.Logf("placed: %v", dec)
	if rOS == "darwin" && (dec == nil || dec["runtime"] != "container") {
		t.Errorf("a Linux job on a Mac must use the container runtime: %v", dec)
	}
	s.timed("remote go test (warm)", func() {
		r = s.kran(30*time.Minute, repo, "run", "-n", "t1-devmac", "--no-fallback", "--", "go", "test", "-count=1", "./internal/proto/", "./internal/config/")
	})
	if r.code != 0 {
		t.Fatalf("warm remote go test: %s", r)
	}
	r = s.kran(5*time.Minute, filepath.Join(repo, "internal"), "run", "-n", "t1-devmac", "--no-fallback", "--", "pwd")
	if r.code != 0 || strings.TrimSpace(string(r.stdout)) != filepath.Join(repo, "internal") {
		t.Errorf("absolute path parity: %s", r)
	}

	// --- status and UI
	r = s.kran(time.Minute, s.base, "status", "--json")
	var st proto.State
	if r.code != 0 || json.Unmarshal(r.stdout, &st) != nil {
		t.Fatalf("kran status --json: %s", r)
	}
	if ns, ok := nodeView(&st, "t1-devmac"); !ok || ns.Status != proto.NodeOnline {
		t.Errorf("status: t1-devmac %+v", ns.Status)
	}
	r = s.kran(time.Minute, s.base, "jobs", "--json")
	var jobs []proto.Job
	if r.code != 0 || json.Unmarshal(r.stdout, &jobs) != nil || len(jobs) < 3 {
		t.Errorf("kran jobs --json: %s", r)
	}
	ui := s.uiAddr()
	// The UI's API needs the token; a user gets it with kran ui --print-url.
	r = s.kran(time.Minute, s.base, "ui", "--print-url")
	_, token, ok := strings.Cut(strings.TrimSpace(string(r.stdout)), "#token=")
	if r.code != 0 || !ok || token == "" {
		t.Fatalf("kran ui --print-url gave no token: %s", r)
	}
	for path, want := range map[string]string{"/": "<!doctype html", "/api/v1/state": `"self":"t1-gx10"`, "/api/v1/version": `"api":"v1"`} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+ui+path, nil)
		req.Header.Set("X-Kran-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("UI %s: %v", path, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(string(body)), strings.ToLower(want)) {
			t.Errorf("UI %s: %d %.200s", path, resp.StatusCode, body)
		}
	}

	// --- unlink
	s.timed("kran unlink", func() { r = s.kran(5*time.Minute, s.base, "unlink", "t1-devmac") })
	if r.code != 0 {
		t.Fatalf("kran unlink: %s", r)
	}
	if after, _ := os.ReadFile(localAK); !bytes.Equal(after, localBefore) {
		t.Errorf("local authorized_keys differs after unlink (sha %x → %x)", sha256.Sum256(localBefore), sha256.Sum256(after))
	}
	if after := s.mustSSH("cat ~/.ssh/authorized_keys 2>/dev/null; true"); after != remoteBefore {
		t.Errorf("%s authorized_keys differs after unlink", node)
	}
	s.report()
}

// startDaemon runs the local dev daemon.
func (s *smoke) startDaemon() {
	cmd := exec.Command(kranBin, "daemon")
	cmd.Env = s.env
	out, _ := os.Create(filepath.Join(s.base, "daemon.out"))
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	must(s.t, cmd.Start())
	s.daemon = cmd
	if !poll(30*time.Second, func() bool { return s.kran(5*time.Second, s.base, "status", "--json").code == 0 }) {
		s.t.Fatalf("local dev daemon did not start:\n%s", readFileString(filepath.Join(s.base, "daemon.out")))
	}
}

func (s *smoke) waitPeer(name string, d time.Duration) {
	s.t.Helper()
	var last string
	if !poll(d, func() bool {
		r := s.kran(10*time.Second, s.base, "status", "--json")
		var st proto.State
		if json.Unmarshal(r.stdout, &st) != nil {
			return false
		}
		ns, ok := nodeView(&st, name)
		last = fmt.Sprintf("%s %s", ns.Status, ns.Reason)
		if !ok || ns.Status != proto.NodeOnline || !ns.Link.Connected {
			return false
		}
		for _, rt := range ns.Info.Runtimes {
			if rt.OS == runtime.GOOS && rt.Available {
				return true
			}
			if rt.OS == runtime.GOOS {
				last += "; runtime " + string(rt.Kind) + ": " + rt.Reason
			}
		}
		return false
	}) {
		s.t.Fatalf("%s did not come online with a %s runtime: %s\n%s", name, runtime.GOOS, last, s.tailLog())
	}
}

var uiRe = regexp.MustCompile(`ui: listening on http://(\S+)`)

func (s *smoke) uiAddr() string {
	m := uiRe.FindAllStringSubmatch(readFileString(filepath.Join(s.base, "st", "daemon.log")), -1)
	if len(m) == 0 {
		s.t.Fatal("the dev daemon's UI is not listening")
	}
	return m[len(m)-1][1]
}

// smokePath prepends the node's tool dirs to PATH for its non-login ssh
// shell; KRAN_SMOKE_PATH adds dirs before the Homebrew defaults.
func smokePath() string {
	p := "/opt/homebrew/bin:/usr/local/bin"
	if extra := os.Getenv("KRAN_SMOKE_PATH"); extra != "" {
		p = extra + ":" + p
	}
	return "PATH=" + p + ":$PATH; "
}

// realKran describes the node's real install: its daemon and its sync
// container must be the same before and after (its job containers come and
// go with its own work).
func (s *smoke) realKran() string {
	out, _ := s.ssh(`pgrep -f "kran daemon" | while read p; do ps -o pid=,command= -p $p; done | grep -v kran-dev; ` +
		smokePath() + `docker ps --format '{{.Names}} {{.ID}}' 2>/dev/null | grep '^kran-sync '; true`)
	return out
}

// devLeftovers lists anything of the dev install on the node.
func (s *smoke) devLeftovers() string {
	out, _ := s.ssh(`ps -axo pid=,command= | grep "kran-dev/t1/" | grep -v grep; ` +
		smokePath() +
		`docker ps -a --format 'container {{.Names}}' 2>/dev/null | grep ' kran-dev-'; ` +
		`docker volume ls --format 'volume {{.Name}}' 2>/dev/null | grep ' kran-dev-'; ` +
		`launchctl list 2>/dev/null | grep -i kran-dev; true`)
	return strings.TrimSpace(out)
}

func (s *smoke) cleanup(localAK string, localBefore []byte, remoteBefore, realBefore string) {
	t := s.t
	if s.daemon != nil {
		s.daemon.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { s.daemon.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			s.daemon.Process.Kill()
		}
	}
	// The node's dev daemon was started on demand by the forced command:
	// stop it, then remove what the dev namespace created in Docker.
	s.ssh(`pkill -TERM -f "` + s.rdir + `/bin/kran daemon"; sleep 3; pkill -KILL -f "` + s.rdir + `/bin/kran"; true`)
	keepImage := os.Getenv("KRAN_SMOKE_KEEP_IMAGE") != ""
	dockerClean := smokePath() +
		`for c in $(docker ps -aq --filter name=^kran-dev-); do docker rm -f $c; done; ` +
		`for v in $(docker volume ls -q --filter name=^kran-dev-); do docker volume rm $v; done; `
	if !keepImage {
		dockerClean += `for i in $(docker images -q 'kran-dev-runner'); do docker rmi -f $i; done; `
	}
	if out, err := s.ssh(dockerClean + "true"); err != nil {
		t.Errorf("docker cleanup on %s: %v %s", s.node, err, out)
	}
	s.ssh("rm -rf " + s.rdir + "; rmdir " + s.rhome + "/kran-dev 2>/dev/null; true")

	// Nothing left, nothing of the real install touched.
	if left := s.devLeftovers(); left != "" {
		t.Errorf("leftovers on %s:\n%s", s.node, left)
	}
	if after := s.realKran(); after != realBefore {
		t.Errorf("the real kran on %s changed:\nbefore %s\nafter  %s", s.node, realBefore, after)
	}
	if after, _ := os.ReadFile(localAK); !bytes.Equal(after, localBefore) {
		t.Errorf("local authorized_keys not restored")
		os.WriteFile(localAK, localBefore, 0o600)
	}
	if after := s.mustSSH("cat ~/.ssh/authorized_keys 2>/dev/null; true"); after != remoteBefore {
		t.Errorf("%s authorized_keys not restored", s.node)
	}
	if procs := processesWith(s.base); len(procs) > 0 {
		t.Errorf("local leftovers: %v", procs)
	}
	if t.Failed() {
		t.Logf("local daemon log:\n%s", s.tailLog())
		t.Logf("kept %s", s.base)
		return
	}
	os.RemoveAll(s.base)
}

func (s *smoke) tailLog() string {
	l := strings.Split(readFileString(filepath.Join(s.base, "st", "daemon.log")), "\n")
	return strings.Join(l[max(0, len(l)-60):], "\n")
}

func (s *smoke) report() {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "SMOKE %s\t\n", s.node)
	for _, tm := range s.timings {
		fmt.Fprintf(w, "%s\t%s\n", tm[0], tm[1])
	}
	w.Flush()
	s.t.Logf("\n%s", b.String())
}

func moduleRoot(t *testing.T) string {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

func readFileString(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
