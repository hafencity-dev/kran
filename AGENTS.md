# Notes for coding agents

Read `CONTRIBUTING.md` and `SPEC.md` first; they apply to you too. In short:
`SPEC.md` is the contract, `internal/proto` types are public API (add, never
rename), write the failing test first, keep `go test -race ./...` and
`GOOS=darwin go vet ./...` green, and run tests with `nice -n 10 -p 4` on a
shared machine. If kran itself is installed, `kran run -- go test ./...` sends
the suite to a machine with room.
