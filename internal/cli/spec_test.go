package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/workspace"
)

func testInput(argv ...string) SpecInput {
	cfg := config.Defaults()
	return SpecInput{
		Argv: argv, Cwd: "/work/repo/pkg", Home: "/home/u", Env: []string{"SHELL=/bin/zsh", "FOO=1", "GITHUB_TOKEN=x"},
		Now: time.Unix(100, 0), PID: 7, Config: &cfg,
		Owner: proto.Owner{Kind: proto.OwnerHuman, ID: "u", Name: "u"},
		Detect: func(ctx context.Context, cwd string) (proto.Workspace, error) {
			return proto.Workspace{Root: "/work/repo", RepoID: "r", Rel: "pkg", Git: true, Name: "repo"}, nil
		},
		Project: func(root string) (*workspace.ProjectConfig, error) { return &workspace.ProjectConfig{}, nil },
	}
}

func TestBuildSpecBasics(t *testing.T) {
	in := testInput("go", "test", "./...")
	spec, err := BuildSpec(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Argv, []string{"go", "test", "./..."}) || spec.Workspace.Root != "/work/repo" ||
		spec.Req.OS != proto.OSSame || spec.Req.Prefer != proto.PreferAuto || spec.Req.OriginOnly ||
		spec.Priority != proto.PriorityNormal || spec.ClientPID != 7 {
		t.Fatalf("%+v", spec)
	}
	if spec.Env["FOO"] != "1" {
		t.Fatal("env not forwarded")
	}
	if _, ok := spec.Env["GITHUB_TOKEN"]; ok {
		t.Fatal("secret forwarded")
	}
}

func TestBuildSpecShellAndFlags(t *testing.T) {
	in := testInput()
	in.Flags = RunFlags{Sh: "make test | tail -5", Remote: true, Node: "devmac", GPU: true, CPUs: 4, Mem: "6GiB",
		AnyOS: true, Priority: "low", Labels: []string{"a=b"}, ExtraEnv: []string{"GITHUB_TOKEN"}, Stdin: true}
	in.TTY = &proto.TTY{Rows: 24, Cols: 80}
	spec, err := BuildSpec(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Argv, []string{"zsh", "-c", "make test | tail -5"}) || spec.Shell == "" {
		t.Fatalf("%v", spec.Argv)
	}
	r := spec.Req
	if r.Prefer != proto.PreferRemote || r.Node != "devmac" || !r.GPU || r.CPUs != 4 || r.Mem != 6<<30 || r.OS != proto.OSAny {
		t.Fatalf("%+v", r)
	}
	if !spec.NoFallback || spec.Priority != proto.PriorityLow || spec.Labels["a"] != "b" || spec.Env["GITHUB_TOKEN"] != "x" || !spec.Stdin || spec.TTY == nil {
		t.Fatalf("%+v", spec)
	}
}

func TestBuildSpecTTYDefaultsHigh(t *testing.T) {
	in := testInput("ls")
	in.TTY = &proto.TTY{Rows: 1, Cols: 1}
	spec, _ := BuildSpec(context.Background(), in)
	if spec.Priority != proto.PriorityHigh {
		t.Fatal(spec.Priority)
	}
}

