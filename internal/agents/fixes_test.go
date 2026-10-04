package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Park closes the workspace of lost sessions too, so their surviving panes
// are checked: a build the user started there must not be killed.
func TestParkRefusesBusyPaneOfLostSession(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.removeAgent("mg-11920-codex-judge-5d01cf") // the judge exited; its shell remains
	e.clock.Add(LostGrace + time.Second)
	e.observe()
	if s := e.sessions()[0]; s.Role != string(RoleJudge) || s.State != store.SessionLost {
		t.Fatalf("judge session = %+v, want lost", s)
	}
	e.h.busy[ws.Panes[RoleJudge]] = true
	if err := e.m.Park(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if len(e.h.closed) != 0 {
		t.Fatalf("closed = %v, want nothing closed", e.h.closed)
	}
	e.h.busy[ws.Panes[RoleJudge]] = false
	if err := e.m.Park(e.ctx, e.pr); err != nil || len(e.h.closed) != 1 {
		t.Fatalf("idle again: err %v, closed %v", err, e.h.closed)
	}
}

// A live agent session whose agent is gone is checked like a shell pane.
func TestParkRefusesBusyPaneOfVanishedAgent(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.h.removeAgent("mg-11920-claude-review-5d01cf")
	e.h.busy[ws.Panes[RoleClaude]] = true
	if err := e.m.Park(e.ctx, e.pr); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

// A workspace created for a PR whose root pane cannot be recorded is closed:
// no session row would ever lead back to it.
func TestEnsureWorkspaceClosesUnrecordedWorkspace(t *testing.T) {
	e := newEnv(t)
	pr := e.pr
	pr.RepoID = 999 // the agent name cannot be computed
	if _, err := e.m.EnsureWorkspace(e.ctx, pr, "/Users/x/Projects/talkable.review1", nil, "l", e.coreRoles()); err == nil {
		t.Fatal("want an error")
	}
	if len(e.h.creates) != 1 || len(e.h.closed) != 1 || e.h.closed[0] != "w1" {
		t.Fatalf("creates %d, closed %v: want the new workspace closed", len(e.h.creates), e.h.closed)
	}
	if n := len(e.sessions()); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
}

// A conversation of another agent kind is never resumed for the role.
func TestResumeIDMatchesTheRolesKind(t *testing.T) {
	e := newEnv(t)
	add := func(kind, id string) {
		if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleClaude), AgentName: store.Ptr("x"),
			AgentKind: store.Ptr(kind), SessionID: store.Ptr(id), State: store.SessionParked}); err != nil {
			t.Fatal(err)
		}
	}
	add(KindClaude, "claude-old")
	add(KindCodex, "codex-newer") // the role ran on codex for a while
	if id, err := e.m.ResumeID(e.ctx, e.pr.ID, RoleClaude); err != nil || id != "claude-old" {
		t.Fatalf("ResumeID = %q, %v; want the claude conversation", id, err)
	}
	// herdr shows the codex conversation: Recover does not restore it either.
	e.h.addWorkspace("w9")
	e.h.addPane(herdr.Pane{ID: "w9:p1", WorkspaceID: "w9", Cwd: "/x"})
	e.h.addAgent(herdr.AgentInfo{PaneID: "w9:p1", WorkspaceID: "w9", Agent: KindCodex, AgentStatus: herdr.StatusIdle,
		AgentSession: &herdr.AgentSession{Kind: "id", Value: "codex-newer"}})
	got, err := e.m.Recover(e.ctx, e.pr)
	if err != nil || len(got) != 0 {
		t.Fatalf("Recover = %+v, %v; want nothing restored", got, err)
	}
}

