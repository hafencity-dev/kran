package link

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ControlPath must live in a fresh private directory: in a shared /tmp
// another account could pre-create a predictable socket and answer as our
// ssh master (link-info with its own key and host key).
func TestControlPathIsPrivate(t *testing.T) {
	w := newWorld(t)
	a, b := newSSH(&w.opts), newSSH(&w.opts)
	defer a.exitMaster()
	defer b.exitMaster()
	if !strings.HasSuffix(a.ctl, "/%C") || filepath.Dir(a.ctl) == filepath.Dir(b.ctl) {
		t.Fatalf("control paths %q %q must be <private dir>/%%C, one dir per client", a.ctl, b.ctl)
	}
	st, err := os.Stat(filepath.Dir(a.ctl))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("control dir: %v %v", st, err)
	}
	a.exitMaster()
	if _, err := os.Stat(filepath.Dir(a.ctl)); !os.IsNotExist(err) {
		t.Fatalf("control dir not removed: %v", err)
	}
}
