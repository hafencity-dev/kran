package link

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
)

const (
	repoSlug   = "hafencity-dev/kran"
	modulePath = "github.com/hafencity-dev/kran"
)

func assetName(ver, goos, goarch string) string {
	return fmt.Sprintf("kran_%s_%s_%s.tar.gz", strings.TrimPrefix(ver, "v"), goos, goarch)
}

// binarySource finds kran binaries for a platform.
type binarySource struct {
	o *Options
	r Runner
}

// acquire returns a local file holding the kran binary for goos/goarch at
// the link's version. Order for releases: this process (same platform), the
// binary cache, a GitHub release download (gh, then GITHUB_TOKEN), a source
// build. Development builds prefer the source build, since the cache and
// releases cannot contain the code being developed.
func (b binarySource) acquire(ctx context.Context, goos, goarch string) (string, error) {
	o := b.o
	if goos == runtime.GOOS && goarch == runtime.GOARCH && o.SelfPath != "" {
		return o.SelfPath, nil
	}
	ver := o.Version
	cache := paths.BinaryCache(ver, goos, goarch)
	type step struct {
		name string
		fn   func() (string, error)
	}
	cacheStep := step{"cache", func() (string, error) {
		if st, err := os.Stat(cache); err == nil && st.Mode().IsRegular() {
			return cache, nil
		}
		return "", errors.New("not in the binary cache")
	}}
	dlStep := step{"release", func() (string, error) { return b.download(ctx, goos, goarch, cache) }}
	srcStep := step{"source", func() (string, error) { return b.build(ctx, goos, goarch, cache) }}
	steps := []step{cacheStep, dlStep, srcStep}
	if isDevVersion(ver) {
		steps = []step{srcStep, cacheStep}
	}
	var why []string
	for _, s := range steps {
		p, err := s.fn()
		if err == nil {
			return p, nil
		}
		why = append(why, fmt.Sprintf("%s: %v", s.name, err))
	}
	return "", fmt.Errorf("no kran %s binary for %s/%s (%s). Options: `make install` on a machine with the source (fills the binary cache), `gh auth login` so kran can download the release, or set GITHUB_TOKEN", ver, goos, goarch, strings.Join(why, "; "))
}

func (b binarySource) download(ctx context.Context, goos, goarch, dest string) (string, error) {
	ver := strings.TrimPrefix(b.o.Version, "v")
	if isDevVersion(b.o.Version) {
		return "", errors.New("development builds have no release")
	}
	asset := assetName(ver, goos, goarch)
	tmp, err := os.MkdirTemp("", "kran-dl-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	var tgz string
	// 1. gh, when installed and authenticated (the repo is private).
	if _, err := b.r.Run(ctx, Cmd{Name: "gh", Args: []string{"auth", "status"}}); err == nil {
		_, err := b.r.Run(ctx, Cmd{Name: "gh", Args: []string{"release", "download", "v" + ver, "-R", repoSlug, "-p", asset, "-D", tmp, "--clobber"}})
		if err == nil {
			tgz = filepath.Join(tmp, asset)
		}
	}
	// 2. GitHub API with a token.
	if tgz == "" {
		tok := b.o.GitHubToken
		if tok == "" {
			tok = firstEnv("GITHUB_TOKEN", "GH_TOKEN")
		}
		if tok == "" {
			return "", errors.New("gh is not installed or not logged in, and GITHUB_TOKEN is not set")
		}
		tgz = filepath.Join(tmp, asset)
		if err := b.apiDownload(ctx, tok, "v"+ver, asset, tgz); err != nil {
			return "", err
		}
	}
	return dest, extractKran(tgz, dest)
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func (b binarySource) apiDownload(ctx context.Context, token, tag, asset, out string) error {
	base := b.o.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	hc := b.o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	get := func(url, accept string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		return hc.Do(req)
	}
	resp, err := get(fmt.Sprintf("%s/repos/%s/releases/tags/%s", base, repoSlug, tag), "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub answered %s for release %s (is the token allowed to read %s?)", resp.Status, tag, repoSlug)
	}
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	for _, a := range rel.Assets {
		if a.Name != asset {
			continue
		}
		r2, err := get(a.URL, "application/octet-stream")
		if err != nil {
			return err
		}
		defer r2.Body.Close()
		if r2.StatusCode != 200 {
			return fmt.Errorf("GitHub answered %s downloading %s", r2.Status, asset)
		}
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r2.Body); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	return fmt.Errorf("release %s has no asset %s", tag, asset)
}

// extractKran writes the `kran` file from a .tar.gz to dest (0755, atomic).
func extractKran(tgz, dest string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive: %w", filepath.Base(tgz), err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s contains no kran binary", filepath.Base(tgz))
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "kran" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		tmp := dest + ".tmp"
		w, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, io.LimitReader(tr, 512<<20)); err != nil {
			w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, dest)
	}
}

// build cross-compiles from a source checkout.
func (b binarySource) build(ctx context.Context, goos, goarch, dest string) (string, error) {
	root := b.o.SourceDir
	if root == "" {
		root = findSourceRoot(b.o.SelfPath)
	}
	if root == "" {
		return "", errors.New("not running from a source checkout")
	}
	if _, err := b.r.Run(ctx, Cmd{Name: "go", Args: []string{"version"}}); err != nil {
		return "", errors.New("go is not installed")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".tmp"
	ld := ""
	if b.o.Version != "" && b.o.Version != "dev" {
		ld = "-X " + modulePath + "/internal/version.Version=" + strings.TrimPrefix(b.o.Version, "v")
	}
	args := []string{"build", "-trimpath"}
	if ld != "" {
		args = append(args, "-ldflags", ld)
	}
	args = append(args, "-o", tmp, "./cmd/kran")
	_, err := b.r.Run(ctx, Cmd{Name: "go", Args: args, Dir: root,
		Env: []string{"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch}})
	if err != nil {
		return "", fmt.Errorf("go build for %s/%s failed: %w", goos, goarch, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// findSourceRoot walks up from the binary and the cwd looking for this
// module's go.mod.
func findSourceRoot(self string) string {
	var starts []string
	if self != "" {
		starts = append(starts, filepath.Dir(self))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, s := range starts {
		for d := s; ; d = filepath.Dir(d) {
			if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil &&
				strings.Contains(string(b), "module "+modulePath) {
				return d
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return ""
}

// pushBinary installs src as <dirWord>/kran on the remote atomically,
// streaming over ssh stdin (no scp dependency).
func (c *sshClient) pushBinary(ctx context.Context, src, dir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	d := remoteWord(dir)
	script := `set -e; d=` + d + `; mkdir -p "$d"; t="$d/.kran.tmp.$$"; trap 'rm -f "$t"' EXIT; cat > "$t"; chmod 755 "$t"; mv -f "$t" "$d/kran"; trap - EXIT`
	_, err = c.run(ctx, script, f)
	return err
}

// remoteSHA returns the sha256 of a remote file, or "" if absent.
func (c *sshClient) remoteSHA(ctx context.Context, file string) string {
	f := remoteWord(file)
	out, err := c.run(ctx, `f=`+f+`; [ -f "$f" ] || exit 0; (sha256sum "$f" 2>/dev/null || shasum -a 256 "$f" 2>/dev/null) | cut -d' ' -f1`, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
