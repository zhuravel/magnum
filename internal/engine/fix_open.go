package engine

// autofix = "open"'s look: once a tick, before dispatch, every own open PR
// of a watch whose autofix is "open" gets a verdict (KVPROpen, decideOpen in
// fix_open_verdict.go), and a fix loop starts by itself on one with work
// once its reviewers settled. The look reads GitHub only when the PR's read
// fingerprint (head, updatedAt, activity, the last review) changed, 30
// minutes after its last read as a safety net (an edited comment may not
// move updatedAt), and 5 minutes after a failed one; never below the
// GitHub floor, at most openReadsPerTick PRs a tick (the oldest read
// first), and only where the read can move the verdict (readWanted). The
// base a PR merges into is fetched once a tick per clone, and each PR's
// head is checked against it every 30 minutes. The look is the only writer
// of its rows; it learns of every fix loop, by hand or its own, from the
// loop's row while it runs and from its fix.start and fix.pushed events
// once it ended: the keys its fixers had (the external files they got, never
// the look's own read), the review they worked on and the heads they
// pushed. A dry-run daemon reads and decides, but writes no row and starts
// nothing.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// openReadsPerTick caps the PRs whose GitHub read one tick makes.
	openReadsPerTick = 4
	// openReadEvery is the safety read of an unchanged PR.
	openReadEvery = 30 * time.Minute
	// openReadRetry is how soon a failed read is tried again.
	openReadRetry = 5 * time.Minute
	// openConflictEvery is how often a PR's head is checked against its base.
	openConflictEvery = 30 * time.Minute
	// openFixerHeadsMax is how many of the fixer's pushed heads the ledger keeps.
	openFixerHeadsMax = 10
	// openEventsRead is how many of a PR's newest events the look reads for
	// the loops that ended since its last look.
	openEventsRead = 200
)

// openCand is an own PR the look covers this tick: its row, repository,
// watch and ledger (raw: as stored, "" for none), and magnum's last review
// that stands at its head (openSummary).
type openCand struct {
	pr     store.PR
	repo   store.Repo
	w      *config.Watch
	ledger openLedger
	raw    string
	sum    *store.ReviewSummary
}

// autofixOpen is the tick's look (this file's comment). A PR of a watch
// that is not "open" gets no verdict, and a row an earlier look left is
// deleted, so a switch back to "own" leaves no stale verdict.
func (e *Engine) autofixOpen(ctx context.Context) error {
	e.openReads, e.openSlotStarts = map[int64]externalRead{}, map[string]int{}
	e.mu.Lock()
	e.openFetches = map[string]error{}
	e.mu.Unlock()
	defer func() { // a request of the next tick fetches again
		e.openReads, e.openSlotStarts = nil, nil
		e.mu.Lock()
		e.openFetches = nil
		e.mu.Unlock()
	}()
	prs, err := e.st.ListPRs(ctx, store.PRFilter{GHOpen: true})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: list the open PRs")
		return err
	}
	repos := map[int64]store.Repo{}
	var cands []openCand
	for _, pr := range prs {
		if !e.ownPR(pr) {
			continue
		}
		repo, ok := repos[pr.RepoID]
		if !ok {
			if repo, err = e.st.RepoByID(ctx, pr.RepoID); err != nil {
				continue
			}
			repos[pr.RepoID] = repo
		}
		w := e.cfg.WatchFor(repo.FullName())
		if w == nil || e.cfg.AutofixFor(w) != config.AutofixOpen {
			if _, ok := e.getKV(ctx, KVPROpen(pr.ID)); ok && !e.d.DryRun {
				e.delKV(ctx, KVPROpen(pr.ID))
			}
			continue
		}
		// fix_owner.go: a PR magnum does not fix gets no look, which starts loops and writes the ledger the factory imported
		if err := e.ownerRefusal(ctx, fmt.Sprintf("%s#%d", repo.FullName(), pr.Number), pr.ID); err != nil {
			if e.logOnce(fmt.Sprintf("open-owner:%d", pr.ID), err.Error(), e.now()) {
				e.log.Info("autofix open: no look at the PR", "subject", prSubject(repo, pr.Number), "why", err.Error())
			}
			continue
		}
		c := openCand{pr: pr, repo: repo, w: w}
		if raw, ok := e.getKV(ctx, kvPRFixSeen(pr.ID)); ok {
			c.raw = raw
			if err := json.Unmarshal([]byte(raw), &c.ledger); err != nil {
				e.log.Warn("autofix open: the PR's ledger cannot be read; it starts over", "subject", prSubject(repo, pr.Number), "err", err)
				c.ledger = openLedger{}
			}
		}
		cands = append(cands, c)
	}
	var reviewed []int64
	for _, c := range cands {
		if deref(c.pr.ReviewedSHA) == c.pr.HeadSHA {
			reviewed = append(reviewed, c.pr.ID)
		}
	}
	sums, err := e.st.LastReviewSummaries(ctx, reviewed)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: magnum's last reviews")
	}
	for i := range cands {
		if sum, ok := sums[cands[i].pr.ID]; ok {
			cands[i].sum = e.openSummary(ctx, cands[i].pr, sum)
		}
	}
	slices.SortStableFunc(cands, func(a, b openCand) int { return a.ledger.Read.At.Compare(b.ledger.Read.At) })
	reads, pauses := 0, e.openPauses(ctx, e.now())
	for i := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.lookOpen(ctx, &cands[i], pauses, &reads)
	}
	return nil
}

