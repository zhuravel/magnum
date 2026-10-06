package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// transcriptMax: only a captured stdout this short is classified when its
	// command failed (a tool may print its login or usage error there).
	transcriptMax  = 3000
	transcriptTail = 15
)

// exitError is a shell role's command that finished with an exit status
// outside the role's ok_status.
type exitError struct {
	role   string
	status int
}

func (e *exitError) Error() string {
	return fmt.Sprintf("%s exited with status %d", e.role, e.status)
}

// runRole runs one non-judge role and returns its report; the run ends
// final.
func (rd *round) runRole(ctx context.Context, role config.Role, run store.Run) RoleReport {
	path := rd.reportPath(run, role)
	t, anchor, early := rd.turn(ctx, role, run, path)
	if early != nil {
		return *early
	}
	if t.run.ID != "" {
		run = t.run // a continuation on a fallback model
	}
	return rd.finishReviewer(ctx, role, run, path, anchor, t)
}

// turn prompts role (an agent session) or runs its command (a shell role)
// and waits for the turn to end. A session turn that ended on its model's
// own limit continues on the kind's fallback models (reviewerFallbacks); the
// returned turn is then the last continuation's. anchor locates the last
// prompt in the pane text. early is set when the role ended without a turn
// (a prompt that could not be rendered, no pane, a busy shell,
// cancellation); its run is final then.
func (rd *round) turn(ctx context.Context, role config.Role, run store.Run, path string) (t turn, anchor string, early *RoleReport) {
	if err := setAside(path); err != nil {
		rd.finishRun(ctx, run.ID, store.RunFailed, ReportFailed, err.Error())
		rep := rd.newReport(role, run.ID)
		rep.Status, rep.Detail = ReportFailed, execx.Redact(err.Error())
		return turn{}, "", &rep
	}
	if role.IsShell() {
		return rd.shellTurn(ctx, role, run, path)
	}
	kind, data := rd.roleData(role, run.ID, path)
	text, err := rd.r.Agents.RolePrompt(role, kind, data)
	if err != nil {
		rd.finishRun(ctx, run.ID, store.RunFailed, ReportFailed, err.Error())
		rep := rd.newReport(role, run.ID)
		rep.Status, rep.Detail = ReportFailed, execx.Redact(err.Error())
		return turn{}, "", &rep
	}
	rd.setMark(role, run.ID, text)
	rd.event(ctx, "info", "round.reviewer", fmt.Sprintf("prompting %s (run %s, %s)", role.Name, run.ID, kind), map[string]any{"run": run.ID, "role": run.Role})
	anchor = rd.in.TargetSHA
	if strings.Contains(text, path) {
		anchor = path
	}
	t, anchor = rd.reviewerFallbacks(ctx, role, rd.submitAndWait(ctx, run, text, rd.timeout(role), "", nil), path, anchor)
	if t.kind == waitTimeout {
		t = rd.timeUp(ctx, role, t, path)
	}
	return t, anchor, nil
}

// timeUpText is the last call to a reviewer whose time ran out: its budget,
// its report file and the marker its report starts with (timeUpMark, ""
// when its prompt named none).
const timeUpText = "Time is up: this review had %s. Stop every background task you started with TaskStop and start nothing new. " +
	"Write the report to %s now with what you found so far%s, list the checks you stopped or did not run as pending, and end your turn."

// timeUpMark names the report's first line in timeUpText.
const timeUpMark = " (its first line: `%s`)"

// stopBackgroundText is the one message an interrupted reviewer that left
// background work running gets (stopReviewer).
const stopBackgroundText = "This review is over. Stop every background task you started with TaskStop and do nothing else."

