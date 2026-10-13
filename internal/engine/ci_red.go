package engine

// CI follow-ups (DECISIONS "A head whose CI turns red after the review gets
// a judge check"): on one PR magnum's re-review said "CI is red: Dependency
// Audit failed, unrelated" a minute before the head's RSpec run started,
// and that run finished red with two failures the PR causes; nothing looked
// at the reviewed head's CI again, and a colleague's review reported both an
// hour later. The judge sees the head's failing checks only when its prompt
// goes out (pipeline.FailingChecksFile). Now a check of the reviewed head
// that fails after the judge of the review standing on it was prompted
// (store.PostedPromptAt; a check that failed before was in that prompt's
// failing-checks.json) queues a judge-only round of that head, the same way
// replies do (replies.go): the PR waits in rereview_pending for nothing but
// a snooze or a mute, quiet hours, the interval and the cap do not hold it,
// it counts against no cap, and its prompt tells the judge that CI finished
// red after its review (pipeline.RoundInput.CIRed). kvPRCIFollowup keeps
// the failed checks each head's follow-ups covered, so a rerun that fails
// the same way starts nothing more; a rerun that turns them pending or
// passes drops a follow-up still waiting, and a push supersedes it. The
// checks' names are the PR's workflows' (PR content): they stay in the
// registry, never in an event.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRCIFollowup holds a PR's CI follow-ups of its reviewed head
// (ciFollowup as JSON).
func kvPRCIFollowup(prID int64) string { return fmt.Sprintf("pr.%d.ci_followup", prID) }

// ciFollowup is what kvPRCIFollowup records: the head, the failed checks its
// follow-ups covered or wait to cover, when failed checks last queued one
// that has not started (zero: none waits) and when the last one started.
type ciFollowup struct {
	Head    string      `json:"head"`
	Checks  []ciFailure `json:"checks"`
	Queued  time.Time   `json:"queued,omitzero"`
	Started time.Time   `json:"started,omitzero"`
}

// ciFailure is a failed check of a CI follow-up: its workflow, its name and
// when it failed (store.CheckResult.At). A check is one (workflow, name).
type ciFailure struct {
	Workflow string    `json:"workflow,omitempty"`
	Name     string    `json:"name"`
	At       time.Time `json:"at"`
}

// same reports whether c and o are the same check, whenever each failed.
func (c ciFailure) same(o ciFailure) bool { return c.Workflow == o.Workflow && c.Name == o.Name }

// covers reports whether the record holds check c already.
func (f ciFollowup) covers(c ciFailure) bool { return slices.ContainsFunc(f.Checks, c.same) }

// ciRedText is the wait's words for a PR whose judge check of red CI is due
// and held by nothing else (WaitCIRed).
const ciRedText = "CI turned red after the review; judge check pending"

// ciRed is what the CI of a PR's reviewed head says for its follow-up
// (ciRedOf): the record, when the judge of the review posted on the head
// was prompted, and the failed checks after that prompt: fresh ones the
// record lacks (they queue a judge check), the record's that still fail (a
// judge check is due) and the record's that fail no longer (gone: a rerun
// runs or passed, so their wait drops). A check of the record that failed
// before that prompt is covered: a posted review's judge saw it.
type ciRed struct {
	rec              ciFollowup
	prompt           time.Time
	fresh, due, gone []ciFailure
}

// ciWatched reports whether pr's CI may call for a follow-up: open, not
// merged, its head the one magnum reviewed and the stored checks that
// head's. A muted PR queues none (considerCIRed), and one queued before the
// mute waits for the unmute as any round does.
func ciWatched(pr store.PR) bool {
	return pr.GHState == store.GHOpen && !postMerge(pr) && pr.HeadSHA != "" &&
		pr.HeadSHA == deref(pr.ReviewedSHA) && pr.CI != nil && pr.CI.SHA == pr.HeadSHA
}