// openPauses are the `magnum pause` spans that ended within openHoldsBack
// of now, oldest first, read from the daemon.paused and daemon.resumed
// events (an expiry writes one too): rule 5's late clock counts from the
// end of one that held a review (magnumDue). A pause renewed while it ran
// begins at its first event, and a resume with no pause before it (one that
// lifted nothing, or an infrastructure pause alone) is no span.
func (e *Engine) openPauses(ctx context.Context, now time.Time) []openHold {
	evs, err := e.st.EventsOfKindsSince(ctx, now.Add(-openHoldsBack), "daemon.paused", "daemon.resumed")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: the pauses that ended")
		return nil
	}
	var holds []openHold
	var from time.Time
	for _, ev := range evs { // in the order written
		switch {
		case ev.Kind == "daemon.paused" && from.IsZero():
			from = ev.At
		case ev.Kind == "daemon.resumed" && !from.IsZero():
			holds = append(holds, openHold{From: from, To: ev.At})
			from = time.Time{}
		}
	}
	return holds
}

// openSettle are the configuration's keys of autofix = "open".
func (e *Engine) openSettle() openSettle {
	p := e.cfg.Pipeline
	return openSettle{Quiet: p.SettleQuiet.Duration, Max: p.SettleMax.Duration, Late: p.MagnumLateAfter.Duration,
		HandsOff: p.HandsOffAfterPush.Duration, Checks: p.SettleChecks, MaxLoops: cmp.Or(p.AutofixMaxLoops, config.DefaultAutofixMaxLoops),
		QuietHours: e.cfg.Daemon.QuietHours}
}

// quietRules is the configuration's.
func (e *Engine) quietRules() quietRules {
	return quietRules{logins: e.cfg.Pipeline.QuietLogins, markers: e.cfg.Pipeline.QuietMarkers}
}

// lookOpen is the look at one PR: its facts, the loops that ended, the
// read and the base check when due, the verdict, a start, and the rows.
// pauses are openPauses'.
func (e *Engine) lookOpen(ctx context.Context, c *openCand, pauses []openHold, reads *int) {
	pr, repo := c.pr, c.repo
	now := e.now()
	l := c.ledger
	f := openFacts{Now: now, PR: pr, Settle: e.openSettle(), Off: e.openOff(ctx, pr, *c.w)}
	hasRow := e.openLoopRow(ctx, repo, &l, &f)
	if raw, ok := e.getKV(ctx, KVPRFixHeld(pr.ID)); ok {
		f.Held = heldWhyOf(raw)
	}
	if pr.State == store.PRNeedsAttention {
		kind, _ := e.getKV(ctx, KVPRAttention(pr.ID))
		f.Attention = cmp.Or(kind, "attention")
	}
	if raw, ok := e.getKV(ctx, KVPRStalemate(pr.ID)); ok {
		if s, ok := ParseStalemate(raw); ok {
			for _, t := range s.Threads {
				f.Stalemate = append(f.Stalemate, t.URL)
			}
		}
	}
	f.Pending = len(pr.PendingReplies())
	f.Raised = l.RaisedHere(pr, f.Attention) // fix_open_needs.go
	f.Sum = c.sum
	if f.Pause = e.userPause(ctx); f.Pause != "" { // it holds the look's starts, as it held "own"'s rounds
		f.PauseUntil, _ = e.kvTime(ctx, store.KVDaemonPausedUntil)
	}
	f.Pauses = pauses
	fp := openPrint(pr)
	_, f.Low = e.githubLow(c.w.PollIdentity)
	f.PollStale = e.pollBehind(ctx, *c.w, now)
	e.openLookRead(ctx, c, &l, &f, fp, hasRow, reads)
	if c := l.Conflict; c.Unchecked && c.Head == pr.HeadSHA {
		f.MergeUnchecked = c.Base
	}
	f.CI = e.openCI(ctx, repo.FullName(), pr)
	f.CIWorkflow, f.CIStart = e.openCIStartOf(ctx, repo.FullName(), pr, f.CI) // fix_open_ci.go
	if len(f.CI.Missing) > 0 {
		f.CIRerun = e.ciRerunAt(ctx, pr)            // ci_rerun.go
		f.CI = ciShape3(f.CI, f.CIStart, f.CIRerun) // ci_fix.go: magnum's run of the head that ended failure
	}
	_, f.CILow = e.githubLow(e.cfg.CIDispatchAs(repo.FullName()))
	f.Approved, f.AfterCount = e.openApproval(ctx, *c.w, pr, &l)
	l.Starts = slices.DeleteFunc(l.Starts, func(at time.Time) bool { return now.Sub(at) >= 24*time.Hour })
	if !e.d.DryRun && e.openLift(ctx, repo, pr, &l, f) { // fix_open_needs.go: you answered what the look raised, or its reviewer approved it
		pr.State, pr.LastError = store.PRReviewed, nil
		f.PR, f.Attention, f.Raised = pr, "", false
		if f.Pending > 0 { // replies.go: the replies that waited behind the raise get their round, as after a round's end
			if err := e.considerReplies(ctx, repo, *c.w, pr.ID, now); err != nil {
				e.log.Info("autofix open: the replies after the lift", "subject", prSubject(repo, pr.Number), "err", err)
			}
		}
	}
	if f.Whole && !f.Failed && len(f.Cut) == 0 {
		openPrune(&l, f.Items)
	}
	// fix_open_needs.go: pushback counts from the loop's hand-off, however late the look reads it
	l.SeedWaited(f.Items)
	gap := max(f.Settle.Quiet, openStalePolls*e.cfg.Daemon.PollInterval.Duration) // longer than the looks' own spacing
	l.Spell = l.Spell.Next(pr, f.Settle.Quiet, gap, now, f.Fix != nil || f.FixBad || f.Held != "")
	f.Ledger = l
	f.CIFix = e.ciFixFactsOf(ctx, repo, pr, f, l) // ci_fix.go
	v, work, needs := decideOpen(f)
	if !e.d.DryRun && v.ReadCIBase { // ci_fix.go: rule 11 reads the base's CI of the failed required check, once per head, failed set and tip
		f.CIFix = e.readCIBase(ctx, repo, *c.w, pr, f, l)
		v, work, needs = decideOpen(f)
	}
	acts := e.ciActions(e.ciDispatchIdentity(repo.FullName(), *c.w)) // ci_actions.go: one token for the look's Actions calls
	defer acts.end()
	if !e.d.DryRun && (v.ReadRuns || v.Rerun) { // ci_rerun.go: rule 11 reads the head's runs, re-runs the one that started no job
		v, work = e.openCIRerun(ctx, acts, repo, pr, &f, v, work)
	}
	ci := v.Verdict == OpenFixing && len(work) == 1 && work[0].Kind == itemCI
	// ci_failures.go: why it failed, once, before the start, or again where a read of a run it named listed no failed job
	if !e.d.DryRun && (ci || v.RecheckCIRead) && e.ensureCIRead(ctx, acts, repo, pr, &f) {
		v, work, needs = decideOpen(f)
		ci = v.Verdict == OpenFixing && len(work) == 1 && work[0].Kind == itemCI
	}
	switch {
	case e.d.DryRun:
	case v.Leftovers && e.openRaise(ctx, repo, pr, &l, needs): // fix_open_needs.go
		pr.State = store.PRNeedsAttention
		f.PR, f.Attention, f.Ledger = pr, attention.KindFix, l
		v, work, _ = decideOpen(f)
	case v.Late:
		e.openLate(ctx, repo, pr, &l, v.Why)
	case v.Dispatch: // fix_open_ci.go: a required check did not run at the head
		if s, ok := e.openStartCI(ctx, acts, repo, pr, f.CIWorkflow, missingWhat(f.CI.Missing)); ok {
			f.CIStart = s
			v, work, _ = decideOpen(f)
		}
	}
	acts.end() // before the fixer starts: the deferred end does nothing more
	// fix_open_needs.go: a change request their reviewers make from now on waits for you, in the loop below too
	l.NoteWaited(needs, now)
	// a person's handed items wait, those their reviewer accepted, and their reviewers' pushback: a loop's fixer leaves them alone
	waits := l.LoopWaits(f.Items, needs)
	switch {
	case ci: // ci_fix.go: the record first, so a restart never starts a fourth; its next cycles, on findings, read the other reviewers too
		v = e.openStart(ctx, repo, pr, &l, v, work, FixState{Mode: FixModeCI, CIChecks: ciEntries(f.CI.FailedRuns), Waits: waits}, fp)
	case v.Verdict == OpenFixing && len(work) > 0:
		v = e.openStart(ctx, repo, pr, &l, v, work, FixState{Waits: waits}, fp)
	}
	e.openWrite(ctx, pr, c.raw, l, v)
}

