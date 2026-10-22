//go:build chaos

package e2e

// Chaos suite (make chaos): a randomized schedule of submissions from
// several owners on three real daemons, plus random kills of CLIs,
// daemons and supervisors and cuts/stalls/outages of the links. After the
// run every invariant of SPEC §1.6 is checked:
//
//   - every kran run ended; every job it got reached a terminal state that
//     agrees with what the CLI reported, output is never corrupted or
//     duplicated, and no command ran twice;
//   - no job process, supervisor or kran systemd unit is left;
//   - every node is idle: queue empty, nothing allocated or running;
//   - slots were released (4 parallel jobs per repo and target start at
//     once);
//   - no goroutines piled up in the daemons (KRAN_DEBUG=1 endpoint);
//   - no data race in any kran process (KRAN_E2E_RACE=1).
//
// KRAN_CHAOS_DURATION (default 3m) and KRAN_CHAOS_SEED replay a run.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

type chaosJob struct {
	n        int
	origin   string
	target   string
	owner    string
	kind     string
	code     int    // the command's own exit code
	want     []byte // stdout of a complete run
	proc     *cliProc
	signaled string // "", "KILL", "INT", "INT2"
	// filled by the checks
	id     string
	result result
	record *proto.Job
}

type chaosRun struct {
	t      *testing.T
	c      *cluster
	rng    *rand.Rand
	marks  string
	repos  map[string]string
	jobs   []*chaosJob
	faults map[string]int
	issues []string
	later  []pending
	down   string // node whose daemon is down
	outage string // node whose link is down
	start  time.Time
	events []string // timeline of submissions and faults
}

func (cr *chaosRun) logf(format string, a ...any) {
	cr.events = append(cr.events, fmt.Sprintf("%7.2fs ", time.Since(cr.start).Seconds())+fmt.Sprintf(format, a...))
}

type pending struct {
	at time.Time
	fn func()
}

func (cr *chaosRun) violation(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	cr.issues = append(cr.issues, msg)
	cr.t.Errorf("invariant: %s", msg)
}

func TestChaos(t *testing.T) {
	dur := 3 * time.Minute
	if v := os.Getenv("KRAN_CHAOS_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("KRAN_CHAOS_DURATION: %v", err)
		}
		dur = d
	}
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv("KRAN_CHAOS_SEED"); v != "" {
		s, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("KRAN_CHAOS_SEED: %v", err)
		}
		seed = s
	}
	t.Logf("chaos: seed %d (replay: KRAN_CHAOS_SEED=%d), duration %v", seed, seed, dur)

	names := []string{"a", "b", "c"}
	opts := map[string]nodeOpts{}
	for _, n := range names {
		opts[n] = nodeOpts{MaxJobs: 4, Env: []string{"KRAN_DEBUG=1"}}
	}
	c := newCluster(t, names, opts)
	cr := &chaosRun{t: t, c: c, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		marks: filepath.Join(c.base, "marks"), repos: map[string]string{}, faults: map[string]int{}}
	for _, n := range names {
		cr.repos[n] = makeRepo(t, filepath.Join(c.base, "repo-"+n), map[string]string{
			"f":          n,
			".kran.toml": "[setup]\nrun = \"sleep 1\"\nkey = [\"setup.lock\"]\n",
			"setup.lock": "1",
		})
	}

	// Warm up: every node runs a job for every other one, then record the
	// steady-state goroutine counts.
	for _, o := range names {
		for _, tg := range names {
			if o == tg {
				continue
			}
			if r := c.node(o).kran(runOpts{dir: cr.repos[o]}, "run", "-q", "-n", tg, "--", "true"); r.code != 0 {
				t.Fatalf("warm-up %s → %s: %s", o, tg, r)
			}
		}
	}
	baseline := map[string]int{}
	baseStacks := map[string]map[string]int{}
	for _, n := range names {
		c.node(n).waitIdle(30 * time.Second)
	}
	time.Sleep(3 * time.Second)
	for _, n := range names {
		baseline[n], baseStacks[n] = goroutines(t, c.node(n))
	}

	cr.start = time.Now()
	cr.run(dur)
	cr.heal()
	cr.collect()
	cr.checkJobs()
	cr.checkLeftovers()
	cr.checkIdle()
	cr.checkSlots()
	time.Sleep(3 * time.Second)
	goroutineReport := cr.checkGoroutines(baseline, baseStacks)
	cr.summary(seed, dur, goroutineReport)
}

