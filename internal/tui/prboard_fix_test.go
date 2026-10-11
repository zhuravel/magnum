package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A PR whose fixer works says so in its state cell, "fixing 1/3 ·
// claude-fix" (a narrow cell drops the role), and on its card; GitHub's
// merge outranks it. A PR switched out of autofix carries "fix off" among
// its tags beside "muted", on the board and on the card.
func TestTheBoardCellReadsFixing(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	fixing := PRBoardRow{Ref: "talkable/talkable#2", Owner: "talkable", Repo: "talkable", Number: 2, State: "reviewed", GHState: "OPEN",
		Title: "Guard the owner", Fixing: "fixing 1/3 · claude-fix"}
	if cell := strings.TrimSpace(ansi.Strip(p.stateWaitCell(fixing).render(nil))); cell != "fixing 1/3 · claude-fix" {
		t.Errorf("state cell = %q", cell)
	}
	if cell := strings.TrimSpace(ansi.Strip(p.cells(fixing, [3]int{}).stateFit(len(" fixing 1/3 ")).render(nil))); cell != "fixing 1/3" {
		t.Errorf("narrow state cell = %q", cell)
	}
	if card := ansi.Strip(strings.Join(p.cardContent(fixing, 160), "\n")); !strings.Contains(card, "fixing 1/3 · claude-fix") {
		t.Errorf("the card does not say the fixer works:\n%s", card)
	}
	merged := fixing
	merged.GHState = "MERGED"
	if cell := ansi.Strip(p.stateWaitCell(merged).render(nil)); strings.Contains(cell, "fixing") {
		t.Errorf("a merged PR's state cell = %q", cell)
	}
	waiting := PRBoardRow{State: "reviewed", GHState: "OPEN"}
	if cell := ansi.Strip(p.stateWaitCell(waiting).render(nil)); strings.Contains(cell, "fixing") {
		t.Errorf("a PR without a fixer at work: state cell = %q", cell)
	}

	off := fixing
	off.Fixing, off.Muted, off.FixOff = "", true, true
	if title := ansi.Strip(p.titleCell(off).render(nil)); !strings.Contains(title, "muted fix off Guard the owner") {
		t.Errorf("title cell = %q", title)
	}
	if card := ansi.Strip(strings.Join(p.cardContent(off, 160), "\n")); !strings.Contains(card, "fix off") {
		t.Errorf("the card lacks fix off:\n%s", card)
	}
	if title := ansi.Strip(p.titleCell(fixing).render(nil)); strings.Contains(title, "fix off") {
		t.Errorf("a PR with autofix on: title cell = %q", title)
	}
}

// fixBoard is a board of one reviewed PR (talkable#5, one finding) edited by
// edit, with the cursor on it.
func fixBoard(t *testing.T, edit func(r *PRBoardRow)) (prBoardModel, *fakeActions) {
	t.Helper()
	m, _, act := newBoard(t, 170, 40, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(edit)}})
	return m, act
}

// f asks before it has the PR's findings fixed, as you; only y sends the
// request, and any other key (enter too) cancels.
func TestFAsksBeforeFixing(t *testing.T) {
	const want = "Fix the findings of talkable#5 as you?"
	m, act := fixBoard(t, nil)
	m, cmd := send(t, m, keyMsg("f"))
	if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 {
		t.Fatalf("f alone ran %v", act.calls)
	}
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("f asked %+v, want %q", m.confirm, want)
	}
	mustContain(t, viewOf(m), want+" y/N", "y confirms, any other key cancels")
	for _, no := range []string{"n", "esc", "enter", "x"} {
		if c, _ := send(t, m, keyMsg(no)); len(act.calls) != 0 || c.confirm != nil {
			t.Errorf("%s acted or left the question up: %v", no, act.calls)
		}
	}
	m, follow := boardAct(t, m, "y")
	if !slices.Equal(act.calls, []string{"fix talkable/talkable#5"}) || !hasMsg[prbDataMsg](follow) {
		t.Errorf("y called %v (refresh %v), want the fix of the row's ref", act.calls, hasMsg[prbDataMsg](follow))
	}
}

// f says, before asking anything, why there is nothing to fix; the daemon's
// refusal is not the first the user sees.
func TestFRefusesBeforeAsking(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(r *PRBoardRow)
		want string
	}{
		{"unreviewed", func(r *PRBoardRow) { r.Findings = nil }, "magnum has not reviewed this PR: nothing to fix"},
		{"no findings", func(r *PRBoardRow) {
			r.Findings = &FindingsInfo{Verdict: "clean", Posted: "APPROVE", SHA: "abcdef1234"}
		},
			"talkable#5: magnum's review of abcdef1 found nothing to fix"},
	} {
		m, act := fixBoard(t, c.edit)
		m, _ = send(t, m, keyMsg("f"))
		if m.confirm != nil || len(act.calls) != 0 {
			t.Errorf("%s: f asked %+v, ran %v", c.name, m.confirm, act.calls)
		}
		mustContain(t, viewOf(m), c.want)
	}
}

