package engine

// The verdict autofix = "open" keeps per own PR (KVPROpen), and the seen
// ledger behind it (kvPRFixSeen): what the look (fix_open.go) decided at
// the PR's head, in plain words, and what it remembers of the items it saw,
// the loops it saw start and end and the keys their fixers had. The engine
// is their only writer; the factory's screen reads the verdict.
//
// decideOpen is the decision, a pure function of the facts the look
// gathered (openFacts). The first of these rules that applies wins:
//
//  1. a PR the look does not cover (a draft, a fork, fix off, a muted PR, a
//     watch that leaves own PRs out, a baseline or ineligible PR) is off;
//  2. a PR that needs attention needs you, unless its attention is the
//     fixer's decision calls alone and the whole read shows your decision
//     in each one's thread (decided, fix_decide.go): the decisions are work;
//  3. a running fix loop is fixing; a held one waits; an unreadable one
//     needs you (`magnum abort` clears it);
//  4. a stalemate needs you;
//  5. magnum's review of the head waits, without a cap, until
//     magnum_late_after past the time its round was due (counted from the
//     end of each `magnum pause` and [daemon] quiet_hours window that began
//     before it was late, and never late inside one), when it needs you;
//  6. replies magnum has not re-decided wait;
//  7. without a whole read of GitHub since the PR changed, or while the
//     poll of its watch is behind (the PR's row may be old), it is unknown,
//     or waits for the GitHub budget; a read GitHub cut short needs you;
//  8. a handed item still open needs you: a person's, magnum's, a conflict,
//     a bot's thread, a finding the fixer skipped without a word, a thread
//     a fixer resolved with no answer, a thread with two fixer replies that
//     the reviewer answered again (a bot's receipt or note is seen and
//     drops out), unless every one of them is a person's and rule 9 has
//     work to start on: the person's items then wait, listed and named in
//     the verdict's words, while the loop works on the rest (its fixer
//     leaves them alone: FixState.Waits) and while that work waits for its
//     start, but at the daily cap they need you. A person's change request
//     made once one of their items waited for you counts as handed (their
//     pushback), and an item whose reviewer approved the PR after its
//     latest word waits for no one (fix_open_needs.go);
//  9. work (an item never handed, magnum's review no loop worked on) starts
//     a loop once settle allows, a person's handed items waiting or not:
//     never on magnum's rebuttals alone with no review work, at most
//     autofix_max_loops a day, never under `magnum pause` or inside
//     [daemon] quiet_hours (the work waits for their end, spending no
//     start), never within hands_off_after_push of the operator's own push;
//  10. a head the reviewers have not settled on waits;
//  11. red required CI starts a CI fix on rule 9's terms where the failed
//     check passes on the base (ci_fix.go: findings first, at most 3 a PR,
//     none at a head a CI fix tried already, never one that changes its CI
//     config, once a required check no longer runs) and needs you
//     otherwise, with why; pending required CI waits; a required
//     check with no run at a head whose checks magnum read whole, where
//     GitHub requires it on the PR's base or the repository sets
//     ci_dispatch, waits while a check of the head runs and until
//     settle_max after the push, and then needs you (it did not run: a
//     workflow that runs only when the PR opens), never green; a merge
//     with the base the look could not check is unknown;
//  12. otherwise the PR is idle: nothing open at its head. It says whether
//     the merge is the operator's (OpenMergeIsYours: approved, or a base
//     that requires no review) and names the operator's optional calls
//     still open (P3 author's calls a fixer left them, fix_optional.go),
//     which are never work and hold nothing. A migration that must go to a
//     staging before the merge keeps it idle: the screens put that deploy
//     first on their wait on the merge (StagingFirst), from the registry.
//
// Unknown is never idle.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPROpen holds autofix = "open"'s verdict on an own PR (OpenVerdict as
// JSON), written by the engine's look; no row for a PR the look does not
// cover.
func KVPROpen(prID int64) string { return fmt.Sprintf("pr.%d.open", prID) }

