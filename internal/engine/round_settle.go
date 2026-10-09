package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// setupFailed puts the PR back in line (or in needs_attention after
// maxAttempts) after a failure before the pipeline ran. An infrastructure
// failure pauses dispatch (pauseInfra) and the PR, not charged, waits for
// the probe.
func (e *Engine) setupFailed(ctx context.Context, job *roundJob, se *setupError) {
	if se.pause != nil {
		e.pauseTool(ctx, *se.pause)
	}
	if se.infra != "" {
		se.retryAt = e.pauseInfra(ctx, se.infra, se.err, se.probeDir)
		e.refund(ctx, job, job.round, RefundInfra, "setup failed")
	}
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("setup failed and PR is gone", "pr", job.pr.ID, "err", err)
		return
	}
	subject := prSubject(job.repo, pr.Number)
	e.event(ctx, "warn", subject, "engine.setup_failed", "round setup failed: "+se.err.Error(), nil)
	if job.kind != kindContinue {
		e.delKV(ctx, KVPRDeltaCheck(pr.ID))
	}
	if !se.noCharge {
		e.keptApprovalFailed(ctx, job.repo, pr, "its round could not start")
	}
	if job.hasSlot {
		// The slot stays the PR's, idle (held): claimed would keep it from
		// eviction while the PR waits for its retry.
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed}, store.SlotHeld, nil)
	}
	to := claimableState(pr)
	if job.kind == kindContinue {
		to = store.PRPaused
	}
	e.retryOrAttention(ctx, job, pr, []string{store.PRClaiming, store.PRReviewing}, to, se.err.Error(), !se.noCharge, se.retryAt)
	// Not charged is a wait (a person in the panes, a busy agent), not a
	// failure; an infrastructure pause has its own toast.
	if !se.noCharge && se.infra == "" {
		e.toastRequestedFailed(ctx, job, pr, "setup failed", se.err.Error()) // operator.go
	}
}

// retryOrAttention charges one attempt (when charge) and moves the PR to to
// with a backoff, or to needs_attention once the budget is spent.
func (e *Engine) retryOrAttention(ctx context.Context, job *roundJob, pr store.PR, from []string, to, msg string, charge bool, retryAt time.Time) {
	now := e.now()
	attempts := pr.Attempts
	if charge {
		attempts++
	}
	if retryAt.IsZero() && charge {
		retryAt = now.Add(backoff(attempts))
	}
	if attempts >= maxAttempts {
		e.needsAttention(ctx, job, pr, from, "failed", fmt.Sprintf("%d attempts on %s: %s", attempts, textx.ShortSHA(pr.HeadSHA), msg), func(u *store.PRUpdate) {
			u.Set("attempts", attempts)
		})
		return
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
		u.Set("attempts", attempts)
		u.Set("next_attempt_at", retryAt)
		u.Set("last_error", msg)
	})
	if err != nil {
		e.log.Info("PR moved on during the round", "pr", pr.ID, "err", err)
	}
}

// needsAttention parks the PR for a human and toasts. A post-merge round
// puts its PR back in closed instead (postMergeFailed).
func (e *Engine) needsAttention(ctx context.Context, job *roundJob, pr store.PR, from []string, why, msg string, extra func(*store.PRUpdate)) {
	if job.postMerge {
		e.postMergeFailed(ctx, job, pr, from, why, msg, extra)
		return
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", msg)
		u.Set("forced", false)
		u.Set("next_attempt_at", nil)
		if extra != nil {
			extra(u)
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the round", "pr", pr.ID, "err", err)
		return
	}
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRRedecide(pr.ID)) // the forced request ended with it
	e.setKV(ctx, KVPRAttention(pr.ID), why)
	if job.hasSlot {
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed, store.SlotBusy}, store.SlotHeld, nil)
	}
	label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
	key := fmt.Sprintf("attention:%d:%s", pr.ID, why)
	reason := attention.Explain(why, msg, label)
	e.event(ctx, "warn", prSubject(job.repo, pr.Number), "pr.needs_attention", why+": "+msg,
		map[string]any{"stage": reason.Stage, "cause": reason.Cause, "fix": reason.Fix})
	e.urgent(key, "magnum: "+label+" needs attention", reason.Summary, attentionWindow)
	e.revealAttention(ctx, pr.ID, nil, key)
}