// F takes a PR out of autofix and puts it back: the label, the question and
// the call follow the row's flag, and only y sends it.
func TestShiftFTogglesFixOff(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(r *PRBoardRow)
		label string
		ask   string
		call  string
	}{
		{"autofix on", nil, "fix off", "Leave talkable#5's findings to you (no autofix)?", "fix --off talkable/talkable#5"},
		{"autofix off", func(r *PRBoardRow) { r.FixOff = true }, "fix on", "Autofix talkable#5 again?", "fix --on talkable/talkable#5"},
		{"autofix on, unreviewed", func(r *PRBoardRow) { r.Findings = nil }, "fix off", "Leave talkable#5's findings to you (no autofix)?",
			"fix --off talkable/talkable#5"},
	} {
		m, act := fixBoard(t, c.edit)
		var label string
		for _, it := range m.menuItems() {
			if it.key == "F" {
				label = it.label
				if !it.ok {
					t.Errorf("%s: the menu dims F", c.name)
				}
			}
		}
		if label != c.label {
			t.Errorf("%s: the menu entry of F reads %q, want %q", c.name, label, c.label)
		}
		m, cmd := send(t, m, keyMsg("F"))
		if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 {
			t.Fatalf("%s: F alone ran %v", c.name, act.calls)
		}
		if m.confirm == nil || m.confirm.question != c.ask {
			t.Fatalf("%s: F asked %+v, want %q", c.name, m.confirm, c.ask)
		}
		if want := c.call; m.confirm.what != want {
			t.Errorf("%s: the action is named %q, want %q", c.name, m.confirm.what, want)
		}
		mustContain(t, viewOf(m), c.ask+" y/N")
		if n, _ := send(t, m, keyMsg("enter")); n.confirm != nil || len(act.calls) != 0 {
			t.Errorf("%s: enter acted or left the question up: %v", c.name, act.calls)
		}
		if _, follow := boardAct(t, m, "y"); !slices.Equal(act.calls, []string{c.call}) || !hasMsg[prbDataMsg](follow) {
			t.Errorf("%s: y called %v, want %q", c.name, act.calls, c.call)
		}
	}
}

// The help, the card's actions and the right-click menu name both keys.
func TestTheBoardNamesTheFixKeys(t *testing.T) {
	m, _ := fixBoard(t, nil)
	card, _ := send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(card), "f fix findings", "F fix off")
	help, _ := send(t, m, keyMsg("?"))
	mustContain(t, viewOf(help), "f / F", "fix findings as you / fix off or on (asks y/N)")
	off, _ := fixBoard(t, func(r *PRBoardRow) { r.FixOff = true })
	card, _ = send(t, off, keyMsg("enter"))
	mustContain(t, viewOf(card), "F fix on")
}

// The dashboard asks the same questions and calls the same actions; it does
// not know a PR's flag, so F there only takes a PR out of autofix.
func TestDashboardFixKeysAsk(t *testing.T) {
	data := dashData()
	data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
	for _, c := range []struct {
		key, ask, call string
	}{
		{"f", "Fix the findings of talkable#7 as you?", "fix talkable#7"},
		{"F", "Leave talkable#7's findings to you (no autofix)?", "fix --off talkable#7"},
	} {
		m, src, act := newDash(t, 220, 50)
		m, _ = send(t, m, dashDataMsg{data: data})
		m, cmd := send(t, m, keys("j", "j", c.key)...) // talkable#7, reviewed
		if len(execCmd(t, cmd)) != 0 || len(act.calls) != 0 || m.confirm == nil || m.confirm.question != c.ask {
			t.Fatalf("%s asked %+v (calls %v), want %q", c.key, m.confirm, act.calls, c.ask)
		}
		mustContain(t, viewOf(m), c.ask+" y/N")
		if n, _ := send(t, m, keyMsg("enter")); n.confirm != nil || len(act.calls) != 0 {
			t.Errorf("%s then enter acted or kept asking: %v", c.key, act.calls)
		}
		before := src.count()
		if _, follow := dashAct(t, m, "y"); !slices.Equal(act.calls, []string{c.call}) || !hasMsg[dashDataMsg](follow) || src.count() != before+1 {
			t.Errorf("%s then y called %v, want %q and a refresh", c.key, act.calls, c.call)
		}
	}
	m, _, _ := newDash(t, 220, 50)
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "fix the PR's findings as you (asks y/N)", "no autofix (asks y/N)")
}

