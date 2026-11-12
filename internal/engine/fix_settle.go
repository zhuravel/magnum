package engine

// The fix job's end (fix.go runs it): the settle reads the fixer's report,
// puts the slot back in order (Slots.Resync at the pushed head, or at the
// reviewed one when nothing was pushed, so the next round's guard sees no
// drift) and ends the cycle: a push goes on to a forced re-review (phase
// reviewing; findings the fixer left to the author are toasted at once and
// wait in the loop's row for that round's end), anything else ends the
// loop, with an info toast when the fixer answered everything without a
// push and as needs attention (attention.KindFix) otherwise. A stop
// (`magnum abort`) ends the loop quietly.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kindFixEnded counts the fix loops that ended without the operator in a
// batched toast.
var kindFixEnded = notify.Kind{One: "fix loop ended", Many: "fix loops ended"}

// fixThreadsRe reads the thread count off a report's "## Replied" line
// ("3 threads (2 fixed, ...)").
var fixThreadsRe = regexp.MustCompile(`^(\d+)\s+threads?\b`)

// settleFix ends the cycle of a fixer whose run ended. Its step (fix.settle)
// puts the slot back in order: at the head the fixer pushed (pushedHead)
// when its report lists commits, else (or when that head cannot be
// confirmed, which fails the job) at the reviewed head; a slot that cannot
// be put back fails the job. Then the report decides: none, one of
// another run or a broken one, or a failed push, fails the job; a push goes
// on to the re-review (fixReReview), with the findings left to the author
// kept for that round's end (spec: the push deserves its re-review), but
// for a conflict of the base merge whose tip the push holds, which the
// fixer merged after all (dropMergedConflicts);
// findings left to the author without a push end the loop as needs
// attention (a fix the fixer left as unanswered in another reviewer's
// thread that GitHub shows its reply in is none: dropFixerAnswered; nor is a
// finding whose thread of magnum's is resolved, and one answered there asks
// only for the resolution: freshNeedsYou; both looks share one GitHub read
// of the threads, threadsReader); replies alone end the loop with an info
// toast. Before the report decides, and for a run that fails too, a
// fix.unmarked warning names the author's replies without the fix marker
// since the run started (nameUnmarkedReplies, on that same read and one of
// the conversation) whenever the run may have replied: its report counts
// replies or leaves calls, or there is no report to say. fix.replied,
// or fix.failed for a settle that fails, says when the operator typed into
// the fixer's pane during the run (operatorTyped). The events name counts,
// SHAs, the role and the report, never what the fixer wrote. stop is the
// job's own context, which only the confirmation of the push reads
// (confirmPush, fix_confirm.go): a shutdown cuts it at once and leaves the
// loop to the next start, which fails it as unconfirmed. GitHub that does
// not answer within its retries leaves the loop in its settle for the
// ticks (awaitPush), and a push it does not confirm fails the fix as
// unconfirmed (failUnconfirmed), which the poll may settle late.
func (e *Engine) settleFix(ctx, stop context.Context, job *fixJob) {
	ctx, stop = github.WithOp(ctx, opFixSettle), github.WithOp(stop, opFixSettle) // github_cost.go
	report := deref(job.run.ReportPath)
	rep, rerr := e.readFixReport(job)
	typed := e.operatorTyped(ctx, job) // a run that failed too: one the operator drove without a report matters most
	pushed := rerr == nil && len(rep.Pushed) > 0 && !rep.PushFailed
	target, headErr, err := e.settleSlot(ctx, stop, job, rep, pushed, typed)
	if headErr != nil && stop.Err() != nil && errors.Is(headErr, stop.Err()) {
		e.log.Info("fix settle interrupted by a shutdown while it confirmed the push; the next start fails the fix as unconfirmed", "subject", job.subject())
		return
	}
	read := e.threadsReader(ctx, job)                                  // fix_calls.go: one GitHub read for every look
	if !mayHaveReplied(rep) || e.nameUnmarkedReplies(ctx, job, read) { // fix_external.go; a run that fails too, before it fails
		e.noteRepliesNamed(ctx, job) // fix_confirm.go: a later settle of the push need not look again
	}
	var held fixMergeHeld
	var unanswered pushUnanswered
	var unconfirmed pushNotConfirmed
	switch {
	case err != nil:
		if errors.As(headErr, &held) { // fix_held_merge.go: no row will name what the hold wrote
			e.unholdFiles(ctx, job.repo, job.pr, held.h)
		}
		e.dropFixPush(ctx, job.pr.ID) // fix_confirm.go: a fix that failed otherwise is never settled late
		e.failFixTyped(ctx, job, "fix settle failed: "+err.Error(), "", typed)
		return
	case errors.As(headErr, &unanswered): // fix_confirm.go: the ticks confirm the push
		e.awaitPush(ctx, job, unanswered)
		return
	case errors.As(headErr, &unconfirmed): // fix_confirm.go: the typed kind, which the poll may settle late
		e.failUnconfirmed(ctx, job, unconfirmed.head, "fix settle failed: "+headErr.Error(), typed)
		return
	case errors.As(headErr, &held): // fix_held_merge.go: the fixer's merge magnum holds for your y
		e.finishFixRun(ctx, job.run, store.RunVerified, "needs_you", "")
		e.endFixState(ctx, job)
		e.settleHeld(ctx, job, held)
		return
	case errors.As(headErr, new(fixHandRefused)): // fix_parent.go: the fixer's merge the daemon would not push
		msg := "fix: " + job.handRefusedWords(headErr.Error())
		if carried := carriedText(job); carried != "" { // fix_carry.go: the calls of the loop's earlier cycles stay the author's
			msg += "; " + carried
		}
		e.finishFixRun(ctx, job.run, store.RunVerified, "needs_you", "")
		e.endFixState(ctx, job)
		e.event(ctx, "warn", job.subject(), "fix.needs_you", msg, job.eventData())
		e.fixAttention(ctx, job, msg)
		return
	case headErr != nil:
		e.failFixTyped(ctx, job, "fix settle failed: "+headErr.Error(), "", typed)
		return
	case rerr != nil:
		e.failFixTyped(ctx, job, "fix run failed: "+rerr.Error(), "", typed)
		return
	case rep.PushFailed:
		e.failFixTyped(ctx, job, "fix run failed: the fixer's push failed: "+report, "", typed)
		return
	}
	e.settleReport(ctx, job, rep, pushed, target, typed, read)
}

