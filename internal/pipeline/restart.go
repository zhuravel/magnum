package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// A push the poller records while the reviewers run (and the judge's own
// pass, ownpass.go; before the judge is prompted for the candidates)
// restarts them in place on the new head, at most RoundInput.MaxRestarts
// times per round: the turns in flight are interrupted and their runs
// abandoned (ReportHeadMoved), RoundInput.Switch
// checks the new head out, the round's target and report directory follow
// it, and every role that runs gets a new run on the same session, a session
// reviewer with its restart prompt (config.PromptRestart) when it has one.
// After the last restart the round finishes on the head it has.
//
// The reviewers' waits look at the PR row every poll (noticePush) and cancel
// the stages with a *headMovedError cause; a stage of shell roles only
// notices a push when it ends, as every stage does before the next one.

// headMovedError is the cause of a stage context a push cancelled.
type headMovedError struct{ sha string }

func (e *headMovedError) Error() string {
	return "pipeline: the PR head moved to " + textx.ShortSHA(e.sha)
}

// pushCut reports whether ctx was cancelled by a push (noticePush): a turn it
// cut short leaves its run for restart to settle.
func pushCut(ctx context.Context) bool {
	_, ok := errors.AsType[*headMovedError](context.Cause(ctx))
	return ok
}

// canRestart reports whether the round may still restart its reviewers.
func (rd *round) canRestart() bool {
	return rd.in.Switch != nil && rd.in.Kind != KindContinue && rd.restarts < rd.in.MaxRestarts
}

// movedHead returns the PR's head when the poller recorded a push since the
// round last looked and the round may still restart; "" otherwise. Neither
// the round's own target nor an ancestor of it counts: the checkout may have
// fetched past the head the poller saw last.
func (rd *round) movedHead(ctx context.Context) string {
	if !rd.canRestart() {
		return ""
	}
	pr, err := rd.r.Store.PRByID(ctx, rd.in.PR.ID)
	if err != nil {
		return ""
	}
	head, target := pr.HeadSHA, rd.in.TargetSHA
	rd.mu.Lock()
	known := head == "" || head == rd.seenHead
	if head == target {
		rd.seenHead, known = head, true
	}
	rd.mu.Unlock()
	if known {
		return ""
	}
	if rd.r.Git != nil {
		if mb, err := rd.r.Git.MergeBase(ctx, rd.in.SlotPath, head, target); err == nil && mb == head {
			rd.logf("pipeline: %s: head %s is an ancestor of %s: no push", rd.subject, textx.ShortSHA(head), textx.ShortSHA(target))
			rd.mu.Lock()
			rd.seenHead = head
			rd.mu.Unlock()
			return ""
		}
	}
	return head
}

// noticePush cancels the running stages (cause *headMovedError) once the
// poller recorded a push the round may restart on. Every wait calls it each
// poll; outside the stages it does nothing.
func (rd *round) noticePush(ctx context.Context) {
	rd.mu.Lock()
	cancel := rd.stageCancel
	rd.mu.Unlock()
	if cancel == nil {
		return
	}
	if head := rd.movedHead(ctx); head != "" {
		cancel(&headMovedError{sha: head})
	}
}

// reviewers runs the stages, restarting them on a newer head while restarts
// remain, and returns the judge's run (a restart replaces it: a run carries
// its target and report path). own is the judge's own-pass run, prompted
// with the first stage and replaced by a restart too (nil = none:
// ownPassDue).
func (rd *round) reviewers(ctx context.Context, runs map[string]*store.Run, judgeRun, own *store.Run) (*store.Run, error) {
	for {
		head, err := rd.runStages(ctx, runs, own, rd.marker(*judgeRun))
		if err != nil || head == "" {
			return judgeRun, err
		}
		if judgeRun, own, err = rd.restart(ctx, head, runs, judgeRun, own); err != nil {
			return nil, err
		}
	}
}

