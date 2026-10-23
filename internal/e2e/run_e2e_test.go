//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// twoNodes is the common fixture: a (origin) and b (target), a repo on a.
func twoNodes(t *testing.T, files map[string]string, opts map[string]nodeOpts) (*cluster, *knode, *knode, string) {
	t.Helper()
	c := newCluster(t, []string{"a", "b"}, opts)
	if files == nil {
		files = map[string]string{"README": "e2e\n"}
	}
	repo := makeRepo(t, filepath.Join(c.base, "repo"), files)
	return c, c.node("a"), c.node("b"), repo
}

func TestE2ELocalRun(t *testing.T) {
	c, a, _, repo := twoNodes(t, nil, nil)
	r := a.kran(runOpts{dir: repo}, "run", "--local", "--", "sh", "-c", `printf 'out\n'; printf 'err\n' >&2; echo "id=$KRAN_JOB_ID node=$KRAN_NODE"; exit 7`)
	if r.code != 7 {
		t.Fatalf("local run: %s", r)
	}
	if !strings.HasPrefix(string(r.stdout), "out\nid=") || !strings.Contains(string(r.stdout), "node=a\n") {
		t.Errorf("stdout %q", r.stdout)
	}
	if string(r.stderr) != "err\n" {
		t.Errorf("kran must stay silent on a plain local run; stderr %q", r.stderr)
	}
	id := strings.TrimPrefix(strings.Fields(strings.SplitN(string(r.stdout), "\n", 3)[1])[0], "id=")
	j, ok := a.job(id)
	if !ok || j.State != proto.JobFailed || j.ExitCode == nil || *j.ExitCode != 7 || !j.Local || j.Node != "a" {
		t.Errorf("job record %s: %+v", id, j)
	}
	a.waitIdle(10 * time.Second)
	_ = c
}

func TestE2ERemoteRunOutputAndExit(t *testing.T) {
	bin := make([]byte, 1<<20)
	rng := rand.NewChaCha8([32]byte{1, 2, 3})
	rng.Read(bin)
	crlf := "line1\r\nline2\r\n\r\nno newline at end\r"
	c, a, b, repo := twoNodes(t, map[string]string{"blob.bin": string(bin), "crlf.txt": crlf}, nil)

	// Byte-exact stdout (binary and CRLF), stderr kept apart, exit code.
	r := a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", `cat blob.bin crlf.txt; printf 'to stderr\r\n' >&2; exit 42`)
	if r.code != 42 {
		t.Fatalf("remote run: %s", r)
	}
	want := append(append([]byte(nil), bin...), crlf...)
	if !bytes.Equal(r.stdout, want) {
		t.Fatalf("stdout differs: got %d bytes sha %x, want %d bytes sha %x", len(r.stdout), sha256.Sum256(r.stdout), len(want), sha256.Sum256(want))
	}
	if !strings.HasSuffix(string(r.stderr), "to stderr\r\n") {
		t.Errorf("stderr %q", r.stderr)
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(r.stderr), "to stderr\r\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, "kran: ") {
			t.Errorf("non-kran line on stderr: %q", l)
		}
	}

	// Signals: 128+n as a shell would.
	for _, tc := range []struct {
		script string
		code   int
	}{
		{`kill -TERM $$`, 143},
		{`kill -KILL $$`, 137},
		{`kill -INT $$; sleep 5`, 130},
		{`exit 0`, 0},
		{`exit 255`, 255},
		{`no-such-command-kran-e2e`, 127},
	} {
		r := a.kran(runOpts{dir: repo}, "run", "-q", "-n", "b", "--", "sh", "-c", tc.script)
		if r.code != tc.code {
			t.Errorf("%q: want exit %d, %s", tc.script, tc.code, r)
		}
	}

	// The job record agrees.
	jobs := a.jobsJSON()
	var remote int
	for _, j := range jobs {
		if j.Node == "b" && !j.Local {
			remote++
		}
	}
	if remote != 7 {
		t.Errorf("want 7 remote jobs on b in kran jobs, got %d", remote)
	}
	a.waitIdle(10 * time.Second)
	b.waitIdle(10 * time.Second)
	_ = c
}