// kvPRFixSeen holds the look's ledger of the PR (openLedger as JSON).
func kvPRFixSeen(prID int64) string { return fmt.Sprintf("pr.%d.fix_seen", prID) }

// openSettle are the [pipeline] keys of autofix = "open".
type openSettle struct {
	quiet, max, late, handsOff time.Duration
	checks                     []string
	maxLoops                   int
	// quietHours is [daemon] quiet_hours ("" = none).
	quietHours string
}

// openCI is what the head's required checks say (openCIOf): what failed
// ("required check rspec", "required workflow CI", "CI" for the rollup;
// "" for nothing), whether one still runs or the rollup has not started
// (beside a failure too), the required entries that name no run on the
// head (missing: whose checks magnum read whole, openMissingCI), and,
// beside those, whether a check of the head has not finished (running: a
// job GitHub creates only once the jobs it needs end has no run until
// then); the failed required checks a CI fix may fix (failedRuns, ci_fix.go;
// none for the rollup's failure) and how a run of magnum's ended where it
// decided the failure (how, ciShape3).
type openCI struct {
	failed     string
	pending    bool
	missing    []string
	running    bool
	failedRuns []ciFailedCheck
	how        string
}

// openCIOf is rule 11's CI of pr's head by required, its repository's
// required checks (store.Store.RequiredChecks: the [[repo]] block's, else
// GitHub's for the default branch), and enforced, the checks GitHub
// requires on the PR's base with no run at the head (enforcedMissing): a
// known empty list leaves CI out; the head's stored checks decide where
// they can (requiredCIOf, where strict, the [[repo]] list of a repository
// that sets ci_dispatch, makes an entry with no run missing, never green;
// a required run that failed, or one skipped beside a failed job of its
// workflow file, failedRequired, fails it), and GitHub's rollup otherwise,
// a required list with no rollup at all (no check has run) waiting for it.
// An enforced check is missing either way, and goes before the rollup's
// failure, which may be a check nobody requires.
func openCIOf(pr store.PR, required store.RequiredChecks, strict bool, enforced []string) openCI {
	if required.Source != "" && len(required.Checks) == 0 {
		return openCI{}
	}
	ci := openCI{missing: enforced}
	if r, ok := requiredCIOf(pr, required.Checks, strict); ok {
		if failed := failedRequired(pr, required.Checks, r); len(failed) > 0 {
			return openCI{failed: missingWhat(ciEntries(failed)), failedRuns: failed, pending: r.running}
		}
		ci.pending, ci.missing = r.running, withMissing(r.missing, enforced)
	} else {
		switch deref(pr.CIState) {
		case "FAILURE", "ERROR":
			if len(enforced) == 0 {
				return openCI{failed: "CI"}
			}
		case "PENDING", "EXPECTED":
			ci.pending = true
		case "":
			ci.pending = len(required.Checks) > 0
		}
	}
	if len(ci.missing) > 0 && pr.CI != nil {
		ci.running = slices.ContainsFunc(pr.CI.Checks, func(c store.CheckResult) bool { return c.State == store.CheckPending })
	}
	return ci
}

