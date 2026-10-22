package execenv

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// runnerTestNS keeps the real-runner test's image and volumes apart from
// the other container tests (which use a small alpine image).
const runnerTestNS = "kran-testrunner"

// TestContainerRealRunner builds the real runner image and checks what a
// job finds in it: the origin's toolchains (installed on first use, also by
// concurrent jobs), the workspace's pins winning, a user entry that os/user
// finds, the origin's time zone and locale. It downloads Go, Node and
// Python into its own volume, so it is opt-in:
//
//	KRAN_TEST_DOCKER=1 KRAN_TEST_RUNNER=1 go test -run ContainerRealRunner ./internal/execenv/
func TestContainerRealRunner(t *testing.T) {
	if os.Getenv("KRAN_TEST_RUNNER") != "1" {
		t.Skip("set KRAN_TEST_DOCKER=1 KRAN_TEST_RUNNER=1 to test the real runner image")
	}
	te := newContainerEnv(t, false, func(o *Options, tun *tunables) {
		o.Namespace = runnerTestNS
		tun.runnerDockerfile = ""
	})
	ref, err := te.m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeContainer)
	if err != nil {
		t.Fatal(err)
	}
	te.commit(ref, testRepo, map[string]string{
		"sub/go.mod": "module kranuser\n\ngo 1.24\n",
		"sub/main.go": `package main

import (
	"fmt"
	"os"
	"os/user"
)

func main() {
	u, err := user.Current()
	if err != nil {
		fmt.Println("current:", err)
		os.Exit(1)
	}
	l, err := user.Lookup(u.Username)
	if err != nil {
		fmt.Println("lookup:", err)
		os.Exit(1)
	}
	g, _ := user.LookupGroupId(u.Gid)
	fmt.Printf("user=%s uid=%s home=%s lookup=%s group=%v env=%s\n", u.Username, u.Uid, u.HomeDir, l.Uid, g != nil, os.Getenv("HOME"))
}
`,
	})
	origin := &proto.OriginEnv{User: "alice", Home: "/work/kran-test", TZ: "Europe/Berlin",
		Toolchains: map[string]string{"go": "1.25.1", "node": "24.8.0", "python": "3.12.3", "mise": "2026.1.1"}}
	withOrigin := func(r *StartRequest) { r.Spec.OriginEnv = origin }

	t.Run("origin-toolchains-and-user", func(t *testing.T) {
		p := te.startCtr(ref, sh(`set -e
node --version; go version; python3 --version
command -v deno || echo no-deno
go run .
CGO_ENABLED=0 go run .
id; whoami
date +%Z
locale 2>&1
git -C . status >/dev/null 2>&1 || echo git-not-a-repo
ulimit -Sn; df -k /dev/shm | tail -1`), withOrigin, withEnv("LANG", "de_DE.UTF-8"), withEnv("LC_CTYPE", "UTF-8"))
		exit := waitExit(t, p, 10*time.Minute)
		out, errOut := output(t, p)
		if exit.Code != 0 {
			t.Fatalf("exit %+v\n%s\n%s", exit, out, errOut)
		}
		uid := os.Getuid()
		for _, want := range []string{"v24.8.0\n", "go1.25.1 ", "Python 3.12.", "no-deno",
			fmt.Sprintf("user=alice uid=%d home=/work/kran-test lookup=%d group=true env=/work/kran-test", uid, uid),
			fmt.Sprintf("uid=%d(alice)", uid), "\nalice\n", "CE", "LANG=de_DE.UTF-8", "LC_CTYPE=C.UTF-8\n", "65536\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if strings.Contains(out+errOut, "No version is set") || strings.Contains(errOut, "Cannot set") || strings.Contains(errOut, "cannot change locale") {
			t.Errorf("tool or locale errors:\n%s", errOut)
		}
	})

	// Two jobs installing the same pinned version at once both get it.
	t.Run("repo-pin-wins-concurrently", func(t *testing.T) {
		var wg sync.WaitGroup
		outs := make([]string, 2)
		for i := range outs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := te.startCtr(ref, sh(`node --version; npx --version >/dev/null && echo npx-ok`), withOrigin,
					func(r *StartRequest) { r.Spec.Req.Toolchains = map[string]string{"node": "22.19.0"} })
				exit := waitExit(t, p, 10*time.Minute)
				out, errOut := output(t, p)
				outs[i] = fmt.Sprintf("exit=%d\n%s%s", exit.Code, out, errOut)
			}()
		}
		wg.Wait()
		for _, o := range outs {
			if !strings.Contains(o, "exit=0\n") || !strings.Contains(o, "v22.19.0\n") || !strings.Contains(o, "npx-ok") {
				t.Errorf("job:\n%s", o)
			}
		}
	})
}
