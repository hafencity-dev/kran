package execenv

import (
	"os"
	"os/user"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The job container's environment beyond the workspace (SPEC §6.8): the
// origin's toolchain versions, a user entry for the job's uid, the origin's
// home path and time zone. kran-init (runner-init.sh) applies it as root
// before it drops to the job's uid.

// Paths inside the runner image (see runner.Dockerfile).
const (
	ctrInit     = "/usr/local/lib/kran/init"
	ctrShims    = "/usr/local/lib/kran/shims"
	ctrMiseConf = "/usr/local/lib/kran/mise.toml"
	ctrKran     = "/usr/local/lib/kran/kran" // the node's Linux kran binary
	// The Docker host's socket in a job container; the job's own
	// /var/run/docker.sock is kran's proxy in front of it (dockerproxy.go).
	ctrDockerHost = "/run/kran/docker-host.sock"
	// The job's /tmp (and TMPDIR): a volume of its own, removed with its
	// container, so the proxy can map bind mounts of it.
	ctrTmp = "/tmp"
)

// ctrToolBins lists the binaries mise provides per toolchain key; a job
// gets a shim for each, which installs that tool (only) on first use.
var ctrToolBins = map[string][]string{
	"go":     {"go", "gofmt"},
	"node":   {"node", "npm", "npx", "corepack"},
	"python": {"python", "python3", "pip", "pip3"},
	"pnpm":   {"pnpm"},
	"yarn":   {"yarn"},
	"bun":    {"bun", "bunx"},
	"deno":   {"deno"},
	"rust":   {"cargo", "rustc", "rustup", "rustdoc", "rustfmt", "cargo-fmt", "cargo-clippy", "clippy-driver"},
	"uv":     {"uv", "uvx"},
	"npm":    {"npm", "npx"},
}

var (
	ctrToolKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	ctrVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// toolSet is how a container job gets its toolchains.
type toolSet struct {
	versions map[string]string // mise's global config: tool → version
	order    []string          // keys of versions, in config order
	shims    []string          // binaries to shim
	eager    []string          // tool@version for `mise exec` (no shims known)
}

// containerToolchains resolves a job's tools: every tool the origin has
// at the origin's version, unless the workspace pins it (SPEC §6.8). A
// pin the origin's version satisfies keeps the origin's exact version.
func containerToolchains(origin, pins map[string]string) toolSet {
	t := toolSet{versions: map[string]string{}}
	for k, v := range origin {
		// npm comes with node; a separate one would shadow node's.
		if k == "mise" || k == "npm" || ctrToolBins[k] == nil {
			continue
		}
		if v = originToolVersion(k, v, ""); ctrVersionRe.MatchString(v) {
			t.versions[k] = v
		}
	}
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pin := pins[k]
		if k == "mise" || !ctrToolKeyRe.MatchString(k) {
			continue
		}
		ver := miseVersion(pin)
		if have, ok := origin[k]; ok && satisfies(have, pin) {
			ver = originToolVersion(k, have, pin)
		}
		if !ctrVersionRe.MatchString(ver) {
			continue
		}
		if ctrToolBins[k] == nil {
			t.eager = append(t.eager, k+"@"+ver)
			continue
		}
		t.versions[k] = ver
	}
	for k := range t.versions {
		t.order = append(t.order, k)
	}
	// A pinned npm comes first so its binaries win over node's.
	sort.Slice(t.order, func(i, j int) bool {
		a, b := t.order[i], t.order[j]
		if (a == "npm") != (b == "npm") {
			return a == "npm"
		}
		return a < b
	})
	seen := map[string]bool{}
	for _, k := range t.order {
		bins := ctrToolBins[k]
		if k == "python" {
			if v, n, ok := parseVer(t.versions[k]); ok && n >= 2 {
				mm := strconv.Itoa(v[0]) + "." + strconv.Itoa(v[1])
				bins = append(append([]string(nil), bins...), "python"+mm, "pip"+mm)
			}
		}
		for _, b := range bins {
			if !seen[b] {
				seen[b] = true
				t.shims = append(t.shims, b)
			}
		}
	}
	return t
}

// originToolVersion is the version a container installs for the origin's
// version have of tool. Python keeps the minor version unless the
// workspace pins an exact one: distribution pythons are older patch
// releases than mise can verify, and patch releases are compatible.
func originToolVersion(tool, have, pin string) string {
	if tool != "python" {
		return have
	}
	if _, n, ok := parseVer(pin); ok && n >= 3 && !strings.ContainsAny(pin, "<>^~=*| ") {
		return have
	}
	if v, n, ok := parseVer(have); ok && n >= 2 {
		return strconv.Itoa(v[0]) + "." + strconv.Itoa(v[1])
	}
	return have
}

// initEnv renders t for kran-init.
func (t toolSet) initEnv() (tools, shims string) {
	parts := make([]string, 0, len(t.order))
	for _, k := range t.order {
		parts = append(parts, k+"="+t.versions[k])
	}
	return strings.Join(parts, " "), strings.Join(t.shims, " ")
}

// ctrHomeFor picks the job's $HOME: the origin's home directory when the
// node's persistent container home can be mounted there (so ~ paths and
// workspace paths relate as on the origin), else /kran/cache/home.
func ctrHomeFor(root, originHome string, canMount bool) (home string, mount bool) {
	if originHome == "" || !path.IsAbs(originHome) || strings.ContainsAny(originHome, ":") {
		return ctrHome, false
	}
	h := path.Clean(originHome)
	switch {
	case h == root:
		return root, false // the workspace is the home directory
	case !canMount || reservedMount(h) || strings.HasPrefix(h, root+"/"):
		return ctrHome, false
	}
	return h, true
}

// ctrUser is the job's user name in the container: the origin's, so code
// that looks up the user finds the same name as locally.
func ctrUser(oe *proto.OriginEnv, fallback string) string {
	if oe != nil && validCtrName(oe.User) {
		return oe.User
	}
	if validCtrName(fallback) {
		return fallback
	}
	return "kran"
}

func validCtrName(s string) bool {
	if s == "" || s == "root" || len(s) > 64 || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// nodeGroupName is the name of this node's primary group (the container's
// gid), "" when unknown.
var nodeGroupName = sync.OnceValue(func() string {
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil || !validCtrName(g.Name) {
		return ""
	}
	return g.Name
})

// ctrDropEnv reports origin variables that would point a container job's
// Docker CLI at the origin's files: the job's Docker is the mounted socket.
func ctrDropEnv(k, v string) bool {
	switch k {
	case "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH":
		return true
	case "DOCKER_HOST":
		return strings.HasPrefix(v, "unix://") || strings.HasPrefix(v, "npipe://")
	}
	return false
}

// ctrShmSize sizes /dev/shm: Docker's 64 MiB default crashes browsers and
// parallel test runners. Pages there count against the memory cap.
func ctrShmSize(memMax int64) int64 {
	if memMax <= 0 {
		return 2 * gib
	}
	return max(memMax/2, 64<<20)
}

// ctrPath is the job's PATH: tool shims first, then the user's own bin
// directories (go install, pip --user, cargo install), then the system.
func ctrPath(home string) string {
	dirs := []string{ctrShims}
	if home != "/" {
		dirs = append(dirs, home+"/.local/bin", home+"/go/bin")
	}
	dirs = append(dirs, ctrCache+"/cargo/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	return strings.Join(dirs, ":")
}