// openLoopRow is the look's read of the PR's fix loop into the facts: its
// row (fix, or fixBad for one that cannot be read), a reviewing loop's head
// among the ledger's fixer heads, or with no row the loops that ended since
// the last look (openLoopsEnded). The look's first look at the PR starts
// the ledger's count of loops (Since) now, or at the start of the loop
// that runs, which then counts whole. It reports whether the PR has a row.
func (e *Engine) openLoopRow(ctx context.Context, repo store.Repo, l *openLedger, f *openFacts) bool {
	pr, now := f.PR, f.Now
	st, _, hasRow, rowErr := e.fixRow(ctx, pr.ID)
	first := l.Since.IsZero() // the look's first look at the PR: it tracks the loops from here on
	if first {
		l.Since = now
		if hasRow && !st.StartedAt.IsZero() { // the loop that runs now counts whole
			l.Since = st.StartedAt
		}
	}
	switch {
	case rowErr != nil:
		f.FixBad = true
	case hasRow:
		f.Fix = &st
		if st.Phase == FixPhaseReviewing {
			l.FixerHeads = addFixerHead(l.FixerHeads, st.Head)
		}
	default:
		if e.loopsToRead(pr.ID) {
			e.openLoopsEnded(ctx, repo, pr, l, first)
		}
	}
	return hasRow
}

// openLookRead is the look's read of GitHub and its check of the merge with
// the base while the verdict wants them (readWanted; a read only when due,
// above the budget floor and within the tick's openReadsPerTick, counted in
// reads), then the items of a whole read into the facts: the others',
// magnum's (its review handed to no loop while none runs: hasRow), the
// operator's optional calls and your decisions on the fixer's calls. fp is
// openPrint's.
func (e *Engine) openLookRead(ctx context.Context, c *openCand, l *openLedger, f *openFacts, fp string, hasRow bool, reads *int) {
	pr, repo, now := f.PR, c.repo, f.Now
	if readWanted(*f) {
		if openReadDue(l.Read, fp, pr.HeadSHA, now) && !f.Low && *reads < openReadsPerTick {
			*reads++
			e.openRead(ctx, repo, *c.w, pr, l, fp, now)
		}
		e.openConflictCheck(ctx, repo, pr, l, now)
	}
	f.Whole, f.Failed = openWhole(l.Read, fp, pr.HeadSHA)
	if f.Whole {
		f.Cut = l.Read.Cut
		handed := !hasRow && f.Sum != nil && l.HandedReview(f.Sum.RunID)
		var optional map[int64]bool
		if !f.Failed && len(f.Cut) == 0 { // fix_optional.go: the operator's optional calls, as the read shows them
			f.Optional, optional = e.openOptional(ctx, pr.ID, l.Read.Mine, f.Sum)
			f.OptionalP3 = optionalSplit(l.Read.Mine, f.Sum, optional)
		}
		f.Described = f.Sum != nil && e.reviewDescribed(repo, pr.Number, f.Sum.SHA) // fix.go: the review's Description line is work
		f.Items = append(slices.Clone(l.Read.Others), magnumItemsOf(l.Read.Mine, f.Sum, handed, openFixConflict(l.Conflict, pr.HeadSHA), pr.HeadSHA, optional,
			f.Described)...)
		if f.Attention == attention.KindFix && !f.Failed && len(f.Cut) == 0 { // fix_decide.go: your decisions answer the fixer's calls
			f.Decided = callsDecided(deref(pr.LastError), func(root int64) bool { return decisionOf(f.Items, root) })
		}
	}
}

