package engine

// What the operator hears about the reviews they asked for and about what
// holds them. A round the operator asked for (a forced PR: `magnum review`,
// the board, the picker) toasts when it posts and when it fails, whatever
// [herdr] toast_every_review says (that switch is for automatic rounds), and
// once when it cannot start for a reason only the operator can lift. While
// `magnum pause` holds the review requests people made, the tab bar and
// `magnum status` say since when and how many, and each held request toasts
// once per pause. Every toast goes through the notifier, so [herdr] notify
// turns them off.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVDaemonPausedAt holds when the running `magnum pause` began
// (store.FormatTime); a pause renewed while it runs keeps it. KVDaemonPausedHeld
// is how many review requests the pause holds (pauseHeldRequest), rewritten by
// every tick while paused. Both go with the pause.
const (
	KVDaemonPausedAt   = "daemon.paused_at"
	KVDaemonPausedHeld = "daemon.paused_held"
)

const (
	// requestedToastWindow dedupes the toast of one requested round's end
	// (its key names the review, or the head and attempt that failed).
	requestedToastWindow = 24 * time.Hour
	// heldToastAfter is how long a requested round waits for a reason only
	// the operator can lift before it toasts; a drain counts once it has
	// lasted drainToastAfter.
	heldToastAfter  = 2 * time.Minute
	drainToastAfter = 15 * time.Minute
	// heldToastWindow dedupes a held toast across daemon restarts.
	heldToastWindow = 6 * time.Hour
	// pauseHeldWindow dedupes the toast of a request a pause holds (its key
	// names the pause by its start).
	pauseHeldWindow = 30 * 24 * time.Hour
)

// Kinds of the operator's toasts in a batch summary.
var (
	kindRequestedFailed = notify.Kind{One: "requested review failed", Many: "requested reviews failed"}
	kindRequestedHeld   = notify.Kind{One: "requested review held", Many: "requested reviews held"}
	kindRequestPaused   = notify.Kind{One: "review request paused", Many: "review requests paused"}
)

// heldWait is how long a forced PR has waited for one reason only the
// operator can lift (noteHeldRequested), and whether that was toasted.
type heldWait struct {
	reason  string
	since   time.Time
	toasted bool
}

// prWait is a waiting PR with the wait noteWaits recorded for it.
type prWait struct {
	pr   store.PR
	wait Wait
}

// noteOperatorWaits runs after noteWaits recorded every waiting PR's wait:
// the held toasts of requested rounds and the pause's held requests.
func (e *Engine) noteOperatorWaits(ctx context.Context, waits []prWait, now time.Time) {
	e.noteHeldRequested(ctx, waits, now)
	e.notePauseHeld(ctx, waits, now)
}

// toastRequestedPosted announces a review the operator asked for once it
// posted: its verdict, its findings by priority and how long the round
// took (since its start, a restart's wait included).
func (e *Engine) toastRequestedPosted(job *roundJob, pr store.PR, res pipeline.RoundResult, now time.Time) {
	label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
	title := fmt.Sprintf("%s: %s (%s)", label, cmp.Or(res.Event, res.Outcome), findingsText(res.Findings))
	body, line := "Your requested review", title
	if start := deref(pr.LastRoundStartedAt); !start.IsZero() && now.After(start) {
		took := tookText(now.Sub(start))
		body += " took " + took + ","
		line += " in " + took
	}
	body += " posted as " + e.reviewerLogin(pr.Identity) + "."
	e.info(notify.Item{Key: fmt.Sprintf("requested:%d:review:%d", pr.ID, res.ReviewID), Title: title, Body: body, Line: line,
		Kind: notify.KindReviewPosted, Window: requestedToastWindow})
}

