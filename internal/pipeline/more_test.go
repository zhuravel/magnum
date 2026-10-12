package pipeline

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

func TestReviewerEndsWithoutReportUsageLimit(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{endSilently()}
	e.ag.reads[agents.RoleClaude] = "⏺ Reviewing…\n  ⎿ You've hit your limit · resets 5pm (UTC)\n"
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	rep := res.Reports[agents.RoleClaude]
	if rep.Status != string(agents.HealthUsageLimit) || rep.Health == nil || rep.Health.ResetAt == nil {
		t.Fatalf("claude report = %+v", rep)
	}
	if res.Outcome != OutcomePosted || res.Pause != nil {
		t.Errorf("a reviewer's usage limit must not stop the round: %+v", res)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-review: missing (usage_limit)")
	if run := e.runOf(agents.RoleClaude, store.RunInitial); run.State != store.RunFailed || store.Deref(run.Outcome) != "usage_limit" {
		t.Errorf("claude run = %s / %v", run.State, store.Deref(run.Outcome))
	}
}

// A failed codex review (exit status 1) is no report; its output names why.
func TestCodexErrorTranscriptIsNotAReport(t *testing.T) {
	e := newEnv(t)
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"), []byte("■ You've hit your usage limit. Try again in 2 hours.\n"), 0o600)
	}
	e.ag.exitStatus[agents.RoleCodexReview] = 1
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != string(agents.HealthUsageLimit) || rep.Health == nil || rep.Path != "" {
		t.Fatalf("codex report = %+v", rep)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "codex-review: missing (usage_limit)")

	// The pane after this run's command line names the error too.
	e2 := newEnv(t)
	e2.ag.exitStatus[agents.RoleCodexReview] = 1
	e2.ag.codex = func(f *fakeAgents, c codexCall) error {
		f.mu.Lock()
		f.reads[agents.RoleCodexReview] = "old: You've hit your usage limit.\n$ " + c.Script + "\nError: unexpected status 401 Unauthorized\n" + c.Marker + " 1\n"
		f.mu.Unlock()
		return nil
	}
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}
	res, _ = e2.r.RunRound(e2.ctx, e2.input(KindInitial))
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != string(agents.HealthLoginRequired) {
		t.Fatalf("codex report from the pane = %+v", rep)
	}
}

// A codex review that finished (status 0) is a report, whatever it says:
// findings about rate limits or 401s must not pause codex.
func TestCodexReportMentioningLimitsIsAReport(t *testing.T) {
	e := newEnv(t)
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"),
			[]byte("[P2] client.rb:12 - on insufficient_quota or status 401 the retry loop spins; also: usage limit reached is not handled.\n"), 0o600)
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(604, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != ReportOK || rep.Path == "" {
		t.Fatalf("codex report = %+v", rep)
	}
	if res.Outcome != OutcomePosted || res.Pause != nil {
		t.Fatalf("result = %+v", res)
	}
}

// A failed command without a recognizable error is a failed report, even
// when it wrote output (a partial review).
func TestShellExitStatusFailsTheReport(t *testing.T) {
	e := newEnv(t)
	e.ag.exitStatus[agents.RoleCodexReview] = 2
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(605, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	rep := res.Reports[agents.RoleCodexReview]
	if rep.Status != ReportFailed || rep.Path != "" || !strings.Contains(rep.Detail, "exited with status 2") {
		t.Fatalf("codex report = %+v", rep)
	}
	if run := e.runOf(agents.RoleCodexReview, store.RunInitial); run.State != store.RunFailed {
		t.Fatalf("codex run = %+v", run)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "codex-review: missing (failed)")

	// ok_status lets a tool's "findings" status count as a report.
	e2 := newEnv(t)
	for i := range e2.cfg.Roles {
		if e2.cfg.Roles[i].Name == config.RoleCodexReview {
			e2.cfg.Roles[i].OKStatus = []int{0, 1}
		}
	}
	e2.r.Config = e2.cfg
	e2.ag.exitStatus[agents.RoleCodexReview] = 1
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgePosts(606, "COMMENTED", "COMMENT").behavior(t)}
	res, _ = e2.r.RunRound(e2.ctx, e2.input(KindInitial))
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != ReportOK {
		t.Fatalf("ok_status [0, 1]: codex report = %+v", rep)
	}
}

