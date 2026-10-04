package engine

// Visible wait reasons: every PR waiting for a round (queued,
// rereview_pending) carries why and until when in one place, KVPRWait,
// which the engine rewrites after each dispatch. The screens read it: the
// dashboard's queue and the board show the compact form (Wait.Short), the
// card and `magnum status <ref>` the sentence with the override hint
// (Wait.Sentence). Before this a PR could sit for hours behind the daily
// cap with no word of it anywhere.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/store"
)

// KVPRAttention holds why the engine parked a PR in needs_attention (the
// kind attention.Explain takes: failed, blocked, identity_error, …); read
// only while the PR is in that state.
func KVPRAttention(prID int64) string { return fmt.Sprintf("pr.%d.attention", prID) }

// KVPRWait holds why a waiting PR has no round yet (Wait as JSON).
func KVPRWait(prID int64) string { return fmt.Sprintf("pr.%d.wait", prID) }

// Wait reasons (Wait.Reason).
const (
	WaitQuiet         = "quiet"          // the push quiet period
	WaitBurst         = "burst"          // the longer quiet period after a burst of pushes
	WaitInterval      = "interval"       // the minimum re-review interval since the last round
	WaitDraftInterval = "draft_interval" // the same for a draft
	WaitCap           = "cap"            // the daily round cap
	WaitDelta         = "delta"          // the re-review threshold: a small delta waits for more
	WaitRequested     = "requested"      // a review request (or ready for review): the debounce, then the next dispatch
	WaitRetry         = "retry"          // the backoff after a failed round
	WaitMuted         = "muted"          // magnum mute
	WaitQuietHours    = "quiet_hours"    // [daemon] quiet_hours
	WaitPaused        = "paused"         // the daemon is paused or draining
	WaitInfra         = "infra"          // an infrastructure failure paused dispatch
	WaitHerdr         = "herdr"          // herdr is unreachable
	WaitKind          = "kind"           // an agent kind the round needs is paused
	WaitBudget        = "budget"         // the Codex budget's soft cap holds first reviews
	WaitIdentity      = "identity"       // the posting identity is unhealthy
	WaitSlot          = "slot"           // the PR's slot, or a free pool slot
	WaitCapacity      = "capacity"       // max_concurrent_reviews or max_total_working_codex
	WaitOther         = "other"          // any other dispatch gate (details, a human, a paused watch, ...)
	WaitNext          = "next"           // nothing holds it: the next dispatch starts it
)

// Wait is why a PR waiting for a round has none yet (KVPRWait).
type Wait struct {
	Reason   string    `json:"reason"`
	Rereview bool      `json:"rereview"`
	Forced   bool      `json:"forced,omitempty"`
	Until    time.Time `json:"until,omitzero"`    // when it ends, when known
	Count    int       `json:"count,omitempty"`   // WaitCap: automatic rounds today; WaitDelta: changed lines
	Max      int       `json:"max,omitempty"`     // WaitCap: the cap; WaitDelta: rereview_min_lines
	Subject  string    `json:"subject,omitempty"` // WaitKind: the paused kind; WaitRequested: Request.Phrase
	// Detail is the reason in words, without the round's kind or the time
	// ("the push quiet period (5m)").
	Detail string `json:"detail"`
}

// ParseWait reads a KVPRWait value; ok is false for "" or a value it cannot
// read.
func ParseWait(s string) (Wait, bool) {
	var w Wait
	if s == "" || json.Unmarshal([]byte(s), &w) != nil || w.Reason == "" {
		return Wait{}, false
	}
	return w, true
}

func (w Wait) kind() string {
	if w.Rereview {
		return "re-review"
	}
	return "review"
}

// waitClock is t as a screen shows it relative to now: "14:09" today or
// within the next 24 hours (the cap's midnight is "00:00"), "Tue 14:09"
// within a week, else "Jan 2 14:09".
func waitClock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	ahead := t.Sub(now)
	switch y1, m1, d1 := t.Date(); {
	case y1 == now.Year() && m1 == now.Month() && d1 == now.Day(), ahead > 0 && ahead < 24*time.Hour:
		return t.Format("15:04")
	case ahead > 0 && ahead < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2 15:04")
}