// run is the randomized schedule.
func (cr *chaosRun) run(dur time.Duration) {
	deadline := time.Now().Add(dur)
	nextFault := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cr.runPending(false)
		if time.Now().After(nextFault) {
			cr.fault()
			nextFault = time.Now().Add(time.Duration(3000+cr.rng.IntN(5000)) * time.Millisecond)
		}
		if cr.live() < 10 {
			cr.submit()
		}
		time.Sleep(time.Duration(300+cr.rng.IntN(1200)) * time.Millisecond)
	}
}

func (cr *chaosRun) live() int {
	n := 0
	for _, j := range cr.jobs {
		if !j.proc.exited() {
			n++
		}
	}
	return n
}

func (cr *chaosRun) runPending(all bool) {
	var keep []pending
	for _, p := range cr.later {
		if all || time.Now().After(p.at) {
			p.fn()
		} else {
			keep = append(keep, p)
		}
	}
	cr.later = keep
}

func (cr *chaosRun) after(d time.Duration, fn func()) {
	cr.later = append(cr.later, pending{at: time.Now().Add(d), fn: fn})
}

func (cr *chaosRun) pick(s []string) string { return s[cr.rng.IntN(len(s))] }

func (cr *chaosRun) others(n string) []string {
	var out []string
	for _, m := range cr.c.names {
		if m != n {
			out = append(out, m)
		}
	}
	return out
}

// submit starts one kran run with a random shape.
func (cr *chaosRun) submit() {
	n := len(cr.jobs) + 1
	origin := cr.pick(cr.c.names)
	if !cr.c.node(origin).running() {
		return // its CLI would just run standalone; keep the record checks meaningful
	}
	j := &chaosJob{n: n, origin: origin, owner: fmt.Sprintf("agent:o%d", 1+cr.rng.IntN(4)), code: cr.rng.IntN(4)}
	j.target = cr.pick(cr.others(origin))
	kinds := []string{"local", "remote", "remote", "auto", "stdin", "nofallback"}
	j.kind = cr.pick(kinds)
	lines, sleep := 5+cr.rng.IntN(35), []string{"0.05", "0.1", "0.2"}[cr.rng.IntN(3)]
	var want bytes.Buffer
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&want, "j%d %d\n", n, i)
	}
	script := fmt.Sprintf(`echo %d >> "$MARKS"; i=0; while [ $i -lt %d ]; do echo "j%d $i"; i=$((i+1)); sleep %s; done; exit %d`,
		n, lines, n, sleep, j.code)
	args := []string{"run", "--json-events", "--label", fmt.Sprintf("chaos=%d", n)}
	o := runOpts{dir: cr.repos[origin], owner: j.owner, env: []string{"MARKS=" + cr.marks}, timeout: 10 * time.Minute}
	switch j.kind {
	case "local":
		args = append(args, "--local")
	case "remote":
		args = append(args, "-n", j.target)
	case "nofallback":
		args = append(args, "--no-fallback", "-n", j.target)
	case "stdin":
		in := make([]byte, 16<<10+cr.rng.IntN(64<<10))
		for i := range in {
			in[i] = byte(cr.rng.IntN(256))
		}
		o.stdin = bytes.NewReader(in)
		want.Reset()
		want.Write(in)
		script = fmt.Sprintf(`echo %d >> "$MARKS"; cat; exit %d`, n, j.code)
		args = append(args, "-i", "-n", j.target)
	}
	j.want = want.Bytes()
	args = append(args, "--", "sh", "-c", script)
	j.proc = cr.c.node(origin).startKran(o, args...)
	cr.jobs = append(cr.jobs, j)
	cr.logf("submit #%d %s %s→%s owner %s", n, j.kind, origin, j.target, j.owner)
}

