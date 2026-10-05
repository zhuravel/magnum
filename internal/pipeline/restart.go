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
)

// A push the poller records while the reviewers run (before the judge is
// prompted) restarts them in place on the new head, at most
// RoundInput.MaxRestarts times per round: the turns in flight are
// interrupted and their runs abandoned (ReportHeadMoved), RoundInput.Switch
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

func (e *headMovedError) Error() string { return "pipeline: the PR head moved to " + short(e.sha) }

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
			rd.logf("pipeline: %s: head %s is an ancestor of %s: no push", rd.subject, short(head), short(target))
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
// its target and report path).
func (rd *round) reviewers(ctx context.Context, runs map[string]*store.Run, judgeRun *store.Run) (*store.Run, error) {
	for {
		head, err := rd.runStages(ctx, runs)
		if err != nil || head == "" {
			return judgeRun, err
		}
		if judgeRun, err = rd.restart(ctx, head, runs, judgeRun); err != nil {
			return nil, err
		}
	}
}

// runStages runs the stages in order under a context a push cancels while
// restarts remain. head is the PR's new head when the round must restart: a
// push noticed while a stage ran, or by the check before each stage and
// after the last.
func (rd *round) runStages(ctx context.Context, runs map[string]*store.Run) (head string, err error) {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
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
	for _, stage := range rd.stages {
		if head := rd.movedHead(ctx); head != "" {
			return head, nil
		}
		err := rd.runStage(sctx, stage, runs)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if moved, ok := errors.AsType[*headMovedError](context.Cause(sctx)); ok {
			if err != nil && !errors.Is(err, context.Canceled) {
				return "", err // a checkout that could not be restored is no place to restart in
			}
			return moved.sha, nil
		}
		if err != nil {
			return "", err
		}
	}
	return rd.movedHead(ctx), nil
}

// restart starts the reviewers over on head: the turns cut short are
// settled (settleCut), the checkout switches to head (RoundInput.Switch), the
// round's target, merge base and report directory follow it, and every role
// that runs gets a new run, the judge too.
func (rd *round) restart(ctx context.Context, head string, runs map[string]*store.Run, judgeRun *store.Run) (*store.Run, error) {
	from := rd.in.TargetSHA
	cut := []string{}
	rd.mu.Lock()
	cont := rd.cont
	rd.cont = nil
	rd.mu.Unlock()
	for _, role := range rd.running() {
		run := runs[role.Name]
		if c, ok := cont[role.Name]; ok && run != nil {
			run = &c // the turn in flight is the continuation on a fallback model
		}
		if run != nil && rd.settleCut(ctx, role, *run, head) {
			cut = append(cut, role.Name)
		}
	}
	rd.finishRun(ctx, judgeRun.ID, store.RunAbandoned, ReportHeadMoved, "the PR head moved to "+short(head))

	sw, err := rd.in.Switch(ctx, head)
	if err != nil {
		return nil, fmt.Errorf("pipeline: restart on %s: %w", short(head), err)
	}
	target := cmp.Or(sw.TargetSHA, head)
	rd.restarts++
	rd.restartedFrom = from
	rd.in.TargetSHA, rd.pr.HeadSHA = target, target
	rd.unverified = "" // an earlier review of the old head is no review of this one
	rd.in.BaseSHA = cmp.Or(sw.BaseSHA, rd.in.BaseSHA)
	rd.in.ForcePushed = sw.ForcePushed
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
		return nil, fmt.Errorf("pipeline: report dir: %w", err)
	}
	if err := rd.setAsideStale(); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("the PR head moved from %s to %s before the judge was prompted: restart %d of %d",
		short(from), short(target), rd.restarts, rd.in.MaxRestarts)
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
			return nil, err
		}
	}
	return rd.newRun(ctx, rd.judge, rd.in.Kind)
}

// settleCut ends role's run when the push cut it short: a turn still in
// flight is interrupted, and an agent waited for until idle (InterruptWait;
// a git-diff role was interrupted and its tree restored by runPatchRole),
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
		if role.Capture != config.CaptureGitDiff {
			rd.interrupt(ctx, role, cur)
			if !rd.waitIdle(ctx, role, cur) {
				rd.warn(ctx, "%s still works %s after it was interrupted for the restart", role.Name, InterruptWait)
			}
		}
	case store.RunPending, store.RunEnded:
	default:
		return false
	}
	rd.finishRun(ctx, cur.ID, store.RunAbandoned, ReportHeadMoved, "the PR head moved to "+short(head))
	return true
}

// AppendToReview adds text as the last paragraph of review reviewID, which
// the runner's identity posted (GitHub lets only a review's author edit it):
// it reads the body over REST and puts it back with text appended. A body
// that already ends with text is left alone.
func (r *Runner) AppendToReview(ctx context.Context, owner, repo string, number int, reviewID int64, text string) error {
	if r.GitHub == nil || r.Identity == nil {
		return fmt.Errorf("%w: AppendToReview needs GitHub and Identity", ErrInvalid)
	}
	text = strings.TrimSpace(text)
	rv, err := r.GitHub.ReviewREST(ctx, owner, repo, number, reviewID)
	if err != nil {
		return err
	}
	if login := r.Identity.Login(); !github.SameAccount(rv.UserLogin, login) {
		return fmt.Errorf("pipeline: review %d was posted by %q, not %q", reviewID, rv.UserLogin, login)
	}
	body := strings.TrimRight(rv.Body, "\n\t ")
	if strings.HasSuffix(body, text) {
		return nil
	}
	return r.GitHub.UpdateReviewBody(ctx, owner, repo, number, reviewID, body+"\n\n"+text)
}