// timeUp gives a session reviewer whose turn t ran out of time one last
// call (the live case: a spec run in the background, the report not
// written): Agents.TimeUp asks its agent, within the same run, to stop its
// background tasks (TaskStop) and write its report now, and the run is
// waited for TimeUpGrace more. A turn that then ends is read as any ended
// turn (its report, else missing); one that does not is the timeout it
// was. A time-up that cannot be sent leaves the timeout as it is.
func (rd *round) timeUp(ctx context.Context, role config.Role, t turn, path string) turn {
	if role.IsShell() || ctx.Err() != nil {
		return t
	}
	budget := durationWords(rd.timeout(role))
	mark := ""
	if id := rd.markOf(role); id != "" {
		mark = fmt.Sprintf(timeUpMark, agents.ReportMarker(id))
	}
	if err := rd.r.Agents.TimeUp(ctx, t.run, fmt.Sprintf(timeUpText, budget, path, mark)); err != nil {
		rd.warn(ctx, "%s ran out of its %s and could not be asked for its report: %v", role.Name, budget, err)
		return t
	}
	rd.event(ctx, "warn", "round.time_up", fmt.Sprintf("%s ran out of its %s: asked it to write its report now, waiting %s more (run %s)",
		role.Name, budget, durationWords(TimeUpGrace), t.run.ID), map[string]any{"run": t.run.ID, "role": role.Name, "grace": TimeUpGrace.String()})
	next := rd.wait(ctx, t.run.ID, TimeUpGrace, "", nil)
	if next.kind == waitTimeout {
		next.err = fmt.Errorf("pipeline: %s run %s: no report after its %s and %s more", role.Name, t.run.ID, budget, durationWords(TimeUpGrace))
	}
	return next
}

// durationWords spells a duration for a prompt or a message: "40 minutes",
// "2 hours", "1 minute"; anything else as Go writes it.
func durationWords(d time.Duration) string {
	unit := func(n int64, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	switch {
	case d > 0 && d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d > 0 && d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	case d > 0 && d%time.Second == 0 && d < time.Minute:
		return unit(int64(d/time.Second), "second")
	}
	return d.String()
}

// setAside renames a report file present before its role is prompted to
// <name>.prev: report paths are per head, not per run, so a file there now
// was written by an earlier run (a reviewer that kept working after its run
// ended) and must never pass for this run's report. The round's start set
// aside what was there then (setAsideStale); this catches what arrived
// since.
func setAside(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = os.Rename(path, path+".prev")
	}
	if err != nil {
		return fmt.Errorf("pipeline: set aside the stale %s: %w", filepath.Base(path), err)
	}
	return nil
}

// roleData fills a session role's prompt for run runID: the rereview prompt
// (and mode) for a rereview round after an earlier review, else the initial
// one; after a restart on a newer head the role's restart prompt when it
// has one. The effort is the role's for the round (config.Role.EffortFor).
func (rd *round) roleData(role config.Role, runID, path string) (string, agents.RoleData) {
	in := rd.in
	prev := rd.previousHead()
	rereview := rd.reviewersRereview()
	effort := role.EffortFor(rereview)
	d := agents.RoleData{
		URL: rd.pr.URL, Owner: rd.owner, Repo: rd.name, Number: in.PR.Number,
		HeadSHA: in.TargetSHA, BaseSHA: in.BaseSHA, BaseRef: rd.baseRef(), ReportPath: path,
		Model: rd.r.Config.RoleModel(role), Effort: effort, EffortInPrompt: rd.effortInPrompt(role, effort),
		Mode: agents.ModeInitial, NotesPath: in.NotesPath, HistoryFile: rd.historyFile, Blind: in.Blind, PostMerge: in.PostMerge,
		Budget: durationWords(rd.timeout(role)), RunID: runID,
	}
	kind := config.PromptInitial
	if rereview {
		kind, d.Mode, d.PreviousHeadSHA, d.ForcePushed, d.BaseMerged = config.PromptRereview, agents.ModeRereview, prev, in.ForcePushed, in.BaseMerged
		if !in.Since.IsZero() {
			d.Since = in.Since.UTC().Format(time.RFC3339)
		}
	}
	if from := rd.restartedFrom; from != "" && from != in.TargetSHA && role.PromptFile(config.PromptRestart) != "" {
		kind, d.Mode, d.RestartedFrom = config.PromptRestart, agents.ModeRestart, from
	}
	return kind, d
}