// finish maps a round result onto the PR state machine.
func (e *Engine) finish(ctx context.Context, job *roundJob, in pipeline.RoundInput, ws agents.Workspace, res pipeline.RoundResult, runErr error, cancelled bool) {
	subject := prSubject(job.repo, job.pr.Number)
	if res.TargetSHA != "" {
		in.TargetSHA = res.TargetSHA // the head of the round's last restart
	}
	if err := e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotBusy}, store.SlotHeld, nil); err != nil {
		// The next dispatch of the PR repairs a slot left busy (slotGate).
		e.log.Warn("round finished: slot back to held", "slot", job.slot.Name, "err", err)
	}
	e.delKV(ctx, kvPRFresh(job.pr.ID))
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("round finished and PR is gone", "pr", job.pr.ID, "err", err)
		return
	}
	outcome := res.Outcome
	if outcome == "" {
		outcome = pipeline.OutcomeError
	}
	switch {
	case cancelled, outcome == pipeline.OutcomeUsageLimit, outcome == pipeline.OutcomeLoginRequired, outcome == pipeline.OutcomeOverloaded:
		// The round goes on later (a recovery, a continue): it keeps its kind.
	default:
		e.delKV(ctx, KVPRDeltaCheck(pr.ID))
	}
	msg := res.Error
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	e.event(ctx, "info", subject, "engine.round_result", fmt.Sprintf("round %d: %s %s", res.Round, outcome, res.Event),
		map[string]any{"outcome": outcome, "event": res.Event, "review_id": res.ReviewID, "target_sha": in.TargetSHA,
			"restarts": res.Restarts, "error": msg})
	if !cancelled { // a judge a shutdown left working records its notes with the round that continues it
		e.recordRoundNotes(ctx, job, in, res) // notes_record.go
	}
	e.noteStalemates(ctx, job.repo, pr, res) // stalemate.go

	// Role health → pauses of the roles' agent kinds (the round itself may
	// have posted). A model limit never pauses a kind: the pipeline switched
	// models, and reports a usage limit when no fallback was left.
	paused := map[string]bool{}
	for _, role := range slices.Sorted(maps.Keys(res.Reports)) {
		rep := res.Reports[role]
		if rep.Status != string(agents.HealthLoginRequired) && rep.Status != string(agents.HealthUsageLimit) {
			continue
		}
		tool := e.reportKind(&job.watch, rep)
		if tool == "" || paused[tool] || (res.Pause != nil && res.Pause.Tool == tool) {
			continue
		}
		paused[tool] = true
		p := pipeline.Pause{Kind: rep.Status, Tool: tool, Detail: rep.Detail}
		if rep.Health != nil && rep.Health.ResetAt != nil {
			p.Until = *rep.Health.ResetAt
		}
		e.pauseTool(ctx, p)
	}
	if res.Pause != nil && res.Pause.Kind != string(agents.HealthOverloaded) && res.Pause.Kind != string(agents.HealthModelLimit) {
		e.pauseTool(ctx, *res.Pause) // overload is a per-PR backoff (below)
	}
	if why := e.refundReason(ctx, job, res, outcome, msg); why != "" && !e.turnContinues(ctx, job, pr, outcome, cancelled) {
		e.refund(ctx, job, res.Round, why, outcome)
	}
	if cancelled && outcome == pipeline.OutcomeStopped {
		e.log.Info("round stopped by shutdown; recovery re-evaluates it on the next start", "subject", subject)
		return
	}

	from := []string{store.PRReviewing, store.PRClaiming}
	now := e.now()
	switch outcome {
	case pipeline.OutcomeDryRun:
		e.onDryRun(ctx, job, pr, in, res, from)
	case pipeline.OutcomePosted:
		e.onPosted(ctx, job, pr, in, res, from)
	case pipeline.OutcomeReplied:
		e.onReplied(ctx, job, pr, in, res, from) // replies.go
	case pipeline.OutcomeUsageLimit, pipeline.OutcomeLoginRequired:
		e.toPaused(ctx, pr, from, outcome+": "+msg, time.Time{}, nil)
	case pipeline.OutcomeOverloaded:
		attempts := pr.Attempts + 1
		setAttempts := func(u *store.PRUpdate) { u.Set("attempts", attempts) }
		if attempts >= maxAttempts {
			e.needsAttention(ctx, job, pr, from, outcome, msg, setAttempts)
			break
		}
		e.toPaused(ctx, pr, from, outcome+": "+msg, now.Add(backoff(attempts)), setAttempts)
	case pipeline.OutcomeRefused: // the PR is flagged: never reviewed again (codex_flag.go)
		e.onRefused(ctx, job, pr, in, res, from)
	case pipeline.OutcomeBlocked, pipeline.OutcomeNeedsAttention:
		e.keptApprovalFailed(ctx, job.repo, pr, "its round ended "+outcome)
		e.needsAttention(ctx, job, pr, from, outcome, msg, nil)
	case pipeline.OutcomeIdentityError:
		e.keptApprovalFailed(ctx, job.repo, pr, "its round ended "+outcome)
		e.setKV(ctx, KVIdentityCheck(pr.Identity), "fail")
		e.setKV(ctx, KVIdentityError(pr.Identity), "the judge's identity check failed: "+msg)
		e.needsAttention(ctx, job, pr, from, outcome, msg, nil)
	case pipeline.OutcomeIdentityLeak:
		e.keptApprovalFailed(ctx, job.repo, pr, "its round ended "+outcome)
		// msg is the pipeline's sentence: review N carrying the run's marker was posted as "x", not "y".
		e.setKV(ctx, KVWatchPaused(job.repo.WatchOwner), fmt.Sprintf("identity leak on %s#%d: %s", job.repo.FullName(), pr.Number, msg))
		e.needsAttention(ctx, job, pr, from, outcome, msg, nil)
		e.urgent(fmt.Sprintf("leak:%d", pr.ID), "magnum: IDENTITY LEAK on "+job.repo.Name+fmt.Sprintf("#%d", pr.Number),
			msg+". Automation for "+job.repo.WatchOwner+" is paused (magnum resume --watch "+job.repo.WatchOwner+").", 0)
	case pipeline.OutcomeClosed:
		if job.postMerge { // merged: no poll will ever close it again
			e.postMergeFailed(ctx, job, pr, from, outcome, "the judge found the PR closed: "+msg, nil)
			break
		}
		// confirmMissing closes the PR if it really closed; until then it
		// waits closedRecheck in line (reviewed only when its head was).
		to := claimableState(pr)
		if deref(pr.ReviewedSHA) == pr.HeadSHA {
			to = store.PRReviewed
		}
		_ = e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
			u.Set("next_attempt_at", now.Add(closedRecheck))
			u.Set("last_error", "the judge found the PR closed")
		})
	case pipeline.OutcomeStopped:
		e.retryOrAttention(ctx, job, pr, from, claimableState(pr), "round stopped: "+msg, false, now.Add(stopRetryDelay))
	default: // timeout, error
		if errors.Is(runErr, agents.ErrHumanActive) {
			e.retryOrAttention(ctx, job, pr, from, claimableState(pr), msg, false, classifySetup(runErr, pr, e.cfg, now).retryAt)
			break
		}
		e.keptApprovalFailed(ctx, job.repo, pr, "its round ended "+outcome)
		e.retryOrAttention(ctx, job, pr, from, claimableState(pr), outcome+": "+msg, true, time.Time{})
	}
	e.requestedRoundFailed(ctx, job, pr, outcome, msg, runErr) // operator.go
	token := outcome
	if res.Event != "" {
		token = res.Event
	}
	e.sidebar(ctx, ws.WorkspaceID, map[string]string{"magnum": token + " " + textx.ShortSHA(in.TargetSHA)})
}

