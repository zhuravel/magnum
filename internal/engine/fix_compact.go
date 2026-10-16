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
// starts it fresh: the fix prompt carries the findings anyway. A pane that
// shows no figure leaves the fixer as it is and warns once per PR. A fixer
// started fresh is never compacted.

import (
	"context"
	"fmt"
	"maps"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRFixContextUnknown records when the PR's fixer pane first showed no
// context figure although fix_compact_at is on (store.FormatTime): the
// fix.context_unknown warning is said once per PR.
func kvPRFixContextUnknown(prID int64) string { return fmt.Sprintf("pr.%d.fix_context_unknown", prID) }

// fixCompact runs before a fix cycle prompts a fixer it resumed (its
// session was live, or ensureAgent resumed a parked one), when [pipeline]
// fix_compact_at is on and the fixer's kind has a context_pattern: it reads
// the fixer's context (agents.Manager.ContextUsed) and leaves a figure under
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
func (e *Engine) fixCompact(ctx context.Context, job *fixJob, ws agents.Workspace) error {
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
	from, ok := e.d.Agents.ContextUsed(ctx, s)
	if !ok { // a figure missing (right after a resume, say) costs no fresh fixer
		e.warnContextUnknown(ctx, job, kind, at)
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
		e.warnContextUnknown(ctx, job, kind, at)
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
// the PR's fixer pane shows no context figure (kvPRFixContextUnknown); later
// cycles say nothing more. The fixer goes on in its conversation either way.
func (e *Engine) warnContextUnknown(ctx context.Context, job *fixJob, kind string, at int) {
	key := kvPRFixContextUnknown(job.pr.ID)
	if _, warned := e.getKV(ctx, key); warned {
		return
	}
	e.setKV(ctx, key, store.FormatTime(e.now()))
	e.event(ctx, "warn", job.subject(), "fix.context_unknown", fmt.Sprintf("%s's pane shows no context figure that [kinds.%s] context_pattern reads, "+
		"so magnum cannot tell how full its conversation is and resumes it as it is (fix_compact_at %d; said once for this PR)",
		job.role.Name, kind, at), map[string]any{"role": job.role.Name, "kind": kind, "at": at})
}