func TestE2EStdinForwarding(t *testing.T) {
	_, a, b, repo := twoNodes(t, nil, nil)
	in := make([]byte, 3<<20)
	rng := rand.NewChaCha8([32]byte{9})
	rng.Read(in)
	copy(in, "\r\n\x00\x04\x03")
	r := a.kran(runOpts{dir: repo, stdin: bytes.NewReader(in)}, "run", "-i", "-n", "b", "--", "cat")
	if r.code != 0 || !bytes.Equal(r.stdout, in) {
		t.Fatalf("stdin round trip: code %d, got %d bytes (want %d), equal=%v\nstderr %q", r.code, len(r.stdout), len(in), bytes.Equal(r.stdout, in), r.stderr)
	}
	// Without -i a remote job sees an empty stdin (EOF), never the
	// caller's input.
	r = a.kran(runOpts{dir: repo, stdin: strings.NewReader("secret input\n")}, "run", "-n", "b", "--", "sh", "-c", `cat; echo done`)
	if r.code != 0 || string(r.stdout) != "done\n" {
		t.Fatalf("no -i: %s", r)
	}
	a.waitIdle(10 * time.Second)
	b.waitIdle(10 * time.Second)
}

// helper for tests that print a counter
func counter(n int, sleep string) (script, want string) {
	var w strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&w, "tick %d\n", i)
	}
	return fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo "tick $i"; i=$((i+1)); sleep %s; done`, n, sleep), w.String()
}

var _ = os.Getpid

// TestE2ELocalRecordsMatchExit: a local job's record always gets the exit
// its kran run reported, also when the CLI quits right after a burst of
// output (the run session must read the exit before treating the closed
// connection as a cancel).
func TestE2ELocalRecordsMatchExit(t *testing.T) {
	c := newCluster(t, []string{"a"}, nil)
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	for i := 0; i < 12; i++ {
		r := a.kran(runOpts{dir: repo}, "run", "--json-events", "--local", "--", "sh", "-c", "sleep 1.9; seq 1 400000; exit 3")
		id, _ := event(r.events(), "decision")["job_id"].(string)
		if r.code != 3 || id == "" {
			t.Fatalf("run %d: %s", i, r)
		}
		j := a.waitJob(id, 30*time.Second, func(j proto.Job) bool { return j.State.Terminal() })
		if j.State != proto.JobFailed || j.ExitCode == nil || *j.ExitCode != 3 {
			t.Fatalf("run %d: record %s %v (%s), the CLI reported exit 3", i, j.State, j.ExitCode, j.Reason)
		}
	}
}

// A remote job that fails on a full disk keeps its exit code, and kran run
// says why (a `kran: disk short:` line, disk_short in the exit event), so a
// caller can rerun it instead of counting a red test.
func TestE2EDiskShortReported(t *testing.T) {
	_, a, _, repo := twoNodes(t, map[string]string{"f": "x"}, nil)
	r := a.kran(runOpts{dir: repo}, "run", "--json-events", "-n", "b", "--", "sh", "-c", `echo "write /tmp/x: no space left on device" >&2; exit 1`)
	if r.code != 1 || !strings.Contains(string(r.stderr), `"reason":"disk short: the job's output says no space left on device on b; rerun it"`) {
		t.Fatalf("disk short over json events: %s", r)
	}
	ex := event(r.events(), "exit")
	if ex == nil || ex["disk_short"] != true {
		t.Fatalf("exit event %v", ex)
	}
	r = a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", `echo "no space left on device" >&2; exit 3`)
	if r.code != 3 || !strings.Contains(string(r.stderr), "kran: disk short: ") {
		t.Fatalf("disk short on stderr: %s", r)
	}
}
