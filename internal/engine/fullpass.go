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
// (pipeline.RoundInput.FullPass), then decides every earlier thread; its own
// pass reads no earlier review first.
//
// The count reads the runs table: the PR's rereview and recovery rounds
// that posted a review on a head other than the one reviewed before them,
// numbered after the last full pass (kvPRFullPass), so a PR reviewed before
// this rule counts its past re-reviews. A re-review of the same head, a
// reply round, a CI check and a round someone asked for (forced or
// requested; those count, as the runs table cannot tell them apart) never
// become one; `magnum review --full` asks for one by hand (store.KVPRFresh
// = FreshFullPass).
//
// Rounds measure how often the whole-PR view was lost, not how much the code
// changed (DECISIONS "A full pass also follows churn and age"): one round can
// replace a subsystem and ten can fix spelling. So a PR keeps a baseline, its
// last full pass or else its first review, with the PR's size then
// (prSize), and the churn since it (kvPRFullPassChurn): the lines and files
// of every review that posted on new commits since, measured as the
// re-review reads them. The next automatic re-review is also a full pass
// once that churn reaches full_pass_lines or full_pass_files, or a quarter of
// the PR's lines or files at the baseline (fullPassShare) when that is more,
// or once full_pass_after passed since the baseline of a large PR
// (fullPassLargeFiles, fullPassLargeLines). A due full pass overrides a
// delta check (planFullPass), which small pushes would otherwise renew
// without end.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRFullPass holds the PR's last full pass that posted (fullPassRecord as
// JSON): the re-reviews of later rounds count toward the next one, and it
// is the PR's baseline.
func kvPRFullPass(prID int64) string { return fmt.Sprintf("pr.%d.full_pass", prID) }

// kvPRFullPassRound is "1" while the PR's round in flight is a full pass
// (the status comment's in-progress line, and a continue of a paused full
// pass, which records it when it posts); the round's end deletes it, a pause
// keeps it for the continue.
func kvPRFullPassRound(prID int64) string { return fmt.Sprintf("pr.%d.full_pass_round", prID) }

// kvPRReviewBaseline holds the PR's baseline before its first full pass
// posts (fullPassRecord as JSON): its first review, or, for a PR reviewed
// before the churn rules, its first review that posted since.
func kvPRReviewBaseline(prID int64) string { return fmt.Sprintf("pr.%d.review_baseline", prID) }

// kvPRFullPassChurn holds the churn since the PR's baseline (fullPassChurn
// as JSON); a new baseline deletes it.
func kvPRFullPassChurn(prID int64) string { return fmt.Sprintf("pr.%d.full_pass_churn", prID) }

// The fixed parts of the churn and age rules: the churn must reach at least
// a fullPassShare-th of the PR's lines or files at the baseline, and only a
// PR of fullPassLargeFiles files or fullPassLargeLines lines then gets a
// full pass by age.
const (
	fullPassShare      = 4
	fullPassLargeFiles = 50
	fullPassLargeLines = 2000
	// maxChurnPaths caps the touched paths a churn keeps; past it only its
	// count grows.
	maxChurnPaths = 500
)

// fullPassRecord is a PR's baseline: a full pass that posted
// (kvPRFullPass), or the review before one (kvPRReviewBaseline).
type fullPassRecord struct {
	Run    string    `json:"run"`    // the judge's run, whose id the review's marker carries
	Round  int       `json:"round"`  // the round's number among the PR's runs
	Target string    `json:"target"` // the commit it reviewed
	At     time.Time `json:"at"`
	// Base is the commit the PR's size was measured from, and Files and
	// Lines that size (prSize; 0 when it could not be measured, and in a
	// full pass recorded before the churn rules).
	Base  string `json:"base,omitempty"`
	Files int    `json:"files,omitempty"`
	Lines int    `json:"lines,omitempty"`
	// Initial: a review baseline that is the PR's first review.
	Initial bool `json:"initial,omitempty"`
}

