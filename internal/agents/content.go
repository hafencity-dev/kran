package agents

// Text installed for coding agents. Keep it short: agents read it on every
// session, and every line costs context.

const (
	blockBegin = "<!-- kran:begin (managed by kran agents) -->"
	blockEnd   = "<!-- kran:end -->"
)

const skillText = `---
name: kran
description: Run heavy commands (go test/build, cargo, next build, vitest/jest/playwright, pytest, tsc -b, docker build, make test, GPU jobs) on whichever of the user's machines has room, using ` + "`kran run`" + `. Use it for any test, build or type-check that takes more than a few seconds. Not for dev servers, watchers or interactive commands.
---
<!-- managed by kran agents; edits are overwritten by ` + "`kran agents install`" + ` -->

# kran

kran runs a command on the user's machine with the most free CPU and memory
(often another computer) and streams the output back. Output is byte-for-byte
the command's own and the exit code is the command's exit code. If kran cannot
help, it runs the command locally, so using it is never worse than not using it.

## Use it

    kran run -- go test ./...
    kran run --sh 'cd web && pnpm test && pnpm build'
    kran run --gpu -- python train.py

* Use ` + "`kran run`" + ` for tests, builds, type checks, linters over many files, docker builds
  and anything GPU-bound. A Claude Code hook may already wrap such Bash commands
  for you; do not wrap a command that already starts with ` + "`kran`" + `.
* Batch: run the whole suite in ONE ` + "`kran run`" + ` (` + "`go test ./...`" + `, ` + "`pnpm -r test`" + `) instead of
  many small runs. Each run pays a small sync cost; one big run parallelizes better.
* Chain steps with ` + "`--sh`" + ` so they run on the same machine in one go.
* Pass what the job needs: ` + "`--cpus N`" + `, ` + "`--mem 8G`" + `, ` + "`--gpu`" + `, ` + "`--node NAME`" + `. Add ` + "`-i`" + ` only when the command reads stdin.

## What stays local

Use plain commands (not ` + "`kran run`" + `) for: dev servers and watchers (` + "`next dev`" + `, ` + "`vite`" + `,
` + "`--watch`" + `), anything that needs localhost services, a GUI, a browser window or
an interactive terminal, git, ssh, and commands that must leave files in this
checkout (a build output you will use next). A remote job works on a synced copy
of the repo, so do not count on files it writes appearing here; add ` + "`--local`" + `
(forces this machine) when you need them.

## Reading kran's messages

kran speaks only on stderr, one line, prefixed ` + "`kran:`" + `, e.g.
` + "`kran: waiting - needs 6 GiB, devmac has 3.1 GiB free`" + ` or
` + "`kran: running on devmac (linux container)`" + `. They are information, not errors.

Exit codes: the command's own code passes through (1 = test failure, as usual);
126 not executable, 127 not found, 128+n killed by signal n; **125 = kran itself
failed after the job started** (not your code's fault): rerun the same command,
or use ` + "`kran run --local`" + `. 2 = usage error (` + "`kran run --help`" + `).

## Status

    kran status --json        machines, free CPU/memory, queue, running jobs
    kran jobs --json          recent jobs and where they ran
    kran run --json-events …  machine-readable progress lines on stderr
    kran doctor               checks and fixes for the setup

If ` + "`kran`" + ` reports the daemon is not running, commands still run locally; tell the
user to run ` + "`kran setup`" + ` or ` + "`kran service start`" + `.
`

const blockBody = `## kran

This machine has kran: heavy commands (tests, builds, type checks, docker builds)
can run on the user's other machines. Use ` + "`kran run -- <cmd>`" + ` or
` + "`kran run --sh '<shell string>'`" + ` for them and run the whole suite in one call.
Output and exit code are the command's own; ` + "`kran:`" + ` lines on stderr are kran's;
exit 125 means kran failed (rerun, or add ` + "`--local`" + `). Dev servers, watchers and
interactive commands stay local. ` + "`kran status --json`" + ` shows machines and load.
`

// blockText is the managed block including both markers (no trailing newline).
func blockText() string { return blockBegin + "\n" + blockBody + blockEnd }