// Short is the compact form for a table cell, e.g. "re-review · cap 6/6 →
// 00:00", "re-review · quiet → 14:09", "review · codex paused → 15:00",
// "re-review · small delta 8/30 lines → 16:40", "re-review · requested by
// alice → now".
func (w Wait) Short(now time.Time) string {
	var what string
	switch w.Reason {
	case WaitRequested:
		s := w.kind() + " · " + cmp.Or(w.Subject, "review requested")
		if w.Until.After(now) {
			return s + " → " + waitClock(w.Until, now)
		}
		return s + " → now"
	case WaitDelta:
		what = fmt.Sprintf("small delta %d/%d lines", w.Count, w.Max)
	case WaitQuiet:
		what = "quiet"
	case WaitBurst:
		what = "burst quiet"
	case WaitInterval:
		what = "interval"
	case WaitDraftInterval:
		what = "draft interval"
	case WaitCap:
		what = fmt.Sprintf("cap %d/%d", w.Count, w.Max)
	case WaitRetry:
		what = "retry"
	case WaitMuted:
		what = "muted"
	case WaitQuietHours:
		what = "quiet hours"
	case WaitPaused:
		what = "paused"
	case WaitInfra:
		what = "infra pause"
	case WaitHerdr:
		what = "herdr down"
	case WaitKind:
		what = strings.TrimSpace(w.Subject + " paused")
	case WaitBudget:
		what = "codex budget"
	case WaitIdentity:
		what = "identity"
	case WaitSlot:
		what = "slot"
	case WaitCapacity:
		what = "capacity"
	case WaitNext:
		what = "next tick"
	default:
		what = "waiting"
	}
	s := w.kind() + " · " + what
	if !w.Until.IsZero() {
		s += " → " + waitClock(w.Until, now)
	}
	return s
}

// overridable reports whether `magnum review` starts the round despite the
// reason (it forces a round past the timing rules, quiet hours and the soft
// budget cap, but not past pauses, the identity or capacity).
func (w Wait) overridable() bool {
	switch w.Reason {
	case WaitQuiet, WaitBurst, WaitInterval, WaitDraftInterval, WaitCap, WaitDelta, WaitRequested, WaitRetry,
		WaitQuietHours, WaitBudget, WaitNext:
		return true
	}
	return false
}

// Sentence is the full account for a card or `magnum status <ref>`, with
// the hint that lifts it: "re-review waits for the daily round cap (6 of 6
// today) until 00:00; `magnum review talkable#729` runs it now". ref is how
// the PR is named on the command line.
func (w Wait) Sentence(ref string, now time.Time) string {
	s := w.kind()
	if w.Forced {
		s = "forced " + s
	}
	if w.Reason == WaitRequested {
		s += " " + cmp.Or(w.Subject, "review requested")
	}
	requestedNow := w.Reason == WaitRequested && !w.Until.After(now)
	switch {
	case w.Reason == WaitNext, requestedNow:
		s += " starts at the next dispatch"
	case w.fromGate():
		s += " waits: " + w.Detail // the dispatcher's own words, with any time in them
	default:
		s += " waits for " + w.Detail
		if !w.Until.IsZero() {
			s += " until " + waitClock(w.Until, now)
		}
	}
	switch {
	case w.Forced || w.Reason == WaitNext || requestedNow:
	case w.overridable() && ref != "":
		s += "; `magnum review " + ref + "` runs it now"
	case w.Reason == WaitMuted && ref != "":
		s += "; `magnum unmute " + ref + "` brings it back"
	case w.Reason == WaitPaused || w.Reason == WaitInfra || w.Reason == WaitKind:
		s += "; `magnum resume` lifts the pause"
	}
	return s
}

// noteWaits records, after the dispatch of a tick, why every PR still
// waiting for a round waits (KVPRWait, rewritten only when it changed).
func (e *Engine) noteWaits(ctx context.Context, ts tickState) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRQueued, store.PRRereviewPending}})
	if err != nil {
		e.log.Warn("wait reasons", "err", err)
		return
	}
	now := e.now()
	global := e.globalWait(ctx, ts, now)
	for _, pr := range prs {
		b, err := json.Marshal(e.waitFor(ctx, pr, global, now))
		if err != nil {
			continue
		}
		if prev, _ := e.getKV(ctx, KVPRWait(pr.ID)); prev != string(b) {
			e.setKV(ctx, KVPRWait(pr.ID), string(b))
		}
	}
}