// mayHaveReplied reports whether the run of rep (a run with no report
// too) may have replied, so the settle names the author's replies without
// the fix marker (nameUnmarkedReplies): its report counts replies or leaves
// calls, or there is no report to say.
func mayHaveReplied(rep FixReport) bool {
	threads, counted := fixThreads(rep.Replied) // not counted without a report: the run may have replied
	return !counted || threads > 0 || len(rep.NeedsYou) > 0
}

// settleSlot is the settle's step fix.settle: the slot back in order at
// the head the fixer pushed (pushedHead) when the report lists commits,
// else, or when that head cannot be confirmed, at the reviewed head; at
// the push while GitHub does not answer, whose confirmation the ticks go
// on with (pushUnanswered). It returns the head, pushedHead's error and
// the step's. A confirmation a shutdown cut (stop ended) leaves the slot as
// it is, for the next start's recovery.
func (e *Engine) settleSlot(ctx, stop context.Context, job *fixJob, rep FixReport, pushed, typed bool) (target string, headErr, err error) {
	target = job.head
	err = steps.Step(ctx, e.st, job.subject(), "fix.settle", func(ctx context.Context) error {
		if pushed {
			var unanswered pushUnanswered
			switch target, headErr = e.pushedHead(ctx, stop, job, rep, typed); {
			case headErr != nil && stop.Err() != nil && errors.Is(headErr, stop.Err()):
				return headErr
			case errors.As(headErr, &unanswered):
				target = unanswered.head
			case headErr != nil:
				target = job.head
			}
		}
		return e.resyncFixSlot(ctx, job, target)
	})
	return target, headErr, err
}

