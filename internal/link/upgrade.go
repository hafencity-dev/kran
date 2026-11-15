package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// LatestRelease returns the newest release version of hafencity-dev/kran
// without the leading "v". It asks the public GitHub API anonymously first,
// then gh, then the API with a token (higher rate limits).
func LatestRelease(ctx context.Context, o Options) (string, error) {
	b := binarySource{o: &o, r: o.runner()}
	hc, base := b.apiClient()
	v, anonErr := latestViaAPI(ctx, hc, "", fmt.Sprintf("%s/repos/%s/releases/latest", base, repoSlug))
	if anonErr == nil {
		return v, nil
	}
	if b.ghReady(ctx) {
		out, err := b.r.Run(ctx, Cmd{Name: "gh", Args: []string{"release", "view", "-R", repoSlug, "--json", "tagName", "-q", ".tagName"}})
		if err == nil {
			if v := strings.TrimPrefix(strings.TrimSpace(string(out.Stdout)), "v"); v != "" {
				return v, nil
			}
		}
	}
	if tok := b.token(); tok != "" {
		return latestViaAPI(ctx, hc, tok, fmt.Sprintf("%s/repos/%s/releases/latest", base, repoSlug))
	}
	return "", fmt.Errorf("cannot look up the latest kran release: %w. If GitHub is rate limiting you, run `gh auth login` or set GITHUB_TOKEN, or pass --version", anonErr)
}

// latestViaAPI reads tag_name from the latest-release endpoint; token may be "".
func latestViaAPI(ctx context.Context, hc *http.Client, token, url string) (string, error) {
	resp, err := apiGet(ctx, hc, token, url, "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404:
		return "", fmt.Errorf("GitHub answered %s: no kran release has been published yet", resp.Status)
	case 403, 429:
		return "", fmt.Errorf("GitHub answered %s (rate limited?)", resp.Status)
	default:
		return "", fmt.Errorf("GitHub answered %s asking for the latest kran release", resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if v := strings.TrimPrefix(rel.Tag, "v"); v != "" {
		return v, nil
	}
	return "", errors.New("GitHub returned a release without a tag")
}

// FetchRelease downloads release `version` for goos/goarch into the binary
// cache (verifying checksums.txt when the release has one) and returns the
// cached path. A cache hit returns immediately.
func FetchRelease(ctx context.Context, o Options, version, goos, goarch string) (string, error) {
	b := binarySource{o: &o, r: o.runner()}
	p, err := b.fetch(ctx, version, goos, goarch)
	if err != nil {
		return "", fmt.Errorf("download kran %s for %s/%s: %w. Check your network access to github.com; if GitHub is rate limiting you, run `gh auth login` or set GITHUB_TOKEN", strings.TrimPrefix(version, "v"), goos, goarch, err)
	}
	return p, nil
}

// UpgradeResult reports what Upgrade did to one peer.
type UpgradeResult struct {
	Name      string
	From, To  string
	Installed bool
	Restarted bool
	// Daemon is the version the peer's daemon reports afterwards (set with
	// Options.Exact; "" when it was not asked).
	Daemon string
	Notes  []string
}

// Upgrade brings an already linked peer to the version of o.SelfPath/o.Version:
// installs the matching binary (plus the linux binary for macOS peers) and
// restarts the peer's service so it runs it. Keys are not touched.
func Upgrade(ctx context.Context, name string, o Options) (*UpgradeResult, error) {
	peer, err := loadPeer(name)
	if err != nil {
		return nil, fmt.Errorf("no linked machine named %q; `kran status` lists the linked machines", name)
	}
	if st, ok := readLinkState(name); ok {
		if o.Host == "" {
			o.Host = st.SSHDest
			if o.SSHArgs == nil {
				o.SSHArgs = st.SSHArgs
			}
		}
		if o.RemoteEnv == nil {
			o.RemoteEnv = st.RemoteEnv
		}
		if o.Tag == "" {
			o.Tag = st.Tag
		}
	}
	if o.Host == "" {
		o.Host = destFromPeer(peer, &o)
	}
	if o.RemoteEnv == nil && peer.RemoteConfigDir != "" {
		o.RemoteEnv = map[string]string{"KRAN_CONFIG_DIR": peer.RemoteConfigDir}
	}
	if o.Host == "" {
		return nil, fmt.Errorf("do not know how to reach %s; run `kran link <host>` again", name)
	}
	if o.SelfPath == "" {
		if o.SelfPath, err = selfExe(); err != nil {
			return nil, err
		}
	}
	if o.RemoteBinDir == "" && peer.KranPath != "" {
		o.RemoteBinDir = pathDir(peer.KranPath) // update in place
	}
	res := &UpgradeResult{Name: name, To: o.Version}
	c := newSSH(&o)
	defer c.exitMaster()
	o.progress("connect", "Connecting to "+name)
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, `uname -sm; printf '%s\n' "$HOME"`, nil)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected answer from %s: %q", name, strings.TrimSpace(string(out)))
	}
	rOS, rArch, err := platform(lines[0])
	if err != nil {
		return nil, err
	}
	home := strings.TrimSpace(lines[1])

	r := &Result{}
	src := binarySource{o: &o, r: o.runner()}
	var before func(*Info) error
	runtimeCh := false
	if rOS == "darwin" {
		// A Mac runs its container jobs with the linux binary of the same
		// version. It is copied (and checked there) before the Mac's own
		// binary, in either direction: a failure up to the binary's rename
		// leaves the Mac running what it ran, with at most the new
		// version's runtime added to its cache, where nothing uses it yet.
		before = func(cur *Info) error {
			if cur == nil || cur.CacheDir == "" {
				return nil // a kran that does not say where its cache is: after, as before
			}
			if note := pushLinuxRuntime(ctx, c, src, cur.CacheDir, o.Version, rArch, &runtimeCh); note != "" {
				return errors.New(note)
			}
			return nil
		}
	}
	kranPath, info, changed, err := ensureKran(ctx, &o, c, src, rOS, rArch, home, r, before)
	if err != nil {
		return nil, err
	}
	changed = changed || runtimeCh
	res.From = r.RemoteVersion
	res.Installed = changed
	res.Notes = append(res.Notes, r.Notes...)
	if rOS == "darwin" {
		ch := false
		// Normally already there (copied before the binary); this fills in
		// one that is missing when the binary needed no change.
		if note := pushLinuxBinary(ctx, c, src, info, rArch, &ch); note != "" {
			if o.Exact {
				return nil, fmt.Errorf("%s; the binary on %s is kran %s, its daemon was not restarted", note, name, info.Version)
			}
			res.Notes = append(res.Notes, note)
		}
		res.Installed = res.Installed || ch
	}
	return res.finish(ctx, c, &o, kranPath, info)
}

