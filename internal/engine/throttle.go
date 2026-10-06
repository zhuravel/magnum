package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRPushes holds the PR's recent head changes as the poller saw them (a
// JSON list of times, oldest first): the burst quiet period counts them.
func kvPRPushes(id int64) string { return fmt.Sprintf("pr.%d.pushes", id) }

// pushRing is how many head changes kvPRPushes keeps at least (more when a
// burst_pushes asks for more).
const pushRing = 16

// recordPush appends a head change of the PR at at to kvPRPushes.
func (e *Engine) recordPush(ctx context.Context, prID int64, at time.Time) {
	times := append(e.pushTimes(ctx, prID), at)
	keep := pushRing
	for _, w := range e.cfg.Watches {
		keep = max(keep, e.cfg.ThrottleFor(&w).BurstPushes)
	}
	if len(times) > keep {
		times = times[len(times)-keep:]
	}
	b, err := json.Marshal(times)
	if err != nil {
		return
	}
	e.setKV(ctx, kvPRPushes(prID), string(b))
}

// pushTimes reads kvPRPushes (nil when absent or unreadable).
func (e *Engine) pushTimes(ctx context.Context, prID int64) []time.Time {
	v, ok := e.getKV(ctx, kvPRPushes(prID))
	if !ok || v == "" {
		return nil
	}
	var out []time.Time
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil
	}
	return out
}

// throttle is eligibility.Throttle for pr under its watch's settings
// (config.Config.ThrottleFor: the burst quiet period, the re-review delta
// threshold) with throttleFacts.
func (e *Engine) throttle(ctx context.Context, w config.Watch, pr store.PR, f eligibility.PRFacts, now time.Time) eligibility.ThrottleDecision {
	return eligibility.Throttle(e.cfg.ThrottleFor(&w), e.throttleFacts(ctx, pr, f), now.Local())
}

// throttleFacts completes f with what the registry's kv holds for pr: its
// recent pushes, its snooze (snoozeOf), a pending review request
// (pendingRequest; none for a forced PR) and the measured delta since the
// reviewed commit (deltaFacts).
// A head that arrived during the PR's last review (f.PendingSince not after
// reviewed_at) is not held by the re-review interval: that review already
// covers an older head (rereviewAt, arrivedDuringReview).
func (e *Engine) throttleFacts(ctx context.Context, pr store.PR, f eligibility.PRFacts) eligibility.PRFacts {
	f.PushTimes = e.pushTimes(ctx, pr.ID)
	if s, ok := e.snoozeRecord(ctx, pr.ID); ok {
		f.SnoozedUntil = s.Until // Throttle reads one that has ended as none
	}
	if pr.ReviewedAt != nil && !f.PendingSince.IsZero() && !f.PendingSince.After(*pr.ReviewedAt) {
		f.LastRoundStartedAt = time.Time{}
	}
	if req, ok := e.pendingRequest(ctx, pr); ok && !f.Forced {
		f.RequestedAt = req.At
	} else if at, _, ok := e.replyTrigger(ctx, pr); ok && !f.Forced {
		f.RepliedAt, f.ReplyRoundAt = at, e.lastReplyRound(ctx, pr).At
	}
	e.deltaFacts(ctx, pr, &f)
	return f
}