func TestJudgePromptRefusedHumanActive(t *testing.T) {
	e := newEnv(t)
	e.ag.refuse[agents.RoleJudge] = fmt.Errorf("submit: %w", agents.ErrHumanActive)
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if !errorsIs(err, agents.ErrHumanActive) || res.Outcome != OutcomeError {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	run := e.runOf(agents.RoleJudge, store.RunInitial)
	if run.State != store.RunAbandoned {
		t.Errorf("judge run = %s / %v", run.State, store.Deref(run.Outcome))
	}
}

func TestReviewerPromptRefusedNoSession(t *testing.T) {
	e := newEnv(t)
	e.ag.refuse[agents.RoleClaude] = fmt.Errorf("submit: %w", agents.ErrNoSession)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if rep := res.Reports[agents.RoleClaude]; rep.Status != ReportNoSession {
		t.Errorf("claude report = %+v", rep)
	}
	if run := e.runOf(agents.RoleClaude, store.RunInitial); run.State != store.RunAbandoned {
		t.Errorf("claude run = %s", run.State)
	}
}

func TestJudgeResultFromPaneLine(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		id := markerRunID(t, text)
		f.mu.Lock()
		f.reads[agents.RoleJudge] = "MAGNUM_RESULT {\"status\":\"posted\",\"run_id\":\"r-older-1\"}\n" + // an earlier round
			"...\nMAGNUM_RESULT {\"status\":\"blocked\",\"run_id\":\"" + id + "\",\"blocker\":\"HEAD mismatch\"}\nDone.\n"
		f.mu.Unlock()
		return f.end(run.ID)
	}}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeBlocked || !strings.Contains(res.Error, "HEAD mismatch") || res.Nudged {
		t.Fatalf("result = %+v", res)
	}
}

func TestRecoveryPromptListsHistory(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(604, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindRecovery)
	in.Previous = &PreviousReview{ID: 901, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour)}
	in.History = []PreviousReview{
		{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-2 * time.Hour)},
		*in.Previous,
	}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "mode: recovery",
		"  - 900 CHANGES_REQUESTED on `0011223` (2026-10-03T10:00:00Z)", "  - 901 COMMENTED on `0011223`")
	if claude := e.ag.submitsFor(agents.RoleClaude)[0].Text; strings.Contains(claude, "New commits were pushed") {
		t.Errorf("recovery used the claude rereview prompt:\n%s", claude)
	}
}

// A judge that started fresh because its prompt cache was cold gets the
// recovery prompt, while the reviewers, which kept their conversations,
// re-review the new commits as in a re-review.
func TestAColdJudgesRecoveryKeepsTheReviewersOnTheReReview(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(606, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindRecovery)
	in.ColdJudge = true
	in.Previous = &PreviousReview{ID: 901, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour)}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "mode: recovery")
	mustContain(t, "claude prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "New commits were pushed", " medium")
}

func TestVerifyRetriesTransientGitHubError(t *testing.T) {
	e := newEnv(t)
	e.gh.failLists = 1
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(605, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 605 {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
}

func TestJudgeSessionLost(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		return f.st.TransitionSession(e.ctx, e.sess[agents.RoleJudge].ID, nil, store.SessionLost, nil)
	}}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "lost") {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if run := e.runOf(agents.RoleJudge, store.RunInitial); run.State != store.RunFailed {
		t.Errorf("judge run = %s", run.State)
	}
}

func TestParseResultLenient(t *testing.T) {
	r, ok := parseResult([]byte(`{"status":"Posted","review_id":"123","event":"REQUEST_CHANGES","findings":{"P1":"2","P2":1},"blocker":null}`))
	if !ok || r.Status != "posted" || r.ReviewID != 123 || r.Findings["P1"] != 2 || r.Findings["P2"] != 1 || r.Blocker != "" {
		t.Fatalf("parseResult = %+v, %v", r, ok)
	}
	if _, ok := parseResult([]byte(`{"review_id":1}`)); ok {
		t.Errorf("result without status accepted")
	}
	if _, ok := parseResult([]byte(`not json`)); ok {
		t.Errorf("garbage accepted")
	}
}

