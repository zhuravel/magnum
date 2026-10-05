package tui

import (
	"slices"
	"testing"
)

// On the overview M follows the board: on a PR GitHub merged before its last
// push was reviewed it dismisses the merged-unreviewed flag, on one whose
// flag is dismissed it restores it, on any other merged or closed PR it says
// there is nothing to mute, and on an open PR it asks as it always did.

// muteMergedData is dashData with the queue after talkable#1 (reviewing, open)
// holding a merged unreviewed PR (#7, its row) and four more. The cursor
// starts on slot review1 (talkable#1); j moves to review2, then down the queue.
func muteMergedData() StatusData {
	data := dashData()
	data.Queue[0].State, data.Queue[0].GHState, data.Queue[0].MergedUnreviewed = "closed", "MERGED", true // talkable#7
	data.Queue = append(data.Queue,
		PRRow{Ref: "talkable#9", State: "released", GHState: "MERGED", FlagDismissed: true},
		PRRow{Ref: "talkable#10", State: "released", GHState: "merged"},
		PRRow{Ref: "talkable#11", State: "released", GHState: "CLOSED"},
	)
	return data
}

// muteMergedDash is a dashboard that received muteMergedData with the cursor
// on queue row i (0 = talkable#7).
func muteMergedDash(t *testing.T, data StatusData, down ...string) (dashboardModel, *fakeActions) {
	t.Helper()
	m, _, act := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	m, _ = send(t, m, keys(down...)...)
	return m, act
}

func TestDashboardMuteOnAFlaggedMergedPRAsksToDismissTheFlag(t *testing.T) {
	const want = "Dismiss the merged-unreviewed flag on talkable#7? (r still runs a post-merge review)"
	m, act := muteMergedDash(t, muteMergedData(), "j", "j")
	m, _ = send(t, m, keyMsg("M"))
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("M asked %+v, want %q", m.confirm, want)
	}
	mustContain(t, viewOf(m), want+" y/N")
	if len(act.calls) != 0 {
		t.Fatalf("asking ran %v", act.calls)
	}
	if _, follow := dashAct(t, m, "y"); !slices.Equal(act.calls, []string{"mute talkable#7"}) || !hasMsg[dashDataMsg](follow) {
		t.Errorf("y called %v, want the mute request and a refresh", act.calls)
	}
	act.calls = nil
	for _, no := range []string{"n", "esc", "enter"} {
		c, _ := send(t, m, keyMsg(no))
		if len(act.calls) != 0 || c.busy != "" || c.confirm != nil {
			t.Errorf("%s acted or left the question up: %v", no, act.calls)
		}
	}
}

func TestDashboardMuteOnADismissedMergedPRAsksToRestoreTheFlag(t *testing.T) {
	const want = "Restore the merged-unreviewed flag on talkable#9?"
	m, act := muteMergedDash(t, muteMergedData(), "j", "j", "j", "j") // talkable#1, then #9
	m, _ = send(t, m, keyMsg("M"))
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("M asked %+v, want %q", m.confirm, want)
	}
	mustContain(t, viewOf(m), want+" y/N")
	if _, follow := dashAct(t, m, "y"); !slices.Equal(act.calls, []string{"unmute talkable#9"}) || !hasMsg[dashDataMsg](follow) {
		t.Errorf("y called %v, want the unmute request and a refresh", act.calls)
	}
	act.calls = nil
	if c, _ := send(t, m, keyMsg("n")); len(act.calls) != 0 || c.confirm != nil {
		t.Errorf("n acted: %v", act.calls)
	}
}

func TestDashboardMuteOnAPlainMergedOrClosedPRFlashesAndSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		down []string
		want string
	}{
		{[]string{"j", "j", "j", "j", "j"}, "talkable#10 is merged: nothing to mute"}, // the state is read without regard to case
		{[]string{"j", "j", "j", "j", "j", "j"}, "talkable#11 is closed: nothing to mute"},
	} {
		m, act := muteMergedDash(t, muteMergedData(), tc.down...)
		m, _ = send(t, m, keyMsg("M"))
		if len(act.calls) != 0 || m.confirm != nil || m.busy != "" {
			t.Fatalf("M asked or ran: %v (confirm %+v, busy %q)", act.calls, m.confirm, m.busy)
		}
		if m.flash != tc.want {
			t.Errorf("flashed %q, want %q", m.flash, tc.want)
		}
		mustContain(t, viewOf(m), tc.want)
		mustNotContain(t, viewOf(m), "y/N")
		if m, _ = send(t, m, keyMsg("y")); len(act.calls) != 0 || m.busy != "" {
			t.Errorf("a y after the flash ran %v", act.calls)
		}
	}
}

