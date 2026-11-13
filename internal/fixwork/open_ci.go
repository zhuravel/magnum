package fixwork

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// FixModeCI is the mode of a fix loop's first cycle that fixes the PR's
// failed required CI (FixState.Mode, FixPayload.Mode).
const FixModeCI = "ci"

// CIFixMax is how many CI fixes a PR gets across its heads.
const CIFixMax = 3

// ItemCI is the look's work item of a CI fix (class ClassCI).
const (
	ItemCI  = "ci"
	ClassCI = "ci"
)

// ciCannotClasses are the class words a fixer's ci entry under Needs you
// gives (skills/magnum-fix/SKILL.md, section 8), with the verdict's words.
var ciCannotClasses = map[string]string{
	"flaky":   "a flaky test, not the PR's code",
	"infra":   "an infrastructure failure",
	"secret":  "a missing secret",
	"config":  "a CI config change it may not make",
	"base":    "a failure on the base",
	"unfixed": "a failure it could not fix",
}

// CICannotWords are the verdict's words of class, a class word of a
// fixer's ci entry under Needs you (ciCannotClasses); "" for another word.
func CICannotWords(class string) string { return ciCannotClasses[class] }

// ciBaseDepth is how many first-parent commits of the base the base read
// looks at (github.FirstParentDepth), in the verdict's words.
const ciBaseDepth = github.FirstParentDepth

// CIFailedCheck is one failed required check at a head (openCIOf,
// ciShape3) that the reader reads: the required entry ("Completion",
// "workflow:CI"), the check run's name ("" for a workflow entry's missing
// check), its workflow's file, the run that failed where the look knows it
// (Run: magnum's start of the workflow or its re-run; 0: the newest of
// WorkflowFile at the head), when the rollup says it finished (At: a re-run
// that fails again is a new failure), the check names the base read
// compares (Compare: its own, or the failed jobs beside a skipped one;
// none: the entry's runs), and the base's word on it (zero: not read).
// Dispatched: Run is magnum's start of ci_dispatch's workflow, which no
// rollup shows, nor a re-run of it or a later start of the workflow at the
// head (CIRunNow).
type CIFailedCheck struct {
	Entry        string
	Name         string
	WorkflowFile string
	Run          int64
	Dispatched   bool
	At           time.Time
	Compare      []string
	Base         CIBaseCheck
}

// CIFixFacts are what the look knows of a CI fix of f.CI's failed checks
// at the head (ciFixFactsOf): On, the look gathered them; why the failure
// is never the fixer's (Refused) or the files to tell are not listed at the
// head yet (FilesWait); the CI fixes the PR had (Starts) and the one at
// this head (AtHead); the base's word (Base, its commit BaseSHA, the base
// branch in words BaseName, BaseDue: a read is due, BaseFailed: the last
// read failed, retried at BaseRetry); the CI reader's standing read at the
// head (Read: zero when none, or one due again); and the newest CI fix that
// pushed (Last, its number LastN), whose head the waits and the idle
// verdict name.
type CIFixFacts struct {
	On         bool
	Refused    string
	FilesWait  bool
	Starts     int
	AtHead     CIFixStart
	Base       string
	BaseSHA    string
	BaseName   string
	BaseDue    bool
	BaseFailed bool
	BaseRetry  time.Time
	Read       CIFailRead
	Last       CIFixStart
	LastN      int
}

// CIWorkOf is the CI fix item of f, when rule 11's failure is the fixer's
// and nothing else is open (findings first): the look gathered the facts, a
// failed required check that is an Actions job, no required check still
// running, files that change no CI config, a base that ran it green, fewer
// than CIFixMax CI fixes, none at this head yet, and no standing read of
// the failed jobs that says why not.
func CIWorkOf(f Facts, work, needs []Item) (Item, bool) {
	c := f.CIFix
	r := c.Read
	switch {
	case !c.On, f.CI.Failed == "", len(f.CI.FailedRuns) == 0, f.CI.Pending, len(work) > 0, len(needs) > 0, f.Sum == nil,
		c.Refused != "", c.FilesWait, c.Base != CIBasePassed, c.BaseDue, c.Starts >= CIFixMax, c.AtHead.Head != "",
		r.Head != "" && (r.Unread || r.Running || r.Jobs == 0):
		return Item{}, false
	}
	return Item{Key: "ci:" + textx.ShortSHA(f.PR.HeadSHA), Class: ClassCI, Kind: ItemCI, URL: f.PR.URL}, true
}

