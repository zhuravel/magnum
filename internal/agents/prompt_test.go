package agents

import (
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestPromptSubmitsAndRecordsRun(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(time.Minute)

	id, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "/code-review https://x high")
	if err != nil {
		t.Fatal(err)
	}
	p := e.h.prompts[0]
	if p.Target != "mg-11920-claude-review-5d01cf" || p.Text != "/code-review https://x high" || p.Wait == nil ||
		p.Wait.Timeout != 60*time.Second || len(p.Wait.Until) != 2 || p.Wait.Until[0] != herdr.StatusWorking || p.Wait.Until[1] != herdr.StatusBlocked {
		t.Fatalf("prompt call = %+v (wait %+v)", p, p.Wait)
	}
	r := e.run1(id)
	sess := e.session(RoleClaude)
	if r.State != store.RunWorking || r.SubmittedAt == nil || r.WorkingSeenAt == nil || store.Deref(r.SessionID) != sess.ID ||
		r.Kind != store.RunInitial || r.Round != 1 || r.TargetSHA != e.pr.HeadSHA || r.Identity != "talkable-app" ||
		r.ReviewerLogin != "talkable[bot]" || r.PromptText != "/code-review https://x high" {
		t.Fatalf("run = %+v", r)
	}
	wantReport := filepath.Join(e.cfg.Layout.ReviewDir("talkable", "talkable", 11920, e.pr.HeadSHA), "claude-review.md")
	if store.Deref(r.ReportPath) != wantReport {
		t.Fatalf("report path = %q, want %q", store.Deref(r.ReportPath), wantReport)
	}
	if sess.LastPromptAt == nil || !sess.LastPromptAt.Equal(e.clock.Now()) || sess.IdleTicks != 0 || store.Deref(sess.AgentStatus) != "working" {
		t.Fatalf("session = %+v", sess)
	}
}

func TestSubmitPrecreatedRunAndRefuseResend(t *testing.T) {
	e := newEnv(t)
	e.started()
	run, err := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunInitial, 3)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.RunPending || run.Round != 3 || store.Deref(run.SessionID) != e.session(RoleJudge).ID {
		t.Fatalf("new run = %+v", run)
	}
	if err := e.m.Submit(e.ctx, run, "judge "+run.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.run1(run.ID); got.PromptText != "judge "+run.ID || got.State != store.RunWorking {
		t.Fatalf("run = %+v", got)
	}
	err = e.m.Submit(e.ctx, run, "again")
	if !errors.Is(err, store.ErrConflict) || len(e.h.prompts) != 1 {
		t.Fatalf("resend: err = %v, prompts = %d; want ErrConflict and no second prompt", err, len(e.h.prompts))
	}
	// Prompt without an explicit round joins the latest round.
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunNudge, "nudge")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.run1(id); r.Round != 3 || r.Kind != store.RunNudge {
		t.Fatalf("nudge run = %+v", r)
	}
}

func TestPromptErrorMapping(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		status    herdr.Status
		want      error
		wantState string
	}{
		{"blocked before send", &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentBlocked, Message: "blocked"}, "", ErrBlocked, store.RunFailed},
		{"stalled", &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentPromptStalled, Message: "stalled"}, "", ErrStalled, store.RunSubmitted},
		{"server timeout", &herdr.Error{Method: "agent.prompt", Code: herdr.CodeTimeout, Message: "timeout"}, "", ErrTimeout, store.RunSubmitted},
		{"client timeout", herdr.ErrTimeout, "", ErrTimeout, store.RunSubmitted},
		{"unavailable", herdr.ErrUnavailable, "", herdr.ErrUnavailable, store.RunFailed},
		{"blocked after send", nil, herdr.StatusBlocked, ErrBlocked, store.RunSubmitted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			if tc.err != nil {
				e.h.errs["AgentPrompt"] = tc.err
			}
			if tc.status != "" {
				e.h.promptStatus = tc.status
			}
			id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "go")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if id == "" {
				t.Fatal("Prompt must return the run id even on error")
			}
			r := e.run1(id)
			if r.State != tc.wantState {
				t.Fatalf("run state = %s, want %s", r.State, tc.wantState)
			}
			if tc.err != nil && r.Error == nil {
				t.Fatal("run error should be recorded")
			}
		})
	}
}

