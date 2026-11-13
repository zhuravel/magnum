package engine

// CI fix mode (the operator's standing wish "fix CI for any PR", and their
// answer of 2026-11-08, Q3 "Yes, on own PRs"): a required check that failed
// at the head of an own PR of autofix = "open" starts a fix loop in CI mode
// (FixModeCI), when the same check passes on the base. Rule 11 of the look
// (fix_open_verdict.go) gets the failed checks from openCIOf: a run that
// failed, a run skipped beside a failed job of its workflow file (talkable's
// Completion, whose `needs` has no always()), and the run the look started
// (ci_dispatch) or re-ran that ended failure (ciShape3: GitHub's rollup
// leaves a dispatched run out, and Completion is posted on success alone;
// a re-run of that run by hand, or a later start of the dispatched
// workflow at the head, that the head's runs read before a start show
// running or passed takes the failure back: noteRunMoved). Before a start,
// all of these hold (ciWorkOf): nothing else is open
// (findings first: a findings push moves the head and runs CI again, and
// the fixer does one job at a time), no required check still runs, the PR's
// own files at the head are listed whole and change no CI config
// (CIConfigPath), each failed check is a GitHub Actions job, the base ran
// it green (readCIBase: the newest of the base tip's first parents that
// finished the failed job's check decides, read once per head, failed set
// and tip), the PR had fewer than ciFixMax CI fixes, and none ran at this
// head. Then rule 9's gates hold it as any start (the settle, hands off,
// `magnum pause`, quiet hours, autofix_max_loops), the CI reader reads why
// it failed (ci_failures.go), and the record of the start is written before
// startFix (kvPRCIFix), so a restart never starts a fourth: every start
// counts but one the fix job refused at once (unstarted). The loop's first
// cycle merges the parent first as any loop's (the daemon's clean merge, or
// the fixer's conflict), its fixer reproduces and fixes the failure, runs
// the failing tests and pushes; its push gets the forced re-review, whose
// findings the next cycles fix as any loop's, and they leave alone what a
// loop the look starts would (FixState.Waits: an item its reviewer
// accepted, a reviewer's pushback). A fixer that says it cannot
// (a flaky test, an infrastructure failure, a missing secret, ...) ends the
// loop quietly with that class word, and the head needs you, kind ci. A
// coworker's PR is never fixed: the look covers own PRs alone, and
// prepareFix refuses another's.

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRCIFix holds the PR's CI fixes (ciFixRecord as JSON).
func kvPRCIFix(prID int64) string { return fmt.Sprintf("pr.%d.ci_fix", prID) }

// ciFixRefusal is why the failed runs of a PR's head whose files are files
// (listed: the poll listed them) get no CI fix ("" = none), and whether the
// look waits for the head's list (wait): a check that is no GitHub Actions
// job (no log to read), a list GitHub cut short, and a PR that changes its
// own CI config (CIConfigPath: a workflow, an action, a script of
// .github/bin), whose CI is the
// operator's.
func ciFixRefusal(files store.PRFiles, listed bool, head string, runs []ciFailedCheck) (refused string, wait bool) {
	if slices.ContainsFunc(runs, func(c ciFailedCheck) bool { return c.WorkflowFile == "" }) {
		return "no GitHub Actions job, so magnum cannot read why", false
	}
	switch {
	case !listed || files.HeadSHA != head:
		return "", true
	case slices.ContainsFunc(files.Paths, CIConfigPath):
		return "the PR changes its own CI config (a workflow, an action, a script of .github/bin), so its CI is yours", false
	case files.Truncated:
		return "GitHub did not list every file of the PR, so magnum cannot tell whether it changes its CI", false
	}
	return "", false
}

