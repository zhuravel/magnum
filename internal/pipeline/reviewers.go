package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
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

// runRole runs one non-judge role whose capture is file (or stdout) and
// returns its report; the run ends final.
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

// runPatchRole runs a git-diff role: its turn, then `git diff` of the slot
// against TargetSHA saved as its report, then the checkout restored to
// TargetSHA. A turn cut short (timeout, cancelled round) is interrupted and
// the restore waits until the agent is seen idle (InterruptWait). The error
// is set when the checkout could not be restored, or when the agent kept
// working past the wait (nothing may run on a tree an agent may still edit).
func (rd *round) runPatchRole(ctx context.Context, role config.Role, run store.Run) (RoleReport, error) {
	path := rd.reportPath(run, role)
	t, anchor, early := rd.turn(ctx, role, run, path)
	if early != nil {
		return *early, nil // nothing ran, so nothing was edited
	}
	if t.run.ID != "" {
		run = t.run // a continuation on a fallback model
	}
	var rep RoleReport
	switch t.kind {
	case waitRefused:
		rep = rd.newReport(role, run.ID)
		rep.Status, rep.Detail = refusedStatus(t.err), execx.Redact(t.err.Error())
		return rep, nil // nothing was sent
	case waitCancelled:
		rep = rd.newReport(role, run.ID)
		rep.Status = ReportCancelled
		if t.unsent {
			return rep, nil
		}
		rd.interrupt(ctx, role, run)
	case waitEnded:
		rep = rd.collectPatch(ctx, role, run, path, anchor)
	default:
		rep = rd.finishReviewer(ctx, role, run, path, anchor, t) // interrupts a timed-out turn
	}
	idle := t.kind == waitEnded || t.unsent || rd.waitIdle(ctx, role, run)
	if err := rd.restoreTree(ctx, role); err != nil {
		rep.Status, rep.Detail = ReportFailed, execx.Redact(err.Error())
		rd.finishReviewerRun(ctx, role, run, rep)
		return rep, err
	}
	if !idle {
		err := fmt.Errorf("pipeline: %s still works %s after it was interrupted; its checkout was restored but cannot be trusted", role.Name, InterruptWait)
		rd.warn(ctx, "%v", err)
		return rep, err
	}
	if t.kind == waitEnded {
		rd.finishReviewerRun(ctx, role, run, rep)
	}
	return rep, nil
}

// turn prompts role (an agent session) or runs its command (a shell role)
// and waits for the turn to end. A session turn that ended on its model's
// own limit continues on the kind's fallback models (reviewerFallbacks); the
// returned turn is then the last continuation's. anchor locates the last
// prompt in the pane text. early is set when the role ended without a turn
// (a prompt that could not be rendered, no pane, a busy shell,
// cancellation); its run is final then.
func (rd *round) turn(ctx context.Context, role config.Role, run store.Run, path string) (t turn, anchor string, early *RoleReport) {
	if role.IsShell() {
		return rd.shellTurn(ctx, role, run, path)
	}
	kind, data := rd.roleData(role, path)
	text, err := rd.r.Agents.RolePrompt(role, kind, data)
	if err != nil {
		rd.finishRun(ctx, run.ID, store.RunFailed, ReportFailed, err.Error())
		rep := rd.newReport(role, run.ID)
		rep.Status, rep.Detail = ReportFailed, execx.Redact(err.Error())
		return turn{}, "", &rep
	}
	rd.event(ctx, "info", "round.reviewer", fmt.Sprintf("prompting %s (run %s, %s)", role.Name, run.ID, kind), map[string]any{"run": run.ID, "role": run.Role})
	anchor = rd.in.TargetSHA
	if strings.Contains(text, path) {
		anchor = path
	}
	t, anchor = rd.reviewerFallbacks(ctx, role, rd.submitAndWait(ctx, run, text, rd.timeout(role), "", nil), path, anchor)
	return t, anchor, nil
}