// fullPassChurn is what the PR's reviews on new commits changed since its
// baseline (kvPRFullPassChurn).
type fullPassChurn struct {
	Lines int      `json:"lines"`           // added plus deleted lines, summed over the reviews
	Files int      `json:"files"`           // the files they touched, each once while Paths has room
	Paths []string `json:"paths,omitempty"` // the first maxChurnPaths of those files, sorted
	To    string   `json:"to"`              // the last commit added, which is never added twice
}

// add adds a review's commits: lines, and the paths they touched.
func (c *fullPassChurn) add(lines int, paths []string) {
	c.Lines += lines
	for _, p := range paths {
		i, found := slices.BinarySearch(c.Paths, p)
		if found {
			continue
		}
		if len(c.Paths) < maxChurnPaths {
			c.Paths = slices.Insert(c.Paths, i, p)
		}
		c.Files++
	}
}

// fullReReviewLabel is how the round's start event names a full pass.
const fullReReviewLabel = "re-review (full pass)"

// fullPassWhy is why a re-review runs as a full pass: the rule that fired
// (the [pipeline] key, or "requested") and the reason the events give.
type fullPassWhy struct{ rule, text string }

// planFullPass decides whether job, a re-review the dispatcher plans, runs
// as a full pass (fullPassDue). One that does is no delta check: the round
// runs in full, however small the push.
func (e *Engine) planFullPass(ctx context.Context, job *roundJob) {
	why := e.fullPassDue(ctx, job)
	job.fullPass, job.fullPassRule = why.text, why.rule
	if why.text != "" {
		job.deltaCheck = false
	}
}

// fullPassDue reports why job, a re-review the dispatcher plans, runs as a
// full pass (the zero fullPassWhy = it does not), its rules in this order:
// `magnum review --full` asked for one; this automatic re-review is the
// full_pass_every-th since the PR's first review or its last full pass
// (rereviewsSinceFullPass); the churn since the baseline reached
// full_pass_lines lines or full_pass_files files, or a quarter of the PR's
// at the baseline when that is more; or the PR was large at the baseline
// and full_pass_after passed since. A delta check may become one; a forced
// or requested round, a post-merge review, a re-review of the same head, a
// reply round and a CI check never do.
func (e *Engine) fullPassDue(ctx context.Context, job *roundJob) fullPassWhy {
	pr := job.pr
	if job.kind != pipeline.KindRereview || postMerge(pr) {
		return fullPassWhy{}
	}
	if v, _ := e.getKV(ctx, store.KVPRFresh(pr.ID)); v == store.FreshFullPass {
		return fullPassWhy{"requested", "requested by magnum review --full"}
	}
	if pr.Forced || job.requested || job.sameHead || job.replies > 0 || job.ciRed {
		return fullPassWhy{}
	}
	p := e.cfg.Pipeline
	if every := p.FullPassEvery; every > 0 {
		if n, last := e.rereviewsSinceFullPass(ctx, pr.ID); n+1 >= every {
			since := "the first review"
			if last.Round > 0 {
				since = fmt.Sprintf("the full pass of round %d", last.Round)
			}
			return fullPassWhy{"full_pass_every", fmt.Sprintf("re-review %d since %s (full_pass_every %d)", n+1, since, every)}
		}
	}
	base, since, ok := e.fullPassBaseline(ctx, pr.ID)
	if !ok {
		return fullPassWhy{}
	}
	churn, _ := e.churnSince(ctx, pr.ID)
	if n := p.FullPassLines; n > 0 && churn.Lines >= max(n, base.Lines/fullPassShare) {
		return fullPassWhy{"full_pass_lines", fmt.Sprintf("%d lines changed since %s (full_pass_lines %d; a quarter of its %d lines then: %d)",
			churn.Lines, since, n, base.Lines, base.Lines/fullPassShare)}
	}
	if n := p.FullPassFiles; n > 0 && churn.Files >= max(n, base.Files/fullPassShare) {
		return fullPassWhy{"full_pass_files", fmt.Sprintf("%d files touched since %s (full_pass_files %d; a quarter of its %d files then: %d)",
			churn.Files, since, n, base.Files, base.Files/fullPassShare)}
	}
	if after := p.FullPassAfter.Duration; after > 0 && (base.Files >= fullPassLargeFiles || base.Lines >= fullPassLargeLines) {
		if age := e.now().Sub(base.At); age >= after {
			return fullPassWhy{"full_pass_after", fmt.Sprintf("%s since %s of a large PR (%d files, %d lines then; full_pass_after %s)",
				humanDuration(age.Truncate(time.Hour)), since, base.Files, base.Lines, humanDuration(after))}
		}
	}
	return fullPassWhy{}
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
	return e.readBaseline(ctx, kvPRFullPass(prID))
}