// CIFixWords is rule 11's verdict on a failed required check that is no
// work (no CI fix starts now): needs you with why, or waits; readBase asks
// the look to read the base (lookOpen), and recheck to check a read that
// listed no failed job of a run the look named against the head's runs
// (ensureCIRead, ciRunMoved: at most once a settle_quiet), so a re-run of
// it by hand is read again. A check the look gathered nothing for (a
// rollup's failure, no facts) needs you as before CI fix mode.
func CIFixWords(f Facts, short string) (verdict, why string, until time.Time, readBase, recheck bool) {
	c := f.CIFix
	failed := ciFailedAt(f, short)
	base := cmp.Or(c.BaseName, "the base")
	clock := func(t time.Time) string { return WaitClock(t, f.Now) }
	if !c.On || len(f.CI.FailedRuns) == 0 {
		return OpenNeedsYou, failed, time.Time{}, false, false
	}
	if at := c.AtHead; at.Head != "" {
		switch {
		case at.Outcome == CIOutcomeCannot, at.Outcome == CIOutcomeMerged && at.Why != "": // a cannot, or one on the parent's merge
			return OpenNeedsYou, failed + ": the CI fix found " + cmp.Or(ciCannotClasses[at.Why], ciCannotClasses["unfixed"]), time.Time{}, false, false
		case at.Outcome == CIOutcomeStopped:
			return OpenNeedsYou, failed + ": the CI fix at " + short + " was stopped; it is yours", time.Time{}, false, false
		case at.Outcome == CIOutcomePushed, at.Outcome == CIOutcomeMerged:
			return OpenNeedsYou, failed + ": the CI fix at " + short + " ended; it is yours", time.Time{}, false, false
		}
		return OpenNeedsYou, failed + ": the CI fix at " + short + " failed; it is yours", time.Time{}, false, false
	}
	switch {
	case c.Refused != "":
		return OpenNeedsYou, failed + ": " + c.Refused, time.Time{}, false, false
	case c.Starts >= CIFixMax:
		return OpenNeedsYou, fmt.Sprintf("%s after %s: the CI is yours", failed, textx.Count(c.Starts, "CI fix", "CI fixes")), time.Time{}, false, false
	case f.CI.Pending:
		return OpenWaits, failed + "; a required check still runs", time.Time{}, false, false
	case c.FilesWait:
		return OpenWaits, failed + "; magnum reads the PR's files with the next poll", time.Time{}, false, false
	case c.Base == "" && c.BaseDue && f.Low:
		return OpenWaits, failed + "; reading " + base + "'s CI waits for the GitHub budget", time.Time{}, false, false
	case c.Base == "" && c.BaseDue:
		return OpenWaits, failed + "; reading " + base + "'s CI", time.Time{}, true, false
	case c.Base == "" && c.BaseFailed:
		return OpenUnknown, failed + "; reading " + base + "'s CI failed; trying again at " + clock(c.BaseRetry), c.BaseRetry, false, false
	case c.Base == CIBaseFailed:
		return OpenNeedsYou, fmt.Sprintf("%s, and on %s at %s too: magnum fixes only a check that passes on the base", failed, base,
			textx.ShortSHA(c.BaseSHA)), time.Time{}, false, false
	case c.Base == CIBaseNever:
		return OpenNeedsYou, fmt.Sprintf("%s; %s's last %d commits never ran it: magnum fixes only a check that passes on the base", failed, base,
			ciBaseDepth), time.Time{}, false, false
	case c.Base == CIBaseRunning:
		return OpenWaits, fmt.Sprintf("%s; it still runs on %s at %s", failed, base, textx.ShortSHA(c.BaseSHA)), time.Time{}, c.BaseDue && !f.Low, false
	case c.Read.UnreadWhy != "":
		return OpenNeedsYou, failed + ": magnum cannot read why (" + c.Read.UnreadWhy + ")", time.Time{}, false, false
	case c.Read.Unread:
		retry := c.Read.At.Add(f.Settle.Max)
		return OpenUnknown, failed + "; reading the failed jobs failed; trying again at " + clock(retry), retry, false, false
	case c.Read.Running:
		retry := c.Read.At.Add(f.Settle.Max)
		return OpenWaits, failed + "; a re-run of its run has not ended", retry, false, false
	case c.Read.Head != "" && c.Read.Jobs == 0:
		return OpenNeedsYou, failed + ": its run lists no failed job magnum can read; it is yours", time.Time{}, false,
			slices.ContainsFunc(f.CI.FailedRuns, func(c CIFailedCheck) bool { return c.Run != 0 }) // a run the look named may run again
	}
	return OpenNeedsYou, failed, time.Time{}, false, false
}

