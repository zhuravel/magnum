package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// reviewerWarnings are the round.reviewer warnings about role.
func (e *env) reviewerWarnings(role agents.Role) []string {
	e.t.Helper()
	var out []string
	for _, ev := range e.eventsOf("round.reviewer") {
		if ev.Level == "warn" && strings.HasPrefix(ev.Message, string(role)+" report ") {
			out = append(out, ev.Message)
		}
	}
	return out
}

// A reviewer still working when its time runs out (the live case: a spec
// run in the background) is asked once to stop its background tasks
// (TaskStop) and write its report now, within its run, starting with its
// run marker; the report it then writes is its report.
func TestAReviewerOutOfTimeIsAskedForItsReport(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{hang()}
	e.ag.onTimeUp = writeReport("## P1 found before the time ran out\n\n- pending: spec/models (still running)\n")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != ReportOK {
		t.Fatalf("claude report = %+v, want the report written after the time-up", got)
	}
	if len(e.ag.timeUps) != 1 {
		t.Fatalf("time-ups = %d, want 1", len(e.ag.timeUps))
	}
	up := e.ag.timeUps[0]
	run := e.runOf(agents.RoleClaude, store.RunInitial)
	if up.Run.ID != run.ID {
		t.Fatalf("time-up sent for run %s, want the reviewer's run %s", up.Run.ID, run.ID)
	}
	mustContain(t, "time-up text", up.Text, "Time is up", "40 minutes", filepath.Join(e.reportDir(), "claude-review.md"),
		"Stop every background task you started with TaskStop", agents.ReportMarker(run.ID), "pending")
	if run.State != store.RunVerified {
		t.Fatalf("claude run = %s, want verified", run.State)
	}
	if len(e.keys.sends) != 0 {
		t.Fatalf("interrupt keys = %v, want none: the reviewer finished", e.keys.sends)
	}
	ups := e.eventsOf("round.time_up")
	if len(ups) != 1 || !strings.Contains(ups[0].Message, "claude-review ran out of its 40 minutes") {
		t.Fatalf("round.time_up events: %+v", ups)
	}
	if elapsed := e.clock.Now().Sub(t0); elapsed >= e.cfg.Daemon.ReviewerTimeout.Duration+TimeUpGrace {
		t.Fatalf("elapsed %s, want the report taken as soon as it was written", elapsed)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-review: "+filepath.Join(e.reportDir(), "claude-review.md"))
}

// A reviewer that does not answer the time-up within TimeUpGrace is
// interrupted and reported as timed out, as before. The background work it
// started outlives the interrupt, so it is told once more to stop it with
// TaskStop and do nothing else; what its transcript still shows running
// StopGrace later is counted in the event (magnum kills nothing).
func TestAReviewerThatIgnoresTheTimeUpIsInterruptedAfterTheGrace(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{hang()}
	e.ag.background = map[agents.Role]int{agents.RoleClaude: 2}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != ReportTimeout {
		t.Fatalf("claude report = %+v, want timeout", got)
	}
	if len(e.ag.timeUps) != 2 {
		t.Fatalf("messages = %d, want the time-up and the stop after the interrupt", len(e.ag.timeUps))
	}
	run := e.runOf(agents.RoleClaude, store.RunInitial)
	if stop := e.ag.timeUps[1]; stop.Run.ID != run.ID || stop.Text != stopBackgroundText {
		t.Fatalf("after the interrupt: %+v, want %q to run %s", stop, stopBackgroundText, run.ID)
	}
	if elapsed := e.clock.Now().Sub(t0); elapsed < e.cfg.Daemon.ReviewerTimeout.Duration+TimeUpGrace+StopGrace {
		t.Fatalf("elapsed %s, want the timeout, the grace and the stop's grace", elapsed)
	}
	if want := "agent:" + agents.AgentName("talkable/talkable", 11920, agents.RoleClaude) + ":esc"; len(e.keys.sends) != 1 || e.keys.sends[0] != want {
		t.Fatalf("interrupt keys = %v, want %s", e.keys.sends, want)
	}
	warns := e.reviewerWarnings(agents.RoleClaude)
	if len(warns) != 1 {
		t.Fatalf("claude warnings: %q", warns)
	}
	mustContain(t, "warning", warns[0], "claude-review report timeout",
		"interrupted claude-review; asked it to stop the 2 background tasks it started: 2 still run after 2 minutes")
	if run.State != store.RunFailed || store.Deref(run.Outcome) != ReportTimeout {
		t.Fatalf("claude run = %s / %s", run.State, store.Deref(run.Outcome))
	}
}

// A reviewer interrupted with background work running is told to stop it
// (TaskStop) and do nothing else; once its transcript shows none left, the
// round goes on, and the event says it stopped them.
func TestAnInterruptedReviewerIsToldToStopItsBackgroundTasks(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{endSilently()}
	e.ag.background = map[agents.Role]int{agents.RoleClaude: 1}
	e.ag.onTimeUp = func(f *fakeAgents, run store.Run, text string) error {
		if text == stopBackgroundText {
			f.mu.Lock()
			f.background[agents.RoleClaude] = 0
			f.mu.Unlock()
		}
		return nil
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(609, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Reports[agents.RoleClaude].Status != ReportMissing {
		t.Fatalf("claude report = %+v", res.Reports[agents.RoleClaude])
	}
	if len(e.keys.sends) != 1 {
		t.Fatalf("interrupt keys = %v", e.keys.sends)
	}
	if len(e.ag.timeUps) != 1 || e.ag.timeUps[0].Text != stopBackgroundText {
		t.Fatalf("messages = %+v, want only the stop", e.ag.timeUps)
	}
	mustContain(t, "stop message", stopBackgroundText, "TaskStop", "do nothing else")
	warns := e.reviewerWarnings(agents.RoleClaude)
	if len(warns) != 1 || warns[0] != "claude-review report missing: finished without writing claude-review.md; "+
		"interrupted claude-review; asked it to stop the 1 background task it started: it did" {
		t.Fatalf("claude warnings: %q", warns)
	}
}

// cutClaude sets up a round whose stages cut claude-review's turn short
// while it works: a push, on which the round restarts (restart), or
// codex-review's refusal, which ends the round (else; claude-review hangs
// until the round cancels it). It returns the round's input.
func (e *env) cutClaude(restart bool) RoundInput {
	in := e.input(KindInitial)
	if !restart {
		e.ag.exitStatus[agents.RoleCodexReview] = 1
		e.ag.codex = func(f *fakeAgents, c codexCall) error {
			return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"),
				[]byte("ERROR: This content was flagged for possible cybersecurity risk.\n"), 0o600)
		}
		e.ag.hangs = map[agents.Role]bool{agents.RoleClaude: true} // at work until the round cancels it
		return in
	}
	e.withRestarts(&in, 2)
	pushAfterCodex := func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunVerified)
		e.push(head2)
		return nil // the turn stays in flight until the push cuts it
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushAfterCodex, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(611, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(e.t)}
	return in
}

// stopsWhenTold makes claude-review stop its background work when it is
// told so with text, and returns how its run stood at each message: its
// state and whether its turn was interrupted (esc) before.
func (e *env) stopsWhenTold(text string) *[]string {
	var seen []string
	e.ag.onTimeUp = func(f *fakeAgents, run store.Run, got string) error {
		seen = append(seen, fmt.Sprintf("%s esc=%t", run.State, slices.Contains(e.sendsTo(agents.RoleClaude), "esc")))
		if got == text {
			f.mu.Lock()
			f.background[agents.RoleClaude] = 0
			f.mu.Unlock()
		}
		return nil
	}
	return &seen
}

// A push that cuts claude-review short while work it started in the
// background still runs: once its turn is interrupted (esc), it is told
// within the cut run to stop that work, in words that leave the review to
// the restart prompt that follows; the run is abandoned after, and the
// warning says what happened.
func TestAPushTellsACutReviewerToStopItsBackgroundTasks(t *testing.T) {
	e := newEnv(t)
	in := e.cutClaude(true)
	e.ag.background = map[agents.Role]int{agents.RoleClaude: 2}
	seen := e.stopsWhenTold(headMovedStopText)

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Restarts != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(e.ag.timeUps) != 1 || e.ag.timeUps[0].Text != headMovedStopText || e.ag.timeUps[0].Run.TargetSHA != target {
		t.Fatalf("messages = %+v, want the restart's stop within the cut run", e.ag.timeUps)
	}
	mustContain(t, "stop message", headMovedStopText, "head moved", "TaskStop", "do nothing else", "The next message restarts the review")
	if strings.Contains(headMovedStopText, "over") {
		t.Errorf("the restart's stop says the review is over: %q", headMovedStopText)
	}
	if want := []string{store.RunWorking + " esc=true"}; !slices.Equal(*seen, want) {
		t.Errorf("cut run at the message = %v, want %v (interrupted, not yet abandoned)", *seen, want)
	}
	var claude []string // claude-review's prompts and messages, in order
	for _, o := range e.ag.order {
		if strings.HasSuffix(o, ":claude-review") {
			claude = append(claude, o)
		}
	}
	if want := []string{"submit:claude-review", "time_up:claude-review", "submit:claude-review"}; !slices.Equal(claude, want) {
		t.Errorf("claude-review got %v, want the stop between its prompt and the restart's", claude)
	}
	for _, r := range e.runs() {
		if r.Role == string(agents.RoleClaude) && r.TargetSHA == target {
			wantRun(t, r, store.RunAbandoned, ReportHeadMoved)
		}
	}
	want := "interrupted claude-review for the restart; asked it to stop the 2 background tasks it started: it did"
	if !slices.Contains(res.Warnings, want) {
		t.Errorf("warnings = %q, want %q", res.Warnings, want)
	}
}

// The end of the round does the same for a reviewer it cuts short, with
// the words that say the review is over; one that still shows working after
// the interrupt says that too.
func TestARoundsEndTellsACutReviewerToStopItsBackgroundTasks(t *testing.T) {
	e := newEnv(t)
	in := e.cutClaude(false)
	e.ag.background = map[agents.Role]int{agents.RoleClaude: 1}
	seen := e.stopsWhenTold(stopBackgroundText)

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeRefused {
		t.Fatalf("result = %+v", res)
	}
	if len(e.ag.timeUps) != 1 || e.ag.timeUps[0].Text != stopBackgroundText {
		t.Fatalf("messages = %+v, want only the stop", e.ag.timeUps)
	}
	if want := []string{store.RunWorking + " esc=true"}; !slices.Equal(*seen, want) {
		t.Errorf("cut run at the message = %v, want %v (interrupted, not yet abandoned)", *seen, want)
	}
	wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunAbandoned, ReportCancelled)
	want := "claude-review still works 1m0s after it was interrupted for the round's end; " +
		"asked it to stop the 1 background task it started: it did"
	if !slices.Contains(res.Warnings, want) {
		t.Errorf("warnings = %q, want %q", res.Warnings, want)
	}
}

