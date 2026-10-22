package dockercli

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindPrefersPATHThenHomeInstalls(t *testing.T) {
	home := t.TempDir()
	onPath := filepath.Join(t.TempDir(), "bin")
	writeExe(t, filepath.Join(onPath, "docker"))
	if got := Find("relative/dir"+string(os.PathListSeparator)+onPath, home); got != filepath.Join(onPath, "docker") {
		t.Fatalf("Find = %q, want the PATH entry", got)
	}

	// Not on PATH: OrbStack's per-user install is found (unless a system
	// location on this machine has one first).
	orb := filepath.Join(home, ".orbstack", "bin", "docker")
	writeExe(t, orb)
	got := Find("/nonexistent", home)
	if got == "" {
		t.Fatal("Find did not look in ~/.orbstack/bin")
	}
	for _, d := range binDirs("") {
		if got == filepath.Join(d, "docker") {
			return // a real system docker wins; fine
		}
	}
	if got != orb {
		t.Fatalf("Find = %q, want %q", got, orb)
	}
}

func TestFindSkipsNonExecutables(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Find(dir, ""); got == filepath.Join(dir, "docker") {
		t.Fatal("a non-executable file is not a docker CLI")
	}
}

func TestEndpoint(t *testing.T) {
	home := "/h"
	colima := "/h/.colima/default/docker.sock"
	orb := "/h/.orbstack/run/docker.sock"
	exists := func(have ...string) func(string) bool {
		return func(p string) bool {
			for _, h := range have {
				if p == h {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name, env, ctx string
		have           []string
		want, override string
	}{
		{"DOCKER_HOST wins", "tcp://1.2.3.4:2375", "unix://" + colima, []string{colima}, "tcp://1.2.3.4:2375", ""},
		{"context socket exists", "", "unix://" + colima, []string{colima, "/var/run/docker.sock"}, "unix://" + colima, ""},
		{"non-unix context is trusted", "", "ssh://box", nil, "ssh://box", ""},
		{"stale context falls back to a live socket", "", "unix:///gone.sock", []string{orb}, "unix://" + orb, "unix://" + orb},
		{"no context: colima before system", "", "", []string{colima, "/var/run/docker.sock"}, "unix://" + colima, "unix://" + colima},
		{"no context: system socket", "", "", []string{"/var/run/docker.sock"}, DefaultHost, ""},
		{"rootless", "", "", []string{"/run/user/1000/docker.sock"}, "unix:///run/user/1000/docker.sock", "unix:///run/user/1000/docker.sock"},
		{"nothing exists: the CLI's own choice", "", "unix:///gone.sock", nil, "unix:///gone.sock", ""},
		{"nothing at all", "", "", nil, DefaultHost, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Endpoint(c.env, c.ctx, home, "/run/user/1000", exists(c.have...)); got != c.want {
				t.Errorf("Endpoint = %q, want %q", got, c.want)
			}
			if got := Override(c.env, c.ctx, home, "/run/user/1000", exists(c.have...)); got != c.override {
				t.Errorf("Override = %q, want %q", got, c.override)
			}
		})
	}
}

func TestFromLoginShell(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	writeExe(t, docker)
	shell := filepath.Join(dir, "fakesh")
	// A "login shell" that knows a PATH the daemon does not.
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nPATH="+dir+":$PATH\nshift\nexec /bin/sh -c \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := fromLoginShell(shell); got != docker {
		t.Fatalf("got %q, want %q", got, docker)
	}
	if got := fromLoginShell("sh"); got != "" {
		t.Fatalf("relative shell: %q", got)
	}
}
