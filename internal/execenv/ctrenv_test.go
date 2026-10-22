package execenv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The container resolves the origin's version of every tool it has, the
// workspace's pins win, and only tools kran has no shims for are
// installed eagerly (the shims-without-version failure: a tool the origin
// has must always resolve).
func TestContainerToolchains(t *testing.T) {
	origin := map[string]string{"go": "1.27.3", "node": "24.8.0", "pnpm": "10.32.1", "python": "3.12.3",
		"bun": "1.2.20", "rust": "1.90.0", "uv": "0.8.0", "npm": "11.6.0", "mise": "2026.9.18", "zig": "0.14.0"}
	cases := []struct {
		name  string
		pins  map[string]string
		want  map[string]string
		eager []string
	}{
		{"origin only", nil,
			map[string]string{"go": "1.27.3", "node": "24.8.0", "pnpm": "10.32.1", "python": "3.12", "bun": "1.2.20", "rust": "1.90.0", "uv": "0.8.0"}, nil},
		{"repo pins win", map[string]string{"node": "22", "go": "1.25.1", "python": "3.11"},
			map[string]string{"go": "1.25.1", "node": "22", "pnpm": "10.32.1", "python": "3.11", "bun": "1.2.20", "rust": "1.90.0", "uv": "0.8.0"}, nil},
		{"pin satisfied by origin keeps the origin's version", map[string]string{"node": ">=20", "pnpm": "^10", "python": "3.12"},
			map[string]string{"go": "1.27.3", "node": "24.8.0", "pnpm": "10.32.1", "python": "3.12", "bun": "1.2.20", "rust": "1.90.0", "uv": "0.8.0"}, nil},
		{"exact python pin", map[string]string{"python": "3.12.3"},
			map[string]string{"go": "1.27.3", "node": "24.8.0", "pnpm": "10.32.1", "python": "3.12.3", "bun": "1.2.20", "rust": "1.90.0", "uv": "0.8.0"}, nil},
		{"unknown tools stay eager, bad values dropped", map[string]string{"java": "21", "terraform": "1.9.0", "mise": "x", "node": "24; rm -rf /", "Bad Key": "1"},
			map[string]string{"go": "1.27.3", "node": "24.8.0", "pnpm": "10.32.1", "python": "3.12", "bun": "1.2.20", "rust": "1.90.0", "uv": "0.8.0"},
			[]string{"java@21", "terraform@1.9.0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := containerToolchains(origin, c.pins)
			if !reflect.DeepEqual(ts.versions, c.want) {
				t.Errorf("versions %v, want %v", ts.versions, c.want)
			}
			if !reflect.DeepEqual(ts.eager, c.eager) {
				t.Errorf("eager %v, want %v", ts.eager, c.eager)
			}
			shims := " " + strings.Join(ts.shims, " ") + " "
			for k := range c.want {
				for _, b := range ctrToolBins[k] {
					if !strings.Contains(shims, " "+b+" ") {
						t.Errorf("no shim for %s (%s)", b, k)
					}
				}
			}
		})
	}
}

func TestContainerToolchainsShims(t *testing.T) {
	ts := containerToolchains(map[string]string{"node": "24.8.0", "python": "3.13.1"}, map[string]string{"npm": "10.9.0"})
	tools, shims := ts.initEnv()
	if tools != "npm=10.9.0 node=24.8.0 python=3.13" {
		t.Errorf("tools %q", tools)
	}
	if shims != "npm npx node corepack python python3 pip pip3 python3.13 pip3.13" {
		t.Errorf("shims %q", shims)
	}
	if ts := containerToolchains(nil, nil); len(ts.versions) != 0 || len(ts.shims) != 0 {
		t.Errorf("empty: %+v", ts)
	}
}

