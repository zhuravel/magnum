package runtime

// The agents' own hooks (R50 slice 2): the factory daemon listens on its
// hook socket (Layout.FactoryHookSocket), every pane the runtime makes from
// then on names it (UseHookSocket), and the engine hands each event an
// agent's hook sends there to Wake, which looks at that pane now. A
// StopFailure its screen proves asks the loop for an observe pass now
// (hookFailure, ObserveSoon), and a run's end a later look found closes
// Ended. An event is a hint, never the source of truth: the observe passes
// still end the runs, decide the pauses, and the stages still take their
// events.

import (
	"context"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// UseHookSocket has every pane the runtime makes from now on carry the
// factory daemon's hook socket at path and the magnum binary
// (agents.Manager.SetHookSocket: MAGNUM_HOOK_SOCKET, MAGNUM_HOOK_BIN), so
// its agents' hooks send their events there; a pane already made keeps its
// env. The daemon calls it under its lock once its listener is up, before
// its startup, so every pane it makes names the socket. "" leaves the panes
// made from then on without the hook env.
func (r *Runtime) UseHookSocket(path string) { r.mgr.SetHookSocket(path) }

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

// hookFailure is the agents manager's HookFailure: a Claude turn of session
// s ended on an API error of class failure, as its own StopFailure hook
// named it and its screen, read again, shows it (h). It decides no pause:
// it asks the daemon's loop for an observe pass now (ObserveSoon), without
// blocking (a request pending absorbs it), and that pass applies its own
// rule to the screen after the run's prompt (completed, health's prompt
// anchor), as without hooks, so a limit line an earlier turn left above
// the prompt pauses nothing. It runs on the listener's goroutine or the
// manager's, takes no lock and never waits on the loop. Its debug line
// names the class and the screen's kind, never the screen's text.
func (r *Runtime) hookFailure(s store.Session, failure agents.StopFailure, h agents.Health) {
	r.log.Debug("an agent's StopFailure hook: the observe pass reads its screen now", "role", s.Role, "session", s.ID,
		"failure", string(failure), "screen", string(h.Kind))
	select {
	case r.soon <- struct{}{}:
	default:
	}
}

// ObserveSoon is where the runtime asks the daemon's loop for an observe
// pass now (a StopFailure hook its screen proves: hookFailure); one request
// pending absorbs the others.
func (r *Runtime) ObserveSoon() <-chan struct{} { return r.soon }

// Ended is the agents manager's Ended: a channel that closes once a pass,
// or a hook's look, ends a run or loses a session after it was taken, so
// the loop takes a turn's end that a later look of Wake's found at once
// (each pass takes a fresh one).
func (r *Runtime) Ended() <-chan struct{} { return r.mgr.Ended() }
