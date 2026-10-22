package link

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/transport"
)

// AuthorizedKeysLine renders the forced-command entry for a kran public key:
//
//	command="<kranPath> serve --stdio",restrict ssh-ed25519 AAAA… kran@<node>[ tag]
//
// pubKey may be a full "type blob comment" line. The key can then do nothing
// except speak the kran protocol (SPEC §3).
func AuthorizedKeysLine(kranPath, pubKey, tag string) string {
	return keyLine(kranPath, "", pubKey, tag, nil)
}

// PeerKeyLine is AuthorizedKeysLine for a key linked as peer: the forced
// command becomes `<kranPath> serve --stdio --peer <peer>`, so the daemon
// binds the session to that linked name instead of trusting the hello.
func PeerKeyLine(kranPath, peer, pubKey, tag string) string {
	if !transport.ValidPeerName(peer) {
		return ""
	}
	return keyLine(kranPath, peer, pubKey, tag, nil)
}

// installEnv are the variables that place a kran install (KRAN_*_DIR and
// the resource namespace). A development install next to a real one sets
// them; its forced command must carry them, or `kran serve --stdio` would
// reach (and autostart) the default install's daemon instead.
var installEnv = []string{"KRAN_CONFIG_DIR", "KRAN_STATE_DIR", "KRAN_RUNTIME_DIR", "KRAN_CACHE_DIR", "KRAN_NAMESPACE"}

// InstallEnv returns the install-placing variables set in this process
// (nil for a normal install).
func InstallEnv() map[string]string {
	var m map[string]string
	for _, k := range installEnv {
		if v := os.Getenv(k); v != "" {
			if m == nil {
				m = map[string]string{}
			}
			m[k] = v
		}
	}
	return m
}

// PeerKeyLineEnv is PeerKeyLine for an install placed by env (see
// InstallEnv): the forced command runs `env K='v' … <kran> serve --stdio …`.
func PeerKeyLineEnv(kranPath, peer, pubKey, tag string, env map[string]string) string {
	if !transport.ValidPeerName(peer) {
		return ""
	}
	return keyLine(kranPath, peer, pubKey, tag, env)
}

func keyLine(kranPath, peer, pubKey, tag string, env map[string]string) string {
	f := strings.Fields(pubKey)
	if len(f) < 2 || hasControl(kranPath+tag+strings.Join(f, " ")) { // a newline would start another key line
		return ""
	}
	comment := strings.Join(f[2:], " ")
	if comment == "" {
		comment = "kran"
	}
	if tag != "" && !strings.Contains(" "+comment+" ", " "+tag+" ") {
		comment += " " + tag
	}
	// sshd runs the command with the user's shell and only undoes \" (so
	// a backslash stays as is); the path must be one word for that shell.
	cmd := kranPath
	if !plainPath.MatchString(cmd) {
		cmd = shq(cmd)
	}
	if len(env) > 0 {
		var b strings.Builder
		b.WriteString("env")
		for _, k := range installEnv {
			if v, ok := env[k]; ok {
				if hasControl(v) {
					return ""
				}
				fmt.Fprintf(&b, " %s=%s", k, shq(v))
			}
		}
		cmd = b.String() + " " + cmd
	}
	cmd += " serve --stdio"
	if peer != "" {
		cmd += " --peer " + peer
	}
	esc := strings.ReplaceAll(cmd, `"`, `\"`)
	return fmt.Sprintf(`command="%s",restrict %s %s %s`, esc, f[0], f[1], comment)
}

var plainPath = regexp.MustCompile(`^[A-Za-z0-9_./+:@,=-]+$`)

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// authLine is a parsed authorized_keys line.
type authLine struct {
	opts, keyType, blob, comment string
}

func isKeyType(s string) bool {
	return strings.HasPrefix(s, "ssh-") || strings.HasPrefix(s, "ecdsa-") || strings.HasPrefix(s, "sk-")
}

// splitFieldQuoted splits off the first whitespace-delimited token, honoring
// double quotes (authorized_keys options may contain spaces).
func splitFieldQuoted(s string) (tok, rest string) {
	s = strings.TrimLeft(s, " \t")
	inQ := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && inQ && i+1 < len(s):
			i++
		case c == '"':
			inQ = !inQ
		case (c == ' ' || c == '\t') && !inQ:
			return s[:i], strings.TrimLeft(s[i:], " \t")
		}
	}
	return s, ""
}

