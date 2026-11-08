package engine

// The review daemon's receiver of the agents' own hooks (R50 slice 2,
// DECISIONS "The review daemon listens for its agents' hooks"). The daemon
// command claims the daemon lock before it builds its App (ClaimDaemon),
// and under it starts the hook listener on Layout.HookSocket, so the App's
// agents managers make their panes with the socket in their env only while
// a listener reads it (agents.Deps.HookSocket). Run serves the listener
// once startup is done and hands each event to Wake; a path too long for a
// Unix socket, or a listener that cannot start, leaves the daemon on its
// polling alone, with one warning that says why. At shutdown the listener
// stops first, then the daemon waits for the later looks Wake started
// (WaitWakes).

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/paths"
)

// hooksOffMsg is the warning of a daemon without a hook listener.
const hooksOffMsg = "agent hooks off: the daemon polls its agents alone"

// Claim is what a review daemon takes before it builds its App
// (ClaimDaemon): the daemon lock and, when it got it, the hook listener on
// Layout.HookSocket. Run takes the lock it holds (Options.Claim) and serves
// the listener; Release gives both up.
type Claim struct {
	unlock   func() // the daemon lock's; nil when another process holds it
	held     bool   // another process holds the daemon lock
	listener *agenthooks.Listener
	// off says why there is no listener ("" while there is one, or when
	// the lock is held).
	off string
	// log is the listener's log: nothing until Run hands it the daemon's
	// (the App that makes it is built after the claim).
	log  *lateHandler
	once sync.Once
}

// ClaimDaemon takes the daemon lock (Layout.Lock) for a daemon that is
// about to build its App, and, when it got it, starts the agents' hook
// listener on Layout.HookSocket (paths.CheckSocketPath first; Listen
// replaces a socket an earlier daemon left, since the lock's holder owns
// the path, and leaves alone a file that is no socket). The lock taken by
// another process is no error: the claim says so (Held), and Run exits as
// when it finds the lock taken itself. A path too long for a Unix socket
// and a listener that cannot start are no errors either: the claim has no
// listener and says why (Off), so the daemon makes no pane with the hook
// env and polls. Only a lock that cannot be taken at all is an error.
func ClaimDaemon(l paths.Layout) (*Claim, error) {
	if err := l.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("engine: state dirs: %w", err)
	}
	unlock, held, err := AcquireLock(l.Lock())
	if err != nil {
		return nil, fmt.Errorf("engine: lock: %w", err)
	}
	c := &Claim{unlock: unlock, held: held, log: &lateHandler{target: &atomic.Pointer[slog.Handler]{}}}
	if held {
		return c, nil
	}
	path := l.HookSocket()
	if err := paths.CheckSocketPath(path); err != nil {
		c.off = err.Error()
		return c, nil
	}
	if c.listener, err = agenthooks.Listen(path, slog.New(c.log)); err != nil {
		c.off = err.Error()
	}
	return c, nil
}

// Held reports whether another process held the daemon lock.
func (c *Claim) Held() bool { return c != nil && c.held }

// HookSocket is the listener's socket, the panes' MAGNUM_HOOK_SOCKET; ""
// without a listener (no claim, the lock held, a path too long, a listener
// that failed).
func (c *Claim) HookSocket() string {
	if c == nil || c.listener == nil {
		return ""
	}
	return c.listener.Path()
}

// Off says why the claim has no listener while it holds the lock ("" with
// one).
func (c *Claim) Off() string {
	if c == nil {
		return ""
	}
	return c.off
}

// Release closes the listener and gives the lock up, once; a nil claim
// does nothing.
func (c *Claim) Release() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if c.listener != nil {
			_ = c.listener.Close()
		}
		if c.unlock != nil {
			c.unlock()
		}
	})
}

// lockDaemon is the daemon lock Run holds: the claim's when it has one,
// else its own (AcquireLock).
func (e *Engine) lockDaemon(c *Claim) (unlock func(), held bool, err error) {
	if c != nil {
		return c.Release, c.held, nil
	}
	return AcquireLock(e.d.Layout.Lock())
}

// startHooks starts serving the claim's hook listener on a goroutine of its
// own, each event to Wake on ctx (the daemon's: Wake's later looks end with
// it), and returns what stops it: ctx's cancel first (stop), then the wait
// for Serve, which returns once the Wake in flight returned, then
// WaitWakes. Without a listener it warns once why (a claim that holds the
// lock) and returns stop alone.
func (e *Engine) startHooks(ctx context.Context, stop context.CancelFunc, c *Claim) func() {
	if c == nil || c.listener == nil {
		if why := c.Off(); why != "" {
			e.log.Warn(hooksOffMsg, "why", why)
		}
		return stop
	}
	c.log.set(e.log.Handler())
	served := make(chan struct{})
	go func() {
		defer close(served)
		c.listener.Serve(ctx, func(ev agenthooks.Event) { e.wakeHook(ctx, ev) })
	}()
	e.log.Info("agent hooks: listening", "socket", c.listener.Path())
	return func() {
		stop()
		<-served
		if e.d.Agents != nil {
			e.d.Agents.WaitWakes()
		}
	}
}

// wakeHook hands a hook event to the agents manager.
func (e *Engine) wakeHook(ctx context.Context, ev agenthooks.Event) {
	if e.d.Agents == nil {
		return
	}
	e.d.Agents.Wake(ctx, ev)
}

// lateHandler is a slog.Handler that drops its records until set gives it
// the handler to pass them to: the hook listener starts before the App
// that makes the daemon's logger.
type lateHandler struct {
	target *atomic.Pointer[slog.Handler]
	// with replays the WithAttrs and WithGroup calls made on this handler
	// onto the target (nil: none).
	with func(slog.Handler) slog.Handler
}

// set has every lateHandler sharing h's target pass its records to to.
func (h *lateHandler) set(to slog.Handler) { h.target.Store(&to) }

func (h *lateHandler) handler() slog.Handler {
	p := h.target.Load()
	if p == nil {
		return nil
	}
	if h.with != nil {
		return h.with(*p)
	}
	return *p
}

func (h *lateHandler) Enabled(ctx context.Context, level slog.Level) bool {
	to := h.handler()
	return to != nil && to.Enabled(ctx, level)
}

func (h *lateHandler) Handle(ctx context.Context, r slog.Record) error {
	if to := h.handler(); to != nil {
		return to.Handle(ctx, r)
	}
	return nil
}

func (h *lateHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.then(func(to slog.Handler) slog.Handler { return to.WithAttrs(attrs) })
}

func (h *lateHandler) WithGroup(name string) slog.Handler {
	return h.then(func(to slog.Handler) slog.Handler { return to.WithGroup(name) })
}

// then is h with step after its own WithAttrs and WithGroup calls.
func (h *lateHandler) then(step func(slog.Handler) slog.Handler) slog.Handler {
	prev := h.with
	return &lateHandler{target: h.target, with: func(to slog.Handler) slog.Handler {
		if prev != nil {
			to = prev(to)
		}
		return step(to)
	}}
}