// ciPollWorth reports, from the row a poll upserted, whether considerCIRed
// has anything to read: a watched head with a failed check, or one waiting
// (a follow-up may drop).
func ciPollWorth(pr store.PR) bool {
	if !ciWatched(pr) {
		return false
	}
	return pr.State == store.PRRereviewPending || slices.ContainsFunc(pr.CI.Checks, func(c store.CheckResult) bool {
		return c.State == store.CheckFailed
	})
}

// ciFollowupRecord reads the PR's record, whatever its head.
func (e *Engine) ciFollowupRecord(ctx context.Context, prID int64) (ciFollowup, bool) {
	v, ok := e.getKV(ctx, kvPRCIFollowup(prID))
	var rec ciFollowup
	if !ok || json.Unmarshal([]byte(v), &rec) != nil || rec.Head == "" {
		return ciFollowup{}, false
	}
	return rec, true
}

// putCIFollowup records rec, or deletes the record when rec covers no
// check.
func (e *Engine) putCIFollowup(ctx context.Context, prID int64, rec ciFollowup) {
	if len(rec.Checks) == 0 {
		e.delKV(ctx, kvPRCIFollowup(prID))
		return
	}
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, kvPRCIFollowup(prID), string(b))
	}
}

// ciRedOf reads what pr's CI says for its follow-up; ok is false when its
// CI may call for none (ciWatched), no check failed and the record is
// empty, or no review posted on the head has a prompt time (a push that
// only merged the base kept an older head's review: magnum never reviewed
// this one).
func (e *Engine) ciRedOf(ctx context.Context, pr store.PR) (ciRed, bool) {
	if !ciWatched(pr) {
		return ciRed{}, false
	}
	rec, _ := e.ciFollowupRecord(ctx, pr.ID)
	if rec.Head != pr.HeadSHA {
		rec = ciFollowup{Head: pr.HeadSHA} // another head's (supersedeCIRed deletes it)
	}
	var failed []ciFailure
	for _, c := range pr.CI.Checks {
		if c.State == store.CheckFailed {
			failed = append(failed, ciFailure{Workflow: c.Workflow, Name: c.Name, At: c.At.UTC()})
		}
	}
	if len(failed) == 0 && len(rec.Checks) == 0 {
		return ciRed{}, false
	}
	prompt, ok, err := e.st.PostedPromptAt(ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "CI follow-up: the review's prompt", "pr", pr.ID)
		return ciRed{}, false
	}
	if !ok {
		return ciRed{}, false
	}
	st := ciRed{rec: rec, prompt: prompt}
	for _, c := range failed {
		// A zero time is unknown: no telling whether the judge saw it.
		if !c.At.IsZero() && c.At.After(prompt) && !rec.covers(c) {
			st.fresh = append(st.fresh, c)
		}
	}
	for _, c := range rec.Checks {
		switch {
		case !c.At.After(prompt):
		case slices.ContainsFunc(failed, c.same):
			st.due = append(st.due, c)
		default:
			st.gone = append(st.gone, c)
		}
	}
	return st, true
}

// ciRedWait is when the latest check of a due judge check of pr's red CI
// failed (eligibility.PRFacts.CIRedAt); ok is false when none is due.
func (e *Engine) ciRedWait(ctx context.Context, pr store.PR) (time.Time, bool) {
	st, ok := e.ciRedOf(ctx, pr)
	if !ok || len(st.due) == 0 {
		return time.Time{}, false
	}
	return latestFailure(st.due), true
}

// ciRedDue reports whether a round dispatched for pr is a judge check of
// its red CI: a same-head re-review (sameHead) nobody forced or requested
// (those run as asked, and their judge reads the failing checks too) with
// a follow-up due.
func (e *Engine) ciRedDue(ctx context.Context, pr store.PR, sameHead, requested bool) bool {
	if !sameHead || requested || pr.Forced {
		return false
	}
	_, ok := e.ciRedWait(ctx, pr)
	return ok
}

