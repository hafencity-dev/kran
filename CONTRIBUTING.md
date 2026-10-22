# Contributing to kran

Thanks for helping. kran's first promise is that a job is never worse off
than running it locally, so changes are judged by how they fail as much as by
how they work.

## Start here

* `SPEC.md` is the contract: architecture, job lifecycle, protocols, security
  model. If code and spec disagree, fix one of them in the same change.
* `internal/proto` holds the types shared by the CLI, daemons and web UI. JSON
  field names are public API: add fields, don't rename or remove them.
* Each package's main `.go` file holds its interface; packages talk only
  through those.

## Build and test

```sh
make build          # bin/kran
make test           # go test -race ./...
make lint           # vet + gofmt check
make ui             # rebuild the web UI into internal/ui/dist (needs Node)
make e2e            # black-box tests on the real binary (slower)
make chaos          # randomized kill/cut test, a few minutes
```

Code must build for linux and darwin on arm64 and amd64 with `CGO_ENABLED=0`
(`GOOS=darwin go vet ./...`). OS-specific code goes in `_linux.go` /
`_darwin.go` / `_unix.go` files.

Tests run real processes and daemons. On a busy machine run them gently:
`nice -n 10 go test -race -p 4 ./internal/<pkg>/...`.

## Code

* Standard library first; new dependencies need a good reason.
* Small focused files. Comments explain why, not what.
* Errors carry context (`fmt.Errorf("sync to %s: %w", node, err)`).
* User-facing text is plain sentences that say what to do next. kran speaks
  on stderr, prefixed `kran:`, only when it adds information.
* Inject clocks and paths so tests are deterministic. Every goroutine has an
  owner and an exit path; `go test -race` must be clean.
* For a bug fix, write the failing test first.

## Pull requests

Keep them focused, describe the failure mode you considered, and include the
tests. Security issues: please report privately via GitHub's security
advisories instead of a public issue.
