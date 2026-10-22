package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

// settings.json editing. Claude Code's settings hold many unrelated keys and
// the user's own hooks, so we keep every untouched value byte-for-byte (as
// json.RawMessage), keep key order, and refuse to touch invalid JSON.

type kv struct {
	key string
	val json.RawMessage
}

type object []kv

func parseObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("top-level value is not an object")
	}
	var o object
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o = append(o, kv{kt.(string), raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("unexpected data after the object")
	}
	return o, nil
}

func (o object) get(k string) (json.RawMessage, bool) {
	for _, e := range o {
		if e.key == k {
			return e.val, true
		}
	}
	return nil, false
}

func (o object) set(k string, v json.RawMessage) object {
	for i := range o {
		if o[i].key == k {
			o[i].val = v
			return o
		}
	}
	return append(o, kv{k, v})
}

func (o object) del(k string) object {
	out := o[:0:0]
	for _, e := range o {
		if e.key != k {
			out = append(out, e)
		}
	}
	return out
}

func (o object) marshal() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(e.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func indent(raw []byte) []byte {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return raw
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// isOurCommand reports whether a hook command is `<...>/kran hook claude`.
func isOurCommand(cmd string) bool {
	const suffix = " hook claude"
	cmd = strings.TrimSpace(cmd)
	if !strings.HasSuffix(cmd, suffix) {
		return false
	}
	bin := strings.TrimSpace(strings.TrimSuffix(cmd, suffix))
	if len(bin) >= 2 && bin[0] == '\'' && bin[len(bin)-1] == '\'' {
		bin = strings.ReplaceAll(bin[1:len(bin)-1], `'\''`, "'")
	} else if strings.ContainsAny(bin, " \t") {
		return false
	}
	return path.Base(bin) == "kran"
}

// settingsDoc is a parsed settings.json with typed access to our hook group.
type settingsDoc struct {
	top     object
	hooks   object
	pre     []json.RawMessage
	existed bool
	orig    []byte
}

func loadSettings(p string) (*settingsDoc, error) {
	data, exists, err := readIfExists(p)
	if err != nil {
		return nil, err
	}
	d := &settingsDoc{existed: exists && len(bytes.TrimSpace(data)) > 0, orig: data}
	if !d.existed {
		return d, nil
	}
	bad := func(err error) error {
		return fmt.Errorf("%s is not valid JSON (%v); kran will not overwrite it: fix or move the file, then rerun `kran agents install`", p, err)
	}
	if d.top, err = parseObject(data); err != nil {
		return nil, bad(err)
	}
	if raw, ok := d.top.get("hooks"); ok && string(bytes.TrimSpace(raw)) != "null" {
		if d.hooks, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("%s: \"hooks\" must be an object (%v); fix it, then rerun", p, err)
		}
	}
	if raw, ok := d.hooks.get("PreToolUse"); ok && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &d.pre); err != nil {
			return nil, fmt.Errorf("%s: \"hooks.PreToolUse\" must be an array (%v); fix it, then rerun", p, err)
		}
	}
	return d, nil
}

