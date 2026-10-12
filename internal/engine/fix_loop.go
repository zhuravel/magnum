package engine

// The fix loop between its cycles (fix.go runs one cycle, fix_settle.go
// ends it): a review round's end, once its reservation is gone, decides what
// the PR's loop does next (fixAfterRound). A loop waiting for the re-review
// of its fixer's push (phase reviewing) goes on to its next cycle while that
// review finds something and cycles are left (fix_cycles), and ends
// otherwise: done when the review found nothing, as needs attention at the
// cap, after findings the fixer left to the author, or when the re-review
// did not post (a re-review whose judge turn a usage or login pause cut
// short is continued later, and the loop waits for that continue). A PR
// without a loop gets one by itself after a review with findings when its
// watch says autofix = "own", the PR is the operator's and it was not
// switched out of autofix (`magnum fix --off`).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// autofixBy is who starts a loop autofix starts (FixState.By).
const autofixBy = "autofix"

// needsYouEntryRe reads a "## Needs you" entry's comment id, priority and
// place ("2101 P1 app/models/offer.rb:42: <reason>"; "5485002407
// description: <reason>" for the review body's "Description: ✗" finding,
// which has the review's id and no priority), the place in plain path
// characters only, so no control character or other text of the fixer's
// reaches a message. The reason is the fixer's own words about the PR, so
// it is never read.
var needsYouEntryRe = regexp.MustCompile(`^(\d{1,20}) +(?:(P[0-3]) +)?([A-Za-z0-9._/@+-]+:\d{1,9}|description)(?::|\s|$)`)

// fixAfterRound decides what the end of a review round means for the PR's
// fix loop: a loop waiting for this re-review goes on or ends
// (continueFixLoop), and a PR without one may get one (autofix). launch
// calls it on the round's goroutine once the round's reservation is gone,
// with the daemon's context (startFix reserves the PR for the next cycle and
// runs it on that context); res is the round's result, an error one for a
// round whose setup failed or that panicked. A round an abort, an ignore or
// a shutdown stopped never comes here: the stop ended the loop (stopPR,
// endFixLoop), or the next start re-reviews and the loop goes on after it.
// A row that cannot be read is logged and left alone: never a second loop
// over it. The loop's ends and its next cycle act only on the row read here
// (compare-and-set): a `magnum abort` that ended the loop meanwhile, with no
// reservation to stop, leaves the hook nothing to do.
func (e *Engine) fixAfterRound(ctx context.Context, pr store.PR, res pipeline.RoundResult) {
	if ctx.Err() != nil || e.d.DryRun {
		return
	}
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil {
		e.log.Warn("fix: the PR after its round", "pr", pr.ID, "err", err)
		return
	}
	repo, err := e.st.RepoByID(ctx, cur.RepoID)
	if err != nil {
		e.log.Warn("fix: the PR's repository", "pr", pr.ID, "err", err)
		return
	}
	st, raw, ok, err := e.fixRow(ctx, cur.ID)
	switch {
	case err != nil:
		e.log.Warn("fix: the loop's row cannot be read; nothing starts over it", "subject", prSubject(repo, cur.Number), "err", err)
	case !ok:
		e.autofix(ctx, repo, cur, res)
	case st.Phase == FixPhaseReviewing:
		e.continueFixLoop(ctx, e.loopJob(ctx, repo, cur, st), raw, res)
	}
}

