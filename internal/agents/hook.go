package agents

import (
	"encoding/json"
	"io"
	"os"

	"github.com/hafencity-dev/kran/internal/paths"
)

// hookInput is the part of Claude Code's PreToolUse payload we care about.
// Docs: https://code.claude.com/docs/en/hooks (PreToolUse input and decision control).
type hookInput struct {
	Cwd       string                     `json:"cwd"`
	ToolName  string                     `json:"tool_name"`
	ToolInput map[string]json.RawMessage `json:"tool_input"`
}

type hookOutput struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

type hookSpecific struct {
	HookEventName string `json:"hookEventName"`
	// UpdatedInput replaces the ENTIRE tool input, so it carries every field
	// of the original (description, timeout, …) next to the new command.
	UpdatedInput map[string]json.RawMessage `json:"updatedInput"`
	// PermissionDecision is "allow" only when the user's own allow rules
	// cover the original command (see permissions.go); otherwise it is
	// absent and Claude Code asks as it would have. updatedInput is applied
	// without a decision (verified with claude 2.1.308, `claude -p`).
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

// hookContext reaches the agent with every wrapped command, so it says only
// what changes its next move: the output is the command's own, the files may
// not be here, and what 125 means.
const hookContext = "kran ran this command (as `kran run --sh '…'`), possibly on another machine. Output and exit code are the command's own; a `kran:` line on stderr is kran's. Files it wrote may not be in this checkout: rerun with `kran run --local --sh '…'` if you need them. Exit 125 means kran itself failed."

// daemonUp reports whether a kran daemon has a socket. Without one kran could
// only run the command here and say so on every call, so the hook stays out
// of the way. A variable so tests can set it.
var daemonUp = func() bool {
	_, err := os.Stat(paths.Socket())
	return err == nil
}

// HookClaude implements `kran hook claude`, a Claude Code PreToolUse hook. It
// reads the hook payload from in and, for heavy Bash commands that are safe
// to move, writes an updatedInput that wraps the command in `kran run --sh`.
// For everything else it prints nothing. It always returns 0: a hook must
// never be the reason a tool call fails.
func HookClaude(in io.Reader, out io.Writer, kranPath string) int {
	if os.Getenv("KRAN_NO_HOOK") != "" {
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return 0
	}
	var h hookInput
	if json.Unmarshal(raw, &h) != nil || h.ToolName != "Bash" || h.ToolInput == nil {
		return 0
	}
	var cmd string
	if json.Unmarshal(h.ToolInput["command"], &cmd) != nil {
		return 0
	}
	var bg bool
	if b, ok := h.ToolInput["run_in_background"]; ok && json.Unmarshal(b, &bg) == nil && bg {
		return 0
	}
	wrapped, ok := wrapWith(kranPath, cmd)
	if !ok || !daemonUp() {
		return 0
	}
	rules, err := loadRules(settingsFiles(h.Cwd))
	if err != nil {
		return 0 // rules we cannot read: let Claude Code apply them unwrapped
	}
	wrap, allowRule := permissionVerdict(rules, cmd, []string{wrapped, "kran run --sh " + ShellQuote(cmd)})
	if !wrap {
		return 0
	}
	enc, err := json.Marshal(wrapped)
	if err != nil {
		return 0
	}
	upd := make(map[string]json.RawMessage, len(h.ToolInput))
	for k, v := range h.ToolInput {
		upd[k] = v
	}
	upd["command"] = enc
	o := hookOutput{hookSpecific{
		HookEventName:     "PreToolUse",
		UpdatedInput:      upd,
		AdditionalContext: hookContext,
	}}
	if allowRule != "" {
		o.HookSpecificOutput.PermissionDecision = "allow"
		o.HookSpecificOutput.PermissionDecisionReason = "allowed by your rule " + allowRule + " (kran runs the same command)"
	}
	b, err := json.Marshal(o)
	if err != nil {
		return 0
	}
	_, _ = out.Write(append(b, '\n'))
	return 0
}
