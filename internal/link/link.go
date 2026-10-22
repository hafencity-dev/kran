package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/paths"
)

// Options configure Link and Unlink.
type Options struct {
	Host        string // ssh destination exactly as the user typed it
	Name        string // desired node name for the remote ("" = its own)
	SelfName    string // this node's name (default: config)
	SelfID      string // this node's ID (default: config)
	Version     string // this kran's version
	SelfPath    string // absolute path of the running kran
	Interactive bool   // may prompt for an ssh password / host key on the terminal
	SSHArgs     []string
	// Tag marks authorized_keys comments and peers.toml entries this link
	// adds ("kran-dev") so development links can be found and cleaned up.
	Tag string
	// RemoteEnv is exported before every remote kran invocation
	// (KRAN_CONFIG_DIR etc.); development links never touch the real config.
	RemoteEnv map[string]string
	// RemoteBinDir is where kran is installed on the remote (default ~/.local/bin).
	RemoteBinDir string
	// SelfAddresses overrides the addresses advertised for this machine.
	SelfAddresses []string
	// Force makes Upgrade restart the remote service without waiting for idle.
	Force bool
	// SourceDir is a kran source checkout for cross-compiling (default: found from SelfPath/cwd).
	SourceDir    string
	GitHubToken  string       // default $GITHUB_TOKEN / $GH_TOKEN
	APIBase      string       // GitHub API base (tests)
	DownloadBase string       // release download host, default https://github.com (tests)
	HTTPClient   *http.Client // tests
	Progress     func(step, detail string)
	Logf         func(string, ...any)
	Runner       Runner
	Now          func() time.Time
}

// Result describes a finished link.
type Result struct {
	Peer          config.Peer
	Direction     string // both | outbound | inbound
	ReverseNote   string // one sentence when the link is one-way
	RTT           time.Duration
	Bandwidth     float64 // bytes/s
	Installed     bool
	RemoteVersion string
	Already       bool // nothing needed changing
	Notes         []string
}

func (o *Options) progress(step, detail string) {
	if o.Progress != nil {
		o.Progress(step, detail)
	}
}

func (o *Options) logf(f string, a ...any) {
	if o.Logf != nil {
		o.Logf(f, a...)
	}
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) fill() error {
	if strings.TrimSpace(o.Host) == "" {
		return errors.New("no machine given; run `kran link <host>` (an ssh name such as devmac or user@host)")
	}
	if strings.HasPrefix(o.Host, "-") {
		return fmt.Errorf("%q is not a valid host name", o.Host)
	}
	if o.SelfPath == "" {
		p, err := selfExe()
		if err != nil {
			return err
		}
		o.SelfPath = p
	}
	if o.SelfName == "" {
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		o.SelfName = cfg.Name
	}
	if o.SelfID == "" {
		id, _, err := config.EnsureNodeID()
		if err != nil {
			return err
		}
		o.SelfID = id
	}
	return nil
}

// platform maps `uname -sm` to GOOS/GOARCH.
func platform(uname string) (goos, goarch string, err error) {
	f := strings.Fields(uname)
	if len(f) < 2 {
		return "", "", fmt.Errorf("could not tell what %q is; kran supports linux and macOS machines", uname)
	}
	switch strings.ToLower(f[0]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("kran supports linux and macOS machines; this one reports %q", f[0])
	}
	switch strings.ToLower(f[1]) {
	case "arm64", "aarch64":
		goarch = "arm64"
	case "x86_64", "amd64":
		goarch = "amd64"
	default:
		return "", "", fmt.Errorf("kran supports arm64 and amd64 machines; this one reports %q", f[1])
	}
	return goos, goarch, nil
}

