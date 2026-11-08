package agents

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestTitle(t *testing.T) {
	cases := []struct {
		repo   string
		number int
		role   Role
		want   string
	}{
		{"talkable", 11920, RoleJudge, "PR #11920 codex-judge - talkable"},
		{"talkable/talkable", 11920, RoleClaude, "PR #11920 claude-review - talkable"},
		{"talkable", 11920, RoleSimplify, "PR #11920 claude-simplify - talkable"},
		{"talkable", 11920, RoleCodexReview, "PR #11920 codex-review - talkable"},
		{"zhuravel/magnum", 7, RoleJudge, "PR #7 codex-judge - magnum"},
		{"", 42, RoleCodexReview, "PR #42 codex-review"},
	}
	for _, tc := range cases {
		if got := Title(tc.repo, tc.number, tc.role); got != tc.want {
			t.Errorf("Title(%q, %d, %s) = %q, want %q", tc.repo, tc.number, tc.role, got, tc.want)
		}
	}
	// Pane labels are unchanged: Title without a repo.
	if got := TaggedPaneLabel("", 11920, RoleCodexReview); got != "PR #11920 codex-review" {
		t.Errorf("TaggedPaneLabel = %q", got)
	}
}

func TestShellLineTitlePrefix(t *testing.T) {
	role := defaultRole(t, RoleCodexReview)
	got, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/codex.md", Marker: DoneMarker("r-1"),
		Title: Title("talkable", 11920, RoleCodexReview)})
	if err != nil {
		t.Fatal(err)
	}
	want := `printf '\033]0;%s\007' 'PR #11920 codex-review - talkable'; DISABLE_AUTO_TITLE=true; set -o pipefail; ` +
		`{ printf '<!-- magnum:run=r-1 -->\n'; magnum db-lock --role codex-review --run r-1 -- codex review -c model_reasoning_effort=high --base origin/master; } | tee /tmp/codex.md; printf '\nMAGNUM_DONE_r-1 %d\n' "$?"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !strings.HasSuffix(got, `; printf '\n`+DoneMarker("r-1")+` %d\n' "$?"`) {
		t.Fatalf("marker must stay last: %s", got)
	}
	// Quotes in a title stay inside the printf argument.
	got, err = ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/codex.md", Marker: DoneMarker("r-1"), Title: "it's #1"})
	if err != nil || !strings.HasPrefix(got, `printf '\033]0;%s\007' 'it'\''s #1'; `) {
		t.Fatalf("quoted title = %q, %v", got, err)
	}
	if _, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/codex.md", Marker: DoneMarker("r-1"), Title: "a\nb"}); err == nil {
		t.Fatal("title with a newline: want error")
	}
}

const judgeTitle = "PR #11920 codex-judge - talkable"

// setAgentTitle sets the terminal title herdr reports for an agent.
func (f *fakeHerdr) setAgentTitle(name, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.agents {
		if f.agents[i].Name == name {
			f.agents[i].TerminalTitle = "⠋ " + title
			f.agents[i].TerminalTitleStripped = title
		}
	}
}

// renames returns the `/rename` commands typed into pane.
func (e *env) renames(pane string) []string {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	var out []string
	for _, r := range e.h.runs {
		if r.Pane == pane && strings.HasPrefix(r.Command, "/rename") {
			out = append(out, r.Command)
		}
	}
	return out
}

func TestNameJudgeOnSubmitAndObserve(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	pane := ws.Panes[RoleJudge]
	const judge = "mg-11920-codex-judge-5d01cf"
	e.h.setAgentTitle(judge, "talkable.review1")

	// Submit sees the judge working with another title: rename right away.
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatal(err)
	}
	if got := e.renames(pane); len(got) != 1 || got[0] != "/rename "+judgeTitle {
		t.Fatalf("renames after submit = %q", got)
	}

	// The rename stuck: ticks cost nothing.
	e.h.setAgentTitle(judge, judgeTitle+" | talkable.review1")
	e.clock.Add(30 * time.Second)
	e.observe()
	if got := e.renames(pane); len(got) != 1 {
		t.Fatalf("renames with a matching title = %q", got)
	}

	// Codex names the thread itself a few seconds into the turn: rename again.
	e.h.setAgentTitle(judge, "Review the PR | talkable.review1")
	e.clock.Add(30 * time.Second)
	e.observe()
	if got := e.renames(pane); len(got) != 2 || got[1] != "/rename "+judgeTitle {
		t.Fatalf("renames after codex renamed the thread = %q", got)
	}

	// Claude roles are named by --name, never by /rename.
	if got := e.renames(ws.Panes[RoleClaude]); len(got) != 0 {
		t.Fatalf("claude renames = %q", got)
	}
}