// openPrune drops from the ledger's handed and raised keys those that a
// whole read no longer shows open (items): resolved, answered, deleted, or
// a thread with a newer latest comment, whose new key is new work. A key
// handed after the read was made stays: the read may predate the item
// (one that arrived during the loop, in the fixer's file).
func openPrune(l *openLedger, items []openItem) {
	open := map[string]bool{}
	for _, it := range items {
		open[it.Key] = true
	}
	maps.DeleteFunc(l.Handed, func(k string, at time.Time) bool { return !open[k] && l.Read.At.After(at) })
	l.Raised = slices.DeleteFunc(l.Raised, func(k string) bool { return !open[k] })
}

// pollBehind reports whether w's poll of GitHub (WatchPoll) is behind,
// so that the PR rows of its PRs may not show what happened on GitHub
// since and the look starts nothing on them: no good poll known, two
// polls in a row failed, or the last good one is over openStalePolls poll
// intervals old (polls that did not run or count: the Mac slept, no
// client). One failed poll is not: GitHub's bad minute leaves the rows a
// tick old, and a verdict that flapped to unknown with every HTTP 502
// would be rewritten twice for nothing.
func (e *Engine) pollBehind(ctx context.Context, w config.Watch, now time.Time) bool {
	raw, ok := e.getKV(ctx, store.KVWatchPoll(strings.ToLower(w.Owner)))
	if !ok {
		return true
	}
	p, ok := ParseWatchPoll(raw)
	return !ok || p.LastOK.IsZero() || p.Failures >= 2 || now.Sub(p.LastOK) > openStalePolls*e.cfg.Daemon.PollInterval.Duration
}

// openOff is why the look leaves pr alone ("" = it covers it): a draft, a
// fork, a watch that reviews no own PRs, a muted PR (a mute stops every
// automatic round), a baseline or ineligible PR, fix switched off (an
// opt-out that cannot be read counts as one: never a loop over it).
func (e *Engine) openOff(ctx context.Context, pr store.PR, w config.Watch) string {
	switch {
	case pr.IsDraft:
		return OpenOffDraft
	case pr.IsCrossRepo:
		return OpenOffFork
	case !w.OwnIncluded():
		return OpenOffIncludeOwn
	case pr.Muted:
		return OpenOffMuted
	case pr.State == store.PRBaseline:
		return OpenOffBaseline
	case pr.State == store.PRIneligible:
		return OpenOffIneligible
	}
	if _, off, err := e.fixOff(ctx, pr.ID); err != nil || off {
		return OpenOffFixOff
	}
	return ""
}

// heldWhyOf is why the held fix raw (a KVPRFixHeld value) is held, as
// openFacts.held carries it.
func heldWhyOf(raw string) string {
	var h heldFix
	if json.Unmarshal([]byte(raw), &h) != nil || h.Why == "" {
		return heldRestart
	}
	return h.Why
}

// openSummary is sum, magnum's last review of pr, when it stands at the
// head (its own head's, or an older one's whose verdict the head kept:
// reviewStood); nil when magnum has not reviewed the head, or it does not
// stand.
func (e *Engine) openSummary(ctx context.Context, pr store.PR, sum store.ReviewSummary) *store.ReviewSummary {
	if deref(pr.ReviewedSHA) != pr.HeadSHA {
		return nil
	}
	if sum.SHA != pr.HeadSHA {
		if stood, err := e.reviewStood(ctx, pr, sum); err != nil || !stood {
			return nil
		}
	}
	return &sum
}

// openPrint is pr's read fingerprint: what moves when anything happens on
// the PR (its head, GitHub's updatedAt, its activity, magnum's last review).
func openPrint(pr store.PR) string {
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	var review int64
	if pr.LastReviewID != nil {
		review = *pr.LastReviewID
	}
	return fmt.Sprintf("%s|%s|%s|%d", pr.HeadSHA, at(pr.GHUpdatedAt), at(pr.ActivityAt), review)
}

// openReadDue reports whether the look reads GitHub for a PR whose last
// read is r, at the fingerprint fp and head: never read, the PR changed,
// or the safety read is due; a read that failed is tried again
// openReadRetry later.
func openReadDue(r openRead, fp, head string, now time.Time) bool {
	if !r.FailedAt.IsZero() && r.FailedAt.After(r.At) && now.Sub(r.FailedAt) < openReadRetry {
		return false
	}
	return r.At.IsZero() || r.Print != fp || r.Head != head || now.Sub(r.At) >= openReadEvery ||
		!r.FailedAt.IsZero() && r.FailedAt.After(r.At)
}

// openWhole reports whether r is a whole read of the PR at the
// fingerprint fp and head (whole), and whether the last read since failed
// (failed).
func openWhole(r openRead, fp, head string) (whole, failed bool) {
	failed = !r.FailedAt.IsZero() && r.FailedAt.After(r.At)
	return !r.At.IsZero() && r.Print == fp && r.Head == head, failed
}

