package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

func TestUnknownCommandIsAUsageError(t *testing.T) {
	a, out, errb := testApp(nil)
	if code := a.Execute(context.Background(), []string{"nonsense"}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if out.Len() != 0 || !strings.Contains(errb.String(), `unknown command "nonsense"`) {
		t.Fatalf("out=%q err=%q", out, errb)
	}
	errb.Reset()
	if code := a.Execute(context.Background(), []string{"stauts"}); code != 2 || !strings.Contains(errb.String(), "did you mean `kran status`?") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestBareKranShowsHelpGroupedAndHidesPlumbing(t *testing.T) {
	a, out, _ := testApp(nil)
	if code := a.Execute(context.Background(), nil); code != 0 {
		t.Fatal(code)
	}
	s := out.String()
	for _, want := range []string{"Run and watch jobs:", "Your machines:", "This machine:", "  run ", "  link "} {
		if !strings.Contains(s, want) {
			t.Errorf("help lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "  serve ") || strings.Contains(s, "  hook ") || strings.Contains(s, "  internal ") {
		t.Errorf("plumbing commands must be hidden:\n%s", s)
	}
}

func TestVersionStringDoesNotRepeatTheCommit(t *testing.T) {
	v, c, d := version.Version, version.Commit, version.BuildDate
	defer func() { version.Version, version.Commit, version.BuildDate = v, c, d }()
	version.BuildDate = "2026-01-02T03:04:05Z"
	for _, tc := range []struct{ ver, commit, want string }{
		{"0.1.0", "abc1234", "kran 0.1.0 (abc1234, built 2026-01-02T03:04:05Z)"},
		{"abc1234", "abc1234", "kran abc1234 (built 2026-01-02T03:04:05Z)"},
		{"abc1234-dirty", "abc1234", "kran abc1234-dirty (built 2026-01-02T03:04:05Z)"},
		{"dev", "none", "kran dev (built 2026-01-02T03:04:05Z)"},
	} {
		version.Version, version.Commit = tc.ver, tc.commit
		if got := versionString(); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.ver, tc.commit, got, tc.want)
		}
	}
}

func TestStatusTodayLineIsQuietWhenThereIsNothingToSay(t *testing.T) {
	one := &proto.State{Nodes: []proto.NodeState{{Self: true, Status: proto.NodeOnline, Info: proto.NodeInfo{Name: "gx10", CPUs: 4}}}}
	for _, tc := range []struct {
		name  string
		stats proto.Stats
		nodes int
		want  string
	}{
		{"none", proto.Stats{}, 1, "today: no jobs yet"},
		{"one local", proto.Stats{JobsToday: 1}, 1, "today: 1 job\n"},
		{"singles do not see offloaded 0", proto.Stats{JobsToday: 3}, 1, "today: 3 jobs\n"},
		{"mesh shows offloaded 0", proto.Stats{JobsToday: 3}, 2, "today: 3 jobs, 0 offloaded\n"},
		{"fallback and infra", proto.Stats{JobsToday: 5, OffloadedToday: 2, FallbacksToday: 1, FailedInfraToday: 1}, 2,
			"today: 5 jobs, 2 offloaded, 1 ran here after a remote attempt failed, 1 failed on kran's side (kran doctor)\n"},
	} {
		st := *one
		st.Stats = tc.stats
		for len(st.Nodes) < tc.nodes {
			st.Nodes = append(st.Nodes, proto.NodeState{Status: proto.NodeOnline, Info: proto.NodeInfo{Name: "devmac", CPUs: 8}})
		}
		a, out, _ := testApp(&stateClient{st: &st})
		if code := a.Execute(context.Background(), []string{"status"}); code != 0 {
			t.Fatal(code)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: want %q in\n%s", tc.name, tc.want, out)
		}
	}
}

func TestJobsWhyAddsTheSchedulerReason(t *testing.T) {
	cl := &stateClient{jobs: []proto.Job{{Spec: proto.JobSpec{ID: "j1", Argv: []string{"true"}}, State: proto.JobSucceeded, Node: "gx10",
		Reason: "here: est. 0s, too short to send elsewhere"}}}
	a, out, _ := testApp(cl)
	if code := a.Execute(context.Background(), []string{"jobs"}); code != 0 || strings.Contains(out.String(), "too short") {
		t.Fatalf("plain jobs must not show the reason: %d\n%s", code, out)
	}
	out.Reset()
	if code := a.Execute(context.Background(), []string{"jobs", "--why"}); code != 0 || !strings.Contains(out.String(), "WHY") || !strings.Contains(out.String(), "too short to send elsewhere") {
		t.Fatalf("%d\n%s", code, out)
	}
}

func TestStatusDoesNotSayWaitingTwice(t *testing.T) {
	st := sampleState()
	st.Jobs[1].Reason = "waiting: needs 6 GiB, devmac has 3.1 GiB free"
	a, out, _ := testApp(&stateClient{st: st})
	if code := a.Execute(context.Background(), []string{"status"}); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out.String(), "waiting: waiting") || !strings.Contains(out.String(), "waiting: needs 6 GiB") {
		t.Fatalf("\n%s", out)
	}
}

func TestArgumentErrorsSayWhatIsNeeded(t *testing.T) {
	a, _, errb := testApp(nil)
	for args, want := range map[string]string{
		"cancel": "cancel needs a job (list jobs with `kran jobs`)",
		"drain":  "drain needs a machine",
		"unlink": "unlink needs a machine",
	} {
		errb.Reset()
		if code := a.Execute(context.Background(), []string{args}); code != 2 || !strings.Contains(errb.String(), want) || strings.Contains(errb.String(), "arg(s)") {
			t.Errorf("%s: %d %q", args, code, errb.String())
		}
	}
}

func TestLogsOfALocalJobExplainTheEmptyOutput(t *testing.T) {
	cl := &stateClient{jobs: []proto.Job{{Spec: proto.JobSpec{ID: "j1"}, Local: true}}}
	a, out, errb := testApp(cl)
	if code := a.Execute(context.Background(), []string{"logs"}); code != 0 || out.Len() != 0 || !strings.Contains(errb.String(), "ran on this machine") {
		t.Fatalf("%d out=%q err=%q", code, out, errb)
	}
}
