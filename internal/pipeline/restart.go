package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
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
// times per round: the turns in flight are all interrupted first, then
// waited for and their runs abandoned (cut, ReportHeadMoved), RoundInput.Switch
// checks the new head out, the round's target and report directory follow
// it, and every role that runs gets a new run on the same session, a session
// reviewer with its restart prompt (config.PromptRestart) when it has one.
// After the last restart the round finishes on the head it has. A refusal
// wins over a push: the round ends refused (cutCause).
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
// last. It first writes the head's history.json, which the prompts name
// (writeHistory), and notes the checkout, which each stage's roles and the
// own pass must leave as they found it (checkTree). marker is the run id
// the round's review carries, which the own-pass prompt quotes.
//
// Stages cut short end the round unless a push cut them (cutCause: a
// refusal wins over a push that came first); its turns still in flight are
// stopped then (endStages), and after a push the restart settles them.
func (rd *round) runStages(ctx context.Context, runs map[string]*store.Run, own *store.Run, marker string) (head string, err error) {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rd.writeHistory(ctx)
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
		op = rd.startOwnPass(sctx, cancel, *own, marker)
		covered := false // a check ran after the own pass ended: it left nothing unchecked
		also = func() []string {
			if covered {
				return nil
			}
			covered = op.finished()
			return []string{rd.judge.Name}
		}
	}
	var serr error // the last stage's: ctx's, or a checkout that could not be restored
	for _, stage := range rd.stages {
		if head := rd.movedHead(ctx); head != "" {
			cancel(&headMovedError{sha: head})
			break
		}
		if serr = rd.runStage(sctx, cancel, stage, runs, also); serr != nil || sctx.Err() != nil {
			break
		}
	}
	if serr != nil && sctx.Err() == nil {
		cancel(serr) // the own pass stops with the stages: nothing may run on a modified checkout
	}
	if op != nil {
		<-op.done // a cut ends its turn at once; else its waits notice a push meanwhile
	}
	if ctx.Err() != nil {
		return "", ctx.Err() // the round's cancellation leaves the turns in flight, observed
	}
	cause := rd.cutCause(sctx, op)
	if moved, ok := cause.(*headMovedError); ok {
		if serr == nil || errors.Is(serr, context.Canceled) {
			return moved.sha, nil // the restart settles the turns the push cut short
		}
		cause = serr // a checkout that could not be restored is no place to restart in
	}
	if cause != nil {
		rd.endStages(ctx, runs, op, own, cause)
		return "", cause
	}
	if op != nil {
		if names := also(); len(names) > 0 {
			if err := rd.checkTree(ctx, names); err != nil {
				return "", err
			}
		}
	}
	return rd.movedHead(ctx), nil
}

// cutCause is why the stages ended early (nil = they did not): a refusal a
// reviewer's or the ended own pass's (op) report records, whatever cancelled
// the stages first, else the cause their context was cancelled with. A push
// cancels them before a turn it raced ends refused (refuseStages then
// changes nothing), and a restart would prompt the refused provider again
// on the new head. The first refusal is the one named.
func (rd *round) cutCause(sctx context.Context, op *ownPass) error {
	cause := context.Cause(sctx)
	if _, ok := cause.(*refusedError); ok {
		return cause
	}
	refused := string(agents.HealthRefused)
	for _, role := range rd.running() {
		if rep, ok := rd.report(role); ok && rep.Status == refused {
			return &refusedError{reportRefusal(rep)}
		}
	}
	if op != nil && op.finished() && op.rep.Status == refused {
		return &refusedError{reportRefusal(op.rep)}
	}
	return cause
}

