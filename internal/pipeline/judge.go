package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// judgeReadLines covers the judge's final MAGNUM_RESULT line plus its
	// closing summary.
	judgeReadLines = 200
	// verifyAttempts bounds GitHub read retries during verification.
	verifyAttempts = 3
	// clockSkew widens "posted after the prompt" for marker-less reviews.
	clockSkew = 2 * time.Minute
)

// verdict is the judge's turn turned into a round outcome.
type verdict struct {
	final   bool // false: nothing posted, no result, healthy -> nudge
	outcome string
	err     error
	review  *postedReview
	result  *judgeResult
	pause   *Pause
	// modelLimit: the turn ended on its model's own limit. The verdict is
	// the usage limit it was before per-model limits existed; runJudge first
	// tries a fallback model (modelFallback).
	modelLimit *agents.Health
}

// postedReview is the review verification found.
type postedReview struct {
	id     int64
	url    string
	commit string
	state  string
	leak   string // the login that posted it when it is not the reviewer
	body   string // the review's body as GitHub shows it ("" = not read)
	// duplicates are the reviewer's other submitted reviews carrying the
	// run's marker (the kept one is the first), pending its unsubmitted
	// drafts carrying it (see handleDuplicates).
	duplicates, pending []int64
}

// runJudge prompts the judge, waits for its turn, verifies the result and
// nudges once when it stopped without one. A turn that ended on its model's
// own limit continues on the kind's fallback models first (modelFallback;
// a kind that cannot switch pauses as on a usage limit).
func (rd *round) runJudge(ctx context.Context, run store.Run) (RoundResult, error) {
	in := rd.in
	marker := run.ID
	switch {
	case in.Kind == KindContinue && in.ContinueRunID != "":
		marker = in.ContinueRunID
	case rd.unverified != "":
		// An earlier review on this head may be on GitHub: the judge looks
		// for its marker before posting (unverified.go).
		marker = rd.unverified
	}
	rd.mu.Lock()
	rd.res.JudgeRunID = marker
	rd.mu.Unlock()

	jd := rd.judgeData(run, marker)
	rd.addThreads(ctx, &jd)
	rd.addDeltaCheck(ctx, &jd)
	text, err := rd.r.Agents.RolePrompt(rd.judge, judgePrompt(in.Kind), jd)
	if err != nil {
		rd.finishRun(ctx, run.ID, store.RunFailed, OutcomeError, err.Error())
		return rd.done(ctx, OutcomeError, fmt.Errorf("pipeline: judge prompt: %w", err))
	}
	msg := fmt.Sprintf("prompting %s (run %s, %s)", rd.judge.Name, run.ID, in.Kind)
	if usual, _ := JudgeEvents(rd.r.Config, rd.owner+"/"+rd.name, &rd.idCfg, in.PostMerge); usual != jd.NoFindingsEvent {
		msg += fmt.Sprintf("; no %s this round, a report is missing: %s", usual, strings.Join(missingReports(jd.Reports), ", "))
	}
	rd.event(ctx, "info", "round.judge", msg,
		map[string]any{"run": run.ID, "marker": marker, "reports": jd.Reports, "no_findings_event": jd.NoFindingsEvent})
	markers := []string{marker}
	if marker != run.ID {
		markers = append(markers, run.ID)
	}
	ids := map[string]bool{marker: true, run.ID: true}
	t := rd.submitAndWait(ctx, run, text, rd.timeout(rd.judge), jd.ResultFile, ids)

	runIDs := []string{run.ID}
	since := rd.start
	if cur, err := rd.r.Store.RunByID(context.WithoutCancel(ctx), run.ID); err == nil && cur.SubmittedAt != nil {
		since = *cur.SubmittedAt
	}
	since = since.Add(-clockSkew)

	anchor := marker // locates the last prompt in the pane text
	var tried []string
	for nudged := false; ; nudged = true {
		v := rd.judgeVerdict(ctx, t, jd.ResultFile, markers, ids, since, anchor)
		for v.modelLimit != nil && t.kind == waitEnded {
			next, nanchor, nrun, ok := rd.modelFallback(ctx, rd.judge, t, *v.modelLimit, &tried, jd.ResultFile, jd.ResultFile, ids, false)
			if !ok {
				break
			}
			runIDs = append(runIDs, nrun)
			t, anchor = next, nanchor
			v = rd.judgeVerdict(ctx, t, jd.ResultFile, markers, ids, since, anchor)
		}
		if v.final && v.outcome == OutcomeTimeout {
			rd.stopJudge(ctx, t.run)
		}
		if v.final {
			return rd.finalizeJudge(ctx, runIDs, v)
		}
		if nudged {
			return rd.finalizeJudge(ctx, runIDs, verdict{final: true, outcome: OutcomeNeedsAttention, result: v.result,
				err: errors.New("the judge stopped without posting a review or writing its result, also after a nudge")})
		}
		nrun, err := rd.newRun(ctx, rd.judge, store.RunNudge)
		if err != nil {
			return rd.finalizeJudge(ctx, runIDs, verdict{final: true, outcome: OutcomeError, err: err})
		}
		ntext, err := rd.r.Agents.RolePrompt(rd.judge, config.PromptNudge, jd)
		if err != nil {
			return rd.finalizeJudge(ctx, append(runIDs, nrun.ID), verdict{final: true, outcome: OutcomeError, err: err})
		}
		rd.mu.Lock()
		rd.res.Nudged = true
		rd.mu.Unlock()
		rd.event(ctx, "warn", "round.nudge", fmt.Sprintf("the judge stopped without a result; nudging (run %s)", nrun.ID),
			map[string]any{"run": nrun.ID, "marker": marker})
		runIDs = append(runIDs, nrun.ID)
		ids[nrun.ID] = true
		t = rd.submitAndWait(ctx, *nrun, ntext, rd.timeout(rd.judge), jd.ResultFile, ids)
		anchor = marker
	}
}