// K reaches a PR with a fix loop in either phase although the PR stays
// reviewed while the fixer works (PRBoardRow.FixLoop; Fixing names only the
// fixing phase): it asks, and y sends the abort that stops the fixer or ends
// the loop. A reviewed PR without a loop is still refused before asking.
func TestKAbortsAFixLoop(t *testing.T) {
	for _, edit := range []func(r *PRBoardRow){
		func(r *PRBoardRow) { r.FixLoop, r.Fixing = true, "fixing 1/3 · claude-fix" },
		func(r *PRBoardRow) { r.FixLoop = true }, // waiting for its next cycle
	} {
		m, act := fixBoard(t, edit)
		m, _ = send(t, m, keyMsg("K"))
		if m.confirm == nil || len(act.calls) != 0 {
			t.Fatalf("K on a PR with a fix loop asked %+v, ran %v", m.confirm, act.calls)
		}
		if _, _ = boardAct(t, m, "y"); !slices.Equal(act.calls, []string{"abort talkable/talkable#5"}) {
			t.Errorf("y called %v, want the abort of the row's ref", act.calls)
		}
	}
	m, act := fixBoard(t, nil)
	m, _ = send(t, m, keyMsg("K"))
	if m.confirm != nil || len(act.calls) != 0 {
		t.Errorf("K on a reviewed PR without a loop asked %+v, ran %v", m.confirm, act.calls)
	}
	mustContain(t, viewOf(m), "no review of talkable#5 is running or queued (reviewed)")
}

// The dashboard knows a PR's fix loop from the status gather, on its slot's
// row (where a PR whose fixer works shows: it is reviewed, so in no queue)
// and on a queue row: K asks there and y sends the abort.
func TestDashboardKAbortsAFixLoop(t *testing.T) {
	data := dashData()
	data.Slots = []SlotRow{{Name: "review3", Folder: "~/Projects/talkable.review3", PRRef: "talkable#5", PRState: "reviewed",
		State: "busy", SlotState: "busy", DBs: "-", Disk: "4.0G", PRFixLoop: true}}
	data.Queue = []PRRow{{Ref: "talkable#7", Title: "Fix the referral widget", State: "reviewed", Next: "watching for new pushes", FixLoop: true}}
	for _, c := range []struct {
		keys []string
		call string
	}{
		{[]string{"K", "y"}, "abort talkable#5"},
		{[]string{"j", "K", "y"}, "abort talkable#7"},
	} {
		m, src, act := newDash(t, 220, 50)
		src.data = data
		m, _ = send(t, m, dashDataMsg{data: data})
		if _, _ = dashAct(t, m, c.keys...); !slices.Equal(act.calls, []string{c.call}) {
			t.Errorf("%v called %v, want %q", c.keys, act.calls, c.call)
		}
	}
	data.Slots[0].PRFixLoop = false
	m, _, act := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data}, keyMsg("K"))
	if m.confirm != nil || len(act.calls) != 0 {
		t.Errorf("K on a reviewed PR without a loop asked %+v, ran %v", m.confirm, act.calls)
	}
}

// The dashboard's F follows the PR's opt-out of autofix as the board's does:
// on a row switched out it reads "fix on", asks to autofix the PR again and
// sends --on.
func TestDashboardFTogglesFixOff(t *testing.T) {
	data := dashData()
	data.Slots = []SlotRow{{Name: "review3", Folder: "~/Projects/talkable.review3", PRRef: "talkable#5", PRState: "reviewed",
		State: "busy", SlotState: "busy", DBs: "-", Disk: "4.0G", PRFixOff: true}}
	data.Queue = []PRRow{{Ref: "talkable#7", Title: "Fix the referral widget", State: "reviewed", Next: "watching for new pushes"}}
	for _, c := range []struct {
		keys             []string
		label, ask, call string
	}{
		{nil, "fix on", "Autofix talkable#5 again?", "fix --on talkable#5"},
		{[]string{"j"}, "fix off", "Leave talkable#7's findings to you (no autofix)?", "fix --off talkable#7"},
	} {
		m, src, act := newDash(t, 220, 50)
		src.data = data
		m, _ = send(t, m, dashDataMsg{data: data})
		m, _ = send(t, m, keys(c.keys...)...)
		label := ""
		for _, it := range m.menuItems() {
			if it.key == "F" {
				label = it.label
			}
		}
		if label != c.label {
			t.Errorf("%v: the menu entry of F reads %q, want %q", c.keys, label, c.label)
		}
		m, _ = send(t, m, keyMsg("F"))
		if m.confirm == nil || m.confirm.question != c.ask {
			t.Fatalf("%v: F asked %+v, want %q", c.keys, m.confirm, c.ask)
		}
		if _, _ = dashAct(t, m, "y"); !slices.Equal(act.calls, []string{c.call}) {
			t.Errorf("%v: y called %v, want %q", c.keys, act.calls, c.call)
		}
	}
}
