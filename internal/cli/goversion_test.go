package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIsGoCommand(t *testing.T) {
	cases := []struct {
		argv  []string
		shell string
		want  bool
	}{
		{[]string{"go", "test", "./..."}, "", true},
		{[]string{"/usr/local/go/bin/go", "vet"}, "", true},
		{[]string{"gotestsum", "--", "./..."}, "", true},
		{[]string{"make", "test"}, "", false},
		{[]string{"pnpm", "go"}, "", false},
		{nil, "cd x && go test ./...", true},
		{nil, "CGO_ENABLED=0 go build ./cmd/kran", true},
		{nil, "time go test -race ./...", true},
		{nil, "pnpm build; go vet ./...", true},
		{nil, "echo go | cat", false},
		{nil, "make test | tail -5", false},
	}
	for _, c := range cases {
		if got := isGoCommand(c.argv, c.shell); got != c.want {
			t.Errorf("%v %q: %v, want %v", c.argv, c.shell, got, c.want)
		}
	}
}

func TestParseGoVersion(t *testing.T) {
	for in, want := range map[string]string{
		"go1.26.2\n":              "1.26.2",
		"go1.27":                  "1.27",
		"go1.27rc1":               "1.27rc1",
		"devel go1.28-abc123 +x":  "",
		"go1.26.2 X:nocoverage\n": "",
		"":                        "",
	} {
		if got := parseGoVersion(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestBuildSpecPinsTheLocalGoVersionForGoCommands(t *testing.T) {
	calls := 0
	in := testInput("go", "test", "./...")
	in.GoVersion = func(ctx context.Context, dir string, env []string) string {
		calls++
		if dir != "/work/repo/pkg" {
			t.Errorf("asked in %s, want the job's cwd", dir)
		}
		return "1.26.2"
	}
	spec, err := BuildSpec(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Req.Toolchains["go"] != "1.26.2" {
		t.Fatalf("toolchains %v", spec.Req.Toolchains)
	}
	in = testInput("make", "test")
	in.GoVersion = func(context.Context, string, []string) string { calls++; return "1.26.2" }
	spec, _ = BuildSpec(context.Background(), in)
	if _, ok := spec.Req.Toolchains["go"]; ok || calls != 1 {
		t.Fatalf("non-Go command: toolchains %v, %d lookups", spec.Req.Toolchains, calls)
	}
}

func TestLocalGoVersionAsksTheGoOnPATHOncePerDir(t *testing.T) {
	bin := t.TempDir()
	count := filepath.Join(t.TempDir(), "count")
	script := "#!/bin/sh\necho x >> " + count + "\necho go1.26.2\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := []string{"PATH=" + bin}
	for i := 0; i < 3; i++ {
		if v := localGoVersion(context.Background(), dir, env); v != "1.26.2" {
			t.Fatalf("version %q", v)
		}
	}
	b, _ := os.ReadFile(count)
	if string(b) != "x\n" {
		t.Fatalf("go ran %q times; want once (cached)", b)
	}
	if v := localGoVersion(context.Background(), t.TempDir(), []string{"PATH=/nonexistent"}); v != "" {
		t.Fatalf("no go on PATH: %q", v)
	}
}
