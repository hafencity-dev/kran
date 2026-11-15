package proto

import "time"

// A daemon restart (kran upgrade, kran service restart) carries queued and
// running jobs over to the new daemon when their kran run comes back to it
// (SPEC §6.1 step 8). Both sides wait for each other with these bounds, so a
// client never gives up on a daemon that is only restarting:
//
//   - the new daemon keeps a restored job for its kran run ReattachWait after
//     it restored it;
//   - kran run redials ClientReconnect after it lost the daemon: the longest
//     a restart may take until the new daemon serves (DaemonStartBound) plus
//     ReattachWait, so it is still dialing for the whole time the daemon
//     waits for it.
//
// Only a daemon that is not back after ClientReconnect is gone; then a kran
// run whose job never started runs it itself (or, with --remote or
// --no-fallback, exits 125).
const (
	ReattachWait = 20 * time.Second
	// DaemonStartBound is how long a restart may take from the old
	// daemon's stop to the new one serving its socket: the old one's
	// graceful stop (5 s), systemd's RestartSec or launchd's respawn (2 s),
	// and the new one's start (store, restore, slice) on a machine under
	// heavy load, with a wide margin.
	DaemonStartBound = 90 * time.Second
	ClientReconnect  = DaemonStartBound + ReattachWait
)
