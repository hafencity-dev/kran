package execenv

import (
	"context"
	"fmt"
	"os"
	"runtime"
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
echo "py=$(command -v python3) sys=$(/usr/bin/python3 -c 'import sys; print(sys.version_info[0])')"
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
		for _, want := range []string{"v24.8.0\n", "go1.25.1 ", "Python 3.12.", "no-deno", "py=/usr/local/lib/kran/shims/python3 sys=3\n",
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

// TestContainerDockerFromJob runs Docker from inside a job of the real
// runner image, as a Docker-based test suite would: a bind mount of the
// workspace ($PWD, and a relative path in a compose file) and of the home
// directory reaches the job's own files through kran's Docker proxy, with
// slots on the host and in the volume (the macOS layout). It also checks
// that the image has the everyday tools scripts expect. Opt-in like
// TestContainerRealRunner:
//
//	KRAN_TEST_DOCKER=1 KRAN_TEST_RUNNER=1 go test -run ContainerDockerFromJob ./internal/execenv/
func TestContainerDockerFromJob(t *testing.T) {
	if os.Getenv("KRAN_TEST_RUNNER") != "1" {
		t.Skip("set KRAN_TEST_DOCKER=1 KRAN_TEST_RUNNER=1 to test the real runner image")
	}
	variants := []bool{runtime.GOOS != "linux"}
	if runtime.GOOS == "linux" {
		variants = append(variants, true)
	}
	for _, volume := range variants {
		name := "hostslots"
		if volume {
			name = "volumeslots"
		}
		t.Run(name, func(t *testing.T) { testDockerFromJob(t, volume) })
	}
}

func testDockerFromJob(t *testing.T, volume bool) {
	te := newContainerEnv(t, volume, func(o *Options, tun *tunables) {
		o.Namespace = runnerTestNS
		tun.runnerDockerfile = ""
	})
	ref, err := te.m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeContainer)
	if err != nil {
		t.Fatal(err)
	}
	te.commit(ref, testRepo, map[string]string{"sub/somefile": "content from the slot\n"})
	origin := &proto.OriginEnv{User: "alice", Home: "/work/kran-test", TZ: "UTC"}
	p := te.startCtr(ref, sh(`set -e
img=alpine:3.20
echo "1:$(docker run --rm --user "$(id -u)" -v "$PWD":/w $img cat /w/somefile)"
docker run --rm --user "$(id -u)" --mount type=bind,src="$PWD",dst=/w $img sh -c 'echo written-by-nested > /w/nested-out'
echo "2:$(cat nested-out)"
mkdir -p c
printf 'services:\n  t:\n    image: %s\n    user: "%s"\n    volumes:\n      - ../:/w\n    command: cat /w/somefile\n' $img "$(id -u)" > c/compose.yml
echo "3:$(docker compose -p "kran-test-$KRAN_JOB_ID" -f c/compose.yml run --rm t)"
docker compose -p "kran-test-$KRAN_JOB_ID" -f c/compose.yml down >/dev/null 2>&1
echo home-file > "$HOME/.kran-test-hf"
echo "4:$(docker run --rm --user "$(id -u)" -v "$HOME":/h $img cat /h/.kran-test-hf)"
printf 'FROM %s\nRUN --mount=type=cache,target=/c echo cached > /c/x\nCOPY somefile /s\nCMD ["cat", "/s"]\n' $img > Dockerfile.kt
tag="kran-test-build-$KRAN_JOB_ID"
docker build -q -t "$tag" -f Dockerfile.kt . >/dev/null
echo "5:$(docker run --rm "$tag")"
docker image rm "$tag" >/dev/null
t=$(mktemp -d); echo tmp-file-ok > "$t/f"; mkdir "$t/ssl"; echo tmp-dir-ok > "$t/ssl/cert"
echo "6:$(docker run --rm -v "$t/f":/etc/x.conf:ro $img cat /etc/x.conf)"
echo "7:$(docker run --rm -v "$t/ssl":/etc/nginx/sslcerts:ro $img cat /etc/nginx/sslcerts/cert)"
echo "8:$(env -i /usr/bin/python3 -I -S -c 'print(6*7)')"
python3 -m venv "$t/venv" && "$t/venv/bin/python" -c 'import sys; print("9:venv", sys.prefix != sys.base_prefix)'
cid=$(docker run -d --label "kran.job=$KRAN_JOB_ID" -v "$PWD":/w -v "$t/f":/etc/x.conf:ro $img sleep 300)
echo "11:$(docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' "$cid")"
echo "12:$(docker inspect -f '{{json .HostConfig.Binds}}' "$cid")"
echo "13:$(docker ps --filter "id=$cid" --format '{{.ID}}' | wc -l) $(docker container ls -q --filter "volume=$PWD" | wc -l)"
docker rm -f "$cid" >/dev/null
docker volume create --opt type=none --opt o=bind --opt device="$PWD" "kt-$KRAN_JOB_ID" >/dev/null
echo "14:$(docker volume inspect -f '{{.Options.device}}' "kt-$KRAN_JOB_ID")"
docker volume rm "kt-$KRAN_JOB_ID" >/dev/null
echo "15:$(awk --version | head -1 | cut -d, -f1) $(awk 'BEGIN { print gensub(/o/, "0", "g", "foo") }')"
for tool in gawk parallel uuidgen hostname tput cpio getopt column hexdump timeout flock setsid nproc realpath iconv perl; do
	command -v "$tool" >/dev/null || echo "missing: $tool"
done
echo "10:$(env -i PATH=/usr/bin:/bin sh -c 'docker compose version >/dev/null && docker buildx version >/dev/null && docker-compose version >/dev/null && command -v docker')"
for tool in envsubst lsof sfdisk fdisk jq rg shellcheck file unzip zip xz zstd bzip2 rsync ps pkill killall nc dig ip ss ping less patch diff bc sqlite3 make gcc g++ curl wget git ssh tree xxd strace openssl docker; do
	command -v "$tool" >/dev/null || echo "missing: $tool"
done
docker compose version >/dev/null && docker buildx version >/dev/null && echo plugins-ok
command -v node >/dev/null && echo "node: $(command -v node)" || echo no-node`), func(r *StartRequest) { r.Spec.OriginEnv = origin })
	exit := waitExit(t, p, 10*time.Minute)
	out, errOut := output(t, p)
	if exit.Code != 0 {
		t.Fatalf("exit %+v\n%s\n%s", exit, out, errOut)
	}
	for _, want := range []string{"1:content from the slot\n", "2:written-by-nested\n", "3:content from the slot\n", "4:home-file\n", "5:content from the slot\n", "6:tmp-file-ok\n", "7:tmp-dir-ok\n", "8:42\n", "9:venv True\n", "10:/usr/bin/docker\n",
		ctrRoot + "/sub ", "/f ", `12:["` + ctrRoot + `/sub:/w","/tmp/`, "13:1 ", "14:" + ctrRoot + "/sub\n", "15:GNU Awk 5", " f00\n", "plugins-ok\n", "no-node\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s\n%s", want, out, errOut)
		}
	}
	if strings.Contains(out, "missing:") {
		t.Errorf("tools missing from the runner image:\n%s", out)
	}
	if v := tmpVolumes(t, te); v != "" {
		t.Errorf("job /tmp volumes left: %s", v)
	}
	t.Logf("job output:\n%s", out)
}
