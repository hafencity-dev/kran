# Working on kran

Read `SPEC.md` first. It is the contract; `internal/proto` holds the shared
types and each package's main `.go` file holds its interface.

## Ownership

Each package has one owner (SPEC §3 table). Edit only your packages and their
tests. Shared changes:

* `internal/proto`: additive changes only (new fields, types, constants), each
  marked with a `// added by <owner letter>: why` comment. No renames or
  removals.
* Another owner's interface: don't edit it. Write the request into
  `NOTES-<letter>.md` at the repo root and work around it locally (adapter in
  your package) until the lead merges it.
* Replace your package's `stub.go` with the real implementation; keep the
  exported interface file intact unless additive.

## Code

* Go 1.26, pure Go (`CGO_ENABLED=0`), must build for linux and darwin on
  arm64 and amd64: check with
  `GOOS=darwin go vet ./internal/<pkg>/...` and `GOOS=linux go vet ./...`.
  Use build tags (`_linux.go`, `_darwin.go`, `_unix.go`) for OS code.
* Only the dependencies already in `go.mod` (plus the standard library). Ask
  in your NOTES file before adding one.
* Small focused files. Comments explain why, not what. Errors carry context
  (`fmt.Errorf("sync to %s: %w", node, err)`). User-facing text follows
  SPEC §11 style: plain sentences, says what to do next, no emoji.
* No global state except where the interface requires it. Inject clocks and
  paths so tests are deterministic.
* Every goroutine has an owner and an exit path; every file/socket is closed;
  `go test -race` must be clean.

## Tests

* Table-driven unit tests next to the code; integration tests may use
  `t.TempDir()`, real processes and the in-memory transport.
* This machine (gx10) is shared and was overloaded today. Run tests gently:
  `nice -n 10 go test -race -p 4 ./internal/<pkg>/...`. Never run more than
  one test process at a time yourself.
* macOS: `ssh devmac` reaches a Mac Studio (M3 Ultra, macOS 26). Go lives in
  `~/toolchains/go-1.27.3/bin`. Use only `~/kran-dev/<letter>/` there:
  `rsync -a --delete --exclude .git ./ devmac:kran-dev/<letter>/ && ssh devmac 'cd kran-dev/<letter> && PATH=$HOME/toolchains/go-1.27.3/bin:$PATH nice -n 10 go test ./internal/<pkg>/...'`.
  Docker on the Mac: `PATH=/usr/local/lokalaise/tools/bin:$PATH docker …`
  (Colima). Never touch containers, volumes or files you did not create; the
  Mac also runs a product install. Name everything you create with `kran-dev`.

## Git

* Work on your branch in your worktree; commit early and often with clear
  messages ending in
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
* Don't push, don't rebase other branches; the lead merges.
* Finish with: `go build ./... && go vet ./... && nice -n 10 go test -race -p 4 ./internal/<your pkgs>/...`
  green, and a short report: done, not done, interface/proto changes, notes.
