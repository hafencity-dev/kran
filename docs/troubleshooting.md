# Troubleshooting

Run `kran doctor` first. Add `--fix` to repair permissions, a missing key, and the service. `kran doctor <node>` focuses on one machine; `--json` is machine-readable.

## The daemon

- `kran service status` shows whether it is installed and running. The daemon's log is `daemon.log` in the state dir (`~/.local/state/kran` on Linux, `~/Library/Application Support/kran` on macOS). If the daemon does not start at all, the reason is in `journalctl --user -u kran` (Linux) or `daemon.out.log` next to `daemon.log` (macOS, and the on-demand start).
- `kran doctor` says when the service definition is from an older kran; `kran doctor --fix` rewrites it (keeping your PATH), and `kran upgrade` does so on every machine.
- `kran service restart` waits only while a job is starting (syncing or setup); queued and running jobs carry over the restart and reattach. `--force` restarts immediately.
- On Linux the service stops at logout unless lingering is on: `loginctl enable-linger $USER`.
- macOS without a GUI login (a headless Mac Studio): launch agents cannot load there. kran does not need one: its daemon starts on demand when a job or a linked machine connects, and `kran doctor` reports this as `on-demand`. To keep the daemon running permanently, run `kran service install --system` (installs a launch daemon in `/Library/LaunchDaemons` that runs as you; asks for your password once; preview with `--dry-run`).
- No user systemd bus over SSH ("Failed to connect to bus"): `export XDG_RUNTIME_DIR=/run/user/$(id -u)` and retry.

## Linking

- `kran link` needs working `ssh <host>`. Try it by hand first; the error is then yours to read.
- "Linked one direction": the other machine cannot reach this one. Enable Remote Login / start sshd here, make sure the address is reachable (Tailscale helps), then run `kran link <host>` again.
- Wrong or missing kran binary on the other side: `kran link` installs it from the binary cache (`make install` fills it), a GitHub release (downloaded anonymously; `gh auth login` or `GITHUB_TOKEN` only help when GitHub rate limits you), or a source build when run from a checkout.
- `kran unlink <node>` undoes a link on both sides. If the other machine is unreachable it tells you which lines to remove from its `authorized_keys` (they end in `kran@<node>`).

## Jobs

- Everything ran locally: that is often right. `kran jobs --why` says why each job ran where it did ("too short to send elsewhere", "no other node has room", "not inside a project"). `kran status` shows why a machine was skipped (offline, draining, different OS, missing toolchain, disk floor). Waiting jobs print the reason after two seconds.
- Jobs outside a git repository, in your home directory or in `/tmp` always stay here; commands that mention `localhost` do too.
- A job hit its memory cap: the message names the cap and the flag, e.g. `rerun with --mem 12G`. kran learns the peak for next time.
- Files created by a remote job do not appear locally. Use `kran run --local` for commands whose output files you need here.
- Mac jobs for Linux projects need a container runtime: `colima start` or Docker Desktop. `kran doctor` reports `docker` as `not installed` or `not reachable`; without it that Mac only runs native macOS jobs. A Linux machine does not need Docker for kran.
- A tool is missing on the other machine ("missing toolchain node@24"): install it there, or install `mise`, which lets kran fetch the pinned version.

## Network and versions

- Slow uploads: `kran doctor` warns when a machine is below 10 MiB/s and says whether this machine's default route is Wi-Fi. Ethernet, or a direct Tailscale path (`tailscale ping <host>`) instead of a relay, is the fix. The first sync of a repository is the slow one; later runs send only what changed.
- Version skew: machines with different kran versions still work as long as the protocol matches; `kran doctor` warns about a difference and `kran upgrade` updates this machine and every linked one. A running daemon keeps its old binary until `kran service restart`.
- A machine that is `degraded` failed to start three jobs in a row and is skipped for a minute; `kran doctor <node>` shows the error.

## Agents

- `kran agents status` shows what is installed; `outdated` means `kran agents install` has newer text.
- A Claude Code command was not wrapped: that is the hook's rule for servers, installs, quick commands and anything that writes a file you want here (see [agents.md](agents.md)). The hook also stays out when no daemon is running.
- Turn the hook off for a moment with `KRAN_NO_HOOK=1`, for good with `kran agents uninstall`.

## Starting over

`kran service uninstall`, then remove `~/.config/kran` and the state directory (see [config.md](config.md)). `kran unlink` first for each machine keeps their `authorized_keys` clean.