// readBaseline reads the fullPassRecord under key; ok is false when there is
// none, or it cannot be read.
func (e *Engine) readBaseline(ctx context.Context, key string) (fullPassRecord, bool) {
	v, ok := e.getKV(ctx, key)
	var rec fullPassRecord
	if !ok || json.Unmarshal([]byte(v), &rec) != nil {
		return fullPassRecord{}, false
	}
	return rec, true
}

// fullPassBaseline is the PR's baseline, the churn rules' starting point:
// its last full pass, else its review baseline (kvPRReviewBaseline), with
// how the reasons name it; ok is false when it has neither.
func (e *Engine) fullPassBaseline(ctx context.Context, prID int64) (rec fullPassRecord, since string, ok bool) {
	if rec, ok := e.lastFullPass(ctx, prID); ok {
		return rec, fmt.Sprintf("the full pass of round %d", rec.Round), true
	}
	if rec, ok := e.readBaseline(ctx, kvPRReviewBaseline(prID)); ok {
		if rec.Initial {
			return rec, "the first review", true
		}
		return rec, fmt.Sprintf("the review of round %d", rec.Round), true
	}
	return fullPassRecord{}, "", false
}

// churnSince is the PR's churn since its baseline (kvPRFullPassChurn); ok is
// false when none is recorded, or it cannot be read.
func (e *Engine) churnSince(ctx context.Context, prID int64) (fullPassChurn, bool) {
	v, ok := e.getKV(ctx, kvPRFullPassChurn(prID))
	var c fullPassChurn
	if !ok || json.Unmarshal([]byte(v), &c) != nil {
		return fullPassChurn{}, false
	}
	return c, true
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

// settleFullPass keeps a review that posted (onPosted) for the full-pass
// rules: a full pass becomes the PR's baseline, from which the next count
// and churn start (kvPRFullPass); the review of a PR without a baseline (its
// first review, or its first since the churn rules) becomes its review
// baseline (kvPRReviewBaseline); any other review of new commits adds them
// to the churn (addChurn). A post-merge review and an eval's replay keep
// nothing.
func (e *Engine) settleFullPass(ctx context.Context, job *roundJob, in pipeline.RoundInput, res pipeline.RoundResult) {
	if job.postMerge || job.evalHead != "" {
		return
	}
	prID, prev := in.PR.ID, deref(in.PR.ReviewedSHA)
	_, _, hasBase := e.fullPassBaseline(ctx, prID)
	switch {
	case in.FullPass:
		e.setBaseline(ctx, kvPRFullPass(prID), e.baselineOf(ctx, job, in, res))
		e.delKV(ctx, kvPRReviewBaseline(prID), kvPRFullPassChurn(prID))
	case !hasBase:
		rec := e.baselineOf(ctx, job, in, res)
		rec.Initial = prev == ""
		e.setBaseline(ctx, kvPRReviewBaseline(prID), rec)
		e.delKV(ctx, kvPRFullPassChurn(prID))
	case prev != "" && prev != in.TargetSHA:
		e.addChurn(ctx, job, prID, prev, in.TargetSHA)
	}
}

// baselineOf is the review a round posted as a baseline: its judge run,
// round, target and time, and the PR's size at the target (prSize).
func (e *Engine) baselineOf(ctx context.Context, job *roundJob, in pipeline.RoundInput, res pipeline.RoundResult) fullPassRecord {
	rec := fullPassRecord{Run: res.JudgeRunID, Round: res.Round, Target: in.TargetSHA, At: e.now().UTC()}
	rec.Base, rec.Files, rec.Lines = e.prSize(ctx, job, in)
	return rec
}

// setBaseline writes rec under key.
func (e *Engine) setBaseline(ctx context.Context, key string, rec fullPassRecord) {
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, key, string(b))
	}
}

