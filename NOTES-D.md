# NOTES-D: web dashboard, `kran top`, mock daemon

Owner D. Packages: `web/`, `internal/ui` (+ `internal/ui/mock`, `internal/ui/mockserver`), `internal/tui`.
No changes to `internal/proto` or any other owner's interface.

## What exists

* `ui.Handler() http.Handler` serves the embedded build (`internal/ui/dist`, committed). It does the following:
  * falls back to `index.html` for extension-less paths (SPA);
  * returns 404 for missing assets and for `/api/*`;
  * sets `Cache-Control: immutable` on hashed `assets/*` and `no-cache` everywhere else;
  * sets explicit content types and a CSP on `index.html`;
  * allows only GET and HEAD.
* `tui.Run(ctx, client.Client) error` is `kran top`. Keys: tab panes, ↑↓ select, `c` cancel (asks first), `d` drain (asks) or resume, `p` cycle priority of a waiting job, `l` follow log, `q` quit. It works in 80×24, adds columns and a second machine line on larger terminals, and with `NO_COLOR` set the output contains no escape sequences.
* `internal/ui/mock` is a deterministic simulation of gx10, devmac and e2e (e2e drops offline now and then):
  * a `Sim` with the local-API HTTP handler (state, SSE events, jobs, log follow, cancel, priority, drain/resume, token rules);
  * `mock.Client`, a `client.Client` backed by the simulation, used by the TUI tests and by `-top`.
* `go run ./internal/ui/mockserver [-nodes 10 -jobs 200] [-token x] [-top]` serves the API and the built UI on :7271. `web/` `npm run dev` proxies `/api` there.
* Build: `make ui` (root Makefile) or `go generate ./internal/ui` runs `npm ci && npm run build` in `web/`. `ui.mk` (included by the root Makefile) adds `ui-mock`, `ui-dev`, `ui-shots`.
* Budget: JS is 24.5 KB gzipped (the target was under 100 KB), CSS 6 KB. One 90 KB variable font (Archivo, latin) is served from the binary, so nothing loads from the network.
* Measured with `web/scripts/perf.mjs` (10 nodes, 200 active jobs, about 120 events/s, 12× the target rate): 59.5 fps, zero long tasks, worst single frame 100 ms on this loaded host, 14 MB heap. No virtualization and no `content-visibility`: an earlier `content-visibility: auto` on queue and history rows left them unpainted in full-page captures, so it was removed.

## Wiring (lead / A / E)

```go
// daemon UI listener (127.0.0.1:7270 by default), same handler as the unix socket for /api
mux := http.NewServeMux()
mux.Handle("/api/v1/", apiHandler)   // A
mux.Handle("/", ui.Handler())        // D
```

* The UI is same-origin; no CORS is needed. Please **check the `Host` header** on the TCP listener (allow `127.0.0.1:7270`, `localhost:7270` and the configured bind address). Otherwise a DNS-rebinding page can read `/api/v1/state` from a loopback UI that has no token.
* Token (SPEC §9): the UI reads `#token=` and keeps it in sessionStorage. It sends `X-Kran-Token` on every fetch and `?token=` on EventSource URLs, because EventSource cannot set headers. So with a token configured:
  * GETs must accept `?token=`;
  * mutations must require the header (the mock does exactly this);
  * a 401 or 403 should carry `APIError{error, hint}`, and the UI shows a "needs a token: run kran ui --print-url" banner.
* `kran ui` (E) opens `http://127.0.0.1:7270/` (plus `#token=…` when a token is configured). `kran top` (E) dials the client and calls `tui.Run(ctx, c)`. If the daemon is down, E prints the usual "start it with kran service start" line instead of opening the TUI.
* The TUI uses Bubble Tea v2 (`charm.land/bubbletea/v2`) and Lip Gloss v2, which do no terminal I/O at package init (v1 queried the terminal background on every kran command with a tty).

## SSE contracts the UI relies on (A, please confirm or tell me to adapt)

1. `GET /api/v1/events`:
   * each message is `data: <proto.Event JSON>`, and `id: <seq>` is welcome; the `event:` name is optional (the UI handles both);
   * the first event is `state`;
   * **seq must be contiguous per stream**, with the `state` event's seq equal to the last seq it already includes. On a gap the UI reconnects and resyncs;
   * close slow subscribers rather than drop events silently;
   * keep `node.metrics` flowing every 2 s. The UI marks data stale after 9 s with no event (SSE comments are invisible to JS) and reconnects after 30 s.