// openFacts are what decideOpen decides from, all gathered by the look:
// the time, the PR, why it is off ("" = the look covers it), its attention
// kind while it needs attention, its loop's row (fixBad: one that does not
// parse) and a held fix (why it is held, heldFix.Why), the links of its
// stalemate, magnum's last review that stands at the head (nil: none), its
// pending replies, the read of GitHub (whole: one since the PR changed;
// failed: the last read failed; low: the GitHub budget is below the floor;
// pollStale: the watch's poll is behind (pollBehind);
// cut: what the read lacked), the items, the ledger, the settle's keys,
// CI, the approval and the commits after it, the base whose merge with
// the head could not be checked, and `magnum pause`.
type openFacts struct {
	now        time.Time
	pr         store.PR
	off        string
	attention  string
	fix        *FixState
	fixBad     bool
	held       string
	stalemate  []string
	sum        *store.ReviewSummary
	pending    int
	raised     bool // the look's own raise holds it (openLedger.raisedHere), not a loop's or a round's Needs you
	whole      bool
	failed     bool
	low        bool
	pollStale  bool
	cut        []string
	items      []openItem
	ledger     openLedger
	settle     openSettle
	ci         openCI
	approved   string
	afterCount int
	// mergeUnchecked is the base whose merge with the head the look's last
	// check could not check ("" = checked, or not looked at this head).
	mergeUnchecked string
	// pause is `magnum pause`'s words ("" = none: userPause), and
	// pauseUntil its end (zero: until `magnum resume`); pauses are the
	// pauses that ended (openPauses), oldest first.
	pause      string
	pauseUntil time.Time
	pauses     []openHold
	// ciWorkflow is [[repo]] ci_dispatch where a required check did not
	// run at the head ("" = magnum starts none), and ciStart the look's
	// start of it at the head (zero: none; fix_open_ci.go).
	ciWorkflow string
	ciStart    ciStart
	// ciRerun is the look's read of the runs at the head and its re-run of
	// the one that started no job, where a required check did not run at
	// the head (zero: none; ci_rerun.go).
	ciRerun ciRerun
	// ciLow: the identity [[repo]] ci_dispatch_as names, which starts
	// ciWorkflow, is below the GitHub budget floor (low is the poll
	// identity's).
	ciLow bool
	// decided: the attention is the fixer's decision calls alone, and the
	// whole read shows your decision in each one's thread (callsDecided),
	// so it stops no start (rule 2).
	decided bool
	// optional are the operator's optional calls the whole read shows open
	// (openOptional, fix_optional.go): no work, and named at the merge;
	// optionalP3 counts them against sum's P3s (optionalSplit).
	optional   []optionalCall
	optionalP3 p3Calls
	// described: magnum's review that stands at the head has a
	// `Description: ✗` line (reviewDescribes), which is review work.
	described bool
	// ciFix is what the look knows of a CI fix of the head's failed
	// required checks (ci_fix.go; zero: nothing).
	ciFix ciFixFacts
}

