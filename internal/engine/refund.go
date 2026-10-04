package engine

// Daily-cap refunds: rounds_today counts a round when it starts, so rounds
// that magnum itself cut short used up the cap (one PR reached its six
// rounds with one that failed to render a prompt and one stopped by a
// restart, then sat seven hours through six pushes). A round that ended
// because of magnum is refunded: rounds_today goes back down and
// last_round_started_at back to the round before, so neither the cap nor
// the re-review interval holds the PR for it.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// Refund reasons (the round.refunded event's data).
const (
	RefundStopped     = "stopped"      // a shutdown, restart, drain or abort stopped the round
	RefundUnprompted  = "unprompted"   // the round failed before any agent was prompted
	RefundRender      = "render"       // a prompt did not render (a prompt edited for another build)
	RefundModelLimits = "model_limits" // every fallback model was limited too: the kind paused
	RefundInfra       = "infra"        // an infrastructure failure paused dispatch
)

// recordStart remembers that enterReviewing recorded job's round start
// (the start time and the start before it) and whether it counted the round
// against the daily cap on day, so refund can take both back.
func (job *roundJob) recordStart(day string, started time.Time, prev *time.Time, counted bool) {
	job.started, job.counted, job.countedDay, job.startedAt, job.prevStart = true, counted, day, started, prev
}

// refundReason says why a finished round must not count against the cap
// ("" = it counts): stopped rounds, errors before any agent was prompted or
// from a prompt that did not render, and a judge whose model and every
// fallback were limited. Posted, needs-attention, blocked, timed-out and
// other failed rounds count.
func (e *Engine) refundReason(ctx context.Context, job *roundJob, res pipeline.RoundResult, outcome, msg string) string {
	switch outcome {
	case pipeline.OutcomeStopped:
		return RefundStopped
	case pipeline.OutcomeError:
		if strings.Contains(msg, "agents: render ") {
			return RefundRender
		}
		if !e.roundPrompted(ctx, job.pr.ID, res.Round) {
			return RefundUnprompted
		}
	case pipeline.OutcomeUsageLimit:
		if res.Pause != nil && e.modelLimited(res.Pause.Tool, res.Pause.Detail) {
			return RefundModelLimits
		}
	}
	return ""
}

// roundPrompted reports whether any run of round sent its agent a prompt
// (or its shell a command). A store failure answers yes: the round counts.
func (e *Engine) roundPrompted(ctx context.Context, prID int64, round int) bool {
	if round <= 0 {
		return false
	}
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		return true
	}
	for _, r := range runs {
		if r.Round == round && r.SubmittedAt != nil {
			return true
		}
	}
	return false
}

// modelLimited reports whether a pause's pane line is a per-model limit of
// kind (its model_limit health pattern): the pipeline reports one that no
// fallback model took over as a usage limit.
func (e *Engine) modelLimited(kind, detail string) bool {
	k, ok := e.cfg.KindSpec(kind)
	if !ok || detail == "" {
		return false
	}
	rx, err := k.HealthPatterns.Compile()
	if err != nil {
		return false
	}
	return agents.ClassifyWith(rx, detail, e.now()).Kind == agents.HealthModelLimit
}

// refund takes back job's round from the daily cap (only a round
// enterReviewing counted, on the day it counted it) and the re-review
// interval, once, and records round.refunded. A requested round taken back
// leaves its request pending (pendingRequest compares the round start).
func (e *Engine) refund(ctx context.Context, job *roundJob, round int, reason, outcome string) {
	if !job.started || e.d.DryRun {
		return
	}
	counted := job.counted
	job.started, job.counted = false, false
	cur, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("refund round", "pr", job.pr.ID, "err", err)
		return
	}
	decrement := counted && deref(cur.RoundsDay) == job.countedDay && cur.RoundsToday > 0
	restore := cur.LastRoundStartedAt != nil && cur.LastRoundStartedAt.Equal(job.startedAt)
	if !decrement && !restore {
		return
	}
	left := cur.RoundsToday
	err = e.st.UpdatePR(ctx, cur.ID, func(u *store.PRUpdate) {
		if decrement {
			left--
			u.Where("rounds_today", cur.RoundsToday)
			u.Set("rounds_today", left)
		}
		if restore {
			if job.prevStart != nil {
				u.Set("last_round_started_at", *job.prevStart)
			} else {
				u.Set("last_round_started_at", nil)
			}
		}
	})
	if err != nil {
		e.log.Warn("refund round", "pr", job.pr.ID, "err", err)
		return
	}
	msg := fmt.Sprintf("round %d does not count against the daily cap (%s): %s", round, refundWhy(reason), outcome)
	switch limit := e.cfg.Daemon.MaxRoundsPerPRPerDay; {
	case !counted:
		msg = fmt.Sprintf("round %d is taken back (%s): %s; it was not counted against the daily cap", round, refundWhy(reason), outcome)
	case limit > 0:
		msg += fmt.Sprintf("; %d of %d rounds today", left, limit)
	}
	e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "round.refunded", msg,
		map[string]any{"round": round, "reason": reason, "outcome": outcome, "rounds_today": left, "counted": counted})
}

// refundWhy is a refund reason in words.
func refundWhy(reason string) string {
	switch reason {
	case RefundStopped:
		return "stopped by a shutdown, restart, drain or abort"
	case RefundUnprompted:
		return "it failed before any agent was prompted"
	case RefundRender:
		return "a prompt did not render with this build"
	case RefundModelLimits:
		return "every fallback model was limited too"
	case RefundInfra:
		return "an infrastructure failure"
	}
	return reason
}
