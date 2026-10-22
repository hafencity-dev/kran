package link

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sshResolved is what the user's ssh config says about a destination
// (`ssh -G <dest>`).
type sshResolved struct {
	User, Hostname, HostKeyAlias string
	Port                         int
}

func parseSSHG(out string) sshResolved {
	r := sshResolved{Port: 22}
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), " ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "user":
			r.User = v
		case "hostname":
			r.Hostname = v
		case "port":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				r.Port = n
			}
		case "hostkeyalias":
			r.HostKeyAlias = v
		}
	}
	return r
}

// sshHost is a concrete (non-wildcard) Host entry of ~/.ssh/config.
type sshHost struct {
	Alias, HostName, User string
}

// readSSHConfigHosts parses an ssh config (following Include) and returns
// concrete Host aliases. Patterns with * ? or ! are skipped.
func readSSHConfigHosts(file, home string, depth int) []sshHost {
	if depth > 5 {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sshHost
	var cur []int // indexes into out for the current Host line
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		key, val := splitKV(ln)
		switch strings.ToLower(key) {
		case "include":
			// Include inside a Host block is rare; treat it as global.
			for _, pat := range strings.Fields(val) {
				pat = strings.Trim(pat, `"`)
				if strings.HasPrefix(pat, "~/") {
					pat = filepath.Join(home, pat[2:])
				} else if !filepath.IsAbs(pat) {
					pat = filepath.Join(home, ".ssh", pat)
				}
				matches, _ := filepath.Glob(pat)
				for _, m := range matches {
					out = append(out, readSSHConfigHosts(m, home, depth+1)...)
				}
			}
		case "host":
			cur = cur[:0]
			for _, a := range strings.Fields(val) {
				a = strings.Trim(a, `"`)
				if strings.ContainsAny(a, "*?!") {
					continue
				}
				out = append(out, sshHost{Alias: a})
				cur = append(cur, len(out)-1)
			}
		case "match":
			cur = cur[:0]
		case "hostname":
			for _, i := range cur {
				out[i].HostName = val
			}
		case "user":
			for _, i := range cur {
				out[i].User = val
			}
		}
	}
	return out
}

func splitKV(ln string) (string, string) {
	if i := strings.IndexAny(ln, " \t="); i > 0 {
		return ln[:i], strings.Trim(strings.TrimLeft(ln[i:], " \t="), `"`)
	}
	return ln, ""
}
