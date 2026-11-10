package execenv

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

// A node that is not Linux runs container jobs in a Docker VM and needs the
// Linux kran of its own version in its binary cache
// (<cache>/bin/<version>/linux-<arch>/kran, SPEC §4): `kran link` and
// `kran upgrade` copy it there from the machine that has it. A hand-made
// install of only the native binary leaves it out, and the node then looks
// healthy while it cannot take a single container job (devmac, 10.11.2026).

// LinuxRuntimeNeeded reports whether this node's container runtime needs the
// Linux binary from the cache. A Linux node runs containers with its own
// binary.
func LinuxRuntimeNeeded() bool { return runtime.GOOS != "linux" }

// LinuxRuntimePath is where this daemon looks for its Linux kran ("" on
// Linux). The cache directory of a "v"-prefixed spelling of the version is
// accepted when it is the one that exists.
func LinuxRuntimePath() string {
	if !LinuxRuntimeNeeded() {
		return ""
	}
	p := paths.BinaryCache(strings.TrimPrefix(version.Version, "v"), "linux", runtime.GOARCH)
	if _, err := os.Stat(p); err != nil {
		if p2 := paths.BinaryCache(version.Version, "linux", runtime.GOARCH); p2 != p {
			if _, err := os.Stat(p2); err == nil {
				return p2
			}
		}
	}
	return p
}

var releaseRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// isReleaseVersion: a plain x.y.z has a published asset; a development build
// (0.1.10-6-gc9403ef, dirty, "dev") has none.
func isReleaseVersion(v string) bool { return releaseRe.MatchString(strings.TrimPrefix(v, "v")) }

// CheckLinuxRuntime checks the Linux kran at path for the given version:
// present, a regular file, executable. nil means fine. The cache keeps no
// checksum next to the file (it was verified against the release's
// checksums.txt when it was downloaded), so there is nothing to compare with.
func CheckLinuxRuntime(path, ver string) *proto.RuntimeProblem {
	if path == "" {
		return nil
	}
	p := &proto.RuntimeProblem{Version: strings.TrimPrefix(ver, "v"), Path: path, Release: isReleaseVersion(ver)}
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		p.Code = proto.ProblemLinuxBinaryMissing
	case !fi.Mode().IsRegular():
		p.Code = proto.ProblemLinuxBinaryNotFile
	case fi.Mode().Perm()&0o111 == 0:
		p.Code = proto.ProblemLinuxBinaryNotExecutable
	default:
		return nil
	}
	p.Fix = linuxRuntimeFix(p)
	return p
}

func linuxRuntimeFix(p *proto.RuntimeProblem) string {
	if p.Code == proto.ProblemLinuxBinaryNotExecutable {
		return "run chmod 755 " + p.Path
	}
	var f string
	if p.Release {
		f = fmt.Sprintf("run `kran upgrade` on a machine linked to this node: it copies the linux binary of release %s there", p.Version)
	} else {
		f = fmt.Sprintf("%s is a development build with no published release asset: copy a linux/%s build of this exact version to %s (chmod 755), or install a release with `kran upgrade --version <release>`",
			p.Version, runtime.GOARCH, p.Path)
	}
	if other := otherArchBinaries(p.Path); len(other) > 0 {
		f += fmt.Sprintf(" (the cache has %s for this version, but this node needs linux-%s)", strings.Join(other, ", "), runtime.GOARCH)
	}
	return f
}

// otherArchBinaries lists linux-<arch> directories of the same version that
// hold a kran: a binary copied for the wrong architecture.
func otherArchBinaries(path string) []string {
	verDir := filepath.Dir(filepath.Dir(path))
	ents, err := os.ReadDir(verDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "linux-") || n == filepath.Base(filepath.Dir(path)) {
			continue
		}
		if _, err := os.Stat(filepath.Join(verDir, n, "kran")); err == nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// LinuxRuntimeCause is the node-side sentence: what is wrong and the fix.
// The scheduler puts "<node>: cannot run container jobs: " in front.
func LinuxRuntimeCause(p *proto.RuntimeProblem) string {
	var what string
	switch p.Code {
	case proto.ProblemLinuxBinaryNotFile:
		what = "is not a file"
	case proto.ProblemLinuxBinaryNotExecutable:
		what = "is not executable"
	default:
		what = "is missing"
	}
	s := fmt.Sprintf("its linux runtime for %s %s at %s", p.Version, what, p.Path)
	if p.Fix != "" {
		s += "; " + p.Fix
	}
	return s
}
