package execenv

import (
	"os"
	"path/filepath"
	"testing"
)

// The control socket may fall back to $TMPDIR/kran-<uid>, which in a shared
// /tmp another account can pre-create (as a symlink to its own directory, or
// world-writable) and then bind its own socket there to read the job's
// stdin and tty input.
func TestPrivateDir(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, "good")
	if err := privateDir(good); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(good); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode())
	}
	loose := filepath.Join(base, "loose")
	os.Mkdir(loose, 0o777)
	os.Chmod(loose, 0o777)
	if err := privateDir(loose); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(loose); st.Mode().Perm() != 0o700 {
		t.Fatalf("our own loose dir not tightened: %v", st.Mode())
	}
	other := filepath.Join(base, "other")
	os.Mkdir(other, 0o777)
	link := filepath.Join(base, "link")
	os.Symlink(other, link)
	if err := privateDir(link); err == nil {
		t.Fatal("symlinked control dir accepted")
	}
}
