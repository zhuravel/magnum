package engine

// A judge's late review is adopted (DECISIONS "A judge's late review puts
// its PR back in line at once" and "The next round on the head adopts the
// late review of a judge whose round gave up on its result"): on 2026-11-05
// a re-review's judge ended its turn and its nudge's without its result, so
// the round ended needing attention; the judge worked on and posted its
// review with the round's marker 15 minutes later, and the PR stayed parked
// as failed, its status comment saying so, until a push 45 minutes later.
// Once the judge's result file holds that review, the health check puts the
// PR back in line, and its next round adopts the review (the pipeline's
// findUnverified), prompting nothing.

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// eventLateResult is recorded (info) under a PR that went back in line
// because its judge's late result landed (checkLateResults): the round, the
// review and how long after the round (data round, review_id, runs).
const eventLateResult = "round.late_result"

// kvPRLateResult holds the PR's late result (lateResult, JSON) from its
// return to the line until its next round ends (finish).
func kvPRLateResult(prID int64) string { return fmt.Sprintf("pr.%d.late_result", prID) }

// lateResult is a judge's late result for a PR's head: the head, the round
// that gave up on it, the review the judge posted and when magnum found it.
type lateResult struct {
	Head   string    `json:"head"`
	Round  int       `json:"round"`
	Review int64     `json:"review_id"`
	At     time.Time `json:"at"`
}

// lateResultWait is when the PR's late result for its head was found
// (eligibility.PRFacts.LateAt): its next round adopts the review, so the
// re-review rules hold it no longer; ok is false when none waits.
func (e *Engine) lateResultWait(ctx context.Context, pr store.PR) (time.Time, bool) {
	rec, ok, _ := readKVJSON[lateResult](ctx, e, kvPRLateResult(pr.ID), "late result")
	if !ok || rec.Head != pr.HeadSHA {
		return time.Time{}, false
	}
	return rec.At, true
}

// checkLateResults puts back in line, at each health check, every PR a
// round parked in needs attention because its judge stopped without its
// result, also after a nudge (agents.JudgeNoResult), whose judge has since
// written its result file with a posted review for that round's runs on the
// PR's head (pipeline.LateReview), within the judge's timeout of the round's
// end (gaveUpRuns): back where it waits for a round, at once and past the
// re-review rules (lateResultWait), its attempts as they are, its attention
// cleared, with one round.late_result info event. A result for another
// head, one the judge wrote after its timeout and one that is no posted
// review leave the PR parked.
func (e *Engine) checkLateResults(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRNeedsAttention}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the PRs that need attention")
		return
	}
	now := e.now()
	for _, pr := range prs {
		if !strings.HasPrefix(deref(pr.LastError), agents.JudgeNoResult) || e.roundActive(pr.ID) {
			continue
		}
		if why, _ := e.getKV(ctx, KVPRAttention(pr.ID)); why != pipeline.OutcomeNeedsAttention {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil || e.cfg.WatchFor(repo.FullName()) == nil {
			continue
		}
		path, ids, last, ok := e.gaveUpRuns(ctx, repo, pr, now)
		if !ok {
			continue
		}
		review, ok := pipeline.LateReview(path, ids...)
		if !ok {
			continue
		}
		if err := writeKVJSON(ctx, e, kvPRLateResult(pr.ID), "late result", lateResult{Head: pr.HeadSHA, Round: last.Round, Review: review, At: now}); err != nil {
			e.warnUnlessStopped(ctx, err, "record the judge's late result", "pr", pr.ID)
			continue
		}
		after := humanDuration(now.Sub(*last.EndedAt).Round(time.Minute))
		to := claimableState(pr)
		err = e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, to, func(u *store.PRUpdate) {
			u.Where("head_sha", pr.HeadSHA)
			u.Set("next_attempt_at", now)
			u.Set("next_eligible_at", now)
			u.Set("last_error", fmt.Sprintf("back in line: the judge posted review %d of round %d on %s %s after the round ended without its result; "+
				"the next round adopts it", review, last.Round, textx.ShortSHA(pr.HeadSHA), after))
		})
		if err != nil {
			e.delKV(ctx, kvPRLateResult(pr.ID))
			e.log.Info("put a PR back in line for its judge's late review: the PR moved on", "pr", pr.ID, "err", err)
			continue
		}
		e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID))
		e.event(ctx, "info", prSubject(repo, pr.Number), eventLateResult,
			fmt.Sprintf("the judge posted review %d of round %d on %s %s after the round ended without its result: back in line (%s → %s), "+
				"and the next round adopts the review, prompting nothing", review, last.Round, textx.ShortSHA(pr.HeadSHA), after, store.PRNeedsAttention, to),
			map[string]any{"round": last.Round, "review_id": review, "runs": ids, "to": to})
	}
}

// gaveUpRuns are the PR's judge runs on its head whose round ended without
// the judge's result (agents.JudgeNoResult), newest first and back to the
// judge's last run verified as posted, as the pipeline's findUnverified
// reads them: the newest one's result file (its report path, else the
// judge's report file in the head's review directory), their ids, and the
// newest one. ok is false for none, and once the judge's timeout has passed
// since the newest one ended.
func (e *Engine) gaveUpRuns(ctx context.Context, repo store.Repo, pr store.PR, now time.Time) (path string, ids []string, last store.Run, ok bool) {
	runs, err := e.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the PR's judge runs", "pr", pr.ID)
		return "", nil, store.Run{}, false
	}
	for _, r := range slices.Backward(runs) {
		if !e.isJudge(r.Role) {
			continue
		}
		if r.State == store.RunVerified && deref(r.Outcome) == pipeline.OutcomePosted {
			break
		}
		if r.State != store.RunFailed || r.TargetSHA != pr.HeadSHA || !strings.HasPrefix(deref(r.Error), agents.JudgeNoResult) {
			continue
		}
		if ids = append(ids, r.ID); len(ids) == 1 {
			last = r
		}
	}
	if len(ids) == 0 || last.EndedAt == nil {
		return "", nil, store.Run{}, false
	}
	judge, found := e.cfg.RoleByNameOrAlias(nil, last.Role)
	if !found || !now.Before(last.EndedAt.Add(judge.Timeout.Duration)) {
		return "", nil, store.Run{}, false
	}
	path = deref(last.ReportPath)
	if path == "" {
		path = filepath.Join(e.d.Layout.ReviewDir(repo.Owner, repo.Name, pr.Number, pr.HeadSHA), judge.ReportFile())
	}
	return path, ids, last, true
}
