package estimate

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

var (
	// Temp paths differ per run (go test -coverprofile, mktemp output dirs).
	reTmp = regexp.MustCompile(`(?:/private)?/(?:tmp|var/folders|var/tmp)/[^\s:=,;'"]*`)
	// UUIDs and long hex IDs (build IDs, image digests) vary per run.
	reUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reHex  = regexp.MustCompile(`(?i)\b[0-9a-f]{16,}\b`)
	// Numbers with more than 6 digits are timestamps, PIDs-with-time, seeds.
	reLongNum = regexp.MustCompile(`\d{7,}`)
	reSpace   = regexp.MustCompile(`\s+`)
)

// normalizeArgv returns the words that identify "the same job". Flags and
// their values are kept (-count=N and -run REGEX matter), only values that
// vary between otherwise identical runs are replaced.
func normalizeArgv(spec *proto.JobSpec) []string {
	argv := spec.Argv
	if spec.Shell != "" {
		// Argv is [$SHELL -c Shell]; the user's login shell is not part of
		// the job's identity.
		argv = []string{"sh", "-c", reSpace.ReplaceAllString(strings.TrimSpace(spec.Shell), " ")}
	}
	out := make([]string, 0, len(argv))
	for i, a := range argv {
		if i == 0 {
			// /usr/local/go/bin/go and go are the same tool.
			a = path.Base(a)
		}
		out = append(out, normalizeWord(a, spec.Workspace.Root))
	}
	return out
}

func normalizeWord(a, root string) string {
	if root != "" && root != "/" && strings.Contains(a, root) {
		// Worktrees of one repo share the repo ID; absolute paths into the
		// worktree must not split their history.
		a = strings.ReplaceAll(a, root, "<root>")
	}
	a = reTmp.ReplaceAllString(a, "<tmp>")
	a = reUUID.ReplaceAllString(a, "<uuid>")
	a = reHex.ReplaceAllStringFunc(a, func(h string) string {
		// Pure-digit runs are handled by reLongNum; a hex ID has letters.
		if strings.IndexFunc(h, func(r rune) bool { return r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' }) < 0 {
			return h
		}
		return "<hex>"
	})
	return reLongNum.ReplaceAllString(a, "<n>")
}

// fingerprint = sha256(repo ID, cwd relative to the repo, normalized argv),
// truncated to 128 bits.
func fingerprint(spec *proto.JobSpec) string {
	h := sha256.New()
	repo := spec.Workspace.RepoID
	if repo == "" {
		repo = "cwd:" + spec.Cwd
	}
	h.Write([]byte(repo))
	h.Write([]byte{0})
	h.Write([]byte(spec.Workspace.Rel))
	for _, w := range normalizeArgv(spec) {
		h.Write([]byte{0})
		h.Write([]byte(w))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