// effortInPrompt reports whether a prompt must ask role's agent for effort
// in words: its kind sets the effort only through launch args (a running
// session cannot switch) and effort is not the role's launch effort.
func (rd *round) effortInPrompt(role config.Role, effort string) bool {
	if effort == "" || effort == role.Effort || rd.r.Config == nil {
		return false
	}
	k, ok := rd.r.Config.KindSpec(role.AgentKind())
	return ok && len(k.Effort) > 0
}

// shellTurn types a shell role's line into its pane and waits for the done
// marker (or the role's timeout).
func (rd *round) shellTurn(ctx context.Context, role config.Role, run store.Run, path string) (turn, string, *RoleReport) {
	end := func(to, status, detail string) (turn, string, *RoleReport) {
		rep := rd.newReport(role, run.ID)
		rep.Status, rep.Detail = status, detail
		if status == ReportCancelled {
			rep.Detail = ""
		}
		rd.finishRun(ctx, run.ID, to, status, detail)
		return turn{}, "", &rep
	}
	sess, err := rd.r.Store.LiveSessionByPRRole(ctx, rd.pr.ID, role.Name)
	if err != nil || store.Deref(sess.HerdrPaneID) == "" {
		if ctx.Err() != nil {
			rep := rd.newReport(role, run.ID)
			rep.Status = ReportCancelled
			return turn{}, "", &rep
		}
		return end(store.RunAbandoned, ReportNoSession, "no live "+role.Name+" pane")
	}
	pane := *sess.HerdrPaneID
	marker := agents.DoneMarker(run.ID)
	anchor := agents.CommandAnchor(marker)
	line, err := rd.r.Agents.ShellLine(ctx, rd.pr.ID, role, agents.ShellData{
		Title:      agents.TaggedTitle(rd.r.AgentTag, rd.name, rd.pr.Number, agents.Role(role.Name)),
		ReportPath: path, Marker: marker, RunID: run.ID,
		BaseRef: rd.baseRef(), BaseSHA: rd.in.BaseSHA, HeadSHA: rd.in.TargetSHA, URL: rd.pr.URL,
		Model: rd.r.Config.RoleModel(role), Effort: role.EffortFor(rd.reviewersRereview()),
		Checkout: rd.in.SlotPath,
	})
	if err != nil {
		return end(store.RunFailed, ReportFailed, execx.Redact(err.Error()))
	}
	rd.setMark(role, run.ID, line)
	if err := rd.r.Store.TransitionRun(ctx, run.ID, []string{store.RunPending}, store.RunSubmitted, func(u *store.RunUpdate) {
		u.Set("session_id", sess.ID)
		u.Set("prompt_text", execx.Redact(line))
		u.Set("submitted_at", rd.r.now())
	}); err != nil {
		if ctx.Err() != nil {
			rep := rd.newReport(role, run.ID)
			rep.Status = ReportCancelled
			return turn{}, "", &rep
		}
		return end(store.RunFailed, ReportFailed, execx.Redact(err.Error()))
	}
	run.SessionID = &sess.ID
	rd.event(ctx, "info", "round.reviewer", fmt.Sprintf("running %s in pane %s (run %s)", role.Name, pane, run.ID), map[string]any{"run": run.ID, "role": run.Role})

	status, err := rd.r.Agents.RunShell(ctx, rd.pr, role, pane, line, marker, rd.timeout(role))
	switch {
	case err == nil:
		if terr := rd.r.Store.TransitionRun(context.WithoutCancel(ctx), run.ID, []string{store.RunSubmitted}, store.RunEnded,
			func(u *store.RunUpdate) { u.Set("ended_at", rd.r.now()) }); terr != nil {
			rd.logf("pipeline: end %s run %s: %v", role.Name, run.ID, terr)
		}
		if status != agents.ShellStatusUnknown && !role.StatusOK(status) {
			return turn{kind: waitFailed, run: run, err: &exitError{role: role.Name, status: status}}, anchor, nil
		}
		return turn{kind: waitEnded, run: run}, anchor, nil
	case pushCut(ctx):
		// The restart interrupts the command and settles its run.
		return turn{kind: waitCancelled, run: run, err: ctx.Err()}, anchor, nil
	case ctx.Err() != nil:
		// Nobody will read the command's output, and left running it would
		// hold the pane: the next round's command would find it busy.
		rd.interrupt(ctx, role, run)
		return end(store.RunFailed, ReportCancelled, "round cancelled")
	case errors.Is(err, agents.ErrBusy):
		return end(store.RunFailed, ReportBusy, execx.Redact(err.Error()))
	case errors.Is(err, agents.ErrHumanActive):
		return end(store.RunFailed, ReportHumanActive, execx.Redact(err.Error()))
	case errors.Is(err, agents.ErrTimeout):
		return turn{kind: waitTimeout, run: run, err: err}, anchor, nil
	}
	return turn{kind: waitFailed, run: run, err: err}, anchor, nil
}

