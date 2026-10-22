package link

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hafencity-dev/kran/internal/config"
)

// world is two simulated machines: "local" is this process (HOME and KRAN_*
// set via t.Setenv), "remote" is reached through fakeSSH.
type world struct {
	t          *testing.T
	localHome  string
	remoteHome string
	remoteEnv  map[string]string
	self       string
	runner     *fakeSSH
	opts       Options
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, localHome: t.TempDir(), remoteHome: t.TempDir()}
	t.Setenv("HOME", w.localHome)
	t.Setenv("KRAN_CONFIG_DIR", filepath.Join(w.localHome, "cfg"))
	t.Setenv("KRAN_STATE_DIR", filepath.Join(w.localHome, "state"))
	t.Setenv("KRAN_RUNTIME_DIR", filepath.Join(w.localHome, "run"))
	t.Setenv("KRAN_CACHE_DIR", filepath.Join(w.localHome, "cache"))
	w.remoteEnv = map[string]string{
		"KRAN_CONFIG_DIR":  filepath.Join(w.remoteHome, "cfg"),
		"KRAN_STATE_DIR":   filepath.Join(w.remoteHome, "state"),
		"KRAN_RUNTIME_DIR": filepath.Join(w.remoteHome, "run"),
		"KRAN_CACHE_DIR":   filepath.Join(w.remoteHome, "cache"),
	}
	exe, _ := os.Executable()
	w.self = filepath.Join(t.TempDir(), "self", "kran")
	copyFile(t, exe, w.self)
	w.runner = &fakeSSH{t: t, remoteHome: w.remoteHome}
	w.opts = Options{
		Host: "devbox", Name: "devbox", SelfName: "gx-local", Version: "1.2.3", SelfPath: w.self,
		Tag: "kran-dev", RemoteEnv: w.remoteEnv, RemoteBinDir: filepath.Join(w.remoteHome, "bin"),
		Runner: w.runner, Now: func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) },
	}
	return w
}

func (w *world) remoteAuthKeys() string {
	return filepath.Join(w.remoteHome, ".ssh", "authorized_keys")
}
func (w *world) localAuthKeys() string { return filepath.Join(w.localHome, ".ssh", "authorized_keys") }

func (w *world) readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func (w *world) remotePeers() *config.Peers {
	ps, err := config.LoadPeers(filepath.Join(w.remoteEnv["KRAN_CONFIG_DIR"], "peers.toml"))
	if err != nil {
		w.t.Fatal(err)
	}
	return ps
}

// start brings up the two test sshds. remoteSSHD authorizes keys listed in the
// remote authorized_keys; localSSHD those in the local one. Passing
// inbound=false makes our advertised address point nowhere.
func (w *world) start(inbound bool) {
	os.MkdirAll(filepath.Dir(w.remoteAuthKeys()), 0o700)
	os.MkdirAll(filepath.Dir(w.localAuthKeys()), 0o700)
	rs := newTestSSHD(w.t, w.remoteAuthKeys())
	w.runner.g = fmt.Sprintf("user tester\nhostname %s\nport %s\n", strings.Split(rs.Addr, ":")[0], strings.Split(rs.Addr, ":")[1])
	w.remoteEnv["KRAN_SSHD_HOSTKEY"] = rs.HostPub
	if inbound {
		ls := newTestSSHD(w.t, w.localAuthKeys())
		w.t.Setenv("KRAN_SSHD_HOSTKEY", ls.HostPub)
		w.opts.SelfAddresses = []string{ls.Addr}
	} else {
		w.opts.SelfAddresses = []string{"127.0.0.1:1"}
	}
}

func TestLinkEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	w := newWorld(t)
	w.start(true)
	var steps []string
	w.opts.Progress = func(s, d string) { steps = append(steps, s) }

	res, err := Link(context.Background(), w.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Direction != "both" || res.ReverseNote != "" || !res.Installed || res.Already {
		t.Fatalf("%+v", res)
	}
	if res.RTT <= 0 || res.Bandwidth <= 0 {
		t.Fatalf("measurements: %v %v", res.RTT, res.Bandwidth)
	}
	if strings.Join(steps, ",") != "connect,install,setup,keys,verify" {
		t.Fatalf("steps %v", steps)
	}

	// remote kran installed, set up under the dev env only
	if st, err := os.Stat(filepath.Join(w.remoteHome, "bin", "kran")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("remote binary: %v %v", st, err)
	}
	// keys: each side authorizes the other's kran key with a forced command and the tag
	ours, _ := config.PublicKey()
	ra := w.readFile(w.remoteAuthKeys())
	// Both installs are placed by KRAN_*_DIR (a dev setup): the forced
	// commands carry those dirs so they reach these daemons.
	if !strings.Contains(ra, `command="env KRAN_CONFIG_DIR=`) ||
		!hasPath(ra, filepath.Join(w.remoteHome, "bin", "kran"), ` serve --stdio --peer gx-local",restrict `) ||
		!strings.Contains(ra, keyBlob(ours)) || !strings.HasSuffix(strings.TrimSpace(ra), "kran@gx-local kran-dev") {
		t.Fatalf("remote authorized_keys:\n%s", ra)
	}
	la := w.readFile(w.localAuthKeys())
	if !strings.Contains(la, `command="env KRAN_CONFIG_DIR=`) || !hasPath(la, w.self, ` serve --stdio --peer devbox",restrict `) || !strings.HasSuffix(strings.TrimSpace(la), "kran@devbox kran-dev") {
		t.Fatalf("local authorized_keys:\n%s", la)
	}
	// peers.toml on both sides
	p := res.Peer
	if p.Name != "devbox" || p.Direction != "both" || p.Tag != "kran-dev" || p.User != "tester" || !strings.HasPrefix(p.HostKey, "ssh-ed25519 ") ||
		(p.KranPath != filepath.Join(w.remoteHome, "bin", "kran") && p.KranPath != resolved(filepath.Join(w.remoteHome, "bin", "kran"))) || p.PreferredAddress == "" || p.RemoteConfigDir != w.remoteEnv["KRAN_CONFIG_DIR"] {
		t.Fatalf("local peer: %+v", p)
	}
	rp, ok := w.remotePeers().Find("gx-local")
	if !ok || rp.Direction != "both" || rp.Tag != "kran-dev" || (rp.KranPath != w.self && rp.KranPath != resolved(w.self)) || len(rp.Addresses) != 1 {
		t.Fatalf("remote peer: %+v", rp)
	}
	// no real-config pollution: the remote's files live under remoteHome only
	if _, err := os.Stat(filepath.Join(w.localHome, ".config")); err == nil {
		t.Fatal("link wrote to the default config location")
	}

	// second run: nothing changes, nothing duplicates, no re-upload
	before := map[string]string{"ra": ra, "la": la,
		"lp": w.readFile(filepath.Join(w.localHome, "cfg", "peers.toml")),
		"rp": w.readFile(filepath.Join(w.remoteEnv["KRAN_CONFIG_DIR"], "peers.toml"))}
	installs := w.runner.sawInstall()
	res2, err := Link(context.Background(), w.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Already || res2.Installed || res2.Direction != "both" {
		t.Fatalf("second link: %+v", res2)
	}
	if w.runner.sawInstall() != installs {
		t.Fatal("binary was uploaded again")
	}
	after := map[string]string{"ra": w.readFile(w.remoteAuthKeys()), "la": w.readFile(w.localAuthKeys()),
		"lp": w.readFile(filepath.Join(w.localHome, "cfg", "peers.toml")),
		"rp": w.readFile(filepath.Join(w.remoteEnv["KRAN_CONFIG_DIR"], "peers.toml"))}
	for k := range before {
		if before[k] != after[k] {
			t.Errorf("%s changed on re-link:\n%s\n---\n%s", k, before[k], after[k])
		}
	}

	// unlink removes exactly what link added
	ur, err := UnlinkDetailed(context.Background(), "devbox", w.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ur.RemovedPeer || !ur.RemovedKey || !ur.RemoteCleaned || len(ur.Leftover) != 0 {
		t.Fatalf("%+v", ur)
	}
	if strings.Contains(w.readFile(w.remoteAuthKeys()), "serve --stdio") || strings.Contains(w.readFile(w.localAuthKeys()), "serve --stdio") {
		t.Fatal("authorized_keys lines remain")
	}
	if len(w.remotePeers().Peer) != 0 {
		t.Fatal("remote peers remain")
	}
	if ps, _ := config.LoadPeers(""); len(ps.Peer) != 0 {
		t.Fatal("local peers remain")
	}
	if err := Unlink(context.Background(), "devbox", w.opts); err == nil || !strings.Contains(err.Error(), "no linked machine") {
		t.Fatalf("second unlink: %v", err)
	}
}

func TestLinkOneWayWhenWeCannotBeReached(t *testing.T) {
	w := newWorld(t)
	w.start(false)
	res, err := Link(context.Background(), w.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Direction != "outbound" || !strings.Contains(res.ReverseNote, "kran link devbox") {
		t.Fatalf("%+v", res)
	}
	if rp, _ := w.remotePeers().Find("gx-local"); rp == nil || rp.Direction != "inbound" {
		t.Fatalf("remote should see an inbound-only peer: %+v", rp)
	}
	if res.Peer.Direction != "outbound" {
		t.Fatal(res.Peer)
	}
}

func TestLinkFailsClearlyWhenKranKeyRejected(t *testing.T) {
	w := newWorld(t)
	w.start(false)
	// sshd that never accepts the key: point -G at a server with an empty authorized file
	dead := newTestSSHD(t, filepath.Join(t.TempDir(), "none"))
	w.runner.g = fmt.Sprintf("user tester\nhostname %s\nport %s\n", strings.Split(dead.Addr, ":")[0], strings.Split(dead.Addr, ":")[1])
	w.remoteEnv["KRAN_SSHD_HOSTKEY"] = dead.HostPub
	_, err := Link(context.Background(), w.opts)
	if err == nil || !strings.Contains(err.Error(), "could not connect to devbox with kran's key") {
		t.Fatalf("%v", err)
	}
	if ps, _ := config.LoadPeers(""); len(ps.Peer) != 0 {
		t.Fatal("no peer should be recorded on failure")
	}
}

func TestLinkRefusesSameName(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	w.opts.SelfName = "devbox"
	_, err := Link(context.Background(), w.opts)
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("%v", err)
	}
}

func TestLinkErrors(t *testing.T) {
	w := newWorld(t)
	for host, want := range map[string]string{"": "kran link <host>", "-oProxyCommand=x": "not a valid host"} {
		o := w.opts
		o.Host = host
		if _, err := Link(context.Background(), o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", host, err)
		}
	}
	// ssh missing
	o := w.opts
	o.Runner = scripted{func(Cmd) (Output, error) {
		return Output{}, fmt.Errorf("exec: \"ssh\": executable file not found in $PATH")
	}}
	if _, err := Link(context.Background(), o); err == nil || !strings.Contains(err.Error(), "install OpenSSH") {
		t.Errorf("%v", err)
	}
	// permission denied, not interactive
	o.Runner = scripted{func(Cmd) (Output, error) {
		return Output{}, &ExitError{Code: 255, Stderr: "devbox: Permission denied (publickey,password)."}
	}}
	if _, err := Link(context.Background(), o); err == nil || !strings.Contains(err.Error(), "works without a password") {
		t.Errorf("%v", err)
	}
	// unknown host key
	o.Runner = scripted{func(Cmd) (Output, error) {
		return Output{}, &ExitError{Code: 255, Stderr: "Host key verification failed."}
	}}
	if _, err := Link(context.Background(), o); err == nil || !strings.Contains(err.Error(), "accept its host key") {
		t.Errorf("%v", err)
	}
	// interactive retry uses the terminal
	var sawInherit bool
	o.Interactive = true
	o.Runner = scripted{func(c Cmd) (Output, error) {
		if c.Inherit {
			sawInherit = true
			return Output{}, &ExitError{Code: 255, Stderr: "Permission denied"}
		}
		return Output{}, &ExitError{Code: 255, Stderr: "Permission denied"}
	}}
	Link(context.Background(), o)
	if !sawInherit {
		t.Error("interactive mode should retry on the terminal")
	}
	// unsupported OS
	o.Interactive = false
	o.Runner = scripted{func(c Cmd) (Output, error) {
		if strings.Contains(c.Args[len(c.Args)-1], "uname") {
			return Output{Stdout: []byte("FreeBSD amd64\n/home/x\n")}, nil
		}
		return Output{}, nil
	}}
	if _, err := Link(context.Background(), o); err == nil || !strings.Contains(err.Error(), "linux and macOS") {
		t.Errorf("%v", err)
	}
}

func TestPickHostKeyOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte("h.example ssh-ed25519 FROMKNOWN\n"), 0o600)
	var scanned bool
	r := scripted{func(c Cmd) (Output, error) {
		if c.Name == "ssh-keyscan" {
			scanned = true
			return Output{Stdout: []byte("# h2\nh2.example ssh-ed25519 FROMSCAN\n")}, nil
		}
		return Output{}, fmt.Errorf("no")
	}}
	c := &sshClient{r: r, o: &Options{}}
	g := sshResolved{Hostname: "h.example", Port: 22}
	if k := pickHostKey(context.Background(), c, g, &Info{HostKey: "ssh-ed25519 FROMINFO"}); k != "ssh-ed25519 FROMINFO" {
		t.Fatal(k)
	}
	if k := pickHostKey(context.Background(), c, g, &Info{}); k != "ssh-ed25519 FROMKNOWN" || scanned {
		t.Fatal(k)
	}
	// ssh-keyscan is unauthenticated: a man in the middle would get pinned.
	g.Hostname = "h2.example"
	if k := pickHostKey(context.Background(), c, g, &Info{}); k != "" || scanned {
		t.Fatal(k, scanned)
	}
}

