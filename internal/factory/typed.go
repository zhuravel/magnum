package factory

// The turns you typed into a task's panes (spec section 11.1). The observe
// pass hands over every user turn of an agent's transcript that the factory
// did not send (runtime.EventTyped, with the proof of its origin), and each
// becomes a pane_typed input once: the registry keys it by its role and the
// time you typed it, so a pass that hands it again, a second session that
// holds it, or a restart records nothing more and loses nothing. Its words
// are scrubbed, a long paste is dropped with its reason, and a turn within
// minutes of an access card is dropped, as it may hold a code or a password.
// A turn of unknown origin is kept for evaluation only: the stats count it
// apart, and no lesson reads it.

import (
	"context"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// paneSurface is the Surface of a turn typed into a pane.
const paneSurface = "pane"

// accessWait leads the WaitsWhat of a task that waits on you for an access
// step (a login, MFA or VPN step: its access card, the NEEDS YOU kind
// "access needed"), as its spans record the wait.
const accessWait = "access needed"

// accessMargin is how near an access card a typed turn is dropped: from
// this long before the task began to wait on you for access to this long
// after the wait ended (or now, while it lasts).
const accessMargin = 5 * time.Minute

// nearAccess is the DroppedReason of a turn typed near an access card.
const nearAccess = "within minutes of an access card: it may hold a code or a password"

// typed records the turns of ev (runtime.EventTyped) that the task's
// inputs do not hold yet, each as a pane_typed input of ev's role, through
// recordInput (its class with it).
func (e *Engine) typed(ctx context.Context, ev runtime.Event) {
	if len(ev.Turns) == 0 {
		return
	}
	ins, err := e.reg.Inputs(ctx, ev.TaskID)
	if err != nil {
		e.log.Warn("record the turns typed into a pane", "task", ev.TaskID, "role", ev.Role, "err", err)
		return
	}
	spans, err := e.reg.Spans(ctx, ev.TaskID)
	if err != nil {
		e.log.Warn("record the turns typed into a pane", "task", ev.TaskID, "role", ev.Role, "err", err)
		return
	}
	now := e.now()
	for _, tu := range ev.Turns {
		if typedBefore(ins, ev.Role, tu.At) {
			continue
		}
		in := registry.Input{TaskID: ev.TaskID, Kind: registry.InputPaneTyped, Surface: paneSurface, Stage: tu.Stage, Role: ev.Role,
			Origin: tu.Origin, AgentState: tu.AgentState, At: tu.At}
		if nearAccessCard(spans, tu.At, now) {
			in.DroppedReason = nearAccess // and the agent's turns, which may repeat the code
		} else {
			in = withWords(in, tu.Text)
			in.Before = scrubbed(tu.Before)
		}
		if _, err := e.recordInput(ctx, in); err != nil {
			e.log.Warn("record a turn typed into a pane", "task", ev.TaskID, "role", ev.Role, "err", err)
			continue
		}
		ins = append(ins, in)
	}
}

// typedBefore reports whether ins holds the turn role's pane took at at.
func typedBefore(ins []registry.Input, role string, at time.Time) bool {
	for _, in := range ins {
		if in.Kind == registry.InputPaneTyped && in.Role == role && in.At.Equal(at) {
			return true
		}
	}
	return false
}

// nearAccessCard reports whether at lies within accessMargin of one of the
// task's waits on you for access (spans: its span to the next, or to now
// while it lasts).
func nearAccessCard(spans []registry.Span, at, now time.Time) bool {
	for i, s := range spans {
		if !s.OnYou || !strings.HasPrefix(s.WaitsWhat, accessWait) {
			continue
		}
		end := now
		if i+1 < len(spans) {
			end = spans[i+1].At
		}
		if !at.Before(s.At.Add(-accessMargin)) && !at.After(end.Add(accessMargin)) {
			return true
		}
	}
	return false
}
