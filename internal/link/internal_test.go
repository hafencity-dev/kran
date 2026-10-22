package link

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
)

func internalEnv(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("KRAN_CONFIG_DIR", filepath.Join(h, "cfg"))
	t.Setenv("KRAN_STATE_DIR", filepath.Join(h, "state"))
	t.Setenv("KRAN_RUNTIME_DIR", filepath.Join(h, "run"))
	t.Setenv("KRAN_CACHE_DIR", filepath.Join(h, "cache"))
	t.Setenv("KRAN_SSHD_HOSTKEY", filepath.Join(h, "none"))
}

func runInternal(t *testing.T, in any, args ...string) (map[string]any, int, string) {
	t.Helper()
	var stdin bytes.Buffer
	if in != nil {
		json.NewEncoder(&stdin).Encode(in)
	}
	var out, errb bytes.Buffer
	code := InternalMain(args, &stdin, &out, &errb)
	var m map[string]any
	json.Unmarshal(out.Bytes(), &m)
	return m, code, errb.String()
}

func TestInternalInfo(t *testing.T) {
	internalEnv(t)
	m, code, _ := runInternal(t, nil, "link-info")
	if code != 0 || m["pubkey"] != "" || m["id"] != "" || m["os"] == "" || m["binary_sha256"] == "" || m["config_dir"] != os.Getenv("KRAN_CONFIG_DIR") {
		t.Fatalf("%d %v", code, m)
	}
	config.EnsureNodeID()
	config.EnsureKey("x1")
	m, _, _ = runInternal(t, nil, "link-info")
	if !strings.HasPrefix(m["pubkey"].(string), "ssh-ed25519 ") || len(m["id"].(string)) != 32 {
		t.Fatal(m)
	}
}

func TestInternalUsage(t *testing.T) {
	if _, code, _ := runInternal(t, nil); code != 2 {
		t.Fatal(code)
	}
	if _, code, e := runInternal(t, nil, "bogus"); code != 2 || !strings.Contains(e, "unknown internal command") {
		t.Fatal(code, e)
	}
	internalEnv(t)
	if _, code, e := runInternal(t, nil, "link-accept"); code != 1 || !strings.Contains(e, "read request") {
		t.Fatal(code, e)
	}
}

func TestInternalAcceptForget(t *testing.T) {
	internalEnv(t)
	_, code, _ := runInternal(t, AcceptRequest{Peer: PeerInfo{Name: "BAD NAME", PubKey: keyA}}, "link-accept")
	if code != 1 {
		t.Fatal("invalid name must fail")
	}
	req := AcceptRequest{Tag: "kran-dev", Peer: PeerInfo{Name: "devmac", ID: "abc", PubKey: keyA, Addresses: []string{"h:22"}, User: "u", KranPath: "/k", Direction: "both"}}
	m, code, e := runInternal(t, req, "link-accept")
	if code != 0 || m["changed"] != true || m["pubkey"] == "" {
		t.Fatal(code, e, m)
	}
	home, _ := os.UserHomeDir()
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	exe, _ := selfExe()
	want := PeerKeyLineEnv(exe, "devmac", keyA, "kran-dev", InstallEnv()) + "\n"
	if b, _ := os.ReadFile(ak); string(b) != want {
		t.Fatalf("%q want %q", b, want)
	}
	m, _, _ = runInternal(t, req, "link-accept")
	if m["changed"] != false {
		t.Fatalf("repeat must be a no-op: %v", m["changed"])
	}
	ps, _ := config.LoadPeers("")
	if p, ok := ps.Find("devmac"); !ok || p.Tag != "kran-dev" || p.ID != "abc" {
		t.Fatal(ps)
	}
	// forget by id removes both
	m, code, _ = runInternal(t, ForgetRequest{ID: "abc", PubKey: keyA}, "link-forget")
	if code != 0 || m["removed_key"] != true || m["removed_peer"] != true {
		t.Fatal(m)
	}
	if b, _ := os.ReadFile(ak); len(b) != 0 {
		t.Fatalf("%q", b)
	}
	m, _, _ = runInternal(t, ForgetRequest{ID: "abc", PubKey: keyA}, "link-forget")
	if m["removed_key"] != false || m["removed_peer"] != false {
		t.Fatal(m)
	}
}