// judgePrompt is the judge's prompt kind for a round kind.
func judgePrompt(kind string) string {
	switch kind {
	case KindRereview:
		return config.PromptRereview
	case KindContinue:
		return config.PromptContinue
	case KindRecovery:
		return config.PromptRecovery
	}
	return config.PromptInitial
}

// judgeVerdict decides what one judge turn produced: GitHub first (the
// oracle), then the result file / MAGNUM_RESULT line, then pane health
// after anchor (the turn's prompt).
func (rd *round) judgeVerdict(ctx context.Context, t turn, resultFile string, markers []string, ids map[string]bool, since time.Time, anchor string) verdict {
	switch {
	case t.kind == waitCancelled:
		return verdict{final: true, outcome: OutcomeStopped, err: t.err}
	case t.kind == waitRefused:
		return verdict{final: true, outcome: OutcomeError, err: fmt.Errorf("pipeline: judge prompt refused: %w", t.err)}
	case t.unsent:
		if errors.Is(t.err, agents.ErrBlocked) {
			return verdict{final: true, outcome: OutcomeBlocked, err: t.err}
		}
		return verdict{final: true, outcome: OutcomeError, err: fmt.Errorf("pipeline: judge prompt failed: %w", t.err)}
	}

	text := ""
	if s, ok := rd.judgeSession(ctx, t.run); ok {
		if out, err := rd.r.Agents.ReadRecent(context.WithoutCancel(ctx), s, judgeReadLines); err == nil {
			text = out
		} else {
			rd.logf("pipeline: read judge pane: %v", err)
		}
	}
	res, hasRes := readResultFile(resultFile)
	if hasRes && res.RunID != "" && !ids[res.RunID] {
		rd.warn(ctx, "ignoring %s written for run %s", resultFile, res.RunID)
		hasRes = false
	}
	if !hasRes {
		res, hasRes = parseResultLine(text, ids)
	}
	v := verdict{}
	if hasRes {
		v.result = &res
	}

	if !rd.in.DryRun {
		found, err := rd.findReview(ctx, markers, res, since)
		if err != nil {
			if ctx.Err() != nil {
				return verdict{final: true, outcome: OutcomeStopped, err: ctx.Err()}
			}
			return verdict{final: true, outcome: OutcomeError, result: v.result, err: fmt.Errorf("%w: %w", errUnverified, err)}
		}
		if found != nil {
			v.final, v.review = true, found
			if found.leak != "" {
				v.outcome = OutcomeIdentityLeak
				v.err = fmt.Errorf("review %d carrying the run's marker was posted as %q, not %q", found.id, found.leak, rd.login)
			} else {
				v.outcome = OutcomePosted
			}
			return v
		}
	}

	if hasRes {
		v.final = true
		blocker := res.Blocker
		switch res.Status {
		case statusDryRun:
			if rd.in.DryRun {
				v.outcome = OutcomeDryRun
				return v
			}
			v.outcome, v.err = OutcomeNeedsAttention, errors.New("the judge wrote a dry-run result for a live round")
		case statusPosted:
			v.outcome = OutcomeNeedsAttention
			if rd.in.DryRun {
				v.err = errors.New("the judge reported a posted review in a dry run")
			} else {
				v.err = fmt.Errorf("the judge reported review %d as posted, but GitHub shows no review by %s with the run's marker", res.ReviewID, rd.login)
			}
		case statusBlocked:
			v.outcome, v.err = OutcomeBlocked, reason("judge blocked", blocker)
		case statusIdentityError:
			v.outcome, v.err = OutcomeIdentityError, reason("judge identity check failed", blocker)
		case statusClosed:
			v.outcome, v.err = OutcomeClosed, reason("the judge found the PR closed", blocker)
		case statusStopped:
			v.outcome, v.err = OutcomeStopped, reason("the judge stopped", blocker)
		case statusError:
			v.outcome, v.err = OutcomeError, reason("the judge reported an error", blocker)
		default:
			v.outcome, v.err = OutcomeNeedsAttention, fmt.Errorf("unknown judge status %q", res.Status)
		}
		return v
	}

	h := rd.classifyAfter(rd.judge.AgentKind(), text, anchor)
	var limit *agents.Health
	if h.Kind == agents.HealthModelLimit {
		ml := h
		limit = &ml
		h = asUsageLimit(h)
	}
	switch h.Kind {
	case agents.HealthUsageLimit, agents.HealthLoginRequired, agents.HealthOverloaded:
		p := &Pause{Kind: string(h.Kind), Tool: rd.judge.AgentKind(), Detail: h.Detail}
		if h.ResetAt != nil {
			p.Until = *h.ResetAt
		}
		outcome := map[agents.HealthKind]string{agents.HealthUsageLimit: OutcomeUsageLimit,
			agents.HealthLoginRequired: OutcomeLoginRequired, agents.HealthOverloaded: OutcomeOverloaded}[h.Kind]
		return verdict{final: true, outcome: outcome, pause: p, err: fmt.Errorf("judge pane: %s", h.Detail), modelLimit: limit}
	case agents.HealthBlocked, agents.HealthTrustDialog:
		return verdict{final: true, outcome: OutcomeBlocked, err: fmt.Errorf("judge waits on a dialog: %s", h.Detail)}
	}
	switch t.kind {
	case waitTimeout:
		return verdict{final: true, outcome: OutcomeTimeout, err: t.err}
	case waitLost:
		return verdict{final: true, outcome: OutcomeError, err: fmt.Errorf("pipeline: judge session lost: %w", t.err)}
	case waitFailed:
		return verdict{final: true, outcome: OutcomeError, err: fmt.Errorf("pipeline: judge run failed: %w", t.err)}
	}
	return verdict{} // ended quietly: nudge
}