func TestCtrHomeFor(t *testing.T) {
	const root = "/home/alice/src/proj"
	cases := []struct {
		home     string
		canMount bool
		want     string
		mount    bool
	}{
		{"/home/alice", true, "/home/alice", true},
		{"/home/alice/", true, "/home/alice", true},
		{"/Users/alice", true, "/Users/alice", true},
		{"/home/alice", false, ctrHome, false}, // Docker too old for subpath mounts
		{"", true, ctrHome, false},
		{"relative", true, ctrHome, false},
		{root, true, root, false},
		{root + "/inner", true, ctrHome, false},
		{"/", true, ctrHome, false},
		{"/etc", true, ctrHome, false},
		{"/kran/cache/home", true, ctrHome, false},
		{"/home/a:b", true, ctrHome, false},
		{"/home/a,src=/", true, ctrHome, false},
	}
	for _, c := range cases {
		got, mount := ctrHomeFor(root, c.home, c.canMount)
		if got != c.want || mount != c.mount {
			t.Errorf("ctrHomeFor(%q, %v) = %q, %v; want %q, %v", c.home, c.canMount, got, mount, c.want, c.mount)
		}
	}
}

func TestCtrUser(t *testing.T) {
	for _, c := range []struct {
		oe       *proto.OriginEnv
		fallback string
		want     string
	}{
		{&proto.OriginEnv{User: "alice"}, "bob", "alice"},
		{nil, "bob", "bob"},
		{&proto.OriginEnv{User: "root"}, "bob", "bob"},
		{&proto.OriginEnv{User: "x:0:0::/:/bin/sh"}, "", "kran"},
		{&proto.OriginEnv{User: "-rf"}, "a b", "kran"},
	} {
		if got := ctrUser(c.oe, c.fallback); got != c.want {
			t.Errorf("ctrUser(%+v, %q) = %q, want %q", c.oe, c.fallback, got, c.want)
		}
	}
}

func TestCtrDropEnv(t *testing.T) {
	for _, c := range []struct {
		k, v string
		drop bool
	}{
		{"DOCKER_HOST", "unix:///Users/alice/.colima/default/docker.sock", true},
		{"DOCKER_HOST", "tcp://build.example:2376", false},
		{"DOCKER_CONTEXT", "colima", true},
		{"DOCKER_CONFIG", "/home/alice/.docker", true},
		{"DOCKER_BUILDKIT", "1", false},
		{"FOO", "bar", false},
	} {
		if got := ctrDropEnv(c.k, c.v); got != c.drop {
			t.Errorf("ctrDropEnv(%s=%s) = %v", c.k, c.v, got)
		}
	}
}

func TestCtrShmSizeAndPath(t *testing.T) {
	if got := ctrShmSize(0); got != 2*gib {
		t.Errorf("no cap: %d", got)
	}
	if got := ctrShmSize(8 * gib); got != 4*gib {
		t.Errorf("8 GiB cap: %d", got)
	}
	if got := ctrShmSize(64 << 20); got != 64<<20 {
		t.Errorf("tiny cap: %d", got)
	}
	p := ctrPath("/home/alice")
	if !strings.HasPrefix(p, ctrShims+":/home/alice/.local/bin:/home/alice/go/bin:") || strings.Contains(p, "mise/shims") {
		t.Errorf("PATH %q", p)
	}
}

// The image is built from a context holding the Dockerfile and kran-init,
// and a change to either rebuilds it.
func TestRunnerBuildContext(t *testing.T) {
	b, err := buildContext("FROM scratch\n", "")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got[h.Name] = string(body)
		if h.Name == "kran-init" && h.Mode&0o111 == 0 {
			t.Error("kran-init not executable")
		}
	}
	if got["Dockerfile"] != "FROM scratch\n" || got["kran-init"] != runnerInit || got["kran"] != noBinary {
		t.Errorf("context files: %v", len(got))
	}
	for _, want := range []string{"COPY kran-init " + ctrInit, "COPY kran " + ctrKran, "MISE_GLOBAL_CONFIG_FILE=" + ctrMiseConf, ctrShims + ":", "locales", "tzdata", "safe.directory"} {
		if !strings.Contains(runnerDockerfile, want) {
			t.Errorf("runner Dockerfile lacks %q", want)
		}
	}
	if strings.Contains(runnerDockerfile, "/kran/tools/mise/shims") {
		t.Error("mise's own shims are on the image PATH: a tool installed by another job but not configured fails with 'No version is set'")
	}
	m := &manager{}
	before := m.dockerfileLabel()
	saved := runnerInit
	runnerInit += "\n# changed\n"
	defer func() { runnerInit = saved }()
	if m.dockerfileLabel() == before {
		t.Error("changing kran-init does not change the image label")
	}
}
