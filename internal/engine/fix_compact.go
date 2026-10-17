package engine

// The fixer's compaction (spec 57): the fixer is one Claude Code session
// resumed across every fix cycle of a PR, and nothing rotates it as
// max_session_rounds rotates the judge. Claude Code compacts a full
// conversation by itself, in the middle of a turn, and can lose the finding
// it was fixing. So before a cycle sends its prompt to a fixer it resumed,
// the job reads how full the fixer's context is from the pane's status
// lines (the kind's context_pattern) and, at or above [pipeline]
// fix_compact_at, has it compact between two turns. A fixer still that full
// afterwards, or whose /compact did not finish, is parked and the cycle
// starts it fresh: the fix prompt carries the findings anyway. A fixer
// resumed from a park draws its status line only some time after the resume
// (spec 59), so its pane is read until a figure shows, for a minute at most,
// once per PR until a figure is read (spec 71). A pane that shows no figure
// leaves the fixer as it is and warns once per PR. A fixer started fresh is
// never compacted.

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRFixContextUnknown records when the PR's fixer pane first showed no
// context figure although fix_compact_at is on (store.FormatTime): the
// fix.context_unknown warning is said once per PR, until a figure is read.
func kvPRFixContextUnknown(prID int64) string { return fmt.Sprintf("pr.%d.fix_context_unknown", prID) }

// How long fixCompact waits for the context figure of a fixer the cycle
// resumed from a park: Claude Code draws its status line some time after
// the resume (the first two live warnings came 0.2 s after it, and the same
// panes showed the figure later), so the pane is read every
// fixContextPoll until a figure shows, for fixContextAwait at most.
const (
	fixContextPoll  = 2 * time.Second
	fixContextAwait = time.Minute
)

// fixCompact runs before a fix cycle prompts a fixer it resumed (its
// session was live, or ensureAgent resumed a parked one: resumed), when
// [pipeline] fix_compact_at is on and the fixer's kind has a
// context_pattern: it reads the fixer's context (readContext, which waits
// for the figure of a fixer resumed from a park) and leaves a figure under
// the key alone; at or above it, it sends /compact (agents.Manager.Compact),
// reads again and records fix.compacted (from N% to M%) when the figure fell
// under the key. A figure still at or above it and a compaction that did not
// finish start the fixer fresh (fixFresh). A pane that shows no figure,
// before the compaction or after one that ended, leaves the session as it is
// (fix.context_unknown, a warning said once per PR; after a compaction,
// fix.compacted from N%): a figure missing right after a resume would
// otherwise cost a fresh fixer each cycle. The cycle's row says what
// happened (FixState.Context). Only ctx's end and a fresh start that fails
// fail the cycle.
func (e *Engine) fixCompact(ctx context.Context, job *fixJob, ws agents.Workspace, resumed bool) error {
	at := e.cfg.Pipeline.FixCompactAt
	if at <= 0 {
		return nil
	}
	kind := job.role.AgentKind()
	if k, ok := e.cfg.KindSpec(kind); !ok || k.ContextPattern == "" {
		return nil // the kind's panes show no figure: nothing to read
	}
	s, err := e.st.LiveSessionByPRRole(ctx, job.pr.ID, job.role.Name)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "fix: the fixer's session before its context check", "subject", job.subject())
		return ctx.Err()
	}
	data := func(extra map[string]any) map[string]any {
		d := map[string]any{"role": job.role.Name, "cycle": job.state.Cycle, "at": at, "session": s.ID}
		maps.Copy(d, extra)
		return d
	}
	from, ok, err := e.readContext(ctx, job, s, resumed)
	if err != nil {
		return err
	}
	if !ok { // a figure missing costs no fresh fixer
		e.warnContextUnknown(ctx, job, kind, at, resumed)
		return nil
	}
	if from < at {
		return nil
	}
	cerr := e.d.Agents.Compact(ctx, s)
	if err := ctx.Err(); err != nil {
		return err
	}
	to, ok := e.d.Agents.ContextUsed(ctx, s)
	switch {
	case cerr != nil:
		return e.fixFresh(ctx, job, ws, s, fmt.Sprintf("its context was at %d%% and /compact did not finish: %v (fix_compact_at %d)", from, cerr, at),
			"/compact failed", data(map[string]any{"from": from}))
	case !ok: // the compaction ended: the session goes on, and the missing figure warns once
		e.warnContextUnknown(ctx, job, kind, at, false)
		job.state.Context = fmt.Sprintf("compacted from %d%%", from)
		e.event(ctx, "info", job.subject(), "fix.compacted", fmt.Sprintf("%s compacted its conversation before cycle %d: its context was at %d%%, and its pane shows no figure after /compact (fix_compact_at %d)",
			job.role.Name, job.state.Cycle, from, at), data(map[string]any{"from": from}))
		return nil
	case to >= at:
		return e.fixFresh(ctx, job, ws, s, fmt.Sprintf("its context was at %d%% after /compact (%d%% before; fix_compact_at %d)", to, from, at),
			fmt.Sprintf("%d%% after /compact", to), data(map[string]any{"from": from, "to": to}))
	}
	job.state.Context = fmt.Sprintf("compacted %d%% → %d%%", from, to)
	e.event(ctx, "info", job.subject(), "fix.compacted", fmt.Sprintf("%s compacted its conversation before cycle %d: its context went from %d%% to %d%% (fix_compact_at %d)",
		job.role.Name, job.state.Cycle, from, to, at), data(map[string]any{"from": from, "to": to}))
	return nil
}

