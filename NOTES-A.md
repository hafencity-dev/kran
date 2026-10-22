# NOTES-A (transport, daemon, client)

## Wiring the lead must do on merge

1. `internal/daemon/run.go`: set `UI: ui.Handler()` in the `Deps` literal (currently `nil`, which
   serves a small placeholder page from `internal/daemon/ui.go`; delete that file afterwards if you like).
2. Nothing else: `run()` already wires `store.Open`, `metrics.New`, `estimate.New`, `scheduler.New`,
   `execenv.New`, `workspace.NewScanner`, `transport.NewDialer` and E's `internal/config`. In this
   branch the C stubs panic, so `kran daemon` only runs once B and C are merged.
3. `execenv.Options.LinuxBinary` and `BlobCap`/`DiskFloor` are not set by `run()` (B decides defaults).

## Proto changes (additive, marked `// added by A`)

* `proto.QueueEvent{Queue}` — data of `EventQueue`.
* `proto.LogEvent{Level, Msg, Time}` — data of `EventLog` (not emitted yet).
* `proto.OutputChunk{Stream, Offset, B64}` — SSE event `output` of `GET /api/v1/jobs/{id}/log?follow=1`.

## Contracts others rely on (as implemented)

### For E (CLI)
* Run session: daemon sends heartbeats every 2 s in every phase. `client.RunSession` sends its own
  heartbeats, swallows incoming ones, and `Recv` fails with `client.ErrSessionTimeout` after 10 s
  (`client.RecvTimeout`) of silence.
* After a pre-start remote failure the daemon sends `notice{kind:"fallback"}` and then a second
  `decision{mode:"local"}` (or another `decision{mode:"remote"}` when a different node is tried).
  `--remote` or `--no-fallback` (when it would fall back) end with `exit{code:125, state:infra_error}`.
* `reattach{job_id, offset}` is accepted as the first frame of any session, for remote and local jobs,
  and for jobs that already ended (output from offset + exit). Unknown job → `FrameError`.
* Local jobs: the CLI's `exit` frame is final; disconnecting right after it is not a cancel. A CLI
  that disconnects without `exit` gets its scope (`kran*.scope` only) or process group killed
  (TERM, KILL after 5 s). Signals for local jobs are ignored by the daemon (the child has the tty).
* `signal` frames before the job runs cancel it (exit 130 for INT, 143 otherwise).
* `client.Dial` errors: `ErrNoDaemon` (missing/stale socket, or no answer within 3 s),
  `ErrPermission`, `ErrIncompatible` (API or protocol major differs). Extra: `client.APIError`,
  `client.IsNotFound`, and `JobsQuery(ctx, client.JobsQuery{Limit, Owner, Node})` via type assertion.
* `daemon.Options.Logf`: when set, the daemon does not additionally log to stderr itself.
* `kran serve --stdio` starts the daemon on demand (lead request): if the peer socket does not
  answer it waits 1.5 s (a service may be restarting it), then spawns `<exe> daemon` detached
  (`setsid`, stdin `/dev/null`, stdout/stderr appended to `<state>/daemon.stdio.log`, env inherited
  incl. `KRAN_*_DIR`), waits up to 5 s for the socket, then bridges. A flock on
  `<runtime>/daemon-start.lock` makes concurrent serve processes spawn one daemon.
  `KRAN_NO_AUTOSTART=1` disables it. Exit 69 = daemon not running, 77 = socket permission denied.
  Caveat: on Linux with logind `KillUserProcesses=yes` such a daemon dies with the SSH session.
