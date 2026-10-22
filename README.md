# kran

kran moves heavy dev jobs (tests, builds, type checks, docker builds, GPU runs) to whichever of your machines has room, and never makes a job worse than running it locally. Output, colors and exit codes are the same as running the command yourself. If anything in kran fails before your command starts, the command runs on this machine.

## Quick start (60 seconds)

You need: macOS or Linux (arm64 or amd64) on both machines, and `ssh <other-machine>` working from this one. The installer downloads from GitHub without a login.

```sh
# 1. Install on this machine. Puts kran in ~/.local/bin and runs `kran setup`.
curl -fsSL https://raw.githubusercontent.com/hafencity-dev/kran/main/install.sh | sh
#    Alternative with a Go toolchain: go install github.com/hafencity-dev/kran/cmd/kran@latest
#    then run `kran setup`.

# 2. Link a second machine (kran installs itself there).
kran link devmac          # or just `kran link` and pick from a list

# 3. Run something heavy, from any git repository.
kran run -- go test ./...
```

If the installer says `~/.local/bin` is not on your PATH, add the line it prints to your shell profile and open a new terminal.

`kran status` should now list both machines as `online`. `kran setup` creates this machine's identity and SSH key, installs the background service (systemd user unit or launchd agent) and, if Claude Code or Codex is installed, teaches them about kran (`kran agents uninstall` reverses that). It is safe to run again.

`kran link` uses plain `ssh`, so your SSH config, agent and jump hosts just work; with a terminal it asks once for a password or a new host key. It installs the matching kran binary on the other machine, creates a dedicated key restricted to one forced command, pins the host key and checks both directions. If only one direction works it says so and how to enable both. `kran link devmac` names the machine `devmac` on both sides.

## Everyday use

```sh
kran run -- cargo test                  # runs where it fits; same output, same exit code
kran run --sh 'pnpm i && pnpm build'    # a shell string: pipes, &&, cd
kran run -n devmac -- docker build .    # pin a machine
kran run --local -- ./build.sh          # stay here, e.g. when you need the output files
kran status                             # machines, free CPU and memory, queue
kran top                                # live terminal view        kran ui   # web dashboard
kran jobs [--why]                       # history; --why shows where each job ran and why
kran logs [-f] [job]   kran cancel <job>
kran drain devmac / kran resume devmac  # stop / start sending jobs to a machine
kran upgrade                            # update kran here and on every linked machine
kran doctor [--fix]                     # find and repair what is wrong
```

A job that runs on another machine works on a synced copy of your files: files it creates stay there. Use `--local` for commands whose output files you need here.

`kran run` flags: `-l/--local`, `-r/--remote`, `-n/--node`, `--gpu`, `--cpus`, `--mem`, `--any-os`, `-i/--stdin`, `-q/--quiet`, `--json-events`, `--priority`, `--label k=v`, `--no-fallback`, `--env NAME`, `--sh`. `kran run --help` has examples.

kran is quiet: it prints one `kran:` line on stderr only when that tells you something (running on another machine, waiting for room, falling back). Output, colors and exit codes are the command's own.

Exit codes: the command's own; `125` kran failed after your command started (or `--remote`/`--no-fallback` could not be honored); `126`/`127` not executable / not found; `128+n` killed by signal n; `2` usage error.

## How kran decides

