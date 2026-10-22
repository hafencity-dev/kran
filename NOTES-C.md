# NOTES-C: scheduler, metrics, estimate, store

Owner: C. Branch `agent/c-sched`. Every package is implemented, and `go test -race` passes on
gx10 (linux/arm64) and devmac (darwin/arm64). `go vet` is clean for
linux and darwin on arm64 and amd64.

## Shared changes (additive only)

### `internal/proto`
* `NodeState.MaxJobs int` holds config `accept.max_jobs` (0 = unlimited). Origins use it to respect a peer's job limit.
* `NodeState.AllocatedJobs []string` lists the job IDs counted in `Allocated`. A node fills it
  from its own `Snapshot().Jobs[self]`. With it, an origin can tell which of its placements a peer's report
  already includes, so nothing is counted twice. Without it, the origin takes the larger of the report and its own
  placements, and the target's `Admit` catches the rest.

### `go.mod`
`modernc.org/sqlite` (store) and `golang.org/x/sys` (metrics, darwin) are now
imported directly. They are still marked `// indirect`, which builds fine. `go mod tidy` at merge
time will drop the comment. No new modules were added.

## Interface additions in my packages

### scheduler (`scheduler.go`)
* `UpdateJobUsage(jobID, live proto.Resources)`: feed it every ~2 s for running jobs.
  Admission then knows which memory a job already uses (and the node's live metrics already show) and which it
  may still grow into. Without it, a job counts as fully pending for 10 s (local) or 60 s (remote)
  after placement and as visible after that.
* `Admit(job, runtime) (Placement, error)` is the target side of `proto.PeerAdmit`. It never
  queues. It returns `*AdmitError{Code, Reason}` with these codes: `busy`, `draining`, `not_accepting`,
  `disk_floor`, `no_runtime`, `max_jobs`, `unknown`. These map straight onto `PeerResponse.Code`.
  Repeated calls for the same job return the same placement. Free the reservation with `Release(jobID, …)`.
* `Snapshot` adds:
  * `Jobs` (node → job IDs)
  * `Unavailable` (node → reason, set while the failed-start cool-down is active)
  * `Placements` (job → Placement)
* `Options` adds:
  * `SyncCost func(repoID, node) float64`. Optional; otherwise the cost is learned from `Release` usage.
  * `StarvationAfter` (default 2 min).
  * `MaxRemoteAttempts` (default 2).
* Errors:
  * `ErrCanceled`
  * `*UnplaceableError{Reason}`, a user-facing sentence. Exit 125 for
    `--remote`/`--no-fallback`; otherwise the CLI falls back to local.
  * `*AdmitError`

### estimate
* `NewWithOptions(st, Options{Self, Score func(node) float64, Logf})`. `Self` is this node's name.
  `Score` returns a node's `SingleCoreScore` and is used as the speed prior. `New(st)` still works; it
  infers `Self` from observed jobs and has no benchmark prior. The scheduler then applies
  its own prior.
* `LabelReqSource = "kran.req_source"`: E sets the label to `"rule"` when `Req.CPUs/Mem` came from a
  `.kran.toml` rule. It only changes `Estimate.Source` (`"rule"` instead of `"flag"`).
* `Speed` returns exactly 1 when nothing is known. The scheduler treats exactly 1 as "unknown"
  and falls back to the SingleCoreScore ratio.

### store
* `ErrNotFound` (wrapped by `GetJob`).
* `ListJobs`: `Owner` matches the owner ID or display name. `Node` matches the target node, or the
  origin for jobs that were never placed. Results are newest first, with a default limit of 100.
* `PruneJobs(olderThan)` never deletes active jobs. It also prunes old estimate samples but keeps the
  newest 20 per fingerprint. Samples are capped at 50 per fingerprint when inserted.
* Other packages can create their own tables via `DB()`. The handle runs in WAL mode with a 10 s busy timeout, and
  transactions begin as `IMMEDIATE`.

### metrics (exported helpers beyond `Prober`)
* `CgroupStats(path)` returns memory now, memory peak and CPU seconds for a local job's final usage.
* `ProbeDocker(ctx)` returns the engine's CPUs and memory, or a reason it is unreachable ("Docker not
  reachable: …"). Good for `Runtime.Reason`.
* `DockerBinary()`, `CgroupRoot`, `SampleInterval`.
* `Info.Name` is the short lowercased hostname; the daemon should override it with the configured name.
  `Info.ID` and `Info.Runtimes` are left for the daemon to fill (from execenv).
* `JobUsage` for a container may take about 2 s on macOS when the cached `docker stats` result is older than 10 s.
* On Linux, process-group usage is read from /proc rather than `ps -g`, because `ps -g` selects sessions on Linux.

## Wiring (for A, daemon)

1. **Start**
   * `st := store.Open(paths.DBFile())`.
   * `pr := metrics.New(Options{StateDir, Logf})`, then `go pr.Run(ctx)`.
   * `info := pr.Info(ctx)`. Override the name and ID, and set `Runtimes` from `execenv.Runtimes`.
   * `est := estimate.NewWithOptions(st, {Self: name, Score: peerScore})`.
   * `sch := scheduler.New({Origin: name, Speed: est.Speed, Now: time.Now, Logf})`.
2. **Nodes.** Call `UpdateNode` for self on every metrics sample (2 s), filling:
   * `Info`, `Metrics`, `Status` and `Accepts`
   * `Reserve` (config reserve_cpus/mem)
   * `Capacity`: optional. Zero means Info minus Reserve; if you set it, it is the amount *after* the reserve.
   * `MaxJobs`

   Call `UpdateNode` for each peer on every update from its `metrics.subscribe` stream, using the
   peer's reported NodeState plus `Link`. **Register every peer from peers.toml (status offline) before
   accepting submits**, so that `--node X` can be checked and GPU jobs do not fail fast while a
   GPU peer is still connecting. Call `RemoveNode` on unlink.
3. **Publishing self to peers.** Take `Allocated`, `Free` and `Running` from `Snapshot()` for self, and set
   `AllocatedJobs = Snapshot().Jobs[self]`.
4. **Submit**
   * `job.Fingerprint = est.Fingerprint(&spec)`
   * `job.Estimate = est.Estimate(&spec, fp, selfInfo.CPUs)`
   * `job.QueuedAt = now`, `job.Origin = self`
   * `tk := sch.Submit(job)`

   Leave `Spec.Priority` empty when no flag was given: the scheduler then uses high for TTY jobs and
   normal otherwise. Relay `tk.Updates()` (coalesced, closed on resolve) as `notice{queued}` frames;
   the CLI prints after 2 s. `tk.Wait(ctx)` returns the `Placement`.
   * For `Decision`, use `Mode` from `Placement.Local` and `Reason` from `Placement.Reason`.
   * The envelope's memory cap comes from `Placement.Alloc` (cap = 2× Alloc.Mem, at least 2 GiB, SPEC §6.8).
   * If `Wait`'s ctx ends (CLI gone), call `Cancel`.
5. **Remote placement.** Send `PeerAdmit` to the target, whose daemon calls `sch.Admit(job, runtime)`.
   If admission fails, or anything before `running` fails (sync, setup, start), call
   `tk = sch.Reject(jobID, node, err)` and wait on the new ticket. The re-placement excludes
   that node for this job. After `MaxRemoteAttempts` failed remote attempts the job falls back to the origin with
   `Placement.Fallback = true`. With `--remote` or `--no-fallback`, the ticket resolves with
   `*UnplaceableError`.
6. **Running.** Call `sch.UpdateJobUsage(id, live)` every ~2 s:
   * local jobs: `metrics.JobUsage(UsageRef{CgroupPath})` or the PGID
   * remote jobs: `Process.Live()`
7. **End**
   * `sch.Release(id, &usage)`. Include `SyncSeconds` and `SetupSeconds`, which teach sync cost per repo and node. A
     release with `WallSeconds > 0` also resets that node's failed-start breaker.
   * `est.Observe(&job)` with the terminal job (State, Usage, Node, Local, Origin, EndedAt).
   * `st.PutJob`.
   * `Release` on a job that is still queued counts as a cancel.
8. **Periodic work.** Call `sch.Tick(time.Now())` every 1–2 s (starvation boost, breaker cool-down). Call
   `st.PruneJobs(now-7d)` daily.
9. **State and UI**
   * `State.Queue = Snapshot().Queue`
   * `Job.Reason` for queued jobs = `Snapshot().Reasons[id]`, `QueuePos` = index+1
   * Placed jobs carry `Placement.Reason`.
   * `Unavailable[node]` can be shown as the node's reason while its status is online.
10. **Breaker.** The scheduler counts consecutive `Reject`s per node: after 3, the node is skipped for 30 s.
    The cool-down doubles on each failed retry, up to 5 min, and resets after a job runs there. The daemon can
    also set a node `Degraded` with a `Reason`; the reason is shown verbatim as `devmac: <reason>`.

## Behaviour other owners should know

* Reason strings are shown verbatim, for example:
  * `waiting: needs 6.0 GiB, devmac's Linux VM has 3.1 GiB free, gx10 has 1.2 GiB free`.
    A Linux job on a Mac is limited by the Colima VM, so the reason names the VM.
  * `waiting: needs 8 CPUs, gx10 has 2 free`
  * `devmac: draining`
  * `devmac: no linux runtime (Docker not reachable)`
  * `only gx10 has an NVIDIA GPU (gx10: offline)`
  * `waiting: needs 24.0 GiB, gx10 has 10.0 GiB free (only gx10 has an NVIDIA GPU)`
  * `waiting: gx10 should free up in ~5s; est. 1m there vs 1m29s on devmac (Linux container)`
  * `waiting: est. 3s, too short to send elsewhere; needs 1.0 GiB, gx10 has 512 MiB free`
  * Starving jobs append `; next in line on gx10`.
  * Placement reasons look like `devmac (Linux container): 15.8 CPUs free, est. 1m5s; here: needs 4.0 GiB, gx10 has 2.0 GiB free`
    and `here: est. 1m; devmac (Linux container) would take 48s plus ~17s to sync`.
* Placement decisions:
  * Jobs estimated under 5 s stay on the origin. They are admitted on memory alone, with CPU allowed
    up to 2× capacity, because running them locally without kran would share the CPUs too.
  * Offloading must beat running here by more than max(1 s, 5%), sync and setup included.
  * A job may wait up to 60 s for a clearly better node (one that would finish in under 80% of the time).
* Fair queue:
  * Weighted fair queueing across owners (high 4, normal 2, low 1); an owner returning from idle gets no banked credit.
  * Within an owner, jobs go by priority, then FIFO.
  * A job that fits nowhere never blocks others. After 2 min, a job is boosted and holds its best node
    (later jobs cannot take the freed resources), so a stream of small jobs cannot starve a big one.
    Holds also apply to `Admit`.
* Oversized jobs: a job larger than every node runs alone on the largest eligible node, once that node has no kran jobs.
* Eligibility:
  * Toolchains: a runtime that reports no toolchain map counts as capable. The container runtime always counts as
    capable (mise). Native: `go` present is enough (GOTOOLCHAIN); otherwise the pin must match, or mise must be present.
  * Disk floor for remote targets: max(10 GiB, 5% of DiskTotal) free.
  * A draining origin still runs `--local`, origin-only and fallback jobs.
  * `--gpu` fails fast only when every known node has been probed and none has a matching GPU.
* Estimates are in origin seconds, and `Speed` is the duration multiplier relative to the origin.

## Simulator results (`go test -v -run Simulate ./internal/scheduler/`)

The scenario: gx10 (20 cores, 128 GB, CUDA) is the origin, carrying the agents' own load (≈3 CPUs, 26 GB).
devmac (28 cores, 96 GB) runs its product install (2 CPUs, 30 GB) and a Colima VM with 16 CPUs / 32 GB.
Five agent sessions, each a closed loop, submit bursts of 1–4 jobs: go test, go build, next build, vitest, tsc,
docker build, pytest, lint, and vLLM-GPU (28 GB). Each run covers 1 h of arrivals and then drains.

| | gx10 + devmac | gx10 only |
|---|---|---|
| jobs completed | 220 | 120 |
| offloaded | 116 | n/a |
| queue wait p50 / p95 / max | 0s / 1m4s / 2m9s | 1m34s / 5m50s / 7m |
| turnaround mean / p95 | 1m38s / 3m48s | 3m43s / 7m59s |
| CPU allocated (real use) | gx10 64% (59%), devmac 44% (37%) | gx10 77% (69%) |
| memory overcommit / OOM samples | 0 / 0 | 0 / 0 |

Mean wait per session in the mesh ranges from 7 s to 18 s. All GPU jobs ran on gx10, and no Linux job ran
natively on macOS. The devmac allocation is capped at 16 of its 26 schedulable CPUs, because Linux jobs must run in
the VM. The test asserts all of these bounds. Random-mix simulations over 6 seeds (3–8 sessions, heavier
load) all drain. Their worst wait is 17m45s, which is inherent: three sessions queue up 34 GiB GPU jobs that only gx10
can run, two at a time.

## Tests

* Property tests run random operation streams (submit, release, cancel, reject, metrics, status, tick,
  priority, admit, usage). After every operation they check:
  * memory allocation ≤ capacity in every pool (host and VM)
  * CPU allocation ≤ 2× capacity
  * every queued job has a reason
  * queue, owner and allocation bookkeeping is consistent

  Determinism is checked by running the same seed twice and comparing traces. `FuzzScheduler` was fuzzed for several minutes with no
  findings. `KRAN_PROPERTY_LONG=1` runs 20 seeds × 3000 steps.
* Dispatch takes about 0.14 ms per pass with 500 queued jobs and about 0.7 ms with 2000. Within a pass, the verdict for
  each job shape is reused. `Snapshot` takes about 50 µs.

## Not done / open

* `accept.nice`: the scheduler only places jobs. Nice levels are execenv's and the daemon's concern.
* Speeds are learned per node, not per runtime: container and native runs on the Mac share one ratio. Splitting them would need
  `store.Sample` to carry the runtime.
* Fairness counts jobs, not CPU-seconds. A session submitting docker builds gets the same share of
  dispatches as one submitting go tests.