// onPosted records a verified review and queues the next one when the head
// moved during the round (noteMovedHead tells the review's readers). The PR
// becomes reviewed only while its head is still the reviewed commit (the
// transition is guarded on head_sha); a push the poller recorded after the
// read below fails that guard and the PR waits for a re-review of the new
// head instead. A post-merge review puts the PR back in closed, released
// after a fresh close grace: a merged head never moves, and the round's
// target is the merged head GitHub keeps (head_sha follows it when the
// poller never saw the last push).
func (e *Engine) onPosted(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult, from []string) {
	now := e.now()
	target := in.TargetSHA
	to := store.PRReviewed
	if job.postMerge {
		to = store.PRClosed
	}
	// reviewed is the commit the review stands for: the round's target, or
	// the head that moved during the round when the delta is trivial
	// (comments, whitespace, docs, a base merge: no re-review follows).
	reviewed := target
	var trivial []string
	var settled deltaCheck
	var next time.Time
	if !job.postMerge {
		m := e.settleHead(ctx, job, pr, target, now)
		reviewed, trivial, settled, to, next = m.reviewed, m.trivial, m.settled, m.to, m.next
	}
	// simplify_done (shown by the board) follows the role answering to
	// "simplify" (claude-simplify by default).
	simplified := false
	if r, ok := e.cfg.RoleByNameOrAlias(&job.watch, "simplify"); ok {
		if rep, ok := res.Reports[agents.Role(r.Name)]; ok {
			simplified = rep.Status == pipeline.ReportOK || rep.Status == ""
		}
	}
	login := e.reviewerLogin(pr.Identity)
	readAt := repliesReadAt(in.Kind, res, pr.LastRoundStartedAt, now)
	recordReview := func(u *store.PRUpdate) {
		u.Set("replies_read_at", readAt)
		u.Set("reviewed_sha", reviewed)
		if res.ReviewID != 0 {
			u.Set("last_review_id", res.ReviewID)
		}
		if res.Event != "" {
			u.Set("last_review_event", res.Event)
		}
		u.Set("reviewed_at", now)
		u.Set("last_review_login", login)
	}
	// set reads pr, to and next when it runs: the head-moved retry below
	// reassigns them.
	set := func(u *store.PRUpdate) {
		if to == store.PRReviewed {
			u.Where("head_sha", reviewed)
		}
		recordReview(u)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		u.Set("forced", false)
		u.Set("skip_reason", nil)
		if simplified {
			u.Set("simplify_done", true)
		}
		if to == store.PRRereviewPending {
			u.Set("next_eligible_at", next)
			if pr.PendingSince == nil {
				u.Set("pending_since", pr.HeadChangedAt)
			}
		} else {
			u.Set("next_eligible_at", nil)
		}
		if job.postMerge {
			u.Set("release_after", e.releaseAfter())
			if pr.HeadSHA != reviewed {
				u.Set("head_sha", reviewed)
			}
		}
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, set)
	if errors.Is(err, store.ErrConflict) && to == store.PRReviewed {
		// The head moved after the read above: this review covers an
		// older head, so the PR waits for a re-review of the new one.
		if cur, rerr := e.st.PRByID(ctx, pr.ID); rerr == nil && slices.Contains(from, cur.State) && cur.HeadSHA != reviewed {
			pr, to, next = cur, store.PRRereviewPending, e.rereviewAt(ctx, cur, job.watch, target, now)
			reviewed, trivial = target, nil
			err = e.st.TransitionPR(ctx, pr.ID, from, to, set)
		}
	}
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			e.log.Warn("record review", "pr", pr.ID, "err", err)
			return
		}
		// The PR closed during the round: keep the review on record anyway.
		if err := e.st.UpdatePR(ctx, pr.ID, recordReview); err != nil {
			e.log.Warn("record review", "pr", pr.ID, "err", err)
		}
	} else if to == store.PRReviewed {
		e.requeueMovedHead(ctx, job.watch, pr.ID, now)
		if trivial != nil {
			e.recordTrivial(ctx, job.repo, pr, TrivialSkip{From: target, To: reviewed, Classes: trivial, Files: settled.files, At: now},
				fmt.Sprintf("the push to %s during the review %s since %s; no re-review, the review stands",
					textx.ShortSHA(reviewed), settled.change(), textx.ShortSHA(target)))
			e.noteMovedHead(ctx, job, pr, target, res.ReviewID, trivial)
		}
	} else if to == store.PRRereviewPending {
		e.noteMovedHead(ctx, job, pr, target, res.ReviewID, nil)
	} else {
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "pr.post_merge_reviewed",
			fmt.Sprintf("post-merge review %s posted on %s; closed again, slot released after %s", reviewSummary(res), textx.ShortSHA(reviewed),
				e.cfg.Daemon.CloseGrace.Duration),
			map[string]any{"review_id": res.ReviewID, "event": res.Event, "reviewed_sha": reviewed, "previous_sha": deref(pr.ReviewedSHA)})
		if res.Event != "" && res.Event != "COMMENTED" { // the judge was asked for a comment: say so, undo nothing
			e.event(ctx, "warn", prSubject(job.repo, pr.Number), "pr.post_merge_event",
				fmt.Sprintf("post-merge review %d was posted as %s, not as a comment; it stands as posted", res.ReviewID, res.Event),
				map[string]any{"review_id": res.ReviewID, "event": res.Event})
		}
	}
	if trivial == nil {
		e.delKV(ctx, KVPRTrivial(pr.ID)) // a review of the head itself replaces the note
	}
	e.settleKeptApproval(ctx, job.repo, pr, target, res)
	e.dismissFormer(ctx, job, pr, target, res)
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRRedecide(pr.ID)) // the forced request is served
	// The request is served; the kinds that just worked lose their backoff.
	e.clearRequested(ctx, pr.ID)
	if to == store.PRReviewed && err == nil { // replies that came after the judge read them
		if err := e.considerReplies(ctx, job.repo, job.watch, pr.ID, now); err != nil {
			e.log.Info("replies after the round", "pr", pr.ID, "err", err)
		}
	}
	if k := e.cfg.JudgeFor(&job.watch).AgentKind(); k != "" {
		e.delKV(ctx, kvToolBackoff(k))
	}
	for _, role := range slices.Sorted(maps.Keys(res.Reports)) {
		rep := res.Reports[role]
		if rep.Status != pipeline.ReportOK {
			continue
		}
		if k := e.reportKind(&job.watch, rep); k != "" {
			e.delKV(ctx, kvToolBackoff(k))
		}
	}
	switch {
	case pr.Forced: // the operator asked for it: always toasted (operator.go)
		e.toastRequestedPosted(job, pr, res, now)
	case e.cfg.Herdr.ToastEveryReview && e.batch != nil:
		label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
		title := label + ": " + reviewSummary(res)
		e.batch.Add(notify.Item{
			Key:   fmt.Sprintf("%s#%d@%s", job.repo.FullName(), pr.Number, textx.ShortSHA(target)),
			Title: title, Body: "by " + e.reviewerLogin(pr.Identity), Line: title,
			Kind: notify.KindReviewPosted,
		})
	}
}

