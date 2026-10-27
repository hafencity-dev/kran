package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestUnsyncedProgramStaysHome: a command that runs a git-ignored file of
// the workspace (a locally built bin/lk) would fail on another node with
// "No such file", so it stays here; a command that builds it first, a
// program in a directory the target rebuilds (node_modules) and a file
// named in sync.include may leave.
func TestUnsyncedProgramStaysHome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write(".gitignore", "/bin/\nnode_modules/\n")
	write("bin/lk", "#!/bin/sh\n")
	write("bin/tool.sh", "#!/bin/sh\n")
	write("node_modules/.bin/vitest", "#!/bin/sh\n")
	write("scripts/check.sh", "#!/bin/sh\n")
	write("product/web/x.txt", "x")
	run("add", "-A")
	ws := proto.Workspace{Root: root, Git: true}
	home := t.TempDir()
	for _, tool := range []string{"lk", "go"} {
		p := filepath.Join(home, ".local", "bin", tool)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	}
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-other")
	os.MkdirAll(outside, 0o755)
	pathEnv := filepath.Join(home, ".local", "bin") + string(os.PathListSeparator) + "/usr/bin"
	for _, tc := range []struct {
		shell   string
		cwd     string
		include []string
		want    string // substring of the reason, "" = may leave
	}{
		{shell: "bin/lk docs check", want: "runs bin/lk"},
		{shell: "./bin/lk docs check 2>&1 | tail -5", want: "runs ./bin/lk"},
		{shell: "cd product/web && ../../bin/lk docs check", want: "runs ../../bin/lk"},
		{shell: "bash bin/tool.sh", want: "runs bin/tool.sh"},
		{shell: "bin/lk docs check", cwd: "product/web"}, // product/web/bin/lk does not exist
		{shell: "make lk && bin/lk docs check"},
		{shell: "go build -o bin/lk ./cmd/lk && bin/lk docs check"},
		{shell: "node_modules/.bin/vitest run src"},
		{shell: "scripts/check.sh"},
		{shell: "bin/lk docs check", include: []string{"bin/lk"}},
		{shell: "go test ./..."},                                    // go from ~/.local/bin: kran provides it
		{shell: "lk docs check", want: "runs lk from ~/.local/bin"}, // a personal tool
		{shell: "cd ../" + filepath.Base(outside) + " && go test ./...", want: "outside the workspace"},
		{shell: "cd /nonexistent && go test ./..."}, // fails the same anywhere
	} {
		spec := proto.JobSpec{Shell: tc.shell, Argv: []string{"sh", "-c", tc.shell}, Cwd: filepath.Join(root, tc.cwd)}
		got := unsyncedProgram(context.Background(), spec, ws, tc.include, home, pathEnv)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%q in %q: reason %q, want %q", tc.shell, tc.cwd, got, tc.want)
		}
	}
}
