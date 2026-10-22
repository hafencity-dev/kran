package link

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyA    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA kran@devmac"
	keyB    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB kran@gx10"
	userKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDuser me@laptop"
)

func setupHome(t *testing.T) string {
	t.Helper()
	t.Setenv("KRAN_CONFIG_DIR", t.TempDir())
	return filepath.Join(t.TempDir(), ".ssh", "authorized_keys")
}

func TestAuthorizedKeysLine(t *testing.T) {
	got := AuthorizedKeysLine("/home/u/.local/bin/kran", keyA, "")
	want := `command="/home/u/.local/bin/kran serve --stdio",restrict ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA kran@devmac`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := AuthorizedKeysLine("/k", keyA, "kran-dev"); !strings.HasSuffix(got, "kran@devmac kran-dev") {
		t.Fatal(got)
	}
	// tag not duplicated when the comment already carries it
	if got := AuthorizedKeysLine("/k", keyA+" kran-dev", "kran-dev"); strings.Count(got, "kran-dev") != 1 {
		t.Fatal(got)
	}
	if got := AuthorizedKeysLine(`/Users/a b/"x"/kran`, keyA, ""); !strings.Contains(got, `command="'/Users/a b/\"x\"/kran' serve --stdio"`) {
		t.Fatal(got)
	}
	if AuthorizedKeysLine("/k", keyA+"\n", "t") == "" {
		t.Fatal("a .pub file's trailing newline is fine")
	}
	// A newline in any part would start a second, unrestricted key line.
	for _, bad := range [][3]string{{"/k\nssh-ed25519 AAAA x", keyA, ""}, {"/k", keyA, "t\nssh-ed25519 AAAA evil"}, {"/k", keyA, "t\rx"}} {
		if got := AuthorizedKeysLine(bad[0], bad[1], bad[2]); got != "" {
			t.Fatalf("control character accepted: %q", got)
		}
	}
	if AuthorizedKeysLine("/k", "garbage", "") != "" {
		t.Fatal("garbage key must give empty line")
	}
}

func TestParseAuthLine(t *testing.T) {
	l := AuthorizedKeysLine(`/Users/a b/kran`, keyA, "")
	al, ok := parseAuthLine(l)
	if !ok || !al.isKran() || al.blob != keyBlob(keyA) || al.comment != "kran@devmac" {
		t.Fatalf("%+v %v", al, ok)
	}
	if al, ok := parseAuthLine(userKey); !ok || al.isKran() || al.opts != "" {
		t.Fatalf("%+v", al)
	}
	for _, s := range []string{"", "# comment", "   ", "garbage"} {
		if _, ok := parseAuthLine(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
}

func TestEnsureAuthorizedKeyCases(t *testing.T) {
	lineA := AuthorizedKeysLine("/opt/kran", keyA, "")
	lineA2 := AuthorizedKeysLine("/new/kran", keyA, "")
	lineB := AuthorizedKeysLine("/opt/kran", keyB, "")
	cases := []struct {
		name, before, line, after string
		changed                   bool
	}{
		{"missing file", "", lineA, lineA + "\n", true},
		{"empty file", "\n", lineA, "\n" + lineA + "\n", true},
		{"no trailing newline", userKey, lineA, userKey + "\n" + lineA + "\n", true},
		{"comments and blanks kept", "# mine\n\n" + userKey + "\n", lineA, "# mine\n\n" + userKey + "\n" + lineA + "\n", true},
		{"idempotent", userKey + "\n" + lineA + "\n", lineA, userKey + "\n" + lineA + "\n", false},
		{"replace moved binary only that line", userKey + "\n" + lineA + "\n" + lineB + "\n", lineA2, userKey + "\n" + lineA2 + "\n" + lineB + "\n", true},
		{"dedupe duplicates", lineA + "\n" + userKey + "\n" + lineA + "\n", lineA, lineA + "\n" + userKey + "\n", true},
		{"crlf preserved", userKey + "\r\n", lineA, userKey + "\r\n" + lineA + "\n", true},
		{"crlf entry replaced keeps crlf", lineA + "\r\n", lineA2, lineA2 + "\r\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := setupHome(t)
			if c.before != "" {
				os.MkdirAll(filepath.Dir(p), 0o700)
				os.WriteFile(p, []byte(c.before), 0o640)
			}
			ch, err := EnsureAuthorizedKey(p, c.line)
			if err != nil || ch != c.changed {
				t.Fatalf("changed=%v err=%v", ch, err)
			}
			got, _ := os.ReadFile(p)
			if string(got) != c.after {
				t.Fatalf("got %q\nwant %q", got, c.after)
			}
			if c.before != "" {
				if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
					t.Fatalf("mode changed to %v", st.Mode())
				}
			}
		})
	}
}