// headSettle is where a finished round leaves its PR (settleHead).
type headSettle struct {
	reviewed string     // the commit the review stands for: the round's target, or the head after a trivial delta
	trivial  []string   // the trivial delta's classes (nil: none)
	settled  deltaCheck // the trivial delta's measure
	to       string     // reviewed, or rereview_pending
	next     time.Time  // rereview_pending: when the re-review may start
}

// settleHead settles a round on target whose PR's head may have moved
// while it ran (a posted round and a reply round alike): a trivial delta
// since target (checkDelta: comments, whitespace, docs, a base merge) leaves
// the review standing for the head, reviewed; any other is recorded for the
// re-review's threshold and delta check (recordDelta), and the PR waits in
// rereview_pending until rereviewAt. A head still at target stays
// reviewed.
func (e *Engine) settleHead(ctx context.Context, job *roundJob, pr store.PR, target string, now time.Time) headSettle {
	s := headSettle{reviewed: target, to: store.PRReviewed}
	if pr.HeadSHA == target {
		return s
	}
	dc := e.checkDelta(ctx, job.repo, job.watch, prBase(job.repo, pr), target, pr.HeadSHA)
	if dc.trivial {
		s.reviewed, s.trivial, s.settled = pr.HeadSHA, dc.classes, dc
		return s
	}
	e.recordDelta(ctx, pr.ID, target, pr.HeadSHA, dc, pr.HeadChangedAt)
	s.to, s.next = store.PRRereviewPending, e.rereviewAt(ctx, pr, job.watch, target, now)
	return s
}