// globalWait is what holds every PR this tick (nil = nothing): the daemon
// paused or draining, an infrastructure pause, herdr unreachable.
func (e *Engine) globalWait(ctx context.Context, ts tickState, now time.Time) *Wait {
	if p, ok := e.infraPause(ctx); ok && now.Before(p.Until) {
		return &Wait{Reason: WaitInfra, Until: p.Until, Detail: "an infrastructure pause (" + p.Reason + "); the probe runs"}
	}
	if why := e.pauseReason(ctx); why != "" {
		return &Wait{Reason: WaitPaused, Detail: "the " + why}
	}
	if !ts.herdrUp {
		return &Wait{Reason: WaitHerdr, Detail: "herdr, which is unreachable"}
	}
	return nil
}

// waitFor is why pr waits: its retry backoff, then the timing rules
// (eligibility.Throttle, unless forced), then a mute, quiet hours, what
// holds every PR (global), and finally the reason the last dispatch gave
// (store.KVPRGate); with none of them it starts at the next dispatch.
func (e *Engine) waitFor(ctx context.Context, pr store.PR, global *Wait, now time.Time) Wait {
	base := Wait{Rereview: deref(pr.ReviewedSHA) != "", Forced: pr.Forced}
	with := func(w Wait) Wait { w.Rereview, w.Forced = base.Rereview, base.Forced; return w }
	if pr.NextAttemptAt != nil && pr.NextAttemptAt.After(now) {
		return with(Wait{Reason: WaitRetry, Until: *pr.NextAttemptAt, Detail: fmt.Sprintf("a retry (attempt %d) after: %s", pr.Attempts+1, clipRunes(strings.Join(strings.Fields(deref(pr.LastError)), " "), 120))})
	}
	repo, _ := e.st.RepoByID(ctx, pr.RepoID)
	w := e.cfg.WatchFor(repo.FullName())
	if !pr.Forced && w != nil {
		f := e.throttleFacts(ctx, pr, e.factsFor(pr, *w, now))
		if td := eligibility.Throttle(e.cfg.ThrottleFor(w), f, now.Local()); !td.Ready {
			return with(e.throttleWait(ctx, pr, *w, f, td, now))
		}
	}
	if pr.Muted && !pr.Forced {
		return with(Wait{Reason: WaitMuted, Detail: "nothing: the PR is muted"})
	}
	if spec := e.cfg.Daemon.QuietHours; !pr.Forced && quietHoursNow(spec, now) {
		return with(Wait{Reason: WaitQuietHours, Until: quietHoursEnd(spec, now), Detail: "the end of quiet hours (" + spec + ")"})
	}
	if global != nil {
		return with(*global)
	}
	if gate, ok := e.getKV(ctx, kvPRGate(pr.ID)); ok && gate != "" {
		return with(e.gateWait(ctx, gate))
	}
	if req, ok := e.pendingRequest(ctx, pr); ok && !pr.Forced {
		return with(Wait{Reason: WaitRequested, Subject: req.Phrase(), Detail: "the next dispatch"})
	}
	return with(Wait{Reason: WaitNext, Detail: "the next dispatch"})
}

