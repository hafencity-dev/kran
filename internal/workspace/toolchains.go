package workspace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// toolAliases maps asdf/mise plugin names to kran's toolchain keys.
var toolAliases = map[string]string{
	"nodejs":  "node",
	"golang":  "go",
	"python3": "python",
	"rustc":   "rust",
}

func toolKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	// mise backends like "npm:prettier" or "aqua:foo/bar" are not toolchains
	// the scheduler reasons about; keep only core tool names.
	if strings.ContainsAny(name, ":/") {
		return ""
	}
	if a, ok := toolAliases[name]; ok {
		return a
	}
	return name
}

func toolchains(root string) map[string]string {
	out := map[string]string{}
	set := func(k, v string) {
		k = toolKey(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" || v == "system" {
			return
		}
		out[k] = v
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(b) > 1<<20 {
			return nil
		}
		return b
	}

	// Generic version managers first; tool-specific files override them.
	if b := read(".tool-versions"); b != nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := sc.Text()
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			f := strings.Fields(line)
			if len(f) >= 2 {
				set(f[0], f[1])
			}
		}
	}
	for _, name := range []string{".config/mise.toml", "mise/config.toml", ".mise.toml", "mise.toml"} {
		if b := read(name); b != nil {
			for k, v := range miseTools(b) {
				set(k, v)
			}
		}
	}
	if b := read("package.json"); b != nil {
		var pkg struct {
			Engines        map[string]string `json:"engines"`
			PackageManager string            `json:"packageManager"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			for _, k := range []string{"node", "pnpm", "yarn", "npm", "bun"} {
				if v, ok := pkg.Engines[k]; ok {
					set(k, v)
				}
			}
			if name, ver, ok := strings.Cut(pkg.PackageManager, "@"); ok {
				ver, _, _ = strings.Cut(ver, "+") // drop "+sha512.…" integrity suffix
				set(name, ver)
			}
		}
	}
	for _, name := range []string{".node-version", ".nvmrc"} {
		if b := read(name); b != nil {
			set("node", strings.TrimPrefix(firstLine(b), "v"))
		}
	}
	if b := read(".python-version"); b != nil {
		set("python", firstLine(b))
	}
	if b := read("go.mod"); b != nil {
		if v := goModVersion(b); v != "" {
			set("go", v)
		}
	}
	if b := read("rust-toolchain.toml"); b != nil {
		var rt struct {
			Toolchain struct {
				Channel string `toml:"channel"`
			} `toml:"toolchain"`
		}
		if toml.Unmarshal(b, &rt) == nil {
			set("rust", rt.Toolchain.Channel)
		}
	} else if b := read("rust-toolchain"); b != nil {
		set("rust", firstLine(b))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

// goModVersion returns the toolchain the module builds with: the
// "toolchain goX" line when present, else the "go X" line.
func goModVersion(b []byte) string {
	var goLine, tcLine string
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "go":
			goLine = f[1]
		case "toolchain":
			tcLine = strings.TrimPrefix(f[1], "go")
		}
	}
	if tcLine != "" && tcLine != "default" {
		return tcLine
	}
	return goLine
}

// miseTools reads the [tools] table of a mise config. Values may be a
// version string, a list (first wins) or a table with "version".
func miseTools(b []byte) map[string]string {
	var cfg struct {
		Tools map[string]any `toml:"tools"`
	}
	if toml.Unmarshal(b, &cfg) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range cfg.Tools {
		switch t := v.(type) {
		case string:
			out[k] = t
		case []any:
			if len(t) > 0 {
				if s, ok := t[0].(string); ok {
					out[k] = s
				}
			}
		case map[string]any:
			if s, ok := t["version"].(string); ok {
				out[k] = s
			}
		}
	}
	return out
}