// settleReport ends the cycle of a fixer whose settle read its report rep
// (a run that failed never reaches it) and found target, the head it
// pushed (pushed) and GitHub confirmed, or none: settleFix's decision, and
// a later settle's of a push GitHub confirmed then (settleConfirmedPush,
// fix_confirm.go). typed says the operator typed into the fixer's pane
// during the run, read is the settle's one GitHub read of the threads.
func (e *Engine) settleReport(ctx context.Context, job *fixJob, rep FixReport, pushed bool, target string, typed bool, read func() ([]github.Thread, error)) {
	subject, report := job.subject(), deref(job.run.ReportPath)
	threads, counted := fixThreads(rep.Replied)
	data := job.eventData()
	described := len(rep.Description) > 0 // rerr is nil here
	if described {
		data["description"] = len(rep.Description)
		e.event(ctx, "info", subject, "fix.description", fmt.Sprintf("%s updated the PR description (%s)", job.role.Name,
			textx.Count(len(rep.Description), "change", "changes")), data)
	}
	if pushed {
		data["head"], data["commits"] = target, len(rep.Pushed)
		e.event(ctx, "info", subject, "fix.pushed", fmt.Sprintf("%s pushed %s: the head is %s", job.role.Name,
			textx.Count(len(rep.Pushed), "commit", "commits"), textx.ShortSHA(target)), data)
		e.notePush(ctx, job.pr.ID, target) // ci_dispatch.go: its CI starts after the grace
		if job.conflict.sha != "" {        // fix_parent.go: the fixer's merge of the base may change the schema
			e.parentSchema(ctx, job, job.head, target, false)
		}
	}
	replied := "replied"
	if counted {
		replied, data["threads"] = "replied in "+textx.Count(threads, "thread", "threads"), threads
	}
	unread := job.othersUnread() // fix_external.go: what the fixer did not get of the other reviewers
	if unread != "" {
		replied += "; " + unread
	}
	if typed {
		replied, data["operator_typed"] = replied+typedSentence, true
	}
	e.event(ctx, "info", subject, "fix.replied", job.role.Name+" "+replied, data)
	var answered map[string]bool
	var built map[int64]bool
	var optional map[int64]string
	ciClass, ciCannot := "", false
	if job.mode == FixModeCI && !pushed { // ci_fix.go: the jobs it could not fix end the loop quietly, with their class
		ciClass, ciCannot, rep.NeedsYou = ciCannotOf(rep.NeedsYou)
	}
	rep.NeedsYou, optional = e.settleOptional(ctx, job, rep.Optional, rep.NeedsYou, read) // fix_optional.go: a P3 author's call is yours, and holds nothing
	rep.NeedsYou, built = e.decisionsLeft(ctx, job, rep.NeedsYou, read)                   // fix_decide.go: a decision it did not build goes back to you
	if pushed {
		rep.NeedsYou = e.dropMergedConflicts(ctx, job, rep.NeedsYou, target)
	}
	if len(rep.NeedsYou) > 0 {
		rep.NeedsYou = e.dropFixerAnswered(ctx, job, rep.NeedsYou, read)                // fix_external.go
		rep.NeedsYou, answered = e.freshNeedsYou(ctx, job, rep.NeedsYou, nil, "", read) // no re-review yet
	}
	// fix_carry.go: what earlier cycles left that this one did not ask again or take on
	carried := carriedLeft(job.state.Carried, report, rep.NeedsYou, laterWork{description: job.described, base: job.baseChecked || job.conflict.sha != ""})
	if len(carried) > 0 && !pushed { // the loop ends here: as magnum's threads show them now
		carried = e.freshCarried(ctx, job, carried, "", read)
	}
	job.state.Carried = carried
	n := len(rep.NeedsYou)
	needs := "fix: " + needsYouText(rep.NeedsYou, answered, report, job.pr.URL, job.label(), false)
	attn := withUnread(needs, unread)
	if n > 0 {
		data["needs_you"] = n
	}
	if len(carried) > 0 {
		data["carried"] = len(carried)
	}
	if job.mode == FixModeCI { // ci_fix.go: what the CI fix ended with, before its row may go
		switch {
		case pushed:
			job.ciOutcome, job.ciPushed = ciOutcomePushed, target
		case job.merged.sha != "": // with the class of a ci entry: the merge's head is no new chance for the same cause
			job.ciOutcome, job.ciPushed, job.ciWhy = ciOutcomeMerged, job.head, ciClass
		default:
			job.ciOutcome, job.ciWhy = ciOutcomeCannot, cmp.Or(ciClass, "unfixed")
		}
		if pushed {
			e.noteCIFixEnd(ctx, job) // the row stays for the re-review
		}
	}
	switch {
	case job.mode == FixModeCI && !pushed && n == 0 && len(carried) == 0: // ci_fix.go: rule 11 says it needs you, kind ci
		e.endCIFix(ctx, job, ciCannot, data)
		return
	case pushed && job.mode == FixModeMerge: // fix_parent.go: the poller's rules judge the merge, as any push of a merge
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		e.parentPushed(ctx, job, target, needs, n > 0)
	case pushed && n > 0:
		e.event(ctx, "warn", subject, "fix.needs_you", needs+"; the re-review of "+textx.ShortSHA(target)+" runs first", data)
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		if e.fixReReview(ctx, job, target, rep.NeedsYou, answered) {
			e.fixCallToast(job, attn) // the operator can act on it while the re-review runs
		}
	case pushed:
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		e.fixReReview(ctx, job, target, nil, nil)
	case n > 0 || len(carried) > 0:
		// carried calls are named, since another cycle's report holds them
		needs = "fix: " + loopNeedsText(carried, rep.NeedsYou, answered, report, job.pr.URL, job.label(), len(carried) > 0)
		e.finishFixRun(ctx, job.run, store.RunVerified, "needs_you", "")
		e.endFixState(ctx, job)
		e.event(ctx, "warn", subject, "fix.needs_you", needs, data)
		e.fixAttention(ctx, job, withParentRefused(withUnread(needs, unread), job)) // fix_parent.go
	default:
		e.finishFixRun(ctx, job.run, store.RunVerified, "replied", "")
		e.endFixState(ctx, job)
		e.liftDecided(ctx, job, built, optional) // fix_decide.go: it built, or left as optional calls, every call the attention asked for
		// fix_parent.go: the parent the first cycle did not merge is yours
		if note := job.parentRefusedNote(); note != "" {
			e.event(ctx, "warn", subject, "fix.needs_you", "fix: "+note, data)
			e.fixAttention(ctx, job, withUnread("fix: "+note, unread))
			return
		}
		repliedTo := "the findings"
		switch {
		case job.alone && counted: // a review that found nothing: the fixer answered the other reviewers alone
			repliedTo = textx.Count(threads, "thread or comment", "threads and comments") + " of other reviewers"
		case job.alone:
			repliedTo = "the other reviewers"
		case counted:
			repliedTo = textx.Count(threads, "finding", "findings")
		}
		did := "pushed nothing"
		if described {
			did = "updated the description"
		}
		what := fmt.Sprintf("PR #%d: %s %s, replied to %s", job.pr.Number, job.role.Name, did, repliedTo)
		body, line := job.pr.URL+"\nthe fix loop ends; the report: "+report, what
		if unread != "" {
			body, line = body+"\n"+unread, line+"; "+unread
		}
		e.info(notify.Item{Key: "fix-replied:" + job.run.ID, Title: "magnum: " + what, Body: body, Line: line + " " + job.pr.URL, Kind: kindFixEnded})
	}
}

// endCIFix ends a CI fix whose fixer pushed nothing and left nothing else
// to the author, quietly: its run is verified, its row goes (its outcome
// first: merged, where the daemon's merge of the parent moved the head,
// with the class word of the fixer's ci entry when it gave one, or cannot
// with that word, or unfixed), a fix.ended event gives the class and no
// name, and an info toast says what the look's verdict then says (needs
// you, kind ci). Nothing is raised to needs_attention, the verdict holds
// the PR, but the merge of the parent the daemon did not push: that goes
// to you as any loop's does (parentRefusedNote).
func (e *Engine) endCIFix(ctx context.Context, job *fixJob, cannot bool, data map[string]any) {
	outcome := "replied"
	if cannot {
		outcome = "needs_you"
	}
	e.finishFixRun(ctx, job.run, store.RunVerified, outcome, "")
	e.endFixState(ctx, job)
	why := "the CI fix found " + ciCannotClasses[cmp.Or(job.ciWhy, "unfixed")]
	switch {
	case job.ciOutcome == ciOutcomeMerged && job.ciWhy != "":
		why += ", on magnum's merge of the parent " + textx.ShortSHA(job.head)
	case job.ciOutcome == ciOutcomeMerged:
		why = "magnum merged the parent, and CI runs on the merge " + textx.ShortSHA(job.head)
	}
	data["ci_outcome"], data["ci_class"] = job.ciOutcome, job.ciWhy
	e.event(ctx, "info", job.subject(), "fix.ended", "the CI fix ends: "+why, data)
	if note := job.parentRefusedNote(); note != "" { // fix_parent.go: the parent the first cycle did not merge is yours
		e.event(ctx, "warn", job.subject(), "fix.needs_you", "fix: "+note, data)
		e.fixAttention(ctx, job, "fix: "+note)
		return
	}
	what := fmt.Sprintf("PR #%d: %s", job.pr.Number, why)
	e.info(notify.Item{Key: "fix-ci:" + job.run.ID, Title: "magnum: " + what, Body: job.pr.URL + "\nthe report: " + deref(job.run.ReportPath),
		Line: what + " " + job.pr.URL, Kind: kindFixEnded})
}