// failedRequired are the failed required checks of pr's head by entries
// (its required list) and r (requiredCIOf of them): each required run that
// failed, under the first entry that names it, compared on the base by its
// own name; and each entry whose runs were all skipped while a job of the
// same workflow file failed (talkable's Completion, skipped when a job it
// needs fails), compared on the base by those failed jobs.
func failedRequired(pr store.PR, entries []string, r requiredCI) []ciFailedCheck {
	var out []ciFailedCheck
	entryOf := func(c store.CheckResult) string {
		for _, e := range entries {
			if store.RequiredNames(e, c) {
				return e
			}
		}
		return c.Name
	}
	for _, run := range r.failed {
		out = append(out, ciFailedCheck{Entry: entryOf(run), Name: run.Name, WorkflowFile: run.WorkflowFile, At: run.At, Compare: []string{run.Name}})
	}
	if len(out) > 0 || pr.CI == nil {
		return out
	}
	for _, entry := range entries {
		runs := store.RequiredRuns(pr.CI.Checks, entry)
		if len(runs) == 0 || slices.ContainsFunc(runs, func(c store.CheckResult) bool { return c.State != store.CheckSkipped }) {
			continue
		}
		skipped := runs[0]
		if skipped.WorkflowFile == "" {
			continue
		}
		var compare []string
		at := skipped.At
		for _, c := range pr.CI.Checks {
			if !c.Status && c.State == store.CheckFailed && c.WorkflowFile == skipped.WorkflowFile {
				compare = append(compare, c.Name)
				at = later(at, c.At)
			}
		}
		if len(compare) > 0 {
			out = append(out, ciFailedCheck{Entry: entry, Name: skipped.Name, WorkflowFile: skipped.WorkflowFile, At: at, Compare: compare})
		}
	}
	return out
}

// ciEntries are the distinct required entries of runs, in their order.
func ciEntries(runs []ciFailedCheck) []string {
	var out []string
	for _, r := range runs {
		if !slices.Contains(out, r.Entry) {
			out = append(out, r.Entry)
		}
	}
	return out
}

// ciShape3 is ci with its missing required checks failed where the run of
// their workflow that the look started at the head (ci_dispatch, s) or the
// re-run's new attempt (r) ended failure or timed out: GitHub's rollup
// leaves a dispatched run out, and talkable's Completion is posted only on
// success, so a red CI there shows as a check that did not run. A run that
// ended otherwise, or never came, leaves ci as it is (ci_missing). The
// start's run may be a re-run of it by hand or a later start of the
// workflow the look found (s.Moved, noteRunMoved): "the newest run of
// ci.yml ended failure"; the re-run's attempt one by hand (r.Moved).
func ciShape3(ci openCI, s ciStart, r ciRerun) openCI {
	if len(ci.Missing) == 0 || ci.Running {
		return ci
	}
	var run int64
	var workflow, how string
	dispatched := false
	switch {
	case !s.At.IsZero() && s.Failed == "" && s.Run.Ended() && runFailed(s.Run.Conclusion) && s.Run.ID != 0:
		run, workflow, dispatched = s.Run.ID, s.Workflow, true
		how = "magnum's run of " + workflow + " ended " + strings.ReplaceAll(s.Run.Conclusion, "_", " ")
		if !s.Moved.IsZero() {
			how = "the newest run of " + workflow + " ended " + strings.ReplaceAll(s.Run.Conclusion, "_", " ")
		}
	case r.Asked() && r.Seen.Ended() && !r.Seen.NoJob && runFailed(r.Seen.Conclusion):
		run, workflow = r.Run.ID, r.Run.Workflow
		how = "magnum's re-run of " + r.Run.Named() + " ended " + strings.ReplaceAll(r.Seen.Conclusion, "_", " ")
		if !r.Moved.IsZero() {
			how = "the newest attempt of " + r.Run.Named() + " ended " + strings.ReplaceAll(r.Seen.Conclusion, "_", " ")
		}
	default:
		return ci
	}
	var failed []ciFailedCheck
	for _, m := range ci.Missing {
		c := ciFailedCheck{Entry: m, WorkflowFile: workflow, Run: run, Dispatched: dispatched}
		if !strings.HasPrefix(m, store.RequiredWorkflowPrefix) {
			c.Name = m
		}
		failed = append(failed, c)
	}
	return openCI{Failed: missingWhat(ci.Missing), FailedRuns: failed, How: how}
}

// runFailed reports whether a run's conclusion is a failure of its jobs.
func runFailed(conclusion string) bool { return conclusion == "failure" || conclusion == "timed_out" }