// throttleWait maps an eligibility.Throttle decision for the facts f onto a
// Wait.
func (e *Engine) throttleWait(ctx context.Context, pr store.PR, w config.Watch, f eligibility.PRFacts, td eligibility.ThrottleDecision, now time.Time) Wait {
	d := e.cfg.ThrottleFor(&w)
	reason, until := td.Reason, td.NextEligibleAt
	switch {
	case strings.HasPrefix(reason, eligibility.ReasonRequested):
		req, _ := e.pendingRequest(ctx, pr)
		return Wait{Reason: WaitRequested, Until: until, Subject: req.Phrase(),
			Detail: fmt.Sprintf("the request debounce (%s after the request or the last push)", humanDuration(d.RequestDebounce.Duration))}
	case strings.HasPrefix(reason, eligibility.ReasonSmallDelta):
		return Wait{Reason: WaitDelta, Until: until, Count: f.DeltaLines, Max: d.RereviewMinLines,
			Detail: fmt.Sprintf("a larger delta (%d of %d changed lines since the review) or %s after its first push",
				f.DeltaLines, d.RereviewMinLines, humanDuration(d.RereviewMaxWait.Duration))}
	case strings.HasPrefix(reason, "waiting for burst quiet period"):
		return Wait{Reason: WaitBurst, Until: until, Detail: fmt.Sprintf("the burst quiet period (%s after %d pushes within %s)",
			humanDuration(d.BurstQuietPeriod.Duration), d.BurstPushes, humanDuration(d.BurstWindow.Duration))}
	case strings.HasPrefix(reason, "waiting for push quiet period"):
		return Wait{Reason: WaitQuiet, Until: until, Detail: fmt.Sprintf("the push quiet period (%s)", humanDuration(d.PushQuietPeriod.Duration))}
	case strings.HasPrefix(reason, "waiting for draft re-review interval"):
		return Wait{Reason: WaitDraftInterval, Until: until, Detail: fmt.Sprintf("the draft re-review interval (%s since the last round)", humanDuration(d.DraftMinRereviewInterval.Duration))}
	case strings.HasPrefix(reason, "waiting for min re-review interval"):
		return Wait{Reason: WaitInterval, Until: until, Detail: fmt.Sprintf("the minimum re-review interval (%s since the last round)", humanDuration(d.MinRereviewInterval.Duration))}
	case strings.HasPrefix(reason, "daily round cap reached"):
		rounds := pr.RoundsToday
		if deref(pr.RoundsDay) != store.DayKey(now) {
			rounds = 0
		}
		return Wait{Reason: WaitCap, Until: until, Count: rounds, Max: d.MaxRoundsPerPRPerDay,
			Detail: fmt.Sprintf("the daily round cap (%d of %d automatic rounds today)", rounds, d.MaxRoundsPerPRPerDay)}
	}
	return Wait{Reason: WaitOther, Until: until, Detail: reason}
}

// fromGate reports whether the wait is the dispatcher's own reason
// (gateWait): Detail is its sentence, not a phrase after "waits for".
func (w Wait) fromGate() bool {
	switch w.Reason {
	case WaitKind, WaitBudget, WaitIdentity, WaitSlot, WaitCapacity, WaitOther:
		return true
	}
	return false
}

// gateWait classifies the reason the last dispatch gave for skipping the PR
// (store.KVPRGate; prGate, startRound and the capacity check write it).
func (e *Engine) gateWait(ctx context.Context, gate string) Wait {
	w := Wait{Reason: WaitOther, Detail: gate}
	switch {
	case strings.HasPrefix(gate, "identity "):
		w.Reason = WaitIdentity
	case strings.HasPrefix(gate, "Codex budget "):
		w.Reason = WaitBudget
	case strings.HasPrefix(gate, "working Codex agents at the limit"), strings.HasPrefix(gate, gateCapacity):
		w.Reason = WaitCapacity
	case strings.HasPrefix(gate, "slot "), strings.HasPrefix(gate, gateNoFreeSlot):
		w.Reason = WaitSlot
	default:
		// kindPauseReason: "<kind> paused (<reason>) until 15:04".
		if kind, _, ok := strings.Cut(gate, " paused ("); ok && !strings.Contains(kind, " ") {
			if p, ok := e.toolPause(ctx, kind); ok {
				w.Reason, w.Subject, w.Until = WaitKind, kind, p.Until
			}
		}
	}
	return w
}

// Gates dispatch records besides the ones its checks return.
const (
	gateCapacity   = "review capacity: "    // max_concurrent_reviews is reached
	gateNoFreeSlot = "no free slot in the " // every slot of the pool is taken
)

// quietHoursNow reports whether now is inside [daemon] quiet_hours.
func quietHoursNow(spec string, now time.Time) bool {
	return spec != "" && eligibility.QuietHours(spec, now.Local())
}

// quietHoursEnd is the next local time quiet_hours ("HH:MM-HH:MM") end
// after now; zero when spec cannot be read.
func quietHoursEnd(spec string, now time.Time) time.Time {
	_, end, ok := strings.Cut(spec, "-")
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse("15:04", strings.TrimSpace(end))
	if err != nil {
		return time.Time{}
	}
	n := now.Local()
	at := time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), 0, 0, n.Location())
	if !at.After(n) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// humanDuration is d without zero units: "5m", "2h", "1h30m", "45s".
func humanDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
