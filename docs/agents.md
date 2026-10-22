# Agents

`kran agents install` makes Claude Code and Codex use kran without being told each time. `kran agents status` shows what is installed; `kran agents uninstall` removes exactly that.

| Item | Where | Notes |
|---|---|---|
| Skill | `~/.claude/skills/kran/SKILL.md` | when to use `kran run`, what stays local, that remote files stay remote (`--local`), how to read `kran:` lines and exit 125 |
| Instructions block | `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md` | between `<!-- kran:begin -->` and `<!-- kran:end -->` markers; the rest of the file is never touched |
| Hook | `~/.claude/settings.json` | a `PreToolUse` hook on `Bash` running `kran hook claude`; merged without disturbing other hooks; the previous file is backed up |

## The hook

Claude Code passes each Bash tool call to `kran hook claude`. For a long-running test, check, lint or build that is safe to move, the hook returns `updatedInput` with the command replaced by `kran run --sh '<original>'` (correctly quoted; other fields such as `description` and `timeout` are preserved). Leading `cd DIR` steps stay outside, `cd DIR && kran run --sh '<rest>'`, so kran syncs the directory the command really runs in; a later `cd` to an absolute or home path keeps the command unwrapped and a one-sentence note telling the agent the output is the command's own and that files a remote run writes stay there.

Your Claude Code permission rules keep working as if the command had not been wrapped. The hook reads them (managed policy, `~/.claude/settings.json` or `$CLAUDE_CONFIG_DIR/settings.json`, and the project's `.claude/settings.json` and `.claude/settings.local.json`) and checks the original command:

- a `deny` or `ask` rule matches the command or any part of it: the command is left alone, so Claude Code applies that rule;
- your `allow` rules cover the command (every part of it; output filters like `| tail` need none): it is wrapped and approved, as it would have been;
- no rule matches: it is wrapped and Claude Code asks you, as it would have.

If a settings file cannot be read or parsed, the hook leaves commands alone.

**Never add an allow rule for kran itself** (`Bash(kran run:*)` or similar). It would approve every command the hook wraps, including ones you never allowed. The hook refuses to wrap a command such a rule would approve.

The rule: move what benefits, leave the rest.

| Moved | Left alone |
|---|---|
| `go test` (any flags), `go build ./...`, `go vet ./...`, `cargo test/check/clippy/nextest/build`, `pytest` and `python -m pytest`, `vitest`, `jest`, `playwright test`, `tsc -b/--noEmit`, `next build`, `golangci-lint run`, `ctest`, `cmake --build`, `make test/build/check/lint/ci`, `gradle`, `mvn`, `dotnet`, `swift`, `bazel`, `docker build`, `turbo`/`nx` over test/build/lint, and the `npm\|pnpm\|yarn\|bun` forms (`npm test`, `pnpm build`, `pnpm -r test`, `npx vitest run`) | Dev servers and watchers (`next dev`, `vite`, `docker compose up`, `--watch`, `pytest --looponfail`), installs (`npm install`, `go mod tidy`), git, interactive programs, background tool calls, anything mentioning `localhost`, heredocs, command substitution (except `make -j$(nproc)`), `sudo`/`ssh`, quick commands (`gofmt -l`, `go vet ./pkg`, `ls`) and `make` with no target |

Also left alone because the point of the command is a result on this machine: `go build .` and `go build ./cmd/x` (they write a binary here), `go build -o file`, `go test -coverprofile/-cpuprofile`, `pytest --junitxml/--cov-report=html`, `--fix`, `--write`, `-u`/`--updateSnapshot`, `docker build --load/-o/--push`, output redirected to a file, `tee file`, and a pipe into the heavy command. Filters after the heavy command (`| tail -n 20`, `| grep`, `| head`) are fine, and so are `cd dir &&`, `VAR=x`, `time`, `nice`, `timeout 600`, `set -e` and `echo`.

The hook does nothing when no kran daemon socket exists, so an agent never sees a `kran:` line on every call on a machine where kran is not running. Disable it temporarily with `KRAN_NO_HOOK=1`.

## For agent authors

- Output is passed through byte for byte; only stderr lines starting with `kran:` are kran's.
- `kran run --json-events` prints progress as one JSON object per line on stderr (`{"kran":"decision",...}`, `phase`, `started`, `notice`, `fallback`, `exit`).
- `kran status --json` and `kran jobs --json` describe the mesh and history (`reason` says why a job ran where it did).
- Exit `125` means kran failed after the command started; rerun with `kran run --local`.
- Set `KRAN_OWNER=claude:<session>` (or `CLAUDE_SESSION_ID`) so the scheduler can share capacity fairly between sessions.