// typedSentence ends what the settle says of a run the operator typed into
// (operatorTyped): fix.replied, or fix.failed and its attention.
const typedSentence = "; the operator typed into the fixer's pane during the run"

// operatorTyped reports whether the operator typed into the fixer's pane
// while the job's run was in flight (Agents.TypedDuringRun: a claude
// fixer's transcript; any other kind says no). The observer's
// human_active_at cannot tell: it is set only while no run is in flight.
// The run is read again for its session, which Submit recorded.
func (e *Engine) operatorTyped(ctx context.Context, job *fixJob) bool {
	run, err := e.st.RunByID(ctx, job.run.ID)
	if err != nil {
		e.log.Warn("fix: the fixer's run, for typing in its pane", "subject", job.subject(), "err", err)
		return false
	}
	return e.d.Agents.TypedDuringRun(ctx, run)
}

// fixReReview takes the loop to its re-review after a push: the row in
// phase reviewing (the cycle counted, the pushed head, needs the findings
// the fixer left to the author, none after a clean push, and when the loop
// forces its re-review, by which the hook tells that round from the
// others: FixState.ForcedAt), the push on the
// PR row as the poller records one (recordFixPush), and a forced review of
// the PR on it (forceReview, as `magnum review` queues one: no throttle, no
// daily cap, and the fixing hold does not apply to it). The PR is forced
// before onHeadChange sees the push, so the loop's re-review is never taken
// for a push the review covers already (settlePush). A force that fails
// after the head moved still lets onHeadChange see the push before the
// loop ends, so the push is recorded and its re-review queued the normal
// way (the poller, which finds the head already moved, would never queue
// it). A PR GitHub closed meanwhile ends the loop instead. It reports whether
// the loop goes on to its re-review (false: the loop ended, closed or failed).
// answered are the threads among needs that ask only for the resolution
// (freshNeedsYou), kept beside them (FixState.Answered).
func (e *Engine) fixReReview(ctx context.Context, job *fixJob, head string, needs []string, answered map[string]bool) bool {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err == nil && pr.GHState != store.GHOpen {
		e.endFixState(ctx, job)
		e.event(ctx, "info", job.subject(), "fix.ended", fmt.Sprintf("the fix loop ends: the PR is %s on GitHub", strings.ToLower(pr.GHState)), job.eventData())
		return false
	}
	st := job.state
	st.Phase, st.Head, st.NeedsYou, st.ForcedAt, st.Held, st.Confirm = FixPhaseReviewing, head, needs, e.now().UTC(), false, nil
	st.Answered = slices.Sorted(maps.Keys(answered))
	if err == nil {
		err = e.setFixState(ctx, pr.ID, st)
	}
	moved := false
	if err == nil {
		pr, moved, err = e.recordFixPush(ctx, job, pr, head)
	}
	if err == nil {
		_, err = e.forceReview(ctx, job.repo, pr, ReviewPayload{}, nil, fixBy)
	}
	if moved {
		e.seeFixPush(ctx, job, pr.ID)
	}
	if err != nil {
		e.failFix(ctx, job, "fix settle failed: the re-review could not be queued: "+err.Error(), "")
		return false
	}
	job.state = st
	return true
}

// seeFixPush lets onHeadChange see the push recordFixPush put on the PR row,
// as the poller would have: the push recorded for the burst rules,
// pr.head_changed, the approval followed, the PR requeued (a forced one due
// now, else as the throttle says).
func (e *Engine) seeFixPush(ctx context.Context, job *fixJob, prID int64) {
	if cur, err := e.st.PRByID(ctx, prID); err != nil {
		e.log.Warn("fix: the PR after its push", "subject", job.subject(), "err", err)
	} else if err := e.onHeadChange(ctx, job.repo, job.watch, cur, e.now()); err != nil {
		e.log.Warn("fix: the push's head change", "subject", job.subject(), "err", err)
	}
}

// recordFixPush moves the PR row's head to the head the fixer pushed the way
// the poller records a push (store.UpsertPRFromGitHub: head_sha,
// head_changed_at and the activity time), so the forced re-review targets
// it and the poller's own sight of it later changes nothing. A row whose
// head is no longer the reviewed one (the poller saw this push already, or
// a newer one) is left as it is, unless the pushed head descends from it
// (fixPushDescends): the poller saw the base merge the branch gained during
// the run, which `magnum fix-sync` replayed the fixer's commits onto (spec
// 5.7), and the row would otherwise send the forced re-review to the merge
// alone. It returns the PR and whether its head moved; the caller lets
// onHeadChange see the push.
func (e *Engine) recordFixPush(ctx context.Context, job *fixJob, pr store.PR, head string) (store.PR, bool, error) {
	if head == pr.HeadSHA || (pr.HeadSHA != job.head && !e.fixPushDescends(ctx, job, pr.HeadSHA, head)) {
		return pr, false, nil
	}
	res, err := e.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: pr.RepoID, NodeID: pr.NodeID, Number: pr.Number, URL: pr.URL,
		HeadSHA: head, IsDraft: pr.IsDraft})
	if err != nil {
		return pr, false, err
	}
	return res.PR, res.HeadChanged, nil
}

// fixPushDescends reports whether head, the push the settle confirmed on
// GitHub, descends from seen, the PR row's head the poller recorded during
// the run, in the job's slot, which holds both (the fixer fetched the
// branch to replay onto it). A look that fails says no and logs why: the
// row then waits for the poller, as before.
func (e *Engine) fixPushDescends(ctx context.Context, job *fixJob, seen, head string) bool {
	if e.d.Git == nil || seen == "" {
		return false
	}
	ok, err := e.d.Git.IsAncestor(ctx, job.slot.Path, seen, head)
	if err != nil {
		e.log.Warn("fix: whether the push descends from the PR's head", "subject", job.subject(), "head", textx.ShortSHA(seen), "err", err)
	}
	return ok && err == nil
}