// A slot row reads the flag from itself, else from its PR's queue row, the way
// it reads the GitHub state.
func TestDashboardSlotRowMuteReadsTheMergeFlag(t *testing.T) {
	for name, c := range map[string]struct {
		slot, queue  string // GitHub state
		slotFlagged  bool
		queueFlagged bool
		want         string // "" = the ordinary question
	}{
		"slot merged and flagged":      {slot: "MERGED", slotFlagged: true, want: "Dismiss the merged-unreviewed flag on talkable#1? (r still runs a post-merge review)"},
		"queue row merged and flagged": {queue: "MERGED", queueFlagged: true, want: "Dismiss the merged-unreviewed flag on talkable#1? (r still runs a post-merge review)"},
		"slot merged, queue is behind": {slot: "MERGED", slotFlagged: true, queue: "OPEN", want: "Dismiss the merged-unreviewed flag on talkable#1? (r still runs a post-merge review)"},
		"slot open, queue is behind":   {slot: "OPEN", queue: "MERGED", queueFlagged: true, want: "Mute talkable#1: stop automatic reviews of it?"},
		"neither knows":                {want: "Mute talkable#1: stop automatic reviews of it?"},
	} {
		data := dashData()
		data.Slots[0].PRGHState, data.Slots[0].PRMergedUnreviewed = c.slot, c.slotFlagged
		data.Queue[1].GHState, data.Queue[1].MergedUnreviewed = c.queue, c.queueFlagged // talkable#1
		m, _, _ := newDash(t, 220, 50)
		m, _ = send(t, m, dashDataMsg{data: data})
		m, _ = send(t, m, keyMsg("M")) // the cursor starts on slot review1
		if m.confirm == nil || m.confirm.question != c.want {
			t.Errorf("%s: asked %+v, want %q", name, m.confirm, c.want)
		}
	}

	data := dashData()
	data.Slots[0].PRGHState, data.Slots[0].PRFlagDismissed = "MERGED", true
	m, _, _ := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data}, keyMsg("M"))
	if want := "Restore the merged-unreviewed flag on talkable#1?"; m.confirm == nil || m.confirm.question != want {
		t.Errorf("a slot whose flag is dismissed asked %+v, want %q", m.confirm, want)
	}
}

// An open PR keeps the questions it had: M mutes, U unmutes.
func TestDashboardMuteOnAnOpenPRKeepsItsQuestions(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"M", "Mute talkable#1: stop automatic reviews of it?"},
		{"U", "Unmute talkable#1: resume automatic reviews of it?"},
	} {
		m, act := muteMergedDash(t, muteMergedData(), "j", "j", "j") // talkable#1 in the queue, open
		m, _ = send(t, m, keyMsg(tc.key))
		if m.confirm == nil || m.confirm.question != tc.want || len(act.calls) != 0 {
			t.Errorf("%s asked %+v (calls %v), want %q", tc.key, m.confirm, act.calls, tc.want)
		}
	}
}

// The right-click menu names what M does on a merged row and asks the same
// question through the key; with nothing to dismiss or restore the item is
// dimmed and does nothing.
func TestDashboardMenuMuteAsksTheSameQuestionOnMergedPRs(t *testing.T) {
	data := muteMergedData()
	for _, tc := range []struct {
		row                   int // the dashboard row: 2 slots, then the queue
		label, question, call string
	}{
		{2, "dismiss merged flag", "Dismiss the merged-unreviewed flag on talkable#7? (r still runs a post-merge review)", "mute talkable#7"},
		{4, "restore merged flag", "Restore the merged-unreviewed flag on talkable#9?", "unmute talkable#9"},
	} {
		m, act := muteMergedDash(t, data)
		m, _ = send(t, m, rightClick(10, dashRowY(m, tc.row)))
		got := menuState(m.menuItems())
		if enabled, ok := got[tc.label]; !ok || !enabled {
			t.Fatalf("row %d: menu %v, want %q enabled", tc.row, got, tc.label)
		}
		if _, ok := got["mute"]; ok {
			t.Errorf("row %d: the menu still says mute: %v", tc.row, got)
		}
		mustContain(t, viewOf(m), tc.label)

		onItem := m
		onItem.menu.sel = slices.IndexFunc(m.menuItems(), func(it menuItem) bool { return it.label == tc.label })
		for how, asked := range map[string]dashboardModel{"M": sendOneDash(t, m, "M"), "enter": sendOneDash(t, onItem, "enter")} {
			if asked.menu.open || asked.confirm == nil || asked.confirm.question != tc.question {
				t.Fatalf("row %d: %s in the menu asked %+v (menu open %v), want %q", tc.row, how, asked.confirm, asked.menu.open, tc.question)
			}
			if len(act.calls) != 0 {
				t.Fatalf("row %d: asking ran %v", tc.row, act.calls)
			}
			if _, _ = dashAct(t, asked, "y"); !slices.Equal(act.calls, []string{tc.call}) {
				t.Errorf("row %d: %s then y called %v, want [%s]", tc.row, how, act.calls, tc.call)
			}
			act.calls = nil
		}
	}

	for _, row := range []int{5, 6} { // merged and closed, nothing to dismiss
		m, act := muteMergedDash(t, data)
		m, _ = send(t, m, rightClick(10, dashRowY(m, row)))
		if got := menuState(m.menuItems()); got["mute"] || got["dismiss merged flag"] || got["restore merged flag"] {
			t.Errorf("row %d: the mute item is enabled: %v", row, got)
		}
		m, _ = send(t, m, keyMsg("M"))
		if m.confirm != nil || len(act.calls) != 0 {
			t.Errorf("row %d: a dimmed mute item acted: %+v %v", row, m.confirm, act.calls)
		}
	}

	m, _ := muteMergedDash(t, data)
	m, _ = send(t, m, rightClick(10, dashRowY(m, 3))) // talkable#1, open
	if got := menuState(m.menuItems()); !got["mute"] {
		t.Errorf("an open PR's menu: %v", got)
	}
}

// The overview's key help says what M does on a merged PR.
func TestDashboardHelpSaysWhatMuteDoesOnAMergedPR(t *testing.T) {
	m, _, _ := newDash(t, 160, 50)
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "on a merged PR: dismiss / restore its merged-unreviewed flag")
}

// sendOneDash presses one key.
func sendOneDash(t *testing.T, m dashboardModel, key string) dashboardModel {
	t.Helper()
	m, _ = send(t, m, keyMsg(key))
	return m
}