// continueFixLoop takes a loop whose re-review ended (res) to its next
// step: a re-review that did not post, or findings the fixer left to the
// author when it pushed, end it as needs attention; a review that found
// nothing ends it as done; findings start the next cycle (startFix) while
// cycles are left, and end it as needs attention at the cap, which says what
// is left (capMessage); a next cycle that cannot start ends it as needs attention with why, unless the PR moved
// on since the re-review (closed, or a newer head the operator pushed:
// loopMovedOn), which ends it quietly with fix.ended. A re-review whose
// judge turn a usage or login pause cut short after its judge was prompted
// did not end: dispatch continues that turn once the pause lifts
// (pauseContinues), so the loop stays in phase reviewing with a fix.waiting
// event and the continued round's end decides it; a continued turn that
// never comes back ends the loop as any waiting loop ends (an abort or an
// ignore, stopPR; the next start's recovery, recoverReviewingLoop, once the
// PR moved on). A review that requests changes with no new findings found
// something: the earlier findings it keeps open (fixRemains), as did one
// whose result keeps an earlier P0 to P2 open. raw is the
// loop's row as read: every end acts only while the row is still that
// (endFixRowIf), and the next cycle replaces only that row (claimFixRow), so
// a stop that ended the loop meanwhile wins quietly (errFixLoopGone). A
// daemon that drains for a restart holds the next cycle: the row stays in
// phase reviewing, and the restart's recovery goes on with it
// (recoverReviewingLoop).
func (e *Engine) continueFixLoop(ctx context.Context, job *fixJob, raw string, res pipeline.RoundResult) {
	st := job.state
	found := roundFindings(res)
	left := textx.Count(found, "finding", "findings") + " left"
	switch open := roundOpenProblems(res); {
	case found > 0:
	case open > 0:
		left = textx.Count(open, "earlier finding", "earlier findings") + " still open"
	default:
		left = "earlier findings still open"
	}
	switch {
	case e.pauseContinues(ctx, job.pr, res.Outcome):
		e.log.Info(fmt.Sprintf("fix: the re-review's turn continues after a %s pause; the loop waits for it", res.Outcome),
			"subject", job.subject(), "cycle", st.Cycle)
		e.event(ctx, "info", job.subject(), "fix.waiting",
			fmt.Sprintf("the fix loop waits: the re-review's judge turn paused (%s) and continues after the pause", res.Outcome), job.loopData())
	case res.Outcome != pipeline.OutcomePosted:
		msg := fmt.Sprintf("fix loop ended: the re-review did not post (%s)", cmp.Or(res.Outcome, pipeline.OutcomeError))
		if len(st.NeedsYou) > 0 {
			msg += "; " + needsYourCall(st.NeedsYou, st.Report)
		}
		e.endLoopForAttention(ctx, job, raw, "fix.failed", msg)
	case len(st.NeedsYou) > 0:
		e.endLoopForAttention(ctx, job, raw, "fix.needs_you", "fix: "+needsYourCall(st.NeedsYou, st.Report))
	case !fixRemains(res):
		e.fixLoopDone(ctx, job, raw, cmp.Or(res.TargetSHA, st.Head))
	case st.Cycle >= st.Cap:
		e.endLoopForAttention(ctx, job, raw, "fix.capped", e.capMessage(ctx, job, res, found, left))
	default:
		_, err := e.startFix(ctx, job.pr, st.By, st)
		switch {
		case err == nil:
		case errors.Is(err, errFixLoopGone):
			e.log.Info("fix: the loop ended before its next cycle started", "subject", job.subject(), "why", err.Error())
		case errors.Is(err, errFixDraining):
			e.log.Info("fix: the daemon is draining; the next cycle waits for the restart", "subject", job.subject(), "cycle", st.Cycle+1)
			e.mu.Lock()
			e.drainHeld[job.pr.ID] = true // a drain lifted without a restart goes on with it
			e.mu.Unlock()
		default:
			if why, moved := e.loopMovedOn(ctx, job.pr.ID, cmp.Or(res.TargetSHA, st.Head)); moved {
				if e.endFixRowIf(ctx, job, raw) {
					e.event(ctx, "info", job.subject(), "fix.ended", fmt.Sprintf("the fix loop ends before cycle %d: %s", st.Cycle+1, why), job.loopData())
				}
				return
			}
			e.endLoopForAttention(ctx, job, raw, "fix.failed", fmt.Sprintf("fix loop ended: cycle %d did not start: %v", st.Cycle+1, err))
		}
	}
}

