# Agents

`kran agents install` makes Claude Code and Codex use kran without being told each time. `kran agents status` shows what is installed; `kran agents uninstall` removes exactly that.

| Item | Where | Notes |
|---|---|---|
| Skill | `~/.claude/skills/kran/SKILL.md` | when to use `kran run`, batching, what stays local, how to read `kran:` lines and exit 125 |
| Instructions block | `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` | between `<!-- kran:begin -->` and `<!-- kran:end -->` markers; the rest of the file is never touched |
| Hook | `~/.claude/settings.json` | a `PreToolUse` hook on `Bash` running `kran hook claude`; merged without disturbing other hooks; the previous file is backed up |

## The hook

Claude Code passes each Bash tool call to `kran hook claude`. For a known heavy command that is not already wrapped, the hook returns `updatedInput` with the command replaced by `kran run --sh '<original>'` (correctly quoted; other fields such as `description` and `timeout` are preserved). The permission decision is left alone: your normal prompts and allow rules apply to the wrapped command.

Wrapped: `go test|build|vet`, `cargo build|test|check|clippy`, `next build`, `vitest`, `jest`, `playwright test`, `tsc -b` / `--noEmit`, `pytest`, `docker build` / `buildx build`, `make test|build|all|check`, `gradle`, `mvn`, `dotnet build|test`, `swift build|test`, `bazel build|test`, and their `npm|pnpm|yarn|bun|npx` forms.

Never wrapped: commands already using kran, background tool calls, dev servers and watchers (`next dev`, `vite`, `docker compose up`, `--watch`), interactive programs, anything with `localhost`, heredocs, command substitution, output redirected to files, or a pipe into the heavy command. `go build -o <file>` and `docker build --load/-o` stay local because the artifact would stay on the remote machine.

Disable temporarily with `KRAN_NO_HOOK=1`.

## For agent authors

- Output is passed through byte for byte; only stderr lines starting with `kran:` are kran's.
- `kran run --json-events` prints progress as one JSON object per line on stderr (`{"kran":"decision",...}`, `phase`, `started`, `notice`, `fallback`, `exit`).
- `kran status --json` and `kran jobs --json` describe the mesh and history.
- Exit `125` means kran failed after the command started; rerun with `kran run --local`.
- Set `KRAN_OWNER=claude:<session>` (or `CLAUDE_SESSION_ID`) so the scheduler can share capacity fairly between sessions.