1. **Describe the job.** Command, working tree, environment, who asked (you, Claude, Codex, CI) and what it needs, from flags, `.kran.toml` rules or what kran learned from earlier runs of the same command.
2. **Keep local what must stay local.** Working directory outside a project, a command that mentions `localhost`, `--local`, a `.kran.toml` rule, or no other machine linked.
3. **Check every machine.** Online, accepting jobs, same OS (a Mac offers a Linux container when Docker is running), GPU if asked, toolchains, disk, and free CPU and memory after that machine's owner reserve. Memory is never overcommitted.
4. **Pick the earliest finish.** Wait + upload + setup + the duration scaled by that machine's learned speed. Short jobs, and jobs that cost more to upload than they save, stay local.
5. **Run it.** Changed files are synced into a slot on the target (content-addressed, so repeats are cheap), the command runs under a supervisor, output streams back, and you get the exit code. `.env` files, keys and secret-looking environment variables are never sent unless you ask (`--env NAME` or `.kran.toml`).
6. **Say so when waiting.** A job that has to wait says why on one line (`kran: waiting: needs 6 GiB, devmac has 3.1 GiB free`), only after two seconds. `kran jobs --why` shows the reason behind every past placement.

Per-repository hints go in `.kran.toml`, machine settings in `~/.config/kran/config.toml`; see [docs/config.md](docs/config.md).

## Coding agents

```sh
kran agents install
```

Installs the kran skill for Claude Code, a short block in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`, and a Claude Code `PreToolUse` hook that runs long tests, checks and builds as `kran run --sh '...'` automatically (`go test`, `cargo test`, `pytest`, `vitest`, `pnpm build`, `make test`, `docker build`, ...). Dev servers, watchers, installs, quick commands and anything meant to leave a file on this machine are never touched. `kran agents uninstall` removes exactly what was added. Details in [docs/agents.md](docs/agents.md).

For scripts and agents: `kran status --json`, `kran jobs --json`, `kran run --json-events` (progress as JSON lines on stderr) and the exit codes above.

## Troubleshooting

Start with `kran doctor` (add `--fix` to repair what is safe to repair; `kran doctor devmac` focuses on one machine). Every finding says what to do next.

| Symptom | Likely cause and fix |
|---|---|
| `kran: daemon not running — running locally` | `kran service start`, or `kran setup` if never installed. Jobs still run, just not offloaded. |
| Everything runs locally | `kran status` shows why a machine was skipped (offline, draining, other OS, missing toolchain, low disk). `kran jobs --why` shows the reason for each job. Short jobs stay local on purpose. |
| A machine shows `offline` | Does `ssh <host>` work? Then `kran link <host>` repairs keys. `kran doctor <host>` shows the reason. |
| Mac with no GUI login (headless Mac Studio): no launch agent | kran works on demand: the daemon starts when a job or linked machine connects. To keep it running, `kran service install --system` (asks for your password once; preview with `--dry-run`). |
| Stops accepting jobs after logout (Linux) | `loginctl enable-linger $USER` |
| Syncs are slow | `kran doctor` reports the upload speed per machine. Wi-Fi is the usual cause: use Ethernet, or a direct Tailscale path (`tailscale ping <host>`). |
| Jobs on a Mac fail with a container error | Linux projects run in a container on a Mac: start Docker (`colima start` or Docker Desktop). Without Docker only native jobs can run there. |
| `docker: not installed` in `kran doctor` | Only matters on a Mac that should run Linux jobs, or for `docker build` jobs. Linux machines run native jobs without it. |
| `kran doctor` warns about a version | kran versions differ between machines. Run `kran upgrade`. The daemon keeps the old binary until `kran service restart`. |
| A file my job wrote is missing | The job ran on another machine. Rerun with `kran run --local`. |
| Host key changed | The machine was reinstalled: `kran link <host>` pins the new key. Otherwise treat it as an attack. |

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

The binary cache (`~/.cache/kran` or `~/Library/Caches/kran`) lets `kran link` install kran on machines of another OS or architecture without network access. Release builds come from tags via GoReleaser. Changes are listed in [CHANGELOG.md](CHANGELOG.md). The engineering contract is in [SPEC.md](SPEC.md); contributor rules in [AGENTS.md](AGENTS.md).

`kran top` uses Bubble Tea v2 and Lip Gloss v2, which do no terminal I/O at package init, so no kran command queries the terminal unless it opens the TUI.

## License

Apache License 2.0, see [LICENSE](LICENSE).