// finishReviewer turns a role's turn into its report status and final run
// state. anchor locates this round's prompt in the pane text.
func (rd *round) finishReviewer(ctx context.Context, role config.Role, run store.Run, path, anchor string, t turn) RoleReport {
	rep := rd.newReport(role, run.ID)
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
		rep.Status, rep.Detail = refusedStatus(t.err), detail(t.err)
		return rep // run already abandoned
	case waitFailed:
		rep.Status, rep.Detail = ReportFailed, detail(t.err)
		if errors.Is(t.err, agents.ErrBlocked) {
			rep.Status = string(agents.HealthBlocked)
		}
		if exit, ok := errors.AsType[*exitError](t.err); ok {
			// The command failed: what it wrote is no report, but its output
			// may name why (a usage limit pauses the tool).
			if h := rd.failureHealth(ctx, role, run, path, anchor); h.Kind != agents.HealthOK {
				rep.Status, rep.Detail, rep.Health = string(h.Kind), h.Detail+" ("+exit.Error()+")", &h
			}
		} else if r, ok := rd.reportFile(role, run, path); ok {
			// A run failed externally after the report was written.
			rep = r
		}
		rd.finishReviewerRun(ctx, role, run, rep, "")
		return rep
	case waitTimeout, waitLost:
		stopped := ""
		if t.kind == waitTimeout {
			stopped = rd.stopReviewer(ctx, role, run)
			rep.Status = ReportTimeout
		} else {
			rep.Status = ReportLost
		}
		rep.Detail = detail(t.err)
		if r, ok := rd.reportFile(role, run, path); ok {
			r.Detail = "report present, but the role did not finish: " + rep.Status
			rep = r
		}
		rd.finishReviewerRun(ctx, role, run, rep, stopped)
		return rep
	}
	// waitEnded (or waitResult, unused for these roles).
	rep = rd.checkReport(ctx, role, run, path, anchor)
	stopped := ""
	if rep.Status == ReportMissing && role.IsAgent() {
		stopped = rd.stopReviewer(ctx, role, run)
	}
	rd.finishReviewerRun(ctx, role, run, rep, stopped)
	return rep
}

// stopReviewer interrupts a reviewer whose run magnum ends without its
// report (timed out, or ended without writing it), so it does not go on
// working in the checkout once the round moves on, and says what it did
// for the run's event. An interrupt does not stop the background work a
// claude agent started, whose notifications would resume it: while its
// transcript shows some running, the agent is told once, within the run,
// to stop it with TaskStop and do nothing else (stopBackgroundText), and
// the transcript is read again until none is left or StopGrace passed. The
// event counts what is left; magnum kills no process it did not start.
func (rd *round) stopReviewer(ctx context.Context, role config.Role, run store.Run) string {
	rd.interrupt(ctx, role, run)
	note := "interrupted " + role.Name
	bg := context.WithoutCancel(ctx)
	n, ok := rd.r.Agents.BackgroundTasks(bg, run)
	if !ok || n == 0 {
		return note
	}
	if !role.IsAgent() || ctx.Err() != nil {
		return note + fmt.Sprintf("; %s it started still %s", backgroundTasks(n), runVerb(n))
	}
	rd.waitIdle(ctx, role, run) // the message must not land in the interrupted turn
	if err := rd.r.Agents.TimeUp(bg, run, stopBackgroundText); err != nil {
		return note + fmt.Sprintf("; %s it started still %s (could not ask it to stop: %v)", backgroundTasks(n), runVerb(n), err)
	}
	note += fmt.Sprintf("; asked it to stop the %s it started", backgroundTasks(n))
	switch left, ok := rd.backgroundLeft(ctx, run); {
	case !ok:
		return note
	case left == 0:
		return note + ": it did"
	default:
		return note + fmt.Sprintf(": %d still %s after %s", left, runVerb(left), durationWords(StopGrace))
	}
}

