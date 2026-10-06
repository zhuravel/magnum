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

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
}

// findUnverified looks through the PR's judge runs, newest first and back
// to the last one verified as posted, for runs on the round's target by the
// round's identity that failed verification (errUnverified). A dry run and a
// continued turn (whose marker is already the paused run's) look for none.
func (rd *round) findUnverified(ctx context.Context) *unverifiedRuns {
	if rd.in.DryRun || rd.in.Kind == KindContinue {
		return nil
	}
	runs, err := rd.r.Store.RunsByPR(ctx, rd.in.PR.ID)
	if err != nil {
		rd.logf("pipeline: earlier judge runs of %s: %v", rd.subject, err)
		return nil
	}
	u := &unverifiedRuns{}
	rounds := map[int]bool{}
	for _, r := range slices.Backward(runs) {
		if r.Role != rd.judge.Name {
			continue
		}
		if r.State == store.RunVerified && store.Deref(r.Outcome) == OutcomePosted {
			break
		}
		if r.State != store.RunFailed || r.TargetSHA != rd.in.TargetSHA || r.Identity != rd.r.Identity.Name() ||
			!strings.HasPrefix(store.Deref(r.Error), errUnverified.Error()) {
			continue
		}
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
		if !rounds[r.Round] || marked[r.Round] || r.Role != rd.judge.Name || r.SubmittedAt == nil ||
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
	return u
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
		rd.unverified = u.markers[0]
		rd.event(ctx, "warn", "round.unverified", fmt.Sprintf("the review of judge run %s (round %d) on %s could not be verified and %s; "+
			"this round's judge looks for it before posting: its review carries the same marker", u.markers[0], u.round, textx.ShortSHA(rd.in.TargetSHA), why),
			map[string]any{"markers": u.markers, "runs": u.runs})
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
	rd.res.Findings = u.result.Findings
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
			rd.logf("pipeline: adopt review %d on run %s: %v", found.id, id, err)
		}
	}
	rd.event(ctx, "info", "round.adopted", fmt.Sprintf("review %d of judge run %s (round %d), which GitHub could not verify then, is on %s: "+
		"adopted without a new round", found.id, u.markers[0], u.round, textx.ShortSHA(rd.in.TargetSHA)),
		map[string]any{"review_id": found.id, "markers": u.markers, "runs": u.runs, "outcome": outcome})
	if outcome == OutcomePosted {
		r := u.result
		rd.recordFindings(ctx, u.markers[0], &r)
		rd.handleDuplicates(ctx, found)
		rd.checkLocalPaths(ctx, found)
		rd.appendFooter(ctx, found, &r)
		rd.dismissStale(ctx, res.Event, found.id, found.url)
	}
	return func() (RoundResult, error) { return rd.done(ctx, outcome, verr) }
}