// runStages runs the stages in order under a context a push cancels while
// restarts remain, with the judge's own pass on own (when not nil) beside
// them from the first stage on, and returns once both ended. head is the
// PR's new head when the round must restart: a push noticed while a stage
// or the own pass ran, or by the check before each stage and after the
// last. It first notes the checkout, which each stage's roles and the own
// pass must leave as they found it (checkTree). marker is the run id the
// round's review carries, which the own-pass prompt quotes.
func (rd *round) runStages(ctx context.Context, runs map[string]*store.Run, own *store.Run, marker string) (head string, err error) {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if len(rd.stages) > 0 {
		rd.noteTree(ctx)
	}
	if rd.canRestart() {
		rd.mu.Lock()
		rd.stageCancel = cancel
		rd.mu.Unlock()
		defer func() {
			rd.mu.Lock()
			rd.stageCancel = nil
			rd.mu.Unlock()
		}()
	}
	var op *ownPass
	var also func() []string // names the judge in a stage's checkout check while its own pass works
	if own != nil {
		op = rd.startOwnPass(sctx, *own, marker)
		covered := false // a check ran after the own pass ended: it left nothing unchecked
		also = func() []string {
			if covered {
				return nil
			}
			covered = op.finished()
			return []string{rd.judge.Name}
		}
	}
	// fail ends the stages on err: the own pass stops with them (a push
	// leaves its run to the restart; anything else interrupts it).
	fail := func(err error) (string, error) {
		rd.stopOwnPass(ctx, op, own, cancel, err)
		return "", err
	}
	for _, stage := range rd.stages {
		if head := rd.movedHead(ctx); head != "" {
			rd.stopOwnPass(ctx, op, own, cancel, &headMovedError{sha: head})
			return head, nil
		}
		err := rd.runStage(sctx, stage, runs, also)
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if moved, ok := errors.AsType[*headMovedError](context.Cause(sctx)); ok {
			if err != nil && !errors.Is(err, context.Canceled) {
				return fail(err) // a checkout that could not be restored is no place to restart in
			}
			rd.stopOwnPass(ctx, op, own, cancel, moved)
			return moved.sha, nil
		}
		if err != nil {
			return fail(err)
		}
	}
	if op != nil {
		<-op.done // its waits notice a push meanwhile
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if moved, ok := errors.AsType[*headMovedError](context.Cause(sctx)); ok {
			return moved.sha, nil
		}
		if names := also(); len(names) > 0 {
			if err := rd.checkTree(ctx, names); err != nil {
				return "", err
			}
		}
	}
	return rd.movedHead(ctx), nil
}

// stopOwnPass ends the judge's own pass (op, on run own) when the stages
// end before it did: their context is cancelled with cause, and once its
// turn returned, a turn a push cut is left to the restart (settleCut) and
// one the round's cancellation stopped stays in flight as a reviewer's;
// any other cause (a checkout that could not be restored) interrupts the
// judge and fails the run, so it does not go on working once the round
// ended. nil op does nothing.
func (rd *round) stopOwnPass(ctx context.Context, op *ownPass, own *store.Run, cancel context.CancelCauseFunc, cause error) {
	if op == nil {
		return
	}
	if !op.finished() {
		cancel(cause)
		<-op.done
	}
	if _, push := cause.(*headMovedError); push || ctx.Err() != nil || op.rep.Status != ReportCancelled {
		return
	}
	run := rd.ownRun(*own)
	cur, err := rd.r.Store.RunByID(context.WithoutCancel(ctx), run.ID)
	if err != nil || (cur.State != store.RunSubmitted && cur.State != store.RunWorking && cur.State != store.RunEnded) {
		return
	}
	rd.stopJudge(ctx, cur)
	rd.finishRun(ctx, cur.ID, store.RunFailed, ReportCancelled, "the round ended before the own pass did: "+cause.Error())
}

// ownRun is the own pass's latest run: own, or its continuation on a
// fallback model (modelFallback keeps it under the judge's name).
func (rd *round) ownRun(own store.Run) store.Run {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if c, ok := rd.cont[rd.judge.Name]; ok && c.Kind == store.RunOwnPass {
		return c
	}
	return own
}

