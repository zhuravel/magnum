package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// whiteboardBoard is a board of one reviewed PR (talkable#5) with a
// whiteboard of its head, edited by edit, with the cursor on it.
func whiteboardBoard(t *testing.T, edit func(r *PRBoardRow)) (prBoardModel, *fakeActions) {
	t.Helper()
	return fixBoard(t, func(r *PRBoardRow) {
		r.Whiteboard = &PRWhiteboard{Session: "wb-5", Head: "abcdef1234"}
		if edit != nil {
			edit(r)
		}
	})
}

// w asks before it opens the PR's whiteboard in Whiteboard Desktop, naming
// the head the whiteboard describes; only y sends the call, and any other
// key (enter too) cancels.
func TestWAsksBeforeOpeningTheWhiteboard(t *testing.T) {
	const want = "Open the whiteboard of talkable#5 (abcdef1) in Whiteboard Desktop?"
	m, act := whiteboardBoard(t, nil)
	m, cmd := send(t, m, keyMsg("w"))
	if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 {
		t.Fatalf("w alone ran %v", act.calls)
	}
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("w asked %+v, want %q", m.confirm, want)
	}
	if m.confirm.what != "whiteboard --open talkable/talkable#5" {
		t.Errorf("the action is named %q", m.confirm.what)
	}
	mustContain(t, viewOf(m), want+" y/N", "y confirms, any other key cancels")
	for _, no := range []string{"n", "esc", "enter", "x"} {
		if c, _ := send(t, m, keyMsg(no)); len(act.calls) != 0 || c.confirm != nil {
			t.Errorf("%s acted or left the question up: %v", no, act.calls)
		}
	}
	m, follow := boardAct(t, m, "y")
	if !slices.Equal(act.calls, []string{"whiteboard --open talkable/talkable#5"}) || !hasMsg[prbDataMsg](follow) {
		t.Errorf("y called %v (refresh %v), want the whiteboard of the row's ref opened", act.calls, hasMsg[prbDataMsg](follow))
	}
}

// A whiteboard of an earlier head still opens: the question names that
// head, not the PR's.
func TestWNamesTheHeadTheWhiteboardDescribes(t *testing.T) {
	m, act := whiteboardBoard(t, func(r *PRBoardRow) { r.HeadSHA = "9999999aaa" })
	m, _ = send(t, m, keyMsg("w"))
	if m.confirm == nil || m.confirm.question != "Open the whiteboard of talkable#5 (abcdef1) in Whiteboard Desktop?" || len(act.calls) != 0 {
		t.Fatalf("w asked %+v, ran %v", m.confirm, act.calls)
	}
}

// w says at once, without asking, that the PR has no whiteboard yet; the
// menu dims it.
func TestWRefusesARowWithoutAWhiteboard(t *testing.T) {
	m, act := fixBoard(t, nil)
	m, _ = send(t, m, keyMsg("w"))
	if m.confirm != nil || len(act.calls) != 0 {
		t.Fatalf("w asked %+v, ran %v", m.confirm, act.calls)
	}
	mustContain(t, viewOf(m), "no whiteboard of talkable#5 yet (magnum whiteboard writes one)")
	for _, it := range m.menuItems() {
		if it.key == "w" && it.ok {
			t.Error("the menu offers w on a PR without a whiteboard")
		}
	}
}

// The right-click menu's entry runs through the same question as the key.
func TestTheMenuOpensTheWhiteboardThroughTheQuestion(t *testing.T) {
	m, act := whiteboardBoard(t, nil)
	var found bool
	for _, it := range m.menuItems() {
		if it.key == "w" {
			found = true
			if it.label != "open whiteboard" || !it.ok {
				t.Errorf("the menu entry of w is %q (ok %v)", it.label, it.ok)
			}
		}
	}
	if !found {
		t.Fatal("the menu lacks w")
	}
	m.menu = ctxMenu{rowKey: m.selKey, open: true}
	m, _ = send(t, m, keyMsg("w"))
	if m.confirm == nil || !strings.HasPrefix(m.confirm.question, "Open the whiteboard of talkable#5") || len(act.calls) != 0 {
		t.Fatalf("the menu's w asked %+v, ran %v", m.confirm, act.calls)
	}
}

// The card says which head the whiteboard describes among the PR's flags
// and offers w; a PR without one says nothing of it. The help names the
// key.
func TestTheCardAndTheHelpNameTheWhiteboard(t *testing.T) {
	m, _ := whiteboardBoard(t, nil)
	p := m.painter()
	card := ansi.Strip(strings.Join(p.cardContent(actPR(func(r *PRBoardRow) {
		r.Whiteboard = &PRWhiteboard{Session: "wb-5", Head: "abcdef1234"}
	}), 160), "\n"))
	if !strings.Contains(card, "whiteboard abcdef1") {
		t.Errorf("the card does not name the whiteboard:\n%s", card)
	}
	if card := ansi.Strip(strings.Join(p.cardContent(actPR(nil), 160), "\n")); strings.Contains(card, "whiteboard") {
		t.Errorf("a PR without a whiteboard: the card says\n%s", card)
	}
	open, _ := send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(open), "whiteboard abcdef1", "w open whiteboard")
	help, _ := send(t, m, keyMsg("?"))
	mustContain(t, viewOf(help), "w      open the PR's whiteboard (asks y/N)")
}