// toastRequestedFailed tells the operator why the round they asked for
// failed (what: the outcome, or "setup failed") and when it retries. A PR
// the failure parked in needs_attention, or a post-merge round put back in
// closed, was toasted by needsAttention or postMergeFailed already.
func (e *Engine) toastRequestedFailed(ctx context.Context, job *roundJob, pr store.PR, what, why string) {
	if !pr.Forced {
		return
	}
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil || cur.State == store.PRNeedsAttention || cur.State == store.PRClosed {
		return
	}
	now := e.now()
	label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
	body := what
	if why = oneLine(why, 240); why != "" {
		body += ": " + why
	}
	if cur.NextAttemptAt != nil && cur.NextAttemptAt.After(now) {
		body += "; it retries at " + waitClock(*cur.NextAttemptAt, now)
	}
	e.info(notify.Item{Key: fmt.Sprintf("requested:%d:%s:%d:%s", pr.ID, textx.ShortSHA(cur.HeadSHA), cur.Attempts, what),
		Title: label + ": requested review failed", Body: body, Line: label + ": requested review failed (" + what + ")",
		Kind: kindRequestedFailed, Window: requestedToastWindow})
}

// requestedRoundFailed is the toast of a requested round whose outcome is a
// failure the PR retries (an error, a timeout, an overloaded API); the other
// outcomes toast on their own (a paused agent kind, needs_attention) or are
// not failures, and neither is a person typing in the PR's panes (runErr
// agents.ErrHumanActive: the round waits for them).
func (e *Engine) requestedRoundFailed(ctx context.Context, job *roundJob, pr store.PR, outcome, msg string, runErr error) {
	switch {
	case errors.Is(runErr, agents.ErrHumanActive):
	case outcome == pipeline.OutcomeError, outcome == pipeline.OutcomeTimeout, outcome == pipeline.OutcomeOverloaded:
		e.toastRequestedFailed(ctx, job, pr, outcome, msg)
	}
}

// noteHeldRequested toasts once, per PR and reason, when a requested round
// cannot start for a reason only the operator can lift (operatorHold) and has
// waited heldToastAfter for it; a drain counts once it has lasted
// drainToastAfter. A PR that starts, or stops waiting for such a reason, is
// forgotten.
func (e *Engine) noteHeldRequested(ctx context.Context, waits []prWait, now time.Time) {
	if e.held == nil {
		e.held = map[int64]heldWait{}
	}
	seen := map[int64]bool{}
	for _, pw := range waits {
		pr, w := pw.pr, pw.wait
		if !pr.Forced || !operatorHold(w) {
			continue
		}
		seen[pr.ID] = true
		h, ok := e.held[pr.ID]
		if !ok || h.reason != w.Reason {
			h = heldWait{reason: w.Reason, since: now}
		}
		due := now.Sub(h.since) >= heldToastAfter
		if w.Reason == WaitDraining {
			started, ok := e.kvTime(ctx, KVDaemonDraining)
			due = due && ok && now.Sub(started) >= drainToastAfter
		}
		if due && !h.toasted {
			h.toasted = true
			repo, err := e.st.RepoByID(ctx, pr.RepoID)
			if err == nil {
				label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
				body := w.Sentence(fmt.Sprintf("%s#%d", repo.FullName(), pr.Number), now)
				e.info(notify.Item{Key: fmt.Sprintf("held:%d:%s", pr.ID, w.Reason), Title: label + ": requested review can't start",
					Body: body, Line: label + ": " + body, Kind: kindRequestedHeld, Window: heldToastWindow})
			}
		}
		e.held[pr.ID] = h
	}
	for id := range e.held {
		if !seen[id] {
			delete(e.held, id)
		}
	}
}

// operatorHold reports whether w holds a requested round for a reason only
// the operator can lift: an agent kind its roles need is paused, the
// posting identity is unhealthy, a guard keeps its slot (a pin, a person's
// changes), or a drain for a restart.
func operatorHold(w Wait) bool {
	switch w.Reason {
	case WaitKind, WaitIdentity, WaitDraining:
		return true
	case WaitSlot: // slotGate: "slot X is pinned (…)", "slot X is held: <reason> (…)"
		return strings.Contains(w.Detail, " is pinned") || strings.Contains(w.Detail, " is held")
	}
	return false
}

