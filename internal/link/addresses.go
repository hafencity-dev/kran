package link

import (
	"bufio"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

var cgnat = mustCIDR("100.64.0.0/10") // Tailscale

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// virtualIface reports interface names that are never useful peer addresses.
func virtualIface(name string) bool {
	for _, p := range []string{"lo", "docker", "veth", "br-", "virbr", "cni", "flannel", "lxc", "lxd", "vmnet", "anpi", "awdl", "llw", "bridge", "gif", "stf", "ap", "podman", "tun0"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isPrivateLAN(ip net.IP) bool {
	return ip.IsPrivate() && !ip.IsLoopback()
}

// localAddresses lists host:port candidates other machines can use to reach
// this one over ssh: Tailscale IPs first (stable across networks), then LAN
// addresses, then hostname.local and the bare hostname.
func localAddresses() []string {
	port := sshdPort()
	hp := func(h string) string { return net.JoinHostPort(h, strconv.Itoa(port)) }
	var ts, lan []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			switch {
			case cgnat.Contains(ip4):
				ts = append(ts, hp(ip4.String()))
			case !virtualIface(ifc.Name) && isPrivateLAN(ip4):
				lan = append(lan, hp(ip4.String()))
			}
		}
	}
	out := append(ts, lan...)
	if h, err := os.Hostname(); err == nil && h != "" {
		short := strings.ToLower(h)
		if i := strings.IndexByte(short, '.'); i > 0 {
			short = short[:i]
		}
		out = append(out, hp(short+".local"))
		if strings.Contains(h, ".") {
			out = append(out, hp(strings.ToLower(h)))
		}
	}
	return dedupe(out)
}

// sshdPort reads Port from /etc/ssh/sshd_config (default 22).
func sshdPort() int {
	f, err := os.Open("/etc/ssh/sshd_config")
	if err != nil {
		return 22
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) == 2 && strings.EqualFold(fl[0], "port") {
			if n, err := strconv.Atoi(fl[1]); err == nil && n > 0 && n < 65536 {
				return n
			}
		}
	}
	return 22
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// localHostKey returns one of this machine's sshd host keys ("type
// base64"), or "" when none can be read. KRAN_SSHD_HOSTKEY overrides the
// file. The peer pins it, and learns it over the ssh channel the user's
// known_hosts already verified, so every key type counts.
func localHostKey() string {
	if f := os.Getenv("KRAN_SSHD_HOSTKEY"); f != "" {
		return hostKeyFromFiles(f)
	}
	return hostKeyFrom("/etc/ssh")
}

func hostKeyFrom(dir string) string {
	var files []string
	for _, t := range []string{"ed25519", "ecdsa", "rsa"} {
		files = append(files, filepath.Join(dir, "ssh_host_"+t+"_key.pub"))
	}
	return hostKeyFromFiles(files...)
}

func hostKeyFromFiles(files ...string) string {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fl := strings.Fields(string(b))
		if len(fl) >= 2 && isKeyType(fl[0]) {
			return fl[0] + " " + fl[1]
		}
	}
	return ""
}
