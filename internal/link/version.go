package link

import (
	"strconv"
	"strings"
)

type semver struct{ major, minor, patch int }

// parseSemver accepts "1.2.3", "v1.2.3", "1.2.3-rc1+meta". "dev" and other
// non-release strings are not parseable.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	p := strings.Split(s, ".")
	if len(p) != 3 {
		return semver{}, false
	}
	var v [3]int
	for i := range p {
		n, err := strconv.Atoi(p[i])
		if err != nil || n < 0 {
			return semver{}, false
		}
		v[i] = n
	}
	return semver{v[0], v[1], v[2]}, true
}

// compareVersions returns -1/0/1 and ok=false when either side is not a release.
func compareVersions(a, b string) (int, bool) {
	x, ok1 := parseSemver(a)
	y, ok2 := parseSemver(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	for _, d := range [][2]int{{x.major, y.major}, {x.minor, y.minor}, {x.patch, y.patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func isDevVersion(v string) bool {
	_, ok := parseSemver(v)
	return !ok || strings.Contains(v, "dirty") || strings.Contains(strings.TrimPrefix(v, "v"), "-")
}