// stopJudge interrupts a judge whose turn timed out (ctrl+c twice), so it
// cannot post after the round ended, and waits up to InterruptWait for it to
// be seen idle (a warning when it is not).
func (rd *round) stopJudge(ctx context.Context, run store.Run) {
	rd.interrupt(ctx, rd.judge, run)
	if !rd.waitIdle(ctx, rd.judge, run) {
		rd.warn(ctx, "%s still works %s after it was interrupted", rd.judge.Name, InterruptWait)
	}
}

func reason(what, blocker string) error {
	if blocker == "" {
		return errors.New(what)
	}
	return fmt.Errorf("%s: %s", what, blocker)
}

// judgeSession is the session a judge run was sent to, else the live one.
func (rd *round) judgeSession(ctx context.Context, run store.Run) (store.Session, bool) {
	if s, ok := rd.session(ctx, run); ok {
		return s, true
	}
	s, err := rd.r.Store.LiveSessionByPRRole(context.WithoutCancel(ctx), rd.pr.ID, rd.judge.Name)
	return s, err == nil
}

// findReview looks for this round's review: by marker (any marker review by
// another login is a leak), else the review id from the result file
// (findClaimed), else a marker-less review by the reviewer on the target
// posted after the prompt. Only a submitted review counts (submitted reports
// it): a PENDING draft is never accepted, so nothing is dismissed around it
// either. Of several reviews carrying the marker the first (on the target)
// is the round's; the others are duplicates (postedReview.duplicates,
// pending).
func (rd *round) findReview(ctx context.Context, markers []string, res judgeResult, since time.Time) (*postedReview, error) {
	if p, err := rd.findClaimed(ctx, markers, res, since); p != nil || err != nil {
		return p, err
	}
	target := rd.in.TargetSHA
	all, err := rd.listReviews(ctx, "")
	if err != nil {
		return nil, err
	}
	var pick *github.Review
	for i := range all {
		rv := all[i]
		if rd.isReviewer(rv.AuthorLogin, rv.AuthorType) && rv.CommitOid == target && submitted(rv.State, rv.SubmittedAt) &&
			!rv.SubmittedAt.Before(since) {
			pick = &rv
		}
	}
	if pick == nil {
		return nil, nil
	}
	rd.warn(ctx, "review %d by %s on %s has no magnum:run marker; accepted as this round's review", pick.DatabaseID, rd.login, textx.ShortSHA(target))
	return rd.restCheck(ctx, *pick)
}

