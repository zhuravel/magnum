package pipeline

// A review the judge posted while GitHub failed every verification attempt
// (listReviews) ends its round in error, and the engine queues the PR
// again. The review may well be on GitHub, so the next round on the same
// head looks for it first, by the marker of the judge run that could not be
// verified (the run rows keep it: state failed, error errUnverified):
// found, the round adopts it, prompting nothing; not found (or GitHub still
// failing), the round runs and its judge's review carries that marker, so
// the judge, which lists the reviews for its marker right before it posts,
// finds the earlier review if it is there after all instead of posting a
// second one, and verification finds it by the same marker.
//
// A round that ended without the judge's result, also after a nudge
// (agents.JudgeNoResult), is looked for the same way: on 2026-11-05 such a
// judge worked on and posted its review with the round's marker 15 minutes
// after its round ended, and the PR stayed parked as failed. The next round
// on the head adopts that review, with the late result file's findings.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// errUnverified starts the error of a judge run whose round could not ask
// GitHub for its review; findUnverified reads it back from the run rows.
var errUnverified = errors.New("pipeline: verify on GitHub")

// unverifiedRuns are the judge's attempts on the round's target whose review
// magnum could not verify.
type unverifiedRuns struct {
	round   int       // the earliest such round
	markers []string  // the markers their reviews carry, earliest first
	runs    []string  // the judge runs that failed verification
	since   time.Time // the earliest prompt among them, less clockSkew
	result  judgeResult
	// late: every run is of a round that ended without the judge's result
	// (agents.JudgeNoResult), none failed verification.
	late bool
}

// findUnverified looks through the PR's judge runs (any declared judge's,
// judgeRole), newest first and back to the last one verified as posted, for
// runs on the round's target by the round's identity that failed
// verification (errUnverified) or whose round ended without the judge's
// result (agents.JudgeNoResult); the result is the first such run's
// recorded one, else, for the latter, the judge's result file when the
// judge wrote it late for one of those runs or their markers (lateResult).
// A dry run and a continued turn (whose marker is already the paused run's)
// look for none.
func (rd *round) findUnverified(ctx context.Context) *unverifiedRuns {
	if rd.in.DryRun || rd.in.Kind == KindContinue {
		return nil
	}
	runs, err := rd.r.Store.RunsByPR(ctx, rd.in.PR.ID)
	if err != nil {
		rd.logErr(ctx, err, "pipeline: earlier judge runs of %s: %v", rd.subject, err)
		return nil
	}
	u := &unverifiedRuns{late: true}
	rounds := map[int]bool{}
	var last *store.Run // the newest run whose round gave up on the judge's result
	for _, r := range slices.Backward(runs) {
		if _, judge := rd.judgeRole(r.Role); !judge {
			continue
		}
		if r.State == store.RunVerified && store.Deref(r.Outcome) == OutcomePosted {
			break
		}
		unverified, gaveUp := strings.HasPrefix(store.Deref(r.Error), errUnverified.Error()), strings.HasPrefix(store.Deref(r.Error), agents.JudgeNoResult)
		if r.State != store.RunFailed || r.TargetSHA != rd.in.TargetSHA || r.Identity != rd.r.Identity.Name() || !unverified && !gaveUp {
			continue
		}
		if gaveUp && last == nil {
			last = &r
		}
		u.late = u.late && gaveUp
		u.runs = append(u.runs, r.ID)
		rounds[r.Round] = true
		if u.result.Status == "" && r.ResultJSON != nil {
			u.result, _ = parseResult([]byte(*r.ResultJSON))
		}
	}
	if len(u.runs) == 0 {
		return nil
	}
	// A round's review carries the id of its first judge run that was
	// prompted: nudges and continuations quote it, and the own pass, which
	// posts nothing, quoted it before (ownpass.go).
	marked := map[int]bool{}
	for _, r := range runs {
		if _, judge := rd.judgeRole(r.Role); !rounds[r.Round] || marked[r.Round] || !judge || r.SubmittedAt == nil ||
			r.Kind == store.RunContinue || r.Kind == store.RunNudge || r.Kind == store.RunOwnPass {
			continue
		}
		marked[r.Round] = true
		u.markers = append(u.markers, r.ID)
		if u.round == 0 {
			u.round = r.Round
		}
		if since := r.SubmittedAt.Add(-clockSkew); u.since.IsZero() || since.Before(u.since) {
			u.since = since
		}
	}
	if len(u.markers) == 0 {
		return nil
	}
	if u.result.Status == "" && last != nil {
		u.result = rd.lateResult(*last, append(slices.Clone(u.runs), u.markers...))
	}
	return u
}

// LateReview is the review the judge's result file at path says the judge
// posted for one of runIDs, the runs of a round that ended without its
// result (agents.JudgeNoResult): its late, complete result, which the next
// round on the head adopts (findUnverified); ok is false for none.
func LateReview(path string, runIDs ...string) (reviewID int64, ok bool) {
	res, found := readResultFile(path)
	if !found || res.Status != statusPosted || !slices.Contains(runIDs, res.RunID) {
		return 0, false
	}
	return res.ReviewID, true
}

