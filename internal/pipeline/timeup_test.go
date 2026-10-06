package pipeline

import (
	"errors"
	"os"
	"path/filepath"
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
// run in the background) is asked once to stop waiting and write its report
// now, within its run; the report it then writes is its report.
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
		"Stop waiting for background", "pending")
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
// interrupted and reported as timed out, as before; the event says so, and
// names the background work its agent left running.
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
	if len(e.ag.timeUps) != 1 {
		t.Fatalf("time-ups = %d, want 1", len(e.ag.timeUps))
	}
	if elapsed := e.clock.Now().Sub(t0); elapsed < e.cfg.Daemon.ReviewerTimeout.Duration+TimeUpGrace {
		t.Fatalf("elapsed %s, want the timeout and the grace", elapsed)
	}
	if want := "agent:" + agents.AgentName("talkable/talkable", 11920, agents.RoleClaude) + ":esc"; len(e.keys.sends) != 1 || e.keys.sends[0] != want {
		t.Fatalf("interrupt keys = %v, want %s", e.keys.sends, want)
	}
	warns := e.reviewerWarnings(agents.RoleClaude)
	if len(warns) != 1 {
		t.Fatalf("claude warnings: %q", warns)
	}
	mustContain(t, "warning", warns[0], "claude-review report timeout", "interrupted claude-review", "2 background tasks it started still run")
	if run := e.runOf(agents.RoleClaude, store.RunInitial); run.State != store.RunFailed || store.Deref(run.Outcome) != ReportTimeout {
		t.Fatalf("claude run = %s / %s", run.State, store.Deref(run.Outcome))
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