// decideOpen decides the verdict of f's PR by the rules of this file's
// comment, and returns it with the work a loop would start on (only for
// the fixing verdict of rule 9, a start) and what needs the operator.
func decideOpen(f openFacts) (OpenVerdict, []openItem, []openItem) {
	head := f.pr.HeadSHA
	short := textx.ShortSHA(head)
	v := OpenVerdict{Head: head, At: f.now, ReadAt: f.ledger.Read.At, Approved: f.approved, AfterApproval: f.afterCount}
	if v.Approved == "" {
		v.AfterApproval = 0
	}
	say := func(verdict, why string) OpenVerdict {
		v.Verdict, v.Why = verdict, why
		return v
	}
	if f.off != "" {
		v.Off = f.off
		return say(OpenOff, offWords(f.off)), nil, nil
	}
	work, needs := openWorkOf(f)
	v.Open = len(work) + len(needs)
	v.Needs = openNeeds(needs)
	switch {
	case f.attention != "" && !f.decided:
		return say(OpenNeedsYou, "needs your attention ("+f.attention+")"), nil, needs
	case f.fixBad:
		return say(OpenNeedsYou, fmt.Sprintf("the fix row of #%d cannot be read: `magnum abort` clears it", f.pr.Number)), nil, needs
	case f.fix != nil && f.fix.Phase == FixPhaseReviewing && f.fix.Mode == FixModeCI:
		return say(OpenFixing, "fixing, re-review of "+textx.ShortSHA(f.fix.Head)+" (the CI fix's push)"), nil, needs
	case f.fix != nil && f.fix.Phase == FixPhaseReviewing:
		return say(OpenFixing, "fixing, re-review of "+textx.ShortSHA(f.fix.Head)), nil, needs
	case f.fix != nil && f.fix.Confirm != nil: // fix_confirm.go: GitHub does not answer the settle
		return say(OpenFixing, "fixing, "+f.fix.Confirming()), nil, needs
	case f.fix != nil && f.fix.Mode == FixModeCI:
		return say(OpenFixing, ciRunning(f)), nil, needs
	case f.fix != nil:
		return say(OpenFixing, fmt.Sprintf("fixing, cycle %d of %d", f.fix.Cycle, f.fix.Cap)), nil, needs
	case f.held == heldBudget:
		return say(OpenWaits, "a fix waits for the GitHub budget"), nil, needs
	case f.held == heldPause:
		return say(OpenWaits, "a fix waits for the fixer's pause to end"), nil, needs
	case f.held != "":
		return say(OpenWaits, "a fix waits for the restart"), nil, needs
	}
	if len(f.stalemate) > 0 {
		v.Needs = append(v.Needs, linkNeeds("thread", f.stalemate)...)
		return say(OpenNeedsYou, fmt.Sprintf("magnum stopped arguing in %s", textx.Count(len(f.stalemate), "thread", "threads"))), nil, needs
	}
	if why, late, until := magnumDue(f); why != "" {
		if late {
			v.Needs = append(v.Needs, OpenNeed{Kind: "magnum_late", URL: f.pr.URL})
			v.Late = true
			return say(OpenNeedsYou, why), nil, needs
		}
		v.Until = until
		return say(OpenWaits, why), nil, needs
	}
	if f.sum == nil {
		v.Needs = append(v.Needs, OpenNeed{Kind: "magnum", URL: f.pr.URL})
		return say(OpenNeedsYou, "no review of magnum's stands at "+short+": `magnum review` reviews it"), nil, needs
	}
	if f.pending > 0 {
		return say(OpenWaits, "magnum re-decides "+textx.Count(f.pending, "reply", "replies")), nil, needs
	}
	switch {
	case len(f.cut) > 0:
		return say(OpenNeedsYou, "GitHub cut the read short ("+strings.Join(f.cut, ", ")+"): answer the open items by hand"), nil, needs
	case f.low && (!f.whole || f.failed || f.pollStale):
		return say(OpenWaits, "GitHub budget: reading the PR waits for it"), nil, needs
	case f.pollStale:
		return say(OpenUnknown, "the poll of GitHub is behind: the PR's row may be old"), nil, needs
	case f.failed:
		return say(OpenUnknown, "reading GitHub failed; trying again shortly"), nil, needs
	case !f.whole:
		return say(OpenUnknown, "reading GitHub"), nil, needs
	}
	onlyRebuttals := len(work) > 0 && !slices.ContainsFunc(work, func(it openItem) bool { return it.Kind != itemMagnum }) &&
		!reviewWork(*f.sum, f.optionalP3, f.described)
	if len(needs) > 0 && (len(work) == 0 || onlyRebuttals || !personsAlone(needs)) { // rule 8; a person's items alone wait beside work (rule 9)
		v.Leftovers = true
		return say(OpenNeedsYou, needsWords(needs)), nil, needs
	}
	settled, until := settleDone(f)
	if len(work) > 0 {
		return decideWork(f, v, work, needs, onlyRebuttals, settled, until)
	}
	if !settled {
		v.Until = until
		return say(OpenWaits, "settling until "+waitClock(until, f.now)+": a reviewer can still be at work"), nil, needs
	}
	switch {
	case f.ci.failed != "": // ci_fix.go: no CI fix starts now
		verdict, why, until, readBase, recheck := ciFixWords(f, short)
		if verdict == OpenNeedsYou {
			v.Needs = append(v.Needs, OpenNeed{Kind: "ci", URL: f.pr.URL})
		}
		v.Until, v.ReadCIBase, v.RecheckCIRead = until, readBase, recheck
		return say(verdict, why), nil, needs
	case len(f.ci.missing) > 0:
		return openMissingCI(f, v), nil, needs
	case f.ci.pending:
		return say(OpenWaits, "CI at "+short+ciAfter(f)), nil, needs
	case f.mergeUnchecked != "":
		return say(OpenUnknown, "the merge with "+f.mergeUnchecked+" could not be checked; trying again shortly"), nil, needs
	}
	why, optional := idleWords(f, short)
	v.Optional = optional
	return say(OpenIdle, why), nil, nil
}

