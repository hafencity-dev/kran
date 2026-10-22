package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadMissingGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Accept.Enabled || c.Accept.ReserveCPUs != 2 || c.ReserveMemBytes() != 8<<30 || c.UI.Listen != "127.0.0.1:7270" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("name = \"gx10\"\n[accept]\nreserve_cpus = 4\nenabled = true\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "gx10" || c.Accept.ReserveCPUs != 4 || c.Accept.Nice != 10 || c.Accept.ReserveMem != "8GiB" {
		t.Fatalf("%+v", c)
	}
}

func TestLoadErrorsAreHelpful(t *testing.T) {
	cases := map[string]string{
		"name = \"Bad Name\"\n":            "try \"bad-name\"",
		"[submit]\nprefer = \"sometimes\"": "auto, local or remote",
		"[accept]\nreserve_mem = \"lots\"": "invalid size",
		"nme = \"x\"\n":                    "unknown field",
		"name = = 1":                       "config.toml:1",
	}
	for body, want := range cases {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte(body), 0o600)
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want mention of %q", body, err, want)
		}
	}
}

func TestSaveRoundTripAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.toml")
	c := Defaults()
	c.Name = "devmac"
	c.Accept.ReserveMem = "4GiB"
	if err := Save(p, &c); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	got, err := Load(p)
	if err != nil || got.Name != "devmac" || got.Accept.ReserveMem != "4GiB" {
		t.Fatalf("%+v %v", got, err)
	}
	c.Name = "BAD NAME"
	if err := Save(p, &c); err == nil {
		t.Fatal("expected validation error")
	}
	after, _ := Load(p)
	if after.Name != "devmac" {
		t.Fatal("invalid save must not touch the file")
	}
}

func TestAtomicWriteLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	for i := 0; i < 20; i++ {
		if err := WriteFileAtomic(p, []byte(fmt.Sprint(i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func TestUpdatePeersConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "peers.toml")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := UpdatePeers(p, func(ps *Peers) error {
				ps.Upsert(Peer{Name: fmt.Sprintf("n%d", i), ID: fmt.Sprintf("id%d", i), Addresses: []string{"h:22"}, Direction: "both", LinkedAt: time.Now()})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ps, err := LoadPeers(p)
	if err != nil || len(ps.Peer) != 16 {
		t.Fatalf("lost updates: %d %v", len(ps.Peer), err)
	}
}

func TestPeersUpsertRemoveValidate(t *testing.T) {
	var ps Peers
	ps.Upsert(Peer{Name: "a", ID: "1", Addresses: []string{"a:22"}, Direction: "both"})
	ps.Upsert(Peer{Name: "a2", ID: "1", Addresses: []string{"b:22"}, Direction: "both"}) // same ID replaces
	if len(ps.Peer) != 1 || ps.Peer[0].Name != "a2" {
		t.Fatalf("%+v", ps)
	}
	if !ps.Remove("a2") || ps.Remove("a2") {
		t.Fatal("remove semantics")
	}
	// User and addresses end up as ssh destinations: never an option.
	for _, p := range []Peer{
		{Name: "a", Addresses: []string{"a:22"}, Direction: "both", User: "-oProxyCommand=sh -c x"},
		{Name: "a", Addresses: []string{"a:22"}, Direction: "both", User: "me\nx"},
		{Name: "a", Addresses: []string{"-oProxyCommand=x:22"}, Direction: "both"},
	} {
		if p.Validate() == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	if err := (&Peer{Name: "a", Addresses: []string{"a:22"}, Direction: "both", User: "first.last"}).Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Peer{Name: "X", Direction: "sideways"}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "no addresses") || !strings.Contains(err.Error(), "direction") {
		t.Fatalf("%v", err)
	}
}

func TestNodeID(t *testing.T) {
	t.Setenv("KRAN_CONFIG_DIR", t.TempDir())
	if NodeID() != "" {
		t.Fatal("no id before setup")
	}
	id, created, err := EnsureNodeID()
	if err != nil || !created || len(id) != 32 {
		t.Fatal(id, created, err)
	}
	id2, created2, _ := EnsureNodeID()
	if id2 != id || created2 {
		t.Fatal("id must be stable")
	}
}

func TestSize(t *testing.T) {
	for in, want := range map[string]int64{"8GiB": 8 << 30, "512M": 512 << 20, "6g": 6 << 30, "1.5GiB": 3 << 29, "1024": 1024} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "5 parsecs", "-1G"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if FormatSize(8<<30) != "8GiB" || FormatSize(3<<29) != "1.5GiB" || FormatSize(100) != "100B" {
		t.Error(FormatSize(8<<30), FormatSize(3<<29))
	}
}

func TestEnsureKey(t *testing.T) {
	t.Setenv("KRAN_CONFIG_DIR", t.TempDir())
	pub, created, err := EnsureKey("gx10")
	if err != nil || !created || !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " kran@gx10") {
		t.Fatal(pub, created, err)
	}
	pub2, created2, err := EnsureKey("gx10")
	if err != nil || created2 || pub2 != pub {
		t.Fatal("second call must be a no-op", created2, err)
	}
	if st, _ := os.Stat(filepath.Join(os.Getenv("KRAN_CONFIG_DIR"), "id_ed25519")); st.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", st.Mode())
	}
	s, err := Signer()
	if err != nil || s.PublicKey() == nil {
		t.Fatal(err)
	}
}

func TestSetNameKeepsOtherKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("[accept]\nreserve_cpus = 4\n"), 0o600)
	if err := SetName(p, "gx10"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || c.Name != "gx10" || c.Accept.ReserveCPUs != 4 {
		t.Fatalf("%+v %v", c, err)
	}
	if err := SetName(p, "Bad Name"); err == nil {
		t.Fatal("must reject")
	}
	p2 := filepath.Join(t.TempDir(), "new", "config.toml")
	if err := SetName(p2, "devmac"); err != nil {
		t.Fatal(err)
	}
}