// ciRunNow is the run that speaks for c, a failed check of a run the look
// named (ciShape3: c.Run, magnum's start of ci_dispatch's workflow or its
// re-run of a run that started no job), among runs, the head's runs: c.Run
// at its newest attempt (a re-run by hand keeps the run's id), or, for
// magnum's start (c.Dispatched), the newest run a dispatch started of c's
// workflow file at the head after c.Run (someone's start by hand). Zero
// where c names no run, or GitHub no longer lists it at the head. moved
// reports that it no longer says that c.Run failed: it is a later run, or
// c.Run's newest attempt has not ended or ended without failing. No rollup
// shows a dispatched run, so only the head's runs tell.
func ciRunNow(runs []github.WorkflowRun, c ciFailedCheck) (github.WorkflowRun, bool) {
	i := slices.IndexFunc(runs, func(r github.WorkflowRun) bool { return r.ID == c.Run })
	if c.Run == 0 || i < 0 {
		return github.WorkflowRun{}, false
	}
	now := runs[i]
	for _, r := range runs {
		if c.Dispatched && r.Event == "workflow_dispatch" && r.Workflow == c.WorkflowFile && r.CreatedAt.After(now.CreatedAt) {
			now = r
		}
	}
	return now, now.ID != c.Run || !now.Done() || !runFailed(now.Conclusion)
}

// ciFixRecordOf reads the PR's CI fixes (none: zero).
func (e *Engine) ciFixRecordOf(ctx context.Context, prID int64) (ciFixRecord, error) {
	r, _, err := readKVJSON[ciFixRecord](ctx, e, kvPRCIFix(prID), "the PR's CI fixes")
	return r, err
}

// updateCIFix changes the PR's CI fixes by fn, under ciFixMu: the look, a
// request and a loop's end on its own goroutine all write the record, and
// none may lose another's change. fn returns false to write nothing.
func (e *Engine) updateCIFix(ctx context.Context, prID int64, fn func(r *ciFixRecord) bool) error {
	e.ciFixMu.Lock()
	defer e.ciFixMu.Unlock()
	r, err := e.ciFixRecordOf(ctx, prID)
	if err != nil {
		return err
	}
	if !fn(&r) {
		return nil
	}
	return writeKVJSON(ctx, e, kvPRCIFix(prID), "the PR's CI fixes", r)
}

// ciBaseTip is the base tip the look checks the CI of pr's base at: the one
// its conflict check fetched for base (fresh, where pr.BaseSHA, the last
// Details read, can trail by days), else pr.BaseSHA.
func ciBaseTip(pr store.PR, l openLedger, base string) string {
	if c := l.Conflict; c.Base == base && c.Tip != "" {
		return c.Tip
	}
	return deref(pr.BaseSHA)
}

// ciKeyAt is the read key of the failed runs: their entries, runs and when
// the rollup says each finished (ciReadKey).
func ciKeyAt(runs []ciFailedCheck) string {
	return ciReadKey(runs, func(c ciFailedCheck) string { return c.At.UTC().Format(time.RFC3339Nano) })
}