// ciAfter is what the waits and the idle verdict of f's head add when the
// head is a CI fix's push: " after CI fix 1 of 3" ("" otherwise).
func ciAfter(f Facts) string {
	if c := f.CIFix; c.Last.Pushed != "" && c.Last.Pushed == f.PR.HeadSHA {
		return fmt.Sprintf(" after CI fix %d of %d", c.LastN, CIFixMax)
	}
	return ""
}

// ciWorkWords names the CI fix the look starts or holds: "the CI fix of
// required check Completion".
func ciWorkWords(f Facts) string { return "the CI fix of " + f.CI.Failed }

// ciRunning is rule 3's words of a CI loop's fixer at work: "fixing CI:
// required check Completion at a1a1a1a, cycle 1 of 5 (CI fix 1 of 3)".
func ciRunning(f Facts) string {
	what := "required CI"
	if len(f.Fix.CIChecks) > 0 {
		what = MissingWhat(f.Fix.CIChecks)
	}
	return fmt.Sprintf("fixing CI: %s at %s, cycle %d of %d (CI fix %d of %d)", what, textx.ShortSHA(f.Fix.Head), f.Fix.Cycle, f.Fix.Cap,
		max(f.CIFix.Starts, 1), CIFixMax)
}

// openMissingCI is rule 11's verdict v of f's head, at which the required
// checks f.ci.missing names have no run (their words: MissingWhat): while a
// check of the head has not finished (f.ci.running: a job that needs others
// is created only once they end, however long they run) and until
// settle_max after the head moved, it waits for them to start (CI at the
// head). After that they did not run, and it needs you with how to start
// them, unless the look starts the workflow ci_dispatch names
// (f.ciWorkflow): it asks for the start (v.dispatch) when nothing holds it,
// waits for the end of `magnum pause` and for the GitHub budget (the poll
// identity's, f.low, and ci_dispatch_as's, f.ciLow), and waits
// for the run of a start it made (f.ciStart) while the head's checks run,
// while the run is queued or runs (CIStart.Run) and until settle_max after
// the start, or after the look found that run run again (CIStart.Moved:
// "ci.yml was run again after magnum's run of it failed"). A start that
// failed, one magnum refused, one whose run ended without the checks (in
// words of the run's conclusion, RunEnd) and one whose run never came need
// you. Before either, where no start was made
// at the head, the look's read of the head's runs and its re-run of the
// one that started no job decide (openRerunVerdict, ci_rerun.go), and the
// re-run's attempt is waited for as a start's run is.
func openMissingCI(f Facts, v OpenVerdict) OpenVerdict {
	short, what := textx.ShortSHA(f.PR.HeadSHA), MissingWhat(f.CI.Missing)
	waits := func(why string, until time.Time) OpenVerdict {
		if !until.After(f.Now) {
			until = time.Time{} // unknown: until the head's checks end
		}
		v.Verdict, v.Why, v.Until = OpenWaits, "CI at "+short+": "+why+ciAfter(f), until
		return v
	}
	needsYou := func(how string) OpenVerdict {
		v.Needs = append(v.Needs, OpenNeed{Kind: "ci_missing", URL: f.PR.URL})
		v.Verdict, v.Why = OpenNeedsYou, fmt.Sprintf("%s did not run at %s: %s", what, short, how)
		return v
	}
	wf, s := f.CIWorkflow, f.CIStart
	started := !s.At.IsZero() && s.Failed == ""
	again := wf + " was run again after magnum's run of it failed" // CIStart.Moved: a re-run by hand, a later start
	startedWhy := func() string {
		if !s.Moved.IsZero() {
			return again
		}
		return fmt.Sprintf("magnum started %s at %s, as %s did not run", wf, WaitClock(s.At, f.Now), what)
	}
	startWait := later(s.At, s.Moved).Add(f.Settle.Max)
	due := f.PR.HeadChangedAt.Add(f.Settle.Max)
	switch {
	case (f.CI.Running || s.Run.Running()) && started:
		return waits(startedWhy(), startWait)
	case (f.CI.Running || f.CIRerun.Seen.Running()) && f.CIRerun.Asked(): // ci_rerun.go
		return waits(rerunWhy(f, what), later(f.CIRerun.At, f.CIRerun.Moved).Add(f.Settle.Max))
	case f.CI.Running || f.Now.Before(due):
		return waits(what+" has not started", due)
	}
	if s.At.IsZero() && s.Refused == "" { // ci_rerun.go: a run of the head that started no job, before any start
		if rv, ok := openRerunVerdict(f, &v, waits, needsYou, what); ok {
			return rv
		}
	}
	const byHand = "; run it on the PR's branch"
	switch {
	case wf == "":
		return needsYou("run its workflow on the PR's branch, or push")
	case s.Refused != "":
		return needsYou(fmt.Sprintf("magnum does not start %s (%s)%s", wf, s.Refused, byHand))
	case s.Failed != "":
		return needsYou(fmt.Sprintf("magnum's start of %s failed (%s)%s", wf, s.Failed, byHand))
	case started && f.Now.Before(startWait):
		return waits(startedWhy(), startWait)
	case started && s.Run.Ended() && !s.Moved.IsZero():
		return needsYou(fmt.Sprintf("%s; that run ended%s without it%s", again, RunEnd(s.Run.Conclusion), byHand))
	case started && s.Run.Ended():
		return needsYou(fmt.Sprintf("magnum started %s at %s and its run ended%s without it%s", wf, WaitClock(s.At, f.Now), RunEnd(s.Run.Conclusion), byHand))
	case started:
		return needsYou(fmt.Sprintf("magnum started %s at %s and no run came%s", wf, WaitClock(s.At, f.Now), byHand))
	case f.Pause != "":
		return waits(fmt.Sprintf("%s did not run; magnum starts %s once the pause ends", what, wf), f.PauseUntil)
	case f.Low || f.CILow:
		return waits(fmt.Sprintf("%s did not run; magnum starts %s once the GitHub budget allows", what, wf), time.Time{})
	}
	v.Dispatch = true
	return needsYou("magnum starts " + wf)
}