// findClaimed is findReview without its last resort: the review carrying one
// of markers, else the one the result file names (res.ReviewID) when the
// reviewer posted it on the target after since; nil when neither is there.
func (rd *round) findClaimed(ctx context.Context, markers []string, res judgeResult, since time.Time) (*postedReview, error) {
	target := rd.in.TargetSHA
	for _, id := range markers {
		reviews, err := rd.listReviews(ctx, "magnum:run="+id)
		if err != nil {
			return nil, err
		}
		re := markerRe(id)
		var ours, foreign, pending []github.Review
		for _, rv := range reviews {
			if !re.MatchString(rv.Body) {
				continue
			}
			switch {
			case isPending(rv.State):
				if rd.isReviewer(rv.AuthorLogin, rv.AuthorType) {
					pending = append(pending, rv)
				}
			case !rd.isReviewer(rv.AuthorLogin, rv.AuthorType):
				foreign = append(foreign, rv)
			case submitted(rv.State, rv.SubmittedAt):
				ours = append(ours, rv)
			}
		}
		if len(foreign) > 0 {
			rv := foreign[len(foreign)-1]
			login := rv.AuthorLogin
			if login == "" {
				login = "ghost"
			}
			return &postedReview{id: rv.DatabaseID, url: rv.URL, commit: rv.CommitOid, state: rv.State, leak: login}, nil
		}
		if len(ours) == 0 {
			continue
		}
		// Reviews come oldest first: keep the first, on the target if any.
		pick := ours[0]
		if i := slices.IndexFunc(ours, func(rv github.Review) bool { return rv.CommitOid == target }); i >= 0 {
			pick = ours[i]
		} else {
			rd.warn(ctx, "review %d is on commit %s, not the round's target %s", pick.DatabaseID, textx.ShortSHA(pick.CommitOid), textx.ShortSHA(target))
		}
		p, err := rd.restCheck(ctx, pick)
		if p == nil || err != nil || p.leak != "" {
			return p, err
		}
		for _, rv := range ours {
			if rv.DatabaseID != pick.DatabaseID {
				p.duplicates = append(p.duplicates, rv.DatabaseID)
			}
		}
		for _, rv := range pending {
			p.pending = append(p.pending, rv.DatabaseID)
		}
		return p, nil
	}

	if res.ReviewID > 0 {
		rest, err := rd.r.GitHub.ReviewREST(ctx, rd.owner, rd.name, rd.in.PR.Number, res.ReviewID)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			rd.logf("pipeline: review %d from the result file: %v", res.ReviewID, err)
		case rd.isReviewer(rest.UserLogin, rest.UserType) && rest.CommitID == target &&
			submitted(rest.State, rest.SubmittedAt) && !rest.SubmittedAt.Before(since):
			rd.warn(ctx, "review %d has no magnum:run marker; accepted from the judge's result", rest.ID)
			return &postedReview{id: rest.ID, url: rest.HTMLURL, commit: rest.CommitID, state: rest.State, body: rest.Body}, nil
		}
	}
	return nil, nil
}

