// Package runtime is the factory's agent runtime: the owner rows of its
// tasks in the factory's own agents store, the launches it records before
// any workspace exists, the prompt ledger that never sends a prompt of
// unknown delivery twice, the pauses that are not failures, and a watchdog
// with its recovery steps (docs/superpowers/specs/2026-11-03-factory-design.md,
// sections 4.3, 10 and 10.1).
//
// The factory engine (package factory) drives it through its Runtime
// interface; this file holds the events that interface reports, which the
// engine declares before the runtime itself exists.
package runtime

// EventKind is what happened to a run of a factory agent.
type EventKind string

// The kinds of Event.
const (
	// EventEnded: the run ended (its pane went idle); the stage checks its
	// end fact.
	EventEnded EventKind = "ended"
	// EventBlocked: the pane shows a dialog the factory does not answer.
	EventBlocked EventKind = "blocked"
	// EventPaused: a usage limit, a login or an overload screen; Reason says
	// which and until when, when known. A pause is not a failure.
	EventPaused EventKind = "paused"
	// EventLost: the pane is gone and its recoveries ran out.
	EventLost EventKind = "lost"
	// EventStuck: the watchdog's steps ran out on a hung turn; Reason holds
	// the pane's last line.
	EventStuck EventKind = "stuck"
	// EventNoProgress: a loop without progress outlived its compaction and
	// its fresh session.
	EventNoProgress EventKind = "no_progress"
	// EventRefused: a Codex cybersecurity warning; the task never runs on
	// Codex again.
	EventRefused EventKind = "refused"
	// EventHooksTrust: a Codex session shows the hooks-trust dialog.
	EventHooksTrust EventKind = "hooks_trust"
	// EventDeliveryUnknown: a prompt to the role may or may not have
	// arrived, and no transcript can tell (Runtime.Reconcile): the role gets
	// no prompt until the operator looked at its pane and let it go
	// (Runtime.LetGo).
	EventDeliveryUnknown EventKind = "delivery_unknown"
	// EventUndelivered: a prompt of unknown delivery is proven never to
	// have arrived (Runtime.Reconcile's no): its entry is settled no and its
	// run abandoned, and the stage sends the role what it is due once more.
	EventUndelivered EventKind = "undelivered"
	// EventDelivered: a prompt of unknown delivery is found in its
	// transcript by a later read (Runtime.Reconcile's yes): its entry is
	// settled yes and its run watched as submitted; nothing is sent again,
	// and a wait on you for it is settled.
	EventDelivered EventKind = "delivered"
)

// PausedPrefix leads the error of a run its CLI's pause ended (a usage
// limit, a login, an overload): the run is abandoned with "paused: " and
// the pause's wait ("usage: claude limit until 16:00"), which RunState
// returns, so a stage rebuilt after a restart knows the role is to go on
// once the pause is over.
const PausedPrefix = "paused: "

// Event is one thing the runtime saw on a run of a task's role.
type Event struct {
	TaskID int64
	Role   string
	RunID  string
	Kind   EventKind
	Reason string
}
