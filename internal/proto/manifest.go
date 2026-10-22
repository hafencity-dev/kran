package proto

// Manifest describes a workspace snapshot (SPEC §6.7).
type Manifest struct {
	RepoID  string          `json:"repo_id"`
	Root    string          `json:"root"` // origin absolute path (for same-path container mounts)
	Entries []ManifestEntry `json:"entries"`
	// Keep lists dir prefixes on the target that must survive slot updates
	// even though they are not in the manifest (dependency/cache dirs).
	Keep []string `json:"keep,omitempty"`
}

type EntryKind uint8

const (
	EntryFile    EntryKind = 0
	EntrySymlink EntryKind = 1
	EntryDir     EntryKind = 2 // only for empty dirs that must exist
)

type ManifestEntry struct {
	Path   string    `json:"p"` // slash-separated, relative to root
	Kind   EntryKind `json:"k,omitempty"`
	Mode   uint32    `json:"m"` // permission bits (0755/0644…)
	Size   int64     `json:"s,omitempty"`
	MTime  int64     `json:"t"`           // unix nanoseconds
	Hash   string    `json:"h,omitempty"` // sha256 hex (files)
	Target string    `json:"l,omitempty"` // symlink target (relative, never escapes root)
}

// SlotRef identifies an acquired slot on a target.
type SlotRef struct {
	Node   string `json:"node"`
	RepoID string `json:"repo_id"`
	Index  int    `json:"index"`
	Path   string `json:"path"` // target-local path (native) or volume subpath (container)
}

// SyncPlan is the target's answer to a manifest diff.
type SyncPlan struct {
	Slot    SlotRef  `json:"slot"`
	Missing []string `json:"missing"` // blob hashes the target needs
	Changed int      `json:"changed"` // files that will be written
	Deleted int      `json:"deleted"` // files that will be removed
	Bytes   int64    `json:"bytes"`   // bytes of Missing
	// Committed: nothing was missing, so the target already brought the
	// slot up to date while diffing; slot.commit is not needed (and a
	// no-op if sent). Protocol 1.1; older targets never set it.
	Committed bool `json:"committed,omitempty"`
}