// restCheck confirms the posting login over REST, where an App keeps its
// "[bot]" suffix. A failed lookup keeps the GraphQL verdict with a warning.
func (rd *round) restCheck(ctx context.Context, rv github.Review) (*postedReview, error) {
	p := &postedReview{id: rv.DatabaseID, url: rv.URL, commit: rv.CommitOid, state: rv.State, body: rv.Body}
	rest, err := rd.r.GitHub.ReviewREST(ctx, rd.owner, rd.name, rd.in.PR.Number, rv.DatabaseID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rd.warn(ctx, "REST check of review %d failed (GraphQL author matched): %v", rv.DatabaseID, err)
		return p, nil
	}
	if !rd.isReviewer(rest.UserLogin, rest.UserType) {
		p.leak = rest.UserLogin
	} else if !submitted(rest.State, rest.SubmittedAt) {
		return nil, nil // REST knows better: not (or no longer) a submitted review
	}
	if p.url == "" {
		p.url = rest.HTMLURL
	}
	if p.commit == "" {
		p.commit = rest.CommitID
	}
	if rest.Body != "" {
		p.body = rest.Body
	}
	return p, nil
}

// listReviews reads reviews with a few retries (a transient GitHub failure
// must not throw away a posted review).
func (rd *round) listReviews(ctx context.Context, marker string) ([]github.Review, error) {
	for attempt := 1; ; attempt++ {
		reviews, err := rd.r.GitHub.ReviewsWithMarker(ctx, rd.owner, rd.name, rd.in.PR.Number, marker)
		if err == nil {
			return reviews, nil
		}
		if ctx.Err() != nil || attempt >= verifyAttempts || errors.Is(err, github.ErrNotFound) ||
			errors.Is(err, github.ErrUnauthorized) || errors.Is(err, github.ErrForbidden) {
			return nil, err
		}
		rd.logf("pipeline: list reviews (attempt %d): %v", attempt, err)
		if serr := rd.r.sleep(ctx, rd.r.poll()); serr != nil {
			return nil, serr
		}
	}
}

// isReviewer reports whether a review author is the round's reviewer login:
// same login (ignoring "[bot]") and the same kind of account.
func (rd *round) isReviewer(login, typ string) bool {
	return login != "" && github.SameLogin(login, rd.login) && github.IsBot(typ, login) == rd.expectBot
}

// submitted reports whether a review was submitted: a COMMENTED, APPROVED or
// CHANGES_REQUESTED state with a submission time.
func submitted(state string, at time.Time) bool {
	switch normalizeEvent(state) {
	case "APPROVED", "COMMENTED", "CHANGES_REQUESTED":
		return !at.IsZero()
	}
	return false
}

func isPending(state string) bool { return strings.EqualFold(strings.TrimSpace(state), "PENDING") }

// markerRe matches the run's marker exactly (r-…-1 must not match r-…-10).
func markerRe(id string) *regexp.Regexp {
	return regexp.MustCompile(`magnum:run=` + regexp.QuoteMeta(id) + `(?:[^A-Za-z0-9._-]|$)`)
}

// finalizeJudge records the verdict on the judge's runs, dismisses a stale
// CHANGES_REQUESTED after a posted review, and ends the round.
func (rd *round) finalizeJudge(ctx context.Context, runIDs []string, v verdict) (RoundResult, error) {
	if v.outcome == OutcomeStopped && ctx.Err() != nil {
		for _, id := range runIDs {
			rd.abandonIfPending(ctx, id)
		}
		return rd.stopped(ctx)
	}
	rd.mu.Lock()
	if v.review != nil {
		rd.res.ReviewID, rd.res.ReviewURL, rd.res.ReviewCommit = v.review.id, v.review.url, v.review.commit
		rd.res.Event = normalizeEvent(v.review.state)
	}
	if v.result != nil {
		rd.res.Findings = v.result.Findings
		if rd.res.Event == "" {
			rd.res.Event = normalizeEvent(v.result.Event)
		}
		if rd.res.ReviewURL == "" && v.outcome == OutcomePosted {
			rd.res.ReviewURL = v.result.ReviewURL
		}
	}
	rd.res.Pause = v.pause
	res := rd.res
	rd.mu.Unlock()

	errMsg := ""
	if v.err != nil {
		errMsg = v.err.Error()
	}
	for _, id := range runIDs {
		if v.outcome == OutcomePosted || v.outcome == OutcomeDryRun {
			rd.finishRun(ctx, id, store.RunVerified, v.outcome, "", func(u *store.RunUpdate) {
				if v.review != nil {
					u.Set("review_id", v.review.id)
					u.Set("review_event", res.Event)
					u.Set("review_commit", v.review.commit)
					u.Set("review_url", res.ReviewURL)
				} else if res.Event != "" {
					u.Set("review_event", res.Event)
				}
				if v.result != nil && v.result.Raw != "" {
					u.Set("result_json", execx.Redact(v.result.Raw))
				}
			})
			continue
		}
		rd.finishRun(ctx, id, store.RunFailed, v.outcome, errMsg, func(u *store.RunUpdate) {
			if v.review != nil {
				u.Set("review_id", v.review.id)
				u.Set("review_url", v.review.url)
			}
			if v.result != nil && v.result.Raw != "" {
				u.Set("result_json", execx.Redact(v.result.Raw))
			}
		})
	}
	level := "info"
	if v.outcome != OutcomePosted && v.outcome != OutcomeDryRun {
		level = "warn"
	}
	rd.event(ctx, level, "round.verify", fmt.Sprintf("judge verdict: %s", v.outcome), map[string]any{
		"outcome": v.outcome, "review_id": res.ReviewID, "event": res.Event, "commit": res.ReviewCommit, "runs": runIDs})

	if v.result != nil {
		rd.environmentFailures(ctx, v.result.EnvironmentFailures)
	}
	if v.outcome == OutcomePosted {
		rd.recordFindings(ctx, runIDs[0], v.result)
		rd.handleDuplicates(ctx, v.review)
		rd.checkLocalPaths(ctx, v.review)
		rd.fixFooter(ctx, v.review)
		rd.dismissStale(ctx, res.Event, res.ReviewID, res.ReviewURL)
	}
	return rd.done(ctx, v.outcome, v.err)
}