func parseAuthLine(line string) (authLine, bool) {
	t := strings.TrimSpace(strings.TrimRight(line, "\r"))
	if t == "" || strings.HasPrefix(t, "#") {
		return authLine{}, false
	}
	var al authLine
	tok, rest := splitFieldQuoted(t)
	if !isKeyType(tok) {
		al.opts = tok
		tok, rest = splitFieldQuoted(rest)
	}
	if !isKeyType(tok) {
		return authLine{}, false
	}
	al.keyType = tok
	blob, rest := splitFieldQuoted(rest)
	if blob == "" {
		return authLine{}, false
	}
	al.blob, al.comment = blob, strings.TrimSpace(rest)
	return al, true
}

// isKranLine reports whether the entry is a kran forced-command entry.
func (a authLine) isKran() bool {
	return strings.Contains(a.opts, `serve --stdio`) && strings.Contains(a.opts, "command=")
}

func authorizedKeysPath() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".ssh", "authorized_keys"), nil
}

func lockName() string { return filepath.Join(paths.ConfigDir(), "authorized_keys") }

// readAuthorized reads path following symlinks; missing = empty.
func readAuthorized(path string) (resolved string, data string, mode os.FileMode, exists bool, err error) {
	resolved = path
	if r, e := filepath.EvalSymlinks(path); e == nil {
		resolved = r
	}
	b, err := os.ReadFile(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return resolved, "", 0o600, false, nil
	}
	if err != nil {
		return resolved, "", 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return resolved, "", 0, false, err
	}
	return resolved, string(b), st.Mode().Perm(), true, nil
}

func writeAuthorized(resolved, data string, mode os.FileMode) error {
	dir := filepath.Dir(resolved)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return config.WriteFileAtomic(resolved, []byte(data), mode)
}

// EnsureAuthorizedKey makes sure line (an AuthorizedKeysLine) is present in
// the authorized_keys file at path: appended when the key blob is absent,
// replacing exactly the existing kran entry when it differs (e.g. the kran
// binary moved), untouched otherwise. Other lines are never modified.
func EnsureAuthorizedKey(path string, line string) (changed bool, err error) {
	want, ok := parseAuthLine(line)
	if !ok {
		return false, fmt.Errorf("invalid authorized_keys line %q", line)
	}
	line = strings.TrimRight(line, "\r\n")
	err = config.WithLock(lockName(), func() error {
		resolved, data, mode, exists, e := readAuthorized(path)
		if e != nil {
			return e
		}
		var out []string
		placed := false
		parts := strings.SplitAfter(data, "\n")
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		for _, p := range parts {
			al, ok := parseAuthLine(p)
			if !ok || al.blob != want.blob {
				out = append(out, p)
				continue
			}
			if !al.isKran() {
				return fmt.Errorf("%s already lists this key without kran's restrictions; remove that line and run kran link again", path)
			}
			if placed {
				changed = true // drop duplicate kran entries
				continue
			}
			placed = true
			if strings.TrimRight(p, "\r\n") != line {
				changed = true
				nl := "\n"
				if strings.HasSuffix(p, "\r\n") {
					nl = "\r\n"
				}
				out = append(out, line+nl)
			} else {
				out = append(out, p)
			}
		}
		if !placed {
			if n := len(out); n > 0 && !strings.HasSuffix(out[n-1], "\n") {
				out[n-1] += "\n"
			}
			out = append(out, line+"\n")
			changed = true
		}
		if !changed && exists {
			return nil
		}
		return writeAuthorized(resolved, strings.Join(out, ""), mode)
	})
	return changed, err
}

// RemoveAuthorizedKey removes kran forced-command entries for the key blob
// (the base64 field). Other lines stay byte-identical.
func RemoveAuthorizedKey(path, keyBlob string) (removed bool, err error) {
	err = config.WithLock(lockName(), func() error {
		resolved, data, mode, exists, e := readAuthorized(path)
		if e != nil || !exists {
			return e
		}
		var out []string
		for _, p := range strings.SplitAfter(data, "\n") {
			if al, ok := parseAuthLine(p); ok && al.blob == keyBlob && al.isKran() {
				removed = true
				continue
			}
			out = append(out, p)
		}
		if !removed {
			return nil
		}
		return writeAuthorized(resolved, strings.Join(out, ""), mode)
	})
	return removed, err
}

// keyBlob returns the base64 field of a public key line.
func keyBlob(pub string) string {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return ""
	}
	return f[1]
}