// backgroundLeft reads the background tasks run's agent left running until
// none is left, StopGrace passed or ctx ends, and returns how many are (ok
// false: its transcript could not be read).
func (rd *round) backgroundLeft(ctx context.Context, run store.Run) (int, bool) {
	deadline := rd.r.now().Add(StopGrace)
	for {
		n, ok := rd.r.Agents.BackgroundTasks(context.WithoutCancel(ctx), run)
		if !ok || n == 0 || !rd.r.now().Before(deadline) || rd.r.sleep(ctx, rd.r.poll()) != nil {
			return n, ok
		}
	}
}

// backgroundTasks spells n background tasks: "1 background task", "2
// background tasks".
func backgroundTasks(n int) string {
	if n == 1 {
		return "1 background task"
	}
	return fmt.Sprintf("%d background tasks", n)
}

// runVerb is "runs" for one, else "run".
func runVerb(n int) string {
	if n == 1 {
		return "runs"
	}
	return "run"
}

// finishReviewerRun records role's run as its report says; stopped is what
// stopReviewer did, added to the warning ("" = nothing).
func (rd *round) finishReviewerRun(ctx context.Context, role config.Role, run store.Run, rep RoleReport, stopped string) {
	if rep.Status == ReportOK {
		rd.finishRun(ctx, run.ID, store.RunVerified, rep.Status, rep.Detail)
		return
	}
	msg := rep.Detail
	if msg == "" {
		msg = rep.Status
	}
	rd.finishRun(ctx, run.ID, store.RunFailed, rep.Status, msg)
	if stopped != "" {
		msg += "; " + stopped
	}
	rd.event(ctx, "warn", "round.reviewer", fmt.Sprintf("%s report %s: %s", role.Name, rep.Status, msg), map[string]any{"run": run.ID, "role": run.Role, "status": rep.Status})
}

// reportFile reports a present, non-empty report of this round's run as ok
// (see reportProblem). Its text is never classified: a review that
// discusses usage limits or 401s is still a review.
func (rd *round) reportFile(role config.Role, run store.Run, path string) (RoleReport, bool) {
	if rd.reportProblem(role, path) != "" {
		return RoleReport{}, false
	}
	rep := rd.newReport(role, run.ID)
	rep.Status, rep.Path = ReportOK, path
	return rep, true
}

// reportProblem says why the file at path is not role's report this round
// ("" = it is): absent, empty (nothing but its marker line), or stale. A
// role whose prompt or line named its run's marker (rd.marks) has a report
// only when the report's first line is that marker: one without it, or
// with another run's, was written by another run (a reviewer that kept
// working after its run ended, on the same head) and is stale.
func (rd *round) reportProblem(role config.Role, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "finished without writing " + filepath.Base(path)
	}
	got, rest := agents.ReportRun(b)
	if len(b) == 0 || (got != "" && len(bytes.TrimSpace(rest)) == 0) {
		return "empty " + filepath.Base(path)
	}
	switch want := rd.markOf(role); {
	case want == "" || got == want:
		return ""
	case got == "":
		return "stale report (no run marker)"
	default:
		return "stale report from run " + got
	}
}