// fault injects one random failure.
func (cr *chaosRun) fault() {
	kinds := []string{"cli-kill", "cli-int", "cli-int2", "daemon-kill", "daemon-restart", "supervisor-kill", "cut", "stall", "outage"}
	kind := cr.pick(kinds)
	switch kind {
	case "cli-kill", "cli-int", "cli-int2":
		var live []*chaosJob
		for _, j := range cr.jobs {
			if !j.proc.exited() && j.signaled == "" {
				live = append(live, j)
			}
		}
		if len(live) == 0 {
			return
		}
		j := live[cr.rng.IntN(len(live))]
		cr.logf("%s #%d", kind, j.n)
		switch kind {
		case "cli-kill":
			j.signaled = "KILL"
			j.proc.signal(syscall.SIGKILL)
		case "cli-int":
			j.signaled = "INT"
			j.proc.signal(syscall.SIGINT)
		default:
			j.signaled = "INT2"
			j.proc.signal(syscall.SIGINT)
			time.Sleep(300 * time.Millisecond)
			j.proc.signal(syscall.SIGINT)
		}
	case "daemon-kill", "daemon-restart":
		if cr.down != "" {
			return
		}
		n := cr.c.node(cr.pick(cr.c.names))
		cr.logf("%s %s", kind, n.name)
		if kind == "daemon-restart" {
			n.restart()
			cr.logf("%s back", n.name)
			break
		}
		n.crash()
		cr.down = n.name
		cr.after(time.Duration(500+cr.rng.IntN(3500))*time.Millisecond, func() {
			n.start()
			cr.down = ""
			cr.logf("%s back", n.name)
		})
	case "supervisor-kill":
		var sups []procInfo
		for _, p := range processesWith(cr.c.base) {
			if strings.Contains(p.cmd, "internal supervise") {
				sups = append(sups, p)
			}
		}
		if len(sups) == 0 {
			return
		}
		victim := sups[cr.rng.IntN(len(sups))]
		cr.logf("supervisor-kill pid %d %s", victim.pid, victim.cmd)
		syscall.Kill(victim.pid, syscall.SIGKILL)
	case "cut":
		n := cr.pick(cr.c.names)
		cr.logf("cut %s", n)
		cr.c.node(n).proxy.Cut()
	case "stall":
		n := cr.pick(cr.c.names)
		p := cr.c.node(n).proxy
		d := time.Duration(2000+cr.rng.IntN(6000)) * time.Millisecond
		cr.logf("stall %s for %v", n, d)
		p.Stall(true)
		cr.after(d, func() { p.Stall(false); cr.logf("stall %s over", n) })
	case "outage":
		if cr.outage != "" {
			return
		}
		n := cr.c.node(cr.pick(cr.c.names))
		cr.outage = n.name
		cr.logf("outage %s", n.name)
		n.proxy.SetDown(true)
		// Longer than the lease: jobs across that link are lost.
		cr.after(time.Duration(18000+cr.rng.IntN(7000))*time.Millisecond, func() {
			n.proxy.SetDown(false)
			cr.outage = ""
			cr.logf("outage %s over", n.name)
		})
	}
	cr.faults[kind]++
}

// heal ends every fault and waits for the mesh.
func (cr *chaosRun) heal() {
	cr.runPending(true)
	for _, n := range cr.c.names {
		node := cr.c.node(n)
		node.proxy.Stall(false)
		node.proxy.SetDown(false)
		if !node.running() {
			node.start()
		}
	}
	cr.c.waitMesh(2 * time.Minute)
}