// ciFixFactsOf gathers f's CI fix facts for pr of repo (lookOpen): the
// PR's record of CI fixes, its files at the head (ciFixRefusal), the base's
// last read for the failed set at the tip the look fetched (l's conflict
// check), and the CI reader's standing read. Only a failed required check
// with runs the look may fix turns them on.
func (e *Engine) ciFixFactsOf(ctx context.Context, repo store.Repo, pr store.PR, f openFacts, l openLedger) ciFixFacts {
	var c ciFixFacts
	rec, err := e.ciFixRecordOf(ctx, pr.ID)
	failed := f.CI.Failed != "" && len(f.CI.FailedRuns) > 0
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: the PR's CI fixes", "pr", pr.ID)
		c.On, c.Refused = failed, "the record of its CI fixes cannot be read"
		return c
	}
	c.Starts = rec.Count()
	c.Last, c.LastN = rec.LastPushed()
	if !failed {
		return c
	}
	c.On = true
	files, listed, err := e.st.PRFilesOf(ctx, pr.ID)
	if err != nil {
		c.FilesWait = true
	} else {
		c.Refused, c.FilesWait = ciFixRefusal(files, listed, pr.HeadSHA, f.CI.FailedRuns)
	}
	c.AtHead = rec.AtHead(pr.HeadSHA)
	base := cmp.Or(deref(pr.BaseRef), repo.DefaultBranch)
	c.BaseName = "the base"
	if base != "" && gitx.ShellSafeRef(base) {
		c.BaseName = base
	}
	key, tip := ciKeyAt(f.CI.FailedRuns), ciBaseTip(pr, l, base)
	b := rec.Base
	if b.Head == pr.HeadSHA && b.Key == key && b.Tip == tip {
		c.Base, c.BaseSHA = b.Word()
		c.BaseFailed, c.BaseRetry = b.Failed, b.At.Add(f.Settle.Max)
		c.BaseDue = (b.Failed || c.Base == ciBaseRunning) && !f.Now.Before(c.BaseRetry)
		if b.Failed {
			c.Base = ""
		}
	} else {
		c.BaseDue = true
	}
	if r := rec.Read; r.Head == pr.HeadSHA && r.Key == key && (!r.Unread && !r.Running || f.Now.Before(r.At.Add(f.Settle.Max))) {
		c.Read = r
	}
	return c
}

// firstParentChecker is the engine's GitHub client's read of a commit and
// its first parents with their checks (*github.Client's FirstParentChecks).
type firstParentChecker interface {
	FirstParentChecks(ctx context.Context, owner, repo, sha string) ([]github.CommitWithChecks, error)
}

var _ firstParentChecker = (*github.Client)(nil)

// readCIBase reads the CI of pr's base for f's failed checks (rule 11
// asked: v.readCIBase), as w's poll identity in one GraphQL call (the base
// tip the look fetched and its first parents, FirstParentChecks), records
// each check's word in the PR's CI fixes (kvPRCIFix's Base) and returns the
// facts again. A read that fails is recorded as failed, tried again
// settle_max later; nothing is read without a tip or a client.
func (e *Engine) readCIBase(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, f openFacts, l openLedger) ciFixFacts {
	base := cmp.Or(deref(pr.BaseRef), repo.DefaultBranch)
	tip := ciBaseTip(pr, l, base)
	read := ciBaseRead{Head: pr.HeadSHA, Key: ciKeyAt(f.CI.FailedRuns), Tip: tip, At: e.now()}
	gh := e.gh(w.PollIdentity)
	fp, ok := gh.(firstParentChecker)
	var commits []github.CommitWithChecks
	var err error
	switch {
	case !ok || tip == "":
		read.Failed = true
	default:
		if commits, err = fp.FirstParentChecks(ctx, repo.Owner, repo.Name, tip); err != nil {
			read.Failed = true
			if e.changed("ci_base:"+repo.FullName(), err.Error()) {
				e.event(ctx, "warn", prSubject(repo, pr.Number), "fix.ci_base_error",
					fmt.Sprintf("the CI of %s's base at %s, read as %s: %v", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(tip), w.PollIdentity, err),
					map[string]any{"head": pr.HeadSHA, "tip": tip})
			}
		} else {
			e.changed("ci_base:"+repo.FullName(), "")
		}
	}
	if !read.Failed {
		for _, c := range f.CI.FailedRuns {
			read.Checks = append(read.Checks, ciBaseOf(commits, c))
		}
		word, sha := read.Word()
		e.log.Info("autofix open: the base's CI of a failed required check", "subject", prSubject(repo, pr.Number),
			"head", textx.ShortSHA(pr.HeadSHA), "tip", textx.ShortSHA(tip), "word", word, "at", textx.ShortSHA(sha), "commits", len(commits))
	}
	if err := e.updateCIFix(ctx, pr.ID, func(r *ciFixRecord) bool { r.Base = read; return true }); err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: record the base's CI", "pr", pr.ID)
	}
	return e.ciFixFactsOf(ctx, repo, pr, f, l)
}

