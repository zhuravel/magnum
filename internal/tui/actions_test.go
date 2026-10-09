package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// leaveCase drives one screen through the action lifecycle: start a
// long action (its command is never run, so it stays in flight), then
// press keys and feed messages.
type leaveCase struct {
	name  string
	start func(t *testing.T) (tea.Model, string) // a screen with an action in flight, and its label
}

func leaveCases() []leaveCase {
	return []leaveCase{
		{"dashboard", func(t *testing.T) (tea.Model, string) {
			m, _, _ := newDash(t, 140, 50)
			m, _ = send(t, m, keys("p")...)
			return m, m.busy
		}},
		{"board", func(t *testing.T) (tea.Model, string) {
			m, _, _ := newBoard(t, 140, 40, PRBoardOptions{})
			m, _ = send(t, m, keys("p")...)
			return m, m.busy
		}},
	}
}

// bar is the action state of either screen.
func bar(m tea.Model) actionBar {
	switch m := m.(type) {
	case dashboardModel:
		return m.actionBar
	case prBoardModel:
		return m.actionBar
	}
	return actionBar{}
}

func update(t *testing.T, m tea.Model, msgs ...tea.Msg) (tea.Model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, msg := range msgs {
		m, cmd = m.Update(msg)
	}
	return m, cmd
}

// TestLeavingWaitsForTheActionInFlight: q, esc, tab and a first ctrl+c
// must not stop a running action (the screen's context ends when it
// closes); the screen says it is finishing and closes once the action
// returns.
func TestLeavingWaitsForTheActionInFlight(t *testing.T) {
	for _, c := range leaveCases() {
		for _, k := range []string{"q", "esc", "tab", "ctrl+c"} {
			t.Run(c.name+"/"+k, func(t *testing.T) {
				m, what := c.start(t)
				if what == "" {
					t.Fatal("no action in flight")
				}
				m, cmd := update(t, m, keyMsg(k))
				if isQuit(execCmd(t, cmd)) {
					t.Fatalf("%s closed the screen while %s ran", k, what)
				}
				b := bar(m)
				if !b.leaving || b.switching != (k == "tab") {
					t.Fatalf("leaving %v switching %v after %s", b.leaving, b.switching, k)
				}
				v := viewOf(m)
				mustContain(t, v, "finishing "+what, "ctrl+c stops it now")

				// Other keys wait; actions are not started.
				m, cmd = update(t, m, keyMsg("j"), keyMsg("r"))
				if isQuit(execCmd(t, cmd)) || bar(m).confirm != nil {
					t.Fatal("a key during the wait acted")
				}

				m, cmd = update(t, m, actionDoneMsg{what: what, text: "ok"})
				if !isQuit(execCmd(t, cmd)) {
					t.Fatal("the screen did not close once the action returned")
				}
				if bar(m).switching != (k == "tab") {
					t.Fatalf("switching = %v after %s", bar(m).switching, k)
				}
			})
		}
	}
}

func TestLeavingCanBeCutShort(t *testing.T) {
	for _, c := range leaveCases() {
		t.Run(c.name+"/ctrl+c", func(t *testing.T) {
			m, _ := c.start(t)
			m, _ = update(t, m, keyMsg("q"))
			_, cmd := update(t, m, keyMsg("ctrl+c"))
			if !isQuit(execCmd(t, cmd)) {
				t.Fatal("ctrl+c during the wait did not close the screen")
			}
		})
		t.Run(c.name+"/grace", func(t *testing.T) {
			m, _ := c.start(t)
			m, _ = update(t, m, keyMsg("tab"))
			m, cmd := update(t, m, finishGraceMsg{})
			if !isQuit(execCmd(t, cmd)) || !bar(m).switching {
				t.Fatal("the grace did not close the screen to switch")
			}
		})
		t.Run(c.name+"/q then tab", func(t *testing.T) {
			m, _ := c.start(t)
			m, _ = update(t, m, keyMsg("q"), keyMsg("tab"))
			if !bar(m).switching {
				t.Fatal("tab during the wait should switch instead of quitting")
			}
		})
	}
}

func TestLeavingWithoutAnActionIsImmediate(t *testing.T) {
	d, _, _ := newDash(t, 140, 50)
	b, _, _ := newBoard(t, 140, 40, PRBoardOptions{})
	for _, m := range []tea.Model{d, b} {
		for _, k := range []string{"q", "ctrl+c"} {
			if _, cmd := update(t, m, keyMsg(k)); !isQuit(execCmd(t, cmd)) {
				t.Errorf("%T: %s did not quit at once", m, k)
			}
		}
		if _, cmd := update(t, m, finishGraceMsg{}); isQuit(execCmd(t, cmd)) {
			t.Errorf("%T: a stray grace message closed the screen", m)
		}
	}
}

