package execenv

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Errors callers can match with errors.Is. Messages follow SPEC §11: plain,
// and say what to do next.
var (
	// ErrDiskFloor: the target is below its free-disk floor (reason `disk`).
	ErrDiskFloor = errors.New("disk space below kran's floor")
	// ErrUnknownJob: Attach for a job this node has no record of.
	ErrUnknownJob = errors.New("unknown job")
	// ErrRuntimeUnavailable: the requested runtime cannot run jobs now.
	ErrRuntimeUnavailable = errors.New("runtime not available")
	// ErrBlobMismatch: PutBlob content did not hash to the announced hash.
	ErrBlobMismatch = errors.New("blob content does not match its hash")
	// ErrMissingBlob: Commit needs a blob that is not in the blob store.
	ErrMissingBlob = errors.New("blob missing on target")
	// ErrBadManifest: the manifest contains a path or link kran refuses to
	// materialize (absolute, "..", escaping symlink, duplicate).
	ErrBadManifest = errors.New("manifest rejected")
)

var (
	jobIDRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	repoIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
)

func validJobID(id string) error {
	if !jobIDRe.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	return nil
}

func validRepoID(id string) error {
	if !repoIDRe.MatchString(id) {
		return fmt.Errorf("invalid repo id %q", id)
	}
	return nil
}

func validHash(h string) error {
	// By hand: this runs for every file of every manifest.
	ok := len(h) == 64
	for i := 0; ok && i < len(h); i++ {
		c := h[i]
		ok = c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
	}
	if !ok {
		return fmt.Errorf("invalid blob hash %q", h)
	}
	return nil
}

// layout maps the state dir (SPEC §4): blobs/, slots/, jobs/.
type layout struct{ state string }

func (l layout) blobsDir() string { return filepath.Join(l.state, "blobs") }
func (l layout) blob(h string) string {
	return filepath.Join(l.state, "blobs", h[:2], h)
}
func (l layout) blobTmp() string              { return filepath.Join(l.state, "blobs", "tmp") }
func (l layout) slotsDir() string             { return filepath.Join(l.state, "slots") }
func (l layout) repoSlots(repo string) string { return filepath.Join(l.state, "slots", repo) }
func (l layout) jobsDir() string              { return filepath.Join(l.state, "jobs") }
func (l layout) job(id string) string         { return filepath.Join(l.state, "jobs", id) }

// Files inside a job dir. The supervisor owns everything except spec.json
// (written once by the manager before launch) and lease (touched by the
// manager on every heartbeat).
const (
	fileSpec    = "spec.json"   // supervisor input
	fileLock    = "lock"        // flock held by the supervisor for its lifetime
	fileLease   = "lease"       // mtime = last heartbeat
	fileOut     = "out"         // framed output spool
	fileEvents  = "events"      // JSON lines: ready, phase changes
	fileStatus  = "status.json" // proto.Exit, written once at the end
	fileLive    = "live.json"   // latest usage sample
	fileLog     = "supervisor.log"
	fileCtlSock = "ctl.sock"
	dirTmp      = "tmp" // per-job TMPDIR (native)
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// writeFileAtomic writes via temp + rename so readers see old or new, never
// a torn file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp-" + randHex(4)
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