// openWorkOf sorts f's items into work and needs (sortOpen), none of
// either while the items are not known (no whole read, a failed one, or
// one GitHub cut short); red required CI is work once nothing else is
// open (ciWorkOf).
func openWorkOf(f openFacts) (work, needs []openItem) {
	work, needs = sortOpen(f.items, f.ledger)
	if !f.whole || f.failed || len(f.cut) > 0 { // the items are not known: none counts
		work, needs = nil, nil
	} else if it, ok := ciWorkOf(f, work, needs); ok { // ci_fix.go: red required CI is work once nothing else is open
		work = append(work, it)
	}
	return work, needs
}

// decideWork is decideOpen's verdict on open work, v as decideOpen made it
// so far (rule 9 and what holds its start, in this order): magnum's
// replies alone wait for your answer, then the day's cap of loops, `magnum
// pause`, quiet hours, the hands off after your push and the settle
// (settled and until are settleDone's) hold the start; else a CI fix or a
// loop starts on work. A person's handed items (needs) wait beside the
// work, named in the words of each wait and of the loop's start.
func decideWork(f openFacts, v OpenVerdict, work, needs []openItem, onlyRebuttals, settled bool, until time.Time) (OpenVerdict, []openItem, []openItem) {
	head := f.pr.HeadSHA
	short := textx.ShortSHA(head)
	say := func(verdict, why string) OpenVerdict {
		v.Verdict, v.Why = verdict, why
		return v
	}
	ww, ci := workWords(work), len(work) == 1 && work[0].Kind == itemCI
	if ci {
		ww = ciWorkWords(f)
	}
	handsOff := f.pr.HeadChangedAt.Add(f.settle.handsOff)
	quietEnd, quiet := quietHoursNow(f.settle.quietHours, f.now)
	// waiting names a person's handed items beside the work, which wait
	// for the loop the work starts (rule 8's exception), as long as the
	// work itself waits too: raised now, they would hold the PR and no
	// loop would start on it.
	waiting := func(why string) string {
		if len(needs) == 0 {
			return why
		}
		return why + "; " + textx.Count(len(needs), "item waits", "items wait") + " for you: " + workWords(needs)
	}
	switch {
	case onlyRebuttals:
		v.Needs = append(v.Needs, openNeeds(work)...)
		return say(OpenNeedsYou, "magnum's reply waits for your answer: "+work[0].URL), nil, work
	case f.ledger.StartsSince(f.now.Add(-24*time.Hour)) >= f.settle.maxLoops:
		if len(needs) > 0 { // no loop starts today: rule 8, as with no work beside them
			v.Leftovers = true
			return say(OpenNeedsYou, needsWords(needs)), nil, needs
		}
		v.Needs = append(v.Needs, OpenNeed{Kind: "loop", URL: f.pr.URL})
		capped := fmt.Sprintf("%s today: f starts another", textx.Count(f.settle.maxLoops, "loop", "loops"))
		if ci { // the factory reads "f starts another"; c starts a CI fix by hand
			capped += ", c a CI fix"
		}
		return say(OpenNeedsYou, capped), nil, needs
	case f.pause != "" && f.pauseUntil.After(f.now):
		v.Until = f.pauseUntil
		return say(OpenWaits, waiting(f.pause+": "+ww+" waits until "+waitClock(f.pauseUntil, f.now))), nil, needs
	case f.pause != "":
		return say(OpenWaits, waiting(f.pause+": "+ww+" waits for `magnum resume`")), nil, needs
	case quiet:
		v.Until = quietEnd
		return say(OpenWaits, waiting(fmt.Sprintf("quiet hours (%s): %s waits until %s", f.settle.quietHours, ww,
			waitClock(quietEnd, f.now)))), nil, needs
	case !slices.Contains(f.ledger.FixerHeads, head) && f.now.Before(handsOff):
		v.Until = handsOff
		return say(OpenWaits, waiting(fmt.Sprintf("you pushed %s at %s; hands off until %s", short, waitClock(f.pr.HeadChangedAt, f.now),
			waitClock(handsOff, f.now)))), nil, needs
	case !settled:
		v.Until = until
		return say(OpenWaits, waiting("settling: "+ww+" waits until "+waitClock(until, f.now))), nil, needs
	case ci:
		return say(OpenFixing, fmt.Sprintf("starts a CI fix of %s at %s (CI fix %d of %d)", f.ci.failed, short, f.ciFix.starts+1, ciFixMax)), work, needs
	}
	return say(OpenFixing, waiting("starts a fix loop on "+workWords(work))), work, needs // the loop leaves the person's items alone
}