// rereviewAt is when a PR whose review covered target (not its head) may
// get its next round: eligibility.Throttle with target as the reviewed
// commit and the push quiet period (burst-aware) counted from the head's
// last change. The commits arrived during the review, so the re-review
// interval does not hold it (dispatch's backstop agrees,
// arrivedDuringReview); the daily cap still does.
func (e *Engine) rereviewAt(ctx context.Context, pr store.PR, w config.Watch, target string, now time.Time) time.Time {
	f := e.factsFor(ctx, pr, w, now)
	f.ReviewedSHA = target
	f.LastRoundStartedAt = time.Time{}
	if f.PendingSince.IsZero() {
		f.PendingSince = pr.HeadChangedAt
	}
	return e.throttle(ctx, w, pr, f, now).NextEligibleAt
}

// noteMovedHead appends a line to the review the round posted when commits
// arrived during it (GitHub's compare of the reviewed commit and the head
// confirms them; after a merge of the base branch they are counted as the
// PR's own, rangeMeasure.commits): which commit it covers and that a
// re-review follows, or, when the commits were trivial (classes, see
// TrivialDelta), what they changed and that none is needed. Best effort: an
// identity that cannot edit its own review leaves it as posted (a warning
// event).
func (e *Engine) noteMovedHead(ctx context.Context, job *roundJob, pr store.PR, target string, reviewID int64, trivial []string) {
	if e.d.DryRun || reviewID == 0 || e.d.Rounds == nil || pr.HeadSHA == target {
		return
	}
	rounds := e.d.Rounds(job.pr.Identity)
	if rounds == nil {
		return
	}
	// Only commits GitHub confirms: a head the poller has not caught up
	// with yet (the round fetched a newer one) adds none.
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return
	}
	m, err := e.measureRange(ctx, gh, job.repo, prBase(job.repo, pr), target, pr.HeadSHA) // checkDelta's comparisons, when it made them
	if err != nil || m.push.Commits == 0 {
		if err != nil {
			e.log.Info("note on the review: compare", "pr", pr.ID, "err", err)
		}
		return
	}
	n := m.commits()
	what, are := fmt.Sprintf("%d commits", n), "are"
	if n == 1 {
		what, are = "1 commit", "is"
	}
	// The re-review is promised only when it starts by itself
	// (rereviewFollows); a delta check of the commits is named as one.
	var follows, line string
	if trivial != nil {
		follows = DeltaLabel(trivial) + ", no re-review needed"
		line = fmt.Sprintf("_Reviewed %s; %s arrived during the review (%s), no re-review needed._", textx.ShortSHA(target), what, DeltaLabel(trivial))
	} else if when, check, ok := e.rereviewFollows(ctx, job.watch, pr.ID); !ok {
		follows = "not reviewed yet"
		line = fmt.Sprintf("_Reviewed %s; %s arrived during the review and %s not reviewed yet._", textx.ShortSHA(target), what, are)
	} else {
		follows = "re-review follows"
		if check {
			follows = "a short check of " + map[bool]string{true: "that commit", false: "those commits"}[n == 1] + " follows"
		}
		if when != "" {
			follows += " " + when
		}
		line = fmt.Sprintf("_Reviewed %s; %s arrived during the review, %s._", textx.ShortSHA(target), what, follows)
	}
	subject := prSubject(job.repo, pr.Number)
	if err := rounds.AppendToReview(ctx, job.repo.Owner, job.repo.Name, pr.Number, reviewID, line); err != nil {
		e.event(ctx, "warn", subject, "round.review_note_failed",
			fmt.Sprintf("review %d: could not add that %s arrived during it: %v", reviewID, what, err), nil)
		return
	}
	e.event(ctx, "info", subject, "round.review_noted", fmt.Sprintf("review %d: %s arrived during it, %s", reviewID, what, follows),
		map[string]any{"review_id": reviewID, "reviewed_sha": target, "head_sha": pr.HeadSHA, "trivial": trivial})
}

