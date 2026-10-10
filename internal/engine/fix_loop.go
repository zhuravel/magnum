package engine

// The fix loop between its cycles (fix.go runs one cycle, fix_settle.go
// ends it): a review round's end, once its reservation is gone, decides what
// the PR's loop does next (fixAfterRound). A loop waiting for the re-review
// of its fixer's push (phase reviewing) goes on to its next cycle while that
// review finds something and cycles are left (fix_cycles), and ends
// otherwise: done when the review found nothing, as needs attention at the
// cap, after findings the fixer left to the author, or when the re-review
// did not post. A PR without a loop gets one by itself after a review with
// findings when its watch says autofix = "own", the PR is the operator's
// and it was not switched out of autofix (`magnum fix --off`).

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
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
// place ("2101 P1 app/models/offer.rb:42: <reason>"). The reason is the
// fixer's own words about the PR, so it is never read.
var needsYouEntryRe = regexp.MustCompile(`^(\d+)\s+(P[0-3])\s+(\S+?:\d+)\b`)

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
// over it.
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
	st, ok, err := e.fixState(ctx, cur.ID)
	switch {
	case err != nil:
		e.log.Warn("fix: the loop's row cannot be read; nothing starts over it", "subject", prSubject(repo, cur.Number), "err", err)
	case !ok:
		e.autofix(ctx, repo, cur, res)
	case st.Phase == FixPhaseReviewing:
		e.continueFixLoop(ctx, e.loopJob(ctx, repo, cur, st), res)
	}
}

// continueFixLoop takes a loop whose re-review ended (res) to its next
// step: a re-review that did not post, or findings the fixer left to the
// author when it pushed, end it as needs attention; a review that found
// nothing ends it as done; findings start the next cycle (startFix) while
// cycles are left, and end it as needs attention at the cap; a next cycle
// that cannot start ends it as needs attention with why.
func (e *Engine) continueFixLoop(ctx context.Context, job *fixJob, res pipeline.RoundResult) {
	st := job.state
	found := roundFindings(res)
	switch {
	case res.Outcome != pipeline.OutcomePosted:
		msg := fmt.Sprintf("fix loop ended: the re-review did not post (%s)", cmp.Or(res.Outcome, pipeline.OutcomeError))
		if len(st.NeedsYou) > 0 {
			msg += "; " + needsYourCall(st.NeedsYou, st.Report)
		}
		e.endLoopForAttention(ctx, job, "fix.failed", msg)
	case len(st.NeedsYou) > 0:
		e.endLoopForAttention(ctx, job, "fix.needs_you", "fix: "+needsYourCall(st.NeedsYou, st.Report))
	case found == 0:
		e.fixLoopDone(ctx, job, cmp.Or(res.TargetSHA, st.Head))
	case st.Cycle >= st.Cap:
		e.endLoopForAttention(ctx, job, "fix.capped", fmt.Sprintf("%s, %s left: %s",
			textx.Count(st.Cycle, "fix cycle", "fix cycles"), textx.Count(found, "finding", "findings"), st.Report))
	default:
		if _, err := e.startFix(ctx, job.pr, st.By, st); err != nil {
			e.endLoopForAttention(ctx, job, "fix.failed", fmt.Sprintf("fix loop ended: cycle %d did not start: %v", st.Cycle+1, err))
		}
	}
}

// fixLoopDone ends a loop whose re-review of head found nothing: its row
// goes, fix.done is recorded and an info toast says how many cycles it took.
func (e *Engine) fixLoopDone(ctx context.Context, job *fixJob, head string) {
	st, pr := job.state, job.pr
	e.endFixState(ctx, job)
	cycles := textx.Count(st.Cycle, "fix cycle", "fix cycles")
	e.event(ctx, "info", job.subject(), "fix.done", fmt.Sprintf("the fix loop ends clean after %s: the re-review of %s found nothing",
		cycles, textx.ShortSHA(head)), job.loopData())
	what := fmt.Sprintf("PR #%d: clean after %s", pr.Number, cycles)
	e.info(notify.Item{Key: fmt.Sprintf("fix-done:%d:%s", pr.ID, store.FormatTime(st.StartedAt)), Title: "magnum: " + what,
		Body: pr.URL + "\nthe re-review of " + textx.ShortSHA(head) + " found nothing", Line: what + " " + pr.URL, Kind: kindFixEnded})
}

// endLoopForAttention ends a loop after its re-review as needs attention
// with msg: its row goes, an event of kind names msg and the last report,
// and the operator is told as the fix job's attention (fixAttention), unless
// the round itself left the PR needing attention, whose account of why
// stays.
func (e *Engine) endLoopForAttention(ctx context.Context, job *fixJob, kind, msg string) {
	e.endFixState(ctx, job)
	e.event(ctx, "warn", job.subject(), kind, msg, job.loopData())
	if pr, err := e.st.PRByID(ctx, job.pr.ID); err == nil && pr.State == store.PRNeedsAttention {
		return
	}
	e.fixAttention(ctx, job, msg)
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

// roundFindings is how many findings (P0 to P3) a round's review posted.
func roundFindings(res pipeline.RoundResult) int {
	return res.Findings["P0"] + res.Findings["P1"] + res.Findings["P2"] + res.Findings["P3"]
}

// needsYourCall says which findings the fixer left to the author and where
// its report is: "2 findings need your call (2101 P1 app/a.rb:42, 2102 P2
// app/b.rb:7): <report>". Each entry is named by its comment id, priority
// and place alone; an entry in another form is only counted.
func needsYourCall(entries []string, report string) string {
	var named []string
	other := 0
	for _, entry := range entries {
		m := needsYouEntryRe.FindStringSubmatch(strings.TrimSpace(entry))
		if m == nil {
			other++
			continue
		}
		named = append(named, m[1]+" "+m[2]+" "+m[3])
	}
	if other > 0 {
		named = append(named, textx.Count(other, "entry", "entries")+" in another form")
	}
	n := len(entries)
	s := textx.Count(n, "finding", "findings") + " " + textx.Plural(n, "needs", "need") + " your call"
	if len(named) > 0 {
		s += " (" + strings.Join(named, ", ") + ")"
	}
	return s + ": " + report
}
