package eligibility

import (
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// ThrottleDecision is Throttle's verdict.
type ThrottleDecision struct {
	Ready bool
	// NextEligibleAt is the earliest time every timing rule is satisfied. When
	// Ready it equals the now that was passed in, never the zero time.
	NextEligibleAt time.Time
	// Reason names the rule that is holding the PR back (the one that clears
	// last); empty when Ready.
	Reason string
	// Rule is that rule as a code, for callers that tell the rules apart
	// (the engine's wait reasons); empty when Ready.
	Rule Rule
}

// Rule is a timing rule of Throttle (ThrottleDecision.Rule).
type Rule string

// The rules Throttle applies.
const (
	RuleRequested     Rule = "requested"      // a requested round's debounce
	RuleQuiet         Rule = "quiet"          // the push quiet period
	RuleBurst         Rule = "burst"          // the longer quiet period after a burst of pushes
	RuleInterval      Rule = "interval"       // the minimum re-review interval since the last round
	RuleDraftInterval Rule = "draft_interval" // the same for a draft
	RuleCap           Rule = "cap"            // the daily round cap
	RuleSmallDelta    Rule = "small_delta"    // the re-review threshold
)

// Throttle decides whether a PR that is already eligible may start a review
// round at now. It only looks at timing: Muted, filters and quiet hours are
// other decisions. HeadChangedAt, PendingSince and LastRoundStartedAt are
// compared against now; the daily cap day is the calendar day of now in its own
// location, so pass a local time.
//
//   - First review (ReviewedSHA == ""): ready once the head has been quiet for
//     PushQuietPeriod, i.e. HeadChangedAt + PushQuietPeriod <= now. The
//     re-review interval and the daily cap do not apply.
//   - Burst: when at least BurstPushes of PushTimes fall within BurstWindow
//     of the latest one, the quiet period is BurstQuietPeriod instead (when
//     longer). A zero BurstPushes, BurstWindow or BurstQuietPeriod turns
//     the rule off.
//   - Re-review: the same quiet period (counted from the later of HeadChangedAt
//     and PendingSince), and LastRoundStartedAt + MinRereviewInterval
//     (DraftMinRereviewInterval for drafts; zero = MinRereviewInterval) <= now,
//     and RoundsToday < MaxRoundsPerPRPerDay. A cap of zero or less means no
//     cap; a daily cap that is reached holds the PR until the next local
//     midnight.
//   - Small delta (re-review only): while the unreviewed delta is known
//     (DeltaKnown), adds no file and has fewer than RereviewMinLines changed
//     lines, the PR waits until DeltaSince + RereviewMaxWait; a later push
//     that reaches the threshold is measured again by the caller. A zero
//     RereviewMinLines turns the rule off, and so does a delta that gets a
//     delta check (DeltaCheck): it is cheap, so it does not wait.
//   - Requested (RequestedAt set): every rule above is skipped; the PR waits
//     only RequestDebounce after the later of RequestedAt and HeadChangedAt.
//   - Forced bypasses all of it: always ready.
//
// A zero timestamp or zero duration never blocks. When several rules hold the
// PR back, NextEligibleAt is the latest of them and Reason names that one
// (ties go to quiet period, then interval, then cap, then small delta).
func Throttle(d config.Daemon, f PRFacts, now time.Time) ThrottleDecision {
	if f.Forced {
		return ThrottleDecision{Ready: true, NextEligibleAt: now}
	}
	rereview := f.ReviewedSHA != ""

	var holdUntil time.Time
	var reason string
	var rule Rule
	hold := func(until time.Time, r Rule, why string) {
		if until.After(now) && until.After(holdUntil) {
			holdUntil, rule, reason = until, r, why
		}
	}
	decide := func() ThrottleDecision {
		if holdUntil.IsZero() {
			return ThrottleDecision{Ready: true, NextEligibleAt: now}
		}
		return ThrottleDecision{NextEligibleAt: holdUntil, Reason: reason, Rule: rule}
	}

	if !f.RequestedAt.IsZero() {
		anchor := f.RequestedAt
		if f.HeadChangedAt.After(anchor) {
			anchor = f.HeadChangedAt
		}
		if debounce := d.RequestDebounce.Duration; debounce > 0 {
			hold(anchor.Add(debounce), RuleRequested, fmt.Sprintf("%s (%s)", ReasonRequested, debounce))
		}
		return decide()
	}

	quiet, quietRule, why := d.PushQuietPeriod.Duration, RuleQuiet, "push quiet period"
	if n := burst(d, f.PushTimes); n > 0 && d.BurstQuietPeriod.Duration > quiet {
		quiet, quietRule = d.BurstQuietPeriod.Duration, RuleBurst
		why = fmt.Sprintf("burst quiet period after %d pushes within %s", n, d.BurstWindow.Duration)
	}
	anchor := f.HeadChangedAt
	if rereview && f.PendingSince.After(anchor) {
		anchor = f.PendingSince
	}
	if quiet > 0 && !anchor.IsZero() {
		hold(anchor.Add(quiet), quietRule, fmt.Sprintf("waiting for %s (%s)", why, quiet))
	}

	if rereview {
		interval, intervalRule, name := d.MinRereviewInterval.Duration, RuleInterval, "min re-review interval"
		if f.IsDraft && d.DraftMinRereviewInterval.Duration > 0 {
			interval, intervalRule, name = d.DraftMinRereviewInterval.Duration, RuleDraftInterval, "draft re-review interval"
		}
		if interval > 0 && !f.LastRoundStartedAt.IsZero() {
			hold(f.LastRoundStartedAt.Add(interval), intervalRule, fmt.Sprintf("waiting for %s (%s)", name, interval))
		}
		if limit := d.MaxRoundsPerPRPerDay; limit > 0 && f.RoundsToday >= limit {
			hold(nextMidnight(now), RuleCap, fmt.Sprintf("daily round cap reached (%d per day)", limit))
		}
		if smallDelta(d, f) && !DeltaCheck(d, f) && !f.DeltaSince.IsZero() {
			hold(f.DeltaSince.Add(d.RereviewMaxWait.Duration), RuleSmallDelta, fmt.Sprintf("%s %d/%d lines since the review (at most %s)",
				ReasonSmallDelta, f.DeltaLines, d.RereviewMinLines, d.RereviewMaxWait.Duration))
		}
	}
	return decide()
}

// Reason prefixes of two rules (ThrottleDecision.Rule is the code to tell
// the rules apart by).
const (
	// ReasonRequested starts the reason of a requested round's debounce.
	ReasonRequested = "review requested"
	// ReasonSmallDelta starts the reason of the re-review threshold.
	ReasonSmallDelta = "small delta"
)

// smallDelta reports whether a measured delta stays under the re-review
// threshold: known, no added file, fewer changed lines than
// RereviewMinLines (0 = no threshold).
func smallDelta(d config.Daemon, f PRFacts) bool {
	return d.RereviewMinLines > 0 && f.DeltaKnown && f.DeltaAddedFiles == 0 && f.DeltaLines < d.RereviewMinLines
}

// DeltaCheck reports whether a re-review's delta gets a delta check, a
// judge-only round on the commits since the review, instead of a full round
// after the threshold's wait: DeltaCheck (delta_check) and the threshold
// (RereviewMinLines) are on, and the delta since the reviewed commit is
// readable (DeltaReadable), adds no file and has more than 0 and fewer than
// RereviewMinLines changed code lines. Whether the round is forced or
// requested (then it runs in full) is the caller's business.
func DeltaCheck(d config.Daemon, f PRFacts) bool {
	return d.DeltaCheck && d.RereviewMinLines > 0 && f.ReviewedSHA != "" && f.DeltaReadable &&
		f.DeltaAddedFiles == 0 && f.DeltaLines > 0 && f.DeltaLines < d.RereviewMinLines
}

// burst counts the pushes within d.BurstWindow of the latest one (that one
// included) and returns the count when it reaches d.BurstPushes; 0 otherwise
// or when the rule is off.
func burst(d config.Daemon, pushes []time.Time) int {
	window := d.BurstWindow.Duration
	if d.BurstPushes <= 0 || window <= 0 || d.BurstQuietPeriod.Duration <= 0 || len(pushes) < d.BurstPushes {
		return 0
	}
	var last time.Time
	for _, p := range pushes {
		if p.After(last) {
			last = p
		}
	}
	n := 0
	for _, p := range pushes {
		if !p.IsZero() && last.Sub(p) <= window {
			n++
		}
	}
	if n < d.BurstPushes {
		return 0
	}
	return n
}

// nextMidnight is the start of the day after now, in now's location.
func nextMidnight(now time.Time) time.Time {
	y, m, day := now.Date()
	return time.Date(y, m, day+1, 0, 0, 0, 0, now.Location())
}