// rereviewFollows reports whether the re-review of the commits that arrived
// during a review starts by itself, from the PR's wait as it is right after
// the review was recorded (waitFor), when: "" at the next dispatch, "after
// the quiet period" when the push quiet period (or its burst form) holds it,
// and whether it is a delta check (check). It does not when a longer timing
// rule holds the PR (the daily cap, the small-delta threshold, an interval),
// or when something would hold it once its timing clears: `magnum pause`, a
// drain or an infrastructure pause, quiet hours (not a delta check), a mute,
// an agent kind the round's roles use paused (a delta check's judge alone).
func (e *Engine) rereviewFollows(ctx context.Context, w config.Watch, prID int64) (when string, check, ok bool) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil || (pr.State != store.PRRereviewPending && pr.State != store.PRQueued) {
		return "", false, false
	}
	now := e.now()
	wait := e.waitFor(ctx, pr, e.globalWait(ctx, tickState{herdrUp: true}, now), now)
	switch wait.Reason {
	case WaitNext, WaitRequested, WaitCapacity:
	case WaitQuiet, WaitBurst:
		when = "after the quiet period"
	default:
		return "", false, false
	}
	at := now // when the quiet period ends (quiet hours then hold it)
	if wait.Until.After(at) {
		at = wait.Until
	}
	// The roles dispatch would run (their kinds' pauses hold the round).
	roles := e.cfg.RolesFor(&w)
	if toRun, err := pipeline.RolesToRun(ctx, e.st, e.cfg, pr, roles, e.requestedRoles(ctx, pr.ID), kindFor(pr)); err == nil {
		roles = toRun
	}
	if wait.DeltaCheck {
		roles = judgeAlone(roles)
	}
	switch {
	case e.holdReason(ctx) != "":
	case !pr.Forced && (e.userPause(ctx) != "" || pr.Muted || quietHoursHold(e.cfg.Daemon.QuietHours, at, wait.DeltaCheck)):
	case e.kindPauseReason(ctx, agentKinds(roles)) != "":
	default:
		return when, wait.DeltaCheck, true
	}
	return "", false, false
}

