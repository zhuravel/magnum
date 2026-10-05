package cli

// The one way the CLI and the screens hand work to the daemon: queue a
// request, wake the daemon, wait for its answer and print it. Every command
// (review, pin and the other targets, approve, pause, retro, abort, open,
// cleanup, slots, identities) and every screen action goes through reqClient,
// which returns the request id with the outcome, or with "still pending", so
// a screen can keep following a request the daemon has not answered yet.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// reqQuickWait bounds the wait for a request the daemon answers within its
// tick. It handles requests before its GitHub poll, which alone can take
// 12 s; 10 s used to run out before the answer came.
const reqQuickWait = 30 * time.Second

// reqClient queues requests for the daemon and follows them.
type reqClient struct {
	st *store.Store
	// kick wakes the daemon (engine.KickDaemon): its pid, 0 when none runs.
	kick func() (int, error)
	// running reports the daemon's pid without waking it (engine.DaemonPID)
	// for a send that needs a daemon; nil = learn it from the kick.
	running func() (int, error)
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	// version is this binary's build, compared with the daemon's
	// (engine.SkewNote).
	version string
	// sent, when set, hears the outcome of every request send queued, as
	// send returns it: the screens keep the ids to follow the requests
	// still pending.
	sent func(reqOutcome)
}

// reqSend tunes one send.
type reqSend struct {
	// Wait bounds the wait for the answer: 0 = do not wait, < 0 = no limit.
	Wait time.Duration
	// Poll is how often the wait re-reads the request (0 = 250ms).
	Poll time.Duration
	// NeedDaemon is for work that must not run later, when nobody waits
	// for it (a review that posts, a verdict, restoring a parked PR): with
	// no daemon running nothing is queued, and a request no daemon answered
	// is withdrawn. The send then fails with errNoDaemon.
	NeedDaemon bool
	// Held: the daemon holds its lock (the caller found it), so it runs
	// even when the kick found no usable pidfile; the send waits anyway.
	Held bool
}

// errNoDaemon is a NeedDaemon send's error when no daemon runs.
var errNoDaemon = errors.New("no daemon is running: " + actDaemonFix)

// reqOutcome is a request as the client last read it.
type reqOutcome struct {
	Req store.Request
	// PID is the daemon that answered the kick (0 = none did).
	PID int
	// Held is reqSend.Held.
	Held bool
	// KickErr is why waking the daemon failed (the request stays queued).
	KickErr error
	// Skew is the daemon's build skew (engine.SkewNote), "" when none.
	Skew string
}

// ID is the request's id.
func (o reqOutcome) ID() int64 { return o.Req.ID }

// Pending reports that the daemon has not answered yet.
func (o reqOutcome) Pending() bool { return o.Req.State == store.RequestPending }

// Failed reports that the daemon refused the request.
func (o reqOutcome) Failed() bool { return o.Req.State == store.RequestFailed }

// Result is the daemon's answer ("" while pending).
func (o reqOutcome) Result() string { return strings.TrimSpace(store.Deref(o.Req.Result)) }

// send queues kind with payload, wakes the daemon and waits for the answer
// as o says (only when a daemon answered the kick, or holds its lock). The
// error is only for a request that could not be queued, or for a NeedDaemon
// send without a daemon; anything else is in the outcome.
func (rc reqClient) send(ctx context.Context, kind string, payload any, o reqSend) (out reqOutcome, err error) {
	if o.NeedDaemon && rc.running != nil {
		if pid, err := rc.running(); err == nil && pid == 0 {
			return reqOutcome{}, errNoDaemon
		}
	}
	id, err := rc.st.EnqueueRequest(ctx, kind, payload)
	if err != nil {
		return reqOutcome{}, fmt.Errorf("queue %s request: %w", kind, err)
	}
	out = reqOutcome{Req: store.Request{ID: id, Kind: kind, State: store.RequestPending}, Held: o.Held}
	if rc.sent != nil {
		defer func() { rc.sent(out) }()
	}
	out.PID, out.KickErr = rc.kick()
	if o.NeedDaemon && out.PID == 0 {
		why := "no daemon answered, so nothing runs it"
		if err := rc.st.CompleteRequest(ctx, id, store.RequestFailed, why); err == nil || !errors.Is(err, store.ErrConflict) {
			out.Req.State, out.Req.Result = store.RequestFailed, &why
			if out.KickErr != nil {
				return out, fmt.Errorf("%w (waking it failed: %v)", errNoDaemon, out.KickErr)
			}
			return out, errNoDaemon
		}
		// A daemon took it meanwhile: report its answer.
	}
	if req, err := rc.st.RequestByID(ctx, id); err == nil {
		out.Req = req
	}
	if out.PID > 0 || o.Held || !out.Pending() {
		out.Skew = rc.skew(ctx)
	}
	if o.Wait != 0 && out.Pending() && (out.PID > 0 || o.Held) {
		if err := rc.wait(ctx, &out, o.Wait, o.Poll); err != nil {
			return out, nil // interrupted: the outcome says pending
		}
	}
	return out, nil
}