// readContext reads the fixer's context figure from the pane of session s
// (agents.Manager.ContextUsed). The pane of a fixer the cycle resumed from a
// park (resumed) may not show its status line yet, so it is read again every
// fixContextPoll until a figure shows, for fixContextAwait at most; a fixer
// that was live is read once, and so is one resumed while the PR's
// fix.context_unknown warning (kvPRFixContextUnknown) is remembered: that
// minute was waited already and no figure came, and a pane whose status line
// never matches the context_pattern would cost it on every resumed cycle
// (spec 71). A figure read forgets the warning, so a later miss waits again
// and warns again. Only ctx's end fails it.
func (e *Engine) readContext(ctx context.Context, job *fixJob, s store.Session, resumed bool) (int, bool, error) {
	key := kvPRFixContextUnknown(job.pr.ID)
	if _, waited := e.getKV(ctx, key); waited {
		resumed = false // the minute was waited for this PR already: read once
	}
	for i := 1; ; i++ {
		if used, ok := e.d.Agents.ContextUsed(ctx, s); ok {
			if _, warned := e.getKV(ctx, key); warned {
				e.delKV(ctx, key)
			}
			return used, true, nil
		}
		if !resumed || i > int(fixContextAwait/fixContextPoll) {
			return 0, false, nil
		}
		if err := e.d.Sleep(ctx, fixContextPoll); err != nil {
			return 0, false, err
		}
	}
}

// fixFresh parks the fixer of session s (quitToRenew: its conversation is
// kept, as for a cold judge) and starts it fresh in its pane for the cycle,
// whose prompt is then the fresh session's first turn: fix.fresh_session
// with why, and the cycle's row says "fresh session (<words>)". A fixer that
// works, is blocked, or does not quit goes on in its conversation (logged);
// a fresh start that fails fails the cycle.
func (e *Engine) fixFresh(ctx context.Context, job *fixJob, ws agents.Workspace, s store.Session, why, words string, data map[string]any) error {
	if cur, err := e.st.SessionByID(ctx, s.ID); err == nil {
		s = cur // the agent's status as the observer saw it last, after a compaction
	}
	quit, err := e.quitToRenew(ctx, s, "fix fresh session")
	if err != nil {
		return fmt.Errorf("start %s fresh: %w", job.role.Name, err)
	}
	if !quit {
		e.log.Warn("fix: the fixer could not be parked for a fresh session; it goes on in its conversation",
			"subject", job.subject(), "role", job.role.Name, "why", why)
		return nil
	}
	if _, err := e.ensureAgent(ctx, job.pr, job.role, ws, true, effortFull); err != nil {
		return fmt.Errorf("start %s fresh: %w", job.role.Name, err)
	}
	job.state.Context = "fresh session (" + words + ")"
	data["why"] = why
	e.event(ctx, "info", job.subject(), "fix.fresh_session", fmt.Sprintf("%s starts cycle %d in a fresh session: %s",
		job.role.Name, job.state.Cycle, why), data)
	return nil
}

// warnContextUnknown records fix.context_unknown, a warning, the first time
// the PR's fixer pane shows no context figure (kvPRFixContextUnknown),
// naming the wait when the figure was awaited after a resume (awaited);
// later cycles say nothing more until a figure is read (readContext). The
// fixer goes on in its conversation either way.
func (e *Engine) warnContextUnknown(ctx context.Context, job *fixJob, kind string, at int, awaited bool) {
	key := kvPRFixContextUnknown(job.pr.ID)
	if _, warned := e.getKV(ctx, key); warned {
		return
	}
	within := ""
	if awaited {
		within = fmt.Sprintf(" within %ds of its resume", int(fixContextAwait/time.Second))
	}
	e.setKV(ctx, key, store.FormatTime(e.now()))
	e.event(ctx, "warn", job.subject(), "fix.context_unknown", fmt.Sprintf("%s's pane shows no context figure that [kinds.%s] context_pattern reads%s, "+
		"so magnum cannot tell how full its conversation is and resumes it as it is (fix_compact_at %d; said once for this PR)",
		job.role.Name, kind, within, at), map[string]any{"role": job.role.Name, "kind": kind, "at": at})
}