// restart starts the reviewers over on head: the turns cut short are
// settled (settleCut), the judge's own pass (own, when not nil) like a
// reviewer's, the checkout switches to head (RoundInput.Switch), the round's
// target, merge base and report directory follow it, and every role that
// runs gets a new run, the judge too, and the own pass a new one after it.
func (rd *round) restart(ctx context.Context, head string, runs map[string]*store.Run, judgeRun, own *store.Run) (*store.Run, *store.Run, error) {
	from := rd.in.TargetSHA
	cut, ran := []string{}, []string{}
	var ownCut *store.Run
	if own != nil {
		r := rd.ownRun(*own)
		ownCut = &r
	}
	rd.mu.Lock()
	cont := rd.cont
	rd.cont = nil
	rd.ownFindings = ""
	rd.mu.Unlock()
	for _, role := range rd.running() {
		run := runs[role.Name]
		if c, ok := cont[role.Name]; ok && run != nil {
			run = &c // the turn in flight is the continuation on a fallback model
		}
		if run == nil {
			continue
		}
		ran = append(ran, role.Name)
		if rd.settleCut(ctx, role, *run, head) {
			cut = append(cut, role.Name)
		}
	}
	if ownCut != nil {
		ran = append(ran, rd.judge.Name)
		if rd.settleCut(ctx, rd.judge, *ownCut, head) {
			cut = append(cut, rd.judge.Name+" (own pass)")
		}
	}
	rd.finishRun(ctx, judgeRun.ID, store.RunAbandoned, ReportHeadMoved, "the PR head moved to "+textx.ShortSHA(head))
	// The push skipped the check after the stage it cut short; its roles are
	// settled now, so a stray edit is caught before the checkout switches.
	if err := rd.checkTree(ctx, ran); err != nil {
		return nil, nil, fmt.Errorf("pipeline: restart on %s: %w", textx.ShortSHA(head), err)
	}

	sw, err := rd.in.Switch(ctx, head)
	if err != nil {
		return nil, nil, fmt.Errorf("pipeline: restart on %s: %w", textx.ShortSHA(head), err)
	}
	target := cmp.Or(sw.TargetSHA, head)
	rd.restarts++
	rd.restartedFrom = from
	rd.in.TargetSHA, rd.pr.HeadSHA = target, target
	rd.unverified = "" // an earlier review of the old head is no review of this one
	rd.in.BaseSHA = cmp.Or(sw.BaseSHA, rd.in.BaseSHA)
	rd.in.ForcePushed, rd.in.BaseMerged = sw.ForcePushed, sw.BaseMerged
	rd.dir = rd.r.Layout.ReviewDir(rd.owner, rd.name, rd.in.PR.Number, target)
	rd.mu.Lock()
	rd.seenHead = head
	rd.res.ReportDir, rd.res.TargetSHA, rd.res.Restarts = rd.dir, target, rd.restarts
	for _, role := range rd.running() {
		if runs[role.Name] != nil { // a logged-out role keeps its report
			delete(rd.res.Reports, agents.Role(role.Name))
		}
	}
	rd.mu.Unlock()
	if err := os.MkdirAll(rd.dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("pipeline: report dir: %w", err)
	}
	if err := rd.setAsideStale(); err != nil {
		return nil, nil, err
	}
	when := "before the judge was prompted"
	if own != nil {
		when += " for the candidates"
	}
	msg := fmt.Sprintf("the PR head moved from %s to %s %s: restart %d of %d",
		textx.ShortSHA(from), textx.ShortSHA(target), when, rd.restarts, rd.in.MaxRestarts)
	if len(cut) > 0 {
		msg += " (cut short: " + strings.Join(cut, ", ") + ")"
	}
	rd.event(ctx, "info", "round.restarted", msg, map[string]any{"from": from, "to": target, "pushed": head,
		"restart": rd.restarts, "max": rd.in.MaxRestarts, "cut": cut})

	for _, role := range rd.running() {
		if runs[role.Name] == nil {
			continue
		}
		if runs[role.Name], err = rd.newRun(ctx, role, rd.in.Kind); err != nil {
			return nil, nil, err
		}
	}
	if judgeRun, err = rd.newRun(ctx, rd.judge, rd.in.Kind); err != nil || own == nil {
		return judgeRun, nil, err
	}
	own, err = rd.newRun(ctx, rd.judge, store.RunOwnPass)
	return judgeRun, own, err
}

