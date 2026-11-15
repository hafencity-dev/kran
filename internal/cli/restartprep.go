package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// restartWait bounds how long a restart waits for the queue to drain.
const restartWait = 10 * time.Minute

// carriesOver reports whether a queued job survives a restart without
// waiting for it: its kran run redials at least as long as a restart may
// take plus the time the new daemon keeps the job for it.
func carriesOver(j proto.Job) bool {
	return time.Duration(j.Spec.ClientReconnect)*time.Second >= proto.DaemonStartBound+proto.ReattachWait
}

// idleReason: a restart waits while a job is starting (syncing, setup) or
// queued with a kran run that would give up during the restart (one older
// than 0.1.12-6, which redials 20 s). Queued jobs whose kran run outlasts a
// restart carry over (proto.ClientReconnect), and jobs held for the restart
// (Job.Held) join the queue after it; neither makes the restart wait.
func idleReason(st *proto.State) (bool, string, error) {
	starting, queued := 0, 0
	for _, j := range st.Jobs {
		switch {
		case j.State.Starting():
			starting++
		case j.State == proto.JobQueued && !j.Held && !carriesOver(j):
			queued++
		}
	}
	var parts []string
	if starting > 0 {
		parts = append(parts, fmt.Sprintf("%d jobs are starting", starting))
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("%d jobs are queued by a kran run older than 0.1.12-6", queued))
	}
	if len(parts) == 0 {
		return true, "", nil
	}
	return false, strings.Join(parts, ", "), nil
}

// forceLoss says, before a forced restart, what it costs: which jobs it does
// not wait for, and what happens to them.
func forceLoss(st *proto.State) string {
	var starting, oldQueued []string
	for _, j := range st.Jobs {
		switch {
		case j.State.Starting():
			starting = append(starting, j.Spec.ID+" ("+string(j.State)+")")
		case j.State == proto.JobQueued && !j.Held && !carriesOver(j):
			oldQueued = append(oldQueued, j.Spec.ID)
		}
	}
	sort.Strings(starting)
	sort.Strings(oldQueued)
	var parts []string
	if len(oldQueued) > 0 {
		parts = append(parts, fmt.Sprintf("queued jobs %s belong to a kran run older than 0.1.12-6: if the restart takes over 20 s it runs its job here outside the queue, or exits 125 with --remote", strings.Join(oldQueued, ", ")))
	}
	if len(starting) > 0 {
		parts = append(parts, fmt.Sprintf("jobs %s are syncing or in setup: they continue only if their kran run reattaches within %s, else they are canceled", strings.Join(starting, ", "), proto.ReattachWait))
	}
	if len(parts) == 0 {
		return "--force restarts now; no job is at risk (queued jobs carry over)"
	}
	return "--force restarts now without waiting: " + strings.Join(parts, "; ")
}

// waitIdle polls until idle reports true or wait has passed. It returns
// the last reason when it gave up.
func waitIdle(ctx context.Context, idle func(context.Context) (bool, string, error), now func() time.Time,
	sleep func(context.Context, time.Duration) error, every, wait time.Duration, say func(string)) (string, error) {
	deadline := now().Add(wait)
	announced := ""
	for {
		ok, why, err := idle(ctx)
		if err != nil || ok {
			return "", nil // an unreachable daemon has nothing to protect
		}
		if why != announced {
			say("waiting before the restart: " + why + "; new jobs are held until then")
			announced = why
		}
		if !now().Add(every).Before(deadline) {
			return why, fmt.Errorf("the daemon is still busy after %s (%s)", wait, why)
		}
		if err := sleep(ctx, every); err != nil {
			return why, err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// prepareRestart readies a restart of this machine's daemon. Without force
// it asks the daemon to hold new jobs (an older daemon cannot) and waits
// until nothing would be lost; on timeout it releases the hold and nothing
// has changed. With force it says what the restart costs first. release
// ends the hold (call it when the restart did not happen).
func (a *App) prepareRestart(ctx context.Context, force bool, errw io.Writer) (release func(), err error) {
	release = func() {}
	if a.Dial == nil {
		return release, nil
	}
	cl, err := a.Dial(ctx)
	if err != nil {
		return release, nil // no daemon: nothing to protect
	}
	st, err := cl.State(ctx)
	if err != nil {
		cl.Close()
		return release, nil
	}
	if force {
		fmt.Fprintf(errw, "kran: %s\n", forceLoss(st))
		cl.Close()
		return release, nil
	}
	type holder interface {
		HoldNew(context.Context, time.Duration, string) error
		ReleaseHold(context.Context) error
	}
	if h, ok := cl.(holder); ok {
		if h.HoldNew(ctx, restartWait+time.Minute, "a restart of kran is pending") == nil {
			release = func() {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				h.ReleaseHold(rctx)
			}
		} else {
			fmt.Fprintln(errw, "kran: the running daemon cannot hold new jobs (an older kran); new jobs may join the queue while this waits")
		}
	}
	defer cl.Close()
	why, err := waitIdle(ctx, a.idleCheck, time.Now, sleepCtx, 2*time.Second, restartWait,
		func(s string) { fmt.Fprintln(errw, "kran: "+s) })
	if err != nil {
		release()
		if st2, e := cl.State(ctx); e == nil {
			st = st2
		}
		return func() {}, fmt.Errorf("%v; nothing was changed. Run it again later, or with --force (%s)", err, strings.TrimPrefix(forceLoss(st), "--force restarts now without waiting: "))
	}
	_ = why
	return release, nil
}

// daemonVersion is the version the local daemon reports, "" when none
// answers.
func (a *App) daemonVersion(ctx context.Context) string {
	if a.Dial == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cl, err := a.Dial(ctx)
	if err != nil {
		return ""
	}
	defer cl.Close()
	v, err := cl.Version(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(v.Version, "v")
}

// awaitDaemon reads the daemon's version until it reports want, for at
// most bound (a restarted daemon needs a moment before it serves). It
// returns the last version read and an error when that is not want.
func awaitDaemon(ctx context.Context, read func(context.Context) string, want string, bound, every time.Duration,
	now func() time.Time, sleep func(context.Context, time.Duration) error) (string, error) {
	deadline := now().Add(bound)
	for {
		got := read(ctx)
		if got == want {
			return got, nil
		}
		if !now().Before(deadline) {
			if got == "" {
				return "", fmt.Errorf("no daemon answered within %s of the restart", bound)
			}
			return got, fmt.Errorf("it reports kran %s %s after the restart, not %s", got, bound, want)
		}
		if err := sleep(ctx, every); err != nil {
			return got, err
		}
	}
}