// idleWords are rule 12's words on a PR with nothing open: the head, who
// merges, the commits after the approval, the operator's optional calls
// (optional: their links, for the verdict) and the CI fix whose push made
// CI pass.
func idleWords(f openFacts, short string) (why string, optional []string) {
	why = "nothing open at " + short
	switch g := f.pr.ReviewGate; {
	case !OpenMergeIsYours(g):
		why += " · no approval yet"
	case g.Decision == "APPROVED":
		why += " · approved, merge is yours"
	default: // the base requires no review
		why += " · no review required, merge is yours"
	}
	if f.approved != "" && f.afterCount > 0 {
		why += " · " + textx.Count(f.afterCount, "commit", "commits") + " after the approval"
	}
	if len(f.optional) > 0 { // fix_optional.go: the operator's calls, seen at the merge
		optional = optionalLinks(f.optional)
		why += " · " + OptionalCallsWords(optional)
	}
	if c := f.ciFix; ciAfter(f) != "" && len(c.last.Entries) > 0 { // ci_fix.go: the CI fix's push passed
		why += fmt.Sprintf(" · CI fix %d of %d made %s pass", c.lastN, ciFixMax, missingWhat(c.last.Entries))
	}
	return why, optional
}

// ciFailedAt is rule 11's failure in words: "required check Completion
// failed at a1a1a1a", with how a run of magnum's ended where it decided
// (ciShape3, ci_fix.go). The factory screen's move reads these words (kind
// ci), and its test reads this format here.
func ciFailedAt(f openFacts, short string) string {
	s := fmt.Sprintf("%s failed at %s", f.ci.failed, short)
	if f.ci.how != "" {
		s += " (" + f.ci.how + ")"
	}
	return s
}

// OpenMergeIsYours reports whether GitHub's review gate g leaves the
// merge of an own PR with nothing open to the operator (rule 12's "merge
// is yours"): approved, or read with no decision (a base that requires no
// review); not before magnum read it (nil), nor while it still wants a
// review or changes. The board reads it with the look's idle verdict, as
// the factory screen's wait on your merge does.
func OpenMergeIsYours(g *store.ReviewGate) bool {
	return g != nil && (g.Decision == "APPROVED" || g.Decision == "")
}

// heldRestart is a drain's held fix as openFacts.held carries it (its
// heldFix.Why is "").
const heldRestart = "restart"

// offWords is why the look leaves a PR alone, by its off reason.
func offWords(off string) string {
	switch off {
	case OpenOffFixOff:
		return "fix is off (F)"
	case OpenOffDraft:
		return "a draft"
	case OpenOffFork:
		return "the head lives in a fork"
	case OpenOffIncludeOwn:
		return "its watch leaves your own PRs out (include_own)"
	case OpenOffMuted:
		return "muted: `magnum unmute` lets autofix work on it"
	case OpenOffBaseline:
		return "open before magnum first synced the repository"
	}
	return "magnum does not review it"
}

