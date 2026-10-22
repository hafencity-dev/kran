package workspace

import "testing"

func TestMatcher(t *testing.T) {
	cases := []struct {
		pats  []string
		path  string
		isDir bool
		want  bool
	}{
		{[]string{"testdata/huge/"}, "testdata/huge/a.bin", false, true},
		{[]string{"testdata/huge/"}, "testdata/huge", false, false}, // a file named huge
		{[]string{"testdata/huge/"}, "x/testdata/huge/a", false, false},
		{[]string{"*.log"}, "a/b/c.log", false, true},
		{[]string{"*.log", "!keep.log"}, "a/keep.log", false, false},
		{[]string{"/build"}, "build/out.o", false, true},
		{[]string{"/build"}, "src/build/out.o", false, false},
		{[]string{"build"}, "src/build/out.o", false, true},
		{[]string{"**/gen/*.go"}, "a/b/gen/x.go", false, true},
		{[]string{"**/gen/*.go"}, "gen/x.go", false, true},
		{[]string{"docs/**"}, "docs/a/b.md", false, true},
		{[]string{"docs/**"}, "docs", true, false},
		{[]string{"a/**/z"}, "a/z", false, true},
		{[]string{"a/**/z"}, "a/b/c/z", false, true},
		{[]string{"# comment", ""}, "x", false, false},
		{[]string{"\\#hash"}, "#hash", false, true},
		{[]string{"f?o.[ch]"}, "dir/fao.c", false, true},
		{nil, "x", false, false},
	}
	for _, c := range cases {
		if got := NewMatcher(c.pats).Match(c.path, c.isDir); got != c.want {
			t.Errorf("%v match %q (dir=%v) = %v, want %v", c.pats, c.path, c.isDir, got, c.want)
		}
	}
}

func TestHardExcluded(t *testing.T) {
	cases := map[string]bool{
		".git/config":             true,
		"sub/.git":                true,
		"node_modules/x/index.js": true,
		"web/node_modules":        true,
		".kran/state":             true,
		".env":                    true,
		"app/.env.local":          true,
		".env.example":            false,
		"cfg/.env.sample":         false,
		".env.template":           false,
		"certs/server.pem":        true,
		"tls/x.key":               true,
		"store.p12":               true,
		"home/.ssh/id_ed25519":    true,
		"id_rsa":                  true,
		"internal/id_test.go":     false,
		"id_rsa.pub":              false,
		"src/main.go":             false,
		"environment.go":          false,
		"keyboard/keys.go":        false,
	}
	for p, want := range cases {
		if got := HardExcluded(p, false); got != want {
			t.Errorf("HardExcluded(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestLinkStaysInside(t *testing.T) {
	cases := []struct {
		rel, target string
		want        bool
	}{
		{"a/link", "b", true},
		{"a/link", "../b", true},
		{"a/link", "../../b", false},
		{"link", "..", false},
		{"link", "/etc/passwd", false},
		{"a/b/link", "../../c/../d", true},
		{"a/b/link", "../../../x", false},
		{"a/link", "./x/../y", true},
		{"a/link", "", false},
	}
	for _, c := range cases {
		if got := LinkStaysInside(c.rel, c.target); got != c.want {
			t.Errorf("LinkStaysInside(%q, %q) = %v, want %v", c.rel, c.target, got, c.want)
		}
	}
}
