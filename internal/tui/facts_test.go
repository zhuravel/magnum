package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// allFacts is a daemon with every fact a title says: an older build, a
// pause holding requests, a drain and the Codex budget's pace (boardNow is
// Saturday 12:00).
func allFacts() DaemonFacts {
	return DaemonFacts{
		SkewOld: "v1.4.0", SkewNew: "v1.5.0 built", SkewSince: boardNow.Add(-(18*time.Hour + 20*time.Minute)),
		Paused: true, PausedSince: boardNow.Add(-19 * time.Hour), Held: 6,
		Draining: true, DrainerPID: 4242,
		Codex: &CodexPace{Used: 51, Cap: 80, At: time.Date(2026, 10, 6, 13, 30, 0, 0, time.Local)},
	}
}

// boardWithFacts is a board w x h whose source reports facts with its rows.
func boardWithFacts(t *testing.T, w, h int, facts DaemonFacts) prBoardModel {
	t.Helper()
	m, _, _ := newBoard(t, w, h, PRBoardOptions{Facts: func(context.Context) DaemonFacts { return facts }})
	m, _ = send(t, m, m.loadCmd()())
	return m
}

// TestBoardTitleSaysWhatHoldsTheDaemon: the board's title line (no new
// line) names an older build, a drain, a pause with the requests it holds
// and the Codex pace; a narrow screen keeps their short forms, the most
// pressing first, and nothing grows past the width.
func TestBoardTitleSaysWhatHoldsTheDaemon(t *testing.T) {
	wide := boardWithFacts(t, 400, 24, allFacts())
	v := viewOf(wide)
	title := strings.Split(v, "\n")[0]
	for _, want := range []string{
		"daemon on v1.4.0 since Fri 17:40 · v1.5.0 built: daemon-restart", "draining (pid 4242)",
		"paused 19h · 6 requests held", "codex 51% · at this pace 80% Tue 13:30", "magnum · pull requests", "↻ 12:00:00",
	} {
		if !strings.Contains(title, want) {
			t.Errorf("title lacks %q: %q", want, title)
		}
	}

	plain, _, _ := newBoard(t, 160, 24, PRBoardOptions{})
	narrow := boardWithFacts(t, 160, 24, allFacts())
	nv, pv := viewOf(narrow), viewOf(plain)
	nl, pl := strings.Split(nv, "\n"), strings.Split(pv, "\n")
	if len(nl) != len(pl) || len(nl) != 24 {
		t.Fatalf("the facts changed the frame's height: %d lines, %d without", len(nl), len(pl))
	}
	for i := 1; i < len(nl)-2; i++ { // every line but the title and the footer is as without facts
		if nl[i] != pl[i] {
			t.Errorf("line %d changed:\n%q\n%q", i, nl[i], pl[i])
		}
	}
	mustContain(t, nl[0], "v1.5.0 built: daemon-restart", "draining", "paused 19h")
	mustNotContain(t, nl[0], "daemon on v1.4.0")
	if w := maxLineWidth(nv); w > 160 {
		t.Errorf("a line is %d cells wide", w)
	}
	if l := rowLineOf(t, nv, "#42"); !strings.HasPrefix(l, "▌") {
		t.Errorf("the cursor row moved: %q", l)
	}
}

// TestBoardTitleRedrawsWhenTheFactsChange: the frame cache keys on the
// facts, so a pause lifted between two loads leaves the title.
func TestBoardTitleRedrawsWhenTheFactsChange(t *testing.T) {
	f := allFacts()
	m := boardWithFacts(t, 400, 24, f)
	mustContain(t, viewOf(m), "paused 19h")
	f.Paused = false
	m, _ = send(t, m, prbDataMsg{rows: boardRows(), facts: f})
	mustNotContain(t, viewOf(m), "paused 19h")
	mustContain(t, viewOf(m), "draining (pid 4242)")
}

// TestDashboardTitleSaysWhatHoldsTheDaemon: the dashboard's title line
// carries the same facts, with the scroll position; the header keeps its
// lines and the selected row its highlight.
func TestDashboardTitleSaysWhatHoldsTheDaemon(t *testing.T) {
	plain, _, _ := newDash(t, 400, 50)
	data := dashData()
	data.Facts = allFacts()
	data.Facts.SkewSince, data.Facts.PausedSince = dashNow.Add(-(18*time.Hour + 20*time.Minute)), dashNow.Add(-19*time.Hour)
	m := newDashboardModel(context.Background(), &fakeSource{data: data}, &fakeActions{}, DashboardOptions{Now: func() time.Time { return dashNow }})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 400, Height: 50}, dashDataMsg{data: data}, keyMsg("j"))
	plain, _ = send(t, plain, keyMsg("j"))
	v, pv := viewOf(m), viewOf(plain)
	title := strings.Split(v, "\n")[0]
	mustContain(t, title, "magnum status · updated 3s ago", "v1.5.0 built: daemon-restart", "draining (pid 4242)",
		"paused 19h · 6 requests held", "codex 51% · at this pace 80%")
	vl, pl := strings.Split(v, "\n"), strings.Split(pv, "\n")
	if len(vl) != len(pl) {
		t.Fatalf("the facts changed the height: %d lines, %d without", len(vl), len(pl))
	}
	for i := 1; i < len(vl); i++ {
		if vl[i] != pl[i] {
			t.Errorf("line %d changed:\n%q\n%q", i, vl[i], pl[i])
		}
	}
	if l := lineWith(t, v, "review2"); !strings.HasPrefix(l, cursorMark) {
		t.Errorf("the selected row moved: %q", l)
	}

	narrow := m
	narrow, _ = send(t, narrow, tea.WindowSizeMsg{Width: 120, Height: 50})
	nt := strings.Split(viewOf(narrow), "\n")[0]
	mustContain(t, nt, "magnum status · updated 3s ago")
	if w := maxLineWidth(viewOf(narrow)); w > 120 {
		t.Errorf("a line is %d cells wide", w)
	}
}

// TestFactsGiveWayInOrder: the variants run from every fact whole to none,
// the short forms before any fact is left out, the least pressing first.
func TestFactsGiveWayInOrder(t *testing.T) {
	facts := allFacts().list(boardNow)
	if len(facts) != 4 || !strings.HasPrefix(facts[0].full, "daemon on") || facts[3].short != "codex 80% Tue 13:30" {
		t.Fatalf("facts %+v", facts)
	}
	vs := defaultStyles.factVariants(facts, "   ")
	if len(vs) != 6 || vs[len(vs)-1] != "" {
		t.Fatalf("%d variants", len(vs))
	}
	if !strings.Contains(vs[1], "codex 80% Tue 13:30") || strings.Contains(vs[4], "draining") || !strings.Contains(vs[4], "v1.5.0 built") {
		t.Errorf("variants %q", vs)
	}
	if (DaemonFacts{}).list(boardNow) != nil {
		t.Error("no facts said something")
	}
	if got := (DaemonFacts{Paused: true}).list(boardNow)[0].full; got != "paused" {
		t.Errorf("a pause of unknown age: %q", got)
	}
}
