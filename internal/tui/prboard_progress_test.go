package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// runningRow is a PR whose round started at start, with roles.
func runningRow(start time.Time, roles ...RoleProgress) PRBoardRow {
	return PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 740, Title: "Speed up the grid", State: "reviewing",
		Progress: &RoundProgress{StartedAt: start, Roles: roles}}
}

// workingRole is a role whose run has worked since started.
func workingRole(role, label string, started time.Time) RoleProgress {
	return RoleProgress{Role: role, Label: label, Started: started, Working: true}
}

// The state cell of a round in flight says what it is doing and for how
// long: the label of the one role working, "reviewers" for several, "judge"
// for the judge (a delta check's too), and the time alone while no role
// works; whole minutes since the round started.
func TestBoardStateCellShowsTheStageAndTimeOfARunningRound(t *testing.T) {
	m, _, _ := newBoard(t, 220, 40, PRBoardOptions{})
	p := m.painter()
	start := boardNow.Add(-17*time.Minute - 40*time.Second)
	since := boardNow.Add(-10 * time.Minute)
	ended := RoleProgress{Role: "claude-review", Label: "claude", Started: since, Ended: boardNow.Add(-time.Minute)}
	judge := RoleProgress{Role: "codex-judge", Label: "cj", Judge: true, Started: since, Working: true}
	delta := runningRow(boardNow.Add(-4*time.Minute), judge)
	delta.RoundWhy = &RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4, Roles: []string{"codex-judge"}}
	done := runningRow(start, ended)
	done.State = "reviewed"

	for _, tc := range []struct {
		name string
		row  PRBoardRow
		want string // the cell after its pill
	}{
		{"one role working", runningRow(start, ended, workingRole("claude-simplify", "simplify", since)), " simplify · 17m"},
		{"several reviewers at once", runningRow(start, workingRole("claude-review", "claude", since), workingRole("codex-review", "codex", since)),
			" reviewers · 17m"},
		{"the judge", runningRow(boardNow.Add(-31*time.Minute), ended, judge), " judge · 31m"},
		{"a delta check", delta, " delta check · judge · 4m"},
		{"no role working", runningRow(start, ended), " 17m"},
		{"the readiness step of a round just started", runningRow(boardNow.Add(-20 * time.Second)), " 0m"},
		{"a round over an hour", runningRow(boardNow.Add(-65*time.Minute-30*time.Second), judge), " judge · 1h5m"},
		{"a round no longer running", done, ""},
	} {
		got := ansi.Strip(p.stateWaitCell(sanitizeRow(tc.row)).render(nil))
		pill := ansi.Strip(p.stateCell(rowState(tc.row)).render(nil))
		if got != pill+tc.want {
			t.Errorf("%s: state cell = %q, want %q", tc.name, got, pill+tc.want)
		}
	}
}

// On a narrow screen the state cell of a running round gives up its stage
// before its time, and the stage never outlives the time; a column dragged
// narrower than the time keeps the pill alone.
func TestBoardStateCellDropsTheStageBeforeTheTimeOnANarrowScreen(t *testing.T) {
	m, _, _ := newBoard(t, 220, 40, PRBoardOptions{})
	p := m.painter()
	run := sanitizeRow(runningRow(boardNow.Add(-17*time.Minute), workingRole("claude-simplify", "simplify", boardNow.Add(-5*time.Minute))))
	rows := append(boardRows(), run)
	for i := range rows {
		rows[i] = sanitizeRow(rows[i])
	}
	p.all = rows
	n := p.natural(rows)

	full, timeOnly := false, false
	for w := 400; w >= 40; w-- {
		lay := p.fit(n, w, prbWidths{})
		if lay.total() > w {
			break // the line is cut at the screen's edge from here on
		}
		line := ansi.Strip(p.rowLine(run, lay, w, false, false))
		switch {
		case strings.Contains(line, "simplify · 17m"):
			full = true
		case strings.Contains(line, "simplify"):
			t.Fatalf("width %d: the stage stays without its time: %q", w, line)
		case strings.Contains(line, "reviewing  17m") || strings.Contains(line, "reviewing 17m"):
			timeOnly = true
		default:
			t.Fatalf("width %d: the time is gone while the state column is whole: %q", w, line)
		}
	}
	if !full || !timeOnly {
		t.Errorf("widths showed the stage: %v, the time alone: %v; want both", full, timeOnly)
	}

	var dragged prbWidths
	dragged[colState] = ansi.StringWidth(ansi.Strip(p.stateCell(run.State).render(nil)))
	lay := p.fit(n, 300, dragged)
	line := ansi.Strip(p.rowLine(run, lay, 300, false, false))
	if !strings.Contains(line, "reviewing") || strings.Contains(line, "17m") || strings.Contains(line, "…") {
		t.Errorf("a state column dragged to the pill's width: %q", line)
	}
}

