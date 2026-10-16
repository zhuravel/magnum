package engine

// Closing the panes of retired sessions (DECISIONS "A parked session's pane
// is closed at every park and at the restart"): a park, a hard park or an
// agent that went away leaves its session's pane as a shell ("Resume this
// session with: …"), and the next workspace splits a new pane per role.
// Spec 52 closed them only after a fresh start's park, so talkable#11932's
// workspace held 21 panes for 4 live sessions on 2026-10-17. Every park
// that works now closes the panes it retired, a lost session's pane closes
// at the tick that marks it lost, and the reconcile (at the start, then
// every reconcile_interval) closes what is left for every PR with a pane.

import (
	"context"
	"fmt"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// closeRetiredPanes closes, after a park that worked (the idle park, a
// Codex-flagged PR's release, an eviction, a failed open's release, a
// fresh start's park, a stop's park), the panes of the PR's retired
// sessions (agents.Manager.CloseRetiredPanes: an idle shell no starting or
// live session records; a shell the operator opened is never one). A
// parked session keeps its conversation, resumed later in a new pane. A
// failure is only logged: the park held.
func (e *Engine) closeRetiredPanes(ctx context.Context, pr store.PR) {
	if err := e.d.Agents.CloseRetiredPanes(ctx, pr); err != nil {
		e.warnUnlessStopped(ctx, err, "close the panes of retired sessions", "pr", pr.ID)
	}
}

// retiredPanesHeld is what dispatch shows (heldReason) for a PR whose
// retired panes are being closed.
const retiredPanesHeld = "the panes of its retired sessions are being closed"

// closePanesJob closes the retired panes of PR prID, whose session the
// tick just marked lost, under the PR's slot-work reservation. A round, an
// open, a fix or a whiteboard job holding the PR skips it: its setup reuses
// or replaces the lost pane, and the reconcile closes what is left once it
// let go.
func (e *Engine) closePanesJob(prID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveSlotWork(prID, retiredPanesHeld) {
			return nil
		}
		defer e.endEviction(prID)
		pr, err := e.st.PRByID(ctx, prID)
		if err != nil {
			return err
		}
		e.closeRetiredPanes(ctx, pr)
		return nil
	}
}

// closeLostPanes queues closePanesJob for the PR of a session the tick
// marked lost (onObservation).
func (e *Engine) closeLostPanes(prID int64) {
	if e.d.DryRun || e.d.Agents == nil {
		return
	}
	e.enqueueHeavy(fmt.Sprintf("close-panes:%d", prID), e.closePanesJob(prID))
}

// closeLeftoverPanes closes, at every reconcile, the retired panes of every
// PR a session of which records a pane herdr has: one herdr snapshot, one
// query for those PRs, then one sessions query each (agents.Manager.
// CloseRetiredPanesIn), each under the PR's slot-work reservation; a PR a
// round, an open, a fix, a whiteboard job or a park holds is left for the
// next reconcile. A PR with nothing to close records nothing; herdr down
// skips the pass.
func (e *Engine) closeLeftoverPanes(ctx context.Context) {
	if e.d.DryRun || e.d.Agents == nil || e.d.Herdr == nil {
		return
	}
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		e.log.Debug("reconcile: no herdr snapshot; retired panes wait", "err", err)
		return
	}
	ids, err := e.st.PRsWithPanes(ctx, paneIDs(snap))
	if err != nil {
		e.warnUnlessStopped(ctx, err, "reconcile: PRs with panes")
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		e.closeLeftoverPanesOf(ctx, id, snap)
	}
}

// closeLeftoverPanesOf is closeLeftoverPanes for one PR.
func (e *Engine) closeLeftoverPanesOf(ctx context.Context, prID int64, snap herdr.Snapshot) {
	if !e.reserveSlotWork(prID, retiredPanesHeld) {
		return
	}
	defer e.endEviction(prID)
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "reconcile: retired panes", "pr", prID)
		return
	}
	if err := e.d.Agents.CloseRetiredPanesIn(ctx, pr, snap); err != nil {
		e.warnUnlessStopped(ctx, err, "reconcile: close the panes of retired sessions", "pr", prID)
	}
}

// paneIDs are the ids of the panes in snap.
func paneIDs(snap herdr.Snapshot) []string {
	out := make([]string, 0, len(snap.Panes))
	for _, p := range snap.Panes {
		out = append(out, p.ID)
	}
	return out
}
