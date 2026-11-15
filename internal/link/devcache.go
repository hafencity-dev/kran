package link

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hafencity-dev/kran/internal/paths"
)

// buildVersion is a version kran upgrade accepts: a release (1.4.0), a
// release with one tag (1.4.0-rc1), or a build between releases as
// `git describe` names it (0.1.12-5-gf7c1755, also on a tag: 1.4.0-rc1-3-gabc).
// It becomes a cache path, so nothing else (no "/", no "..") gets through.
var buildVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?(-[0-9]+-g[0-9a-f]+)?$`)

// VersionGrammar names the accepted forms, for messages.
const VersionGrammar = "x.y.z, x.y.z-tag or x.y.z-N-g<hex> (like 1.4.0, 1.4.0-rc1, 0.1.12-5-gf7c1755)"

// ValidVersion checks a version string before any file is touched with it:
// the grammar above, never "dirty" (an uncommitted tree is no version), never
// a path separator, "..", white space or a leading dash. A leading "v" is
// allowed.
func ValidVersion(v string) error {
	s := strings.TrimPrefix(v, "v")
	bad := s == "" || strings.ContainsAny(s, "/\\ \t\r\n") || strings.Contains(s, "..") || strings.HasPrefix(s, "-") ||
		strings.Contains(strings.ToLower(s), "dirty") || !buildVersion.MatchString(s)
	if bad {
		return fmt.Errorf("%q is not a kran version; give %s", v, VersionGrammar)
	}
	return nil
}

// describeVersion is a build between releases (git describe:
// <tag>-<commits>-g<hash>): it exists only where it was built.
var describeVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?-[0-9]+-g[0-9a-f]+$`)

// SumsFile is the digest file `make cache` writes beside a version's
// binaries in the binary cache: <cache>/bin/<version>/SHA256SUMS, one line
// "<sha256>  <os>-<arch>/kran" per binary, as sha256sum writes it.
const SumsFile = "SHA256SUMS"

// cachedVerified returns the cached binary of a development version for
// goos/goarch after checking it against the digest written when it was
// built. A development version is never downloaded (no release has it) and
// never built here (that would be the code checked out now, not the code
// that was reviewed): the cache is its only source.
func cachedVerified(ver, goos, goarch string) (string, error) {
	bin := paths.BinaryCache(ver, goos, goarch)
	dir := filepath.Dir(filepath.Dir(bin))
	fix := fmt.Sprintf("build it into the cache from that commit with `make cache` (it writes %s)", SumsFile)
	if st, err := os.Stat(bin); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("kran %s for %s/%s is not in the binary cache (%s); %s", ver, goos, goarch, bin, fix)
	}
	f, err := os.Open(filepath.Join(dir, SumsFile))
	if err != nil {
		return "", fmt.Errorf("kran %s has no %s in %s, so the cached binary cannot be verified; %s", ver, SumsFile, dir, fix)
	}
	defer f.Close()
	name := goos + "-" + goarch + "/kran"
	want := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 2 && strings.TrimPrefix(fs[1], "*") == name {
			want = fs[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s of kran %s names no %s; %s", SumsFile, ver, name, fix)
	}
	got, err := fileSHA256(bin)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("the cached kran %s for %s/%s does not match its %s (sha256 %s, want %s); rebuild it with `make cache`", ver, goos, goarch, SumsFile, got, want)
	}
	return bin, nil
}