// considerCIRed queues the judge check of a reviewed head whose checks
// failed after the review's judge was prompted and are not in the record
// yet (ciRedOf's fresh): a reviewed PR the watch's filters still accept
// moves to rereview_pending, one already waiting there (for replies, say)
// gets its time again, both with pr.ci_red and the checks added to the
// record. A forced or muted PR, a round in flight, a pause or the
// operator's attention is left alone: the next judge prompt lists the
// failing checks anyway. A follow-up whose checks no longer fail drops
// first (dropCIRed).
func (e *Engine) considerCIRed(ctx context.Context, repo store.Repo, w config.Watch, prID int64, now time.Time) error {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return err
	}
	st, ok := e.ciRedOf(ctx, pr)
	if !ok {
		return nil
	}
	if len(st.gone) > 0 {
		if st, err = e.dropCIRed(ctx, repo, pr, st); err != nil {
			return err
		}
		if pr, err = e.st.PRByID(ctx, prID); err != nil {
			return err
		}
	}
	if len(st.fresh) == 0 || pr.Forced || pr.Muted || pr.DetailsAt == nil {
		return nil
	}
	switch pr.State {
	case store.PRReviewed:
		if dec := e.classify(ctx, w, pr, now); !dec.Eligible {
			return nil
		}
	case store.PRRereviewPending:
	default:
		return nil
	}
	prev := st.rec
	rec := st.rec
	rec.Checks = append(slices.Clone(rec.Checks), st.fresh...)
	rec.Queued = now.UTC()
	e.putCIFollowup(ctx, pr.ID, rec) // first: the throttle reads it
	if pr.State == store.PRReviewed {
		if err := e.queue(ctx, pr, w, []string{store.PRReviewed}, false, now, "CI red after the review"); err != nil {
			e.putCIFollowup(ctx, pr.ID, prev)
			return err
		}
	} else {
		e.retime(ctx, repo.FullName(), pr, "CI red after the review") // snooze.go
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.ci_red", ciRedMessage(pr.HeadSHA, st.fresh),
		map[string]any{"head_sha": pr.HeadSHA, "checks": len(st.fresh), "failed_at": latestFailure(st.fresh), "prompted_at": st.prompt})
	return nil
}

// dropCIRed takes the checks CI no longer shows failed (st.gone: a rerun
// runs, or passed) out of the record, so a rerun that fails again queues a
// judge check again: none ran for them. A PR that waited for that check
// alone (waitsForCIRedAlone) goes back to reviewed in a compare-and-set on
// both commits; a PR that waited says so with pr.ci_red_dropped. It
// returns st without them.
func (e *Engine) dropCIRed(ctx context.Context, repo store.Repo, pr store.PR, st ciRed) (ciRed, error) {
	st.rec.Checks = slices.DeleteFunc(slices.Clone(st.rec.Checks), func(c ciFailure) bool { return slices.ContainsFunc(st.gone, c.same) })
	st.gone = nil
	if len(st.due) > 0 {
		e.putCIFollowup(ctx, pr.ID, st.rec)
		return st, nil // another of its checks still fails: the judge check stays due
	}
	st.rec.Queued = time.Time{}
	e.putCIFollowup(ctx, pr.ID, st.rec)
	if pr.State != store.PRRereviewPending || len(st.fresh) > 0 {
		return st, nil
	}
	subject, head := prSubject(repo, pr.Number), textx.ShortSHA(pr.HeadSHA)
	why := "CI no longer shows the checks that queued the judge check of " + head + " failed (a rerun runs or passed)"
	if !e.waitsForCIRedAlone(ctx, pr, st.rec) {
		e.event(ctx, "info", subject, "pr.ci_red_dropped", why+"; the round the PR waits for reads its CI", map[string]any{"head_sha": pr.HeadSHA})
		return st, nil
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRRereviewPending}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", pr.HeadSHA)
		u.Set("next_eligible_at", nil)
		u.Set("pending_since", nil)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		u.Set("skip_reason", nil)
	})
	if err != nil {
		return st, err
	}
	e.event(ctx, "info", subject, "pr.ci_red_dropped", "rereview_pending → reviewed: "+why+"; the review stands",
		map[string]any{"head_sha": pr.HeadSHA, "state": store.PRReviewed})
	return st, nil
}