// readWanted reports whether the look's read of GitHub can change f's
// verdict: the look covers the PR, no loop runs or is held, no stalemate
// waits, magnum is done at the head (one that needs attention after a
// review of the head is) and has no replies to re-decide, unless the
// look's own raise holds the PR: no reply round starts on a PR in needs
// attention (considerReplies), so the read and the lift it allows go
// first, and the replies get their round after the lift.
// Otherwise the read waits, and spends no GitHub budget on a PR whose
// verdict it cannot move.
func readWanted(f openFacts) bool {
	if f.Off != "" || f.Fix != nil || f.FixBad || f.Held != "" || len(f.Stalemate) > 0 || f.Pending > 0 && !f.Raised {
		return false
	}
	if f.PR.State == store.PRNeedsAttention { // its leftovers, and the lift (fix_open_needs.go), need the read
		return deref(f.PR.ReviewedSHA) == f.PR.HeadSHA
	}
	why, _, _ := magnumDue(f)
	return why == ""
}

// openRead reads pr's other reviewers and magnum's threads from GitHub,
// as the fix job does (readExternal, metered as opFixExternal), into the
// ledger's read, and keeps it for this tick (openReads): a start in this
// tick hands it to prepareFix, which reads GitHub no second time.
func (e *Engine) openRead(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, l *openLedger, fp string, now time.Time) {
	login := ""
	if src := e.d.Identities[pr.Identity]; src != nil {
		login = src.Login()
	}
	read, err := e.readExternal(ctx, &fixJob{repo: repo, watch: w, pr: pr, login: login})
	if err != nil {
		l.Read.FailedAt, l.ReadFails = now, l.ReadFails+1
		e.log.Info("autofix open: the PR's threads and comments could not be read", "subject", prSubject(repo, pr.Number), "err", err,
			"failures", l.ReadFails)
		return
	}
	l.ReadFails = 0
	l.Read = openRead{Print: fp, At: now, Head: pr.HeadSHA, Cut: read.cut, Others: othersItemsOf(read, e.quietRules()), Mine: read.mine}
	e.openReads[pr.ID] = read
}

// openFetchKey is the memo key of a fetch of base into clone.
func openFetchKey(clone, base string) string { return clone + "\x00" + base }

// fetchedThisTick reports whether the look fetched base into clone in this
// tick: prepareFix's look at the base then fetches it no second time.
func (e *Engine) fetchedThisTick(clone, base string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	err, ok := e.openFetches[openFetchKey(clone, base)]
	return ok && err == nil
}

// openFetched reports whether the look fetched a base into a clone (key)
// in this tick (done), and that fetch's error.
func (e *Engine) openFetched(key string) (done bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	err, done = e.openFetches[key]
	return done, err
}

// noteOpenFetch records the look's fetch of a base into a clone (key) in
// this tick, with its error.
func (e *Engine) noteOpenFetch(key string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.openFetches != nil {
		e.openFetches[key] = err
	}
}

// openConflictCheck checks pr's head against the tip of the base it merges
// into (its own base_ref: a stacked PR's feature branch), every
// openConflictEvery and whenever the head or the base changed, in the PR's
// slot, else its clone. The base is fetched once per clone in the tick (a
// dry run fetches nothing, and checks the tip last fetched); a check that
// fails leaves the conflict unknown (Unchecked: no conflict item, and the
// verdict is never idle) and is tried again openReadRetry later. A PR
// nothing can check (no git, no checkout, a base name magnum does not pass
// to a shell) has no conflict item and is not unknown for it.
func (e *Engine) openConflictCheck(ctx context.Context, repo store.Repo, pr store.PR, l *openLedger, now time.Time) {
	base := cmp.Or(deref(pr.BaseRef), repo.DefaultBranch)
	every := openConflictEvery
	if l.Conflict.Unchecked {
		every = openReadRetry
	}
	if c := l.Conflict; c.Head == pr.HeadSHA && c.Base == base && now.Sub(c.At) < every {
		return
	}
	dir, clone := e.cloneOf(repo), ""
	if slot, has, err := e.slotOf(ctx, pr.ID); err == nil && has && slot.Path != "" {
		dir, clone = slot.Path, slot.MainClone
	}
	clone = cmp.Or(clone, dir)
	next := openConflict{Head: pr.HeadSHA, Base: base, At: now}
	if e.d.Git == nil || dir == "" || !gitx.ShellSafeRef(base) {
		l.Conflict = next
		return
	}
	if !e.d.DryRun {
		key := openFetchKey(clone, base)
		done, ferr := e.openFetched(key)
		if !done {
			ferr = e.fetchBase(ctx, clone, base, fixRequestFetch)
			e.noteOpenFetch(key, ferr)
		}
		if ferr != nil {
			next.Unchecked = true
			l.Conflict = next
			return
		}
	}
	fc, err := e.baseConflict(ctx, repo, pr, dir, clone, pr.HeadSHA, -1)
	if err != nil {
		next.Unchecked = true
	} else {
		next.Tip, next.Paths = fc.SHA, fc.Paths
		if next.Tip == "" { // a clean merge names no tip: the base's CI is read at the one fetched (ci_fix.go)
			next.Tip, _ = e.d.Git.RevParse(ctx, dir, "refs/remotes/origin/"+base)
		}
	}
	l.Conflict = next
}

// openFixConflict is the conflict c as the fix job reads it (zero: none, or one
// checked at another head).
func openFixConflict(c openConflict, head string) fixConflict {
	if c.Head != head || len(c.Paths) == 0 {
		return fixConflict{}
	}
	return fixConflict{Base: c.Base, SHA: c.Tip, Paths: c.Paths}
}