func TestPromptRefusals(t *testing.T) {
	e := newEnv(t)
	e.workspace()
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("agent not started: err = %v, want ErrNoSession", err)
	}
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleCodexReview, store.RunInitial, "x"); !errors.Is(err, ErrNotAgent) {
		t.Fatalf("shell role: err = %v, want ErrNotAgent", err)
	}
	for _, r := range []Role{RoleJudge, RoleClaude} {
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(r), store.Deref(e.session(r).HerdrPaneID), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.UpdatePR(e.ctx, e.pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", e.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(5 * time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x"); !errors.Is(err, ErrHumanActive) {
		t.Fatalf("human cooldown: err = %v, want ErrHumanActive", err)
	}
	if len(e.h.prompts) != 0 {
		t.Fatal("no prompt may be sent during the human cooldown")
	}
	e.clock.Add(11 * time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x"); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
}

func TestRunShell(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	role := e.spec(RoleCodexReview)
	line, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/codex.md", Marker: DoneMarker("r-1")})
	if err != nil {
		t.Fatal(err)
	}
	pane := ws.Panes[RoleCodexReview]
	e.h.waitLine = "MAGNUM_DONE_r-1 0"
	status, err := e.m.RunShell(e.ctx, e.pr, role, pane, line, DoneMarker("r-1"), 40*time.Minute)
	if err != nil || status != 0 {
		t.Fatalf("RunShell = %d, %v", status, err)
	}
	if len(e.h.runs) != 1 || e.h.runs[0].Pane != pane || e.h.runs[0].Command != line {
		t.Fatalf("pane runs = %+v", e.h.runs)
	}
	if got := e.keysSent(); len(got) != 1 || got[0] != pane+":ctrl+u" {
		t.Fatalf("keys = %v, want the line editor cleared first", got)
	}
	w := e.h.waits[0]
	if !w.Regex || w.Match != `(?m)^MAGNUM_DONE_r-1(?:[ \t]+\d+)?[ \t]*$` || w.Timeout != 40*time.Minute || w.Source != herdr.SourceRecentUnwrapped {
		t.Fatalf("wait = %+v", w)
	}
	re := regexp.MustCompile(w.Match)
	for text, want := range map[string]bool{
		"out\nMAGNUM_DONE_r-1 0\n": true, "MAGNUM_DONE_r-1 127": true, "MAGNUM_DONE_r-1\n": true,
		"$ x; printf '\\nMAGNUM_DONE_r-1 %d\\n' \"$?\"": false, "MAGNUM_DONE_r-10 0": false, "reportMAGNUM_DONE_r-1 0": false,
	} {
		if got := re.MatchString(text); got != want {
			t.Errorf("marker pattern on %q = %v, want %v", text, got, want)
		}
	}
	if got := e.h.callsWith("WaitIdleShell " + pane); len(got) != 1 {
		t.Fatalf("idle-shell check = %v", got)
	}
	s := e.session(RoleCodexReview)
	if s.State != store.SessionLive || s.LastPromptAt == nil || store.Deref(s.AgentKind) != KindShell {
		t.Fatalf("codex-review session = %+v", s)
	}

	for line, want := range map[string]int{"MAGNUM_DONE_r-1 2": 2, "MAGNUM_DONE_r-1": ShellStatusUnknown, "": ShellStatusUnknown} {
		e.h.waitLine = line
		if line == "" {
			e.h.waitLine = "something else"
		}
		if status, err := e.m.RunShell(e.ctx, e.pr, role, pane, "x", DoneMarker("r-1"), time.Minute); err != nil || status != want {
			t.Errorf("matched %q: status = %d, %v; want %d", line, status, err, want)
		}
	}

	e.h.errs["PaneWaitOutput"] = &herdr.Error{Method: "pane.wait_for_output", Code: herdr.CodeTimeout, Message: "t"}
	if _, err := e.m.RunShell(e.ctx, e.pr, role, pane, line, DoneMarker("r-1"), time.Minute); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: err = %v, want ErrTimeout", err)
	}
	e.h.busy[pane] = true
	if _, err := e.m.RunShell(e.ctx, e.pr, role, pane, line, DoneMarker("r-2"), time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy shell: err = %v, want ErrBusy", err)
	}
	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleClaude), pane, line, DoneMarker("r-3"), time.Minute); err == nil {
		t.Fatal("an agent role: want an error")
	}
}

func TestRunShellHonoursHumanCooldown(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	pane := ws.Panes[RoleCodexReview]
	if err := e.st.UpdatePR(e.ctx, e.pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", e.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(5 * time.Minute)
	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "x", DoneMarker("r-1"), time.Minute); !errors.Is(err, ErrHumanActive) {
		t.Fatalf("err = %v, want ErrHumanActive", err)
	}
	if len(e.h.runs) != 0 {
		t.Fatal("nothing may be typed during the human cooldown")
	}
}

// failSQL makes the registry refuse the statements trigger names (a
// BEFORE trigger body's condition), for the rest of the test.
func (e *env) failSQL(name, on string) {
	e.t.Helper()
	if _, err := e.st.DB().ExecContext(e.ctx, "CREATE TRIGGER "+name+" BEFORE "+on+
		" BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) logged(t *testing.T, want ...string) {
	t.Helper()
	for _, l := range e.logs.all() {
		if !slices.ContainsFunc(want, func(w string) bool { return !strings.Contains(l, w) }) {
			return
		}
	}
	t.Fatalf("logs = %q, want a line with %q", e.logs.all(), want)
}

// A refused prompt whose run cannot be marked abandoned says so in the log:
// the run stays pending and counts as in flight.
func TestARefusedPromptLogsAFailedAbandon(t *testing.T) {
	e := newEnv(t)
	e.workspace()
	e.failSQL("fail_abandon", "UPDATE OF state ON runs WHEN NEW.state = 'abandoned'")
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("Prompt = %v, want ErrNoSession", err)
	}
	e.logged(t, "abandon", id, "disk I/O error")
}

