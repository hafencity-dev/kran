package link

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/config"
)

func hashedHost(name string) string {
	salt := []byte("0123456789abcdefghij")
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(name))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestHostKeyFromKnownHosts(t *testing.T) {
	kh := strings.Join([]string{
		"# comment",
		"devmac,100.90.1.2 ssh-rsa RSAKEY",
		"devmac,100.90.1.2 ssh-ed25519 EDKEY",
		"[nas.local]:2222 ssh-ed25519 NASKEY",
		hashedHost("hashed.example") + " ssh-ed25519 HASHEDKEY",
		hashedHost("[hp.example]:2200") + " ssh-ed25519 HPKEY",
		"@cert-authority *.corp ssh-ed25519 CAKEY",
		"@revoked old.example ssh-ed25519 REVOKED",
		"rsaonly ssh-rsa RSAONLY",
		"*.wild ssh-ed25519 WILD",
	}, "\n") + "\n"
	f := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(f, []byte(kh), 0o600)
	cases := []struct {
		host string
		port int
		want string
		err  bool
	}{
		{"devmac", 22, "ssh-ed25519 EDKEY", false}, // prefers ed25519
		{"DEVMAC", 0, "ssh-ed25519 EDKEY", false},
		{"nas.local", 2222, "ssh-ed25519 NASKEY", false},
		{"nas.local", 22, "", true}, // port matters
		{"hashed.example", 22, "ssh-ed25519 HASHEDKEY", false},
		{"hp.example", 2200, "ssh-ed25519 HPKEY", false},
		{"rsaonly", 22, "ssh-rsa RSAONLY", false},
		{"old.example", 22, "", true},
		{"x.corp", 22, "", true},
		{"unknown", 22, "", true},
	}
	for _, c := range cases {
		got, err := hostKeyFromFile(f, c.host, c.port)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%s:%d got %q err=%v, want %q err=%v", c.host, c.port, got, err, c.want, c.err)
		}
	}
}

func TestHostKeyFromKnownHostsHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	os.MkdirAll(filepath.Join(h, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(h, ".ssh", "known_hosts"), []byte("devmac ssh-ed25519 K\n"), 0o600)
	if k, err := HostKeyFromKnownHosts("devmac", 22); err != nil || k != "ssh-ed25519 K" {
		t.Fatal(k, err)
	}
	if _, err := HostKeyFromKnownHosts("nope", 22); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseSSHG(t *testing.T) {
	g := parseSSHG("user acme\nhostname 192.168.1.20\nport 2200\nidentityfile ~/.ssh/id\nhostkeyalias none\n")
	if g.User != "acme" || g.Hostname != "192.168.1.20" || g.Port != 2200 || g.HostKeyAlias != "none" {
		t.Fatalf("%+v", g)
	}
	if g := parseSSHG(""); g.Port != 22 {
		t.Fatal("default port")
	}
}

func TestPlatform(t *testing.T) {
	ok := map[string][2]string{
		"Darwin arm64":  {"darwin", "arm64"},
		"Linux aarch64": {"linux", "arm64"},
		"Linux x86_64":  {"linux", "amd64"},
		"Darwin x86_64": {"darwin", "amd64"},
	}
	for in, w := range ok {
		o, a, err := platform(in)
		if err != nil || o != w[0] || a != w[1] {
			t.Errorf("%s: %s %s %v", in, o, a, err)
		}
	}
	for _, bad := range []string{"", "Linux", "FreeBSD amd64", "Linux armv7l", "MINGW64_NT-10.0 x86_64"} {
		if _, _, err := platform(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if _, _, err := platform("Windows_NT x86_64"); err == nil || !strings.Contains(err.Error(), "linux and macOS") {
		t.Fatal(err)
	}
}

func TestVersions(t *testing.T) {
	cases := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"1.2.3", "1.2.3", 0, true}, {"1.2.3", "1.10.0", -1, true}, {"v2.0.0", "1.9.9", 1, true},
		{"0.3.1-5-gabc", "0.3.1", 0, true}, {"dev", "1.0.0", 0, false}, {"1.0", "1.0.0", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if got != c.cmp || ok != c.ok {
			t.Errorf("%s vs %s: %d %v", c.a, c.b, got, ok)
		}
	}
	if !isDevVersion("dev") || !isDevVersion("0.3.1-5-gabc") || !isDevVersion("0.3.1-dirty") || isDevVersion("0.3.1") || isDevVersion("v1.0.0") {
		t.Fatal("isDevVersion")
	}
}

func TestRemoteWord(t *testing.T) {
	for in, want := range map[string]string{
		"~":              `"$HOME"`,
		"~/kran-dev/e":   `"$HOME"/'kran-dev/e'`,
		"/opt/it's/kran": `'/opt/it'\''s/kran'`,
	} {
		if got := remoteWord(in); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
}

const sshConfig = `
# global
Host *
    ServerAliveInterval 30

Host devmac mac-studio
    HostName 192.168.1.20
    User acme

Host *.internal !bad
    User x

Include conf.d/*.conf

Match host foo
    User nope

Host gx10
    HostName gx10.local
`

const tailscaleJSON = `{"Self":{"HostName":"gx10","OS":"linux","Online":true},
"Peer":{
 "nodekey:1":{"HostName":"devmac","DNSName":"devmac.tail1234.ts.net.","OS":"macOS","Online":true,"TailscaleIPs":["100.64.0.2"]},
 "nodekey:2":{"HostName":"nas","DNSName":"nas.tail1234.ts.net.","OS":"linux","Online":false,"TailscaleIPs":["100.90.1.9"]},
 "nodekey:3":{"HostName":"phone","DNSName":"phone.tail1234.ts.net.","OS":"iOS","Online":true,"TailscaleIPs":["100.90.1.10"]}
}}`

type scripted struct {
	fn func(Cmd) (Output, error)
}

func (s scripted) Run(_ context.Context, c Cmd) (Output, error) { return s.fn(c) }

func TestReadSSHConfigHosts(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh", "conf.d"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(sshConfig), 0o600)
	os.WriteFile(filepath.Join(home, ".ssh", "conf.d", "work.conf"), []byte("Host build-box\n  HostName b.example.com\n  User ci\n"), 0o600)
	hs := readSSHConfigHosts(filepath.Join(home, ".ssh", "config"), home, 0)
	var names []string
	for _, h := range hs {
		names = append(names, h.Alias)
	}
	if got := strings.Join(names, ","); got != "devmac,mac-studio,build-box,gx10" {
		t.Fatalf("got %s", got)
	}
	if hs[0].HostName != "192.168.1.20" || hs[0].User != "acme" || hs[2].User != "ci" {
		t.Fatalf("%+v", hs)
	}
}

func TestCandidates(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(sshConfig), 0o600)
	os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte("legacy.example ssh-ed25519 K\n192.168.1.1 ssh-ed25519 K\n"+hashedHost("secret")+" ssh-ed25519 K\n[nas.tail1234.ts.net]:22 ssh-ed25519 K\n"), 0o600)
	r := scripted{func(c Cmd) (Output, error) {
		if c.Name == "tailscale" {
			return Output{Stdout: []byte(tailscaleJSON)}, nil
		}
		return Output{}, fmt.Errorf("unexpected %s", c.Name)
	}}
	peers := &config.Peers{Peer: []config.Peer{{Name: "nas", Addresses: []string{"nas.tail1234.ts.net:22"}}}}
	got := candidates(context.Background(), r, home, peers, "GX10.fritz.box")
	byHost := map[string]Candidate{}
	var order []string
	for _, c := range got {
		byHost[c.Host] = c
		order = append(order, c.Host)
	}
	if _, ok := byHost["gx10"]; ok {
		t.Fatal("must skip this machine")
	}
	dm := byHost["devmac"] // ssh alias wins over tailscale DNS name and merges details
	if dm.Source != "ssh-config" || dm.Online == nil || !*dm.Online || !strings.Contains(dm.Detail, "macOS") {
		t.Fatalf("%+v", dm)
	}
	if _, ok := byHost["devmac.tail1234.ts.net"]; ok {
		t.Fatal("tailscale duplicate not merged")
	}
	if !byHost["nas.tail1234.ts.net"].Linked && !byHost["nas"].Linked {
		t.Fatalf("nas should be marked linked: %+v", got)
	}
	if _, ok := byHost["legacy.example"]; !ok {
		t.Fatal("known_hosts name missing")
	}
	for _, c := range got {
		if strings.HasPrefix(c.Host, "|") || c.Host == "192.168.1.1" || c.Host == "secret" {
			t.Fatalf("leaked %+v", c)
		}
	}
	// online first, linked last
	if order[0] != "devmac" && order[0] != "phone.tail1234.ts.net" {
		t.Fatalf("order %v", order)
	}
	if !got[len(got)-1].Linked {
		t.Fatalf("linked machines should sort last: %v", order)
	}
}