// notePauseHeld records, while `magnum pause` runs, how many review
// requests it holds (KVDaemonPausedHeld: waiting PRs nobody forced whose
// review someone requested from the operator, pauseHeldRequest) and toasts
// each such request once per pause. Without a pause it clears the count.
func (e *Engine) notePauseHeld(ctx context.Context, waits []prWait, now time.Time) {
	if e.userPause(ctx) == "" {
		if _, ok := e.getKV(ctx, KVDaemonPausedHeld); ok {
			e.delKV(ctx, KVDaemonPausedHeld)
		}
		e.pauseToasted = nil
		return
	}
	since := e.pausedSince(ctx, now)
	if e.pauseToasted == nil {
		e.pauseToasted = map[string]bool{}
	}
	n := 0
	for _, pw := range waits {
		pr := pw.pr
		by, ok := e.pauseHeldRequest(ctx, pr)
		if !ok {
			continue
		}
		n++
		key := fmt.Sprintf("pause-held:%d:%d", pr.ID, since.Unix())
		if e.pauseToasted[key] {
			continue
		}
		e.pauseToasted[key] = true
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		paused := "magnum is paused (since " + pastClock(since, now) + ")"
		e.info(notify.Item{Key: key, Title: label + ": " + by + " asked for your review",
			Body: paused + "; `magnum review " + fmt.Sprintf("%s#%d", repo.FullName(), pr.Number) + "` runs it anyway, `magnum resume` lifts the pause.",
			Line: label + ": " + by + " asked for your review; " + paused, Kind: kindRequestPaused, Window: pauseHeldWindow})
	}
	if v, _ := e.getKV(ctx, KVDaemonPausedHeld); v != strconv.Itoa(n) {
		e.setKV(ctx, KVDaemonPausedHeld, strconv.Itoa(n))
	}
}

// pauseHeldRequest reports whether `magnum pause` holds a review someone
// requested from the operator on pr (a waiting PR): nobody forced it (a
// forced round passes the pause), it is not muted, and the operator is a
// requested reviewer on GitHub (review_requested) or the newest request
// magnum handled is still pending (a draft marked ready is no person's
// request). by is who asked ("someone" when unknown).
func (e *Engine) pauseHeldRequest(ctx context.Context, pr store.PR) (by string, ok bool) {
	if pr.Forced || pr.Muted {
		return "", false
	}
	if req, pending := e.pendingRequest(ctx, pr); pending && req.By != RequestReadyForReview {
		return cmp.Or(req.By, "someone"), true
	}
	if !pr.ReviewRequested {
		return "", false
	}
	if by, _ := e.getKV(ctx, KVPRRequestBy(pr.ID)); by != "" && by != RequestReadyForReview {
		return by, true
	}
	return "someone", true
}

// pausedSince is when the running `magnum pause` began (KVDaemonPausedAt).
// A pause an older build started has none: it takes the time of the newest
// daemon.paused event (now when there is none) and records it.
func (e *Engine) pausedSince(ctx context.Context, now time.Time) time.Time {
	if at, ok := e.kvTime(ctx, KVDaemonPausedAt); ok {
		return at
	}
	at := now
	if evs, err := e.st.EventsOfKindsSince(ctx, time.Time{}, "daemon.paused"); err == nil && len(evs) > 0 {
		at = evs[len(evs)-1].At
	}
	e.setKV(ctx, KVDaemonPausedAt, store.FormatTime(at))
	return at
}

// pauseTabBar is the tab bar's part for `magnum pause`: "paused 19h" and,
// when it holds review requests, "6 requests held".
func (e *Engine) pauseTabBar(ctx context.Context) []string {
	s := "paused"
	if at, ok := e.kvTime(ctx, KVDaemonPausedAt); ok {
		s += " " + pauseAge(e.now().Sub(at))
	}
	parts := []string{s}
	if v, _ := e.getKV(ctx, KVDaemonPausedHeld); v != "" && v != "0" {
		noun := "requests"
		if v == "1" {
			noun = "request"
		}
		parts = append(parts, v+" "+noun+" held")
	}
	return parts
}

// pauseAge is how long a pause has lasted, for the tab bar: "7m", "19h", "3d".
func pauseAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(int(d/time.Minute), 0))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// findingsText is a review's findings by priority for a toast: "1 P1, 2 P2",
// or "no findings".
func findingsText(f map[string]int) string {
	var parts []string
	for _, k := range []string{"P0", "P1", "P2", "P3"} {
		if n := f[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(parts) == 0 {
		return "no findings"
	}
	return strings.Join(parts, ", ")
}

// tookText is how long a round took: seconds under a minute, else minutes.
func tookText(d time.Duration) string {
	if d < time.Minute {
		return humanDuration(d.Round(time.Second))
	}
	return humanDuration(d.Round(time.Minute))
}
