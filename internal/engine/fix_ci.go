package engine

// A CI fix by hand: `magnum fix --ci <ref>` and the screens' c
// (FixPayload.Mode FixModeCI). It runs the look's CI fix (ci_fix.go) on the
// operator's own PR whose required check failed at its head, with the same
// loop (its next cycles leave alone what waits for you, as the look's last
// read sorts it: lastReadWaits), the same record before the start and the
// same cap of 3, and skips what a start by hand skips, as `magnum fix`
// does: the look's settle, the hands off after a push, quiet hours, the
// daily cap of loops, and the look's once per head (you may try again at a
// head whose CI fix said it cannot, and that try counts). It refuses, with
// rule 11's words where they say why, a coworker's PR, a head where nothing
// failed (a check that did not run is ci_dispatch's or yours, one that
// still runs waits), a failure red or never run on the base, a PR that
// changes its own CI config, a check that is no Actions job, the cap, a
// head magnum has not reviewed or whose review leaves work (f fixes it
// first), a drain (for a restart or a reboot) and a GitHub budget below the floor (ask
// again after: a CI fix is never held), and every refusal of prepareFix. It
// reads the base and the failed jobs itself when the PR's record lacks them
// for the head, its dry run too: the plan its y/N asks (CIFixPlan) starts
// nothing and writes nothing to GitHub, but reads why the check failed as a
// start would, once for the head, and keeps it (the CI fixes' record,
// ci-failures.json, the fix.ci_read event) for the start. Its CIFixAsked
// binds the start: a head, failed set or count of CI fixes that moved
// since, or a question as old as mergeAskedWait, starts nothing.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// CIFixAsked is what the y/N of a CI fix asked about, which the start it
// confirms is bound to: the PR's head, the failed required entries, the CI
// fixes the PR had then, and when the daemon planned it.
type CIFixAsked struct {
	Head    string    `json:"head"`
	Entries []string  `json:"entries"`
	Count   int       `json:"count"`
	At      time.Time `json:"at"`
}

// CIFixPlan is the daemon's answer to a dry run of a CI fix: the PR
// (owner/name#N), what its y/N asks about, and the plan in words (Text,
// the answer's last line: "dry run: would fix the failed CI of <PR>:
// claude-fix fixes required check Completion at a1a1a1a (2 failed jobs) in
// review1 as fixer-app[bot], CI fix 1 of 3").
type CIFixPlan struct {
	Ref   string     `json:"ref"`
	Asked CIFixAsked `json:"ci_asked"`
	Text  string     `json:"text"`
}

// CIFixDry is how the words of a CI fix's plan start (CIFixPlan.Text).
const CIFixDry = "dry run: would fix the failed CI of "

// ParseCIFixPlan reads the answer to a dry run of a CI fix (its first
// line); false when it is none (a refusal's words, a merge's plan).
func ParseCIFixPlan(result string) (CIFixPlan, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	var p CIFixPlan
	if json.Unmarshal([]byte(line), &p) != nil || p.Ref == "" || p.Asked.Head == "" || len(p.Asked.Entries) == 0 {
		return CIFixPlan{}, false
	}
	return p, true
}

// result is the dry run's answer: p as JSON, then its words.
func (p CIFixPlan) result() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b) + "\n" + p.Text, nil
}

// movedSince is why a CI fix asked about as a may not start on the facts
// now (head, entries, count at now; "" = it may): what the question named
// moved, or the question is mergeAskedWait old.
func (a CIFixAsked) movedSince(head string, entries []string, count int, now time.Time) string {
	switch {
	case a.Head != head:
		return fmt.Sprintf("the head moved since the question (%s, now %s): ask again", textx.ShortSHA(a.Head), textx.ShortSHA(head))
	case !slices.Equal(a.Entries, entries):
		return "the failed required checks changed since the question: ask again"
	case a.Count != count:
		return "the PR had another CI fix since the question: ask again"
	case !now.Before(a.At.Add(mergeAskedWait)):
		return fmt.Sprintf("the question is %s old: ask again", textx.DurationWords(now.Sub(a.At)))
	}
	return ""
}

