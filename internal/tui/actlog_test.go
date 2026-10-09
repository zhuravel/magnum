package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// refresh feeds a screen one refresh tick and the re-read of the pending
// requests it starts; it returns the model and the follow-up messages.
func refresh[M tea.Model](t *testing.T, m M, tick tea.Msg) (M, []tea.Msg) {
	t.Helper()
	m, cmd := send(t, m, tick)
	var follow []tea.Msg
	for _, msg := range execCmd(t, cmd) {
		if rm, ok := msg.(requestsMsg); ok {
			var next tea.Cmd
			m, next = send(t, m, rm)
			follow = append(follow, execCmd(t, next)...)
		}
	}
	return m, follow
}

// rowLineOf is the board's line for the PR ref (owner/repo#N) in view.
func rowLineOf(t *testing.T, view, number string) string {
	t.Helper()
	return lineWith(t, view, number)
}

// TestPendingRequestMarksItsRowUntilTheAnswerFlashes: an action whose
// request the daemon has not answered when the action returns flashes as
// pending, not as a success, and marks its row; the screen re-reads the
// request on every refresh and flashes the daemon's answer when it comes,
// then drops the mark.
func TestPendingRequestMarksItsRowUntilTheAnswerFlashes(t *testing.T) {
	m, _, act := newBoard(t, 160, 24, PRBoardOptions{})
	act.queued = []Request{{ID: 47, Kind: "pin", State: RequestPending}}
	m, _ = boardAct(t, m, "p")
	v := viewOf(m)
	mustContain(t, v, "pin talkable/magnum#42: request 47 (pin) is queued; the daemon has not answered yet")
	mustNotContain(t, v, "✔ pin talkable/magnum#42 ok")
	if l := rowLineOf(t, v, "#42"); !strings.HasPrefix(l, "▌"+newGlyphs(IconsUnicode).queued) {
		t.Fatalf("the pending row lacks its mark: %q", l)
	}
	if l := rowLineOf(t, v, "#11931"); strings.Contains(l, "◷") {
		t.Fatalf("another row is marked: %q", l)
	}

	// The daemon has not answered at the next refresh: nothing flashes.
	act.answers = map[int64]Request{47: {ID: 47, Kind: "pin", State: RequestPending}}
	m, _ = refresh(t, m, prbTickMsg{})
	if len(act.asked) != 1 || !slices.Equal(act.asked[0], []int64{47}) {
		t.Fatalf("asked %v, want [[47]]", act.asked)
	}
	mustContain(t, viewOf(m), "◷")

	act.answers = map[int64]Request{47: {ID: 47, Kind: "pin", State: RequestDone, Result: "pinned review1"}}
	m, _ = refresh(t, m, prbTickMsg{})
	v = viewOf(m)
	mustContain(t, v, "✔ pin talkable/magnum#42: pinned review1")
	mustNotContain(t, v, "◷")
	m, _ = refresh(t, m, prbTickMsg{})
	if len(act.asked) != 2 {
		t.Errorf("a settled request was asked again: %v", act.asked)
	}
}

// TestRefusedRequestStaysRedUntilAKey: the daemon's refusal of a request a
// screen followed stays on screen in red past the flash's time, until a key
// is pressed; the key still does what it does.
func TestRefusedRequestStaysRedUntilAKey(t *testing.T) {
	m, _, act := newDash(t, 160, 40)
	act.queued = []Request{{ID: 3, Kind: "pin", State: RequestPending}}
	m, _ = dashAct(t, m, "p")
	act.answers = map[int64]Request{3: {ID: 3, Kind: "pin", State: RequestFailed, Result: "slot review1 is pinned (magnum unpin)"}}
	m, _ = refresh(t, m, dashTickMsg{})
	want := "pin talkable#1: request 3 (pin) failed: slot review1 is pinned (magnum unpin)"
	mustContain(t, viewOf(m), want)
	if !m.flashErr {
		t.Error("the refusal is not drawn as an error")
	}
	m, _ = send(t, m, flashExpireMsg{seq: m.flashSeq})
	mustContain(t, viewOf(m), want)
	m, _ = send(t, m, keyMsg("j"))
	mustNotContain(t, viewOf(m), "slot review1 is pinned")
	if m.cursor != 1 {
		t.Errorf("cursor %d: the key that cleared the failure did not move", m.cursor)
	}
}

// TestFailedActionStaysUntilAKey: an action that fails (the CLI's error)
// keeps its red line on screen until a key is pressed.
func TestFailedActionStaysUntilAKey(t *testing.T) {
	m, _, act := newDash(t, 160, 40)
	act.err = errors.New("talkable#1 is not open (released, GitHub MERGED)")
	m, _ = dashAct(t, m, "p")
	m, _ = send(t, m, flashExpireMsg{seq: m.flashSeq})
	mustContain(t, viewOf(m), "pin talkable#1: talkable#1 is not open (released, GitHub MERGED)")
	m, _ = send(t, m, keyMsg("?"))
	m, _ = send(t, m, keyMsg("?"))
	mustNotContain(t, viewOf(m), "is not open")
}