// Without a host key learned over the verified ssh channel (or known_hosts),
// link must stop before touching any authorized_keys instead of pinning
// whatever the first kran handshake presents.
func TestLinkRefusesWithoutVerifiedHostKey(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	w.remoteEnv["KRAN_SSHD_HOSTKEY"] = filepath.Join(t.TempDir(), "none")
	_, err := Link(context.Background(), w.opts)
	if err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(w.remoteAuthKeys()); len(b) != 0 {
		t.Fatalf("remote authorized_keys touched: %q", b)
	}
	if b, _ := os.ReadFile(w.localAuthKeys()); len(b) != 0 {
		t.Fatalf("local authorized_keys touched: %q", b)
	}
}

func TestLocalHostKeyTypes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh_host_rsa_key.pub"), []byte("ssh-rsa RSA root@h\n"), 0o644)
	if k := hostKeyFrom(dir); k != "ssh-rsa RSA" {
		t.Fatalf("rsa-only sshd: %q", k)
	}
	os.WriteFile(filepath.Join(dir, "ssh_host_ecdsa_key.pub"), []byte("ecdsa-sha2-nistp256 EC root@h\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), []byte("ssh-ed25519 ED root@h\n"), 0o644)
	if k := hostKeyFrom(dir); k != "ssh-ed25519 ED" {
		t.Fatalf("prefer ed25519: %q", k)
	}
}

var _ = ssh.KeyAlgoED25519

func TestUnlinkWhenPeerUnreachable(t *testing.T) {
	w := newWorld(t)
	w.start(true)
	if _, err := Link(context.Background(), w.opts); err != nil {
		t.Fatal(err)
	}
	down := w.opts
	down.Runner = scripted{func(Cmd) (Output, error) {
		return Output{}, &ExitError{Code: 255, Stderr: "ssh: connect to host devbox port 22: Connection refused"}
	}}
	ur, err := UnlinkDetailed(context.Background(), "devbox", down)
	if err != nil {
		t.Fatal(err)
	}
	if !ur.RemovedPeer || !ur.RemovedKey || ur.RemoteCleaned || len(ur.Leftover) != 1 || !strings.Contains(ur.Leftover[0], "kran@gx-local") {
		t.Fatalf("%+v", ur)
	}
	// our side is clean, theirs untouched
	if strings.Contains(w.readFile(w.localAuthKeys()), "serve --stdio") {
		t.Fatal("local line remains")
	}
	if !strings.Contains(w.readFile(w.remoteAuthKeys()), "serve --stdio") {
		t.Fatal("remote must not be touched while unreachable")
	}
}

// hasPath reports whether s contains " <p><suffix>" for p as given or
// with symlinks resolved.
func hasPath(s, p, suffix string) bool {
	return strings.Contains(s, " "+p+suffix) || strings.Contains(s, " "+resolved(p)+suffix)
}

// resolved is p with symlinks resolved, as kran records its own path (on
// macOS the temp dir /var/folders/... is really /private/var/folders/...).
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
