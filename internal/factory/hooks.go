package factory

// The agents' own hooks (R50 slice 2): the factory daemon listens on its
// hook socket (Layout.FactoryHookSocket, opened under its lock before its
// startup), every pane its runtime makes from then on names the socket
// (Runtime.UseHookSocket), and each event an agent's hook sends there goes
// to Runtime.Wake, which looks at that pane now, then asks the loop for an
// observe pass at once: the pass returns what the look saw, and a turn's
// end it brings kicks the tick (observe), so the next stage starts a second
// or two after the agent stopped rather than at the next observe. An event
// is a hint, never the source of truth, and the observe passes go on at
// their clock: without a usable socket (a path of 104 bytes or more, a
// listener that fails) the daemon starts all the same, says so in one line,
// and polls as before.

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