// findRemoteKran returns the absolute path of kran on the remote, or "".
func findRemoteKran(ctx context.Context, c *sshClient, home string) string {
	var dirs []string
	if c.o.RemoteBinDir != "" {
		dirs = []string{c.o.RemoteBinDir}
	} else {
		dirs = []string{"~/.local/bin"}
	}
	var b strings.Builder
	for _, d := range dirs {
		fmt.Fprintf(&b, `if [ -x %s/kran ]; then printf %%s %s/kran; exit 0; fi; `, remoteWord(d), remoteWord(d))
	}
	if c.o.RemoteBinDir == "" {
		b.WriteString(`p=$(command -v kran 2>/dev/null) && [ -x "$p" ] && printf %s "$p"; `)
	}
	b.WriteString("exit 0")
	out, err := c.run(ctx, b.String(), nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (c *sshClient) remoteInfo(ctx context.Context, kranPath string) (*Info, error) {
	out, err := c.kran(ctx, kranPath, nil, "internal", "link-info")
	if err != nil {
		return nil, err
	}
	var in Info
	if err := json.Unmarshal(out, &in); err != nil {
		return nil, fmt.Errorf("unexpected answer from kran on %s: %w", c.dest, err)
	}
	return &in, nil
}

func (c *sshClient) kranJSON(ctx context.Context, kranPath string, req, resp any, sub string) error {
	var in *strings.Reader
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	in = strings.NewReader(string(b))
	out, err := c.kran(ctx, kranPath, in, "internal", sub)
	if err != nil {
		return err
	}
	if resp == nil {
		return nil
	}
	if err := json.Unmarshal(out, resp); err != nil {
		return fmt.Errorf("unexpected answer from kran on %s: %w", c.dest, err)
	}
	return nil
}

func absRemote(home, p string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return strings.TrimRight(home, "/") + "/" + p[2:]
	}
	return p
}

// Link links this machine with o.Host. It is idempotent: running it again
// re-verifies and reports Already when nothing needed changing.
func Link(ctx context.Context, o Options) (*Result, error) {
	if err := o.fill(); err != nil {
		return nil, err
	}
	res := &Result{}
	changed := false
	c := newSSH(&o)
	defer c.exitMaster()

	o.progress("connect", "Connecting to "+o.Host)
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, `uname -sm; printf '%s\n' "$HOME"`, nil)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected answer from %s: %q", o.Host, strings.TrimSpace(string(out)))
	}
	rOS, rArch, err := platform(lines[0])
	if err != nil {
		return nil, err
	}
	home := strings.TrimSpace(lines[1])
	g, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}

	// Self: key + node ID exist (link is also the first thing some people run).
	selfPub, _, err := config.EnsureKey(o.SelfName)
	if err != nil {
		return nil, err
	}
	signer, err := config.Signer()
	if err != nil {
		return nil, err
	}
	src := binarySource{o: &o, r: o.runner()}
	kranPath, info, ch, err := ensureKran(ctx, &o, c, src, rOS, rArch, home, res)
	if err != nil {
		return nil, err
	}
	changed = changed || ch
	// The Linux binary for containers goes first: a daemon restarted
	// without it reports its container runtime unavailable.
	if rOS == "darwin" && info.CacheDir != "" {
		if note := pushLinuxBinary(ctx, c, src, info, rArch, &changed); note != "" {
			res.Notes = append(res.Notes, note)
		}
	}
	// A new binary on a machine that already runs kran: its daemon still
	// runs the old one until the service restarts (after running jobs end).
	if ch && info.ID != "" && info.PubKey != "" {
		o.progress("restart", "Restarting kran on "+o.Host)
		if _, err := c.kran(ctx, kranPath, nil, "service", "restart"); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("updated kran on %s, but its daemon still runs the old version: %v (run `kran service restart` there)", o.Host, err))
		}
	}

	// --- first-time setup of the remote (node id, key, service)
	if info.ID == "" || info.PubKey == "" {
		o.progress("setup", "Setting up kran on "+o.Host)
		// The remote's coding agents are its owner's business: link never
		// changes them (they can run `kran agents install` there).
		args := []string{"setup", "--yes", "--quiet", "--no-agents", "--no-link"}
		if o.Name != "" {
			args = append(args, "--name", o.Name)
		}
		if _, err := c.kran(ctx, kranPath, nil, args...); err != nil {
			return nil, fmt.Errorf("kran setup on %s failed: %w (run it there directly to see why)", o.Host, err)
		}
		changed = true
		if info, err = c.remoteInfo(ctx, kranPath); err != nil || info.PubKey == "" {
			return nil, fmt.Errorf("kran setup on %s did not create a key; run `kran setup` there", o.Host)
		}
	} else if o.Name != "" && o.Name != info.Name {
		res.Notes = append(res.Notes, fmt.Sprintf("%s is already named %q; --name %s was ignored", o.Host, info.Name, o.Name))
	}
	res.RemoteVersion = info.Version
	if info.NoGUI {
		res.Notes = append(res.Notes, fmt.Sprintf("%s has no GUI login session, so its kran daemon starts on demand when kran connects. To keep it running permanently: ssh -t %s %s service install --system", config.SanitizeName(info.Name), o.Host, kranPath))
	}
	if rOS == "darwin" { // after a first-time setup; a no-op when it is there already
		if note := pushLinuxBinary(ctx, c, src, info, rArch, &changed); note != "" {
			res.Notes = append(res.Notes, note)
		}
	}

	// --- identity checks
	peerName := config.SanitizeName(info.Name)
	switch {
	case info.ID == o.SelfID || peerName == o.SelfName:
		return nil, fmt.Errorf("%s reports the same node name or ID as this machine (%s); give it another name: kran link %s --name <new-name>", o.Host, o.SelfName, o.Host)
	}
	if ps, err := config.LoadPeers(""); err == nil {
		if ex, ok := ps.Find(peerName); ok && ex.ID != "" && ex.ID != info.ID {
			return nil, fmt.Errorf("a different machine is already linked as %q; run `kran unlink %s` first or choose another name with --name", peerName, peerName)
		}
	}

	// --- addresses and host key
	user := g.User
	if user == "" {
		user = info.User
	}
	remoteAddrs := dedupe(append([]string{net.JoinHostPort(g.Hostname, strconv.Itoa(g.Port))}, info.Addresses...))
	hostKey := pickHostKey(ctx, c, g, info)
	if hostKey == "" {
		return nil, fmt.Errorf("could not learn %s's ssh host key in a verified way (no readable /etc/ssh/ssh_host_*_key.pub there and no known_hosts entry for %s). Run `ssh %s` once to record the key, then run kran link again", o.Host, g.Hostname, o.Host)
	}
	selfAddrs := o.SelfAddresses
	if len(selfAddrs) == 0 {
		selfAddrs = localAddresses()
	}

	// --- key exchange
	o.progress("keys", "Exchanging keys with "+peerName)
	ak, err := authorizedKeysPath()
	if err != nil {
		return nil, err
	}
	if ch, err := EnsureAuthorizedKey(ak, PeerKeyLineEnv(o.SelfPath, peerName, info.PubKey, o.Tag, InstallEnv())); err != nil {
		return nil, err
	} else if ch {
		changed = true
	}
	mine := PeerInfo{Name: o.SelfName, ID: o.SelfID, PubKey: selfPub, Addresses: selfAddrs, User: currentUser(),
		HostKey: localHostKey(), KranPath: o.SelfPath, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: o.Version, Direction: "both"}
	var acc AcceptResponse
	if err := c.kranJSON(ctx, kranPath, AcceptRequest{Peer: mine, Tag: o.Tag}, &acc, "link-accept"); err != nil {
		return nil, fmt.Errorf("could not register this machine on %s: %w", peerName, err)
	}
	if acc.Changed {
		changed = true
	}

	// --- verify both directions
	o.progress("verify", "Checking the connection in both directions")
	outRes := HandshakeAll(ctx, remoteAddrs, user, signer, hostKey, 5*time.Second)
	var probe ProbeResponse
	pErr := c.kranJSON(ctx, kranPath, ProbeRequest{Addresses: selfAddrs, HostKey: mine.HostKey, User: currentUser()}, &probe, "link-probe")
	_, outOK := fastestOK(outRes)
	inOK := pErr == nil && probe.OKAddress != ""
	switch {
	case outOK && inOK:
		res.Direction = "both"
	case outOK:
		res.Direction = "outbound"
		res.ReverseNote = reverseNote(peerName)
	case inOK:
		res.Direction = "inbound"
		res.ReverseNote = fmt.Sprintf("This machine cannot connect to %s with kran's key (%s), so jobs can only flow from %s to here.", peerName, firstErr(outRes), peerName)
	default:
		return nil, noConnectionError(peerName, outRes, probe, pErr)
	}
	// --- record
	preferred := ""
	if best, ok := fastestOK(outRes); ok {
		preferred = best.Address
	}
	peer := config.Peer{Name: peerName, ID: info.ID, Addresses: remoteAddrs, User: user, HostKey: hostKey,
		KranPath: info.KranPath, Direction: res.Direction, OS: info.OS, Arch: info.Arch, Tag: o.Tag,
		PreferredAddress: preferred, RemoteConfigDir: o.RemoteEnv["KRAN_CONFIG_DIR"]}
	if old, _ := config.LoadPeers(""); old != nil {
		if ex, ok := old.Find(peerName); ok && ex.PreferredAddress != "" {
			for _, r := range outRes {
				if r.OK && r.Address == ex.PreferredAddress {
					peer.PreferredAddress = ex.PreferredAddress // keep it stable
				}
			}
		}
	}
	pc, err := upsertPeer(peer, o.now())
	if err != nil {
		return nil, err
	}
	changed = changed || pc
	if sc, err := writeLinkState(peerName, linkState{PubKey: info.PubKey, SSHDest: o.Host, SSHArgs: o.SSHArgs, RemoteEnv: o.RemoteEnv, Tag: o.Tag}); err != nil {
		return nil, err
	} else if sc {
		changed = true
	}
	theirView := map[string]string{"both": "both", "outbound": "inbound", "inbound": "outbound"}[res.Direction]
	if theirView != "both" {
		mine.Direction = theirView
		if err := c.kranJSON(ctx, kranPath, AcceptRequest{Peer: mine, Tag: o.Tag}, &acc, "link-accept"); err != nil {
			res.Notes = append(res.Notes, "could not record the one-way link on "+peerName+": "+err.Error())
		}
	}
	if saved, err := loadPeer(peerName); err == nil {
		peer = saved
	}

	// --- measure
	res.RTT = c.rtt(ctx, 5)
	res.Bandwidth = c.bandwidth(ctx, 8<<20)
	res.Peer = peer
	res.Already = !changed
	return res, nil
}