type group struct {
	Hooks []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

// ours returns, per matcher group, the indexes of our hook entries.
func (d *settingsDoc) ours() (found [][2]int, current []string) {
	for gi, raw := range d.pre {
		var g group
		if json.Unmarshal(raw, &g) != nil {
			continue
		}
		for hi, h := range g.Hooks {
			if isOurCommand(h.Command) {
				found = append(found, [2]int{gi, hi})
				current = append(current, h.Command)
			}
		}
	}
	return
}

func (d *settingsDoc) encode() []byte {
	top := d.top
	hooks := d.hooks
	if len(d.pre) == 0 {
		hooks = hooks.del("PreToolUse")
	} else {
		arr, _ := json.Marshal(d.pre)
		hooks = hooks.set("PreToolUse", arr)
	}
	if len(hooks) == 0 {
		top = top.del("hooks")
	} else {
		top = top.set("hooks", hooks.marshal())
	}
	return indent(top.marshal())
}

func (e *env) backupAndWrite(p string, d *settingsDoc, out []byte) error {
	if d.existed {
		stamp := e.now().Format("20060102-150405")
		bp := p + ".kran-backup-" + stamp
		for i := 2; ; i++ {
			if _, err := os.Stat(bp); err != nil {
				break
			}
			bp = fmt.Sprintf("%s.kran-backup-%s-%d", p, stamp, i)
		}
		if err := writeKeepMode(bp, d.orig, 0o600, 0o700); err != nil {
			return fmt.Errorf("backup %s: %w", p, err)
		}
	}
	return writeKeepMode(p, out, 0o600, 0o700)
}

func (e *env) installHook(p string) (string, string, error) {
	d, err := loadSettings(p)
	if err != nil {
		return "", "", err
	}
	want := e.hookCommand()
	found, current := d.ours()
	state, detail := StateInstalled, ""
	if len(found) == 0 {
		g, _ := json.Marshal(map[string]any{
			"matcher": "Bash",
			"hooks":   []any{map[string]any{"type": "command", "command": want, "timeout": 10}},
		})
		d.pre = append(d.pre, g)
	} else {
		same := true
		for _, c := range current {
			same = same && c == want
		}
		if same {
			return StateUnchanged, "", nil
		}
		for _, f := range found {
			var g map[string]any
			if err := json.Unmarshal(d.pre[f[0]], &g); err != nil {
				return "", "", err
			}
			g["hooks"].([]any)[f[1]].(map[string]any)["command"] = want
			if d.pre[f[0]], err = json.Marshal(g); err != nil {
				return "", "", err
			}
		}
		detail = "updated path"
	}
	if err := e.backupAndWrite(p, d, d.encode()); err != nil {
		return "", "", err
	}
	return state, detail, nil
}

func (e *env) statusHook(p string) (string, string, error) {
	d, err := loadSettings(p)
	if err != nil {
		return "", "", err
	}
	found, current := d.ours()
	if len(found) == 0 {
		return StateMissing, "", nil
	}
	for _, c := range current {
		if c != e.hookCommand() {
			return StateOutdated, "hook points at " + c + "; run `kran agents install` to update it", nil
		}
	}
	return StateInstalled, "", nil
}

func (e *env) removeHook(p string) (string, string, error) {
	d, err := loadSettings(p)
	if err != nil {
		return "", "", err
	}
	found, _ := d.ours()
	if len(found) == 0 {
		return StateUnchanged, "not installed", nil
	}
	drop := map[int]map[int]bool{}
	for _, f := range found {
		if drop[f[0]] == nil {
			drop[f[0]] = map[int]bool{}
		}
		drop[f[0]][f[1]] = true
	}
	var next []json.RawMessage
	for gi, raw := range d.pre {
		hs, ok := drop[gi]
		if !ok {
			next = append(next, raw)
			continue
		}
		var g map[string]any
		if err := json.Unmarshal(raw, &g); err != nil {
			return "", "", err
		}
		var keep []any
		for hi, h := range g["hooks"].([]any) {
			if !hs[hi] {
				keep = append(keep, h)
			}
		}
		if len(keep) == 0 {
			continue // the group held only our hook
		}
		g["hooks"] = keep
		nr, err := json.Marshal(g)
		if err != nil {
			return "", "", err
		}
		next = append(next, nr)
	}
	d.pre = next
	out := d.encode()
	if strings.TrimSpace(string(out)) == "{}" {
		// Nothing but our hook was in there: leave no empty file behind.
		if err := os.Remove(resolve(p)); err != nil {
			return "", "", err
		}
		return StateRemoved, "settings.json held only kran's hook; deleted", nil
	}
	if err := e.backupAndWrite(p, d, out); err != nil {
		return "", "", err
	}
	return StateRemoved, "", nil
}
