# NOTES-T1: system-level tests (e2e, chaos, real-machine smoke)

Branch `agent/t1-e2e-chaos`. Test code lives in `internal/e2e`; fixes for defects the suites found
went into the owners' packages (one commit per fix, each with a failing test first).

## Suites and how to run them

| Suite | Build tag | Run | Duration on gx10 |
|---|---|---|---|
| in-process e2e (existing) | none | part of `go test ./...` | ~15 s |
| black-box e2e | `e2e` | `make e2e` (`KRAN_TEST_DOCKER=1` adds container tests) | ~12 min with `-race` |
| chaos | `chaos` | `make chaos [CHAOS_DURATION=3m] [CHAOS_SEED=n]` | duration + ~1.5 min |
| real-machine smoke | `smoke` | `make smoke NODE=devmac` | see the report |

The default `go test ./...` is unchanged: tagged suites do not compile without their tag.

* **Harness** (`cluster_test.go`): TestMain builds the real `kran` once (`KRAN_E2E_RACE=1` with
  `-race`, `KRAN_E2E_BIN` to use a prebuilt one). Every node is a `kran daemon` process with its own
  `KRAN_{CONFIG,STATE,RUNTIME,CACHE}_DIR` and `KRAN_NAMESPACE=kran-e2e`, reached through an
  in-process SSH server per node whose forced command execs the real `kran serve --stdio`, behind a
  TCP proxy that can cut, refuse or stall the link (also after N bytes). Everything is driven through
  the CLI. Teardown fails a test on any process still carrying the cluster's dirs, any
  `kran-e2e-job-*`/`kran-local-*` unit of its jobs, and any race report of a kran process
  (`GORACE=log_path`). `KRAN_E2E_KEEP=1` keeps the dirs of a failed test.
* **Chaos** (`chaos_test.go`): 3 nodes, seeded schedule of 5 job shapes from 4 owners, faults: CLI
  SIGKILL/SIGINT/double SIGINT, daemon SIGKILL (+restart) and graceful restart, supervisor SIGKILL,
  link cut, stall, outage longer than the lease. Invariants: see the file header. Prints a summary
  table and, on failure, the fault timeline; `KRAN_CHAOS_SEED` replays.
* **Smoke** (`smoke_test.go`): dev install in `/tmp/kran-smoke-*` here and `~/kran-dev/t1` on the
  node, namespace `kran-dev`, link tag `kran-dev`, UI on a free port. Runs `go test` of this repo on
  the node, checks status/jobs JSON and the UI, unlinks, then checks both `authorized_keys` are
  byte-identical, nothing `kran-dev` is left (processes, containers, volumes, launchd) and the real
  install's daemon and containers are unchanged.

## Changes outside internal/e2e (for the owners)

* **daemon (A)**: `Deps.Debug` (`KRAN_DEBUG=1`): `GET /api/v1/debug/goroutines` and
  `/api/v1/debug/slots`, unix socket only. `KRAN_NAMESPACE` sets the execenv namespace and the
  managed slice (`<ns with _>.slice`) so a dev/test daemon never shares `kran.slice`, scopes,
  `kran-sync` or volumes with a real install. `newSliceController` takes the slice name.
* **execenv (B)**: `(*manager).BusySlots()` and `(*process).Running()` (concrete types only, used
  through optional interfaces; the `Manager`/`Process` interfaces are unchanged). The supervisor
  starts the command after a setup only once a newer heartbeat arrived.
* **link (E)**: `InstallEnv()`, `PeerKeyLineEnv()`: forced commands of an install placed by
  `KRAN_*_DIR`/`KRAN_NAMESPACE` carry those variables. Normal installs keep the plain line.
* **scheduler (C)**: `Reject` with an `*AdmitError` of code `busy`/`max_jobs` neither trips the
  breaker nor uses up a remote attempt.

## Open issues

See the report in the branch's last commit message / lead report; in short:
* `TestOriginRestartWithoutCLICancels` (daemon) times out now and then under heavy CPU load
  (in-memory links miss 3 pings); seen 2 of ~8 full-package runs on this branch, 0 of 2 on main.
* Once after the reboot a daemon start took > 30 s (Docker CLI probes timing out); a CLI that tries
  to reattach after a daemon restart only retries for 10 s. The harness now waits 60 s.
* A `--remote`/`-n` job whose only target is draining or below its disk floor waits (with the
  reason) instead of failing with 125; consistent, but an agent may wait forever.
* `submit.prefer = "remote"` in config.toml behaves exactly like `--remote` (no fallback when no
  peer can take a job); only origin-only jobs are exempt now. Decide whether it should be soft.
* `*.key` files are hard-excluded from sync (secrets), also when a project uses one as setup key.
* Under a terminal, remote tty jobs run with the local terminal in cooked mode: with `-i`, typed
  input is echoed twice (local tty and remote pty).