// ensureKran installs (or updates) kran on the remote when it is missing or
// older than ours, and returns its path and link-info. Content hashes decide
// for development builds; an unchanged binary is never re-sent.
func ensureKran(ctx context.Context, o *Options, c *sshClient, src binarySource, rOS, rArch, home string, res *Result) (kranPath string, info *Info, changed bool, err error) {
	binDir := o.RemoteBinDir
	if binDir == "" {
		binDir = "~/.local/bin"
	}
	binDirAbs := absRemote(home, binDir)
	kranPath = findRemoteKran(ctx, c, home)
	if kranPath != "" && o.RemoteBinDir == "" && strings.HasPrefix(kranPath, strings.TrimRight(home, "/")+"/") {
		binDirAbs = path.Dir(kranPath) // update in place instead of shadowing it
	}
	if kranPath != "" {
		info, err = c.remoteInfo(ctx, kranPath)
		if err != nil {
			o.logf("remote kran at %s did not answer link-info (%v); reinstalling", kranPath, err)
			info = nil
		}
	}
	needInstall := info == nil
	if info != nil {
		res.RemoteVersion = info.Version // before any update
		if cmp, ok := compareVersions(info.Version, o.Version); ok {
			needInstall = cmp < 0
		} else {
			needInstall = true // unknown or dev: decided by content hash below
		}
	}
	if needInstall {
		o.progress("install", fmt.Sprintf("Installing kran %s (%s/%s)", o.Version, rOS, rArch))
		bin, aerr := src.acquire(ctx, rOS, rArch)
		switch {
		case aerr != nil && info == nil:
			return "", nil, false, aerr
		case aerr != nil:
			res.Notes = append(res.Notes, fmt.Sprintf("kept kran %s on %s: %v", info.Version, o.Host, aerr))
		default:
			sum, err := fileSHA256(bin)
			if err != nil {
				return "", nil, false, err
			}
			if info != nil && info.SHA256 == sum {
				break
			}
			if err := c.pushBinary(ctx, bin, binDirAbs); err != nil {
				return "", nil, false, fmt.Errorf("install kran on %s: %w", o.Host, err)
			}
			res.Installed, changed = true, true
			kranPath = binDirAbs + "/kran"
			if info, err = c.remoteInfo(ctx, kranPath); err != nil {
				return "", nil, false, fmt.Errorf("the installed kran on %s does not start: %w", o.Host, err)
			}
		}
	}
	if info == nil {
		return "", nil, false, fmt.Errorf("kran is not installed on %s", o.Host)
	}
	return kranPath, info, changed, nil
}

