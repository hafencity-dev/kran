# NOTES-E (cli, config, link, service, agents, doctor, release, docs)

## Wiring the lead must do on merge

1. `cmd/kran/main.go` (kran top lives in D's package, which is a stub in this tree):
   ```go
   cli.DefaultHooks.Top = tui.Run   // func(ctx, client.Client) error
   ```
   Nothing else is needed: `daemon.Run`, `transport.ServeStdio`, `execenv.Supervise`, `client.Dial` and
   `workspace.{Detect,LoadProjectConfig,Toolchains,DefaultSetup}` are called directly from `internal/cli`.
2. `kran ui` only prints/opens `http://<ui.listen>/` (token fragment for non-loopback). It needs the
   daemon to write the token to `paths.TokenFile()` (SPEC §9) and serve `ui.Handler()`. Nothing in cli
   imports `internal/ui`.
3. gofmt: `make lint` fails on files owned by others (`internal/proto/*.go`, `internal/workspace/*.go`,
   `internal/daemon/daemon.go`, `internal/execenv/execenv.go`). Run `gofmt -w` once after merging.

## What E needs from the others (contracts the CLI relies on)

* **A, client.RunSession / daemon**: after `submit` the daemon sends `decision` (local or remote).
  For local placement the CLI answers `started` then `exit` frames (as SPEC). If a remote attempt
  fails before `running`, the daemon sends a second `decision{mode:"local"}` on the same session; the
  CLI marks the final `exit.Fallback`. A `FrameError` before the command started makes the CLI run
  locally (unless `--remote`/`--no-fallback`); after start it exits 125. After a dropped connection the
  CLI redials for 10 s, opens a new session and sends `reattach{job, offset}` as its first frame (offset =
  stdout+stderr payload bytes received). The daemon must accept `reattach` as the first frame of a session.
  Heartbeats: CLI sends one every 2 s and treats 10 s without any frame as a dead connection.
* **A, `Version.API`**: the CLI falls back to local execution if `VersionInfo.API != proto.APIVersion`.
* **A/B, `kran internal supervise`** is routed to `execenv.Supervise(args)` with `args` = everything after
  `supervise` (flag parsing disabled).
* **Service**: `kran service restart` waits for no queued/syncing/setup/running jobs in `State.Jobs`
  (`--force` skips). `kran link` (remote side) and `kran upgrade` call it over ssh.
* **Env**: `KRAN_NO_SERVICE=1` makes `kran setup` skip the service (used by dev/test setups).

## Interface requests (none blocking)

* proto: no changes made.
* config: added `Peer.PreferredAddress`, `RemoteConfigDir`, `OS`, `Arch`, `Tag` (all optional `toml` fields).
  The daemon should try `PreferredAddress` first and update it when another address wins.
* `<ConfigDir>/links/<name>.json` is link's private per-peer state (ssh destination/args, remote env, the
  peer key we authorized). Do not delete it; `kran unlink` uses it to reverse a link exactly.
* A: `kran serve --stdio` is what authorized_keys forces. Its exit code is returned as is.

## Verified on real hardware

Against `devmac` (Darwin arm64) with `KRAN_*_DIR` overrides on both ends, tag `kran-dev`:
link (cross-compile darwin + linux binary push, remote setup, key exchange, both-direction handshake,
rtt/bandwidth), second link (idempotent, "Already linked"), unlink; afterwards `authorized_keys` on both
machines were byte-identical to before and no launchd job or dev files were left.
Finding: devmac has no GUI login session (`gui/501` missing), so launchd refuses user agents
(`launchctl bootstrap user/501 ...: Input/output error`). `kran setup` then says so and continues; on such a
headless Mac the daemon needs a console login, auto-login, or `kran daemon` under another supervisor.
`kran doctor` reports "installed but not running".

## Not done / limits

* Windows is not supported (link rejects it with a message).
* `kran link` for a Linux target from a machine without a source checkout, cache or gh/token cannot get
  the binary; the error lists the three options. `make install` fills the cache for all four platforms.
* `goreleaser` and GitHub Actions were not executed here (no goreleaser, no CI); YAML parses and
  `install.sh` passes shellcheck.
* mDNS discovery for `kran link` is not implemented; ssh config, Tailscale and known_hosts are.
* The hook's `updatedInput` without `permissionDecision` was verified with `claude -p` 2.1.308.

## Tests

`go test -race ./internal/{cli,config,agents,service,doctor,link}`; `go vet` for linux and darwin.
The link suite takes about a minute (8 MiB bandwidth probe through a fake ssh).