// openCI is rule 11's CI of pr's head, of repository full (openCIOf),
// with the checks the configuration ignores left out, and the required entries that name only
// such checks (seenRequired; none left: the rollup derived from the rest
// decides, as without a list); an entry with no run at the head is missing
// where GitHub requires it on the PR's base (enforcedMissing, which reads
// the checks as GitHub gave them: an ignored one ran) or the repository's
// [[repo]] list names it and its ci_dispatch is set.
func (e *Engine) openCI(ctx context.Context, full string, pr store.PR) openCI {
	required, err := e.st.RequiredChecks(ctx, full, e.cfg.RequiredChecks(full))
	if err != nil {
		return openCI{Pending: true}
	}
	g, _, err := e.st.GitHubRequiredChecks(ctx, full)
	if err != nil {
		return openCI{Pending: true}
	}
	ignore := e.cfg.IgnoreChecks(full)
	if len(required.Checks) > 0 {
		if required.Checks = seenRequired(pr, required.Checks, ignore); len(required.Checks) == 0 {
			required = store.RequiredChecks{}
		}
	}
	strict := required.Source == store.RequiredFromConfig && e.cfg.CIDispatch(full) != ""
	return openCIOf(pr.IgnoringChecks(ignore), required, strict, enforcedMissing(pr, g))
}

// commitLister lists the commits of to that from lacks (gitx.Client's
// CommitsAfter); the look counts the commits after a person's approval
// with it where the engine's git has it.
type commitLister interface {
	CommitsAfter(ctx context.Context, dir, from, to string, firstParent bool) ([]gitx.ListedCommit, error)
}

// openApproval is the newest approval of pr by a person (not one of
// magnum's logins), as its short SHA, and the commits on the head after it
// (rule A3; -1 when they cannot be counted: no slot, or a git that cannot
// list them). The count is kept in the ledger by approved commit and head,
// so an unchanged PR costs no git call.
func (e *Engine) openApproval(ctx context.Context, w config.Watch, pr store.PR, l *openLedger) (string, int) {
	own := e.ownLogins(ctx, w, pr)
	var best *store.LatestReview
	for i, r := range pr.LatestReviews {
		if r.State != "APPROVED" || r.CommitSHA == "" || slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, r.Login) }) {
			continue
		}
		if best == nil || r.SubmittedAt != nil && (best.SubmittedAt == nil || r.SubmittedAt.After(*best.SubmittedAt)) {
			best = &pr.LatestReviews[i]
		}
	}
	if best == nil {
		l.Approval = openApprovalCount{}
		return "", 0
	}
	short := textx.ShortSHA(best.CommitSHA)
	if best.CommitSHA == pr.HeadSHA {
		return short, 0
	}
	if a := l.Approval; a.SHA == best.CommitSHA && a.Head == pr.HeadSHA {
		return short, a.After
	}
	n := -1
	if lister, ok := e.d.Git.(commitLister); ok {
		if slot, has, err := e.slotOf(ctx, pr.ID); err == nil && has && slot.Path != "" {
			if cs, err := lister.CommitsAfter(ctx, slot.Path, best.CommitSHA, pr.HeadSHA, true); err == nil {
				n = len(cs)
			}
		}
	}
	l.Approval = openApprovalCount{SHA: best.CommitSHA, Head: pr.HeadSHA, After: n}
	return short, n
}

// openLoopsEnded reads, while pr has no loop, the fix loops that ended
// since the look's last one from the PR's events (the ledger's Cursor):
// each fix.start names the review its cycle fixed (review_run) and its
// report, beside which its fixer's external file lists the other
// reviewers' items it got; each fix.pushed the head the fixer pushed, and
// each fix.parent_merged and fix.parent_pushed the merge of the parent the
// daemon pushed (hands_off_after_push never holds a head magnum made). The
// keys of the items in those files that were unanswered, the review, and
// magnum's threads and the conflict of the look's last read are handed;
// the review is one a loop worked on; the pushed heads are the fixer's.
// Items that showed up during a loop and are in no file are not handed.
// A loop that started before the look tracked the PR (first, its first
// look, or before Since: an f while autofix was not "open") hands
// nothing: what it left open is work again, as for any loop the look did
// not see; its pushed heads are still the fixer's.
func (e *Engine) openLoopsEnded(ctx context.Context, repo store.Repo, pr store.PR, l *openLedger, first bool) {
	evs, err := e.st.EventsBySubject(ctx, prSubject(repo, pr.Number), openEventsRead)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "autofix open: the PR's events", "pr", pr.ID)
		return
	}
	now := e.now()
	hand := func(key string) {
		if l.Handed == nil {
			l.Handed = map[string]time.Time{}
		}
		if _, ok := l.Handed[key]; !ok {
			l.Handed[key] = now
		}
	}
	cursor, loops := l.Cursor, 0
	for _, ev := range evs { // oldest first (EventsBySubject's order), so the bounded lists keep the newest
		if ev.ID <= l.Cursor {
			continue
		}
		cursor = max(cursor, ev.ID)
		var d struct {
			Report    string    `json:"report"`
			ReviewRun string    `json:"review_run"`
			Head      string    `json:"head"`
			Merge     string    `json:"merge"`
			NoSlot    bool      `json:"no_slot"`
			StartedAt time.Time `json:"started_at"`
		}
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &d)
		}
		switch ev.Kind {
		case "fix.start":
			if first || ev.At.Before(l.Since) { // before the look tracked the PR
				continue
			}
			loops++
			if d.ReviewRun != "" {
				hand(reviewKey(d.ReviewRun))
				if !slices.Contains(l.Reviews, reviewKey(d.ReviewRun)) {
					l.Reviews = append(l.Reviews, reviewKey(d.ReviewRun))
				}
			}
			if d.Report == "" {
				continue
			}
			x, ok := readExternalFile(filepath.Dir(d.Report))
			if !ok {
				continue
			}
			for _, t := range x.Threads {
				if !t.Answered && len(t.Comments) > 0 {
					hand(threadKey(t.CommentID, t.Comments[len(t.Comments)-1].ID))
				}
			}
			for _, r := range x.Conversation {
				if !r.Answered {
					hand(remarkKey(r))
				}
			}
		case "fix.pushed":
			if d.Head != "" {
				l.FixerHeads = addFixerHead(l.FixerHeads, d.Head)
			}
		case "fix.parent_merged", "fix.parent_pushed": // the daemon's push of a merge of the parent is no push of yours
			if d.Merge != "" {
				l.FixerHeads = addFixerHead(l.FixerHeads, d.Merge)
			}
		case "fix.ended": // a loop that found no slot started no fixer: its start is not one of the day's
			if d.NoSlot && !d.StartedAt.IsZero() {
				l.Starts = slices.DeleteFunc(l.Starts, func(at time.Time) bool { return at.Equal(d.StartedAt) })
			}
		}
	}
	if loops > 0 {
		for _, it := range magnumItemsOf(l.Read.Mine, nil, false, openFixConflict(l.Conflict, l.Conflict.Head), l.Conflict.Head, nil, false) {
			if it.Kind == itemMagnum || it.Kind == itemConflict {
				hand(it.Key)
			}
		}
	}
	l.Cursor = cursor
	l.Reviews = lastN(l.Reviews, openReviewsMax)
}

