package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	snzOpen   = "talkable/talkable#11950" // rereview_pending, not snoozed
	snzMerged = "talkable/talkable#11800" // released, merged
)

// snoozeBoard is a board of boardRows with the snooze of #11950 set to
// until (zero: none), the cursor on ref.
func snoozeBoard(t *testing.T, ref string, until time.Time) (prBoardModel, *fakeActions) {
	t.Helper()
	rows := boardRows()
	for i := range rows {
		if rows[i].Number == 11950 && !until.IsZero() {
			rows[i].SnoozedUntil, rows[i].SnoozedAt, rows[i].SnoozedBy = until, boardNow.Add(-2*time.Minute), "the board"
		}
	}
	src := &fakeBoardSource{rows: rows}
	act := &fakeActions{}
	m := testPRBoard(context.Background(), src, act, PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 220, Height: 50}, prbDataMsg{rows: src.rows})
	at := slices.Index(boardRefs(m), ref)
	if at < 0 {
		t.Fatalf("%s is not on the board: %v", ref, boardRefs(m))
	}
	m.moveTo(at)
	return m, act
}

// z asks before it snoozes the PR for 2h, says until when, and only y
// sends the snooze.
func TestPRBoardZSnoozesThePRAfterYes(t *testing.T) {
	const want = "Snooze talkable#11950 for 2h: no automatic review starts on it until 14:00 (r and review requests still run)?"
	m, act := snoozeBoard(t, snzOpen, time.Time{})
	m, cmd := send(t, m, keyMsg("z"))
	if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 {
		t.Fatalf("z alone ran %v", act.calls)
	}
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("z asked %+v, want %q", m.confirm, want)
	}
	mustContain(t, viewOf(m), want+" y/N")
	for _, no := range []string{"n", "esc", "enter"} {
		if c2, _ := send(t, m, keyMsg(no)); len(act.calls) != 0 || c2.confirm != nil {
			t.Errorf("%s acted or left the question up: %v", no, act.calls)
		}
	}
	if _, follow := boardAct(t, m, "y"); !slices.Equal(act.calls, []string{"snooze " + snzOpen + " 2h0m0s"}) || !hasMsg[prbDataMsg](follow) {
		t.Errorf("y called %v (refresh %v), want the 2h snooze", act.calls, hasMsg[prbDataMsg](follow))
	}
}

// z on a snoozed PR offers to lift the snooze.
func TestPRBoardZOnASnoozedPROffersToLiftIt(t *testing.T) {
	const want = "Lift the snooze of talkable#11950 (until 13:30): automatic reviews start again?"
	m, act := snoozeBoard(t, snzOpen, boardNow.Add(90*time.Minute))
	m, _ = send(t, m, keyMsg("z"))
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("z asked %+v, want %q", m.confirm, want)
	}
	if _, follow := boardAct(t, m, "y"); !slices.Equal(act.calls, []string{"unsnooze " + snzOpen}) || !hasMsg[prbDataMsg](follow) {
		t.Errorf("y called %v, want the unsnooze", act.calls)
	}
}

// z on a PR GitHub merged says there is nothing to snooze, asking nothing.
func TestPRBoardZRefusesAMergedPR(t *testing.T) {
	m, act := snoozeBoard(t, snzMerged, time.Time{})
	m, _ = send(t, m, keyMsg("z"))
	if m.confirm != nil || len(act.calls) != 0 {
		t.Fatalf("z on a merged PR asked %+v, ran %v", m.confirm, act.calls)
	}
	mustContain(t, viewOf(m), "talkable#11800 is merged: nothing to snooze")
}

// A snoozed PR says so in its state cell when nothing else waits there
// ("snoozed → 13:30"), and its card says until when, since when, by whom
// and the key that lifts it; ACTIONS offers z either way.
func TestPRBoardShowsTheSnooze(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, boardNow.Add(90*time.Minute))
	p := m.painter()
	until := boardNow.Add(90 * time.Minute)
	reviewed := PRBoardRow{State: "reviewed", GHState: "OPEN", SnoozedUntil: until}
	if cell := ansi.Strip(p.stateWaitCell(reviewed).render(nil)); !strings.HasSuffix(cell, " snoozed → 13:30") {
		t.Errorf("reviewed, snoozed: state cell = %q", cell)
	}
	waiting := PRBoardRow{State: "rereview_pending", GHState: "OPEN", SnoozedUntil: until, Wait: "re-review · snoozed → 13:30"}
	if cell := ansi.Strip(p.stateWaitCell(waiting).render(nil)); !strings.HasSuffix(cell, " snoozed → 13:30") || strings.Count(cell, "snoozed") != 1 {
		t.Errorf("waiting, snoozed: state cell = %q", cell)
	}
	ended := PRBoardRow{State: "reviewed", GHState: "OPEN", SnoozedUntil: boardNow.Add(-time.Minute)}
	if cell := ansi.Strip(p.stateWaitCell(ended).render(nil)); strings.Contains(cell, "snoozed") {
		t.Errorf("a snooze that ended shows: %q", cell)
	}

	r, _ := m.selected()
	card := ansi.Strip(strings.Join(p.cardContent(r, 160), "\n"))
	mustContain(t, card, "Snoozed until 13:30", "set 11:58 by the board", "z lifts it", "lift snooze")
	open := boardRows()[5] // #11950 without a snooze
	if c := ansi.Strip(strings.Join(p.cardContent(open, 160), "\n")); strings.Contains(c, "Snoozed") || !strings.Contains(c, "snooze 2h") {
		t.Errorf("an open PR's card:\n%s", c)
	}
}

// The help names z.
func TestPRBoardHelpNamesTheSnoozeKey(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "x / z", "release (asks y/N) / snooze 2h or lift it")
}