// ciBaseOf is the base's word on c, a failed check, from commits, the base
// tip and its first parents, newest first: for each check c is compared by
// (its own name, or the failed jobs a skipped one's workflow had; c's entry
// where it names none: a missing check of magnum's run), the newest commit
// whose check runs of that name (any workflow: a reusable workflow on the
// base names its jobs as the PR's) finished it decides: passed, or failed
// (a run skipped beside a failed job of its workflow file counts as
// failed). A commit where it has not ended is passed over for an older one
// that passed it (a busy base's tip runs it while its parent finished it),
// and it is running when no commit read finished it but one runs it, or
// one runs it above a commit that failed it (the newer run may pass, and a
// running word is read again settle_max later, where a failed one stands
// until the tip moves); a skip with no failure beside it (a path filter) is
// no run. No commit that ran it is never. The worst of
// its names is c's word (failed, running, never, passed), each name's word
// in Each when it compared more than one.
func ciBaseOf(commits []github.CommitWithChecks, c ciFailedCheck) ciBaseCheck {
	names := c.Compare
	if len(names) == 0 {
		names = []string{cmp.Or(c.Name, c.Entry)}
	}
	worst := ciBaseCheck{}
	var each []ciBaseCheck
	rank := map[string]int{ciBaseFailed: 4, ciBaseRunning: 3, ciBaseNever: 2, ciBasePassed: 1}
	for _, name := range names {
		got := ciBaseCheck{Name: name, State: ciBaseNever}
		for _, cm := range commits {
			state, ok := baseStateOf(cm.Checks, name, c.Name == "" && len(c.Compare) == 0)
			switch {
			case !ok:
				continue
			case state == ciBaseRunning: // an older commit that passed it decides; running when none finished it
				if got.State == ciBaseNever {
					got = ciBaseCheck{Name: name, SHA: cm.SHA, State: state}
				}
				continue
			}
			if state == ciBasePassed || got.State != ciBaseRunning { // a failure under a newer run that may pass: running
				got = ciBaseCheck{Name: name, SHA: cm.SHA, State: state}
			}
			break
		}
		each = append(each, got)
		if rank[got.State] > rank[worst.State] {
			worst = got
		}
	}
	if len(each) > 1 {
		worst.Each = each
	}
	return worst
}

// baseStateOf is the word of the newest check run of checks that name
// matches (a check run's name, or, entry, a required entry's runs:
// store.RequiredNames), ok false when none finished it there.
func baseStateOf(checks []github.Check, name string, entry bool) (string, bool) {
	var best *github.Check
	for i, ch := range checks {
		match := !ch.Status && ch.Name == name
		if entry {
			match = store.RequiredNames(name, checkResult(ch))
		}
		if match && (best == nil || ch.At.After(best.At)) {
			best = &checks[i]
		}
	}
	if best == nil {
		return "", false
	}
	switch best.State {
	case github.CheckPassed:
		return ciBasePassed, true
	case github.CheckFailed:
		return ciBaseFailed, true
	case github.CheckPending:
		return ciBaseRunning, true
	}
	if best.WorkflowFile != "" && slices.ContainsFunc(checks, func(o github.Check) bool {
		return !o.Status && o.WorkflowFile == best.WorkflowFile && o.State == github.CheckFailed
	}) {
		return ciBaseFailed, true
	}
	return "", false
}

// ciChecksFor are f's failed runs with the base's word on each, from the
// record's base read (the reader's file names it).
func ciChecksFor(runs []ciFailedCheck, b ciBaseRead) []ciFailedCheck {
	out := slices.Clone(runs)
	if len(b.Checks) == len(out) {
		for i := range out {
			out[i].Base = b.Checks[i]
		}
	}
	return out
}