// A cut reviewer whose transcript shows no background work, or cannot be
// read (not a claude agent, no transcript), gets no message, as before.
func TestACutReviewerWithNoBackgroundTasksGetsNoMessage(t *testing.T) {
	for _, restart := range []bool{true, false} {
		for name, background := range map[string]map[agents.Role]int{
			"none running":          {agents.RoleClaude: 0},
			"transcript unreadable": nil,
		} {
			t.Run(fmt.Sprintf("restart=%t/%s", restart, name), func(t *testing.T) {
				e := newEnv(t)
				in := e.cutClaude(restart)
				e.ag.background = background
				res, err := e.r.RunRound(e.ctx, in)
				if err != nil {
					t.Fatalf("RunRound: %v", err)
				}
				if !slices.Contains(e.sendsTo(agents.RoleClaude), "esc") {
					t.Fatalf("claude-review was not interrupted: %v", e.keys.sends)
				}
				if len(e.ag.timeUps) != 0 {
					t.Errorf("messages = %+v, want none", e.ag.timeUps)
				}
				for _, w := range res.Warnings {
					if strings.Contains(w, "background") {
						t.Errorf("warning %q", w)
					}
				}
			})
		}
	}
}

// A time-up that cannot be sent ends the turn at its timeout, as before.
func TestATimeUpThatCannotBeSentEndsTheTurnAtItsTimeout(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{hang()}
	e.ag.timeUpErr = errors.New("agents: time up: herdr agent.prompt: agent_not_found")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Reports[agents.RoleClaude].Status != ReportTimeout {
		t.Fatalf("claude report = %+v", res.Reports[agents.RoleClaude])
	}
	if elapsed := e.clock.Now().Sub(t0); elapsed >= e.cfg.Daemon.ReviewerTimeout.Duration+TimeUpGrace {
		t.Fatalf("elapsed %s: the grace ran although nothing was asked", elapsed)
	}
	if len(e.keys.sends) != 1 {
		t.Fatalf("interrupt keys = %v", e.keys.sends)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "claude-review ran out of its 40 minutes and could not be asked for its report")
	}
	if !found {
		t.Fatalf("warnings %q", res.Warnings)
	}
}

