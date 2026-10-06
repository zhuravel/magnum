package eligibility

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

const reasonOwn = "waiting for own PR interval (2h0m0s)"

// withOwn is daemon() (a 30m re-review interval, 2h for drafts) with the
// operator's own PRs re-reviewed at most every own (0 = the normal
// interval).
func withOwn(own time.Duration) config.Daemon {
	d := daemon()
	d.OwnMinRereviewInterval = dur(own)
	return d
}

// The operator's own PR waits own_min_rereview_interval after its last
// round instead of min_rereview_interval; someone else's PR keeps the
// normal interval.
func TestThrottleOwnPRWaitsTheOwnInterval(t *testing.T) {
	d := withOwn(2 * time.Hour)
	started := ago(time.Hour)
	f := PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: started, Own: true}
	got := Throttle(d, f, now)
	wantDecision(t, got, false, started.Add(2*time.Hour), reasonOwn)
	if got.Rule != RuleOwnInterval {
		t.Fatalf("rule = %q, want %q", got.Rule, RuleOwnInterval)
	}
	f.Own = false
	wantDecision(t, Throttle(d, f, now), true, now, "")

	f = PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(10 * time.Minute)}
	got = Throttle(d, f, now)
	wantDecision(t, got, false, now.Add(20*time.Minute), reasonInterval)
	if got.Rule != RuleInterval {
		t.Fatalf("someone else's PR: rule = %q, want %q", got.Rule, RuleInterval)
	}
}

// own_min_rereview_interval = "0" keeps the normal interval for the
// operator's own PRs too, and a first review never waits for it.
func TestThrottleOwnIntervalZeroIsTheNormalOne(t *testing.T) {
	d := withOwn(0)
	f := PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(10 * time.Minute), Own: true}
	got := Throttle(d, f, now)
	wantDecision(t, got, false, now.Add(20*time.Minute), reasonInterval)

	first := PRFacts{HeadChangedAt: ago(10 * time.Minute), LastRoundStartedAt: ago(time.Minute), Own: true}
	wantDecision(t, Throttle(withOwn(2*time.Hour), first, now), true, now, "")
}

// An own draft waits the longer of the draft and the own intervals.
func TestThrottleOwnDraftWaitsTheLongerInterval(t *testing.T) {
	started := ago(time.Minute)
	f := PRFacts{ReviewedSHA: reviewed, IsDraft: true, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: started, Own: true}

	got := Throttle(withOwn(3*time.Hour), f, now) // own 3h > draft 2h
	wantDecision(t, got, false, started.Add(3*time.Hour), "waiting for own PR interval (3h0m0s)")
	if got.Rule != RuleOwnInterval {
		t.Fatalf("own longer: rule = %q", got.Rule)
	}
	got = Throttle(withOwn(time.Hour), f, now) // draft 2h > own 1h
	wantDecision(t, got, false, started.Add(2*time.Hour), reasonDraft)
	if got.Rule != RuleDraftInterval {
		t.Fatalf("draft longer: rule = %q", got.Rule)
	}
}

// A review request and a forced round skip the own interval, as they skip
// the normal one.
func TestThrottleOwnIntervalYieldsToRequestsAndForcedRounds(t *testing.T) {
	d := withOwn(2 * time.Hour)
	d.RequestDebounce = dur(time.Minute)
	f := PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Hour), LastRoundStartedAt: ago(10 * time.Minute), Own: true,
		RequestedAt: ago(2 * time.Minute)}
	wantDecision(t, Throttle(d, f, now), true, now, "")
	f.RequestedAt = time.Time{}
	f.Forced = true
	wantDecision(t, Throttle(d, f, now), true, now, "")
}

// A snooze holds every automatic round until it ends: a first review, a
// re-review and a reply round; the latest rule still wins when it ends
// later.
func TestThrottleSnoozeHoldsAutomaticRounds(t *testing.T) {
	d := replyDaemon()
	until := now.Add(3 * time.Hour)

	first := PRFacts{HeadChangedAt: ago(time.Hour), SnoozedUntil: until}
	got := Throttle(d, first, now)
	wantDecision(t, got, false, until, ReasonSnoozed)
	if got.Rule != RuleSnoozed {
		t.Fatalf("rule = %q, want %q", got.Rule, RuleSnoozed)
	}

	re := reviewedFacts()
	re.SnoozedUntil = until
	wantDecision(t, Throttle(d, re, now), false, until, ReasonSnoozed)

	replies := reviewedFacts()
	replies.RepliedAt = ago(10 * time.Minute)
	replies.SnoozedUntil = until
	wantDecision(t, Throttle(d, replies, now), false, until, ReasonSnoozed)

	short := reviewedFacts()
	short.LastRoundStartedAt = ago(time.Minute)
	short.SnoozedUntil = now.Add(time.Minute)
	got = Throttle(d, short, now)
	wantDecision(t, got, false, short.LastRoundStartedAt.Add(30*time.Minute), "waiting for min re-review interval")
}

// A snooze that has ended holds nothing; a review request and a forced
// round pass a snooze that has not.
func TestThrottleSnoozeEndsAndYieldsToRequests(t *testing.T) {
	d := replyDaemon()
	f := reviewedFacts()
	f.SnoozedUntil = ago(time.Second)
	wantDecision(t, Throttle(d, f, now), true, now, "")
	f.SnoozedUntil = now
	wantDecision(t, Throttle(d, f, now), true, now, "")

	f.SnoozedUntil = now.Add(time.Hour)
	f.RequestedAt = ago(2 * time.Minute)
	wantDecision(t, Throttle(d, f, now), true, now, "")
	f.RequestedAt = time.Time{}
	f.Forced = true
	wantDecision(t, Throttle(d, f, now), true, now, "")
}