// setMark records the run role's report must name: runID when text, the
// prompt or shell line that started it, names its marker, else none.
func (rd *round) setMark(role config.Role, runID, text string) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if !strings.Contains(text, agents.ReportMarker(runID)) {
		delete(rd.marks, role.Name)
		return
	}
	if rd.marks == nil {
		rd.marks = map[string]string{}
	}
	rd.marks[role.Name] = runID
}

// markOf is the run role's report must name ("" = any report counts).
func (rd *round) markOf(role config.Role) string {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.marks[role.Name]
}

// failureHealth classifies the output of a shell command that failed: its
// pane after this run's command line, else (capture "stdout") the tail of a
// short captured stdout, where a tool may print its login or usage error.
// Only login_required, usage_limit (a model limit too: a shell cannot switch
// models) and overloaded count.
func (rd *round) failureHealth(ctx context.Context, role config.Role, run store.Run, path, anchor string) agents.Health {
	pauses := func(h agents.Health) bool {
		return h.Kind == agents.HealthLoginRequired || h.Kind == agents.HealthUsageLimit || h.Kind == agents.HealthOverloaded
	}
	if h := asUsageLimit(rd.paneHealth(ctx, role, run, anchor)); pauses(h) {
		return h
	}
	if role.Capture == config.CaptureStdout {
		if st, err := os.Stat(path); err == nil && st.Size() <= transcriptMax {
			if b, err := os.ReadFile(path); err == nil {
				if h := asUsageLimit(rd.classify(role.AgentKind(), tailLines(string(b), transcriptTail))); pauses(h) {
					return h
				}
			}
		}
	}
	return agents.Health{Kind: agents.HealthOK}
}

// checkReport inspects a finished role: the report on disk, else the pane's
// health (a model limit still on screen had no fallback left: a usage
// limit).
func (rd *round) checkReport(ctx context.Context, role config.Role, run store.Run, path, anchor string) RoleReport {
	rep := rd.newReport(role, run.ID)
	problem := rd.reportProblem(role, path)
	if problem == "" {
		rep.Status, rep.Path = ReportOK, path
		return rep
	}
	rep.Status, rep.Detail = ReportMissing, problem
	if h := asUsageLimit(rd.paneHealth(ctx, role, run, anchor)); h.Kind != agents.HealthOK {
		rep.Status, rep.Detail, rep.Health = string(h.Kind), h.Detail, &h
	}
	return rep
}

// reportPath is the run's report file (NewRun fills it from the layout).
func (rd *round) reportPath(run store.Run, role config.Role) string {
	if p := store.Deref(run.ReportPath); p != "" {
		return p
	}
	return filepath.Join(rd.dir, role.ReportFile())
}

// timeout is the role's turn timeout (config.Role.Timeout; config defaults
// it from daemon.judge_timeout / reviewer_timeout and refuses <= 0).
func (rd *round) timeout(role config.Role) time.Duration { return role.Timeout.Duration }

// reviewersRereview reports whether the round's reviewers re-review the
// commits since the previous review (their rereview prompts and effort): a
// re-review, or a recovery whose judge alone started fresh because its
// prompt cache was cold (RoundInput.ColdJudge), with a previous head.
func (rd *round) reviewersRereview() bool {
	k := rd.in.Kind
	return (k == KindRereview || k == KindRecovery && rd.in.ColdJudge) && rd.previousHead() != ""
}

// previousHead is the head the previous review covered.
func (rd *round) previousHead() string {
	if rd.in.Previous != nil && rd.in.Previous.SHA != "" {
		return rd.in.Previous.SHA
	}
	return store.Deref(rd.in.PR.ReviewedSHA)
}

// baseRef is the ref the roles diff against (RoleData/ShellData.BaseRef):
// origin/<base branch>, a ref already qualified kept as is.
func (rd *round) baseRef() string {
	b := rd.in.BaseRef
	if b == "" {
		b = rd.in.Repo.DefaultBranch
	}
	if b == "" {
		b = "master"
	}
	if strings.HasPrefix(b, "origin/") || strings.HasPrefix(b, "refs/") {
		return b
	}
	return "origin/" + b
}