* Node ID: read from `<ConfigDir>/node_id` (E's file); if missing the daemon generates one and keeps
  it in the store (KV `node.id`).
* `peers.toml` `preferred_address` is tried first; when another address wins it is written back with
  `config.UpdatePeers`. Changing it does not reconnect (it is excluded from the change check).

### For C (scheduler, store, estimate, metrics)
* `UpdateNode`: `Capacity` is already **net of allocations the scheduler does not own** (other origins'
  jobs on that node, and our own jobs reattached after a restart). Subtract only your own placements.
  `Allocated`/`Running` in the pushed state are the node's totals for display; please do not subtract
  them again. Self is pushed every 2 s, peers on every status message (2 s) and state change.
* Circuit breaker: the daemon owns it (3 pre-start failures → `NodeDegraded` with reason for 60 s); the
  scheduler only has to skip non-`online` nodes.
* `Reject(job, node, err)` is assumed to **free** the failed placement's allocation; the daemon does not
  call `Release` after `Reject`. `Release(job, nil)` is called when a placement raced a cancel.
* `Ticket.Updates()` may be nil; reasons are forwarded as `notice{kind:"queued"}` on change.
* `Tick` is called every 2 s from the daemon.
* Request: `Restore(job *proto.Job, p Placement)` so allocations of jobs reattached after an origin
  restart are tracked again (today the capacity adjustment above covers it).
* Store: every job transition is `PutJob`; `ActiveJobs` drives restart recovery; KV keys used:
  `node.id`, `drained` (JSON list of node names), `target.jobs` (JSON map, target-side records).
  `ListJobs` with `Since` = local midnight and `Limit` 100000 rebuilds today's stats at start.
* `estimate.Observe` is called for jobs that ended `succeeded` or `failed` (not canceled/infra).
* `metrics.JobUsage(UsageRef{PGID})` is sampled every 2 s for running local jobs (→ `Job.Live`).

### For B (execenv, workspace)
* `Process.Output(offset)`: offset counts **payload bytes of stdout+stderr frames** (same unit as
  `proto.Reattach.Offset`), not bytes of the framed file.
* `StartRequest.OnPhase` must report `JobRunning` when the user's command starts. Until then any
  failure (Start error, or the process ending with `infra_error`/`lost` before `running`) makes the
  origin fall back; after it, infrastructure failures end with 125.
* After a target daemon restart the daemon calls `Attach(id)` for `Active()` jobs and `ReleaseSlot`
  with the persisted `SlotRef` once `Wait` returns: execenv must keep those slots marked busy across
  the restart. `Attach` must also work for recently finished jobs (Output to EOF, Wait → exit).
* `Process.Stdin()` may return nil when `spec.Stdin` is false. `Live()` should be cheap (called
  every 2 s per running job, outside locks). `Renew` errors are only logged.
* Exit: `Code` 128+n and `Signal` "INT"/"TERM"/"KILL" for signals; the daemon fills `Node`.
* `job.cancel` sends TERM, then KILL after 5 s. `Nice` comes from `accept.nice`.
* The daemon also refuses `admit` below the disk floor max(10 GiB, 5 % of `DiskTotal`) using
  `metrics.Sample().DiskFree`; execenv may still refuse on its own.
* `workspace.Scanner.Open(hash)` is called concurrently from 4 upload workers.

### For D (UI)
* `GET /api/v1/events`: SSE with `id: <seq>`, `event: <type>`, `data: <proto.Event JSON>`; the first
  event is `state`; sequence numbers are contiguous per stream, a gap or a closed stream means
  "reconnect" (the daemon drops slow consumers). `node.metrics` ≤ 1/s per node. A `state` event is
  also pushed when peers are added or removed.
* Log follow: `GET /api/v1/jobs/{id}/log?follow=1&offset=N` → events `output` (`proto.OutputChunk`),
  then `exit` (`proto.Exit`) or `end`. Without `follow`: raw bytes. Local jobs have no captured output.
* UI listener auth: loopback → reads open, POSTs need `X-Kran-Token`, Host must be loopback;
  non-loopback (`tailscale` or an IP) → every `/api/` call needs the token (header, or `?token=` for
  GETs such as EventSource); static assets are public. Run sessions are refused on TCP.
* Token: `<state>/ui.token` (= `paths.TokenFile()`), 0600, created at start.
* In `State`, node `Capacity` is gross, `Allocated` all origins, `Free` from our scheduler.

## Design notes

* Peer protocol details are documented at the top of `internal/daemon/peerproto.go` (methods,
  stream phases, private frame types 100–104).
* Origin keeps an append-only per-job spool (`<state>/spool/<job id>`, run-session framing) that run
  sessions tail; the driver never blocks on a slow CLI, reattach is by output offset, logs read the
  same file. Spools are deleted after 7 days.
* Each active job has a daemon-owned driver goroutine; CLI sessions attach/detach. Last detach
  without exit = cancel (SPEC §6.1), except during daemon shutdown.
* Restart recovery: queued/syncing jobs are re-queued; setup/running remote jobs reattach at the spool
  offset (heartbeats resume at once); jobs whose CLI does not reattach within 20 s are canceled
  (local ones are marked `lost`).
* Reattach after a cut retries until lastContact + lease (15 s) + 5 s; then exit 125 "lost contact
  with <node> for 20s". A job stream silent for 10 s also closes the session (half-open detection
  faster than the 3×10 s keepalive).
* Target-side reservations (admit) expire after 30 s without `job.start` and are released at once
  when the admitting session closes; `admit` is serialized so two origins cannot overbook a node.
* `kran.slice`: MemoryMax = current + MemAvailable − reserve (cap MemTotal − reserve, floor 1 GiB),
  MemoryHigh=infinity, refreshed every 20 s and before each local placement, skipped when the change
  is < 64 MiB; disabled without a systemd user session (`systemd-run` + `systemctl --user`).

## Not done / limits

* `EventLog` events are not emitted (only the type exists).
* The chaos suite (`make chaos`) and `internal/e2e` are not written; the daemon package has an
  in-process multi-daemon harness (`harness_test.go`) with fakes that covers the SPEC §12 scenarios
  owned by A.
* Spool size is not capped (a job printing gigabytes uses that much disk on the origin for 7 days).
* Once, on macOS, a race report appeared in `TestRemoteRunOverSSH` while a since-fixed bug closed a
  session mid-stream; it did not reproduce in 60+ further runs on macOS and Linux.

## Tests

`nice -n 10 go test -race -p 4 ./internal/transport/ ./internal/client/ ./internal/daemon/` green on
linux/arm64 (gx10) and darwin/arm64 (devmac, `GOCACHE` must be private there:
`GOCACHE=$HOME/kran-dev/a-gocache`). `GOOS=darwin go vet` clean. The daemon suite takes ~30 s.
Covered: local/remote runs, stdin, signals, Ctrl-C while queued, fallback + breaker + cool-down,
`--no-fallback`, CLI killed mid-sync, target daemon restart mid-run, short cut / short stall / long
outage (lease), origin restart with and without CLI reattach, reattach after finish, local CLI death
kills the process group, two origins racing for one node, atomic admission, incompatible peer and
origin, reservation expiry, graceful shutdown < 5 s with live jobs, queue notices, peers.toml watch,
SSH end-to-end including a connection drop mid-job, autostart spawning exactly one daemon.
