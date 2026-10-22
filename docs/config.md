# Configuration

Everything is optional. kran works with no files at all.

## `~/.config/kran/config.toml` (this machine)

```toml
name = "gx10"                 # node name; default: short hostname, lowercased
[accept]                      # what this machine offers to others
enabled = true
reserve_cpus = 2              # never scheduled: your own headroom
reserve_mem = "8GiB"
max_jobs = 0                  # 0 = limited only by resources
nice = 10                     # remote jobs run niced
[submit]
prefer = "auto"               # auto | local | remote
[ui]
listen = "127.0.0.1:7270"     # "" disables; "tailscale" binds the tailscale address
[runtime.container]
enabled = "auto"              # auto | true | false
```

Unknown keys and bad values are reported with file, line and what to change. kran writes only `name` here (during `kran setup --name`); the rest stays yours.

## `~/.config/kran/peers.toml`

Written by `kran link` and `kran unlink`; do not edit by hand. Each `[[peer]]` holds the node name and ID, candidate addresses (the fastest is remembered), user, pinned host key, the remote kran path and the link direction (`both`, `outbound`, `inbound`).

## `.kran.toml` in a repository

```toml
os = "same"                    # same | any ("any" lets Linux jobs run natively on a Mac)
docker = true                  # false: container jobs get no Docker socket (it is root-equivalent with rootful Docker)
[sync]
include = ["vendor-bin/"]      # extra untracked/ignored paths to send
exclude = ["testdata/huge/"]
[env]
forward = ["DATABASE_URL"]     # extra variables to forward (secrets are dropped otherwise)
set = { CI = "1" }
[setup]                        # runs on the target when the key files change
run = "pnpm install --frozen-lockfile"
key = ["pnpm-lock.yaml"]
[[rule]]                       # per-command hints; first match wins; * matches anything
match = "go test*"
cpus = 8
mem = "6GiB"
# local = true                 # keep matching commands on this machine
# gpu = true                   # also: os = "any", priority = "low"
```

Flags on `kran run` override rules.

## Environment forwarding

Forwarded: everything except host-specific variables (`PATH`, `HOME`, `USER`, `SHELL`, `PWD`, `TMPDIR`, `XDG_*`, `SSH_*`, `DISPLAY`, `GOPATH`, `GOCACHE`, `NVM_*`, `*_SOCK`, `LD_*`, `DYLD_*`, `KRAN_*` except `KRAN_OWNER`, ...) and secrets (names containing TOKEN, SECRET, PASSWORD, PRIVATE, CREDENTIAL, API_KEY, ACCESS_KEY; `AWS_*`, `GH_*`, `GITHUB_*`, `ANTHROPIC_*`, `OPENAI_*`). Always forwarded: `TERM`, `COLORTERM`, `NO_COLOR`, `FORCE_COLOR`, `CI`, `LANG`, `LC_*`, `TZ`. Opt in with `env.forward` or `kran run --env NAME`.

## Locations

| What | Linux | macOS |
|---|---|---|
| config | `~/.config/kran` | `~/.config/kran` |
| state (db, logs, slots, blobs) | `~/.local/state/kran` | `~/Library/Application Support/kran` |
| sockets | `$XDG_RUNTIME_DIR/kran` | state dir |
| binary cache | `~/.cache/kran` | `~/Library/Caches/kran` |

`KRAN_CONFIG_DIR`, `KRAN_STATE_DIR`, `KRAN_RUNTIME_DIR` and `KRAN_CACHE_DIR` override them. With any of the first three set, the service gets its own name, so a test setup never touches your real one.