// ciFactsNow are the look's CI facts of pr of repo under w at once, as
// lookOpen gathers them: the head's required CI (openCI, the look's start
// of ci_dispatch and its re-run, ciShape3), the look's ledger (the base
// tip it fetched) and the CI fix facts.
func (e *Engine) ciFactsNow(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR) (openFacts, openLedger) {
	full := repo.FullName()
	f := openFacts{now: e.now(), pr: pr, settle: e.openSettle()}
	_, f.low = e.githubLow(w.PollIdentity)
	f.ci = e.openCI(ctx, full, pr)
	f.ciWorkflow, f.ciStart = e.openCIStartOf(ctx, full, pr, f.ci)
	if len(f.ci.missing) > 0 {
		f.ciRerun = e.ciRerunAt(ctx, pr)
		f.ci = ciShape3(f.ci, f.ciStart, f.ciRerun)
	}
	var l openLedger
	if raw, ok := e.getKV(ctx, kvPRFixSeen(pr.ID)); ok {
		_ = json.Unmarshal([]byte(raw), &l) // none: the PR's base_sha is the tip
	}
	f.ledger = l
	f.ciFix = e.ciFixFactsOf(ctx, repo, pr, f, l)
	return f, l
}

// requestCIFix handles a CI fix by hand of pr (FixPayload.Mode FixModeCI;
// the file's comment): its refusals, then the plan (dry) or the start,
// bound to asked (nil: nothing was asked, `magnum fix --ci --yes`).
func (e *Engine) requestCIFix(ctx context.Context, pr store.PR, dry bool, asked *CIFixAsked) (string, error) {
	if e.draining(ctx) {
		return "", e.drainedAgain(ctx, "ask again", "a CI fix is never held")
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return "", err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil {
		return "", fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo.FullName())
	}
	if !e.ownPR(pr) {
		return "", fmt.Errorf("PR #%d is %s's, not yours: magnum fixes the CI of your own PRs alone", pr.Number, cmp.Or(deref(pr.AuthorLogin), "someone else"))
	}
	short := textx.ShortSHA(pr.HeadSHA)
	f, l := e.ciFactsNow(ctx, repo, *w, pr)
	switch {
	case f.ci.failed == "" && len(f.ci.missing) > 0:
		return "", fmt.Errorf("%s did not run at %s: nothing failed to fix (ci_dispatch starts it, or you run it)", missingWhat(f.ci.missing), short)
	case f.ci.failed == "" && f.ci.pending:
		return "", fmt.Errorf("CI at %s still runs: nothing failed yet", short)
	case f.ci.failed == "":
		return "", fmt.Errorf("nothing to fix: no required check failed at %s", short)
	case len(f.ci.failedRuns) == 0:
		return "", fmt.Errorf("%s failed at %s, and no required check names the failure: magnum fixes a failed required check alone", f.ci.failed, short)
	case deref(pr.ReviewedSHA) != pr.HeadSHA:
		return "", fmt.Errorf("magnum has not reviewed %s yet: its review comes first", short)
	}
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil {
		return "", err
	}
	sum, ok := sums[pr.ID]
	if !ok {
		return "", fmt.Errorf("magnum has not reviewed %s yet: its review comes first", short)
	}
	if reviewWork(sum, p3Calls{}, e.reviewDescribed(repo, pr.Number, sum.SHA)) {
		return "", fmt.Errorf("magnum's review of %s leaves work: f fixes it first, then the CI after its push", textx.ShortSHA(sum.SHA))
	}
	f.sum = &sum
	byHand := func(c ciFixFacts) ciFixFacts { c.atHead = ciFixStart{}; return c } // a try by hand may follow a cannot at the head
	f.ciFix = byHand(f.ciFix)
	if f.ciFix.baseDue && !f.low && !e.d.DryRun {
		f.ciFix = byHand(e.readCIBase(ctx, repo, *w, pr, f, l))
	}
	refusal := func() error {
		if _, ok := ciWorkOf(f, nil, nil); ok {
			return nil
		}
		_, why, _, _, _ := ciFixWords(f, short)
		return errors.New(why)
	}
	if err := refusal(); err != nil {
		if _, _, _, _, recheck := ciFixWords(f, short); !recheck || e.d.DryRun { // a read that listed no failed job is checked again below
			return "", err
		}
	}
	if !e.d.DryRun {
		acts := e.ciActions(e.ciDispatchIdentity(repo.FullName(), *w)) // ci_actions.go
		e.ensureCIRead(ctx, acts, repo, pr, &f)
		acts.end()
		if f.ci.failed == "" { // noteRunMoved: the run the look named ran again since it failed
			again := f.ciStart.Workflow + " was run again at " + short + " after magnum's run of it failed"
			if f.ciStart.Moved.IsZero() && f.ciRerun.Run != nil {
				again = f.ciRerun.Run.named() + " was run again at " + short + " after magnum's re-run of it failed"
			}
			return "", errors.New(again + ": nothing failed to fix")
		}
		if err := refusal(); err != nil {
			return "", err
		}
	}
	entries, count := ciEntries(f.ci.failedRuns), f.ciFix.starts
	// fix_open_needs.go: its next cycles, on findings, leave alone what waits for you, as the look's CI fix does
	prev := FixState{Mode: FixModeCI, CIChecks: entries, Waits: l.lastReadWaits()}
	if dry {
		job, err := e.prepareFix(ctx, pr, prev, fixPinWouldUnpin, nil, false)
		switch {
		case err != nil:
			return "", err
		case job.budget != nil:
			return "", fmt.Errorf("%s; ask again once it recovers: a CI fix is never held", job.budget.text())
		}
		where := job.slot.Name
		if job.slot.ID == 0 {
			where = "a slot it claims"
		}
		text := fmt.Sprintf("%s%s: %s fixes %s at %s (%s) in %s as %s, CI fix %d of %d", CIFixDry, label, job.role.Name, f.ci.failed, short,
			textx.Count(job.ciJobs, "failed job", "failed jobs"), where, e.fixerLoginOf(job), count+1, ciFixMax)
		for _, n := range job.notes {
			text += "; " + n
		}
		return CIFixPlan{Ref: label, Asked: CIFixAsked{Head: pr.HeadSHA, Entries: entries, Count: count, At: e.now().UTC()}, Text: text}.result()
	}
	if asked != nil {
		if why := asked.movedSince(pr.HeadSHA, entries, count, e.now()); why != "" {
			return "", fmt.Errorf("%s: %s", label, why)
		}
	}
	if !e.noteCIFixStart(ctx, pr, entries, fixBy) {
		return "", fmt.Errorf("%s: the CI fix could not be recorded before its start; nothing started", label)
	}
	job, err := e.startFix(ctx, pr, fixBy, prev, nil)
	if err != nil {
		e.noteCIFixOutcome(ctx, pr.ID, pr.HeadSHA, ciOutcomeUnstarted, "", "")
		var budget *githubBudgetError
		switch {
		case errors.As(err, &budget):
			return "", fmt.Errorf("%s; ask again once it recovers: a CI fix is never held", budget.low.text())
		case errors.Is(err, errFixDraining):
			return "", e.drainedAgain(ctx, "ask again", "a CI fix is never held")
		}
		return "", err
	}
	res := fmt.Sprintf("fixing the failed CI of %s: %s %s, CI fix %d of %d; `magnum abort %s` stops it", label, job.role.Name, job.fixes(),
		count+1, ciFixMax, label)
	for _, n := range job.notes {
		res += "; " + n
	}
	return res, nil
}

// fixerLoginOf is the login the job's fixer pushes as: the watch's fixer
// App's bot, else the fix role's identity's login, else "you".
func (e *Engine) fixerLoginOf(job *fixJob) string {
	if logins := e.cfg.FixerLogins(&job.watch); len(logins) > 0 {
		return logins[0]
	}
	if id := e.cfg.IdentityByName(job.role.Identity); id != nil && id.Login != "" {
		return id.Login
	}
	return "you"
}
