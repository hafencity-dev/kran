package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/cli"
	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// `kran run -i` (the real CLI) through two real daemons and the real
// supervisor: input well beyond the flow-control window reaches a job that
// starts reading late, complete and in order.
func TestKranRunStdinThroughRealStack(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "kran-test-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	os.Setenv("KRAN_CACHE_DIR", filepath.Join(base, "cache"))
	os.Setenv("KRAN_CONFIG_DIR", filepath.Join(base, "cliconfig"))
	log := &logSink{}
	defer func() {
		log.mu.Lock()
		if t.Failed() {
			t.Logf("daemon logs:\n%s", strings.Join(log.lines, "\n"))
		}
		log.closed = true
		log.mu.Unlock()
	}()
	net := transport.NewMemNetwork(transport.SessionConfig{})
	startNode(t, net, log, "b", "a", base)
	a := startNode(t, net, log, "a", "b", base)
	sock := filepath.Join(a.dirs.Runtime, "kran.sock")
	root := gitRepo(t, base, map[string]string{"f": "x\n"})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Wait until a places on b.
	ws, err := workspace.Detect(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	warm := proto.JobSpec{Argv: []string{"true"}, Cwd: root, Workspace: ws, Owner: proto.Owner{Kind: proto.OwnerHuman, ID: "t", Name: "t"},
		Req: proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferRemote, Node: "b"}}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		res, err := runJob(ctx, sock, warm)
		if err == nil && res.exit != nil && res.exit.Node == "b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("b never took a job: %v", err)
		}
	}

	var input bytes.Buffer
	for i := 0; input.Len() < 20<<20; i++ {
		fmt.Fprintf(&input, "line %09d\n", i)
	}
	pr, pw, _ := os.Pipe()
	go func() {
		pw.Write(input.Bytes())
		pw.Close()
	}()
	var out, errOut bytes.Buffer
	app := cli.NewApp()
	app.In, app.Out, app.Err = pr, &out, &errOut
	app.Dial = func(ctx context.Context) (client.Client, error) { return client.DialSocket(ctx, sock) }
	wd, _ := os.Getwd()
	os.Chdir(root)
	defer os.Chdir(wd)
	code := app.Execute(ctx, []string{"run", "-i", "-r", "-n", "b", "--", "sh", "-c", "sleep 1; cat"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if out.Len() != input.Len() || !bytes.Equal(out.Bytes(), input.Bytes()) {
		t.Fatalf("job wrote %d of %d bytes back; stderr %s", out.Len(), input.Len(), errOut.String())
	}
}