// collect waits for every CLI and reads the job records.
func (cr *chaosRun) collect() {
	deadline := time.Now().Add(5 * time.Minute)
	for _, j := range cr.jobs {
		select {
		case <-j.proc.done:
		case <-time.After(time.Until(deadline)):
			j.proc.signal(syscall.SIGQUIT)
			time.Sleep(2 * time.Second)
			j.proc.signal(syscall.SIGKILL)
			<-j.proc.done
			cr.violation("job #%d (%s from %s) hung: kran run did not exit within 5 min after the chaos ended:\n%s", j.n, j.kind, j.origin, j.proc.res)
		}
		j.result = j.proc.res
		if ev := event(j.result.events(), "decision"); ev != nil {
			j.id, _ = ev["job_id"].(string)
		}
	}
	// Records settle (restored jobs may wait for their CLI to reattach).
	for _, j := range cr.jobs {
		if j.id == "" {
			continue
		}
		var rec *proto.Job
		poll(90*time.Second, func() bool {
			jobs, err := cr.c.node(j.origin).apiJobs()
			if err != nil {
				return false
			}
			for i := range jobs {
				if jobs[i].Spec.ID == j.id {
					rec = &jobs[i]
					return rec.State.Terminal()
				}
			}
			return false
		})
		j.record = rec
	}
}

func (cr *chaosRun) checkJobs() {
	marks := map[int]int{}
	data, _ := os.ReadFile(cr.marks)
	for _, f := range strings.Fields(string(data)) {
		n, _ := strconv.Atoi(f)
		marks[n]++
	}
	for _, j := range cr.jobs {
		r := j.result
		standalone := event(r.events(), "fallback") != nil
		tag := fmt.Sprintf("job #%d %s (%s %s→%s, signal %q, cli exit %d, %.1fs)", j.n, j.id, j.kind, j.origin, j.target, j.signaled, r.code, r.dur.Seconds())
		if kl := kranLines(r.stderr); kl != "" {
			tag += "\n    kran said: " + kl
		}
		if marks[j.n] > 1 {
			cr.violation("%s: the command ran %d times", tag, marks[j.n])
		}
		if !bytes.HasPrefix(j.want, r.stdout) {
			cr.violation("%s: output is not a prefix of the expected output (got %d bytes)", tag, len(r.stdout))
		}
		if j.signaled == "" {
			switch {
			case r.code == j.code:
				if !bytes.Equal(r.stdout, j.want) {
					cr.violation("%s: exited with the command's code but the output is incomplete (%d of %d bytes)", tag, len(r.stdout), len(j.want))
				}
				if marks[j.n] != 1 {
					cr.violation("%s: completed but the command ran %d times", tag, marks[j.n])
				}
			case r.code == proto.ExitInfra:
				// kran gave up after the job started: allowed under chaos.
			default:
				cr.violation("%s: unexpected exit code (command exits %d)\nstderr: %s", tag, j.code, clip(r.stderr))
			}
		}
		if j.id == "" {
			continue
		}
		rec := j.record
		switch {
		case rec == nil:
			if !standalone {
				cr.violation("%s: no record of the job on %s", tag, j.origin)
			}
			continue
		case !rec.State.Terminal():
			cr.violation("%s: record still %s (%s)", tag, rec.State, rec.Reason)
			continue
		}
		if j.signaled != "" || standalone {
			continue
		}
		switch {
		case r.code == j.code && (rec.State == proto.JobSucceeded || rec.State == proto.JobFailed):
			if rec.ExitCode == nil || *rec.ExitCode != j.code {
				cr.violation("%s: record says exit %v", tag, rec.ExitCode)
			}
		case r.code == j.code:
			cr.violation("%s: the CLI saw the command finish but the record says %s (%s)", tag, rec.State, rec.Reason)
		case r.code == proto.ExitInfra && (rec.State == proto.JobSucceeded || rec.State == proto.JobFailed) && !rec.Local:
			// The CLI lost its daemon for good while the job finished:
			// allowed, the record is the truth.
		}
	}
}

