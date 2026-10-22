package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Proc is one process in the ancestry walk.
type Proc struct {
	PID  int
	PPID int
	Comm string   // executable path or name as the OS reports it
	Args []string // argv when known (may be nil)
}

// ProcReader looks up a process by pid (/proc on Linux, ps on macOS).
type ProcReader interface {
	Proc(pid int) (Proc, error)
}

// agentRules map an executable base name to an owner kind (SPEC §6.4).
var agentRules = map[string]proto.OwnerKind{
	"claude": proto.OwnerClaude, "codex": proto.OwnerCodex,
	"2code": proto.OwnerAgent, "cursor": proto.OwnerAgent, "code": proto.OwnerAgent,
	"gemini": proto.OwnerAgent, "aider": proto.OwnerAgent, "opencode": proto.OwnerAgent, "windsurf": proto.OwnerAgent,
}

// appPathRules catch Electron apps whose executable is not named after them.
var appPathRules = []struct {
	contains string
	kind     proto.OwnerKind
	name     string
}{
	{"/Visual Studio Code.app/", proto.OwnerAgent, "code"},
	{"/Cursor.app/", proto.OwnerAgent, "cursor"},
	{"/Claude.app/", proto.OwnerClaude, "claude"},
}

var interpreters = map[string]bool{"node": true, "bun": true, "deno": true, "python": true, "python3": true}

func baseName(s string) string {
	b := strings.ToLower(filepath.Base(s))
	for _, ext := range []string{".exe", ".js", ".mjs", ".cjs", ".py"} {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}

// matchAgent reports whether p is a coding agent.
func matchAgent(p Proc) (proto.OwnerKind, string, bool) {
	for _, r := range appPathRules {
		if strings.Contains(p.Comm, r.contains) {
			return r.kind, r.name, true
		}
	}
	names := []string{baseName(p.Comm)}
	if len(p.Args) > 0 {
		names = append(names, baseName(p.Args[0]))
		if interpreters[baseName(p.Args[0])] || interpreters[baseName(p.Comm)] {
			for _, a := range p.Args[1:] {
				if strings.HasPrefix(a, "-") {
					continue
				}
				names = append(names, baseName(a))
				break
			}
		}
	}
	for _, n := range names {
		if k, ok := agentRules[n]; ok {
			return k, n, true
		}
	}
	return "", "", false
}

// OwnerInput is everything DetectOwner looks at.
type OwnerInput struct {
	Env   map[string]string
	PID   int // pid whose ancestry is walked (os.Getpid())
	User  string
	TTY   string // short tty name, "" when none
	Procs ProcReader
}

// DetectOwner implements SPEC §6.4: explicit KRAN_OWNER, then the nearest agent
// ancestor (session id from env when present), then agent session env vars,
// then CI, else a human at a tty.
func DetectOwner(in OwnerInput) proto.Owner {
	if v := in.Env["KRAN_OWNER"]; v != "" {
		return parseOwnerEnv(v)
	}
	var kind proto.OwnerKind
	var name string
	var pid int
	if in.Procs != nil {
		cur := in.PID
		for depth := 0; depth < 64 && cur > 1; depth++ {
			p, err := in.Procs.Proc(cur)
			if err != nil {
				break
			}
			if cur != in.PID {
				if k, n, ok := matchAgent(p); ok {
					kind, name, pid = k, n, p.PID
					break
				}
			}
			if p.PPID == cur {
				break
			}
			cur = p.PPID
		}
	}
	sessionEnv := func(k proto.OwnerKind) string {
		switch k {
		case proto.OwnerClaude:
			return in.Env["CLAUDE_SESSION_ID"]
		case proto.OwnerCodex:
			return in.Env["CODEX_SESSION_ID"]
		}
		return ""
	}
	if kind == "" {
		switch {
		case in.Env["CLAUDE_SESSION_ID"] != "" || in.Env["CLAUDECODE"] == "1":
			kind, name = proto.OwnerClaude, "claude"
		case in.Env["CODEX_SESSION_ID"] != "":
			kind, name = proto.OwnerCodex, "codex"
		}
	}
	if kind != "" {
		id := sessionEnv(kind)
		if id == "" && pid > 0 {
			id = strconv.Itoa(pid)
		}
		if id == "" {
			id = name + "@" + in.User
		}
		return proto.Owner{Kind: kind, ID: id, Name: fmt.Sprintf("%s · %s-%s", name, in.User, shortID(id))}
	}
	if in.Env["CI"] != "" && in.Env["CI"] != "false" && in.Env["CI"] != "0" {
		return proto.Owner{Kind: proto.OwnerCI, ID: "ci", Name: "ci"}
	}
	tty := in.TTY
	if tty == "" {
		tty = "-"
	}
	n := in.User + "@" + tty
	return proto.Owner{Kind: proto.OwnerHuman, ID: n, Name: n}
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// parseOwnerEnv accepts "kind:name" (kind one of the known kinds) or "name".
func parseOwnerEnv(v string) proto.Owner {
	if i := strings.IndexByte(v, ':'); i > 0 {
		switch k := proto.OwnerKind(v[:i]); k {
		case proto.OwnerHuman, proto.OwnerClaude, proto.OwnerCodex, proto.OwnerAgent, proto.OwnerCI:
			return proto.Owner{Kind: k, ID: v[i+1:], Name: v[i+1:]}
		}
	}
	return proto.Owner{Kind: proto.OwnerAgent, ID: v, Name: v}
}