// conclusionRe is a run's conclusion as GitHub writes one ("timed_out").
var conclusionRe = regexp.MustCompile(`^[a-z_]{1,32}$`)

// RunEnd is how a run of the CI workflow ended, by its conclusion, in the
// verdict's words (" (timed out)"); "" for none, or one that is not
// GitHub's plain word.
func RunEnd(conclusion string) string {
	if !conclusionRe.MatchString(conclusion) {
		return ""
	}
	return " (" + strings.ReplaceAll(conclusion, "_", " ") + ")"
}

// MissingWhat names missing, required entries with no run on a head, in
// the verdict's words: the first ("required check Completion", "required
// workflow CI" for a workflow entry), and how many more ("and 1 more"). An
// entry is the repository's configuration or GitHub's rulesets, never PR
// text; one that is not plain text (ShownCheckName) is "a required check".
func MissingWhat(missing []string) string {
	what := "a required check"
	if workflow, ok := strings.CutPrefix(missing[0], store.RequiredWorkflowPrefix); ok && ShownCheckName(workflow) {
		what = "required workflow " + workflow
	} else if !ok && ShownCheckName(missing[0]) {
		what = "required check " + missing[0]
	}
	if n := len(missing) - 1; n > 0 {
		what += fmt.Sprintf(" and %d more", n)
	}
	return what
}

// ShownCheckName reports whether name, a check's workflow and name, is
// plain text an event or the log may carry: ASCII letters, digits, spaces
// and "._-/+@:()[],#", at most 120 bytes, not blank.
func ShownCheckName(name string) bool {
	if strings.TrimSpace(name) == "" || len(name) > 120 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune(" ._-/+@:()[],#", r):
		default:
			return false
		}
	}
	return true
}