func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

func (res *UpgradeResult) finish(ctx context.Context, c *sshClient, o *Options, kranPath string, info *Info) (*UpgradeResult, error) {
	if res.Installed {
		o.progress("restart", "Restarting kran on "+res.Name)
		args := []string{"service", "restart"}
		if o.Force {
			args = append(args, "--force")
		}
		if _, err := c.kran(ctx, kranPath, nil, args...); err != nil {
			msg := err.Error()
			if strings.Contains(msg, "not installed") {
				res.Notes = append(res.Notes, fmt.Sprintf("the kran service is not installed on %s; run `kran setup` there", res.Name))
			} else {
				return res, fmt.Errorf("installed kran %s on %s but restarting its service failed: %w (run `kran service restart` there)", o.Version, res.Name, err)
			}
		} else {
			res.Restarted = true
		}
	}
	now, err := c.remoteInfo(ctx, kranPath)
	if err != nil {
		return res, fmt.Errorf("could not verify kran on %s: %w", res.Name, err)
	}
	res.To = now.Version
	if o.Exact {
		// --version: done only when the daemon there runs exactly it.
		want := strings.TrimPrefix(o.Version, "v")
		dv := c.daemonVersion(ctx, kranPath)
		if dv != want && res.Restarted {
			dv = c.awaitDaemon(ctx, kranPath, want) // a slow machine is still starting it
		}
		if dv != want && !res.Restarted {
			// The binary is right but the daemon still runs another one.
			args := []string{"service", "restart"}
			if o.Force {
				args = append(args, "--force")
			}
			if _, err := c.kran(ctx, kranPath, nil, args...); err != nil {
				return res, fmt.Errorf("the binary on %s is kran %s, but its daemon runs %s and restarting it failed: %w (run `kran service restart` there)", res.Name, now.Version, orNone(dv), err)
			}
			res.Restarted = true
			dv = c.awaitDaemon(ctx, kranPath, want)
		}
		res.Daemon = dv
		if dv != want {
			return res, fmt.Errorf("the binary on %s is kran %s, but its daemon reports %s %s after the restart, not %s", res.Name, now.Version, orNone(dv), daemonWait, want)
		}
	}
	if cmp, ok := compareVersions(now.Version, o.Version); ok && cmp < 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%s still reports kran %s", res.Name, now.Version))
	}
	return res, nil
}

func orNone(v string) string {
	if v == "" {
		return "nothing (not running)"
	}
	return v
}

// daemonVersion asks the peer's kran which version its daemon runs ("" when
// none answers): the "daemon: <version> (…)" line of `kran version`.
// A restarted peer daemon may take up to proto.DaemonStartBound to serve
// (a slow Mac under load), as locally: its version is read until it reports
// the one asked for, for at most that long. Tests drive the clock.
var (
	daemonWait  = proto.DaemonStartBound
	daemonEvery = 2 * time.Second
	daemonClock = time.Now
	daemonSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// awaitDaemon reads the peer daemon's version until it is want or
// daemonWait has passed, and returns the last one read.
func (c *sshClient) awaitDaemon(ctx context.Context, kranPath, want string) string {
	deadline := daemonClock().Add(daemonWait)
	for {
		dv := c.daemonVersion(ctx, kranPath)
		if dv == want || !daemonClock().Before(deadline) {
			return dv
		}
		if daemonSleep(ctx, daemonEvery) != nil {
			return dv
		}
	}
}

func (c *sshClient) daemonVersion(ctx context.Context, kranPath string) string {
	out, err := c.kran(ctx, kranPath, nil, "version")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "daemon: "); ok {
			v, _, _ := strings.Cut(rest, " ")
			if v == "not" {
				return ""
			}
			return strings.TrimPrefix(v, "v")
		}
	}
	return ""
}