// The board's frame follows a running round's time by the minute: the
// cached frame is the one a fresh render draws, says 17m all through the
// round's 17th minute and 18m once it has passed.
func TestBoardFrameFollowsTheRoundTimeByTheMinute(t *testing.T) {
	now := boardNow
	rows := []PRBoardRow{runningRow(boardNow.Add(-17*time.Minute-10*time.Second),
		workingRole("claude-simplify", "simplify", boardNow.Add(-5*time.Minute)))}
	m := newPRBoardModel(context.Background(), &fakeBoardSource{rows: rows}, &fakeActions{},
		PRBoardOptions{Now: func() time.Time { return now }, SelfLogins: boardSelf})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 220, Height: 30})
	next, _ = next.(prBoardModel).Update(prbDataMsg{rows: rows})
	m = next.(prBoardModel)

	check := func(step, want string) {
		t.Helper()
		got := m.View().Content
		if fresh := uncachedBoard(m); got != fresh {
			t.Fatalf("%s: the cached frame differs from a fresh one:\n%s\n---\n%s", step, got, fresh)
		}
		mustContain(t, ansi.Strip(got), want)
	}
	check("17m10s", "simplify · 17m")
	before := m.View().Content
	if again := m.View().Content; again != before {
		t.Error("the frame changed with nothing new")
	}
	now = now.Add(40 * time.Second)
	check("17m50s", "simplify · 17m")
	now = now.Add(15 * time.Second)
	check("18m05s", "simplify · 18m")
}

// The dashboard's rounds line names each round's stage and time, and its
// cached header is drawn again only when a round's minute changes.
func TestDashboardRoundsLineFollowsEachRoundsMinute(t *testing.T) {
	now := dashNow
	d := dashData()
	d.Rounds.Progress = []*RoundProgress{{StartedAt: dashNow.Add(-17*time.Minute - 10*time.Second),
		Roles: []RoleProgress{workingRole("claude-simplify", "simplify", dashNow.Add(-5*time.Minute))}}}
	m := newDashboardModel(context.Background(), &fakeSource{data: d}, nil, DashboardOptions{Now: func() time.Time { return now }})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	next, _ = next.(dashboardModel).Update(dashDataMsg{data: d})
	m = next.(dashboardModel)

	mustContain(t, viewOf(m), "1/2 active (talkable#1 · simplify · 17m)")
	first := m.header(m.viewWidth())
	now = now.Add(40 * time.Second)
	if again := m.header(m.viewWidth()); &again[0] != &first[0] {
		t.Error("the header was drawn again within the round's minute")
	}
	mustContain(t, viewOf(m), "talkable#1 · simplify · 17m")
	now = now.Add(15 * time.Second)
	if again := m.header(m.viewWidth()); &again[0] == &first[0] {
		t.Error("the header was not drawn again when the round's minute changed")
	}
	mustContain(t, viewOf(m), "talkable#1 · simplify · 18m")

	d.Rounds.Progress = nil // a round whose progress is unknown keeps its plain label
	next, _ = m.Update(dashDataMsg{data: d})
	mustContain(t, viewOf(next.(dashboardModel)), "1/2 active (talkable#1)")
}

// The card of a PR whose round runs lists the round's roles one a line:
// when each started and ended and how long it took, a working one the time
// so far, a failed one in red, one without a run "not started yet".
func TestCardListsTheRunningRoundsTimelineByRole(t *testing.T) {
	start := boardNow.Add(-31 * time.Minute)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	clock := func(t time.Time) string { return t.Local().Format("15:04") }
	row := roundsRow()
	row.State = "reviewing"
	row.Progress = &RoundProgress{StartedAt: start, Roles: []RoleProgress{
		{Role: "claude-review", Label: "claude", Started: at(2 * time.Minute), Ended: at(15*time.Minute + 4*time.Second)},
		{Role: "codex-review", Label: "codex", Started: at(2 * time.Minute), Ended: at(9 * time.Minute), Failed: true},
		workingRole("claude-simplify", "simplify", at(2*time.Minute)),
		{Role: "codex-judge", Label: "judge", Judge: true},
	}}

	section := lastRoundSection(t, roundsCard(t, row, 100))
	var got []string
	for _, l := range section[1:] {
		got = append(got, strings.Join(strings.Fields(l), " "))
	}
	want := []string{
		"round started " + clock(start) + " · running 31m00s",
		"claude-review started " + clock(at(2*time.Minute)) + " · ended " + clock(at(15*time.Minute)) + " · 13m04s",
		"codex-review started " + clock(at(2*time.Minute)) + " · failed " + clock(at(9*time.Minute)) + " · 7m00s",
		"claude-simplify started " + clock(at(2*time.Minute)) + " · running 29m00s",
		"codex-judge not started yet",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("LAST ROUND of a running round:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	row.State = "reviewed" // the round ended: its stage timings come back
	section = lastRoundSection(t, roundsCard(t, row, 100))
	if s := strings.Join(section, "\n"); strings.Contains(s, "not started") || !strings.Contains(s, "total 34m00s") {
		t.Errorf("LAST ROUND of a finished round:\n%s", s)
	}
}