// checkLeftovers: nothing of any job survives.
func (cr *chaosRun) checkLeftovers() {
	allowed := func(p procInfo) bool {
		if strings.Contains(p.cmd, "serve --stdio") {
			return true
		}
		for _, n := range cr.c.names {
			if p.pid == cr.c.node(n).pid() {
				return true
			}
		}
		return false
	}
	stray := func() map[int]string {
		m := map[int]string{}
		for _, p := range processesWith(cr.c.base) {
			if !allowed(p) {
				m[p.pid] = p.cmd
			}
		}
		return m
	}
	var left map[int]string
	poll(60*time.Second, func() bool {
		a := stray()
		time.Sleep(2 * time.Second)
		b := stray()
		left = map[int]string{}
		for pid, cmd := range b {
			if a[pid] == cmd {
				left[pid] = cmd
			}
		}
		return len(left) == 0
	})
	for pid, cmd := range left {
		cr.violation("orphan process %d: %s", pid, cmd)
	}
	ids := map[string]bool{}
	for _, n := range cr.c.names {
		if jobs, err := cr.c.node(n).apiJobs(); err == nil {
			for _, j := range jobs {
				ids[j.Spec.ID] = true
			}
		}
	}
	for _, u := range leftoverUnits(ids) {
		cr.violation("leftover systemd unit %s", u)
	}
	if os.Getenv("KRAN_TEST_DOCKER") == "1" {
		for id := range ids {
			if out := dockerPS(cr.t, "label=kran.job="+id); strings.TrimSpace(out) != "" {
				cr.violation("leftover container: %s", out)
			}
		}
	}
}

func (cr *chaosRun) checkIdle() {
	for _, n := range cr.c.names {
		node := cr.c.node(n)
		var why string
		if !poll(90*time.Second, func() bool {
			st, err := node.state()
			if err != nil {
				why = err.Error()
				return false
			}
			why = idleProblem(st)
			for _, j := range st.Jobs {
				if !j.State.Terminal() {
					why += fmt.Sprintf("; job %s still %s", j.Spec.ID, j.State)
				}
			}
			return why == ""
		}) {
			cr.violation("%s not idle after the chaos: %s", n, why)
		}
	}
}

// checkSlots: once every job ended, no workspace slot is still busy on
// any target (a leaked slot blocks that repo there until the reaper's
// 30 min safety net).
func (cr *chaosRun) checkSlots() {
	for _, n := range cr.c.names {
		var busy []string
		if !poll(60*time.Second, func() bool {
			var body struct {
				Busy []string `json:"busy"`
			}
			if err := json.Unmarshal(debugGet(cr.t, cr.c.node(n), "slots"), &body); err != nil {
				return false
			}
			busy = body.Busy
			return len(busy) == 0
		}) {
			cr.violation("%s: workspace slots still busy after every job ended: %v", n, busy)
		}
	}
}

func (cr *chaosRun) checkGoroutines(baseline map[string]int, baseStacks map[string]map[string]int) string {
	var b strings.Builder
	for _, n := range cr.c.names {
		cr.c.node(n).waitIdle(30 * time.Second)
	}
	for _, n := range cr.c.names {
		var now int
		var stacks map[string]int
		node := cr.c.node(n)
		// Idle connections and timers settle; take the lowest of a few.
		for i := 0; i < 5; i++ {
			c, s := goroutines(cr.t, cr.c.node(n))
			if i == 0 || c < now {
				now, stacks = c, s
			}
			time.Sleep(time.Second)
		}
		fmt.Fprintf(&b, "%s %d→%d  ", n, baseline[n], now)
		if limit := baseline[n] + max(30, baseline[n]/4); now > limit {
			var grew []string
			for k, v := range stacks {
				if d := v - baseStacks[n][k]; d > 2 {
					grew = append(grew, fmt.Sprintf("+%d %s", d, k))
				}
			}
			sort.Strings(grew)
			dump := filepath.Join(node.dir, "goroutines.txt")
			os.WriteFile(dump, debugGet(cr.t, node, "goroutines"), 0o644)
			cr.violation("%s: %d goroutines after the chaos, %d before (limit %d; full dump %s); grew:\n%s", n, now, baseline[n], limit, dump, strings.Join(grew, "\n"))
		}
	}
	return b.String()
}