func TestPreflightUsesTheEnvironment(t *testing.T) {
	e := newEnv(t)
	e.run.Rules = append(e.run.Rules, execx.Rule{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in\n")}})
	e.setKind(KindCodex, func(k *config.Kind) { k.Env = map[string]string{"CODEX_HOME": "/kind", "X": "k"} })
	role := e.spec(RoleJudge)
	role.Env = map[string]string{"CODEX_HOME": "/role"}
	if err := e.m.PreflightRole(e.ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Preflight(e.ctx, KindCodex); err != nil {
		t.Fatal(err)
	}
	calls := e.run.CallsWithPrefix("codex")
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", e.run.Calls)
	}
	if env := calls[0].Env; env["CODEX_HOME"] != "/role" || env["X"] != "k" {
		t.Fatalf("PreflightRole env = %v, want the role's env over the kind's", env)
	}
	if env := calls[1].Env; env["CODEX_HOME"] != "/kind" {
		t.Fatalf("Preflight env = %v, want the kind's env", env)
	}
	// A shell role is checked as its tool, with its env.
	shell := e.spec(RoleCodexReview)
	shell.Env = map[string]string{"CODEX_HOME": "/shell"}
	if err := e.m.PreflightRole(e.ctx, shell); err != nil {
		t.Fatal(err)
	}
	if calls := e.run.CallsWithPrefix("codex"); len(calls) != 3 || calls[2].Env["CODEX_HOME"] != "/shell" {
		t.Fatalf("shell role preflight = %+v", calls)
	}
}

// Run ids carry a random part: a review marker cannot be guessed from the
// time and a counter.
func TestNewRunIDs(t *testing.T) {
	e := newEnv(t)
	re := regexp.MustCompile(`^r-20261003T120000-[0-9a-f]{6}$`)
	seen := map[string]bool{}
	for range 5 {
		run, err := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunInitial, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(run.ID) || seen[run.ID] {
			t.Fatalf("run id %q (seen %v)", run.ID, seen)
		}
		seen[run.ID] = true
	}
}

// A prompt in flight when ctx ends may have arrived: the run is recorded as
// submitted (observed, never re-sent), not left pending to be abandoned.
func TestSubmitRecordsAPromptCutByCancellation(t *testing.T) {
	e := newEnv(t)
	e.started()
	ctx, cancel := context.WithCancel(e.ctx)
	e.h.onPrompt = func() { cancel() }
	e.h.errs["AgentPrompt"] = context.Canceled
	run, err := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunInitial, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.Submit(ctx, run, "go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if r := e.run1(run.ID); r.State != store.RunSubmitted || r.SubmittedAt == nil {
		t.Fatalf("run = %+v, want submitted", r)
	}
	if s := e.session(RoleJudge); s.LastPromptAt == nil {
		t.Fatalf("session = %+v, want last_prompt_at", s)
	}
	// ctx already over: nothing is sent and the run stays pending.
	run2, err := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunNudge, 1)
	if err != nil {
		t.Fatal(err)
	}
	n := len(e.h.prompts)
	if err := e.m.Submit(ctx, run2, "go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if r := e.run1(run2.ID); r.State != store.RunPending || len(e.h.prompts) != n {
		t.Fatalf("run = %+v, prompts %d -> %d", r, n, len(e.h.prompts))
	}
}

// Observe never takes another checkout's agent for a session's, even when
// the session's stored (pre-hash) name points at it.
func TestObserveIgnoresAgentInAnotherCheckout(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	legacy := "mg-talkable-11920-codex-judge"
	s := e.session(RoleJudge)
	if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) {
		u.Set("agent_name", legacy)
		u.Set("herdr_pane_id", "w9:p1")
	}); err != nil {
		t.Fatal(err)
	}
	e.h.removeAgent("mg-11920-codex-judge-5d01cf")
	e.h.addWorkspace("w9")
	e.h.addPane(herdr.Pane{ID: "w9:p1", WorkspaceID: "w9", Cwd: "/Users/x/Projects/talkable-frontend.review1"})
	e.h.addAgent(herdr.AgentInfo{Name: legacy, PaneID: "w9:p1", WorkspaceID: "w9", Agent: KindCodex, AgentStatus: herdr.StatusWorking})
	e.clock.Add(LostGrace + time.Second)
	if o := e.observe()[RoleJudge]; o.Kind != ObsLost {
		t.Fatalf("observation = %+v, want lost", o)
	}
	_ = ws
}

// The shell line's ending, run by a real shell: the marker lands on a line
// of its own (also after output without a newline) with the command's exit
// status, not tee's.
func TestShellLineEndingInRealShell(t *testing.T) {
	sh, err := exec.LookPath("zsh")
	if err != nil {
		if sh, err = exec.LookPath("bash"); err != nil {
			t.Skip("no zsh or bash")
		}
	}
	marker := DoneMarker("r-1")
	waitRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(marker) + `(?:[ \t]+\d+)?[ \t]*$`)
	for _, tc := range []struct {
		command string
		status  int
	}{
		{"printf 'report without newline'", 0},
		{"sh -c 'printf partial; exit 3'", 3},
	} {
		dir := t.TempDir()
		report := filepath.Join(dir, "out.md")
		role := config.Role{Name: "x", Kind: config.KindShell, Mode: config.ModeShell, Command: tc.command, Capture: config.CaptureStdout}
		line, err := ShellLine(role, ShellData{ReportPath: report, Marker: marker})
		if err != nil {
			t.Fatal(err)
		}
		out, _ := exec.Command(sh, "-c", line).CombinedOutput()
		m := waitRe.FindString(string(out))
		if m == "" {
			t.Fatalf("%s: no marker line in %q", tc.command, out)
		}
		if got := doneStatus(m, marker); got != tc.status {
			t.Fatalf("%s: status %d, want %d (output %q)", tc.command, got, tc.status, out)
		}
		if b, _ := os.ReadFile(report); len(b) == 0 || strings.Contains(string(b), marker) {
			t.Fatalf("%s: report %q", tc.command, b)
		}
		if anchor := CommandAnchor(marker); strings.Contains(string(out), anchor) || !strings.Contains(line, anchor) {
			t.Fatalf("anchor %q must appear in the typed line only", anchor)
		}
	}
}

func TestShellQuoteEqualsWord(t *testing.T) {
	for in, want := range map[string]string{"=codex": "'=codex'", "a=b": "a=b", "--x=1": "--x=1"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