// noteLoopRan records that a fix loop started on PR prID (startFix): the
// look reads its events once it ended.
func (e *Engine) noteLoopRan(prID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.openLoops[prID] = true
}

// loopsToRead reports whether the look reads PR prID's events for the
// loops that ended: once after the daemon started, and once after each
// loop that started since; it clears the mark.
func (e *Engine) loopsToRead(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	read := e.openLoops[prID] || !e.openCaught[prID]
	delete(e.openLoops, prID)
	e.openCaught[prID] = true
	return read
}

// openReviewsMax is how many reviews a loop worked on the ledger keeps.
const openReviewsMax = 20

// lastN is the last n of s.
func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return slices.Clone(s[len(s)-n:])
}

// addFixerHead adds head to heads, the last openFixerHeadsMax kept.
func addFixerHead(heads []string, head string) []string {
	if head == "" || slices.Contains(heads, head) {
		return heads
	}
	return lastN(append(heads, head), openFixerHeadsMax)
}

// openStart starts the loop the verdict v decided on work, as autofix
// (startFix, with this tick's read carried to prepareFix). A start records
// itself in the ledger and an autofix.open event; a refusal makes the
// verdict wait with its sentence (a budget below the floor, a paused kind,
// a busy or pinned PR, the human cooldown, a read of GitHub that failed, a
// look at the base that failed: mergeUncheckedError, whose conflict is
// unknown, not gone) and is tried again openReadRetry later, not at every
// tick, since some refusals come after prepareFix read GitHub; except
// "nothing to fix", where the look and prepareFix disagree: that needs
// you, with the refusal, and is not tried again until the PR changes
// (Refused), and no free slot, which waits for one and is asked again at
// the next look (prepareFix refuses it on a registry read). A PR without a
// slot waits for one too while this tick's earlier starts took every free
// slot of its pool (openSlotStarts): their claims run on their own
// goroutines, and two loops would race for one slot, the loser ending
// unclaimed. The ledger counts a start at the loop's own start time, which
// a loop that ended for want of a slot names (openLoopsEnded takes it
// back). A start in CI mode (prev.Mode FixModeCI, ci_fix.go) records its
// CI fix before startFix (noteCIFixStart; one that cannot be recorded does
// not start) and a refusal marks it unstarted, which does not count. The
// PR's row is read again right before the start:
// the look decided on the rows it listed at the start of its step, and a
// loop's end on another goroutine may have moved the PR since (to needs
// attention); a PR whose state, mute, reviewed head or fingerprint changed
// waits for the next look, which decides on the new row. A dry run starts
// nothing.
func (e *Engine) openStart(ctx context.Context, repo store.Repo, pr store.PR, l *openLedger, v OpenVerdict, work []openItem, prev FixState, fp string) OpenVerdict {
	subject := prSubject(repo, pr.Number)
	if l.Refused != "" && l.Refused == fp {
		v.Verdict, v.Why = OpenNeedsYou, l.RefusedWhy
		v.Needs = append(v.Needs, OpenNeed{Kind: "loop", URL: pr.URL})
		return v
	}
	if e.d.DryRun {
		return v
	}
	if now := e.now(); !l.WaitAt.IsZero() && now.Sub(l.WaitAt) < openReadRetry {
		v.Verdict, v.Why, v.Until = OpenWaits, l.WaitWhy, l.WaitAt.Add(openReadRetry)
		return v
	}
	switch cur, err := e.st.PRByID(ctx, pr.ID); {
	case err != nil:
		v.Verdict, v.Why = OpenWaits, "the PR's row could not be read again before the start; the next look tries again"
		return v
	case cur.State != pr.State || cur.Muted != pr.Muted || deref(cur.ReviewedSHA) != deref(pr.ReviewedSHA) || openPrint(cur) != fp:
		v.Verdict, v.Why = OpenWaits, "the PR changed during the look; the next look decides on it"
		return v
	default:
		pr = cur
	}
	pool := e.slotPoolOf(ctx, repo, pr) // nil: the PR has a slot, or its repository no pool
	// Only once this tick's starts took slots of the pool: with none free at
	// all, prepareFix's refusals (a drain, a paused kind, a snooze, the
	// cooldown) come first, and its own look for a free slot after them.
	if pool != nil && e.openSlotStarts[pool.Repo] > 0 {
		if free, err := e.st.FreeSlots(ctx, pool.Repo, pr.ID); err == nil && len(free) <= e.openSlotStarts[pool.Repo] {
			e.needSlot(ctx, *pool, subject) // the free ones went to this tick's other starts
			return slotWait(v, work, pool.Repo)
		}
	}
	var carried *externalRead
	if r, ok := e.openReads[pr.ID]; ok {
		carried = &r
	}
	ci := prev.Mode == FixModeCI
	if ci && !e.noteCIFixStart(ctx, pr, prev.CIChecks, autofixBy) {
		v.Verdict, v.Why = OpenWaits, "the CI fix could not be recorded before its start; the next look tries again"
		return v
	}
	job, err := e.startFix(ctx, pr, autofixBy, prev, carried)
	if err != nil && ci { // no fixer ran: the start does not count
		e.noteCIFixOutcome(ctx, pr.ID, pr.HeadSHA, ciOutcomeUnstarted, "", "")
	}
	var budget *githubBudgetError
	var unchecked *mergeUncheckedError
	var noSlot noFreeFixSlot
	switch {
	case err == nil:
		if pool != nil {
			e.openSlotStarts[pool.Repo]++
		}
		l.Starts = append(l.Starts, cmp.Or(job.state.StartedAt, e.now())) // the loop's own time, which its end without a slot names
		l.Refused, l.RefusedWhy, l.WaitAt, l.WaitWhy = "", "", time.Time{}, ""
		counts := map[string]int{}
		for _, it := range work {
			counts[it.Class+":"+it.Kind]++
		}
		msg := "autofix starts a fix loop on " + workWords(work)
		if ci {
			msg = "autofix " + v.Why
		}
		e.event(ctx, "info", subject, "autofix.open", msg, map[string]any{"work": counts, "head": pr.HeadSHA})
		return v
	case errors.As(err, &budget):
		v.Verdict, v.Why = OpenWaits, "GitHub budget: the loop waits for it"
		l.WaitAt, l.WaitWhy = e.now(), v.Why
	case errors.As(err, &noSlot): // prepareFix asked the pool for one (needSlot); the next look asks again, a registry read
		return slotWait(v, work, noSlot.repo)
	case errors.As(err, &unchecked): // the conflict is unknown, not gone: asked again, as a failed read is
		v.Verdict, v.Why = OpenWaits, oneLine(unchecked.cause.Error(), 300)
		l.WaitAt, l.WaitWhy = e.now(), v.Why
		e.log.Info("autofix open: no fix loop started", "subject", subject, "why", v.Why)
	case strings.HasPrefix(err.Error(), "nothing to fix"):
		why := oneLine(err.Error(), 300)
		e.log.Warn("autofix open: the look found work that the fix job refuses", "subject", subject, "why", why)
		l.Refused, l.RefusedWhy = fp, why
		v.Verdict, v.Why = OpenNeedsYou, why
		v.Needs = append(v.Needs, OpenNeed{Kind: "loop", URL: pr.URL})
	default:
		v.Verdict, v.Why = OpenWaits, oneLine(err.Error(), 300)
		l.WaitAt, l.WaitWhy = e.now(), v.Why
		e.log.Info("autofix open: no fix loop started", "subject", subject, "why", v.Why)
	}
	return v
}