// dropMergedConflicts is entries, the fixer's "## Needs you", without its
// conflicts of the base merge when the base's tip the job's fixer was given
// to merge (fixJob.conflict) is an ancestor of head, the head the settle
// confirmed it pushed: the merge is on GitHub, so the conflict asks the
// author for nothing, and the re-review of head judges the merged code. A
// side the fixer kept in that merge stays in its report. A job given no
// conflict, a push without the tip (the fixer aborted the merge and pushed
// its fixes) or a look that fails (logged) keeps every entry. A
// fix.conflict_merged event counts what went.
func (e *Engine) dropMergedConflicts(ctx context.Context, job *fixJob, entries []string, head string) []string {
	tip := job.conflict.sha
	isConflict := func(entry string) bool {
		n, ok := readNeedsYouEntry(entry)
		return ok && n.place == conflictPlace
	}
	if tip == "" || e.d.Git == nil || !slices.ContainsFunc(entries, isConflict) {
		return entries
	}
	merged, err := e.d.Git.IsAncestor(ctx, job.slot.Path, tip, head)
	if err != nil {
		e.log.Warn("fix: whether the push holds the base's tip the fixer merged", "subject", job.subject(), "tip", textx.ShortSHA(tip), "err", err)
		return entries
	}
	if !merged {
		return entries
	}
	kept := slices.DeleteFunc(slices.Clone(entries), isConflict)
	dropped := len(entries) - len(kept)
	data := job.eventData()
	data["conflict_sha"], data["head"], data["dropped"] = tip, head, dropped
	e.event(ctx, "info", job.subject(), "fix.conflict_merged", fmt.Sprintf("%s merged %s at %s and pushed it in %s: its %s %s nothing",
		job.role.Name, job.conflict.base, textx.ShortSHA(tip), textx.ShortSHA(head), textx.Plural(dropped, "conflict entry", "conflict entries"),
		textx.Plural(dropped, "asks", "ask")), data)
	return kept
}

// eventData is the data every fix event carries: the run, the role, the
// cycle and the report.
func (j *fixJob) eventData() map[string]any {
	data := map[string]any{"run": j.run.ID, "role": j.role.Name, "cycle": j.state.Cycle, "cap": j.state.Cap, "report": deref(j.run.ReportPath)}
	if j.state.OthersUnread != "" {
		data["others_unread"] = j.state.OthersUnread
	}
	return data
}

// withUnread is the attention message msg of a loop whose fixer did not get
// all the other reviewers' items, with unread (fixJob.othersUnread) on a
// line of its own: the reason's first line, which attention.Explain reads
// for the calls and the remedy, stays msg, and unread shows under it.
func withUnread(msg, unread string) string {
	if unread == "" {
		return msg
	}
	return msg + "\n" + unread
}

// readFixReport reads the fixer's report of the job's run. Its errors name
// the path and what is wrong, never a line of it: the fixer's text may
// carry the PR's.
func (e *Engine) readFixReport(job *fixJob) (FixReport, error) {
	path := deref(job.run.ReportPath)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return FixReport{}, fmt.Errorf("%s wrote no report at %s", job.role.Name, path)
	case err != nil:
		return FixReport{}, err
	}
	rep, err := ParseFixReport(string(b), job.run.ID)
	switch {
	case err == nil:
		return rep, nil
	case errors.Is(err, ErrFixReportMarker), errors.Is(err, ErrFixReportPartial):
		return FixReport{}, fmt.Errorf("%w: %s", err, path)
	}
	return FixReport{}, fmt.Errorf("the report at %s has a line that does not fit the fix skill's format", path)
}

// pushedHead is the head the fixer pushed: the slot's HEAD, which `git push
// origin HEAD:<branch>` sent, once it is one of the commits the report lists
// (the report gives short shas; Resync needs the whole one) and GitHub shows
// it as the PR's head (confirmPush, on stop, the job's own context): the
// fixer's word alone would record a head GitHub lacks on the PR row and
// force a review of it. The push is recorded before its confirmation
// (noteFixPush) and the record goes once GitHub confirms it; it stays for a
// push GitHub does not confirm, or does not answer about, and for a
// confirmation stop cut (fix_confirm.go).
func (e *Engine) pushedHead(ctx, stop context.Context, job *fixJob, rep FixReport, typed bool) (string, error) {
	if e.d.Git == nil {
		return "", errors.New("no git to read the checkout's HEAD")
	}
	head, err := e.d.Git.RevParse(ctx, job.slot.Path, "HEAD")
	if err != nil {
		return "", fmt.Errorf("the checkout's HEAD: %w", err)
	}
	if !slices.ContainsFunc(rep.Pushed, func(c FixCommit) bool { return strings.HasPrefix(head, c.SHA) }) {
		return "", fmt.Errorf("the checkout's HEAD %s is none of the %s the report lists", textx.ShortSHA(head),
			textx.Count(len(rep.Pushed), "commit", "commits"))
	}
	// fix_parent.go: a base merge magnum push handed to magnum (it carries
	// the base's workflow files) is the daemon's to push, or to hold for
	// your y (fix_held_merge.go)
	if refused, err := e.pushHanded(ctx, job, head); errors.As(err, new(fixMergeHeld)) {
		return "", err
	} else if err != nil {
		return "", fmt.Errorf("magnum's push of %s's merge %s: %w", job.role.Name, textx.ShortSHA(head), err)
	} else if refused != "" {
		return "", fixHandRefused{why: refused}
	}
	e.noteFixPush(ctx, job, rep, head, typed) // fix_confirm.go
	if err := e.confirmPush(stop, job, head); err != nil {
		return "", err
	}
	e.dropFixPush(ctx, job.pr.ID)
	return head, nil
}