func (rd *round) abandonIfPending(ctx context.Context, id string) {
	_ = rd.r.Store.TransitionRun(context.WithoutCancel(ctx), id, []string{store.RunPending}, store.RunAbandoned, func(u *store.RunUpdate) {
		u.Set("error", "round cancelled")
		u.Set("ended_at", rd.r.now())
	})
}

// dismissStale dismisses the identity's previous CHANGES_REQUESTED once a
// non-blocking review superseded it (identity.dismiss_own_stale_change_requests,
// on by default for apps). Failure, including a missing permission, is a
// warning only. A post-merge review dismisses nothing.
func (rd *round) dismissStale(ctx context.Context, event string, newID int64, newURL string) {
	prev := rd.in.Previous
	if rd.in.DryRun || rd.in.PostMerge || prev == nil || prev.ID == 0 || prev.ID == newID || !rd.idCfg.DismissStale() ||
		!isChangesRequested(prev.Event) || isChangesRequested(event) ||
		prev.Former || // a former login's: the engine dismisses it as that identity
		prev.Manual { // the reviewer's own verdict stands until they change it
		return
	}
	msg := fmt.Sprintf("Superseded by the newer magnum review of %s: %s", textx.ShortSHA(rd.in.TargetSHA), newURL)
	if newURL == "" {
		msg = fmt.Sprintf("Superseded by the newer magnum review %d of %s.", newID, textx.ShortSHA(rd.in.TargetSHA))
	}
	err := rd.r.GitHub.DismissReview(ctx, rd.owner, rd.name, rd.in.PR.Number, prev.ID, msg)
	if err != nil {
		if errors.Is(err, github.ErrForbidden) {
			rd.warn(ctx, "dismiss stale review %d: missing permission: %v", prev.ID, err)
		} else {
			rd.warn(ctx, "dismiss stale review %d: %v", prev.ID, err)
		}
		return
	}
	rd.mu.Lock()
	rd.res.DismissedReviewID = prev.ID
	rd.mu.Unlock()
	rd.event(ctx, "info", "round.dismiss", fmt.Sprintf("dismissed stale CHANGES_REQUESTED review %d", prev.ID),
		map[string]any{"review_id": prev.ID, "superseded_by": newID})
}

// PostMergeEvent is the review event of a post-merge round
// (RoundInput.PostMerge), with or without findings.
const PostMergeEvent = "COMMENT"

// noApproveEvent is the no-findings event of a round that lacks a reviewer's
// report (JudgeEvents).
const noApproveEvent = "COMMENT"