// waitsForCIRedAlone reports whether pr waits in rereview_pending at the
// head magnum reviewed (not forced, open) for nothing but its judge check
// of red CI: no review request waiting for its round, no replies to
// re-decide, and no retry of another round (a retry is the follow-up's own
// when it started the PR's last round, rec.Started).
func (e *Engine) waitsForCIRedAlone(ctx context.Context, pr store.PR, rec ciFollowup) bool {
	if pr.State != store.PRRereviewPending || pr.Forced || pr.GHState != store.GHOpen || postMerge(pr) ||
		pr.HeadSHA == "" || pr.HeadSHA != deref(pr.ReviewedSHA) {
		return false
	}
	if _, ok := e.pendingRequest(ctx, pr); ok {
		return false
	}
	if _, _, ok := e.replyTrigger(ctx, pr); ok {
		return false
	}
	retry := pr.Attempts > 0 || pr.NextAttemptAt != nil || pr.LastError != nil
	own := !rec.Started.IsZero() && pr.LastRoundStartedAt != nil && !rec.Started.Before(pr.LastRoundStartedAt.Add(-time.Second))
	return !retry || own
}

// ciRedHolds reports whether pr's red CI holds it in line (a judge check
// queued or due, or one to drop), so a wait for replies that are gone is
// not taken back as theirs (dropReplyWait): considerCIRed decides it.
func (e *Engine) ciRedHolds(ctx context.Context, pr store.PR) bool {
	st, ok := e.ciRedOf(ctx, pr)
	return ok && len(st.fresh)+len(st.due)+len(st.gone) > 0
}

// noteCIRound records that the judge check of target's red CI starts now
// (at): it waits no more.
func (e *Engine) noteCIRound(ctx context.Context, prID int64, target string, at time.Time) {
	rec, ok := e.ciFollowupRecord(ctx, prID)
	if !ok || rec.Head != target {
		return
	}
	rec.Queued, rec.Started = time.Time{}, at.UTC()
	e.putCIFollowup(ctx, prID, rec)
}

// supersedeCIRed deletes the record of an older head when pr's head moved:
// the push's round reads the new head's CI. A judge check that was waiting
// says so (pr.ci_red_dropped).
func (e *Engine) supersedeCIRed(ctx context.Context, repo store.Repo, pr store.PR) {
	rec, ok := e.ciFollowupRecord(ctx, pr.ID)
	if !ok || rec.Head == pr.HeadSHA {
		return
	}
	e.delKV(ctx, kvPRCIFollowup(pr.ID))
	if !rec.Queued.IsZero() {
		e.event(ctx, "info", prSubject(repo, pr.Number), "pr.ci_red_dropped",
			fmt.Sprintf("the push of %s supersedes the judge check of %s's red CI", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(rec.Head)),
			map[string]any{"head_sha": pr.HeadSHA, "previous_sha": rec.Head})
	}
}

// ciRedMessage is pr.ci_red's message for the fresh failures on head,
// which names no check: "CI finished red on b1c2d3e after the review (2
// failed checks, the last at 08:31); a judge check follows".
func ciRedMessage(head string, fresh []ciFailure) string {
	at := latestFailure(fresh).Local().Format("15:04")
	what := "1 failed check, at " + at
	if len(fresh) > 1 {
		what = fmt.Sprintf("%d failed checks, the last at %s", len(fresh), at)
	}
	return fmt.Sprintf("CI finished red on %s after the review (%s); a judge check follows", textx.ShortSHA(head), what)
}

// latestFailure is when the last of fs failed.
func latestFailure(fs []ciFailure) time.Time {
	var at time.Time
	for _, f := range fs {
		if f.At.After(at) {
			at = f.At
		}
	}
	return at
}