// openRerunVerdict is rule 11's verdict by the look's read of the head's
// runs and its re-run of the one that started no job (f.ciRerun), past the
// waits for the head's checks and before ci_dispatch's start: ok false
// leaves the verdict to the rest of rule 11 (no run that started no job,
// or a read that failed). Where the head's runs are due a read it asks for
// it (v.readRuns) unless the GitHub budget is low, and waits for the budget
// where nothing was read and no ci_dispatch would start the workflow
// instead; it needs you, with what to grant, while GitHub would not mint
// the token that reads them (UnreadWhy; a start would fail on the same
// mint); it waits while a run of the head has not ended (queuedWhy), asks
// for the re-run (v.rerun) when nothing holds it (`magnum pause`, the
// budget), waits for the re-run's attempt until settle_max after it, and
// needs you when magnum or GitHub refused the re-run, when its attempt
// started no job either, ended without the check, or never came.
func openRerunVerdict(f Facts, v *OpenVerdict, waits func(string, time.Time) OpenVerdict, needsYou func(string) OpenVerdict, what string) (OpenVerdict, bool) {
	r, low := f.CIRerun, f.Low || f.CILow
	due := r.ReadDue(f.PR.HeadSHA, f.Now, f.Settle.Max)
	if due {
		v.ReadRuns = !low
	}
	if r.Head == f.PR.HeadSHA && r.Unread && r.UnreadWhy != "" { // ci_actions.go: GitHub will not mint the token that reads them
		return needsYou(fmt.Sprintf("magnum cannot read the head's runs (%s); run its workflow on the PR's branch, or push", r.UnreadWhy)), true
	}
	if due {
		if r.Head != f.PR.HeadSHA || r.Unread {
			if low && f.CIWorkflow == "" {
				return waits(what+" did not run; magnum reads the head's runs once the GitHub budget allows", time.Time{}), true
			}
			return OpenVerdict{}, false
		}
	}
	if r.Queued != 0 {
		return waits(queuedWhy(r.Queued, r.QueuedStatus), r.ReadAt.Add(f.Settle.Max)), true
	}
	if r.Run == nil {
		return OpenVerdict{}, false
	}
	const onGitHub = "; re-run it on GitHub"
	run := r.Run.Named()
	switch {
	case r.Refused != "":
		return needsYou(fmt.Sprintf("magnum does not re-run %s (%s)%s", run, r.Refused, onGitHub)), true
	case r.Failed != "":
		return needsYou(fmt.Sprintf("magnum's re-run of %s failed (%s)%s", run, r.Failed, onGitHub)), true
	case r.Asked() && r.Seen.Ended() && r.Seen.NoJob:
		return needsYou(fmt.Sprintf("%s failed to start twice%s%s, or push", run, RunEnd(r.Seen.Conclusion), onGitHub)), true
	case r.Asked() && f.Now.Before(later(r.At, r.Moved).Add(f.Settle.Max)):
		return waits(rerunWhy(f, what), later(r.At, r.Moved).Add(f.Settle.Max)), true
	case r.Asked() && r.Seen.Ended() && !r.Moved.IsZero():
		return needsYou(fmt.Sprintf("%s; that attempt ended%s without it%s", rerunWhy(f, what), RunEnd(r.Seen.Conclusion), onGitHub)), true
	case r.Asked() && r.Seen.Ended():
		return needsYou(fmt.Sprintf("magnum re-ran %s at %s and it ended%s without it%s", run, WaitClock(r.At, f.Now), RunEnd(r.Seen.Conclusion), onGitHub)), true
	case r.Asked():
		return needsYou(fmt.Sprintf("magnum re-ran %s at %s and no new attempt came%s", run, WaitClock(r.At, f.Now), onGitHub)), true
	case f.Pause != "":
		return waits(fmt.Sprintf("%s did not run; magnum re-runs %s once the pause ends", what, run), f.PauseUntil), true
	case low:
		return waits(fmt.Sprintf("%s did not run; magnum re-runs %s once the GitHub budget allows", what, run), time.Time{}), true
	}
	v.Rerun = true
	return needsYou("magnum re-runs " + run), true
}

// queuedWhy is the wait beside run id of the head, which had not ended at
// the read, by status, its status then: one GitHub has not given a runner
// yet (requested, queued, pending) "run 42673098082 has started no job
// yet", and any other (in_progress, waiting for an environment) "run
// 42673098082 has not ended".
func queuedWhy(id int64, status string) string {
	switch status {
	case "requested", "queued", "pending":
		return fmt.Sprintf("run %d has started no job yet", id)
	}
	return fmt.Sprintf("run %d has not ended", id)
}

// rerunWhy is the wait for the look's re-run of f: "magnum re-ran run
// 42673098082 of ci.yml at 20:47, as required check build did not run", or
// for an attempt by hand after its attempt failed (Moved): "run
// 42673098082 of ci.yml was run again after magnum's re-run of it failed".
func rerunWhy(f Facts, what string) string {
	if !f.CIRerun.Moved.IsZero() {
		return f.CIRerun.Run.Named() + " was run again after magnum's re-run of it failed"
	}
	return fmt.Sprintf("magnum re-ran %s at %s, as %s did not run", f.CIRerun.Run.Named(), WaitClock(f.CIRerun.At, f.Now), what)
}