// JudgeEvents are the review events the judge's prompt names for a round of
// repository fullName ("owner/name") posted as identity id: the
// repository's or the identity's (config.Config.VerdictsFor); COMMENT both
// ways in a post-merge round, where a verdict blocks nothing; and COMMENT
// for no findings when one of the round's reports is missing (an APPROVE
// once went out while claude-review had hit a usage limit: a review that did
// not hear every reviewer approves nothing).
func JudgeEvents(cfg *config.Config, fullName string, id *config.Identity, postMerge bool, reports ...agents.Report) (noFindings, blocking string) {
	if postMerge {
		return PostMergeEvent, PostMergeEvent
	}
	noFindings, blocking = cfg.VerdictsFor(fullName, id)
	if len(missingReports(reports)) > 0 {
		noFindings = noApproveEvent
	}
	return noFindings, blocking
}

// missingReports names the reports a role left unusable, with why
// ("claude-review (usage_limit)").
func missingReports(reports []agents.Report) []string {
	var out []string
	for _, r := range reports {
		if r.Missing || r.Path == "" {
			out = append(out, fmt.Sprintf("%s (%s)", r.Role, cmp.Or(r.Status, r.Detail, "no report")))
		}
	}
	return out
}

// judgeData fills the judge templates.
func (rd *round) judgeData(run store.Run, marker string) agents.JudgeData {
	in := rd.in
	reports := rd.judgeReports()
	nf, be := JudgeEvents(rd.r.Config, rd.owner+"/"+rd.name, &rd.idCfg, in.PostMerge, reports...)
	skill := config.SkillPath(rd.judge.Skill, rd.r.Layout) // config.Defaults keeps {{repo}} unexpanded
	base := in.BaseRef
	if base == "" {
		base = in.Repo.DefaultBranch
	}
	jd := agents.JudgeData{
		RunID: marker, Owner: rd.owner, Repo: rd.name, Number: in.PR.Number, URL: in.PR.URL,
		HeadSHA: in.TargetSHA, BaseRef: base, BaseSHA: in.BaseSHA, Checkout: in.SlotPath,
		IdentityKind: rd.r.Identity.Kind(), ReviewerLogin: rd.login, GhConfigDir: rd.ghDir,
		NoFindingsEvent: nf, BlockingEvent: be, SelfAuthored: rd.selfAuthored(), Footer: rd.idCfg.Footer(),
		Reports: reports, ResultFile: rd.reportPath(run, rd.judge), DryRun: in.DryRun, Blind: in.Blind, PostMerge: in.PostMerge,
		SkillPath: skill, Model: rd.r.Config.RoleModel(rd.judge), Effort: rd.judge.EffortFor(in.Kind == KindRereview || rd.deltaCheck() != nil),
		ForcePushed: in.ForcePushed, BaseMerged: in.BaseMerged, MovedFrom: in.MovedFrom, PreviousHeadSHA: rd.previousHead(),
		NotesPath: in.NotesPath,
	}
	jd.EffortInPrompt = rd.effortInPrompt(rd.judge, jd.Effort)
	if in.NotesPath != "" {
		jd.NotesDir, jd.NotesLock = agents.NotesFiles(in.NotesPath)
		jd.NotesHarness, jd.NotesHarnessMore = agents.NotesHarness(jd.NotesDir)
	}
	if p := in.Previous; p != nil {
		jd.PreviousReviewID, jd.PreviousEvent = p.ID, p.Event
	}
	jd.FormerLogins = slices.Clone(in.FormerLogins)
	if !in.Since.IsZero() {
		jd.Since = in.Since.UTC().Format(time.RFC3339)
	}
	history := in.History
	if len(history) == 0 && in.Previous != nil {
		history = []PreviousReview{*in.Previous}
	}
	for _, h := range history {
		pr := agents.PreviousReview{ID: h.ID, Event: h.Event, SHA: textx.ShortSHA(h.SHA)}
		if !h.SubmittedAt.IsZero() {
			pr.SubmittedAt = h.SubmittedAt.UTC().Format(time.RFC3339)
		}
		jd.PreviousReviews = append(jd.PreviousReviews, pr)
	}
	jd.Readiness = rd.readinessData()
	return jd
}

// selfAuthored: the PR's author is the user or the reviewer login itself
// (Account form: the user's PR is self-authored for the user's App too,
// through SelfLogin, never because the App is named like the user).
func (rd *round) selfAuthored() bool {
	author := github.Account(store.Deref(rd.in.PR.AuthorLogin), store.Deref(rd.in.PR.AuthorType))
	if author == "" {
		return false
	}
	return github.SameAccount(author, rd.r.SelfLogin) || github.SameAccount(author, rd.login)
}