func loadPeer(name string) (config.Peer, error) {
	ps, err := config.LoadPeers("")
	if err != nil {
		return config.Peer{}, err
	}
	p, ok := ps.Find(name)
	if !ok {
		return config.Peer{}, errors.New("peer missing")
	}
	return *p, nil
}

func firstErr(rs []ProbeResult) string {
	for _, r := range rs {
		if r.Error != "" {
			return r.Error
		}
	}
	return "unknown error"
}

func reverseNote(peer string) string {
	how := "start sshd (for example `sudo systemctl enable --now ssh`)"
	if runtime.GOOS == "darwin" {
		how = "turn on System Settings > General > Sharing > Remote Login"
	}
	return fmt.Sprintf("%s cannot connect back to this machine, so jobs flow one way (from here to %s); to enable both, %s on this machine and run `kran link %s` again.", peer, peer, how, peer)
}

func noConnectionError(peer string, out []ProbeResult, probe ProbeResponse, pErr error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "could not connect to %s with kran's key, although ssh works. ", peer)
	for _, r := range out {
		fmt.Fprintf(&b, "%s: %s. ", r.Address, r.Error)
	}
	if pErr != nil {
		fmt.Fprintf(&b, "Reverse check failed: %v. ", pErr)
	}
	b.WriteString("Common causes: sshd restricts users (AllowUsers) or key types, or the remote login shell prints output that breaks forced commands. The keys were added; fix the cause and run kran link again.")
	return errors.New(b.String())
}