// roleData fills a session role's prompt: the rereview prompt (and mode) for
// a rereview round after an earlier review, else the initial one; after a
// restart on a newer head the role's restart prompt when it has one. The
// effort is the role's for the round (config.Role.EffortFor).
func (rd *round) roleData(role config.Role, path string) (string, agents.RoleData) {
	in := rd.in
	prev := rd.previousHead()
	rereview := in.Kind == KindRereview && prev != ""
	effort := role.EffortFor(rereview)
	d := agents.RoleData{
		URL: rd.pr.URL, Owner: rd.owner, Repo: rd.name, Number: in.PR.Number,
		HeadSHA: in.TargetSHA, BaseSHA: in.BaseSHA, BaseRef: rd.baseRef(), ReportPath: path,
		Model: rd.r.Config.RoleModel(role), Effort: effort, EffortInPrompt: rd.effortInPrompt(role, effort),
		Mode: agents.ModeInitial, NotesPath: in.NotesPath, Blind: in.Blind, PostMerge: in.PostMerge,
	}
	kind := config.PromptInitial
	if rereview {
		kind, d.Mode, d.PreviousHeadSHA, d.ForcePushed = config.PromptRereview, agents.ModeRereview, prev, in.ForcePushed
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
	line, err := rd.r.Agents.ShellLine(role, agents.ShellData{
		Title:      agents.TaggedTitle(rd.r.AgentTag, rd.name, rd.pr.Number, agents.Role(role.Name)),
		ReportPath: path, Marker: marker, RunID: run.ID,
		BaseRef: rd.baseRef(), BaseSHA: rd.in.BaseSHA, HeadSHA: rd.in.TargetSHA, URL: rd.pr.URL,
	})
	if err != nil {
		return end(store.RunFailed, ReportFailed, execx.Redact(err.Error()))
	}
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
		rd.finishReviewerRun(ctx, role, run, rep)
		return rep
	case waitTimeout, waitLost:
		if t.kind == waitTimeout {
			rd.interrupt(ctx, role, run)
			rep.Status = ReportTimeout
		} else {
			rep.Status = ReportLost
		}
		rep.Detail = detail(t.err)
		if r, ok := rd.reportFile(role, run, path); ok {
			r.Detail = "report present, but the role did not finish: " + rep.Status
			rep = r
		}
		rd.finishReviewerRun(ctx, role, run, rep)
		return rep
	}
	// waitEnded (or waitResult, unused for these roles).
	rep = rd.checkReport(ctx, role, run, path, anchor)
	rd.finishReviewerRun(ctx, role, run, rep)
	return rep
}

func (rd *round) finishReviewerRun(ctx context.Context, role config.Role, run store.Run, rep RoleReport) {
	if rep.Status == ReportOK || rep.Status == ReportEmpty {
		rd.finishRun(ctx, run.ID, store.RunVerified, rep.Status, rep.Detail)
		return
	}
	msg := rep.Detail
	if msg == "" {
		msg = rep.Status
	}
	rd.finishRun(ctx, run.ID, store.RunFailed, rep.Status, msg)
	rd.event(ctx, "warn", "round.reviewer", fmt.Sprintf("%s report %s: %s", role.Name, rep.Status, msg), map[string]any{"run": run.ID, "role": run.Role, "status": rep.Status})
}

// reportFile reports a present, non-empty report as ok. Its text is never
// classified: a review that discusses usage limits or 401s is still a
// review.
func (rd *round) reportFile(role config.Role, run store.Run, path string) (RoleReport, bool) {
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return RoleReport{}, false
	}
	rep := rd.newReport(role, run.ID)
	rep.Status, rep.Path = ReportOK, path
	return rep, true
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
	if rep, ok := rd.reportFile(role, run, path); ok {
		return rep
	}
	rep := rd.newReport(role, run.ID)
	rep.Status, rep.Detail = ReportMissing, "finished without writing "+filepath.Base(path)
	if st, err := os.Stat(path); err == nil && st.Size() == 0 {
		rep.Detail = "empty " + filepath.Base(path)
		if role.Capture == config.CaptureGitDiff {
			rep.Status = ReportEmpty
		}
	}
	if h := asUsageLimit(rd.paneHealth(ctx, role, run, anchor)); h.Kind != agents.HealthOK {
		rep.Status, rep.Detail, rep.Health = string(h.Kind), h.Detail, &h
	}
	return rep
}