// settleCut ends role's run when the push cut it short: a turn still in
// flight is interrupted, and an agent waited for until idle (InterruptWait),
// then the run is abandoned with ReportHeadMoved. A finished run keeps its
// state. It reports whether the run was cut.
func (rd *round) settleCut(ctx context.Context, role config.Role, run store.Run, head string) bool {
	cur, err := rd.r.Store.RunByID(context.WithoutCancel(ctx), run.ID)
	if err != nil {
		rd.logf("pipeline: restart: run %s: %v", run.ID, err)
		return false
	}
	switch cur.State {
	case store.RunSubmitted, store.RunWorking:
		rd.interrupt(ctx, role, cur)
		if !rd.waitIdle(ctx, role, cur) {
			rd.warn(ctx, "%s still works %s after it was interrupted for the restart", role.Name, InterruptWait)
		}
	case store.RunPending, store.RunEnded:
	default:
		return false
	}
	rd.finishRun(ctx, cur.ID, store.RunAbandoned, ReportHeadMoved, "the PR head moved to "+textx.ShortSHA(head))
	return true
}

// AppendToReview adds text as the last paragraph of review reviewID, which
// the runner's identity posted (editReview), above the footer magnum
// appended (footerMarker), so the footer stays the last paragraph. A body
// that carries text there already is left alone.
func (r *Runner) AppendToReview(ctx context.Context, owner, repo string, number int, reviewID int64, text string) error {
	if r.GitHub == nil || r.Identity == nil {
		return fmt.Errorf("%w: AppendToReview needs GitHub and Identity", ErrInvalid)
	}
	text = strings.TrimSpace(text)
	_, err := r.editReview(ctx, owner, repo, number, reviewID, func(body string) (string, bool) {
		return appendNote(body, text)
	})
	return err
}

// identityConfig is the runner's identity as configured (a bare one with the
// built-in defaults when the config does not name it).
func (r *Runner) identityConfig() config.Identity {
	if r.Config != nil {
		if c := r.Config.IdentityByName(r.Identity.Name()); c != nil {
			return *c
		}
	}
	return config.Identity{Name: r.Identity.Name(), Kind: r.Identity.Kind(), Login: r.Identity.Login()}
}

// editReview reads review reviewID over REST, checks that the runner's
// identity posted it (GitHub lets only a review's author edit it) and puts
// back the body edit returns, when edit reports a change.
func (r *Runner) editReview(ctx context.Context, owner, repo string, number int, reviewID int64, edit func(body string) (string, bool)) (bool, error) {
	rv, err := r.GitHub.ReviewREST(ctx, owner, repo, number, reviewID)
	if err != nil {
		return false, err
	}
	if login := r.Identity.Login(); !github.SameAccount(rv.UserLogin, login) {
		return false, fmt.Errorf("pipeline: review %d was posted by %q, not %q", reviewID, rv.UserLogin, login)
	}
	body, changed := edit(rv.Body)
	if !changed {
		return false, nil
	}
	return true, r.GitHub.UpdateReviewBody(ctx, owner, repo, number, reviewID, body)
}

// appendNote adds text as body's last paragraph, or as the last one above
// the footer magnum appended (from footerMarker on); false when text is
// there already.
func appendNote(body, text string) (string, bool) {
	const space = "\r\n\t "
	head, tail := strings.TrimRight(body, space), ""
	if i := strings.LastIndex(head, footerMarker); i >= 0 {
		head, tail = strings.TrimRight(head[:i], space), head[i:]
	}
	if strings.HasSuffix(head, text) {
		return body, false
	}
	out := text
	if head != "" {
		out = head + "\n\n" + text
	}
	if tail != "" {
		out += "\n\n" + tail
	}
	return out, true
}