func TestProbeAndHandshake(t *testing.T) {
	internalEnv(t)
	pub, _, _ := config.EnsureKey("probe")
	home, _ := os.UserHomeDir()
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	os.MkdirAll(filepath.Dir(ak), 0o700)
	srv := newTestSSHD(t, ak)
	signer, _ := config.Signer()

	// not authorized yet
	r := Handshake(context.Background(), srv.Addr, "u", signer, srv.HostKey, 3*time.Second)
	if r.OK || !strings.Contains(r.Error, "kran key was not accepted") {
		t.Fatalf("%+v", r)
	}
	EnsureAuthorizedKey(ak, AuthorizedKeysLine("/k", pub, ""))
	r = Handshake(context.Background(), srv.Addr, "u", signer, srv.HostKey, 3*time.Second)
	if !r.OK || r.RTTMs <= 0 {
		t.Fatalf("%+v", r)
	}
	// unpinned: refused (a reverse probe without our host key must not
	// report a working direction it could not verify)
	if r := Handshake(context.Background(), srv.Addr, "u", signer, "", 3*time.Second); r.OK || !strings.Contains(r.Error, "host key") {
		t.Fatalf("%+v", r)
	}
	// wrong pinned key is rejected
	other := newTestSSHD(t, ak)
	r = Handshake(context.Background(), srv.Addr, "u", signer, other.HostKey, 3*time.Second)
	if r.OK || !strings.Contains(r.Error, "host key") {
		t.Fatalf("%+v", r)
	}
	// closed port
	if r := Handshake(context.Background(), "127.0.0.1:1", "u", signer, "", time.Second); r.OK || r.Error == "" {
		t.Fatalf("%+v", r)
	}
	// probe subcommand: tries all addresses, reports the fastest working one
	m, code, e := runInternal(t, ProbeRequest{Addresses: []string{"127.0.0.1:1", srv.Addr}, HostKey: srv.HostKey, User: "u"}, "link-probe")
	if code != 0 || m["ok_address"] != srv.Addr {
		t.Fatal(code, e, m)
	}
	if rs := m["results"].([]any); len(rs) != 2 || rs[0].(map[string]any)["ok"] == true {
		t.Fatal(rs)
	}
}

// TestAcceptPinsInstallEnv: a kran install placed with KRAN_*_DIR (and
// KRAN_NAMESPACE) — a development setup next to a real one — must get a
// forced command that reaches that install's daemon, not the default one.
func TestAcceptPinsInstallEnv(t *testing.T) {
	internalEnv(t)
	odd := filepath.Join(t.TempDir(), "it's here")
	t.Setenv("KRAN_STATE_DIR", odd)
	t.Setenv("KRAN_NAMESPACE", "kran-dev")
	req := AcceptRequest{Tag: "kran-dev", Peer: PeerInfo{Name: "devmac", ID: "abc", PubKey: keyA, Addresses: []string{"h:22"}, User: "u", KranPath: "/k", Direction: "both"}}
	if _, code, e := runInternal(t, req, "link-accept"); code != 0 {
		t.Fatal(code, e)
	}
	home, _ := os.UserHomeDir()
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	al, ok := parseAuthLine(string(b))
	if !ok || !al.isKran() {
		t.Fatalf("not a kran line: %q", b)
	}
	cmd := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(al.opts, `",`, 2)[0], `command="`), `"`)
	cmd = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(cmd)
	exe, _ := selfExe()
	// Run what sshd would run, with the kran binary swapped for a probe.
	run := exe
	if !plainPath.MatchString(run) {
		run = shq(run)
	}
	run += " serve --stdio --peer devmac"
	if !strings.Contains(cmd, run) {
		t.Fatalf("forced command %q does not run %s", cmd, run)
	}
	probe := strings.Replace(cmd, run, `sh -c 'echo "$KRAN_CONFIG_DIR|$KRAN_STATE_DIR|$KRAN_RUNTIME_DIR|$KRAN_CACHE_DIR|$KRAN_NAMESPACE"'`, 1)
	sh := exec.Command("sh", "-c", probe)
	sh.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home} // what sshd gives the forced command
	out, err := sh.Output()
	want := strings.Join([]string{os.Getenv("KRAN_CONFIG_DIR"), odd, os.Getenv("KRAN_RUNTIME_DIR"), os.Getenv("KRAN_CACHE_DIR"), "kran-dev"}, "|") + "\n"
	if err != nil || string(out) != want {
		t.Fatalf("forced command %q sees %q (%v), want %q", cmd, out, err, want)
	}
}
