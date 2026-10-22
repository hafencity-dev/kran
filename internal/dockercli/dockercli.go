// Package dockercli finds the docker CLI and the Docker endpoint the way a
// user's shell would, also when kran runs under launchd or systemd with a
// minimal PATH. It knows only public install locations of Docker Desktop,
// Colima, OrbStack, Rancher Desktop, Homebrew and distro packages.
package dockercli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultHost is what the docker CLI uses without DOCKER_HOST or a context.
const DefaultHost = "unix:///var/run/docker.sock"

// binDirs are where Docker installers put the CLI when it is not on PATH
// (launchd agents get /usr/bin:/bin:/usr/sbin:/sbin only).
func binDirs(home string) []string {
	dirs := []string{
		"/usr/local/bin",    // Docker Desktop and Colima on Intel Macs, manual installs
		"/opt/homebrew/bin", // Homebrew on Apple silicon (colima, docker)
		"/usr/bin",          // distro packages
		"/Applications/Docker.app/Contents/Resources/bin", // Docker Desktop without its symlinks
	}
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".orbstack", "bin"),
			filepath.Join(home, ".rd", "bin"), // Rancher Desktop
			filepath.Join(home, ".docker", "bin"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	return dirs
}

// sockets are the engine sockets of the common Docker distributions, in
// the order kran tries them when the CLI's own endpoint does not exist.
func sockets(home, runtimeDir string) []string {
	var s []string
	if home != "" {
		s = append(s,
			filepath.Join(home, ".colima", "default", "docker.sock"),
			filepath.Join(home, ".colima", "docker.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".docker", "run", "docker.sock"), // Docker Desktop (macOS)
			filepath.Join(home, ".docker", "desktop", "docker.sock"),
			filepath.Join(home, ".rd", "docker.sock"),
		)
	}
	s = append(s, "/var/run/docker.sock")
	if runtimeDir != "" {
		s = append(s, filepath.Join(runtimeDir, "docker.sock")) // rootless Docker
	}
	return s
}

// Find returns the docker CLI: the first match on pathEnv, then the
// well-known install locations. "" when Docker is not installed.
func Find(pathEnv, home string) string {
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" || !filepath.IsAbs(d) {
			continue // a relative PATH entry depends on the cwd; never trust it here
		}
		if p := filepath.Join(d, "docker"); isExecutable(p) {
			return p
		}
	}
	for _, d := range binDirs(home) {
		if p := filepath.Join(d, "docker"); isExecutable(p) {
			return p
		}
	}
	return ""
}

// FindHere is Find with this process's PATH and home directory, then the
// user's login shell (a daemon under launchd or systemd does not see what
// ~/.zprofile adds to PATH). The login-shell answer is cached.
func FindHere() string {
	home, _ := os.UserHomeDir()
	if p := Find(os.Getenv("PATH"), home); p != "" {
		return p
	}
	loginOnce.Do(func() { loginDocker = fromLoginShell(os.Getenv("SHELL")) })
	return loginDocker
}

var (
	loginOnce   sync.Once
	loginDocker string
)

// fromLoginShell asks the login shell where docker is ("" if nowhere).
func fromLoginShell(shell string) string {
	if shell == "" || !filepath.IsAbs(shell) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lc", "command -v docker")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) || !isExecutable(p) {
		return ""
	}
	return p
}

// Endpoint picks the Docker endpoint: $DOCKER_HOST wins; then the CLI's
// current context (contextHost, from `docker context inspect`) unless it is
// a unix socket that does not exist; then the first existing well-known
// socket (Colima, OrbStack, Docker Desktop, Rancher, system, rootless).
// exists reports whether a socket path exists (nil: the real filesystem).
func Endpoint(dockerHost, contextHost, home, runtimeDir string, exists func(string) bool) string {
	if dockerHost != "" {
		return dockerHost
	}
	if exists == nil {
		exists = socketExists
	}
	if contextHost != "" {
		sock, isUnix := strings.CutPrefix(contextHost, "unix://")
		if !isUnix || exists(sock) {
			return contextHost
		}
	}
	for _, s := range sockets(home, runtimeDir) {
		if exists(s) {
			return "unix://" + s
		}
	}
	if contextHost != "" {
		return contextHost
	}
	return DefaultHost
}

// Override returns the endpoint to pass to the CLI with --host, or "" when
// the CLI already resolves the same endpoint by itself.
func Override(dockerHost, contextHost, home, runtimeDir string, exists func(string) bool) string {
	ep := Endpoint(dockerHost, contextHost, home, runtimeDir, exists)
	own := dockerHost
	if own == "" {
		own = contextHost
	}
	if own == "" {
		own = DefaultHost
	}
	if ep == own {
		return ""
	}
	return ep
}

// ContextHost asks the CLI for its current context's endpoint (Colima,
// OrbStack and Docker Desktop each install a context). env is the CLI's
// environment (nil: this process's). "" when it cannot tell.
func ContextHost(ctx context.Context, cli string, env []string) string {
	if cli == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func socketExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
