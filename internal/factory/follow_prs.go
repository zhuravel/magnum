package factory

// The tick follows the published tasks' PRs in magnum's registry
// (Magnum.PRStates), which it reads and never writes: a merged PR makes its
// task done, the factory's only writer of done, with the end a stop gives
// (closeTask); a PR closed without a merge keeps its task published and,
// past [daemon] close_grace, has it wait on you, since a published task
// has no stage to go back to (needs_you is a live stage's, which answer,
// retry and ready act on); a reopen clears that wait. Unknown moves
// nothing: a registry that cannot be read, a PR magnum does not have, its
// daemon down.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// closedWait is what a published task whose PR #n closed without a merge
// waits on you for.
func closedWait(n int) string {
	return fmt.Sprintf("PR #%d closed without a merge: %s", n, reopenMove)
}

// followPRs follows the PRs of tasks that are published with one (refs,
// publishedRefs), on every tick, whether or not the tick advances (it needs
// neither herdr nor MySQL; a park herdr cannot take waits, parkEnded): see
// the file's comment. A failed read is logged once while it stays the same. It
// returns what magnum's registry said of the PRs (nil when it read none),
// which the PRs' status files are written from (writePRStatuses).
func (e *Engine) followPRs(ctx context.Context, tasks []registry.Task, refs map[int64]PRRef) map[PRRef]PRState {
	if e.d.Magnum == nil {
		return nil
	}
	var list []PRRef
	for _, t := range tasks {
		if ref, ok := refs[t.ID]; ok {
			list = append(list, ref)
		}
	}
	if len(list) == 0 {
		return nil
	}
	states, err := e.d.Magnum.PRStates(ctx, list)
	if err != nil {
		if msg := textx.FirstLine(execx.Redact(err.Error())); msg != e.followErr {
			e.followErr = msg
			e.log.Warn("follow the published tasks' PRs: magnum's registry cannot be read; no task moves", "err", msg)
		}
		return nil
	}
	e.followErr = ""
	now := e.now()
	for _, t := range tasks {
		ref, ok := refs[t.ID]
		if !ok {
			continue
		}
		st, ok := states[ref]
		if !ok || !st.Found {
			continue
		}
		e.copyCI(ctx, t, st)             // what magnum recorded holds with its daemon down
		e.followEscapes(ctx, t, ref, st) // off the loop
		if !st.DaemonUp {
			continue
		}
		if err := e.followPR(ctx, t, ref, st, now); err != nil && !errors.Is(err, registry.ErrConflict) {
			e.log.Warn("follow a published task's PR", "task", t.ID, "pr", ref.Number, "err", err)
		}
	}
	return states
}

// followPR moves published task t by its PR's state st in magnum's
// registry at now.
func (e *Engine) followPR(ctx context.Context, t registry.Task, ref PRRef, st PRState, now time.Time) error {
	n := ref.Number
	marked := t.WaitsWho == waitsYou && t.WaitsWhat == closedWait(n)
	switch {
	case st.GHState == store.GHMerged:
		why := fmt.Sprintf("PR #%d merged", n)
		if err := e.closeTask(ctx, t, taskEnd{from: []string{registry.TaskPublished}, to: registry.TaskDone, because: why}); err != nil {
			return err
		}
		e.event(ctx, t.ID, "pr", registry.PhaseOK, why+": the task is done")
	case st.GHState == store.GHClosed:
		closed := t.Updated
		if st.ClosedAt != nil {
			closed = *st.ClosedAt
		}
		if marked || now.Before(closed.Add(e.cfg.Daemon.CloseGrace.Duration)) {
			return nil
		}
		what := closedWait(n)
		if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskPublished}, "", func(u *registry.TaskUpdate) {
			u.Set("waits_who", waitsYou)
			u.Set("waits_what", what)
			u.Because(fmt.Sprintf("PR #%d closed without a merge", n))
		}); err != nil {
			return err
		}
		e.alert(ctx, t.ID, TaskKey(t)+": "+waitsYou+": "+what)
	case st.GHState == store.GHOpen && marked:
		if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskPublished}, "", func(u *registry.TaskUpdate) {
			u.Set("waits_who", "")
			u.Set("waits_what", "")
			u.Because(fmt.Sprintf("PR #%d reopened", n))
		}); err != nil {
			return err
		}
		e.event(ctx, t.ID, "pr", registry.PhaseOK, fmt.Sprintf("PR #%d reopened: the task waits on magnum again", n))
	}
	return nil
}

// copyCI writes the CI of published task t's PR, as magnum's registry last
// read it (st.CI at st.Head: tui.CIInfo, by the repository's required
// checks), to the task's checks once per head and result (kind ci, source
// github, RecordGitHubCI): a passed or a failed head's, never a pending,
// skipped or absent one's, nor CI that describes an older commit than the
// head (Stale). A head that failed and then passed after a rerun writes
// both. The cause stays "": nothing magnum reads of a PR's CI says a
// failure is not the PR's own (the staging status's not ours is of the
// staging branch's ci.yml run, not of the PR's checks). The last head and
// result written per task are kept, so a tick that sees them again writes
// nothing; a failed write is logged once while it stays the same.
func (e *Engine) copyCI(ctx context.Context, t registry.Task, st PRState) {
	ci := st.CI
	if ci == nil || ci.Stale || st.Head == "" {
		return
	}
	var result string
	switch ci.State {
	case "passed":
		result = registry.CheckPass
	case "failed":
		result = registry.CheckFail
	default:
		return
	}
	seen := st.Head + " " + result
	if e.ciSeen[t.ID] == seen {
		return
	}
	if _, err := e.reg.RecordGitHubCI(ctx, t.ID, st.Head, result, ""); err != nil {
		if msg := textx.FirstLine(execx.Redact(err.Error())); msg != e.ciErr {
			e.ciErr = msg
			e.log.Warn("copy a published task's PR CI to its checks", "task", t.ID, "err", msg)
		}
		return
	}
	e.ciErr = ""
	e.ciSeen[t.ID] = seen
}