// collectPatch writes the slot's changes against TargetSHA (committed or
// not) to path. Untracked files are not part of the patch.
func (rd *round) collectPatch(ctx context.Context, role config.Role, run store.Run, path, anchor string) RoleReport {
	rep := rd.newReport(role, run.ID)
	res, err := rd.r.Exec.Run(ctx, rd.git(role, false, "diff", "--binary", "--no-color", "--no-ext-diff", rd.in.TargetSHA))
	if err != nil {
		rep.Status, rep.Detail = ReportFailed, execx.Redact(fmt.Sprintf("git diff: %v", err))
		return rep
	}
	if len(res.Stdout) == 0 {
		rep = rd.checkReport(ctx, role, run, path, anchor)
		if rep.Health == nil {
			rep.Status, rep.Path, rep.Detail = ReportEmpty, "", "no changes"
		}
		return rep
	}
	if err := writeFileAtomic(path, res.Stdout); err != nil {
		rep.Status, rep.Detail = ReportFailed, fmt.Sprintf("write %s: %v", filepath.Base(path), err)
		return rep
	}
	rep.Status, rep.Path = ReportOK, path
	return rep
}

// restoreTree discards a git-diff role's edits, staged ones included, and
// puts HEAD back on TargetSHA: `git reset --hard` and `git clean -fd`
// (relative to HEAD, so a branch the agent switched to is never moved), then
// HEAD switched back to TargetSHA when the agent moved it. A checkout still
// dirty afterwards is an error.
func (rd *round) restoreTree(ctx context.Context, role config.Role) error {
	ctx = context.WithoutCancel(ctx)
	slot, target := rd.in.SlotPath, rd.in.TargetSHA
	for _, args := range [][]string{{"reset", "--hard", "--quiet"}, {"clean", "-fd"}} {
		if _, err := rd.r.Exec.Run(ctx, rd.git(role, true, args...)); err != nil {
			return fmt.Errorf("pipeline: %s cleanup git %s: %w", role.Name, strings.Join(args, " "), err)
		}
	}
	head, err := rd.r.Git.RevParse(ctx, slot, "HEAD")
	if err != nil {
		return fmt.Errorf("pipeline: %s cleanup: %w", role.Name, err)
	}
	if head != target {
		rd.warn(ctx, "%s moved HEAD to %s; switching back to %s", role.Name, short(head), short(target))
		if err := rd.r.Git.SwitchDetach(ctx, slot, target); err != nil {
			return fmt.Errorf("pipeline: %s cleanup: %w", role.Name, err)
		}
		if head, err = rd.r.Git.RevParse(ctx, slot, "HEAD"); err != nil {
			return fmt.Errorf("pipeline: %s cleanup: %w", role.Name, err)
		}
		if head != target {
			return fmt.Errorf("pipeline: %s cleanup: HEAD is %s, want %s", role.Name, short(head), short(target))
		}
	}
	if st, err := rd.r.Git.Status(ctx, slot); err != nil {
		rd.warn(ctx, "status after %s cleanup: %v", role.Name, err)
	} else if st.Dirty() {
		return fmt.Errorf("pipeline: %s cleanup: checkout still dirty (%d tracked, %d untracked)", role.Name, st.Tracked, st.Untracked)
	}
	return nil
}

// git builds a `git -C <slot>` command. It unsets the variables that would
// point git at another repository, as gitx does for its own commands.
func (rd *round) git(role config.Role, mutates bool, args ...string) execx.Cmd {
	return execx.Cmd{
		Name:    "git",
		Args:    append([]string{"-C", rd.in.SlotPath}, args...),
		Env:     map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		Unset:   gitx.ScrubbedEnv(),
		Mutates: mutates,
		Label:   "pipeline " + role.Name,
	}
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

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
