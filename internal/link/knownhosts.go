package link

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoHostKey means known_hosts has no entry for the host.
var ErrNoHostKey = errors.New("no host key in known_hosts")

// HostKeyFromKnownHosts returns "type base64" for host:port from the user's
// known_hosts (hashed entries included), preferring ed25519.
func HostKeyFromKnownHosts(host string, port int) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var firstErr error
	for _, f := range []string{filepath.Join(h, ".ssh", "known_hosts"), "/etc/ssh/ssh_known_hosts"} {
		k, err := hostKeyFromFile(f, host, port)
		if err == nil {
			return k, nil
		}
		if !errors.Is(err, ErrNoHostKey) && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", fmt.Errorf("%w for %s", ErrNoHostKey, host)
}

func knownHostsNames(host string, port int) []string {
	host = strings.ToLower(host)
	if port == 0 || port == 22 {
		return []string{host, fmt.Sprintf("[%s]:22", host)}
	}
	return []string{fmt.Sprintf("[%s]:%d", host, port)}
}

func hostMatches(field string, names []string) bool {
	for _, pat := range strings.Split(field, ",") {
		pat = strings.TrimSpace(pat)
		if strings.HasPrefix(pat, "!") {
			continue
		}
		if strings.HasPrefix(pat, "|1|") {
			p := strings.Split(pat, "|") // "", "1", salt, hash
			if len(p) != 4 {
				continue
			}
			salt, err1 := base64.StdEncoding.DecodeString(p[2])
			want, err2 := base64.StdEncoding.DecodeString(p[3])
			if err1 != nil || err2 != nil {
				continue
			}
			for _, n := range names {
				m := hmac.New(sha1.New, salt)
				m.Write([]byte(n))
				if hmac.Equal(m.Sum(nil), want) {
					return true
				}
			}
			continue
		}
		for _, n := range names {
			if strings.EqualFold(pat, n) {
				return true
			}
		}
	}
	return false
}

func hostKeyFromFile(path, host string, port int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	names := knownHostsNames(host, port)
	var best string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") {
			continue // marker lines (@cert-authority, @revoked) are not pins
		}
		if !hostMatches(fields[0], names) || !isKeyType(fields[1]) {
			continue
		}
		k := fields[1] + " " + fields[2]
		if fields[1] == "ssh-ed25519" {
			return k, nil
		}
		if best == "" {
			best = k
		}
	}
	if best != "" {
		return best, sc.Err()
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", ErrNoHostKey
}
