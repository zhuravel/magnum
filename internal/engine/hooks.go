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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// hooksOffMsg is the warning of a daemon without a hook listener, and
// hooksListeningMsg the line of one with it.
const (
	hooksOffMsg       = "agent hooks off: the daemon polls its agents alone"
	hooksListeningMsg = "agent hooks: listening"
)

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
	err = paths.CheckSocketPath(path)
	if err == nil {
		c.listener, err = agenthooks.Listen(path, slog.New(c.log))
	}
	if err != nil { // no listener: the daemon polls, and its panes get no hook env
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

// startHooks records whether this start listens for its agents' hooks (for
// `magnum status`: recordHooks), then serves the claim's hook listener on a
// goroutine of its own, each event to Wake on the managers whose registries
// record its pane (routeHook) on ctx (the daemon's: Wake's later looks end
// with it), and the StopFailures the hooks proved (Deps.HookFailures) on
// another (onHookFailure), since the managers hand them on without waiting
// (app.App.SendHookFailure). It returns what stops both: ctx's cancel first
// (stop), then the wait for Serve, which returns once the Wake in flight
// returned, then WaitWakes on the daemon's manager (a pane agent's waits at
// its close: hookRouter.remove), then the wait for the failures' goroutine.
// Without a listener it warns once why (a claim that holds the lock) and
// returns a stop that does nothing: Run's own defers stop the rest as
// before.
func (e *Engine) startHooks(ctx context.Context, stop context.CancelFunc, c *Claim) func() {
	e.recordHooks(ctx, c)
	if c == nil || c.listener == nil {
		if why := c.Off(); why != "" {
			e.log.Warn(hooksOffMsg, "why", why)
		}
		return func() {}
	}
	c.log.set(e.log.Handler())
	e.agentHooks.open(ctx)
	served, failing := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(served)
		c.listener.Serve(ctx, func(ev agenthooks.Event) { e.routeHook(ctx, ev) })
	}()
	go func() {
		defer close(failing)
		if e.d.HookFailures != nil {
			e.hookFailures(ctx, e.d.HookFailures)
		}
	}()
	e.log.Info(hooksListeningMsg, "socket", c.listener.Path())
	return func() {
		stop()
		<-served
		e.agentHooks.close()
		if e.d.Agents != nil {
			e.d.Agents.WaitWakes()
		}
		<-failing // before the shutdown stops the toasts a pause sends
	}
}

// hookEventMsg is the router's debug line of each event it hands on.
const hookEventMsg = "hook event routed"

// hookRouter hands the hook listener's events to the agents managers whose
// registries record a live session on the event's pane: the daemon's own
// (Deps.Agents over the live registry: the rounds', the fix job's and the
// whiteboard's agents) and, while they run, the pane agents' (the retro's,
// a notes curation's or critique's, a skill edit's), each over a scratch
// registry of its own (retro_pane.go), which register while the daemon
// listens. The listener's goroutine is its only caller.
type hookRouter struct {
	mu sync.Mutex
	// ctx is the daemon's while it listens (open); nil: no listener, and
	// no pane agent registers.
	ctx   context.Context
	sinks []*hookSink
}

// hookSink is one pane agent's manager and its registry, registered with
// the router (add): ctx, the daemon's own while the sink is registered,
// bounds the later looks of its Wakes, and busy counts the Wakes in
// flight.
type hookSink struct {
	what string // the pane agent ("retro", "curation", ...)
	st   *store.Store
	mgr  hookManager
	ctx  context.Context
	stop context.CancelFunc
	busy sync.WaitGroup
}

// hookManager is the part of an agents manager the router drives.
type hookManager interface {
	Wake(ctx context.Context, ev agenthooks.Event)
	WaitWakes()
}

// open has the router take pane agents, whose Wakes run on ctx (the
// daemon's), and close stops it taking more.
func (r *hookRouter) open(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

func (r *hookRouter) close() {
	r.mu.Lock()
	r.ctx = nil
	r.mu.Unlock()
}

// add registers a pane agent's manager mgr over its registry st, and
// returns what remove takes; nil (nothing registered) while the daemon does
// not listen, or on a nil router.
func (r *hookRouter) add(what string, st *store.Store, mgr hookManager) *hookSink {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return nil
	}
	s := &hookSink{what: what, st: st, mgr: mgr}
	s.ctx, s.stop = context.WithCancel(r.ctx)
	r.sinks = append(r.sinks, s)
	return s
}

