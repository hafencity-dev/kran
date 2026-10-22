package execenv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A lingering process of the user (an earlier job) can swap a file for a
// symlink while the slot is being updated: chmod must not follow it.
func TestChmodDoesNotFollowSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("x"), 0o600)
	os.Symlink(victim, filepath.Join(root, "f"))
	s, err := openSlotFS(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.chmod(&proto.ManifestEntry{Path: "f", Mode: 0o777})
	if st, _ := os.Stat(victim); st.Mode().Perm() != 0o600 {
		t.Fatalf("chmod followed the symlink: %v", st.Mode())
	}
	// A regular file, even one we cannot read, still gets its mode.
	os.Remove(filepath.Join(root, "f"))
	os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o200)
	if err := s.chmod(&proto.ManifestEntry{Path: "f", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(filepath.Join(root, "f")); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
}

// removeTree must delete below the directory it checked, even if a parent
// is swapped for a symlink in between.
func TestRemoveTreeStaysInSlot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "b", "c", "f"), []byte("x"), 0o444)
	os.MkdirAll(filepath.Join(outside, "b"), 0o755)
	keep := filepath.Join(outside, "b", "keep")
	os.WriteFile(keep, []byte("x"), 0o644)
	s, err := openSlotFS(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	beforeRemoveTree = func() {
		os.Rename(filepath.Join(root, "a"), filepath.Join(root, "a.moved"))
		os.Symlink(outside, filepath.Join(root, "a"))
	}
	defer func() { beforeRemoveTree = nil }()
	if err := s.removeTree("a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("removeTree followed a swapped parent: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a.moved", "b")); !os.IsNotExist(err) {
		t.Fatalf("the checked directory was not removed: %v", err)
	}
}
