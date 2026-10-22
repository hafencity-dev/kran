# Troubleshooting

Run `kran doctor` first. Add `--fix` to repair permissions, a missing key, and the service. `kran doctor <node>` focuses on one machine; `--json` is machine-readable.

## The daemon

- `kran service status` shows whether it is installed and running. Linux logs: `journalctl --user -u kran`. macOS: `~/Library/Application Support/kran/daemon.out.log`. Both: `daemon.log` in the state dir.
- `kran service restart` waits for running jobs; `--force` restarts immediately (running remote jobs reattach if the restart is quick).
- On Linux the service stops at logout unless lingering is on: `loginctl enable-linger $USER`.
- No user systemd bus over SSH ("Failed to connect to bus"): `export XDG_RUNTIME_DIR=/run/user/$(id -u)` and retry.

## Linking

- `kran link` needs working `ssh <host>`. Try it by hand first; the error is then yours to read.
- "Linked one direction": the other machine cannot reach this one. Enable Remote Login / start sshd here, make sure the address is reachable (Tailscale helps), then run `kran link <host>` again.
- Wrong or missing kran binary on the other side: `kran link` installs it from the binary cache (`make install` fills it), a GitHub release (needs `gh auth login` or `GITHUB_TOKEN`), or a source build when run from a checkout.
- `kran unlink <node>` undoes a link on both sides. If the other machine is unreachable it tells you which lines to remove from its `authorized_keys` (they end in `kran@<node>`).

## Jobs

- Everything ran locally: `kran status` shows why nodes were skipped (offline, draining, different OS, missing toolchain, disk floor). Waiting jobs print the reason after two seconds.
- A job hit its memory cap: the message names the cap and the flag, e.g. `rerun with --mem 12G`. kran learns the peak for next time.
- Files created by a remote job do not appear locally. Use `kran run --local` for commands whose output files you need here.
- Mac jobs for Linux projects need a container runtime: `colima start` or Docker Desktop.

## Starting over

`kran service uninstall`, then remove `~/.config/kran` and the state directory (see [config.md](config.md)). `kran unlink` first for each machine keeps their `authorized_keys` clean.