func TestEnsureAuthorizedKeyCreatesWithModes(t *testing.T) {
	p := setupHome(t)
	if _, err := EnsureAuthorizedKey(p, AuthorizedKeysLine("/k", keyA, "")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	ds, _ := os.Stat(filepath.Dir(p))
	if st.Mode().Perm() != 0o600 || ds.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", st.Mode(), ds.Mode())
	}
}

func TestEnsureAuthorizedKeyFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KRAN_CONFIG_DIR", t.TempDir())
	real := filepath.Join(dir, "real")
	os.WriteFile(real, []byte(userKey+"\n"), 0o600)
	link := filepath.Join(dir, "authorized_keys")
	os.Symlink(real, link)
	if _, err := EnsureAuthorizedKey(link, AuthorizedKeysLine("/k", keyA, "")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "serve --stdio") {
		t.Fatal("target not updated")
	}
}

func TestEnsureAuthorizedKeyRefusesUnrestrictedDuplicate(t *testing.T) {
	p := setupHome(t)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(keyA+"\n"), 0o600) // same key, no restrictions
	_, err := EnsureAuthorizedKey(p, AuthorizedKeysLine("/k", keyA, ""))
	if err == nil || !strings.Contains(err.Error(), "without kran's restrictions") {
		t.Fatalf("%v", err)
	}
}

func TestRemoveAuthorizedKey(t *testing.T) {
	p := setupHome(t)
	lineA := AuthorizedKeysLine("/opt/kran", keyA, "")
	lineB := AuthorizedKeysLine("/opt/kran", keyB, "")
	orig := "# keep\r\n" + userKey + "\n" + lineB + "\n"
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(orig[:len("# keep\r\n")+len(userKey)+1]+lineA+"\n"+lineB+"\n"), 0o640)
	removed, err := RemoveAuthorizedKey(p, keyBlob(keyA))
	if err != nil || !removed {
		t.Fatal(removed, err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != orig {
		t.Fatalf("got %q want %q", got, orig)
	}
	if again, _ := RemoveAuthorizedKey(p, keyBlob(keyA)); again {
		t.Fatal("second removal should be a no-op")
	}
	// a user's own unrestricted line with the same blob is never removed
	os.WriteFile(p, []byte(keyA+"\n"), 0o600)
	if r, _ := RemoveAuthorizedKey(p, keyBlob(keyA)); r {
		t.Fatal("removed a non-kran line")
	}
	// missing file is fine
	if r, err := RemoveAuthorizedKey(filepath.Join(t.TempDir(), "nope"), "x"); r || err != nil {
		t.Fatal(r, err)
	}
}

// sshd runs the forced command as `$SHELL -c "<command>"` after undoing only
// \" in the option value (OpenSSH opt_dequote). The kran path must reach
// the shell as one word, whatever it contains.
func TestForcedCommandThroughShell(t *testing.T) {
	for _, dir := range []string{"plain", "with space", `quote"d`, "it's", `back\slash`, "dollar$HOME", "semi;colon"} {
		d := filepath.Join(t.TempDir(), dir)
		os.MkdirAll(d, 0o755)
		kran := filepath.Join(d, "kran")
		out := filepath.Join(t.TempDir(), "args")
		os.WriteFile(kran, []byte("#!/bin/sh\nprintf '%s|' \"$@\" > '"+out+"'\n"), 0o755)
		al, ok := parseAuthLine(AuthorizedKeysLine(kran, keyA, ""))
		if !ok {
			t.Fatalf("%s: unparsable", dir)
		}
		v := strings.TrimSuffix(strings.TrimPrefix(al.opts, `command="`), `",restrict`)
		cmd := strings.ReplaceAll(v, `\"`, `"`)
		if b, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s (command %q)", dir, err, b, cmd)
		}
		if b, _ := os.ReadFile(out); string(b) != "serve|--stdio|" {
			t.Fatalf("%s: args %q", dir, b)
		}
	}
}

// The forced command carries the name the key is linked as, so the daemon
// knows which linked machine a session belongs to whatever its hello says.
func TestPeerKeyLineNamesThePeer(t *testing.T) {
	got := PeerKeyLine("/k", "devmac", keyA, "")
	if !strings.HasPrefix(got, `command="/k serve --stdio --peer devmac",restrict `) {
		t.Fatal(got)
	}
	if al, ok := parseAuthLine(got); !ok || !al.isKran() {
		t.Fatalf("%+v", al)
	}
	for _, bad := range []string{"Dev Mac", "a;b", "-x", ""} {
		if got := PeerKeyLine("/k", bad, keyA, ""); got != "" {
			t.Errorf("peer %q gave %q", bad, got)
		}
	}
}
