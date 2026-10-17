package engine

// A failing check that fails on the base branch's tip too does not hold an
// automatic approval (DECISIONS "A check that fails on the base too does not
// hold an auto-approval"): on one PR Dependency Audit was red for days on the
// PR's heads and on master alike, the reviews said so in prose, and the gate
// still refused every approval of it with "its head's checks fail". A head
// whose rollup fails is let through when each check it fails (the same
// workflow and name) failed on the base tip the last Details fetch read
// (prs.base_sha) too, once its other checks finished; anything it cannot
// tell refuses as before. The base tip's checks are read once per tip and
// baseChecksTTL, not once per PR or tick.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The CI refusals of autoGateRefusal (the card's words).
const (
	ciFailsRefusal = "its head's checks fail (magnum approves it once they pass)"
	ciRunsRefusal  = "its head's checks have not finished (magnum approves it once they pass)"
)

// baseChecksTTL is how long the gates keep a base tip's checks
// (baseTipChecks), a failed read too: a tip's checks change only while they
// run, and a restart reads them again.
const baseChecksTTL = 10 * time.Minute

// baseTip is a base branch's tip: its repository ("owner/name") and commit.
type baseTip struct{ repo, sha string }

// baseTipRead is a base tip's checks as read at at; ok is false when the
// read failed.
type baseTipRead struct {
	checks []github.Check
	ok     bool
	at     time.Time
}

// kvAutoApproveCIBase records "<head>|<run id>" once the gates let that
// head's failing checks through after that round of magnum's because the
// base tip fails them too (review.auto_approve_ci_base, once per head and
// round: the gates run on every tick until the approval posts).
func kvAutoApproveCIBase(prID int64) string { return fmt.Sprintf("pr.%d.auto_approve_ci_base", prID) }

// failingCIRefusal is autoGateRefusal's answer for c, whose head's rollup
// fails, after magnum's round sum: failsLikeTheBase for every check the
// head's stored checks (pr.CI, read on the head, every one listed) fail and
// whether any of them still runs (GitHub's rollup fails as soon as one
// check fails); ciFailsRefusal when it cannot tell (no checks stored for
// the head, or not every one).
func (e *Engine) failingCIRefusal(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) (why string, waits bool) {
	ci := c.PR.CI
	if ci == nil || ci.SHA != c.PR.HeadSHA || !ci.Complete {
		return ciFailsRefusal, false
	}
	var failed []store.CheckResult
	running := false
	for _, ch := range ci.Checks {
		switch ch.State {
		case store.CheckFailed:
			failed = append(failed, ch)
		case store.CheckPending:
			running = true
		}
	}
	return e.failsLikeTheBase(ctx, c, sum, failed, running)
}

// failsLikeTheBase is the CI gate's answer for failed, the failing checks
// of c's head that count (every one, or those required_checks names:
// requiredCIRefusal), after magnum's round sum: "" when each failed on the
// base tip (pr.BaseSHA) too, the same workflow and name, and no check that
// counts still runs (running); ciRunsRefusal, which waits, when they match
// but one still runs; ciFailsRefusal otherwise, and when it cannot tell (no
// failed check, no base tip, a read that failed).
func (e *Engine) failsLikeTheBase(ctx context.Context, c store.RepoPR, sum store.ReviewSummary, failed []store.CheckResult,
	running bool) (why string, waits bool) {
	base := deref(c.PR.BaseSHA)
	if len(failed) == 0 || base == "" {
		return ciFailsRefusal, false
	}
	checks, ok := e.baseTipChecks(ctx, c, base)
	if !ok {
		return ciFailsRefusal, false
	}
	for _, f := range failed {
		if !slices.ContainsFunc(checks, func(b github.Check) bool {
			return b.Workflow == f.Workflow && b.Name == f.Name && b.State == github.CheckFailed
		}) {
			return ciFailsRefusal, false
		}
	}
	if running {
		return ciRunsRefusal, true
	}
	e.noteCIBase(ctx, c, sum, len(failed), base)
	return "", false
}

// baseTipChecks is the checks of c's base tip sha, read as its watch's poll
// identity and kept baseChecksTTL; ok is false when they cannot be read (no
// watch or client, a failed read, logged at info: a reason never carries an
// error's text). A read the daemon's stop cut short is not kept.
func (e *Engine) baseTipChecks(ctx context.Context, c store.RepoPR, sha string) ([]github.Check, bool) {
	k, now := baseTip{c.Repo, sha}, e.now()
	if r, hit := e.baseChecks[k]; hit && now.Sub(r.at) < baseChecksTTL {
		return r.checks, r.ok
	}
	w := e.cfg.WatchFor(c.Repo)
	if w == nil {
		return nil, false
	}
	gh := e.gh(w.PollIdentity)
	if gh == nil {
		return nil, false
	}
	repo := repoOf(c.Repo)
	checks, err := gh.CommitChecks(ctx, repo.Owner, repo.Name, sha)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false
		}
		e.log.Info("auto approval: the base tip's checks", "pr", c.PR.ID, "base", textx.ShortSHA(sha), "err", err)
	}
	for old, r := range e.baseChecks {
		if now.Sub(r.at) >= baseChecksTTL {
			delete(e.baseChecks, old)
		}
	}
	if e.baseChecks == nil {
		e.baseChecks = map[baseTip]baseTipRead{}
	}
	e.baseChecks[k] = baseTipRead{checks: checks, ok: err == nil, at: now}
	return checks, err == nil
}

// noteCIBase records review.auto_approve_ci_base for c's head and round
// sum, once (kvAutoApproveCIBase): its n failing checks fail on the base
// tip sha too, so the approval goes ahead. The event names no check: the
// checks' names are the repository's workflows', kept in the registry.
func (e *Engine) noteCIBase(ctx context.Context, c store.RepoPR, sum store.ReviewSummary, n int, sha string) {
	pr := c.PR
	key, mark := kvAutoApproveCIBase(pr.ID), pr.HeadSHA+"|"+sum.RunID
	if v, _ := e.getKV(ctx, key); v == mark {
		return
	}
	e.setKV(ctx, key, mark)
	tip := "the base branch's tip"
	if b := deref(pr.BaseRef); b != "" { // shell-safe: classify refuses another base first
		tip = b + "'s tip"
	}
	repo := repoOf(c.Repo)
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approve_ci_base",
		fmt.Sprintf("%s#%d: its head's %s %s on %s %s too; the approval goes ahead", repo.FullName(), pr.Number,
			textx.Count(n, "failing check", "failing checks"), textx.Plural(n, "fails", "fail"), tip, textx.ShortSHA(sha)),
		map[string]any{"head_sha": pr.HeadSHA, "run_id": sum.RunID, "base_sha": sha, "failing": n})
}
