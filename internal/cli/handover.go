package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
	"github.com/hafencity-dev/kran/internal/transport"
)

// Handover: a daemon the service manager does not run (started on demand
// by `kran serve --stdio`, or by hand) holds the socket, so the managed one
// would fail with "another kran daemon is already running". Before the
// service starts, that daemon is asked to stop (once idle unless force; a
// graceful stop keeps running jobs, the next daemon restores them) and the
// socket must be free. Meanwhile we hold the autostart lock so no new
// on-demand daemon slips in before the managed one is up.

var (
	handoverIdleWait = 10 * time.Minute
	handoverNowWait  = 30 * time.Second
)

type daemonControl interface {
	DaemonInfo(ctx context.Context) (proto.DaemonInfo, error)
	StopDaemon(ctx context.Context, when string) error
}

// handOver frees the socket from a daemon m does not run. The returned
// function (never nil) must be called once the managed daemon is up.
func (a *App) handOver(ctx context.Context, m service.Manager, force bool) (func(), error) {
	noop := func() {}
	cl, err := a.Dial(ctx)
	if err != nil {
		return noop, nil // nothing serves the socket
	}
	defer cl.Close()
	dc, ok := cl.(daemonControl)
	if !ok {
		return noop, nil
	}
	info, err := dc.DaemonInfo(ctx)
	if err != nil {
		// A daemon from before this API: it cannot be asked; the service
		// manager's start reports the conflict.
		return noop, nil
	}
	if st, err := m.Status(ctx); err == nil && st.Running && st.PID == info.PID {
		return noop, nil // it is the service's own daemon
	}
	unlock, err := transport.HoldStartLock(ctx)
	if err != nil {
		return noop, fmt.Errorf("hold the daemon start lock: %w", err)
	}
	when, wait := "idle", handoverIdleWait
	if force {
		when, wait = "now", handoverNowWait
	}
	how := "once its jobs are done (use --force to stop it now; running jobs continue under the service)"
	if idle, _, _ := a.idleCheck(ctx); idle || force {
		how = "now"
	}
	who := "started on demand or by hand"
	if info.Supervisor != "" {
		who = "run by " + info.Supervisor
	}
	a.errorf("a kran daemon %s (pid %d) is serving; asking it to stop %s, then starting the service", who, info.PID, how)
	if err := dc.StopDaemon(ctx, when); err != nil {
		unlock()
		return noop, fmt.Errorf("ask the running kran daemon (pid %d) to stop: %w; stop it yourself, then retry", info.PID, err)
	}
	deadline := time.Now().Add(wait)
	for {
		c, err := a.Dial(ctx)
		if err != nil {
			return unlock, nil // the socket is free
		}
		c.Close()
		if time.Now().After(deadline) {
			unlock()
			return noop, fmt.Errorf("the kran daemon (pid %d) is still running after %s; stop it (kill %d) and retry", info.PID, wait, info.PID)
		}
		select {
		case <-ctx.Done():
			unlock()
			return noop, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// stopOnDemand asks a daemon started on demand to stop (once idle unless
// force), e.g. after an upgrade, so the next connection starts the new
// binary. It does not wait.
func (a *App) stopOnDemand(ctx context.Context, force bool) error {
	cl, err := a.Dial(ctx)
	if err != nil {
		return nil
	}
	defer cl.Close()
	dc, ok := cl.(daemonControl)
	if !ok {
		return nil
	}
	when := "idle"
	if force {
		when = "now"
	}
	if err := dc.StopDaemon(ctx, when); err != nil && !client.IsNotFound(err) {
		return fmt.Errorf("ask the kran daemon to stop: %w", err)
	}
	return nil
}
