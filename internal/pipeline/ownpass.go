package pipeline

// The judge's own pass (RoundInput.OwnPass, [pipeline] judge_own_pass =
// "parallel"; DECISIONS "The judge does its own pass while the reviewers
// work"): the judge's own full pass (read the code and the PR, run checks,
// find and prove its findings) needs no candidate report, so it starts with
// the reviewers instead of after them. In the first stage the judge gets
// its own-pass prompt (config.PromptOwnPass), a run of kind
// store.RunOwnPass, and writes agents.OwnFindingsFile; it posts nothing. Once
// every stage ended and so did that turn, whichever came last, the judge
// gets its usual prompt in the candidates phase (agents.PhaseCandidates),
// which starts from that file. The round's judge run, the one the review's
// marker names, is created before the own pass's, so the marker is never
// the own pass's and the observer's newest run of the judge's session is
// the one in flight.
//
// The own pass is part of the reviewers' stage: a push cuts it short and
// the restart prompts it again on the new head (restart.go); the checkout
// check after a stage covers it while it works; a model limit continues it
// on a fallback model in another own_pass run; a usage limit or any other
// end without its file is no failure (the candidates prompt asks for the
// pass then, and pauses the round when the judge's kind is limited); the
// round's cancellation leaves its run in flight like a reviewer's; and the
// engine's crash recovery counts it with the reviewers, never as the
// judge's turn (engine.latestJudge).

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// ownPassDue reports whether the round prompts its judge for its own pass
// with the reviewers: the input asks for it, the judge has an own-pass
// prompt, and a reviewer runs (runs: the reviewers' runs; none when every
// reviewer was dropped or logged out). A judge alone gets one prompt: a
// continued turn, a delta check, a re-review of the same head, a round of
// the judge alone.
func (rd *round) ownPassDue(runs map[string]*store.Run) bool {
	return rd.in.OwnPass && rd.in.Kind != KindContinue && rd.deltaCheck() == nil && !rd.sameHead() &&
		rd.judge.PromptFile(config.PromptOwnPass) != "" && len(runs) > 0
}

// ownPass is the judge's own pass in flight (startOwnPass).
type ownPass struct {
	done chan struct{}
	rep  RoleReport // how it ended; read once done is closed
}

// finished reports whether the own pass ended.
func (op *ownPass) finished() bool {
	select {
	case <-op.done:
		return true
	default:
		return false
	}
}

// startOwnPass prompts the judge for its own pass on run in a goroutine of
// its own, under ctx (the stages' context, which a push cancels). marker
// is the run id the round's review will carry, which the prompt quotes.
func (rd *round) startOwnPass(ctx context.Context, run store.Run, marker string) *ownPass {
	op := &ownPass{done: make(chan struct{})}
	go func() {
		defer close(op.done)
		op.rep = rd.ownPassTurn(ctx, run, marker)
	}()
	return op
}

// ownPassTurn prompts the own pass and waits for its turn to end; its run
// ends final, except when ctx was cancelled (a push: the restart settles
// it; the round's end: it stays in flight, observed, as a reviewer's).
func (rd *round) ownPassTurn(ctx context.Context, run store.Run, marker string) RoleReport {
	judge := rd.judge
	path := filepath.Join(rd.dir, agents.OwnFindingsFile) // the run's report path (NewRun)
	rep := rd.newReport(judge, run.ID)
	fail := func(status, detail string) RoleReport {
		rd.finishRun(ctx, run.ID, store.RunFailed, status, detail)
		rep.Status, rep.Detail = status, execx.Redact(detail)
		rd.ownPassEnded(ctx, rep)
		return rep
	}
	if err := setAside(path); err != nil {
		return fail(ReportFailed, err.Error())
	}
	jd := rd.judgeData(run, marker)
	jd.Reports, jd.Mode, jd.Phase, jd.OwnFindings = nil, rd.in.Kind, agents.PhaseOwnPass, path
	if from := rd.restartedFrom; from != "" && from != rd.in.TargetSHA {
		jd.RestartedFrom = from
	}
	rd.addThreads(ctx, &jd)
	rd.addRelated(ctx, &jd)
	text, err := rd.r.Agents.RolePrompt(judge, config.PromptOwnPass, jd)
	if err != nil {
		return fail(ReportFailed, err.Error())
	}
	rd.event(ctx, "info", "round.own_pass", fmt.Sprintf("prompting %s for its own pass while the reviewers work (run %s, %s)", judge.Name, run.ID, rd.in.Kind),
		map[string]any{"run": run.ID, "role": judge.Name, "marker": marker, "file": path})
	rd.mu.Lock()
	rd.ownFindings = path
	rd.mu.Unlock()
	anchor := rd.in.TargetSHA
	if strings.Contains(text, path) {
		anchor = path
	}
	t := rd.submitAndWait(ctx, run, text, rd.timeout(judge), "", nil)
	if t.unsent {
		rd.mu.Lock()
		rd.ownFindings = "" // the judge never got it: the candidates prompt is the one prompt
		rd.mu.Unlock()
	}
	t, anchor = rd.reviewerFallbacks(ctx, judge, t, path, anchor) // continuations of kind own_pass (modelFallback)
	if t.run.ID != "" {
		run = t.run
	}
	return rd.finishOwnPass(ctx, run, path, anchor, t)
}