// ensureCIRead is the CI reader's read before a CI fix starts on pr (the
// look decided on the CI item), in the look's batch acts, unless a read of
// the failed set stands at the head and the runs it named did not move
// since (ciRunMoved); it records the read (kvPRCIFix's Read) and reports
// whether it read, so the look decides again. Where the run the look named
// ran again since it failed (the read's Moved), it records that run in the
// start or the re-run instead (noteRunMoved), which takes the failure out
// of f, records no read, and reports true.
func (e *Engine) ensureCIRead(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, f *openFacts) bool {
	if f.CIFix.Read.Head != "" && !e.ciRunMoved(ctx, acts, repo, pr, f) {
		return false
	}
	rec, err := e.ciFixRecordOf(ctx, pr.ID)
	if err != nil {
		return false
	}
	read := e.readCIFailures(ctx, acts, repo, pr, ciChecksFor(f.CI.FailedRuns, rec.Base))
	if read.Moved != nil {
		e.noteRunMoved(ctx, repo, pr, f, *read.Moved)
		return true
	}
	read.Key = ciKeyAt(f.CI.FailedRuns)
	if err := e.updateCIFix(ctx, pr.ID, func(r *ciFixRecord) bool { r.Read = read; return true }); err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: record the read of the failed CI", "pr", pr.ID)
		return false
	}
	f.CIFix.Read = read
	return true
}

// ciRunMoved reports whether the standing read of f's failed checks is of
// a run the look named (ciShape3: magnum's run or re-run of the head, which
// no rollup shows, so a re-run of it by hand shows nowhere else) whose
// newest attempt is no longer the one read, has not ended or did not fail,
// or, for magnum's dispatched run, that a later start of its workflow at
// the head speaks for (ciRunNow). It reads the head's runs, as the batch
// acts, at most once per settle_quiet (the
// standing read's Checked, recorded), before the start the read would
// feed: a start that waits for a free slot costs no read a look. A read of
// no named run, or one that fails (tried again a settle_quiet later, as
// one that found nothing moved), leaves the read standing.
func (e *Engine) ciRunMoved(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, f *openFacts) bool {
	r := f.CIFix.Read
	named := slices.ContainsFunc(f.CI.FailedRuns, func(c ciFailedCheck) bool { return c.Run != 0 })
	if !named || f.Now.Before(later(r.At, r.Checked).Add(f.Settle.Quiet)) {
		return false
	}
	checked := func() bool { // the read stands; the next check is a settle_quiet away
		now := e.now()
		if err := e.updateCIFix(ctx, pr.ID, func(rec *ciFixRecord) bool {
			if rec.Read.Head != r.Head || rec.Read.Key != r.Key {
				return false
			}
			rec.Read.Checked = now
			return true
		}); err != nil {
			e.warnUnlessStopped(ctx, err, "autofix open: record the check of the failed CI's runs", "pr", pr.ID)
		}
		f.CIFix.Read.Checked = now
		return false
	}
	gh, err := acts.client(ctx)
	var runs []github.WorkflowRun
	if err == nil {
		lister, ok := gh.(headRunLister)
		if !ok {
			return false
		}
		runs, err = lister.HeadRuns(ctx, repo.Owner, repo.Name, pr.HeadSHA)
	}
	if err != nil { // tried again a settle_quiet later, not at every look while the start waits
		e.log.Info("autofix open: the head's runs before a CI fix", "subject", prSubject(repo, pr.Number), "err", err)
		return checked()
	}
	for _, c := range f.CI.FailedRuns {
		run, moved := ciRunNow(runs, c)
		switch {
		case moved: // a newer attempt or run that has not ended or did not fail
			return true
		case run.ID == 0: // no run the look named, or one GitHub no longer lists
			continue
		}
		if i := slices.IndexFunc(r.Runs, func(x ciReadRun) bool { return x.ID == run.ID }); i < 0 || r.Runs[i].Attempt != run.Attempt {
			return true
		}
	}
	return checked()
}