// TestBusyLetsNavigationThrough: one action at a time, but a long one
// (open waits up to 5 minutes) must not freeze the screen: moving works,
// action keys are refused and the action in flight stays visible.
func TestBusyLetsNavigationThrough(t *testing.T) {
	m, _, _ := newDash(t, 140, 50)
	m, _ = send(t, m, keys("o")...)
	if m.busy != "open talkable#1" {
		t.Fatalf("busy = %q", m.busy)
	}
	mustContain(t, viewOf(m), "busy: open talkable#1…")
	m, _ = send(t, m, keys("j", "j")...)
	if m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2: navigation is blocked while busy", m.cursor)
	}
	m, _ = send(t, m, keys("p")...)
	v := viewOf(m)
	mustContain(t, v, "still running: open talkable#1")
	if strings.Count(v, "open talkable#1") != 1 {
		t.Errorf("the refusal repeats the action:\n%s", v)
	}
	m, _ = send(t, m, keys("w")...)
	if !m.showManual {
		t.Error("toggles must work while busy")
	}

	b, _, _ := newBoard(t, 140, 40, PRBoardOptions{})
	b, _ = send(t, b, keys("o")...)
	what, first := b.busy, b.selKey
	b, _ = send(t, b, keys("j", "s", "?")...)
	if b.selKey == first || b.sort == SortUpdated || b.mode != prbHelp {
		t.Fatalf("selected %q sort %v mode %v: board keys blocked while busy", b.selKey, b.sort, b.mode)
	}
	b, _ = send(t, b, keys("esc", "x")...)
	if b.confirm != nil {
		t.Fatal("x asked while busy")
	}
	mustContain(t, viewOf(b), "still running: "+what)
}

// TestScreenLoadsHaveADeadline: a hung source must turn into "refresh
// failed" instead of stopping every later refresh.
func TestScreenLoadsHaveADeadline(t *testing.T) {
	var got []context.Context
	dash := testDashboard(context.Background(), SourceFunc(func(ctx context.Context) (StatusData, error) {
		got = append(got, ctx)
		return StatusData{}, nil
	}), nil, DashboardOptions{})
	board := testPRBoard(context.Background(), PRBoardSourceFunc(func(ctx context.Context) ([]PRBoardRow, error) {
		got = append(got, ctx)
		return nil, nil
	}), nil, PRBoardOptions{})
	dash.gatherCmd()()
	board.loadCmd()()
	for i, ctx := range got {
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) > loadTimeout {
			t.Errorf("load %d: deadline %v (set %v), want within %v", i, dl, ok, loadTimeout)
		}
	}
	if len(got) != 2 {
		t.Fatalf("%d loads ran", len(got))
	}
}

// TestAKeyQueuedBehindAQuitStartsNothing: Bubble Tea hands a screen the
// keys it already read before it handles tea.Quit, and RunDashboard and
// RunPRBoard cancel the screen's context as they return, so a y typed right
// after ctrl+c (with a question up), or a question and its y typed after q,
// esc or tab, must start no action.
func TestAKeyQueuedBehindAQuitStartsNothing(t *testing.T) {
	screens := []struct {
		name string
		open func(t *testing.T) (tea.Model, *fakeActions, []string) // a screen, its actions and the keys that ask
	}{
		{"dashboard", func(t *testing.T) (tea.Model, *fakeActions, []string) {
			t.Helper()
			m, _, act := newDash(t, 140, 50)
			return m, act, []string{"j", "j", "r"} // review talkable#7
		}},
		{"board", func(t *testing.T) (tea.Model, *fakeActions, []string) {
			t.Helper()
			m, _, act := newBoard(t, 140, 40, PRBoardOptions{})
			m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{{Ref: "talkable/talkable#5", Owner: "talkable", Repo: "talkable",
				Number: 5, State: "queued", GHState: "OPEN", ActivityAt: boardNow}}})
			return m, act, []string{"r"} // review talkable#5
		}},
	}
	for _, s := range screens {
		for _, quit := range []string{"ctrl+c", "q", "esc", "tab"} {
			for _, asked := range []bool{true, false} {
				if asked && quit != "ctrl+c" {
					continue // only ctrl+c leaves while a question is up; another key answers it
				}
				t.Run(s.name+"/"+quit+"/asked="+strconv.FormatBool(asked), func(t *testing.T) {
					m, act, ask := s.open(t)
					if asked {
						m, _ = update(t, m, keys(ask...)...)
						if bar(m).confirm == nil {
							t.Fatalf("%v did not ask", ask)
						}
					}
					m, cmd := update(t, m, keyMsg(quit))
					if !isQuit(execCmd(t, cmd)) {
						t.Fatalf("%s did not quit", quit)
					}
					queued := []string{"y"}
					if !asked {
						queued = append(slices.Clone(ask), "y")
					}
					for _, k := range queued {
						var msgs []tea.Msg
						m, cmd = update(t, m, keyMsg(k))
						if msgs = execCmd(t, cmd); hasMsg[actionDoneMsg](msgs) {
							t.Fatalf("%s queued behind %s ran an action: %v", k, quit, act.list())
						}
					}
					b := bar(m)
					if calls := act.list(); len(calls) != 0 || b.busy != "" || b.confirm != nil {
						t.Fatalf("keys behind %s: calls %v, busy %q, question %v", quit, calls, b.busy, b.confirm)
					}
					if b.switching != (quit == "tab") {
						t.Errorf("switching = %v after %s", b.switching, quit)
					}
				})
			}
		}
	}
}