const (
	// fixPushLooks bounds how often confirmPush looks at a PR's head that
	// GitHub shows otherwise, fixPushWait apart: GitHub moves a PR's head a
	// moment after the push to its branch.
	fixPushLooks = 3
	fixPushWait  = 5 * time.Second
	// fixLsRemoteTimeout bounds the `git ls-remote` confirmPush falls back on.
	fixLsRemoteTimeout = 30 * time.Second
)

// remoteLister is the git capability confirmPush falls back on
// (*gitx.Client has it): `git ls-remote origin` in a checkout.
type remoteLister interface {
	LsRemote(ctx context.Context, dir string, timeout time.Duration, patterns ...string) (string, error)
}

// remoteHead is the head of the job's PR on GitHub: the poll identity's
// client's answer (Details), else, when that client cannot say, the
// branch's tip on origin (`git ls-remote` in the slot), with both errors
// when neither answers.
func (e *Engine) remoteHead(ctx context.Context, job *fixJob) (string, error) {
	var why error
	if gh := e.gh(job.watch.PollIdentity); gh == nil {
		why = fmt.Errorf("no GitHub client for poll identity %q", job.watch.PollIdentity)
	} else if det, _, err := gh.Details(ctx, job.repo.Owner, job.repo.Name, []int{job.pr.Number}); err != nil {
		why = err
	} else if d, ok := det[job.pr.Number]; !ok || d.HeadRefOid == "" {
		why = errors.New("GitHub does not say where the PR's head is")
	} else {
		return d.HeadRefOid, nil
	}
	branch := deref(job.pr.HeadRef)
	git, ok := e.d.Git.(remoteLister)
	if !ok || branch == "" {
		return "", why
	}
	out, err := git.LsRemote(ctx, job.slot.Path, fixLsRemoteTimeout, "refs/heads/"+branch)
	if err != nil {
		return "", errors.Join(why, fmt.Errorf("git ls-remote: %w", err))
	}
	for line := range strings.Lines(out) {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && ref == "refs/heads/"+branch {
			return sha, nil
		}
	}
	return "", errors.Join(why, fmt.Errorf("origin has no branch %s", branch))
}

// resyncFixSlot puts the job's slot back in order at sha (Slots.Resync:
// detached, recorded as checked out), once the slot is still claimed or
// held for the PR.
func (e *Engine) resyncFixSlot(ctx context.Context, job *fixJob, sha string) error {
	return e.resyncSlot(ctx, job.pr.ID, job.slot, sha)
}

// resyncSlot puts slot, PR prID's, back in order at sha (Slots.Resync), once
// it is still claimed or held for the PR: the fix and whiteboard jobs'.
func (e *Engine) resyncSlot(ctx context.Context, prID int64, of store.Slot, sha string) error {
	slot, err := e.st.SlotByID(ctx, of.ID)
	if err != nil {
		return err
	}
	if deref(slot.PRID) != prID || (slot.State != store.SlotClaimed && slot.State != store.SlotHeld) {
		return fmt.Errorf("slot %s is no longer the PR's (%s)", slot.Name, slot.State)
	}
	return e.d.Slots.Resync(ctx, slot, sha)
}

// fixThreads reads the thread count off a "## Replied" line (ok false when
// it does not start with one).
func fixThreads(line string) (int, bool) {
	m := fixThreadsRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// failFix ends the job's loop as failed: its run fails (one still active),
// the loop's row goes, a fix.failed event names msg and the report (report,
// "" = the run's), and the operator is told (fixAttention).
func (e *Engine) failFix(ctx context.Context, job *fixJob, msg, report string) {
	e.failFixTyped(ctx, job, msg, report, false)
}

// failFixTyped is failFix for a settle that knows whether the operator
// typed into the fixer's pane during the run (typed, operatorTyped): when
// so, msg ends with typedSentence for the event and the attention alike,
// and the event's data carries operator_typed.
func (e *Engine) failFixTyped(ctx context.Context, job *fixJob, msg, report string, typed bool) {
	e.failFixKind(ctx, job, msg, report, typed, "")
}

// failFixKind is failFixTyped with the failure's kind, which its fix.failed
// event's data carries (kind; "" for none): fixPushUnconfirmed, the one the
// poll may settle late (fix_confirm.go).
func (e *Engine) failFixKind(ctx context.Context, job *fixJob, msg, report string, typed bool, kind string) {
	if typed {
		msg += typedSentence
	}
	if job.run.ID != "" {
		e.finishFixRun(ctx, job.run, store.RunFailed, "failed", msg)
	}
	job.ciOutcome = cmp.Or(job.ciOutcome, ciOutcomeFailed)
	e.endFixState(ctx, job)
	data := job.eventData()
	if report != "" {
		data["report"] = report
	}
	if typed {
		data["operator_typed"] = true
	}
	if kind != "" {
		data["kind"] = kind
	}
	e.event(ctx, "warn", job.subject(), "fix.failed", msg, data)
	if carried := carriedText(job); carried != "" { // fix_carry.go: the calls of the loop's earlier cycles stay the author's
		msg += "; " + carried
	}
	e.fixAttention(ctx, job, withParentRefused(msg, job)) // fix_parent.go
}

// endFixState ends the job's loop: its row goes, once a CI fix's end is
// recorded (noteCIFixEnd, ci_fix.go).
func (e *Engine) endFixState(ctx context.Context, job *fixJob) {
	e.noteCIFixEnd(ctx, job)
	if err := e.clearFixState(ctx, job.pr.ID); err != nil {
		e.log.Warn("fix: clear the loop's row", "subject", job.subject(), "err", err)
	}
}

// fixAttention tells the operator that the job's loop needs them, with msg,
// as the fix job's attention (attention.KindFix: the fixer, its report and
// `magnum fix` or `magnum abort` as the next step). A reviewed PR goes to
// needs_attention (needsAttention: its toast and attention key), and so
// does one already there, a second loop's (`magnum fix` once the author
// answered the first): its message replaces the first loop's, and keeps the
// first's decision calls that GitHub does not show built when it asks for
// no call of its own (keepDecisions: a failed run, the cap), at the time
// recorded for them (recordFixCalls, which dates the calls the loop carried
// from its earlier cycles at the run that made each, carriedCallTimes, and
// the message's other calls at its report's run). A PR waiting
// for a round keeps it, because a push came during the fix and that
// re-review still runs: it gets the urgent toast alone. A PR elsewhere
// (closed, ineligible) has the events only. The toast's dedupe key is the
// loop's cycle's own (fixAttentionKey), so each loop that needs the author
// toasts and a cycle's call is told once, however late the loop ends; a
// late settle of the cycle's push has a key of its own.
func (e *Engine) fixAttention(ctx context.Context, job *fixJob, msg string) {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("fix: the PR is gone", "subject", job.subject(), "err", err)
		return
	}
	rj := job.asRound()
	rj.pr = pr
	var kept map[int64]time.Time // fix_decide.go: the calls kept from the attention replaced, with when they were made
	if kind, _ := e.getKV(ctx, KVPRAttention(pr.ID)); pr.State == store.PRNeedsAttention && kind == attention.KindFix {
		calls := e.fixerCalls(ctx, pr)
		if keep := keepDecisions(deref(pr.LastError), msg, e.callsOpen(ctx, job, calls)); keep != msg {
			msg, kept = keep, calls
		}
	}
	for id, at := range e.carriedCallTimes(ctx, job) { // fix_carry.go: a carried call dates from the cycle that made it
		if _, ok := kept[id]; !ok {
			if kept == nil {
				kept = map[int64]time.Time{}
			}
			kept[id] = at
		}
	}
	switch {
	case pr.State == store.PRReviewed || pr.State == store.PRNeedsAttention:
		e.needsAttentionKeyed(ctx, rj, pr, []string{store.PRReviewed, store.PRNeedsAttention}, attention.KindFix, msg, nil,
			fixAttentionKey(job), fixAttentionWindow)
		e.recordFixCalls(ctx, pr.ID, msg, kept)
	case slices.Contains(waitingStates, pr.State):
		e.fixCallToast(job, msg)
	}
}