// lateResult is the judge's result file of run (one of a round that ended
// without the judge's result) when it was written for one of ids: what the
// judge wrote after its round ended; the zero result otherwise.
func (rd *round) lateResult(run store.Run, ids []string) judgeResult {
	judge, _ := rd.judgeRole(run.Role)
	res, ok := readResultFile(rd.reportPath(run, judge))
	if !ok || !slices.Contains(ids, res.RunID) {
		return judgeResult{}
	}
	return res
}

// judgeRole is the declared judge role named name: an earlier round's judge
// may be another than this round's (a judge_fallback in the place of a held
// kind's, a flagged PR's), and its review is the head's all the same.
func (rd *round) judgeRole(name string) (config.Role, bool) {
	if name == rd.judge.Name {
		return rd.judge, true
	}
	for _, j := range rd.r.Config.Judges() {
		if j.Name == name {
			return j, true
		}
	}
	return config.Role{}, false
}

// adoptUnverified ends the round before it starts when an earlier judge run
// posted its review on this head while GitHub could not be asked
// (findUnverified) and GitHub now shows it: that review becomes the round's
// result, recorded on the runs that posted it, and handled as after any post
// (findings, duplicates, local paths, the footer, a stale
// CHANGES_REQUESTED). nil lets the round run; when there was such a run, its
// judge's review carries the earlier marker (round.unverified).
func (rd *round) adoptUnverified(ctx context.Context) func() (RoundResult, error) {
	u := rd.findUnverified(ctx)
	if u == nil {
		return nil
	}
	found, err := rd.findClaimed(ctx, u.markers, u.result, u.since)
	switch {
	case ctx.Err() != nil:
		return func() (RoundResult, error) { return rd.stopped(ctx) }
	case err != nil || found == nil:
		why := "it is not on GitHub"
		if err != nil {
			why = "GitHub still fails: " + err.Error()
		}
		level, what := "warn", "the review of judge run %s (round %d) on %s could not be verified"
		if u.late { // news, not a failure: the judge may still be at work on it
			level, what = "info", "the judge of run %s (round %d) on %s stopped without its result"
		}
		rd.unverified = u.markers[0]
		rd.event(ctx, level, "round.unverified", fmt.Sprintf(what+" and %s; "+
			"this round's judge looks for it before posting: its review carries the same marker", u.markers[0], u.round, textx.ShortSHA(rd.in.TargetSHA), why),
			map[string]any{"markers": u.markers, "runs": u.runs, "late": u.late})
		return nil
	}

	rd.in.Round = u.round
	outcome, verr := OutcomePosted, error(nil)
	if found.leak != "" {
		outcome, verr = OutcomeIdentityLeak, fmt.Errorf("review %d carrying the run's marker was posted as %q, not %q", found.id, found.leak, rd.login)
	}
	rd.mu.Lock()
	rd.res.Round, rd.res.JudgeRunID = u.round, u.markers[0]
	rd.res.ReviewID, rd.res.ReviewURL, rd.res.ReviewCommit = found.id, found.url, found.commit
	rd.res.Event = normalizeEvent(found.state)
	if rd.res.Event == "" {
		rd.res.Event = normalizeEvent(u.result.Event)
	}
	rd.res.Findings, rd.res.PreviousOpen = u.result.Findings, u.result.PreviousOpen
	res := rd.res
	rd.mu.Unlock()

	wctx := context.WithoutCancel(ctx)
	now := rd.r.now()
	for _, id := range u.runs {
		var err error
		if outcome == OutcomePosted {
			err = rd.r.Store.TransitionRun(wctx, id, []string{store.RunFailed}, store.RunVerified, func(up *store.RunUpdate) {
				up.Set("outcome", OutcomePosted)
				up.Set("error", nil)
				up.Set("verified_at", now)
				up.Set("review_id", found.id)
				up.Set("review_event", res.Event)
				up.Set("review_commit", found.commit)
				up.Set("review_url", found.url)
			})
		} else {
			err = rd.r.Store.UpdateRun(wctx, id, func(up *store.RunUpdate) {
				up.Set("outcome", outcome)
				up.Set("error", verr.Error())
				up.Set("review_id", found.id)
				up.Set("review_url", found.url)
			})
		}
		if err != nil {
			rd.logErr(ctx, err, "pipeline: adopt review %d on run %s: %v", found.id, id, err)
		}
	}
	which := "which GitHub could not verify then"
	if u.late {
		which = "which the judge posted after its round ended without its result"
	}
	rd.event(ctx, "info", "round.adopted", fmt.Sprintf("review %d of judge run %s (round %d), %s, is on %s: "+
		"adopted without a new round", found.id, u.markers[0], u.round, which, textx.ShortSHA(rd.in.TargetSHA)),
		map[string]any{"review_id": found.id, "markers": u.markers, "runs": u.runs, "outcome": outcome, "late": u.late})
	if outcome == OutcomePosted {
		// The adopted review is verified: its follow-ups finish even when
		// the round ends now, as after a review verified at once (judge.go).
		r := u.result
		rd.recordFindings(wctx, u.markers[0], &r)
		rd.handleDuplicates(wctx, found)
		rd.checkLocalPaths(wctx, found)
		rd.appendFooter(wctx, found, &r)
		rd.dismissStale(wctx, res.Event, found.id, found.url)
	}
	return func() (RoundResult, error) { return rd.done(ctx, outcome, verr) }
}
