package agents

// Text installed for coding agents. Keep it short: agents read it on every
// session, and every line costs context.

const (
	blockBegin = "<!-- kran:begin (managed by kran agents) -->"
	blockEnd   = "<!-- kran:end -->"
)

const skillText = `---
name: kran
description: Run long tests, builds, type checks, lints and docker builds on the user's other machines with ` + "`kran run`" + `. Use it for anything of that kind that takes more than about ten seconds. Not for dev servers, watchers, installs, git or quick commands.
---
<!-- managed by kran agents; edits are overwritten by ` + "`kran agents install`" + ` -->

# kran

` + "`kran run -- <command>`" + ` runs a command on whichever of the user's machines has room and
streams the output back: same output, same exit code. If kran cannot place the
job it runs it here, so using it is never worse than not using it.

    kran run -- go test ./...
    kran run --sh 'cd web && pnpm test && pnpm build'
    kran run --gpu -- python train.py

* Use it for tests, builds, type checks, lints, docker builds and GPU jobs that
  take more than about ten seconds. Run the whole suite in ONE call.
* Do not use it for dev servers, watchers, installs (` + "`npm install`, `go mod tidy`" + `), git,
  anything that needs localhost, a browser or a terminal, or quick commands.
* A job on another machine works on a synced copy: files it writes stay there.
  Add ` + "`--local`" + ` when you need them here (a binary you will run, a coverage file).
* Needs: ` + "`--cpus N`, `--mem 8G`, `--gpu`, `--node NAME`" + `; ` + "`-i`" + ` only if the command reads stdin.
* A Claude Code hook may already wrap such commands. Never wrap one that starts with ` + "`kran`" + `.

## Messages and exit codes

kran speaks only on stderr, one line starting with ` + "`kran:`" + `, only when it adds
information (` + "`kran: running on devmac`" + `). They are not errors.

The command's own exit code passes through (1 = failing tests as usual; 126 not
executable, 127 not found, 128+n signal). **125 = kran itself failed after the
command started**: rerun the same command, or add ` + "`--local`" + `.

## Look at the machines

    kran status --json      machines, free CPU and memory, queue
    kran jobs --json        recent jobs and where they ran
    kran doctor             checks the setup

If kran says the daemon is not running, the command still ran here. Tell the
user to run ` + "`kran service start`" + `.
`

const blockBody = `## kran

Long tests, builds, type checks, lints and docker builds can run on the user's
other machines: ` + "`kran run -- <cmd>`" + ` or ` + "`kran run --sh '<shell string>'`" + `, whole suite in
one call. Output and exit code are the command's own; ` + "`kran:`" + ` lines on stderr are
kran's; exit 125 means kran failed (rerun, or add ` + "`--local`" + `). Servers, watchers,
installs and quick commands stay plain. Files a remote run writes stay there:
use ` + "`--local`" + ` when you need them. ` + "`kran status --json`" + ` shows machines and load.
`

// blockText is the managed block including both markers (no trailing newline).
func blockText() string { return blockBegin + "\n" + blockBody + blockEnd }
