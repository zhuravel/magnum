package engine

// Snooze: `magnum snooze <ref>` (or the board's z) holds every automatic
// round of one PR until a time: a push, the quiet period's end, a
// re-review, a reply round, a delta check. A round someone asks for still
// runs: `magnum review`, the board's review keys and a review request on
// GitHub. The snooze is the PR's KVPRSnooze record, so it survives a
// restart, and it ends on its own at its time (a record that has passed
// holds nothing and is never cleaned up). eligibility.Throttle holds the PR
// until then (PRFacts.SnoozedUntil), which sets next_eligible_at and the
// wait the screens show; dispatch checks the record once more
// (snoozeHolds), for a PR whose time was set before the snooze.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// ReqSnooze snoozes a PR or lifts its snooze (SnoozePayload).
const ReqSnooze = "snooze"

// SnoozePayload is a `magnum snooze` request: snooze the PR until Until, or
// lift its snooze (Off). By says who asks ("magnum snooze", "the board"),
// for the card and the events.
type SnoozePayload struct {
	PRTarget
	Until time.Time `json:"until,omitzero"`
	Off   bool      `json:"off,omitempty"`
	By    string    `json:"by,omitempty"`
}

// KVPRSnooze holds a PR's snooze (Snooze as JSON).
func KVPRSnooze(prID int64) string { return fmt.Sprintf("pr.%d.snooze", prID) }

// Snooze is a PR's snooze: until when no automatic round starts, when it
// was set and by whom ("magnum snooze", "the board").
type Snooze struct {
	Until time.Time `json:"until"`
	At    time.Time `json:"at"`
	By    string    `json:"by,omitempty"`
}

// ParseSnooze reads a KVPRSnooze value; ok is false for "" or a value it
// cannot read. Whether the snooze still holds is Active's business.
func ParseSnooze(s string) (Snooze, bool) {
	var z Snooze
	if s == "" || json.Unmarshal([]byte(s), &z) != nil || z.Until.IsZero() {
		return Snooze{}, false
	}
	return z, true
}

// Active reports whether the snooze holds at now: it ends at Until.
func (s Snooze) Active(now time.Time) bool { return s.Until.After(now) }

func (s Snooze) marshal() (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

// snoozeRecord is the PR's snooze record, whether or not it has ended.
func (e *Engine) snoozeRecord(ctx context.Context, prID int64) (Snooze, bool) {
	v, _ := e.getKV(ctx, KVPRSnooze(prID))
	return ParseSnooze(v)
}

// snoozeOf is the PR's snooze when it holds at now.
func (e *Engine) snoozeOf(ctx context.Context, prID int64, now time.Time) (Snooze, bool) {
	s, ok := e.snoozeRecord(ctx, prID)
	if !ok || !s.Active(now) {
		return Snooze{}, false
	}
	return s, true
}

// snoozeHolds reports whether a snooze holds pr's next round at now: one
// that has not ended, on a PR nobody forced and no review request waits
// for (both pass it).
func (e *Engine) snoozeHolds(ctx context.Context, pr store.PR, now time.Time) bool {
	if pr.Forced {
		return false
	}
	if _, ok := e.snoozeOf(ctx, pr.ID, now); !ok {
		return false
	}
	_, requested := e.pendingRequest(ctx, pr)
	return !requested
}

// requestSnooze snoozes an open PR until p.Until, or lifts its snooze
// (p.Off), and gives a PR waiting for a round its time again. A round in
// flight finishes.
func (e *Engine) requestSnooze(ctx context.Context, p SnoozePayload) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	subject := prSubject(repo, pr.Number)
	by := p.By
	if by == "" {
		by = "magnum snooze"
	}
	now := e.now()
	if p.Off {
		if _, ok := e.snoozeOf(ctx, pr.ID, now); !ok {
			e.delKV(ctx, KVPRSnooze(pr.ID)) // one that has ended goes too
			return label + " is not snoozed", nil
		}
		e.delKV(ctx, KVPRSnooze(pr.ID))
		e.retime(ctx, repo.FullName(), pr, "snooze lifted")
		e.event(ctx, "info", subject, "pr.unsnoozed", "snooze lifted by "+by, map[string]any{"by": by})
		return "lifted the snooze of " + label + ": automatic reviews run again", nil
	}
	if pr.GHState != store.GHOpen {
		return "", fmt.Errorf("%s is %s: nothing to snooze", label, strings.ToLower(pr.GHState))
	}
	if !p.Until.After(now) {
		return fmt.Sprintf("nothing to do: a snooze of %s until %s would have ended already", label, p.Until.Local().Format("Jan 2 15:04")), nil
	}
	s := Snooze{Until: p.Until.UTC(), At: now.UTC(), By: by}
	v, err := s.marshal()
	if err != nil {
		return "", err
	}
	e.setKV(ctx, KVPRSnooze(pr.ID), v)
	e.retime(ctx, repo.FullName(), pr, "snoozed")
	clock := waitClock(s.Until, now)
	e.event(ctx, "info", subject, "pr.snoozed", fmt.Sprintf("snoozed until %s by %s: no automatic round starts until then", clock, by),
		map[string]any{"until": s.Until, "by": by})
	return fmt.Sprintf("snoozed %s until %s: no automatic round starts until then; `magnum review` and review requests still run", label, clock), nil
}

// retime gives a PR waiting for a round (queued, rereview_pending, not
// forced) its next_eligible_at again after what holds it changed (why says
// what), as eligibility.Throttle decides it now.
func (e *Engine) retime(ctx context.Context, fullName string, pr store.PR, why string) {
	if pr.Forced || (pr.State != store.PRQueued && pr.State != store.PRRereviewPending) {
		return
	}
	w := e.cfg.WatchFor(fullName)
	if w == nil {
		return
	}
	now := e.now()
	td := e.throttle(ctx, *w, pr, e.factsFor(ctx, pr, *w, now), now)
	if pr.NextEligibleAt != nil && pr.NextEligibleAt.Equal(td.NextEligibleAt) {
		return
	}
	if err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) {
		u.Set("next_eligible_at", td.NextEligibleAt)
	}); err != nil {
		e.log.Info("snooze: next eligible time", "pr", pr.ID, "err", err)
		return
	}
	e.log.Debug("next eligible time", "pr", pr.ID, "why", why, "at", td.NextEligibleAt)
}