// magnumDue is rule 5: magnum has not reviewed the head (its reviewed head
// is another, or a round is due or running): the wait's words, whether it
// is late (needs_you) and when it ends where known. "" when magnum is done
// at the head. The round is due at the later of the head's move and its
// next eligible time; under `magnum pause`, which holds every round nobody
// forced, and inside [daemon] quiet_hours it is never late (no needs you,
// so no toast at night or while you paused it), and the late clock counts
// from the end of every pause and quiet window that began before the
// review was late (heldSince): one that held the round from starting gives
// it magnum_late_after anew from its end, and a review late before one
// began is late again at its end.
func magnumDue(f openFacts) (why string, late bool, until time.Time) {
	pr := f.pr
	due, running := slices.Contains(store.DueStates, pr.State), slices.Contains(store.InFlightStates, pr.State)
	if f.decided { // needs attention for the fixer's calls alone, which you decided: no round is due (rule 2)
		due = false
	}
	if deref(pr.ReviewedSHA) == pr.HeadSHA && !due && !running {
		return "", false, time.Time{}
	}
	short := textx.ShortSHA(pr.HeadSHA)
	since := pr.HeadChangedAt
	if pr.NextEligibleAt != nil && pr.NextEligibleAt.After(since) {
		since = *pr.NextEligibleAt
	}
	if pr.NextEligibleAt != nil && pr.NextEligibleAt.After(f.now) {
		until = *pr.NextEligibleAt
	}
	if !due {
		return "magnum reviews " + short, false, until
	}
	quietEnd, quiet := quietHoursNow(f.settle.quietHours, f.now)
	if quiet {
		until = later(until, quietEnd)
	}
	switch { // never late while either holds the round
	case f.pause != "" && !pr.Forced && f.pauseUntil.After(f.now):
		until = later(until, f.pauseUntil)
		return fmt.Sprintf("%s: magnum's review of %s waits until %s", f.pause, short, waitClock(f.pauseUntil, f.now)), false, until
	case f.pause != "" && !pr.Forced:
		return f.pause + ": magnum's review of " + short + " waits for `magnum resume`", false, until
	case quiet:
		return "magnum reviews " + short, false, until
	}
	if late := f.settle.late; late > 0 {
		holds := append(slices.Clone(f.pauses), quietWindows(f.settle.quietHours, since, f.now)...)
		slices.SortStableFunc(holds, func(a, b openHold) int { return a.from.Compare(b.from) })
		if since = heldSince(since, late, holds); !f.now.Before(since.Add(late)) {
			return fmt.Sprintf("magnum has not reviewed %s since %s", short, waitClock(since, f.now)), true, time.Time{}
		}
	}
	return "magnum reviews " + short, false, until
}

// openHold is a span that held a round nobody forced: a `magnum pause`
// that ended, or a window of [daemon] quiet_hours.
type openHold struct{ from, to time.Time }

// heldSince is a due review's late clock, due at since, moved past holds
// (oldest first, by from): each hold that began before the review was late
// (since+late) and ended after since moves since to its end.
func heldSince(since time.Time, late time.Duration, holds []openHold) time.Time {
	for _, h := range holds {
		if !h.from.Before(since.Add(late)) { // late before it began, and so before every later one
			break
		}
		since = later(since, h.to)
	}
	return since
}

// openHoldsBack is how far back rule 5 looks for the holds of a due
// review's round (heldSince).
const openHoldsBack = 30 * 24 * time.Hour

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// quietHoursNow reports whether [daemon] quiet_hours (spec) hold a full
// round at now, as they hold the look's start of a loop, and when they end.
func quietHoursNow(spec string, now time.Time) (time.Time, bool) {
	if !quietHoursHold(spec, now, false) {
		return time.Time{}, false
	}
	end := quietHoursEnd(spec, now)
	return end, !end.IsZero()
}