// endStages stops the turns still in flight when the stages end early on
// cause (a refusal, the judge's session gone, a checkout that could not be
// restored), so that none goes on working for a round that is over: the
// reviewers' and the judge's own pass's (op, on run own, when a cut ended
// its turn: ReportCancelled), as one cut (cut). On a refusal the turns of
// the refused role's kind come first: each moment more of them on the
// flagged content is an account risk.
func (rd *round) endStages(ctx context.Context, runs map[string]*store.Run, op *ownPass, own *store.Run, cause error) {
	var ownCut *store.Run
	if op != nil && op.rep.Status == ReportCancelled {
		ownCut = own
	}
	turns := rd.cutTurns(runs, ownCut)
	if ref, ok := cause.(*refusedError); ok {
		later := func(t cutTurn) int {
			if t.role.AgentKind() == ref.r.Kind {
				return 0
			}
			return 1
		}
		slices.SortStableFunc(turns, func(a, b cutTurn) int { return cmp.Compare(later(a), later(b)) })
	}
	rd.cut(ctx, turns, cutBy{what: "the round's end", status: ReportCancelled, why: cause.Error(), stop: stopBackgroundText})
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
// settled (cut), the judge's own pass (own, when not nil) with the
// reviewers', the checkout switches to head (RoundInput.Switch), the
// round's target, merge base and report directory follow it, and every role
// that runs gets a new run, the judge too, and the own pass a new one after
// it.
func (rd *round) restart(ctx context.Context, head string, runs map[string]*store.Run, judgeRun, own *store.Run) (*store.Run, *store.Run, error) {
	from := rd.in.TargetSHA
	turns := rd.cutTurns(runs, own)
	rd.mu.Lock()
	rd.cont = nil
	rd.ownFindings = ""
	rd.mu.Unlock()
	ran := make([]string, 0, len(turns))
	for _, t := range turns {
		ran = append(ran, t.role.Name)
	}
	// The sessions are prompted again on the new head: the judge's own pass
	// is interrupted with one ctrl+c, which keeps its session, and a
	// reviewer's background work is stopped with headMovedStopText.
	cut := rd.cut(ctx, turns, cutBy{what: "the restart", status: ReportHeadMoved,
		why: "the PR head moved to " + textx.ShortSHA(head), stop: headMovedStopText, keep: true})
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

// cutTurn is a turn the stages may have cut short: role's run, the latest
// of its turn (a continuation on a fallback model), and whether it is the
// judge's own pass.
type cutTurn struct {
	role config.Role
	run  store.Run
	own  bool
}

// name is how round.restarted names the turn.
func (t cutTurn) name() string {
	if t.own {
		return t.role.Name + " (own pass)"
	}
	return t.role.Name
}

// cutTurns lists the stages' turns in stage order: every reviewer's that
// runs (runs) and, when own is not nil, the judge's own pass's (ownRun).
func (rd *round) cutTurns(runs map[string]*store.Run, own *store.Run) []cutTurn {
	rd.mu.Lock()
	cont := maps.Clone(rd.cont)
	rd.mu.Unlock()
	var out []cutTurn
	for _, role := range rd.running() {
		run := runs[role.Name]
		if run == nil {
			continue
		}
		if c, ok := cont[role.Name]; ok {
			run = &c // the turn in flight is the continuation on a fallback model
		}
		out = append(out, cutTurn{role: role, run: *run})
	}
	if own != nil {
		out = append(out, cutTurn{role: rd.judge, run: rd.ownRun(*own), own: true})
	}
	return out
}

// cutBy is what cut the turns cut ends short: a push the round restarts on
// (restart) or the round's end (endStages).
type cutBy struct {
	what   string // for the warning: "the restart", "the round's end"
	status string // the cut runs' outcome
	why    string // and their error
	stop   string // the message that stops a reviewer's background work (stopBackground)
	keep   bool   // the sessions are prompted again: interrupt keeps the judge's
}

// cut ends the runs of turns that the stages cut short (by c) in two
// phases, so that no turn goes on working while cut waits for another.
// First every turn still in flight gets its interrupt keys (interrupt: esc
// to an agent, ctrl+c to a shell role's pane, and to the judge's own pass
// one ctrl+c, or two, which quit Codex, when the round is over), all within
// seconds of the cut. Then each of them is settled (settle), shell roles
// first (a command that goes on gets its next ctrl+c without waiting for an
// agent's waits), and every cut run ended: abandoned with c.status and
// c.why, an own pass the round's end cut failed. A finished run keeps its
// state. It returns the names of the turns it ended.
func (rd *round) cut(ctx context.Context, turns []cutTurn, c cutBy) []string {
	type cutting struct {
		cutTurn
		cur         store.Run
		interrupted bool
	}
	var cuts []cutting
	names := []string{}
	for _, t := range turns {
		cur, err := rd.r.Store.RunByID(context.WithoutCancel(ctx), t.run.ID)
		if err != nil {
			rd.logf("pipeline: %s: run %s: %v", c.what, t.run.ID, err)
			continue
		}
		switch cur.State {
		case store.RunSubmitted, store.RunWorking:
			rd.interrupt(ctx, t.role, cur, c.keep)
			cuts = append(cuts, cutting{t, cur, true})
		case store.RunPending, store.RunEnded:
			cuts = append(cuts, cutting{t, cur, false})
		default:
			continue
		}
		names = append(names, t.name())
	}
	agent := func(t cutting) int {
		if t.role.IsShell() {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(cuts, func(a, b cutting) int { return cmp.Compare(agent(a), agent(b)) })
	for _, t := range cuts {
		if t.interrupted {
			rd.settle(ctx, t.role, t.cur, c)
		}
		to, why := store.RunAbandoned, c.why
		if t.own && !c.keep && t.cur.State != store.RunPending {
			to, why = store.RunFailed, "the round ended before the own pass did: "+c.why
		}
		rd.finishRun(ctx, t.cur.ID, to, c.status, why)
	}
	return names
}

// settle waits for role's turn on run, which cut (by c) interrupted, to
// stop: an agent until it is seen idle (waitIdle), then told to stop the
// background work it left running (stopBackground, c.stop); a shell role's
// command until it stopped (stopShell). A warning says when it did not, and
// what the agent was told.
func (rd *round) settle(ctx context.Context, role config.Role, run store.Run, c cutBy) {
	msg := ""
	switch {
	case role.IsShell():
		if !rd.stopShell(ctx, run) {
			msg = fmt.Sprintf("%s still runs %s after %d ctrl+c for %s", role.Name, shellStopWait, shellStopPresses, c.what)
		}
	case !rd.waitIdle(ctx, role, run):
		msg = fmt.Sprintf("%s still works %s after it was interrupted for %s", role.Name, InterruptWait, c.what)
	}
	if bg := rd.stopBackground(ctx, role, run, c.stop, false); bg != "" {
		msg = cmp.Or(msg, fmt.Sprintf("interrupted %s for %s", role.Name, c.what)) + "; " + bg
	}
	if msg != "" {
		rd.warn(ctx, "%s", msg)
	}
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