// A shell role has no agent to ask: its command is interrupted at its
// timeout, as before.
func TestAShellRoleOutOfTimeIsNotAskedForItsReport(t *testing.T) {
	e := newEnv(t)
	e.ag.codex = func(*fakeAgents, codexCall) error { return agents.ErrTimeout }
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(604, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Reports[agents.RoleCodexReview].Status != ReportTimeout || len(e.ag.timeUps) != 0 {
		t.Fatalf("codex report %+v, time-ups %d", res.Reports[agents.RoleCodexReview], len(e.ag.timeUps))
	}
	warns := e.reviewerWarnings(agents.RoleCodexReview)
	if len(warns) != 1 || !strings.Contains(warns[0], "interrupted codex-review") {
		t.Fatalf("codex warnings: %q", warns)
	}
}

// A reviewer whose run ended without a report is interrupted, so whatever
// it still does stops before the round moves on, and the event says so.
func TestAReviewerThatEndedWithoutAReportIsInterrupted(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{endSilently()}
	e.ag.background = map[agents.Role]int{agents.RoleClaude: 0}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(605, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Reports[agents.RoleClaude].Status != ReportMissing {
		t.Fatalf("claude report = %+v", res.Reports[agents.RoleClaude])
	}
	if want := "agent:" + agents.AgentName("talkable/talkable", 11920, agents.RoleClaude) + ":esc"; len(e.keys.sends) != 1 || e.keys.sends[0] != want {
		t.Fatalf("interrupt keys = %v, want %s", e.keys.sends, want)
	}
	warns := e.reviewerWarnings(agents.RoleClaude)
	if len(warns) != 1 || warns[0] != "claude-review report missing: finished without writing claude-review.md; interrupted claude-review" {
		t.Fatalf("claude warnings: %q", warns)
	}
	if len(e.ag.timeUps) != 0 {
		t.Fatalf("time-ups = %d for a turn that ended in time", len(e.ag.timeUps))
	}
}

// Report paths are per head, not per run, so a reviewer that kept working
// after its run ended may write where a later round on the same head looks.
// A report already there when a role is prompted is not its report: it is
// set aside, also when it appeared after the round started (a role in a
// later stage).
func TestAReportWrittenBeforeItsRoleWasPromptedIsSetAside(t *testing.T) {
	e := newEnv(t)
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Name == string(agents.RoleClaude) {
			e.cfg.Roles[i].After = []string{string(agents.RoleCodexReview)}
		}
	}
	stale := filepath.Join(e.reportDir(), "claude-review.md")
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		if err := os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"), []byte("[P2] codex finding\n"), 0o600); err != nil {
			return err
		}
		// An earlier round's claude-review agent, still working, writes its
		// report now: after this round set aside the stale files.
		return os.WriteFile(stale, []byte("## late report of an earlier run\n"), 0o600)
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{endSilently()}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(606, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != ReportMissing {
		t.Fatalf("claude report = %+v, want missing: the file it found was not its own", got)
	}
	if b, err := os.ReadFile(stale + ".prev"); err != nil || !strings.Contains(string(b), "late report") {
		t.Fatalf("the late report was not set aside: %q, %v", b, err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-review: missing (missing)")
}

// A continued round reuses only the reports its paused round verified: a
// file a role wrote after its run ended (missing, timed out) is not read as
// that round's report.
func TestAContinuedRoundReadsOnlyReportsItsRoundVerified(t *testing.T) {
	e := newEnv(t)
	dir := e.reportDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"claude-review.md": "## late claude\n", "codex-review.md": "[P2] codex\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []store.Run{
		{Role: config.RoleClaudeReview, State: store.RunFailed, Outcome: store.Ptr(ReportMissing)},
		{Role: config.RoleCodexReview, State: store.RunVerified, Outcome: store.Ptr(ReportOK)},
	} {
		r.PRID, r.Round, r.Kind, r.TargetSHA = e.pr.ID, 1, store.RunInitial, target
		r.ReportPath = store.Ptr(filepath.Join(dir, map[string]string{config.RoleClaudeReview: "claude-review.md", config.RoleCodexReview: "codex-review.md"}[r.Role]))
		if _, err := e.st.CreateRun(e.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(607, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindContinue)
	in.ContinueRunID = "r-20261003T100000-7"

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Reports[agents.RoleClaude].Status != ReportMissing || res.Reports[agents.RoleCodexReview].Status != ReportOK {
		t.Fatalf("reports = %+v", res.Reports)
	}
	if got := res.Reports[agents.RoleCodexReview].Path; got != filepath.Join(dir, "codex-review.md") {
		t.Fatalf("codex report path = %q", got)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "claude-review.md is not round 1's report")
	}
	if !found {
		t.Fatalf("warnings %q", res.Warnings)
	}
}

// The reviewer's prompt names its time budget, rendered from the role's
// timeout.
func TestAReviewerPromptNamesTheRolesTimeBudget(t *testing.T) {
	e := newEnv(t)
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Name == string(agents.RoleClaude) {
			e.cfg.Roles[i].Timeout = config.Duration{Duration: 25 * time.Minute}
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(608, "COMMENTED", "COMMENT").behavior(t)}
	if _, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	mustContain(t, "claude prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "Time budget: 25 minutes. Your turn must end with the report written.")
}

func TestDurationWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		40 * time.Minute: "40 minutes", time.Minute: "1 minute", 90 * time.Minute: "90 minutes",
		2 * time.Hour: "2 hours", time.Hour: "1 hour", 30 * time.Second: "30 seconds", 90 * time.Second: "1m30s",
	} {
		if got := durationWords(d); got != want {
			t.Errorf("durationWords(%s) = %q, want %q", d, got, want)
		}
	}
}
