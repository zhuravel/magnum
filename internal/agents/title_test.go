package agents

import (
	"errors"
	"strings"
	"testing"
	"time"

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
		`{ printf '<!-- magnum:run=r-1 -->\n'; command codex review -c model_reasoning_effort=high --base origin/master; } | tee /tmp/codex.md; printf '\nMAGNUM_DONE_r-1 %d\n' "$?"`
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