// noteRunMoved records run, which now speaks for the run the look named
// that f's failed check failed in (ciRunNow: a re-run of it by hand, or
// someone's later start of ci_dispatch's workflow at the head), with when
// the look found it (Moved): as the start's run (kvPRCIStart's Run) for
// magnum's start, as the attempt (kvPRCIRerun's Seen) for its re-run of a
// run that started no job. It takes the failure back out of f: the look
// waits for that run as for its own (openMissingCI, openRerunVerdict), and
// the poll reads it and the head's checks while it runs and until
// settle_max after Moved (awaitCIRuns), so the Completion of a run that
// passed is read though it moves no count of the rollup. A record it could
// not write is said, and f changes all the same: the next look finds the
// run again.
func (e *Engine) noteRunMoved(ctx context.Context, repo store.Repo, pr store.PR, f *openFacts, run github.WorkflowRun) {
	now := e.now()
	switch r := f.CIRerun; {
	case slices.ContainsFunc(f.CI.FailedRuns, func(c ciFailedCheck) bool { return c.Dispatched }):
		s := f.CIStart
		s.Run, s.Moved = &ciRun{ID: run.ID, Status: run.Status, Conclusion: run.Conclusion}, now
		e.writeCIStart(ctx, pr, s)
		f.CIStart = s
	case r.Run != nil && r.Run.ID == run.ID:
		r.Seen, r.Moved = &ciAttempt{Attempt: run.Attempt, Status: run.Status, Conclusion: run.Conclusion}, now
		if err := writeKVJSON(ctx, e, kvPRCIRerun(pr.ID), "the look's re-run", r); err != nil {
			e.warnUnlessStopped(ctx, err, "autofix open: record the re-run's attempt by hand", "pr", pr.ID)
		}
		f.CIRerun = r
	default: // no record names the run: the failure stays out of this look
		f.CI = e.openCI(ctx, repo.FullName(), pr)
		return
	}
	e.log.Info("autofix open: a run that failed ran again", "subject", prSubject(repo, pr.Number), "head", textx.ShortSHA(pr.HeadSHA),
		"workflow", run.Workflow, "run", run.ID, "attempt", run.Attempt, "status", run.Status, "conclusion", run.Conclusion)
	f.CI = ciShape3(e.openCI(ctx, repo.FullName(), pr), f.CIStart, f.CIRerun)
	f.CIFix = e.ciFixFactsOf(ctx, repo, pr, *f, f.Ledger)
}

// noteCIFixStart records a CI fix of entries at pr's head by by before its
// start (kvPRCIFix's Starts), and reports whether it did: a start whose
// record could not be written does not start. It takes the place of an
// unstarted one at the head (a look that waits for a free slot tries every
// tick), so the record keeps one of them per head at most.
func (e *Engine) noteCIFixStart(ctx context.Context, pr store.PR, entries []string, by string) bool {
	err := e.updateCIFix(ctx, pr.ID, func(r *ciFixRecord) bool {
		s := ciFixStart{Head: pr.HeadSHA, Entries: entries, At: e.now().UTC(), By: by}
		if n := len(r.Starts); n > 0 && r.Starts[n-1].Head == pr.HeadSHA && r.Starts[n-1].Outcome == ciOutcomeUnstarted {
			r.Starts[n-1] = s
			return true
		}
		r.Starts = append(r.Starts, s)
		return true
	})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "record the start of a CI fix", "pr", pr.ID)
		return false
	}
	return true
}

// noteCIFixOutcome sets the outcome of the CI fix at head that has none
// yet (the newest such), with why and the head it pushed: the first end
// that knows says it, and a later one (the loop's end after its push) does
// not overwrite it.
func (e *Engine) noteCIFixOutcome(ctx context.Context, prID int64, head, outcome, why, pushed string) {
	err := e.updateCIFix(ctx, prID, func(r *ciFixRecord) bool {
		for i, s := range slices.Backward(r.Starts) {
			if s.Head == head && s.Outcome == "" {
				r.Starts[i].Outcome, r.Starts[i].Why, r.Starts[i].Pushed = outcome, why, pushed
				return true
			}
		}
		return false
	})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "record the end of a CI fix", "pr", prID)
	}
}

// noteCIFixEnd records the end of the job's CI fix, a loop in CI mode
// (FixState.Mode), with the outcome its end path set (job.ciOutcome; lost
// when none did), before its row goes (endFixState), so the look never
// reads a CI fix that ended with no outcome as one that runs.
func (e *Engine) noteCIFixEnd(ctx context.Context, job *fixJob) {
	if job.state.Mode != FixModeCI {
		return
	}
	e.noteCIFixOutcome(ctx, job.pr.ID, job.state.Since, cmp.Or(job.ciOutcome, ciOutcomeLost), job.ciWhy, job.ciPushed)
}