// The judge ends the title of a finding in code written to fix an earlier
// one with "(in the fix for <earlier title>)" and that of a finding its
// earlier reviews missed with "(missed earlier)" (skills/magnum-review/
// SKILL.md, section 6). The registry cuts a title at 120 runes, so the
// first may lose its closing parenthesis; a P3's "(optional, no reply
// needed)" follows the second.
const (
	titleFalloutMark = "(in the fix for"
	titleMissedMark  = "(missed earlier)"
)

// capMessage is what a loop that reached its cap tells the operator: the
// loop's fix cycles and, once earlier loops of the PR ran some too, all the
// PR's ("5 fix cycles (14 on this PR)": a loop started by hand counts from
// cycle 1 again), and the findings the last re-review left, split by where
// they come from so that a loop converging reads differently from one
// chasing its own fixes: "3 findings left: 1 new, 0 missed earlier, 2
// fallout of the fixes", then the fixer's last report. found is the
// review's finding count and left its words (findingsSplit adds the split).
func (e *Engine) capMessage(ctx context.Context, job *fixJob, res pipeline.RoundResult, found int, left string) string {
	st := job.state
	cycles := textx.Count(st.Cycle, "fix cycle", "fix cycles")
	if total := e.fixRunsOf(ctx, job.pr.ID); total > st.Cycle {
		cycles += fmt.Sprintf(" (%d on this PR)", total)
	}
	if split := e.findingsSplit(ctx, job.pr.ID, res.Round, found); split != "" {
		left += ": " + split
	}
	return fmt.Sprintf("%s, %s: %s", cycles, left, st.Report)
}

// fixRunsOf is how many fix runs the PR has had, across all its loops: 0
// when they cannot be read.
func (e *Engine) fixRunsOf(ctx context.Context, prID int64) int {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		e.log.Warn("fix: the PR's runs, for the cap's count of its fix cycles", "pr", prID, "err", err)
		return 0
	}
	return len(slices.DeleteFunc(runs, func(r store.Run) bool { return r.Kind != store.RunFix }))
}

// findingsSplit says how the findings the review of the PR's round posted
// divide, read from the titles the registry holds for that round
// (store.Finding, the result's provenance): "1 new, 0 missed earlier, 2
// fallout of the fixes". It is "" when the registry holds no posted titles
// for the round or not as many as the review counted (a result written
// without a ledger, or with an incomplete one): no split is better than a
// guessed one.
func (e *Engine) findingsSplit(ctx context.Context, prID int64, round, found int) string {
	if round <= 0 || found <= 0 {
		return ""
	}
	all, err := e.st.FindingsByPR(ctx, prID)
	if err != nil {
		e.log.Warn("fix: the PR's findings, for the cap's account of what is left", "pr", prID, "err", err)
		return ""
	}
	var titles []string
	var runID string
	for _, f := range all { // oldest first: a round recorded again counts once, by its last run
		if f.Round != round {
			continue
		}
		if f.RunID != runID {
			runID, titles = f.RunID, nil
		}
		if f.Verdict == store.FindingPosted {
			titles = append(titles, f.Title)
		}
	}
	if len(titles) != found {
		return ""
	}
	fresh, missed, fallout := splitFindingTitles(titles)
	return fmt.Sprintf("%d new, %d missed earlier, %d fallout of the fixes", fresh, missed, fallout)
}

// splitFindingTitles counts titles by the judge's suffixes: findings in the
// fixes of earlier ones, findings the earlier reviews missed, and the new
// rest.
func splitFindingTitles(titles []string) (fresh, missed, fallout int) {
	for _, title := range titles {
		switch {
		case strings.Contains(title, titleFalloutMark):
			fallout++
		case strings.Contains(title, titleMissedMark):
			missed++
		default:
			fresh++
		}
	}
	return fresh, missed, fallout
}

// loopMovedOn says why a loop's PR moved on after the re-review of head,
// which a next cycle that could not start may be because of: GitHub closed
// it, or its head is no longer head (the operator pushed meanwhile, and the
// push gets a review of its own). ok is false when it did not move on.
func (e *Engine) loopMovedOn(ctx context.Context, prID int64, head string) (string, bool) {
	pr, err := e.st.PRByID(ctx, prID)
	switch {
	case err != nil:
		return "", false
	case pr.GHState != store.GHOpen:
		return fmt.Sprintf("the PR is %s on GitHub", strings.ToLower(pr.GHState)), true
	case head != "" && pr.HeadSHA != head:
		return fmt.Sprintf("the head moved to %s after the re-review of %s", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(head)), true
	}
	return "", false
}

