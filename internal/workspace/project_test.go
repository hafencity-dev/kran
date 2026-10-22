package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestToolchains(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module x\n\ngo 1.25.0\n\ntoolchain go1.25.10 // pinned\n")
	writeFile(t, root, ".nvmrc", "v24.8.0\n")
	writeFile(t, root, "package.json", `{"engines":{"node":">=20"},"packageManager":"pnpm@9.12.1+sha512.abc"}`)
	writeFile(t, root, ".python-version", "3.12\n")
	writeFile(t, root, ".tool-versions", "nodejs 22.0.0\nruby 3.3.0 # comment\n")
	writeFile(t, root, "mise.toml", "[tools]\nterraform = \"1.9\"\nbun = [\"1.1.0\", \"1.0\"]\n\"npm:prettier\" = \"3\"\njava = { version = \"21\" }\n")
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	got := Toolchains(root)
	want := map[string]string{
		"go": "1.25.10", "node": "24.8.0", "pnpm": "9.12.1", "python": "3.12",
		"ruby": "3.3.0", "terraform": "1.9", "bun": "1.1.0", "java": "21", "rust": "1.82.0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Toolchains = %v\nwant %v", got, want)
	}
	if Toolchains(t.TempDir()) != nil {
		t.Fatal("empty dir should have no toolchains")
	}
	r2 := t.TempDir()
	writeFile(t, r2, "go.mod", "module y\ngo 1.24\n")
	if v := Toolchains(r2)["go"]; v != "1.24" {
		t.Fatalf("go line = %q", v)
	}
}

func TestSetupFor(t *testing.T) {
	files := func(names ...string) func(string) bool {
		return func(p string) bool {
			for _, n := range names {
				if n == p {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		files []string
		argv  []string
		want  string
	}{
		{[]string{"pnpm-lock.yaml", "go.sum"}, []string{"pnpm", "test"}, "pnpm install --frozen-lockfile --prefer-offline"},
		{[]string{"pnpm-lock.yaml"}, []string{"pnpm", "install"}, ""},
		{[]string{"package-lock.json"}, []string{"npx", "vitest", "run"}, "npm ci"},
		{[]string{"package-lock.json"}, []string{"npm", "ci"}, ""},
		{[]string{"yarn.lock"}, []string{"/usr/bin/env", "NODE_ENV=test", "jest"}, "yarn install --immutable"},
		{[]string{"bun.lockb"}, []string{"bun", "run", "build"}, "bun install --frozen-lockfile"},
		{[]string{"uv.lock"}, []string{"pytest", "-x"}, "uv sync --frozen"},
		{[]string{"uv.lock"}, []string{"uv", "run", "pytest"}, ""},
		{[]string{"poetry.lock"}, []string{"python", "-m", "pytest"}, "poetry install"},
		{[]string{"go.sum", "pnpm-lock.yaml"}, []string{"go", "test", "./..."}, "go mod download"},
		{[]string{"go.sum"}, []string{"make", "test"}, ""},
		{[]string{"pnpm-lock.yaml"}, []string{"/bin/zsh", "-c", "cd web && FOO=1 pnpm vitest"}, "pnpm install --frozen-lockfile --prefer-offline"},
		{[]string{"pnpm-lock.yaml"}, []string{"bash", "-lc", "pnpm i && pnpm build"}, ""},
		{[]string{"Cargo.lock"}, []string{"cargo", "test"}, ""},
		{[]string{"pnpm-lock.yaml"}, []string{"nice", "-n", "10", "tsc", "--noEmit"}, "pnpm install --frozen-lockfile --prefer-offline"},
		{nil, []string{"pnpm", "test"}, ""},
		{nil, nil, ""},
	}
	for _, c := range cases {
		got := SetupFor(files(c.files...), c.argv)
		run := ""
		if got != nil {
			run = got.Run
			if len(got.Key) != 1 {
				t.Errorf("%v: key %v", c.argv, got.Key)
			}
		}
		if run != c.want {
			t.Errorf("SetupFor(%v, %v) = %q, want %q", c.files, c.argv, run, c.want)
		}
	}
	root := t.TempDir()
	writeFile(t, root, "pnpm-lock.yaml", "x")
	if s := DefaultSetup(root, []string{"vitest"}); s == nil || s.Key[0] != "pnpm-lock.yaml" {
		t.Fatalf("DefaultSetup = %+v", s)
	}
}

func TestLoadProjectConfig(t *testing.T) {
	root := t.TempDir()
	cfg, err := LoadProjectConfig(root)
	if err != nil || cfg.Path != "" {
		t.Fatalf("missing file: %+v %v", cfg, err)
	}
	writeFile(t, root, ".kran.toml", `
os = "any"
[sync]
include = ["vendor-bin/"]
exclude = ["testdata/huge/"]
[env]
forward = ["DATABASE_URL"]
set = { CI = "1" }
[setup]
run = "pnpm install --frozen-lockfile"
key = ["pnpm-lock.yaml"]
[[rule]]
match = "go test*"
cpus = 8
mem = "6GiB"
[[rule]]
match = "pnpm dev"
local = true
priority = "high"
`)
	cfg, err = LoadProjectConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OS != proto.OSAny || cfg.Sync.Include[0] != "vendor-bin/" || cfg.Env.Set["CI"] != "1" ||
		cfg.Setup == nil || cfg.Setup.Key[0] != "pnpm-lock.yaml" || len(cfg.Rules) != 2 ||
		cfg.Rules[0].CPUs != 8 || !cfg.Rules[1].Local || cfg.Path != filepath.Join(root, ".kran.toml") {
		t.Fatalf("config %+v", cfg)
	}

	for body, wantErr := range map[string]string{
		`os = "linux"`:           `use "same" or "any"`,
		"bogus = 1":              "unknown setting",
		"[setup]\nrun = \"\"":    "needs a run command",
		"[[rule]]\nmatch = \"\"": "match is empty",
		"[[rule]]\nmatch = \"x\"\npriority = \"urgent\"": "priority",
		"os = ": ".kran.toml:1",
	} {
		os.WriteFile(filepath.Join(root, ".kran.toml"), []byte(body), 0o644)
		_, err := LoadProjectConfig(root)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%q: err = %v, want %q", body, err, wantErr)
		}
	}
}

func TestLoadProjectConfigDocker(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".kran.toml", "docker = false\n")
	cfg, err := LoadProjectConfig(root)
	if err != nil || cfg.Docker == nil || *cfg.Docker {
		t.Fatalf("%+v %v", cfg, err)
	}
}
