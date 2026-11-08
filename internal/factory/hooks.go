package factory

// The agents' own hooks (R50 slice 2): the factory daemon listens on its
// hook socket (Layout.FactoryHookSocket, opened under its lock before its
// startup), every pane its runtime makes from then on names the socket
// (Runtime.UseHookSocket), and each event an agent's hook sends there goes
// to Runtime.Wake, which looks at that pane now, then asks the loop for an
// observe pass at once (hookPasses: so do a StopFailure its screen proves
// and a run's end a later look found, at most once a second): the pass
// returns what the looks saw, and a turn's end it brings kicks the tick
// (observe), so the next stage starts a second or two after the agent
// stopped rather than at the next observe. An event is a hint, never the
// source of truth, and the observe passes go on at their clock: without a
// usable socket (a path of 104 bytes or more, a listener that fails) the
// daemon starts all the same, says so in one line, and polls as before.

import (
	"context"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/paths"
)

// hookPing is the event `magnum doctor` sends to see that the socket
// listens. The listener drops it (it knows no such event), and so does
// hookEvent, should a later listener hand it on.
const hookPing = "ping"

// listenHooks opens the factory's hook socket (Layout.FactoryHookSocket)
// and has the runtime name it in the panes it makes from now on
// (UseHookSocket). The caller holds the factory lock (RunLocked), so no other
// daemon serves the socket and its stale file may go, and it calls this
// before its startup, so every pane the daemon makes names the socket. A
// path that cannot name a Unix socket (paths.CheckSocketPath) or a listener
// that fails gives nil and one line in the log: no pane names a socket, and
// the observe passes alone see the agents. No runtime, no listener.
func (e *Engine) listenHooks() *agenthooks.Listener {
	if e.d.Runtime == nil {
		return nil
	}
	path := e.d.Layout.FactoryHookSocket()
	err := paths.CheckSocketPath(path)
	var l *agenthooks.Listener
	if err == nil {
		l, err = agenthooks.Listen(path, e.log)
	}
	if err != nil {
		e.log.Warn("the factory listens for no agent hooks: it polls its panes", "err", err)
		return nil
	}
	e.d.Runtime.UseHookSocket(path)
	e.log.Info("the factory listens for its agents' hooks", "socket", path)
	return l
}

// serveHooks serves l on a goroutine, on a context of its own under ctx
// (the daemon's: Wake's later looks run on it), and returns its stop, which
// the daemon calls as it stops: it ends that context, waits for Serve to
// return (its socket closes, so a hook that sends now fails at once) and
// then for the later looks the events' Wake calls started
// (Runtime.WaitWakes), no longer than what is left of the drain's wait
// (drainWaitNow): a drain stays as fast as without hooks.
func (e *Engine) serveHooks(ctx context.Context, l *agenthooks.Listener) (stop func()) {
	hctx, cancel := context.WithCancel(ctx)
	served := make(chan struct{})
	go func() {
		defer close(served)
		l.Serve(hctx, func(ev agenthooks.Event) { e.hookEvent(hctx, ev) })
	}()
	return func() {
		cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-served
			e.d.Runtime.WaitWakes()
		}()
		wait := e.drainWaitNow()
		select {
		case <-done:
		case <-time.After(wait):
			e.log.Warn("the agents' hook looks did not end within the drain's wait; the daemon stops anyway", "wait", wait)
		}
	}
}

// hookEvent takes one event of an agent's hook, on the listener's
// goroutine: Runtime.Wake looks at the event's pane now, on ctx (the
// daemon's, which bounds the later looks Wake starts), then the loop is
// asked for an observe pass at once (requestObserve), which returns what
// the look saw. A ping (hookPing) does neither. It holds no lock: Wake runs
// the runtime's callbacks, which take the runtime's own (Runtime.Wake).
func (e *Engine) hookEvent(ctx context.Context, ev agenthooks.Event) {
	if ev.Name == hookPing {
		return
	}
	e.d.Runtime.Wake(ctx, ev)
	e.requestObserve()
}

// requestObserve asks the loop for an observe pass now (an agent's hook
// event, hookEvent); one pending absorbs the others, as a kick does, so a
// burst of events costs one pass. It never blocks.
func (e *Engine) requestObserve() {
	select {
	case e.observeNow <- struct{}{}:
	default:
	}
}

// hookPassGap is the least time between two observe passes the hooks ask
// for (hookPasses): a flood of events, forged ones or a turn's many
// subagents', costs the loop at most one such pass a second beside the
// clock's.
const hookPassGap = time.Second

// hookPasses is the loop's schedule of the observe passes the agents' hooks
// ask for (RunLocked): an event's request (observeNow), a StopFailure's
// (Runtime.ObserveSoon) and a run's end that a look after the event's own
// found (Runtime.Ended, which closes on it) each run a pass at once, but no
// sooner than gap after the last such pass began: the floor timer holds a
// request until then, and the requests it holds make one pass. The observe
// clock keeps its own pace: a hook pass neither resets it (a look again at
// +1 s or +2 s that finds nothing leaves its turn to the clock, which must
// not be a whole interval away) nor adds a clock pass after it. Every pass,
// the clock's too, covers the requests made before it began, so it takes
// them and a fresh Ended (observePass).
type hookPasses struct {
	gap   time.Duration
	last  time.Time // when the last hook pass began (zero: none yet)
	floor *time.Timer
	armed bool            // the floor holds a request
	ended <-chan struct{} // Runtime.Ended as taken before the last pass (nil: none, or closed and not taken again)
	asked <-chan struct{} // Runtime.ObserveSoon (nil: no runtime)
}

// newHookPasses is the loop's schedule at its start.
func (e *Engine) newHookPasses() *hookPasses {
	p := &hookPasses{gap: e.hookGap, floor: time.NewTimer(time.Hour)}
	p.floor.Stop()
	if e.d.Runtime != nil {
		p.asked, p.ended = e.d.Runtime.ObserveSoon(), e.d.Runtime.Ended()
	}
	return p
}

// due reports whether a hook pass may run at now; when it may not yet, the
// floor is armed for when it may.
func (p *hookPasses) due(now time.Time) bool {
	wait := p.gap - now.Sub(p.last)
	if p.last.IsZero() || wait <= 0 {
		return true
	}
	if !p.armed {
		p.floor.Reset(wait)
		p.armed = true
	}
	return false
}

// observePass runs one observe pass of the loop (hook says whether the hooks
// asked for it): it takes the requests made before it and a fresh Ended,
// which the pass's own end of a run closes too (one more hook pass then, a
// gap later), and lets the floor go.
func (e *Engine) observePass(ctx context.Context, p *hookPasses, hook bool) {
	select {
	case <-e.observeNow:
	default:
	}
	select {
	case <-p.asked:
	default:
	}
	if p.armed {
		p.floor.Stop()
		p.armed = false
	}
	if e.d.Runtime != nil {
		p.ended = e.d.Runtime.Ended()
	}
	if hook {
		p.last = time.Now()
	}
	e.observe(ctx)
}