// prSize measures the PR at the round's target in its checkout, from the
// round's merge base with the base branch (else origin/<base ref>): the
// files it changes and their added plus deleted lines (git diff --numstat;
// a binary file counts as a file), lockfiles and related_ignore's paths
// left out (fullPassSkip). base is "" and the size 0 when it cannot be
// measured (no git, no base, a failure).
func (e *Engine) prSize(ctx context.Context, job *roundJob, in pipeline.RoundInput) (base string, files, lines int) {
	base = in.BaseSHA
	if base == "" && in.BaseRef != "" {
		base = "origin/" + in.BaseRef
	}
	if e.d.Git == nil || base == "" || in.TargetSHA == "" || job.slot.Path == "" {
		return "", 0, 0
	}
	stats, err := e.d.Git.DiffStat(ctx, job.slot.Path, base, in.TargetSHA)
	if err != nil {
		e.log.Info("full pass: the PR's size cannot be measured; its baseline counts none", "pr", in.PR.ID, "err", err)
		return "", 0, 0
	}
	skip := e.fullPassSkip(&job.watch)
	for _, s := range stats {
		if !skip(s.Path) {
			files, lines = files+1, lines+s.Added+s.Removed
		}
	}
	return base, files, lines
}

// addChurn adds the commits a review of PR prID covered, prev to target, to
// its churn since the baseline (kvPRFullPassChurn), once: their lines and
// the files they touched (churnOf). Commits it cannot measure add nothing.
func (e *Engine) addChurn(ctx context.Context, job *roundJob, prID int64, prev, target string) {
	c, _ := e.churnSince(ctx, prID)
	if c.To == target {
		return
	}
	lines, paths, ok := e.churnOf(ctx, job, prev, target)
	if !ok {
		return
	}
	c.add(lines, paths)
	c.To = target
	if b, err := json.Marshal(c); err == nil {
		e.setKV(ctx, kvPRFullPassChurn(prID), string(b))
	}
}

// churnOf measures the commits from prev to target as the re-review reads
// them (measureRange as the watch's poll identity, usually the comparison
// the round's start made): from...to, or, when they merged the base branch
// in or were rebased, the change of the PR's own diff before and after
// them (compareOwnDiffs), so the base branch's commits are not charged to
// the PR. lines are the added plus deleted lines of the files they touched,
// paths those files, lockfiles and related_ignore's paths left out. ok is
// false without a client or a comparison.
func (e *Engine) churnOf(ctx context.Context, job *roundJob, prev, target string) (lines int, paths []string, ok bool) {
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return 0, nil, false
	}
	m, err := e.measureRange(ctx, gh, job.repo, job.diffBase(), prev, target)
	if err != nil {
		e.log.Info("full pass: the reviewed commits cannot be measured; the churn stays", "pr", job.pr.ID, "err", err)
		return 0, nil, false
	}
	skip := e.fullPassSkip(&job.watch)
	if m.ownOK {
		for _, p := range m.own.changed {
			if !skip(p) {
				paths, lines = append(paths, p), lines+m.own.lines[p]
			}
		}
		return lines, paths, true
	}
	for _, f := range m.push.Files {
		if !skip(f.Path) {
			paths, lines = append(paths, f.Path), lines+fileLines(f)
		}
	}
	return lines, paths, true
}

// fileLines is a compared file's added plus deleted lines: GitHub's count,
// else its patch's.
func fileLines(f github.FileDelta) int {
	if f.Lines > 0 {
		return f.Lines
	}
	return patchLines(f.Patch)
}

// fullPassSkip reports whether a path is left out of a PR's size and churn:
// a lockfile (config.DefaultRelatedIgnore) or a path of w's related_ignore.
func (e *Engine) fullPassSkip(w *config.Watch) func(string) bool {
	globs := append(config.DefaultRelatedIgnore(), e.cfg.RelatedFor(w).Ignore...)
	return func(p string) bool {
		return slices.ContainsFunc(globs, func(g string) bool { return config.MatchPath(g, p) })
	}
}
