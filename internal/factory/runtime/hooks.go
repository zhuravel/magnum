package runtime

// The agents' own hooks (R50 slice 2): the factory daemon listens on its
// hook socket (Layout.FactoryHookSocket), every pane the runtime makes from
// then on names it (UseHookSocket), and the engine hands each event an
// agent's hook sends there to Wake, which looks at that pane now. A
// StopFailure whose screen shows a usage limit, a lost login or an overload
// pauses the agent's kind at once (hookFailure). An event is a hint, never
// the source of truth: the observe passes still end the runs and the
// stages still take their events.

import (
	"context"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// UseHookSocket has every pane the runtime makes from now on carry the
// factory daemon's hook socket at path and the magnum binary
// (agents.Deps.HookSocket: MAGNUM_HOOK_SOCKET, MAGNUM_HOOK_BIN), so its
// agents' hooks send their events there. The daemon calls it once, under
// its lock, when its listener is up and before its startup: it builds the
// agents manager anew with the socket, so it must come before the runtime's
// first use. "" keeps the panes without the hook env.
func (r *Runtime) UseHookSocket(path string) {
	d := r.mgrDeps
	d.HookSocket = path
	r.mgr = agents.New(d)
}

// Wake hands an event of an agent's hook to the agents manager
// (agents.Manager.Wake), which looks at the event's pane now; the next
// observe pass returns what the look saw. ctx is the daemon's: it bounds the
// later looks Wake starts, which WaitWakes waits for. Wake takes no lock of
// the runtime's: the manager runs the runtime's callbacks inside it
// (ownerName and hookFailure take r.mu), so a caller must hold none either.
func (r *Runtime) Wake(ctx context.Context, ev agenthooks.Event) { r.mgr.Wake(ctx, ev) }

// WaitWakes waits for the later looks the events' Wake calls started
// (agents.Manager.WaitWakes): the daemon calls it once its listener stopped
// and the last Wake returned.
func (r *Runtime) WaitWakes() { r.mgr.WaitWakes() }

// hookFailure is the agents manager's HookFailure: the class of API error
// a Claude turn of session s ended on, as its own StopFailure hook named it
// and its screen, read again, shows it (h). A usage limit (until its reset),
// a lost login and an overload pause the session's kind now, as the pass
// that ends the turn pauses it on that screen (completed: pauseOf), so the
// kind's launches and prompts wait from the hook on, not from that pass,
// whatever the class (the screen decides, as on the pass). The run itself
// still ends on that pass, which pauses the kind again with the same reset,
// abandons the run as paused and brings the stage its EventPaused. A
// model's own limit (the session moves to a fallback model) and any other
// screen are the pass's alone, and a liaison's screen, no role of the
// task's, pauses nothing, as on the pass. A pause of the same kind in force
// is left as it is. It runs on the listener's goroutine or the manager's,
// never waits on the loop and reads nothing: it takes r.mu for the pause
// alone. Its log names the class and the screen's kind, never the screen's
// text.
func (r *Runtime) hookFailure(s store.Session, failure agents.StopFailure, h agents.Health) {
	who, kind := pauseOf[h.Kind], store.Deref(s.AgentKind)
	if who == "" || kind == "" || s.Role == LiaisonRole {
		return
	}
	if w := r.paused(kind); w != nil && w.Who == who {
		return
	}
	var until time.Time
	if h.ResetAt != nil {
		until = *h.ResetAt
	}
	r.log.Info("an agent's StopFailure hook pauses its kind now", "kind", kind, "role", s.Role, "session", s.ID,
		"failure", string(failure), "screen", string(h.Kind))
	_ = r.pause(context.Background(), kind, who, until) // its wait is what the kind's launches and prompts answer
}
