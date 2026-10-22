package execenv

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// docker parses --mount as CSV with later keys winning: a workspace root
// from the origin must not add keys (src=/ would mount the host root).
func TestReservedMountRejectsCSV(t *testing.T) {
	for _, root := range []string{"/w/a,src=/", "/w/a,dst=/etc", `/w/"x"`, "/w/a\nb"} {
		if !reservedMount(root) {
			t.Errorf("%q accepted as a mount target", root)
		}
	}
	if reservedMount("/home/u/my repo=1") {
		t.Fatal("plain path refused")
	}
}

// The origin's BASH_ENV/ENV name files on the origin; on the target they
// would make every bash/sh -c of the job source a local file first.
func TestHostSpecificShellStartupEnv(t *testing.T) {
	for _, k := range []string{"BASH_ENV", "ENV"} {
		if !hostSpecificEnv(k) {
			t.Errorf("%s forwarded to the job", k)
		}
	}
}

// Every node builds the runner image from this file: what it downloads is
// pinned by version and checksum, and nothing is piped into a shell.
func TestRunnerDockerfilePinsDownloads(t *testing.T) {
	df := runnerDockerfile
	if regexp.MustCompile(`\|\s*(sh|bash)\b`).MatchString(df) {
		t.Error("runner Dockerfile pipes a download into a shell")
	}
	for _, bad := range []string{"mise.run", "latest"} {
		if strings.Contains(df, bad) {
			t.Errorf("runner Dockerfile contains %q", bad)
		}
	}
	downloads := strings.Count(df, "curl -fsSL")
	if downloads == 0 || strings.Count(df, "sha256sum -c") < downloads {
		t.Errorf("%d downloads, %d checksum checks", downloads, strings.Count(df, "sha256sum -c"))
	}
	for _, arch := range []string{"x86_64", "aarch64"} {
		if !strings.Contains(df, arch) {
			t.Errorf("no checksums for %s", arch)
		}
	}
}