// settleDone is rule 2 of the settle: the head is settled once the PR saw
// no activity for settle_quiet and every settle_checks check that exists
// at the head is done (one with checks of another head is not known yet,
// and waited for), both waited for at most settle_max after the head moved
// or, when it began later, after the PR's current spell of activity began
// (the ledger's Spell): on a head older than settle_max, a reviewer's
// first comment of several still waits for quiet. until is when the wait
// ends at the latest.
func settleDone(f openFacts) (bool, time.Time) {
	pr := f.pr
	start := pr.HeadChangedAt
	if f.ledger.Spell.Start.After(start) {
		start = f.ledger.Spell.Start
	}
	capAt := start.Add(f.settle.max)
	if !f.now.Before(capAt) {
		return true, time.Time{}
	}
	activity := lastActivity(pr)
	quietAt := activity.Add(f.settle.quiet)
	if f.now.Before(quietAt) {
		if capAt.Before(quietAt) {
			return false, capAt
		}
		return false, quietAt
	}
	if checksPending(pr, f.settle.checks) {
		return false, capAt
	}
	return true, time.Time{}
}

// checksPending reports whether a check names lists (by check run or
// workflow name, any case) is still running at pr's head, or whether the
// head's checks are not known yet (stored for another commit, or none).
func checksPending(pr store.PR, names []string) bool {
	if len(names) == 0 {
		return false
	}
	if pr.CI == nil || pr.CI.SHA != pr.HeadSHA {
		return true
	}
	for _, c := range pr.CI.Checks {
		if c.State == store.CheckPending && slices.ContainsFunc(names, func(n string) bool {
			return strings.EqualFold(n, c.Name) || c.Workflow != "" && strings.EqualFold(n, c.Workflow)
		}) {
			return true
		}
	}
	return false
}

// personsAlone reports whether every item of needs is a person's (another
// reviewer who is not a bot): rule 8 lets rule 9 start beside them.
func personsAlone(needs []openItem) bool {
	return !slices.ContainsFunc(needs, func(it openItem) bool { return it.Class != classPerson })
}

// openNeeds are items as needs: kind and link only.
func openNeeds(items []openItem) []OpenNeed {
	var out []OpenNeed
	for _, it := range items {
		out = append(out, OpenNeed{Kind: it.Kind, URL: it.URL})
	}
	return out
}

// linkNeeds are urls as needs of kind.
func linkNeeds(kind string, urls []string) []OpenNeed {
	var out []OpenNeed
	for _, u := range urls {
		out = append(out, OpenNeed{Kind: kind, URL: u})
	}
	return out
}

// itemNouns names each kind of item in the verdict's words.
var itemNouns = map[string][2]string{
	itemThread:   {"thread", "threads"},
	itemReview:   {"review body", "review bodies"},
	itemComment:  {"comment", "comments"},
	itemMagnum:   {"reply of magnum's", "replies of magnum's"},
	itemFinding:  {"skipped finding", "skipped findings"},
	itemFindings: {"review of magnum's", "reviews of magnum's"},
	itemConflict: {"conflict with the base", "conflicts with the base"},
	itemCI:       {"CI fix", "CI fixes"},
}

// workWords counts items by kind and class: "1 review of magnum's and 1
// thread of a person".
func workWords(items []openItem) string {
	type group struct{ kind, class string }
	counts := map[group]int{}
	var order []group
	for _, it := range items {
		g := group{it.Kind, it.Class}
		if counts[g] == 0 {
			order = append(order, g)
		}
		counts[g]++
	}
	var parts []string
	for _, g := range order {
		noun := itemNouns[g.kind]
		s := textx.Count(counts[g], noun[0], noun[1])
		switch g.class {
		case classPerson:
			s += " of " + textx.Plural(counts[g], "a person", "people")
		case classBot:
			s += " of " + textx.Plural(counts[g], "a bot", "bots")
		}
		parts = append(parts, s)
	}
	return joinWords(parts)
}

// needsWords is the needs_you verdict of items left to the operator.
func needsWords(items []openItem) string {
	return textx.Count(len(items), "item needs you", "items need you") + ": " + workWords(items)
}

// joinWords joins parts as a sentence does: "a", "a and b", "a, b and c".
func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