// A prompt whose session row cannot record it (last_prompt_at) is still
// sent, and the failure is logged; so is a shell role's.
func TestAPromptTheSessionCannotRecordIsLogged(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.failSQL("fail_prompted", "UPDATE OF last_prompt_at ON sessions")
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "x"); err != nil {
		t.Fatal(err)
	}
	e.logged(t, "claude-review", "disk I/O error")

	role := e.spec(RoleCodexReview)
	e.h.waitLine = "MAGNUM_DONE_r-1 0"
	if _, err := e.m.RunShell(e.ctx, e.pr, role, ws.Panes[RoleCodexReview], "x", DoneMarker("r-1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	e.logged(t, "codex-review", "disk I/O error")
}

// The fix prompt is one paragraph of facts: it opens with the fix skill's
// startup copy, then names the PR, its head and the branch to push to, the
// posted review, its file and the login that posted it, the report path with
// the run marker and the db-lock line. It carries no PR text: the title is
// never in it.
func TestFixPromptRendersTheSkillLinkAndFacts(t *testing.T) {
	e := newEnv(t)
	skill := filepath.Join(e.cfg.Layout.Home, "skills", "magnum-fix", "SKILL.md")
	writeFile(t, skill, "# fix skill\n", 0o600)
	if _, err := e.cfg.SnapshotPrompts(filepath.Join(t.TempDir(), "skill"), t0); err != nil {
		t.Fatal(err)
	}
	skillCopy := e.cfg.SkillFile(skill)
	if skillCopy == skill {
		t.Fatalf("the snapshot holds no copy of %s", skill)
	}
	role := Role(config.RoleClaudeFix)
	run, err := e.m.NewRun(e.ctx, e.pr, role, store.RunFix, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := FixPromptInput{
		HeadRef:       "feature/coupon-expiry",
		ReviewURL:     "https://github.com/talkable/talkable/pull/11920#pullrequestreview-4100000001",
		ReviewFile:    "/state/reviews/talkable/talkable/11920/abc1234/review.json",
		ReviewerLogin: "talkable[bot]",
		DBLockCommand: "/opt/magnum/bin/magnum db-lock --checkout /p/talkable.review3 --role claude-fix --",
	}
	got, err := e.m.FixPrompt(e.pr, role, run, in)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[$magnum-fix](" + skillCopy + ") Read " + skillCopy + " first"; !strings.HasPrefix(got, want) {
		t.Errorf("the fix prompt does not start with %q:\n%s", want, got)
	}
	for _, want := range []string{e.pr.URL, "`" + e.pr.HeadSHA + "`", "HEAD:" + in.HeadRef, in.ReviewURL, in.ReviewFile,
		"`" + in.ReviewerLogin + "`", store.Deref(run.ReportPath), ReportMarker(run.ID), "`" + in.DBLockCommand + " <command>`"} {
		if !strings.Contains(got, want) {
			t.Errorf("the fix prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, store.Deref(e.pr.Title)) {
		t.Errorf("the fix prompt carries the PR title:\n%s", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the fix prompt is not one paragraph:\n%s", got)
	}

	if _, err := e.m.FixPrompt(e.pr, RoleClaude, run, in); err == nil {
		t.Error("FixPrompt rendered the prompt of claude-review, which is no fix role")
	}
	noReport := run
	noReport.ReportPath = nil
	if _, err := e.m.FixPrompt(e.pr, role, noReport, in); err == nil {
		t.Error("FixPrompt rendered a prompt for a run without a report path")
	}
	// The prompt puts the branch, unquoted, into the push the fixer runs:
	// a branch name a shell would read is refused, and never quoted.
	unsafe := in
	unsafe.HeadRef = "fix$(touch x)"
	if got, err := e.m.FixPrompt(e.pr, role, run, unsafe); !errors.Is(err, ErrUnsafeHeadRef) || strings.Contains(err.Error(), unsafe.HeadRef) {
		t.Errorf("FixPrompt with branch %q = %q, %v; want ErrUnsafeHeadRef without the name", unsafe.HeadRef, got, err)
	}
}

// The fix skill is never rendered, and magnum reads what the fixer writes:
// the re-review classifies its replies (pipeline's classifyReply) by their
// first words and the fix job parses its report by its headings. The skill
// spells out each of them and carries no template action.
func TestTheFixSkillNamesTheRepliesAndTheReportMagnumReads(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"name: magnum-fix",
		`"Fixed in <sha7>: `, `"Already fixed in <sha7>"`, `"Fixed: the description now `, `"Not a bug: `, `"Won't fix: `,
		"\n## Pushed\n", "\n## Replied\n", "\n## Needs you\n",
		"resolveReviewThread", "pulls/{number}/comments/{comment_id}/replies",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "{{.") {
		t.Error("the fix skill carries a template action, but nothing renders it")
	}
}
