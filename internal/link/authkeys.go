package link

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/paths"
)

// AuthorizedKeysLine renders the forced-command entry for a kran public key:
//
//	command="<kranPath> serve --stdio",restrict ssh-ed25519 AAAA… kran@<node>[ tag]
//
// pubKey may be a full "type blob comment" line. The key can then do nothing
// except speak the kran protocol (SPEC §3).
func AuthorizedKeysLine(kranPath, pubKey, tag string) string {
	f := strings.Fields(pubKey)
	if len(f) < 2 {
		return ""
	}
	comment := strings.Join(f[2:], " ")
	if comment == "" {
		comment = "kran"
	}
	if tag != "" && !strings.Contains(" "+comment+" ", " "+tag+" ") {
		comment += " " + tag
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(kranPath)
	return fmt.Sprintf(`command="%s serve --stdio",restrict %s %s %s`, esc, f[0], f[1], comment)
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