// fixLoopDone ends a loop whose re-review of head found nothing: its row
// goes (while it is still raw, endFixRowIf), fix.done is recorded and an
// info toast says how many cycles it took.
func (e *Engine) fixLoopDone(ctx context.Context, job *fixJob, raw, head string) {
	st, pr := job.state, job.pr
	if !e.endFixRowIf(ctx, job, raw) {
		return
	}
	cycles := textx.Count(st.Cycle, "fix cycle", "fix cycles")
	e.event(ctx, "info", job.subject(), "fix.done", fmt.Sprintf("the fix loop ends clean after %s: the re-review of %s found nothing",
		cycles, textx.ShortSHA(head)), job.loopData())
	what := fmt.Sprintf("PR #%d: clean after %s", pr.Number, cycles)
	e.info(notify.Item{Key: fmt.Sprintf("fix-done:%d:%s", pr.ID, store.FormatTime(st.StartedAt)), Title: "magnum: " + what,
		Body: pr.URL + "\nthe re-review of " + textx.ShortSHA(head) + " found nothing", Line: what + " " + pr.URL, Kind: kindFixEnded})
}

// endLoopForAttention ends a loop after its re-review as needs attention
// with msg: its row goes (while it is still raw, endFixRowIf; a loop a stop
// ended meanwhile is left to the stop), an event of kind names msg and the
// last report, and the operator is told as the fix job's attention
// (fixAttention), unless the round itself left the PR needing attention,
// whose account of why stays.
func (e *Engine) endLoopForAttention(ctx context.Context, job *fixJob, raw, kind, msg string) {
	if !e.endFixRowIf(ctx, job, raw) {
		return
	}
	e.event(ctx, "warn", job.subject(), kind, msg, job.loopData())
	if pr, err := e.st.PRByID(ctx, job.pr.ID); err == nil && pr.State == store.PRNeedsAttention {
		return
	}
	e.fixAttention(ctx, job, msg)
}

// endFixRowIf ends the job's loop by removing its row while the row is still
// raw, as read (Store.DeleteKVIf), and reports whether it did: false when a
// stop ended the loop or a cycle took it on meanwhile, which then owns it.
func (e *Engine) endFixRowIf(ctx context.Context, job *fixJob, raw string) bool {
	gone, err := e.st.DeleteKVIf(ctx, KVPRFix(job.pr.ID), raw)
	if err != nil {
		e.log.Warn("fix: end the loop's row", "subject", job.subject(), "err", err)
	} else if !gone {
		e.log.Info("fix: the loop's row changed before its end; nothing to end", "subject", job.subject())
	}
	return gone
}