// fixAttentionWindow is how long a loop cycle's attention toast (its key,
// fixAttentionKey) holds back a second one: a day, since the key is the
// cycle's own and a re-review can wait for hours before the loop's end asks
// for the same call again.
const fixAttentionWindow = 24 * time.Hour

// fixAttentionKey is the dedupe key of the toast of the job's loop cycle:
// the PR, the loop (when it started) and the cycle. The loop's end after a
// re-review (loopJob, which has no run) builds the key of the settle that
// started that re-review, so a call toasted at once is not toasted again.
func fixAttentionKey(job *fixJob) string {
	key := fmt.Sprintf("attention:%d:%s:%s:%d", job.pr.ID, attention.KindFix, store.FormatTime(job.state.StartedAt), job.state.Cycle)
	if job.late { // fix_confirm.go: the late settle of a cycle whose failure toasted under the key
		key += ":late"
	}
	return key
}

// fixCallToast is the fix job's urgent toast alone (no state change): msg,
// as the fix job's attention, for the PR the job's loop is on. The settle
// raises it when the fixer pushed and left findings to the author, so they
// can act on them while the push is re-reviewed; fixAttention raises it for
// a PR waiting for a round.
func (e *Engine) fixCallToast(job *fixJob, msg string) {
	label := fmt.Sprintf("%s#%d", job.repo.Name, job.pr.Number)
	e.urgent(fixAttentionKey(job), "magnum: "+label+" needs attention", attentionToast(attention.Explain(attention.KindFix, msg, label)), fixAttentionWindow)
}