2. `GET /api/v1/jobs/{id}/log?follow=1&offset=N` (SSE):
   * unnamed messages `data: {"offset": <byte offset after this chunk>, "data": "<utf-8 text>"}`, then a final `event: end` / `data: {"offset": N}`;
   * without `follow`: `text/plain` bytes from `offset`;
   * the UI reconnects with the last offset while the job is active;
   * if `data` is not JSON, it is treated as one raw text line;
   * the UI renders SGR colors and `\r` progress redraws, and strips other escapes.
3. `GET /api/v1/jobs?limit=&owner=&node=`: the UI sends `owner=<Owner.ID>`. The mock accepts ID or name.

## API needs beyond SPEC (concrete proposals, all additive)

1. **Per-runtime capacity.** On a Mac, Linux jobs are bounded by the Docker VM, but `NodeState.Capacity/Allocated/Free` is one number. Today the UI can only add "Linux jobs run in its Docker VM: 16 cores, 32 GiB". Proposal: `NodeState.Runtimes []RuntimeState{Kind RuntimeKind; Capacity, Allocated, Free Resources}`.
2. **Jobs from other origins.** `NodeState.Allocated` includes jobs that other origins placed on that node, but they are not in `State.Jobs`. The UI draws the remainder as "Jobs from other machines". Proposal: `NodeState.Guests []JobRef{ID, Origin string; Owner Owner; Argv []string; Alloc Resources; Live *Resources}`.
3. **Phase timing and progress.** It is unclear whether `Job.StartedAt` means "placed" or "command started"; the UI uses it for elapsed time. Proposal: `Job.PlacedAt`, `Job.RunningAt`, and `Job.Phase *proto.Phase` (the `FramePhase` payload, e.g. "sending 214 files, 3.1 MB", progress 0..1) so the syncing and setup rows can show progress.
4. **Machine-readable wait reason.** Proposal: `Job.ReasonCode string` (`mem`, `cpu`, `gpu`, `offline`, `draining`, `slot`, `disk`, `origin_only`, `fair_share`) next to the human `Reason`. That lets UI and TUI group or flag without parsing text. The text stays verbatim in both.
5. **Typed event payloads in proto:** `QueueEvent{Queue []string}` and `LogEvent{Level, Msg string; Time time.Time; Node, JobID string}`. Today the UI assumes `{queue}` and `{level,msg,time}`.
6. **Metric history on connect.** Sparklines are client-side, so a fresh page starts empty. Proposal: `GET /api/v1/nodes/{name}/metrics?since=` returning the last 10 min (2 s samples, 300 points) from a daemon ring buffer. `kran top` could draw trends with it too.
7. **History paging and filters:** `GET /api/v1/jobs?state=&before=<id>&q=`. The UI filters by result and text client-side over what it has (at most 600 jobs).
8. **Estimate per chosen node.** `Estimate.Duration` does not say which node it applies to; the UI compares it with wall time. Proposal: `Estimate.DurationOnNode float64` (after the speed factor) for the "N % slower / faster than estimate" column.
9. **Do not ship `spec.env` to the UI** (state, events, `/jobs`). It is already secret-filtered, but the UI never shows it and it is personal data on a TCP listener. An `omitempty` field the API leaves empty would do.
10. **`Stats` day boundary:** `Stats.Since time.Time` (local midnight on the daemon), so "today" is unambiguous across time zones.

## Testing

`nice -n 10 go test -race -p 4 ./internal/ui/... ./internal/tui/...` passes.
* `internal/ui`: handler table test (types, caching, SPA fallback, traversal, methods) and a check that the embedded build is real.
* `internal/ui/mock`: simulation invariants over 10 simulated minutes, SSE first event, token rules.
* `internal/tui`: view fits 80×24, 100×30 and 160×50; NO_COLOR has no escapes; cancel asks first; drain asks and resume doesn't; tab cycles panes; log follow; JSON-decoded event data; seq gap detection; log line cleanup.

`web/scripts/shoot.mjs` also fails unless every machine, running, queue and history row is actually painted (checked with `checkVisibility` including `content-visibility`) and the row counts match the section headers.

Screenshots: `web/screenshots/` (gitignored), regenerated with `make ui-mock` in one shell and `make ui-shots` in another.