// TestLongFailurePointsAtTheLog: a failure wider than the footer is cut
// with a pointer to the action log, which shows it whole.
func TestLongFailurePointsAtTheLog(t *testing.T) {
	long := "the daemon refused it: " + strings.Repeat("a long reason that goes on ", 8) + "END"
	for _, c := range []struct {
		name string
		view func() (tea.Model, func(tea.Model) tea.Model)
	}{
		{"board", func() (tea.Model, func(tea.Model) tea.Model) {
			m, _, act := newBoard(t, 100, 24, PRBoardOptions{})
			act.err = errors.New(long)
			m, _ = boardAct(t, m, "p")
			return m, func(x tea.Model) tea.Model { b, _ := send(t, x.(prBoardModel), keyMsg(logKey)); return b }
		}},
		{"dashboard", func() (tea.Model, func(tea.Model) tea.Model) {
			m, _, act := newDash(t, 100, 40)
			act.err = errors.New(long)
			m, _ = dashAct(t, m, "p")
			return m, func(x tea.Model) tea.Model { d, _ := send(t, x.(dashboardModel), keyMsg(logKey)); return d }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, open := c.view()
			v := viewOf(m.(interface{ View() tea.View }))
			mustContain(t, v, "(! shows all)")
			mustNotContain(t, v, "END")
			if w := maxLineWidth(v); w > 100 {
				t.Errorf("a line is %d cells wide", w)
			}
			v = viewOf(open(m).(interface{ View() tea.View }))
			mustContain(t, v, "Action log", "END")
		})
	}
}

// TestActionLogKeepsTheLastOutcomesWhole: ! shows the last 20 outcomes,
// newest first, each with its time, its requests and everything it printed;
// ! closes it.
func TestActionLogKeepsTheLastOutcomesWhole(t *testing.T) {
	m, _, act := newBoard(t, 160, 60, PRBoardOptions{})
	for i := range 22 {
		act.queued = []Request{{ID: int64(100 + i), Kind: "pin", State: RequestDone, Result: "pinned"}}
		m, _ = boardAct(t, m, "p")
	}
	if entries, _ := m.log.snapshot(); len(entries) != actionLogSize {
		t.Fatalf("the log keeps %d outcomes, want %d", len(entries), actionLogSize)
	}
	m, _ = send(t, m, keyMsg(logKey))
	v := viewOf(m)
	mustContain(t, v, "Action log (the last 20, newest first)", "12:00:00", "pin talkable/magnum#42",
		"request 121 (pin) done", "progress line", "pin talkable/magnum#42 ok")
	mustNotContain(t, v, "request 101 (pin)")
	if strings.Index(v, "request 121") > strings.Index(v, "request 120") {
		t.Error("the newest outcome is not first")
	}
	m, _ = send(t, m, keyMsg(logKey))
	if m.mode != prbTable {
		t.Fatalf("! did not close the log: mode %v", m.mode)
	}

	d, _, _ := newDash(t, 140, 50)
	d, _ = send(t, d, keyMsg(logKey))
	mustContain(t, viewOf(d), "Action log", "no actions yet")
	d, _ = send(t, d, keyMsg("esc"))
	if d.showLog {
		t.Fatal("esc did not close the log")
	}
}

// TestActionLogFollowsAcrossScreens: the dashboard and the board share one
// log, so a request queued on the dashboard (repo#N) marks the board's row
// (owner/repo#N) and is followed there.
func TestActionLogFollowsAcrossScreens(t *testing.T) {
	log := NewActionLog()
	log.add(logEntry{what: "review talkable#11931", target: "talkable#11931", reqs: []Request{{ID: 9, Kind: "review", State: RequestPending}}})
	m, _, act := newBoard(t, 160, 24, PRBoardOptions{Log: log})
	if l := rowLineOf(t, viewOf(m), "#11931"); !strings.Contains(l, "◷") {
		t.Fatalf("the row of the request queued on the dashboard is not marked: %q", l)
	}
	act.answers = map[int64]Request{9: {ID: 9, Kind: "review", State: RequestDone, Result: "reviewing talkable#11931 now"}}
	m, _ = refresh(t, m, prbTickMsg{})
	mustContain(t, viewOf(m), "review talkable#11931: reviewing talkable#11931 now")
}

func TestSameTarget(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"talkable#7", "talkable/talkable#7", true},
		{"talkable/talkable#7", "TALKABLE/talkable#7", true},
		{"talkable#7", "talkable#8", false},
		{"example/talkable#7", "talkable/talkable#7", false},
		{"review1", "review1", true},
		{"review1", "talkable#1", false},
		{"", "", false},
	} {
		if got := sameTarget(c.a, c.b); got != c.want {
			t.Errorf("sameTarget(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// TestDashboardMarksThePendingRowSelectedOrNot: the dashboard's mark sits in
// the cell after the cursor's, on the selected row and on the others, and
// the selected row keeps its highlight.
func TestDashboardMarksThePendingRowSelectedOrNot(t *testing.T) {
	m, _, act := newDash(t, 160, 40)
	act.queued = []Request{{ID: 5, Kind: "pin", State: RequestPending}}
	m, _ = dashAct(t, m, "p") // on review1, which holds talkable#1
	v := viewOf(m)
	if l := lineWith(t, v, "review1"); !strings.HasPrefix(l, "›◷") {
		t.Errorf("the selected pending row: %q", l)
	}
	if l := lineWith(t, v, "Bump rails"); !strings.HasPrefix(l, " ◷") { // the queue row of talkable#1
		t.Errorf("the queue row of the same PR: %q", l)
	}
	m, _ = send(t, m, keyMsg("j"))
	v = viewOf(m)
	if l := lineWith(t, v, "review1"); !strings.HasPrefix(l, " ◷") {
		t.Errorf("the pending row unselected: %q", l)
	}
	if l := lineWith(t, v, "review2"); !strings.HasPrefix(l, "› ") {
		t.Errorf("the selected row: %q", l)
	}
	raw := m.View().Content
	if !strings.Contains(lineWith(t, raw, "review2"), "review2") || ansi.Strip(lineWith(t, raw, "review2")) == lineWith(t, raw, "review2") {
		t.Error("the selected row lost its highlight")
	}
}