// finishFixRun moves the fixer's run to to (verified or failed) with
// outcome and errMsg; a run already final keeps its state and outcome.
func (e *Engine) finishFixRun(ctx context.Context, run store.Run, to, outcome, errMsg string) {
	from := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	if to == store.RunVerified {
		from = from[1:]
	}
	now := e.now()
	err := e.st.TransitionRun(context.WithoutCancel(ctx), run.ID, from, to, func(u *store.RunUpdate) {
		u.Set("outcome", outcome)
		if errMsg != "" {
			u.Set("error", oneLine(errMsg, 300))
		}
		if to == store.RunVerified {
			u.Set("verified_at", now)
		} else if run.EndedAt == nil {
			u.Set("ended_at", now)
		}
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("fix: finish run", "run", run.ID, "to", to, "err", err)
	}
}

// stopFix serves the abort and ignore orders that stopped the job: the
// fixer is interrupted and its run abandoned, the slot is put back at the
// reviewed head when the fixer may have touched it, and the loop ends
// quietly (its row goes, an info toast, no attention); an ignore also mutes
// the PR as `magnum ignore` does. The orders are completed with what
// happened.
func (e *Engine) stopFix(ctx context.Context, job *fixJob, orders []stopOrder) {
	var done []string
	var problems []error
	if job.run.ID != "" {
		if e.interruptSessions(ctx, job.pr, &job.watch, job.role.Name) > 0 {
			done = append(done, "fixer interrupted")
			if !e.waitAgentsQuiet(ctx, job.pr.ID) {
				e.log.Info("the fixer still works after the interrupt", "subject", job.subject())
			}
		}
		if e.abandonFixRun(ctx, job.run, abortedBy) {
			done = append(done, "run "+job.run.ID+" abandoned")
		}
	}
	job.ciOutcome = ciOutcomeStopped // ci_fix.go
	if job.touched {
		if err := e.resyncFixSlot(ctx, job, job.head); err != nil {
			problems = append(problems, fmt.Errorf("slot %s not put back: %w", job.slot.Name, err))
		} else {
			done = append(done, "slot "+job.slot.Name+" back at "+textx.ShortSHA(job.head))
		}
	}
	e.endFixState(ctx, job)
	ignore := slices.ContainsFunc(orders, func(o stopOrder) bool { return o.ignore })
	what, kind := "aborted", "pr.aborted"
	if ignore {
		what, kind = "ignored", "pr.ignored"
		if to, err := e.settleStopped(ctx, job.pr, true); err != nil {
			problems = append(problems, err)
		} else if to != "" {
			done = append(done, "PR "+to)
		}
	}
	res := what + " the fix of " + job.label()
	if len(done) > 0 {
		res += ": " + strings.Join(done, ", ")
	}
	if ignore {
		res += "; muted until `magnum unmute " + job.label() + "`"
	}
	perr := errors.Join(problems...)
	level := "info"
	if perr != nil {
		level, res = "warn", res+"; "+perr.Error()
	}
	data := job.eventData()
	data["fix"] = true
	e.event(ctx, level, job.subject(), kind, res, data)
	e.toastFixStopped(job.repo, job.pr, job.state, res)
	for _, o := range orders {
		if perr != nil {
			e.complete(ctx, o.id, errors.New(res), "")
		} else {
			e.complete(ctx, o.id, nil, res)
		}
		e.mu.Lock()
		delete(e.inflight, o.id)
		e.mu.Unlock()
	}
}

// endFixLoop ends a fix loop that no fix job holds (one waiting for its
// re-review, or the row of a stopped daemon) for a stop of the PR that holds
// its reservation: its row goes, with an event and an info toast. The row
// goes only while it is the one read here (Store.DeleteKVIf; claimFixRow is
// the other side), and an abort without the reservation goes through
// abortFixLoop, which hands a fix job that claimed the PR meanwhile the
// stop instead. A PR without a loop has its held fix (a drain's, the
// GitHub budget's or the fixer's pause's) taken back instead (dropHeldFix).
// It returns what it did for the
// stop's answer ("" = the PR had neither, or a cycle took the loop on
// meanwhile).
func (e *Engine) endFixLoop(ctx context.Context, repo store.Repo, pr store.PR, by string) string {
	st, raw, _, err := e.fixRow(ctx, pr.ID)
	if note := e.endFixLoopRow(ctx, repo, pr, st, raw, err, by); note != "" {
		return note
	}
	return e.dropHeldFix(ctx, repo, pr, by) // fix_held.go: a held fix, which no loop runs yet
}

// endFixLoopRow is endFixLoop for the row the caller read: st, raw as stored
// ("" = none) and readErr, the error of a row that cannot be read.
func (e *Engine) endFixLoopRow(ctx context.Context, repo store.Repo, pr store.PR, st FixState, raw string, readErr error, by string) string {
	if raw == "" {
		return ""
	}
	if gone, cerr := e.st.DeleteKVIf(ctx, KVPRFix(pr.ID), raw); cerr != nil || !gone {
		if cerr != nil {
			e.log.Warn("stop: clear the fix loop's row", "pr", pr.ID, "err", cerr)
		}
		return ""
	}
	note := fmt.Sprintf("fix loop ended (cycle %d of %d, %s)", st.Cycle, st.Cap, st.Phase)
	if readErr != nil {
		note = "the fix loop's unreadable row cleared"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.stopped", note+" by "+by, map[string]any{"by": by, "report": st.Report})
	e.toastFixStopped(repo, pr, st, note)
	return note
}

// abortFixLoopTries bounds how often abortFixLoop looks at a loop that keeps
// changing under it.
const abortFixLoopTries = 3

// abortFixLoop serves `magnum abort` (request id) for the fix loop of a PR
// no reservation held when the abort looked. The hook after a round can
// reserve the PR and claim the loop's next cycle meanwhile, so the abort
// reads the row, then looks at the reservation again: a fix job holding the
// PR gets the abort as its stop order (stopHeld: its fixer interrupted, the
// loop ended quietly by stopFix), never a bare delete of the row the job
// works under; with no reservation, the row read goes (endFixLoopRow, a
// compare-and-set), and a row that changed since it was read (a cycle took
// the loop on) is looked at again. A PR without a loop has its held fix
// taken back (dropHeldFix). It reports whether it
// answered the request (false: the PR has neither).
func (e *Engine) abortFixLoop(ctx context.Context, id int64, repo store.Repo, pr store.PR) bool {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	for range abortFixLoopTries {
		st, raw, _, err := e.fixRow(ctx, pr.ID)
		if raw == "" {
			if note := e.dropHeldFix(ctx, repo, pr, "magnum abort"); note != "" {
				e.complete(ctx, id, nil, "aborted "+label+": "+note)
				return true
			}
			return false
		}
		if e.stopHeld(ctx, id, repo, pr, false) {
			return true
		}
		if note := e.endFixLoopRow(ctx, repo, pr, st, raw, err, "magnum abort"); note != "" {
			e.complete(ctx, id, nil, "aborted "+label+": "+note)
			return true
		}
	}
	e.complete(ctx, id, fmt.Errorf("%s: its fix loop changed while the abort looked at it; run `magnum abort %s` again", label, label), "")
	return true
}

// toastFixStopped is the info toast of a fix loop a stop ended (st: its
// row), with what the stop did.
func (e *Engine) toastFixStopped(repo store.Repo, pr store.PR, st FixState, what string) {
	title := fmt.Sprintf("magnum: fix stopped: %s#%d", repo.Name, pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("fix-stopped:%d:%s:%d", pr.ID, store.FormatTime(st.StartedAt), st.Cycle), Title: title,
		Body: pr.URL + "\n" + what, Line: strings.TrimPrefix(title, "magnum: ") + " " + pr.URL, Kind: kindFixEnded})
}