// A re-review's result counts its new findings in findings and the earlier
// ones it keeps open in previous_findings.open, by priority: the round's
// result carries them, so the fix loop goes on while an earlier P2 stays
// open with nothing new posted. A result that gives only their number has
// no priorities, and no PreviousOpen.
func TestParseResultReadsTheEarlierFindingsStillOpen(t *testing.T) {
	r, ok := parseResult([]byte(`{"status":"posted","event":"COMMENT","findings":{"P0":0,"P1":0,"P2":0,"P3":0},
		"previous_findings":{"fixed":97,"open":{"P0":0,"P1":0,"P2":"1"},"answered":14}}`))
	if want := map[string]int{"P0": 0, "P1": 0, "P2": 1, "P3": 0}; !ok || !maps.Equal(r.PreviousOpen, want) {
		t.Fatalf("PreviousOpen = %v, %v; want %v", r.PreviousOpen, ok, want)
	}
	for _, payload := range []string{
		`{"status":"posted","findings":{},"previous_findings":{"fixed":0,"open":3,"answered":1}}`,
		`{"status":"posted","findings":{}}`,
	} {
		if r, ok := parseResult([]byte(payload)); !ok || r.PreviousOpen != nil {
			t.Errorf("%s: PreviousOpen = %v, %v; want nil", payload, r.PreviousOpen, ok)
		}
	}
}

func TestParseResultLine(t *testing.T) {
	text := "x MAGNUM_RESULT {\"status\":\"error\",\"run_id\":\"r-a\"}\n" +
		"MAGNUM_RESULT {\"status\":\"posted\",\"run_id\":\"r-b\"} trailing\n" +
		"MAGNUM_RESULT {broken\n"
	r, ok := parseResultLine(text, map[string]bool{"r-a": true})
	if !ok || r.Status != "error" || r.Source != "pane" {
		t.Fatalf("parseResultLine = %+v, %v", r, ok)
	}
	if r, ok := parseResultLine(text, map[string]bool{"r-a": true, "r-b": true}); !ok || r.Status != "posted" {
		t.Errorf("latest line not preferred: %+v", r)
	}
	if _, ok := parseResultLine(text, map[string]bool{"r-c": true}); ok {
		t.Errorf("line of another run accepted")
	}
}