// requeueMovedHead is the backstop after onPosted's transition to reviewed
// (whose head_sha guard already sends a head moved before it to
// rereview_pending): a reviewed PR whose head is not the reviewed commit
// goes to rereview_pending (a lost compare-and-set means the poller queued
// it already).
func (e *Engine) requeueMovedHead(ctx context.Context, w config.Watch, prID int64, now time.Time) {
	cur, err := e.st.PRByID(ctx, prID)
	if err != nil || cur.State != store.PRReviewed || cur.HeadSHA == deref(cur.ReviewedSHA) {
		return
	}
	if err := e.queue(ctx, cur, w, []string{store.PRReviewed}, false, now, "head moved during the round"); err != nil {
		e.log.Info("requeue a head that moved during the round", "pr", prID, "err", err)
	}
}

// reportKind is the agent kind a role report's health concerns: its Kind,
// else the configured role's (config.Role.AgentKind); "" = none.
func (e *Engine) reportKind(w *config.Watch, rep pipeline.RoleReport) string {
	if rep.Kind != "" {
		return rep.Kind
	}
	if r, ok := e.cfg.RoleByNameOrAlias(w, rep.Role); ok && rep.Role != "" {
		return r.AgentKind()
	}
	return ""
}

// onDryRun ends a dry-run round (review request with dry_run): nothing was
// posted, so reviewed_sha stays and the PR returns to the state it had
// before the request (its claimable state when that was in flight, the head
// moved meanwhile, or reviewed no longer describes it). A post-merge dry run
// returns to closed, released after a fresh close grace.
func (e *Engine) onDryRun(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult, from []string) {
	to, _ := e.getKV(ctx, kvPRDryRun(pr.ID))
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRRedecide(pr.ID)) // the forced request ended with it
	switch {
	case job.postMerge:
		to = store.PRClosed
	case to == "" || pr.HeadSHA != in.TargetSHA || to == store.PRPaused ||
		slices.Contains(store.InFlightStates, to) || slices.Contains(closingStates, to) ||
		(to == store.PRReviewed && deref(pr.ReviewedSHA) != pr.HeadSHA):
		to = claimableState(pr)
		if rs := deref(pr.ReviewedSHA); rs != "" && rs == pr.HeadSHA {
			to = store.PRReviewed
		}
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
		u.Set("forced", false)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		if job.postMerge {
			u.Set("release_after", e.releaseAfter())
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the dry-run round", "pr", pr.ID, "err", err)
	}
	e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.dry_run",
		fmt.Sprintf("dry run at %s: would post %s; nothing was posted (PR back to %s)", textx.ShortSHA(in.TargetSHA), reviewSummary(res), to),
		map[string]any{"event": res.Event, "findings": res.Findings, "report_dir": res.ReportDir})
}

// toPaused parks a PR whose round hit a tool pause; it continues (kind
// continue) once the pause ends. extra adds assignments to the same
// transition.
func (e *Engine) toPaused(ctx context.Context, pr store.PR, from []string, msg string, retryAt time.Time, extra func(*store.PRUpdate)) {
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRPaused, func(u *store.PRUpdate) {
		u.Set("last_error", msg)
		u.Set("next_attempt_at", retryAt)
		if extra != nil {
			extra(u)
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the round", "pr", pr.ID, "err", err)
	}
}

func (e *Engine) reviewerLogin(identityName string) string {
	if id := e.cfg.IdentityByName(identityName); id != nil {
		return id.Login
	}
	return identityName
}

// reviewSummary is "CHANGES_REQUESTED (2 P2)" for a toast.
func reviewSummary(res pipeline.RoundResult) string {
	var parts []string
	for _, k := range []string{"P0", "P1", "P2", "P3"} {
		if n := res.Findings[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	ev := res.Event
	if ev == "" {
		ev = res.Outcome
	}
	if len(parts) == 0 {
		return ev
	}
	return ev + " (" + strings.Join(parts, ", ") + ")"
}