// remove takes s out of the router before its pane agent's registry
// closes: no event reaches it after, its later looks end (its context's
// cancel), the Wake in flight returns, and then WaitWakes waits for the
// looks. A nil s does nothing.
func (r *hookRouter) remove(s *hookSink) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	r.sinks = slices.DeleteFunc(r.sinks, func(x *hookSink) bool { return x == s })
	r.mu.Unlock()
	s.stop()
	s.busy.Wait()
	s.mgr.WaitWakes()
}

// take is the registered sinks, each counted busy (its caller marks each
// done).
func (r *hookRouter) take() []*hookSink {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sinks {
		s.busy.Add(1)
	}
	return slices.Clone(r.sinks)
}

// routeHook hands hook event ev to Wake on every agents manager whose
// registry records a live session on its pane (hookOwner): the daemon's own
// on ctx, the daemon's, and each registered pane agent's on its sink's. A
// ping (agenthooks.PingName: the doctor's probe, which only checks that the
// socket takes a datagram) is dropped without a word, and an event of a
// pane no registry records is dropped with a debug line naming the event
// alone (an agent picks the pane, so it is never logged).
func (e *Engine) routeHook(ctx context.Context, ev agenthooks.Event) {
	if ev.Name == agenthooks.PingName {
		return
	}
	routed := false
	if e.d.Agents != nil && e.hookOwner(ctx, e.st, ev, "daemon") {
		e.d.Agents.Wake(ctx, ev)
		routed = true
	}
	sinks, next := e.agentHooks.take(), 0
	defer func() { // a Wake that panicked (the listener recovers it) leaves no sink busy
		for _, s := range sinks[next:] {
			s.busy.Done()
		}
	}()
	for ; next < len(sinks); next++ {
		s := sinks[next]
		if e.hookOwner(s.ctx, s.st, ev, s.what) {
			s.mgr.Wake(s.ctx, ev)
			routed = true
		}
		s.busy.Done()
	}
	if !routed {
		e.log.Debug("hook event of no magnum session dropped", "event", ev.Name)
	}
}

// hookOwner reports whether registry st (named registry in the log) records
// a live session on ev's pane, and logs the event's debug line then: its
// name, the pane (a herdr id the registry recorded), the registry, the
// session's role and row and its run in flight, never a field of the
// payload.
func (e *Engine) hookOwner(ctx context.Context, st *store.Store, ev agenthooks.Event, registry string) bool {
	live, err := st.LiveSessions(ctx)
	if err != nil {
		e.log.Debug("hook event: the live sessions cannot be read", "event", ev.Name, "registry", registry, "err", err)
		return false
	}
	var s *store.Session
	for i := range live {
		if ev.Pane != "" && deref(live[i].HerdrPaneID) == ev.Pane && (s == nil || live[i].ID > s.ID) {
			s = &live[i]
		}
	}
	if s == nil {
		return false
	}
	run := ""
	if active, err := st.ActiveRuns(ctx); err == nil {
		for _, r := range active {
			if r.SessionID != nil && *r.SessionID == s.ID {
				run = r.ID
			}
		}
	}
	e.log.Debug(hookEventMsg, "event", ev.Name, "pane", ev.Pane, "registry", registry, "role", s.Role, "session", s.ID, "run", run)
	return true
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

// learnAgents makes the pane agents' managers (paneDeps.Agents): agents.New
// over a pane agent's scratch registry with the live herdr client h, tagged
// LearnAgentTag, with the App's hook socket, so the panes they make get the
// hook env exactly while the daemon listens (agents.Deps.HookSocket; the
// pane agents' EnsureWorkspace passes no env of its own), and the
// StopFailures their hooks prove go to the App's queue, as its own
// manager's do (agents.Deps.HookFailure).
func learnAgents(h agents.Herdr, a *app.App) func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
	return func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
		d := agents.Deps{Herdr: h, Store: st, Runner: a.Runner, Config: cfg, Layout: layout, Tag: LearnAgentTag,
			Log: app.Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "learn"}, HookSocket: a.HookSocket}
		if a.HookFailures != nil {
			d.HookFailure = a.SendHookFailure
		}
		return agents.New(d)
	}
}