// pickHostKey chooses the key to pin: what the remote's own sshd reports
// (learned over the ssh channel known_hosts verified), then known_hosts.
// Empty means none was found; ssh-keyscan or the key a kran handshake
// presents are never trusted, since a man in the middle answers those.
func pickHostKey(ctx context.Context, c *sshClient, g sshResolved, info *Info) string {
	if info.HostKey != "" {
		return info.HostKey
	}
	name := g.Hostname
	if g.HostKeyAlias != "" && g.HostKeyAlias != "none" {
		name = g.HostKeyAlias
	}
	if k, err := HostKeyFromKnownHosts(name, g.Port); err == nil {
		return k
	}
	return ""
}

// pushLinuxBinary puts the linux kran for the remote's arch where the
// container runtime on macOS looks for it:
// <remote cache>/bin/<version>/linux-<arch>/kran.
func pushLinuxBinary(ctx context.Context, c *sshClient, src binarySource, info *Info, arch string, changed *bool) string {
	ver := info.Version
	if ver == "" {
		ver = c.o.Version
	}
	dir := fmt.Sprintf("%s/bin/%s/linux-%s", info.CacheDir, ver, arch)
	s := *src.o
	s.Version = ver
	bin, err := binarySource{o: &s, r: src.r}.acquire(ctx, "linux", arch)
	if err != nil {
		return fmt.Sprintf("could not provide the linux binary for containers on %s (%v); container jobs there need it at %s/kran", c.dest, err, dir)
	}
	sum, err := fileSHA256(bin)
	if err != nil {
		return err.Error()
	}
	if c.remoteSHA(ctx, dir+"/kran") == sum {
		return ""
	}
	c.o.progress("install", "Copying the linux binary for containers")
	if err := c.pushBinary(ctx, bin, dir); err != nil {
		return fmt.Sprintf("could not copy the linux binary to %s: %v", c.dest, err)
	}
	*changed = true
	return ""
}

// ---------------------------------------------------------------------------
// link state: what we need later to unlink exactly what link added.

type linkState struct {
	PubKey    string            `json:"pubkey"` // the peer's kran public key we authorized
	SSHDest   string            `json:"ssh_dest"`
	SSHArgs   []string          `json:"ssh_args,omitempty"`
	RemoteEnv map[string]string `json:"remote_env,omitempty"`
	Tag       string            `json:"tag,omitempty"`
}

func linkStatePath(name string) string {
	return filepath.Join(paths.ConfigDir(), "links", name+".json")
}

func writeLinkState(name string, s linkState) (changed bool, err error) {
	b, _ := json.MarshalIndent(s, "", "  ")
	b = append(b, '\n')
	p := linkStatePath(name)
	if old, err := os.ReadFile(p); err == nil && string(old) == string(b) {
		return false, nil
	}
	return true, config.WriteFileAtomic(p, b, 0o600)
}

func readLinkState(name string) (linkState, bool) {
	var s linkState
	b, err := os.ReadFile(linkStatePath(name))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	return s, true
}