// finishOwnPass turns the own pass's turn into its report and final run
// state: verified with ok when it wrote its file, else failed with why (a
// health kind its pane shows, missing, timeout, ...). A timed-out turn is
// interrupted and waited for, so the candidates prompt finds the judge idle.
func (rd *round) finishOwnPass(ctx context.Context, run store.Run, path, anchor string, t turn) RoleReport {
	rep := rd.newReport(rd.judge, run.ID)
	detail := func(err error) string {
		if err == nil {
			return ""
		}
		return execx.Redact(err.Error())
	}
	switch t.kind {
	case waitCancelled:
		rep.Status = ReportCancelled
		return rep
	case waitRefused:
		rep.Status, rep.Detail = refusedStatus(t.err), detail(t.err) // run already abandoned
		rd.ownPassEnded(ctx, rep)
		return rep
	case waitFailed:
		rep.Status, rep.Detail = ReportFailed, detail(t.err)
	case waitTimeout:
		rd.stopJudge(ctx, run)
		rep.Status, rep.Detail = ReportTimeout, detail(t.err)
	case waitLost:
		rep.Status, rep.Detail = ReportLost, detail(t.err)
	default: // waitEnded
		rep = rd.checkReport(ctx, rd.judge, run, path, anchor)
	}
	if r, ok := rd.reportFile(rd.judge, run, path); ok && rep.Status != ReportOK {
		rep.Path = r.Path // a file the turn wrote before it was cut: the candidates phase reads it
	}
	if rep.Status == ReportOK {
		rd.finishRun(ctx, run.ID, store.RunVerified, rep.Status, "")
	} else {
		rd.finishRun(ctx, run.ID, store.RunFailed, rep.Status, cmp.Or(rep.Detail, rep.Status))
	}
	rd.ownPassEnded(ctx, rep)
	return rep
}

// ownPassEnded records how the own pass ended (RoundResult.OwnPass and a
// round.own_pass event).
func (rd *round) ownPassEnded(ctx context.Context, rep RoleReport) {
	rd.mu.Lock()
	r := rep
	rd.res.OwnPass = &r
	rd.mu.Unlock()
	level, msg := "info", fmt.Sprintf("%s's own pass: %s", rd.judge.Name, rep.Status)
	switch {
	case rep.Status == ReportOK:
		msg += " (" + agents.OwnFindingsFile + ")"
	default:
		level = "warn"
		if rep.Detail != "" {
			msg += ": " + rep.Detail
		}
		msg += "; the candidates prompt asks for the pass"
		if rep.Path != "" {
			msg = fmt.Sprintf("%s's own pass: %s, %s written", rd.judge.Name, rep.Status, agents.OwnFindingsFile)
		}
	}
	rd.event(ctx, level, "round.own_pass", msg, map[string]any{"run": rep.RunID, "role": rd.judge.Name, "status": rep.Status})
}

// ownPassPhase fills a candidates-phase judge prompt when the round's own
// pass reached the judge: the phase, its file and whether it is missing
// (absent or empty), so the prompt asks for the pass then.
func (rd *round) ownPassPhase(jd *agents.JudgeData) {
	rd.mu.Lock()
	path := rd.ownFindings
	rd.mu.Unlock()
	if path == "" {
		return
	}
	jd.Mode, jd.Phase, jd.OwnFindings = rd.in.Kind, agents.PhaseCandidates, path
	st, err := os.Stat(path)
	jd.OwnFindingsMissing = err != nil || st.Size() == 0
}