func TestBuildSpecErrors(t *testing.T) {
	for name, mod := range map[string]func(*SpecInput){
		"no command":   func(in *SpecInput) { in.Argv = nil },
		"local+remote": func(in *SpecInput) { in.Flags.Local, in.Flags.Remote = true, true },
		"bad mem":      func(in *SpecInput) { in.Flags.Mem = "lots" },
		"bad priority": func(in *SpecInput) { in.Flags.Priority = "urgent" },
		"bad label":    func(in *SpecInput) { in.Flags.Labels = []string{"nolabel"} },
		"project broken": func(in *SpecInput) {
			in.Project = func(string) (*workspace.ProjectConfig, error) { return nil, errors.New("boom") }
		},
	} {
		in := testInput("go", "test")
		mod(&in)
		if _, err := BuildSpec(context.Background(), in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildSpecRules(t *testing.T) {
	in := testInput("go", "test", "./...")
	in.Project = func(string) (*workspace.ProjectConfig, error) {
		return &workspace.ProjectConfig{
			OS: proto.OSAny,
			Rules: []workspace.Rule{
				{Match: "make *", Local: true},
				{Match: "go test*", CPUs: 8, Mem: "6GiB", Priority: proto.PriorityHigh},
				{Match: "go*", CPUs: 1},
			},
		}, nil
	}
	spec, _ := BuildSpec(context.Background(), in)
	if spec.Req.CPUs != 8 || spec.Req.Mem != 6<<30 || spec.Priority != proto.PriorityHigh || spec.Req.OS != proto.OSAny {
		t.Fatalf("%+v", spec.Req)
	}
	in.Flags.CPUs = 2
	in.Flags.Priority = "low"
	spec, _ = BuildSpec(context.Background(), in)
	if spec.Req.CPUs != 2 || spec.Priority != proto.PriorityLow {
		t.Fatal("flags must beat rules")
	}
	in2 := testInput("make", "test")
	in2.Project = in.Project
	spec, _ = BuildSpec(context.Background(), in2)
	if !spec.Req.OriginOnly {
		t.Fatal("local rule must set origin-only")
	}
}

func TestOriginOnly(t *testing.T) {
	ws := proto.Workspace{Root: "/work/repo", Git: true}
	cases := []struct {
		name  string
		argv  []string
		sh    string
		ws    proto.Workspace
		wsErr error
		want  bool
	}{
		{"plain", []string{"go", "test"}, "", ws, nil, false},
		{"localhost sh", nil, "npm test -- --url http://localhost:3000", ws, nil, true},
		{"127 sh", nil, "curl 127.0.0.1:8080/health && make test", ws, nil, true},
		{"localhost argv", []string{"curl", "localhost:9"}, "", ws, nil, true},
		{"not a hostname", nil, "echo mylocalhost.example.com", ws, nil, false},
		{"home dir", []string{"ls"}, "", proto.Workspace{Root: "/home/u"}, nil, true},
		{"root dir", []string{"ls"}, "", proto.Workspace{Root: "/"}, nil, true},
		{"tmp no git", []string{"ls"}, "", proto.Workspace{Root: "/tmp"}, nil, true},
		{"outside rel", []string{"ls"}, "", proto.Workspace{Root: "/w", Rel: "../x", Git: true}, nil, true},
		{"detect failed", []string{"ls"}, "", ws, errors.New("x"), true},
		{"home inside no-git root", []string{"ls"}, "", proto.Workspace{Root: "/home"}, nil, true},
	}
	for _, c := range cases {
		spec := proto.JobSpec{Argv: c.argv, Shell: c.sh}
		got := originOnlyReason(spec, c.ws, c.wsErr, "/home/u") != ""
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		p, s string
		want bool
	}{
		{"go test*", "go test ./...", true}, {"go test*", "go build", false}, {"*", "", true}, {"*test*", "pnpm run test", true},
		{"go ?est", "go test", true}, {"go test", "go test -v", false}, {"a*b*c", "aXXbYYc", true}, {"a*b*c", "aXXbYY", false},
		{"", "", true}, {"", "x", false},
	} {
		if globMatch(c.p, c.s) != c.want {
			t.Errorf("%q vs %q", c.p, c.s)
		}
	}
}

func TestShellName(t *testing.T) {
	for env, want := range map[string]string{"SHELL=/bin/bash": "bash", "SHELL=/usr/bin/zsh": "zsh", "SHELL=/usr/bin/fish": "sh", "": "sh"} {
		if got := shellName([]string{env}); got != want {
			t.Errorf("%s: %s", env, got)
		}
	}
}

// TestBuildSpecConfigRemoteYieldsToOriginOnly: submit.prefer = "remote" in
// config.toml is a default; a .kran.toml local rule or a localhost command
// keeps the job here instead of failing it as if --remote had been given.
func TestBuildSpecConfigRemoteYieldsToOriginOnly(t *testing.T) {
	for name, mod := range map[string]func(*SpecInput){
		"rule": func(in *SpecInput) {
			in.Project = func(string) (*workspace.ProjectConfig, error) {
				return &workspace.ProjectConfig{Rules: []workspace.Rule{{Match: "npm run dev*", Local: true}}}, nil
			}
		},
		"localhost": func(in *SpecInput) { in.Argv = []string{"curl", "http://localhost:3000"} },
	} {
		in := testInput("npm", "run", "dev")
		in.Config.Submit.Prefer = "remote"
		mod(&in)
		spec, err := BuildSpec(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !spec.Req.OriginOnly || spec.Req.Prefer != proto.PreferAuto || spec.NoFallback {
			t.Errorf("%s: req %+v nofallback %v", name, spec.Req, spec.NoFallback)
		}
		// An explicit --remote still conflicts (the scheduler says so).
		in.Flags.Remote = true
		spec, _ = BuildSpec(context.Background(), in)
		if spec.Req.Prefer != proto.PreferRemote {
			t.Errorf("%s: --remote lost: %+v", name, spec.Req)
		}
	}
	// Without an origin-only reason the preference stays.
	in := testInput("go", "test")
	in.Config.Submit.Prefer = "remote"
	if spec, _ := BuildSpec(context.Background(), in); spec.Req.Prefer != proto.PreferRemote {
		t.Errorf("config prefer remote dropped: %+v", spec.Req)
	}
}

func TestBuildSpecNoDocker(t *testing.T) {
	in := testInput("go", "test", "./...")
	if spec, _ := BuildSpec(context.Background(), in); spec.Req.NoDocker {
		t.Fatal("docker socket is on by default")
	}
	in.Flags.NoDocker = true
	if spec, _ := BuildSpec(context.Background(), in); !spec.Req.NoDocker {
		t.Fatal("--no-docker ignored")
	}
	off := false
	in = testInput("go", "test", "./...")
	in.Project = func(string) (*workspace.ProjectConfig, error) { return &workspace.ProjectConfig{Docker: &off}, nil }
	if spec, _ := BuildSpec(context.Background(), in); !spec.Req.NoDocker {
		t.Fatal(".kran.toml docker = false ignored")
	}
}

// A repo's .kran.toml may forward secrets (its choice), but the user hears
// about it on every run; --env is the user's own choice and stays silent.
func TestBuildSpecNoticesForwardedSecrets(t *testing.T) {
	run := func(forward, extra []string) []string {
		in := testInput("go", "test", "./...")
		in.Env = append(in.Env, "DB_PASSWORD=pw", "PLAIN=1")
		in.Project = func(string) (*workspace.ProjectConfig, error) {
			pc := &workspace.ProjectConfig{}
			pc.Env.Forward = forward
			return pc, nil
		}
		in.Flags.ExtraEnv = extra
		var notes []string
		in.Notice = func(s string) { notes = append(notes, s) }
		if _, err := BuildSpec(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		return notes
	}
	got := run([]string{"GITHUB_TOKEN", "DB_PASSWORD", "PLAIN", "MISSING_TOKEN"}, nil)
	want := []string{".kran.toml forwards GITHUB_TOKEN to other machines", ".kran.toml forwards DB_PASSWORD to other machines"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notices %q", got)
	}
	if got := run([]string{"GITHUB_TOKEN"}, []string{"GITHUB_TOKEN"}); len(got) != 0 {
		t.Fatalf("--env must stay silent: %q", got)
	}
}