// debugGet reads one of the daemon's KRAN_DEBUG endpoints.
func debugGet(t *testing.T, n *knode, what string) []byte {
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", n.sock())
		}}}
	resp, err := cl.Get("http://kran/api/v1/debug/" + what)
	if err != nil {
		t.Errorf("%s: debug %s: %v", n.name, what, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

// goroutines reads a daemon's goroutine count and stacks grouped by their
// innermost kran frame.
func goroutines(t *testing.T, n *knode) (int, map[string]int) {
	body := debugGet(t, n, "goroutines")
	var total int
	if _, err := fmt.Sscanf(string(body), "goroutine profile: total %d", &total); err != nil {
		t.Fatalf("%s: goroutine dump: %q", n.name, clip(body))
	}
	stacks := map[string]int{}
	_, rest, _ := strings.Cut(string(body), "\n") // header line
	for _, blk := range strings.Split(rest, "\n\n") {
		var cnt int
		fmt.Sscanf(blk, "%d @", &cnt)
		key := "?"
		for _, l := range strings.Split(blk, "\n") {
			if strings.HasPrefix(l, "#") && strings.Contains(l, "hafencity-dev/kran/") {
				f := strings.Fields(l)
				if len(f) >= 3 {
					key = f[2]
					break
				}
			}
		}
		stacks[key] += cnt
	}
	return total, stacks
}

func (cr *chaosRun) summary(seed uint64, dur time.Duration, gr string) {
	states := map[string]int{}
	codes := map[int]int{}
	kinds := map[string]int{}
	for _, j := range cr.jobs {
		kinds[j.kind]++
		codes[j.result.code]++
		s := "no job (standalone)"
		switch {
		case j.record != nil:
			s = string(j.record.State)
			if j.record.Fallback {
				s += " (fallback)"
			}
		case j.id != "":
			s = "missing record"
		}
		states[s]++
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "CHAOS SUMMARY\tseed %d, %v, %d kran runs\n", seed, dur, len(cr.jobs))
	row := func(title string, m map[string]int) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
		}
		fmt.Fprintf(w, "%s\t%s\n", title, strings.Join(parts, ", "))
	}
	row("job kinds", kinds)
	row("final states", states)
	cm := map[string]int{}
	for k, v := range codes {
		cm[fmt.Sprintf("%3d", k)] = v
	}
	row("cli exit codes", cm)
	row("faults", cr.faults)
	fmt.Fprintf(w, "goroutines\t%s\n", gr)
	verdict := "all invariants hold"
	if len(cr.issues) > 0 {
		verdict = fmt.Sprintf("%d violations", len(cr.issues))
	}
	fmt.Fprintf(w, "invariants\t%s\n", verdict)
	w.Flush()
	cr.t.Logf("\n%s", b.String())
	if cr.t.Failed() || os.Getenv("KRAN_CHAOS_TIMELINE") != "" {
		cr.t.Logf("timeline:\n%s", strings.Join(cr.events, "\n"))
	}
	if p := os.Getenv("KRAN_CHAOS_SUMMARY"); p != "" {
		os.WriteFile(p, []byte(b.String()), 0o644)
	}
}

// kranLines are the CLI's own stderr lines (notices and JSON events
// without the noisy phase/decision ones), joined.
func kranLines(stderr []byte) string {
	var out []string
	for _, ev := range parseEvents(stderr) {
		switch ev["kran"] {
		case "notice", "fallback", "exit":
			b, _ := json.Marshal(ev)
			out = append(out, string(b))
		}
	}
	return strings.Join(out, " | ")
}