func TestNameJudgeStopsAfterFiveAttempts(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	pane := ws.Panes[RoleJudge]
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatal(err)
	}
	if got := e.renames(pane); len(got) != 1 {
		t.Fatalf("renames after submit = %q", got)
	}
	// At most one attempt per observe tick: Submit used tick 0.
	e.m.nameAgent(e.ctx, e.session(RoleJudge), pane, "")
	if got := e.renames(pane); len(got) != 1 {
		t.Fatalf("second attempt in the same tick = %q", got)
	}
	for range 8 {
		e.clock.Add(30 * time.Second)
		e.observe()
		e.m.nameAgent(e.ctx, e.session(RoleJudge), pane, "") // same tick: no-op
	}
	if got := e.renames(pane); len(got) != TitleAttempts {
		t.Fatalf("renames = %d, want %d: %q", len(got), TitleAttempts, got)
	}

	// A new session row (restart) gets a fresh budget.
	if err := e.m.markLost(e.ctx, e.session(RoleJudge)); err != nil {
		t.Fatal(err)
	}
	e.h.removePane(pane)
	ws = e.workspace()
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatal(err)
	}
	if got := e.renames(ws.Panes[RoleJudge]); len(got) != 1 {
		t.Fatalf("renames in the new generation = %q", got)
	}
}

func TestNameJudgeNeverWhenBlockedOrIdle(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	pane := ws.Panes[RoleJudge]
	const judge = "mg-11920-codex-judge-5d01cf"
	atStart := len(e.renames(pane)) // a fresh codex thread is named right after start

	// Acked as blocked: no rename on submit, nor on ticks while blocked.
	e.h.promptStatus = herdr.StatusBlocked
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Prompt = %v, want ErrBlocked", err)
	}
	for i := range 2 {
		e.clock.Add(30 * time.Second)
		if o := e.observe()[RoleJudge]; o.Kind != ObsBlocked {
			t.Fatalf("tick %d = %+v", i, o)
		}
	}
	if got := e.renames(pane); len(got) != atStart {
		t.Fatalf("renames while blocked = %q", got[atStart:])
	}

	// The dialog is gone and the turn works: rename.
	e.h.setAgentStatus(judge, herdr.StatusWorking)
	e.clock.Add(30 * time.Second)
	e.observe()
	if got := e.renames(pane); len(got) != atStart+1 {
		t.Fatalf("renames once working = %q", got)
	}

	// Idle (the turn ended): no rename, the composer may hold a draft.
	e.h.setAgentStatus(judge, herdr.StatusIdle)
	for range 3 {
		e.clock.Add(30 * time.Second)
		e.observe()
	}
	if got := e.renames(pane); len(got) != atStart+1 {
		t.Fatalf("renames while idle = %q", got)
	}
}

func TestNameJudgeSkipsHumanTurns(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	const judge = "mg-11920-codex-judge-5d01cf"
	atStart := len(e.renames(ws.Panes[RoleJudge]))
	e.clock.Add(3 * time.Minute)
	e.h.setAgentStatus(judge, herdr.StatusWorking) // a human prompted it: no magnum run
	if o := e.observe()[RoleJudge]; o.Kind != ObsHumanActive {
		t.Fatalf("tick = %+v", o)
	}
	if got := e.renames(ws.Panes[RoleJudge]); len(got) != atStart {
		t.Fatalf("renames during a human turn = %q", got[atStart:])
	}
}

// A fresh codex thread is named as soon as it starts idle (it would show as
// "Codex" until its first turn otherwise); a resumed thread keeps its name.
func TestNameJudgeRightAfterAFreshStart(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	pane := ws.Panes[RoleJudge]
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), pane, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.renames(pane); len(got) != 1 || got[0] != "/rename PR #11920 codex-judge - talkable" {
		t.Fatalf("renames after a fresh start = %q", got)
	}
	e2 := newEnv(t)
	ws2 := e2.workspace()
	if err := e2.m.StartAgent(e2.ctx, e2.pr, e2.spec(RoleJudge), ws2.Panes[RoleJudge], "01a0-uuid"); err != nil {
		t.Fatal(err)
	}
	if got := e2.renames(ws2.Panes[RoleJudge]); len(got) != 0 {
		t.Fatalf("renames after a resume = %q", got)
	}
}

// ownerWriter adds a claude role named fx-writer, as a factory's writer role
// is, to e's configuration.
func (e *env) ownerWriter() config.Role {
	e.t.Helper()
	r := e.spec(RoleClaude)
	r.Name = "fx-writer"
	return e.addRole(r)
}

// ownerWorkspace lays out the panes of the fx-writer and codex-judge roles of
// the test PR in the standard checkout.
func (e *env) ownerWorkspace(writer config.Role) Workspace {
	e.t.Helper()
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1",
		map[string]string{"WT_BRANCH": "review1"}, "talkable#11920", []config.Role{writer, e.spec(RoleJudge)})
	if err != nil {
		e.t.Fatalf("EnsureWorkspace: %v", err)
	}
	return ws
}