// onHookFailure feeds a StopFailure the agents' hooks proved (app.HookFailure,
// from Deps.HookFailures) into the handling the screen-read failure of its
// kind gets today: a usage limit and a login loss, the screen's verdict on
// a rate limit, an authentication or a billing error, pause the agent kind
// at once (pauseTool, proven now: what a reviewer's run that proved them
// does at its end, pauseAtOnce, and the retro's and the notes jobs' do),
// a usage limit until the reset the screen names. An overload, a model
// limit and any other failure pause nothing: the run's own end reads its
// screen again and goes on as before (the round's backoff or its shared
// overload, its model switch, the job's stop), so it logs at debug level.
// A turn's failure is no launch failure (the agent took its prompt), so it
// never holds the kind as a shared launch failure does (kind_hold.go).
func (e *Engine) onHookFailure(ctx context.Context, f app.HookFailure) {
	tool := cmp.Or(deref(f.Session.AgentKind), agents.KindClaude)
	switch f.Screen.Kind {
	case agents.HealthUsageLimit, agents.HealthLoginRequired:
		p := pipeline.Pause{Kind: string(f.Screen.Kind), Tool: tool, Detail: f.Screen.Detail, Proven: true, ProvenAt: e.now()}
		if f.Screen.ResetAt != nil {
			p.Until = *f.Screen.ResetAt
		}
		e.log.Info("an agent's hook says its turn ended on an API error its screen shows: the kind pauses", "tool", tool,
			"kind", p.Kind, "failure", string(f.Failure), "role", f.Session.Role, "session", f.Session.ID)
		e.pauseTool(ctx, p)
	default:
		e.log.Debug("an agent's hook says its turn ended on an API error: its run's end handles it", "tool", tool,
			"kind", string(f.Screen.Kind), "failure", string(f.Failure), "role", f.Session.Role, "session", f.Session.ID)
	}
}

// hookFailures handles the queued StopFailures (Deps.HookFailures) one at a
// time until ctx ends; one whose handling panicked costs itself alone.
func (e *Engine) hookFailures(ctx context.Context, in <-chan app.HookFailure) {
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-in:
			e.safely(ctx, "hook failure", "", func() { e.onHookFailure(ctx, f) }, nil)
		}
	}
}

// KVDaemonHooks is the running daemon's hook listener as its start found
// it, DaemonHooks's JSON, for `magnum status`; absent when the start took
// no claim (a dry run).
const KVDaemonHooks = "daemon.hooks"

// DaemonHooks is whether the daemon listens for its agents' hooks: the
// listener's socket, or why it has none (its agents are then polled
// alone).
type DaemonHooks struct {
	Socket string `json:"socket,omitempty"`
	Off    string `json:"off,omitempty"`
}

// ReadDaemonHooks is the running daemon's hook listener (KVDaemonHooks);
// false before a daemon recorded it.
func ReadDaemonHooks(ctx context.Context, st *store.Store) (DaemonHooks, bool) {
	v, ok, err := st.GetKV(ctx, KVDaemonHooks)
	if err != nil || !ok || v == "" {
		return DaemonHooks{}, false
	}
	var h DaemonHooks
	if json.Unmarshal([]byte(v), &h) != nil {
		return DaemonHooks{}, false
	}
	return h, true
}

// recordHooks writes whether this start listens for its agents' hooks
// (KVDaemonHooks): the claim's socket, or why it has none; without a
// claim it removes an earlier start's record.
func (e *Engine) recordHooks(ctx context.Context, c *Claim) {
	var h DaemonHooks
	switch {
	case c.HookSocket() != "":
		h.Socket = c.HookSocket()
	case c.Off() != "":
		h.Off = c.Off()
	default:
		e.delKV(ctx, KVDaemonHooks)
		return
	}
	if raw, err := json.Marshal(h); err == nil {
		e.setKV(ctx, KVDaemonHooks, string(raw))
	}
}