// wait re-reads the request every poll (0 = 250ms) until it leaves pending,
// ctx ends or timeout passes (< 0 = no limit); out keeps the last read.
func (rc reqClient) wait(ctx context.Context, out *reqOutcome, timeout, poll time.Duration) error {
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = rc.now().Add(timeout)
	}
	for {
		req, err := rc.st.RequestByID(ctx, out.Req.ID)
		if err != nil {
			return err
		}
		out.Req = req
		if !out.Pending() || (!deadline.IsZero() && !rc.now().Before(deadline)) {
			return nil
		}
		if err := rc.sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// skew is the daemon's build skew against this binary ("" = none, or no
// daemon recorded its build).
func (rc reqClient) skew(ctx context.Context) string {
	b, ok := engine.ReadDaemonBuild(ctx, rc.st)
	if !ok {
		return ""
	}
	return engine.SkewNote(b, rc.version, engine.FileModTime)
}

// print renders the outcome for humans: the answer on w, a refusal on ew
// (exit 1), or how to follow a request still pending; a build skew and a
// failed wake-up come first, on ew.
func (o reqOutcome) print(w, ew io.Writer) int {
	o.printNotes(ew)
	switch o.Req.State {
	case store.RequestDone:
		res := o.Result()
		if res == "" {
			res = "done"
		}
		fmt.Fprintln(w, res)
		return 0
	case store.RequestFailed:
		fmt.Fprintln(ew, o.Result())
		return 1
	}
	id, kind := o.Req.ID, o.Req.Kind
	switch {
	case o.PID > 0:
		fmt.Fprintf(w, "the daemon (pid %d) is running, so the work was queued as request %d (%s); it has not answered yet: follow it with `magnum logs request:%d -f`\n", o.PID, id, kind, id)
	case o.Held:
		fmt.Fprintf(w, "the daemon is running (it holds its lock), so the work was queued as request %d (%s); it could not be woken (no usable pidfile) and picks the request up on its next tick: follow it with `magnum logs request:%d -f`\n", id, kind, id)
	default:
		fmt.Fprintf(ew, "request %d (%s) is queued, but no daemon is running: %s\n", id, kind, actDaemonFix)
	}
	return 0
}

// printNotes prints the build skew and a failed wake-up on ew.
func (o reqOutcome) printNotes(ew io.Writer) {
	if o.Skew != "" {
		fmt.Fprintf(ew, "warning: %s\n", o.Skew)
	}
	if o.KickErr != nil {
		fmt.Fprintf(ew, "magnum: could not wake the daemon: %v (it picks request %d up on its next tick)\n", o.KickErr, o.Req.ID)
	}
}

// actRequestJSON is a request as --json prints it.
type actRequestJSON struct {
	ID        int64  `json:"request_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Result    string `json:"result,omitempty"`
	DaemonPID int    `json:"daemon_pid"`
	// Skew is the daemon's build skew (engine.SkewNote).
	Skew string `json:"daemon_skew,omitempty"`
}

// view is the outcome as --json prints it.
func (o reqOutcome) view() actRequestJSON {
	return actRequestJSON{ID: o.Req.ID, Kind: o.Req.Kind, State: o.Req.State, Result: store.Deref(o.Req.Result), DaemonPID: o.PID, Skew: o.Skew}
}

// jsonCode is the exit code of a request printed as JSON: 1 when the daemon
// refused it.
func (o reqOutcome) jsonCode() int {
	if o.Failed() {
		return 1
	}
	return 0
}
