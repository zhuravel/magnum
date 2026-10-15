package engine

// Scheduled full passes (DECISIONS "Every tenth re-review of a PR is a full
// pass"): after its first review nothing read a long-lived PR whole again. A
// re-review reads the commits since the last review, in the judge's
// conversation or, when that is gone, through the recovery prompt, which
// reads the earlier reviews and then the new commits too. On a PR of 268
// files open for months a colleague's whole-branch review found three
// latent defects in PR code outside the delta that magnum's re-review of
// the same head had just passed. So the Nth automatic re-review of a PR
// ([pipeline] full_pass_every), counted since its first review or its last
// full pass, runs as a full pass: every role starts in a fresh session (as
// `magnum review --fresh` does), the reviewers review the whole PR with
// their first-review prompts and effort, and the judge reviews the full
// diff again through the recovery prompt with the full-pass clause
// (pipeline.RoundInput.FullPass), then decides every earlier thread.
//
// The count reads the runs table: the PR's rereview and recovery rounds
// that posted a review on a head other than the one reviewed before them,
// numbered after the last full pass (kvPRFullPass), so a PR reviewed before
// this rule counts its past re-reviews. A delta check counts but never
// becomes one, nor does a re-review of the same head, a reply round, a CI
// check, or a round someone asked for (forced or requested; those count, as
// the runs table cannot tell them apart); `magnum review --full` asks for
// one by hand (store.KVPRFresh = FreshFullPass).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRFullPass holds the PR's last full pass that posted (fullPassRecord as
// JSON): the re-reviews of later rounds count toward the next one.
func kvPRFullPass(prID int64) string { return fmt.Sprintf("pr.%d.full_pass", prID) }

// kvPRFullPassRound is "1" while the PR's round in flight is a full pass
// (the status comment's in-progress line, and a continue of a paused full
// pass, which records it when it posts); the round's end deletes it, a pause
// keeps it for the continue.
func kvPRFullPassRound(prID int64) string { return fmt.Sprintf("pr.%d.full_pass_round", prID) }

// fullPassRecord is a full pass that posted (kvPRFullPass).
type fullPassRecord struct {
	Run    string    `json:"run"`    // the judge's run, whose id the review's marker carries
	Round  int       `json:"round"`  // the round's number among the PR's runs
	Target string    `json:"target"` // the commit it reviewed
	At     time.Time `json:"at"`
}

// fullReReviewLabel is how the round's start event names a full pass.
const fullReReviewLabel = "re-review (full pass)"

// fullPassDue reports why job, a re-review the dispatcher plans, runs as a
// full pass ("" = it does not): `magnum review --full` asked for one, or
// full_pass_every is set and this automatic full re-review is the Nth since
// the PR's first review or its last full pass (rereviewsSinceFullPass). A
// forced or requested round, a post-merge review, a delta check, a
// re-review of the same head, a reply round and a CI check are none.
func (e *Engine) fullPassDue(ctx context.Context, job *roundJob) string {
	pr := job.pr
	if job.kind != pipeline.KindRereview || postMerge(pr) {
		return ""
	}
	if v, _ := e.getKV(ctx, store.KVPRFresh(pr.ID)); v == store.FreshFullPass {
		return "requested by magnum review --full"
	}
	every := e.cfg.Pipeline.FullPassEvery
	if every <= 0 || pr.Forced || job.requested || job.judgeOnly() {
		return ""
	}
	n, last := e.rereviewsSinceFullPass(ctx, pr.ID)
	if n+1 < every {
		return ""
	}
	since := "the first review"
	if last.Round > 0 {
		since = fmt.Sprintf("the full pass of round %d", last.Round)
	}
	return fmt.Sprintf("re-review %d since %s (full_pass_every %d)", n+1, since, every)
}

// rereviewsSinceFullPass counts PR prID's re-reviews since its last full
// pass (last; the zero record when none posted): its rounds of kind
// rereview or recovery whose judge posted a review on a head other than the
// one reviewed before them (no same-head, reply or CI-red round), numbered
// after last's round. A delta check and a round someone asked for count
// too. A read error counts none, so no round becomes a full pass by it.
func (e *Engine) rereviewsSinceFullPass(ctx context.Context, prID int64) (int, fullPassRecord) {
	last, _ := e.lastFullPass(ctx, prID)
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "full pass: runs", "pr", prID)
		return 0, last
	}
	rounds := map[int]bool{}
	for _, r := range runs {
		prev := deref(r.PrevReviewedSHA)
		switch {
		case r.Round <= last.Round, r.Kind != store.RunRereview && r.Kind != store.RunRecovery,
			deref(r.ReviewID) == 0, prev == "", prev == r.TargetSHA:
			continue
		}
		rounds[r.Round] = true
	}
	return len(rounds), last
}

// lastFullPass is the PR's last full pass that posted (kvPRFullPass); ok
// is false when none did, or its record cannot be read.
func (e *Engine) lastFullPass(ctx context.Context, prID int64) (fullPassRecord, bool) {
	v, ok := e.getKV(ctx, kvPRFullPass(prID))
	var rec fullPassRecord
	if !ok || json.Unmarshal([]byte(v), &rec) != nil {
		return fullPassRecord{}, false
	}
	return rec, true
}

// fullPassRunning reports whether the PR's round in flight is a full pass
// (kvPRFullPassRound).
func (e *Engine) fullPassRunning(ctx context.Context, prID int64) bool {
	v, _ := e.getKV(ctx, kvPRFullPassRound(prID))
	return v == "1"
}

// noteFullPassRound records whether the PR's round runs a full pass
// (kvPRFullPassRound).
func (e *Engine) noteFullPassRound(ctx context.Context, prID int64, on bool) {
	if on {
		e.setKV(ctx, kvPRFullPassRound(prID), "1")
		return
	}
	e.delKV(ctx, kvPRFullPassRound(prID))
}

// recordFullPass records a full pass that posted (kvPRFullPass): its judge
// run, round and target, from which the next count starts.
func (e *Engine) recordFullPass(ctx context.Context, prID int64, res pipeline.RoundResult, target string) {
	b, err := json.Marshal(fullPassRecord{Run: res.JudgeRunID, Round: res.Round, Target: target, At: e.now().UTC()})
	if err != nil {
		return
	}
	e.setKV(ctx, kvPRFullPass(prID), string(b))
}
