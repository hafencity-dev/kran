# kran

kran moves heavy dev jobs (tests, builds, type checks, docker builds, GPU runs) to whichever of your machines has room, and never makes a job worse than running it locally. Output, colors and exit codes are the same as running the command yourself. If anything in kran fails before your command starts, the command runs locally.

## Quick start (60 seconds)

The repository is private, so downloads go through the GitHub CLI (`gh auth login`) or a `GITHUB_TOKEN`.

```sh
# 1. install (puts kran in ~/.local/bin and runs `kran setup`)
gh api repos/hafencity-dev/kran/contents/install.sh -H 'Accept: application/vnd.github.raw' | sh

# 2. link a second machine (uses your normal ssh setup; installs kran there too)
kran link devmac          # or just `kran link` and pick from a list

# 3. run something heavy
kran run -- go test ./...
```

`kran setup` creates this machine's identity and SSH key, installs the background service (systemd user unit or launchd agent), and offers to link machines and to teach Claude Code and Codex about kran. Running it again is harmless.

`kran link` works over plain `ssh`, so your SSH config, agent and jump hosts just work. It installs the matching kran binary on the other side, creates a dedicated key restricted to one forced command, pins the host key, and checks both directions. If only one direction works it says so in one sentence and how to enable both.

## Everyday use

```sh
kran run -- cargo test                  # runs where it fits; same output, same exit code
kran run --sh 'pnpm i && pnpm build'    # a shell string
kran run -n devmac -- docker build .    # pin a node
kran run --local -- npm run dev         # never leave this machine
kran status                             # nodes, free CPU/memory, queue
kran top                                # live terminal view      kran ui   # web dashboard
kran jobs / logs -f / cancel <id>       # history and control
kran drain devmac / resume devmac       # stop/start sending jobs to a node
```

Flags for `kran run`: `-l/--local`, `-r/--remote`, `-n/--node`, `--gpu`, `--cpus`, `--mem`, `--any-os`, `-i/--stdin`, `-q/--quiet`, `--json-events`, `--priority`, `--label k=v`, `--no-fallback`, `--env NAME`.

Exit codes: the command's own; `125` kran failed after your command started (or `--remote`/`--no-fallback` could not be honored); `126`/`127` not executable / not found; `128+n` killed by signal n; `2` usage error.

## How kran decides

1. The job is described: command, working tree, filtered environment, owner (human, Claude, Codex, CI), resource needs (from flags, `.kran.toml` rules or learned estimates).
2. Jobs that must stay here run here: working directory outside a project, commands that mention `localhost`, `--local`, rules marked local.
3. Otherwise each node is checked: online, accepting, same OS (a Mac offers a Linux container), GPU, toolchains, disk, and free CPU and memory after its owner's reserve. Memory is never overcommitted.
4. Among the nodes with room, the one with the earliest expected finish wins: wait + upload + setup + duration scaled by that node's learned speed. Short jobs and jobs that cost more to sync than they save stay local.
5. Your files are synced into a slot on the target (only changed files, content-addressed), the command runs under a supervisor, output streams back, and the exit code is returned. Secrets and `.env` files are never synced; host-specific and secret-looking environment variables are not forwarded unless you ask (`--env NAME` or `.kran.toml`).
6. Waiting jobs say why on one stderr line (`kran: waiting: needs 6 GiB, devmac has 3.1 GiB free`), only after two seconds.

Per-repo hints go in `.kran.toml`; machine settings in `~/.config/kran/config.toml`. See [docs/config.md](docs/config.md).

## Coding agents

```sh
kran agents install
```

Installs the kran skill for Claude Code, a short managed block in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`, and a Claude Code `PreToolUse` hook that wraps heavy Bash commands (`go test`, `cargo build`, `next build`, `vitest`, `pytest`, `docker build`, `make test`, ...) as `kran run --sh '...'` automatically. Dev servers, watchers, interactive and background commands are never touched. `kran agents uninstall` removes exactly what was added. Details in [docs/agents.md](docs/agents.md).

For scripts and agents: `kran status --json`, `kran run --json-events` (progress as JSON lines on stderr), and the exit codes above.

## Troubleshooting

Start with `kran doctor` (add `--fix` to repair what is safe to repair; `kran doctor devmac` focuses on one node).

| Symptom | Likely cause and fix |
|---|---|
| `kran: daemon not running — running locally` | `kran service start`, or `kran setup` if never installed. Jobs still run, just not offloaded. |
| Node shows `offline` in `kran status` | `ssh <host>` works? Then `kran link <host>` repairs keys. `kran doctor <node>` shows the reason. |
| Stops accepting jobs after logout (Linux) | `loginctl enable-linger $USER` |
| Mac without GUI login: no launch agent | Works on demand; make it permanent with `kran service install --system` |
| Jobs on a Mac fail with a container error | Start Docker: `colima start` or Docker Desktop. |
| Slow syncs | `kran doctor` reports link speed; prefer Ethernet or a direct Tailscale path. |
| Host key changed | Reinstalled machine: `kran link <host>` pins the new key. Otherwise treat it as an attack. |

More in [docs/troubleshooting.md](docs/troubleshooting.md).

## Security model

- No listening ports except the dashboard on `127.0.0.1:7270` (non-loopback binds require a token).
- Machines talk over SSH using a dedicated ed25519 key (`~/.config/kran/id_ed25519`). On the peer, `authorized_keys` pins that key to `command="<kran> serve --stdio",restrict`: no shell, no forwarding, no pty. The host key is pinned at link time.
- Sockets and state are `0600`/`0700`. Jobs run as your user, never root.
- `kran unlink <node>` removes exactly the keys and entries `kran link` added on both sides.

## Development

```sh
make build        # bin/kran
make test         # go test -race ./...
make lint         # gofmt, go vet (linux + darwin)
make install      # install here and fill the binary cache for linux/darwin x arm64/amd64
```

The binary cache (`~/.cache/kran` or `~/Library/Caches/kran`) lets `kran link` install kran on machines of another OS or architecture without network access. Release builds come from tags via GoReleaser. The engineering contract is in [SPEC.md](SPEC.md); contributor rules in [AGENTS.md](AGENTS.md).