// continueDrainHeldLoops goes on with the loops a drain held before their
// next cycle (drainHeld), once that drain ended without a restart
// (continueAfterDrain): each gets the decision the restart's recovery would have made
// (recoverReviewingLoop), so none is stranded in phase reviewing until the
// next start. Only the loops the drain held are looked at: a loop whose
// re-review is still running gets its decision from that round's hook.
func (e *Engine) continueDrainHeldLoops(ctx context.Context) {
	e.mu.Lock()
	held := slices.Sorted(maps.Keys(e.drainHeld))
	clear(e.drainHeld)
	e.mu.Unlock()
	for _, id := range held {
		st, raw, ok, err := e.fixRow(ctx, id)
		if err != nil || !ok || st.Phase != FixPhaseReviewing {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		e.recoverReviewingLoop(ctx, e.loopJob(ctx, repo, pr, st), raw, "while a drain held it")
	}
}

// recoverReviewingLoop goes on with a loop left in phase reviewing (raw: its
// row as read) by a stopped daemon (at startup) or by a drain lifted without
// a restart (continueDrainHeldLoops); while says when ("while the daemon was
// stopped"). A PR whose re-review still has to run (waiting, in flight or
// paused) keeps the loop: the round's recovery re-reviews and the hook
// decides after it. A PR reviewed at the loop's head had its re-review post
// while a stop or a drain kept the hook from deciding: the hook's decision
// is made now, on that review's result. Any other PR has moved on (a newer
// head, closed, needing attention): the loop ends quietly with fix.ended.
func (e *Engine) recoverReviewingLoop(ctx context.Context, job *fixJob, raw, while string) {
	pr, st := job.pr, job.state
	switch {
	case slices.Contains(waitingStates, pr.State), slices.Contains(store.InFlightStates, pr.State), pr.State == store.PRPaused:
		return
	case pr.State == store.PRReviewed && pr.HeadSHA == st.Head && deref(pr.ReviewedSHA) == st.Head:
		if res, ok := e.postedReReview(ctx, pr, st.Head); ok {
			e.continueFixLoop(ctx, job, raw, res)
			return
		}
	}
	if !e.endFixRowIf(ctx, job, raw) {
		return
	}
	e.event(ctx, "info", job.subject(), "fix.ended", fmt.Sprintf("the fix loop ends: its PR moved on %s (%s at %s, the loop waited at %s)",
		while, pr.State, textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(st.Head)), job.loopData())
}

// postedReReview is the result of the review magnum posted on the PR at head,
// as the round that posted it reported it: its findings, the earlier ones it
// keeps open by priority and its event, read back from its stored result
// (ok false when there is none at head).
func (e *Engine) postedReReview(ctx context.Context, pr store.PR, head string) (pipeline.RoundResult, bool) {
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	sum, ok := sums[pr.ID]
	if err != nil || !ok || sum.SHA != head {
		return pipeline.RoundResult{}, false
	}
	event := strings.ToUpper(sum.Event)
	switch event {
	case "REQUEST_CHANGES":
		event = "CHANGES_REQUESTED"
	case "APPROVE":
		event = "APPROVED"
	case "COMMENT":
		event = "COMMENTED"
	}
	res := pipeline.RoundResult{Outcome: pipeline.OutcomePosted, TargetSHA: head, ReviewURL: sum.URL, ReviewID: sum.ReviewID, Event: event,
		Findings: map[string]int{"P0": sum.Counts[0], "P1": sum.Counts[1], "P2": sum.Counts[2], "P3": sum.Counts[3]}}
	if o := sum.OpenCounts; o != nil {
		res.PreviousOpen = map[string]int{"P0": o[0], "P1": o[1], "P2": o[2], "P3": o[3]}
	}
	if run, err := e.st.RunByID(ctx, sum.RunID); err == nil {
		res.Round = run.Round // the cap finds the round's finding titles by it
	}
	return res, true
}

// autofix starts a fix loop by itself after a round that posted a review
// with findings, not an approval, on a PR whose watch says autofix = "own",
// whose author is the operator and which was not switched out of autofix
// (KVPRFixOff). startFix's refusals (a head that moved since the review, a
// fork, a slot that is not free to use, ...) are logged, never toasted: the
// operator asked for nothing. A watch without a fix role is logged once per
// PR.
func (e *Engine) autofix(ctx context.Context, repo store.Repo, pr store.PR, res pipeline.RoundResult) {
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil || e.cfg.AutofixFor(w) != config.AutofixOwn || res.Outcome != pipeline.OutcomePosted ||
		roundFindings(res) == 0 || res.Event == "APPROVED" || !e.ownPR(pr) {
		return
	}
	subject := prSubject(repo, pr.Number)
	switch _, off, err := e.fixOff(ctx, pr.ID); {
	case err != nil:
		e.log.Warn("autofix: the PR's opt-out cannot be read; no fix loop starts", "subject", subject, "err", err)
		return
	case off:
		e.log.Info("autofix: fix is off for the PR (magnum fix --on lifts it)", "subject", subject)
		return
	}
	if _, ok := e.cfg.FixRoleFor(w); !ok {
		if e.firstAutofixWithoutRole(pr.ID) {
			e.log.Info("autofix: no fix role for "+repo.FullName(), "subject", subject)
		}
		return
	}
	job, err := e.startFix(ctx, pr, autofixBy, FixState{})
	if err != nil {
		e.log.Info("autofix: no fix loop started", "subject", subject, "why", err.Error())
		return
	}
	e.log.Info("autofix: fix loop started", "subject", subject, "role", job.role.Name, "cycles", job.state.Cap)
}

// firstAutofixWithoutRole reports whether autofix found no fix role for the
// PR for the first time since the daemon started.
func (e *Engine) firstAutofixWithoutRole(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.autofixNoRole[prID] {
		return false
	}
	e.autofixNoRole[prID] = true
	return true
}

// loopJob is the PR's loop in st as a fix job without a run (the hook's and
// the recovery's, which no cycle runs): its repository, watch, pool, fix
// role and slot, for failFix, fixAttention and the events.
func (e *Engine) loopJob(ctx context.Context, repo store.Repo, pr store.PR, st FixState) *fixJob {
	job := &fixJob{repo: repo, pr: pr, state: st}
	if w := e.cfg.WatchFor(repo.FullName()); w != nil {
		job.watch, job.pool = *w, e.cfg.PoolFor(repo.FullName())
		if role, ok := e.cfg.FixRoleFor(w); ok {
			job.role = role
		}
	}
	if slot, has, err := e.slotOf(ctx, pr.ID); err == nil && has {
		job.slot = slot
	}
	return job
}

// loopData is the data of an event about the job's loop between its cycles:
// the role, the cycle, the cap, who started the loop and its last report.
func (j *fixJob) loopData() map[string]any {
	data := j.eventData()
	if j.run.ID == "" {
		delete(data, "run")
	}
	data["report"], data["by"] = j.state.Report, j.state.By
	if data["role"] == "" {
		data["role"] = j.state.Role
	}
	return data
}

// fixRemains reports whether a posted re-review leaves something to fix:
// new findings, a changes request with none, which keeps earlier findings
// open, or earlier P0 to P2 findings it keeps open (roundOpenProblems: a
// comment that says "Fix 1 problem before merging" with nothing new).
func fixRemains(res pipeline.RoundResult) bool {
	return roundFindings(res) > 0 || res.Event == "CHANGES_REQUESTED" || roundOpenProblems(res) > 0
}

// roundOpenProblems is how many earlier P0 to P2 findings a round's review
// keeps open (an open P3 stays optional); 0 when its result gave no
// priorities.
func roundOpenProblems(res pipeline.RoundResult) int {
	return res.PreviousOpen["P0"] + res.PreviousOpen["P1"] + res.PreviousOpen["P2"]
}

// roundFindings is how many findings (P0 to P3) a round's review posted.
func roundFindings(res pipeline.RoundResult) int {
	return res.Findings["P0"] + res.Findings["P1"] + res.Findings["P2"] + res.Findings["P3"]
}

// needsYourCall says which findings the fixer left to the author and where
// its report is: "2 findings need your call (2101 P1 app/a.rb:42, 2102 P2
// app/b.rb:7): <report>". Each entry is named by its comment id, priority
// and place alone (needsYouEntryRe); an entry magnum cannot read so is only
// counted ("1 entry magnum could not read").
func needsYourCall(entries []string, report string) string {
	var named []string
	other := 0
	for _, entry := range entries {
		m := needsYouEntryRe.FindStringSubmatch(strings.TrimSpace(entry))
		if m == nil {
			other++
			continue
		}
		name := m[1] + " " + m[3]
		if m[2] != "" {
			name = m[1] + " " + m[2] + " " + m[3]
		}
		named = append(named, name)
	}
	if other > 0 {
		named = append(named, textx.Count(other, "entry", "entries")+" magnum could not read")
	}
	n := len(entries)
	s := textx.Count(n, "finding", "findings") + " " + textx.Plural(n, "needs", "need") + " your call"
	if len(named) > 0 {
		s += " (" + strings.Join(named, ", ") + ")"
	}
	return s + ": " + report
}
