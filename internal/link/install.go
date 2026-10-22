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
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
)

const (
	repoSlug   = "hafencity-dev/kran"
	modulePath = "github.com/hafencity-dev/kran"
)

// Variables so tests can keep the default paths off the network.
var (
	defaultDownloadBase = "https://github.com"
	defaultAPIBase      = "https://api.github.com"
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
// binary cache, a GitHub release download (anonymous HTTPS, then gh, then GITHUB_TOKEN), a source
// build. Development builds prefer the source build, since the cache and
// releases cannot contain the code being developed.
func (b binarySource) acquire(ctx context.Context, goos, goarch string) (string, error) {
	o := b.o
	if goos == runtime.GOOS && goarch == runtime.GOARCH && o.SelfPath != "" {
		return o.SelfPath, nil
	}
	ver := o.Version
	cache := paths.BinaryCache(strings.TrimPrefix(ver, "v"), goos, goarch)
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
	return "", fmt.Errorf("no kran %s binary for %s/%s (%s). Options: `make install` on a machine with the source (fills the binary cache), check your network access to github.com (or `gh auth login` / set GITHUB_TOKEN)", ver, goos, goarch, strings.Join(why, "; "))
}

func (b binarySource) download(ctx context.Context, goos, goarch, dest string) (string, error) {
	if isDevVersion(b.o.Version) {
		return "", errors.New("development builds have no release")
	}
	return b.fetch(ctx, b.o.Version, goos, goarch)
}

// fetch downloads release `version` for goos/goarch into the binary cache
// (cache hit returns immediately), verifying it against the release's
// checksums.txt.
func (b binarySource) fetch(ctx context.Context, version, goos, goarch string) (string, error) {
	ver := strings.TrimPrefix(version, "v")
	if !releaseVersion.MatchString(ver) { // it becomes a cache path and a gh argument
		return "", fmt.Errorf("%q is not a release version (like 1.4.0)", version)
	}
	dest := paths.BinaryCache(ver, goos, goarch)
	if st, err := os.Stat(dest); err == nil && st.Mode().IsRegular() {
		return dest, nil
	}
	asset := assetName(ver, goos, goarch)
	tmp, err := os.MkdirTemp("", "kran-dl-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	tgz, sums := filepath.Join(tmp, asset), filepath.Join(tmp, "checksums.txt")
	haveSums := false
	got := false
	// 1. Anonymous HTTPS from the public release page.
	var anonErr error
	if anonErr = b.anonDownload(ctx, ver, asset, tgz); anonErr == nil {
		if anonErr = b.anonDownload(ctx, ver, "checksums.txt", sums); anonErr == nil {
			got, haveSums = true, true
		}
	}
	// 2. gh, when installed and authenticated (higher rate limits).
	if !got && b.ghReady(ctx) {
		tag := "v" + ver
		if _, err := b.r.Run(ctx, Cmd{Name: "gh", Args: []string{"release", "download", tag, "-R", repoSlug, "-p", asset, "-D", tmp, "--clobber"}}); err == nil {
			got = true
			if _, err := b.r.Run(ctx, Cmd{Name: "gh", Args: []string{"release", "download", tag, "-R", repoSlug, "-p", "checksums.txt", "-D", tmp, "--clobber"}}); err == nil {
				haveSums = true
			}
		}
	}
	// 3. GitHub API with a token.
	if !got {
		tok := b.token()
		if tok == "" {
			return "", fmt.Errorf("anonymous download failed (%v), and gh is not logged in and GITHUB_TOKEN is not set", anonErr)
		}
		if err := b.apiDownload(ctx, tok, "v"+ver, asset, tgz); err != nil {
			return "", err
		}
		haveSums = b.apiDownload(ctx, tok, "v"+ver, "checksums.txt", sums) == nil
	}
	if _, err := os.Stat(sums); err != nil {
		haveSums = false
	}
	// Fail closed like install.sh: this binary is pushed to and run on
	// every linked machine.
	if !haveSums {
		return "", fmt.Errorf("release v%s has no checksums.txt (or it could not be downloaded), so %s cannot be verified; try again", ver, asset)
	}
	if err := verifyChecksum(sums, tgz, asset); err != nil {
		return "", err
	}
	return dest, extractKran(tgz, dest)
}

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// anonDownload fetches a release asset from the public download URL without
// credentials and writes it to out.
func (b binarySource) anonDownload(ctx context.Context, ver, asset, out string) error {
	base := b.o.DownloadBase
	if base == "" {
		base = defaultDownloadBase
	}
	hc := b.o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	url := fmt.Sprintf("%s/%s/releases/download/v%s/%s", base, repoSlug, ver, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub answered %s for %s", resp.Status, asset)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (b binarySource) token() string {
	if b.o.GitHubToken != "" {
		return b.o.GitHubToken
	}
	return firstEnv("GITHUB_TOKEN", "GH_TOKEN")
}

func (b binarySource) ghReady(ctx context.Context) bool {
	_, err := b.r.Run(ctx, Cmd{Name: "gh", Args: []string{"auth", "status"}})
	return err == nil
}

func verifyChecksum(sumsFile, file, asset string) error {
	data, err := os.ReadFile(sumsFile)
	if err != nil {
		return err
	}
	for _, ln := range strings.Split(string(data), "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			sum, err := fileSHA256(file)
			if err != nil {
				return err
			}
			if !strings.EqualFold(sum, f[0]) {
				return fmt.Errorf("checksum mismatch for %s (download corrupted or tampered with); try again", asset)
			}
			return nil
		}
	}
	return fmt.Errorf("%s is not listed in the release's checksums.txt, so it cannot be verified", asset)
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func (b binarySource) apiClient() (*http.Client, string) {
	base := b.o.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	hc := b.o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return hc, base
}

func apiGet(ctx context.Context, hc *http.Client, token, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return hc.Do(req)
}

func (b binarySource) apiDownload(ctx context.Context, token, tag, asset, out string) error {
	hc, base := b.apiClient()
	resp, err := apiGet(ctx, hc, token, fmt.Sprintf("%s/repos/%s/releases/tags/%s", base, repoSlug, tag), "application/vnd.github+json")
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
		r2, err := apiGet(ctx, hc, token, a.URL, "application/octet-stream")
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
