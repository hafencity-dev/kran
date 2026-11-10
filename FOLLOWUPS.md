# Follow-ups

Open items that are agreed but not built yet. Each one says what was seen,
what is wanted, and when it may be built.

- **`kran upgrade` on a Mac does not fetch its own linux binary.** Seen
  10.11.2026 (devmac): `kran upgrade` downloads the native asset for the
  machine it runs on, and only the machine that upgrades a *peer* copies the
  linux binary into that peer's cache. A Mac that upgrades itself (or is
  replaced by hand) is left without it until another machine runs
  `kran upgrade` or `kran link`. Wanted: `kran upgrade` on a macOS node also
  fetches `linux-<arch>` for the new release into its own cache (same asset
  and `checksums.txt` check), and its restart prints the runtime line.
  Deliberately not built with the "report it" fix, which changes no
  download behaviour. May be built any time; it needs a release to test
  against.