// A factory names its agents after the task or the ticket they work for, not
// after a pull request: Deps.OwnerName replaces "PR #N" in the pane label,
// the title a claude agent gets by --name and the /rename a codex agent is
// given.
func TestTitlesUseTheOwnerNameWhenGiven(t *testing.T) {
	e := newEnv(t)
	e.m.d.Tag = "factory"
	e.m.d.OwnerName = func(store.PR) string { return "PR-12345" }
	writer := e.ownerWriter()
	ws := e.ownerWorkspace(writer)

	if got := e.h.renames[ws.Panes["fx-writer"]]; got != "factory PR-12345 fx-writer" {
		t.Fatalf("pane label = %q", got)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, writer, ws.Panes["fx-writer"], ""); err != nil {
		t.Fatal(err)
	}
	if len(e.h.starts) != 1 {
		t.Fatalf("starts = %+v", e.h.starts)
	}
	args := e.h.starts[0].Args
	if i := slices.Index(args, "--name"); i < 0 || args[i+1] != "factory PR-12345 fx-writer - talkable" {
		t.Fatalf("claude args = %q, want --name factory PR-12345 fx-writer - talkable", args)
	}
	// What herdr addresses the agent by keeps the number of the row.
	if got, want := e.h.starts[0].Name, TaggedAgentName("factory", "talkable/talkable", 11920, "fx-writer"); got != want {
		t.Fatalf("agent name = %q, want %q", got, want)
	}

	// Codex has no --name: the namer types /rename with the same title.
	judge := ws.Panes[RoleJudge]
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), judge, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.renames(judge); len(got) != 1 || got[0] != "/rename factory PR-12345 codex-judge - talkable" {
		t.Fatalf("codex renames = %q", got)
	}
}

// The pane that replaces a closed one (a dead launch cleared) is labelled
// like the first, with the owner's name.
func TestTitlesUseTheOwnerNameForAReplacementPane(t *testing.T) {
	e := newEnv(t)
	e.m.d.Tag = "factory"
	e.m.d.OwnerName = func(store.PR) string { return "task 12" }
	ws := e.workspace()
	dead := ws.Panes[RoleJudge]
	e.h.addAgent(herdr.AgentInfo{Name: TaggedAgentName("factory", "talkable/talkable", 11920, RoleJudge), PaneID: dead,
		WorkspaceID: ws.WorkspaceID, Agent: KindCodex, AgentStatus: herdr.StatusUnknown, LaunchPending: true})
	e.h.reads[dead] = "codex-cli 0.163.0\nError: the TUI could not start\n% "

	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), dead, ""); err != nil {
		t.Fatal(err)
	}
	repl := e.h.splits[len(e.h.splits)-1].New
	if got := e.h.renames[repl]; got != "factory task 12 codex-judge" {
		t.Fatalf("replacement label = %q", got)
	}
}

// Without Deps.OwnerName, and when it has no name for a PR, the agents are
// named after the pull request, as magnum's reviews always were.
func TestTitlesKeepPRNumbersByDefault(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	if got := e.h.renames[ws.Panes[RoleClaude]]; got != "PR #11920 claude-review" {
		t.Fatalf("pane label = %q", got)
	}
	args := e.h.starts[1].Args
	if i := slices.Index(args, "--name"); i < 0 || args[i+1] != "PR #11920 claude-review - talkable" {
		t.Fatalf("claude args = %q", args)
	}
	if got := e.renames(ws.Panes[RoleJudge]); len(got) != 1 || got[0] != "/rename "+judgeTitle {
		t.Fatalf("codex renames = %q", got)
	}

	// A tag leads the label; the number stays when the owner has no name.
	tagged := newEnv(t)
	tagged.m.d.Tag = "eval"
	tagged.m.d.OwnerName = func(store.PR) string { return "" }
	tws := tagged.workspace()
	if got := tagged.h.renames[tws.Panes[RoleClaude]]; got != "eval PR #11920 claude-review" {
		t.Fatalf("tagged pane label = %q", got)
	}
}

// The review daemon observes every few seconds now: a rename that has not
// shown in the title yet is typed again only titleRetry after the last
// (the factory's own clock, 10 s, and the tick are slower than that), so
// the five attempts are not spent, and five /rename lines typed into a
// working Codex, within half a minute.
func TestNameJudgeWaitsTitleRetryBetweenAttempts(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	pane := ws.Panes[RoleJudge]
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatal(err)
	}
	if got := e.renames(pane); len(got) != 1 {
		t.Fatalf("renames after submit = %q", got)
	}
	e.clock.Add(titleRetry / 2)
	e.observe()
	if got := e.renames(pane); len(got) != 1 {
		t.Fatalf("renames %s after the last = %q, want none more", titleRetry/2, got)
	}
	e.clock.Add(titleRetry / 2)
	e.observe()
	if got := e.renames(pane); len(got) != 2 {
		t.Fatalf("renames %s after the last = %q, want a second", titleRetry, got)
	}
}
