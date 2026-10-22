package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// In a monorepo the pins nearest the command's directory win per tool; tools
// pinned only higher up still come from there.
func TestToolchainsAtNearestPinWins(t *testing.T) {
	root := t.TempDir()
	web := filepath.Join(root, "apps", "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root, ".nvmrc", "v24.8.0\n")
	write(root, ".python-version", "3.12\n")
	write(root, "package.json", `{"packageManager":"pnpm@10.32.1"}`)
	write(web, ".nvmrc", "24.22.1\n")
	write(web, "package.json", `{"packageManager":"pnpm@10.34.7+sha512.abc"}`)

	want := map[string]string{"node": "24.22.1", "pnpm": "10.34.7", "python": "3.12"}
	if got := ToolchainsAt(root, web); !reflect.DeepEqual(got, want) {
		t.Errorf("in apps/web: %v, want %v", got, want)
	}
	if got := ToolchainsAt(root, filepath.Join(root, "apps")); got["node"] != "24.8.0" || got["pnpm"] != "10.32.1" {
		t.Errorf("in apps: %v", got)
	}
	if got := ToolchainsAt(root, root); got["node"] != "24.8.0" {
		t.Errorf("at the root: %v", got)
	}
	if got := ToolchainsAt(root, t.TempDir()); got["node"] != "24.8.0" {
		t.Errorf("outside the root reads the root: %v", got)
	}
}