// slotPoolOf is the pool a loop on pr claims a slot from (fixClaim), nil
// when it needs none: the PR holds a slot, or its repository has no pool
// (a per-PR worktree is made for it).
func (e *Engine) slotPoolOf(ctx context.Context, repo store.Repo, pr store.PR) *config.Pool {
	pool := e.cfg.PoolFor(repo.FullName())
	if pool == nil {
		return nil
	}
	if _, has, err := e.slotOf(ctx, pr.ID); err != nil || has {
		return nil
	}
	return pool
}

// slotWait is v waiting for a free slot of repo's pool for work.
func slotWait(v OpenVerdict, work []openItem, repo string) OpenVerdict {
	v.Verdict, v.Why = OpenWaits, workWords(work)+" waits for a free "+repo+" slot"
	return v
}

// openWrite writes the ledger when it changed, and the verdict when it
// says something else, or the PR's row changed after it (a reader takes
// such a verdict as stale): an unchanged PR costs no write. The verdict
// keeps the stored one's Since while it goes on saying the same (goesOn),
// and begins at its own time otherwise. A dry run writes nothing.
func (e *Engine) openWrite(ctx context.Context, pr store.PR, raw string, l openLedger, v OpenVerdict) {
	if e.d.DryRun {
		return
	}
	if b, err := json.Marshal(l); err == nil && string(b) != raw {
		e.setKV(ctx, kvPRFixSeen(pr.ID), string(b))
	}
	v.Since = v.At
	if cur, ok := e.getKV(ctx, KVPROpen(pr.ID)); ok {
		if prev, ok := ParseOpenVerdict(cur); ok {
			if v.GoesOn(prev) {
				v.Since = cmp.Or(prev.Since, prev.At)
			}
			if prev.SameAs(v) && !pr.UpdatedAt.After(prev.At) {
				return
			}
		}
	}
	if b, err := json.Marshal(v); err == nil {
		e.setKV(ctx, KVPROpen(pr.ID), string(b))
	}
}