func TestMarkerRegexpIsExact(t *testing.T) {
	re := markerRe("r-20261003T120000-1")
	for body, want := range map[string]bool{
		"<!-- magnum:run=r-20261003T120000-1 head=abc1234 -->": true,
		"<!-- magnum:run=r-20261003T120000-1 -->":              true,
		"magnum:run=r-20261003T120000-1":                       true,
		"<!-- magnum:run=r-20261003T120000-10 -->":             false,
		"<!-- magnum:run=r-20261003T120000-1.5 -->":            false,
	} {
		if got := re.MatchString(body); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}

func TestNormalizeEvent(t *testing.T) {
	for in, want := range map[string]string{"APPROVE": "APPROVED", "comment": "COMMENTED", "REQUEST_CHANGES": "CHANGES_REQUESTED",
		"CHANGES_REQUESTED": "CHANGES_REQUESTED", "": "", "DISMISSED": "DISMISSED"} {
		if got := normalizeEvent(in); got != want {
			t.Errorf("normalizeEvent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyAfterIgnoresEarlierRounds(t *testing.T) {
	rd := &round{r: &Runner{Clock: func() time.Time { return t0 }}}
	text := "■ You've hit your usage limit. Try again at 3:45 PM.\n" + // the previous, already resolved pause
		"run_id: r-new\nworking...\nDone.\n"
	if h := rd.classifyAfter("", text, "r-new"); h.Kind != agents.HealthOK {
		t.Errorf("old error counted: %+v", h)
	}
	if h := rd.classifyAfter("", text, "r-missing"); h.Kind != agents.HealthUsageLimit {
		t.Errorf("without the anchor the whole tail counts: %+v", h)
	}
	if got := tailLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("tailLines = %q", got)
	}
}

// loginFailingCodexReview makes codex-review's command fail (exit status 1)
// with its pane showing Codex's login error after this run's command line.
func loginFailingCodexReview(e *env) {
	e.ag.exitStatus[agents.RoleCodexReview] = 1
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		f.mu.Lock()
		f.reads[agents.RoleCodexReview] = "$ " + c.Script + "\nError: unexpected status 401 Unauthorized\n" + c.Marker + " 1\n"
		f.mu.Unlock()
		return nil
	}
}

// A reviewer's run that fails with its tool's login error pauses the tool
// at once (Runner.OnPause, the engine's PauseTool), before the judge is
// prompted, so other rounds stop starting that kind's roles during the
// outage; the round goes on without the reviewer as before. The pause names
// the run as its proof (Proven). On 2026-10-12 the pause came only when the
// round settled, half an hour later.
func TestAReviewersLoginFailurePausesItsToolAtOnce(t *testing.T) {
	e := newEnv(t)
	loginFailingCodexReview(e)
	var pauses []Pause
	var judgePrompts []int
	e.r.OnPause = func(_ context.Context, p Pause) {
		pauses = append(pauses, p)
		judgePrompts = append(judgePrompts, len(e.ag.submitsFor(agents.RoleJudge)))
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(604, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != string(agents.HealthLoginRequired) {
		t.Fatalf("codex-review report = %+v", rep)
	}
	if len(pauses) != 1 || pauses[0].Kind != string(agents.HealthLoginRequired) || pauses[0].Tool != agents.KindCodex || !pauses[0].Proven || pauses[0].Detail == "" {
		t.Fatalf("pauses = %+v", pauses)
	}
	// The proof's time is the run's end: a login file written after it
	// (the operator logged in) makes the proof stale for the engine.
	if run := e.runOf(agents.RoleCodexReview, KindInitial); run.EndedAt == nil || pauses[0].ProvenAt.IsZero() || pauses[0].ProvenAt.Before(*run.EndedAt) {
		t.Errorf("the pause was proven at %v, the run ended %v", pauses[0].ProvenAt, run.EndedAt)
	}
	if judgePrompts[0] != 0 {
		t.Errorf("the pause came after the judge prompt")
	}
}

// The judge is a session of the same kind: during the outage it stalled and
// magnum nudged it, as it does a judge that ends quietly. A judge that stops
// without a result while a reviewer's run proved its kind logged out ends
// the turn as login_required with the pause (the engine continues the turn
// once the login is back) instead of a nudge, which would stall the same way.
func TestAJudgeThatStopsWhileItsKindIsLoggedOutPausesInsteadOfANudge(t *testing.T) {
	e := newEnv(t)
	loginFailingCodexReview(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently()}
	e.ag.reads[agents.RoleJudge] = "Done reading the diff.\n"
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeLoginRequired || res.Pause == nil || res.Pause.Kind != string(agents.HealthLoginRequired) ||
		res.Pause.Tool != agents.KindCodex || !res.Pause.Proven || !strings.Contains(res.Pause.Detail, "codex-review") {
		t.Fatalf("result = %+v, pause %+v", res, res.Pause)
	}
	if res.Nudged || len(e.ag.submitsFor(agents.RoleJudge)) != 1 {
		t.Errorf("nudged = %v, judge submits = %d; want no nudge", res.Nudged, len(e.ag.submitsFor(agents.RoleJudge)))
	}
	run := e.runOf(agents.RoleJudge, store.RunInitial)
	if run.State != store.RunFailed || store.Deref(run.Outcome) != OutcomeLoginRequired {
		t.Errorf("judge run = %s / %v", run.State, store.Deref(run.Outcome))
	}
	if run.EndedAt == nil || !res.Pause.ProvenAt.Equal(*run.EndedAt) {
		t.Errorf("the judge's pause was proven at %v, its turn stopped %v", res.Pause.ProvenAt, run.EndedAt)
	}
	// A judge that ends quietly while nothing proved its kind logged out is
	// nudged as before.
	e2 := newEnv(t)
	e2.ag.behaviors[agents.RoleJudge] = []behavior{endSilently(), e2.judgePosts(605, "COMMENTED", "COMMENT").behavior(t)}
	e2.ag.reads[agents.RoleJudge] = "Done reading the diff.\n"
	if res, err := e2.r.RunRound(e2.ctx, e2.input(KindInitial)); err != nil || res.Outcome != OutcomePosted || !res.Nudged {
		t.Fatalf("without a logged-out reviewer: result = %+v, %v", res, err)
	}
}
