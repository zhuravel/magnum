package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// On a PR GitHub merged or closed, M is not "stop automatic reviews of it":
// no round is coming. On one merged before magnum reviewed its last push it
// dismisses the merged-unreviewed flag (the daemon's mute request does that),
// on one whose flag is dismissed it restores it (an unmute), and on any
// other merged or closed row it says there is nothing to mute.

const (
	mmDismissed  = "talkable/talkable#11993" // merged unreviewed, muted since: flag dismissed
	mmMergedMute = "talkable/talkable#11994" // merged after its review, muted: nothing to restore
	mmClosedMute = "talkable/talkable#11995" // closed unmerged, muted
	mmFlagMuted  = "talkable/talkable#11996" // merged unreviewed, muted and forced before the merge: flagged
)

// muteMergedRows are recentRows (#11990 merged unreviewed, #11991 closed
// unmerged, #11992 merged after its review) with four muted rows.
func muteMergedRows() []PRBoardRow {
	closed := func(n int, state, gh string, muted bool) PRBoardRow {
		return PRBoardRow{Ref: "talkable/talkable#" + strconv.Itoa(n), Owner: "talkable", Repo: "talkable", Number: n,
			Title: "Row " + strconv.Itoa(n), Author: "alice", State: state, GHState: gh, Muted: muted,
			ActivityAt: ago(3 * time.Hour), HeadSHA: "e5e5e5e5e5", ClosedAt: ago(3 * time.Hour), Recent: true}
	}
	dismissed := closed(11993, "released", "MERGED", true)
	dismissed.FlagDismissed = true
	dismissed.LastReview = &ReviewInfo{Login: "talkable[bot]", Event: "COMMENTED", SubmittedAt: ago(6 * time.Hour), CommitSHA: "a1a1a1a1a1", Stale: true}
	reviewed := closed(11994, "released", "MERGED", true)
	reviewed.LastReview = &ReviewInfo{Login: "talkable[bot]", Event: "APPROVED", SubmittedAt: ago(6 * time.Hour), CommitSHA: "e5e5e5e5e5"}
	flagged := closed(11996, "closed", "MERGED", true)
	flagged.MergedUnreviewed = true
	return append(recentRows(), dismissed, reviewed, closed(11995, "released", "CLOSED", true), flagged)
}

// sendOne presses one key.
func sendOne(t *testing.T, m prBoardModel, key string) prBoardModel {
	t.Helper()
	m, _ = send(t, m, keyMsg(key))
	return m
}

// muteMergedBoard is a board that received muteMergedRows, with the cursor
// on ref, and the actions it runs.
func muteMergedBoard(t *testing.T, ref string) (prBoardModel, *fakeActions) {
	t.Helper()
	src := &fakeBoardSource{rows: muteMergedRows()}
	act := &fakeActions{}
	m := testPRBoard(context.Background(), src, act, PRBoardOptions{
		Now: func() time.Time { return boardNow }, SelfLogins: boardSelf, RecentClosed: 24 * time.Hour})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 220, Height: 50}, prbDataMsg{rows: src.rows})
	at := slices.Index(boardRefs(m), ref)
	if at < 0 {
		t.Fatalf("%s is not on the board: %v", ref, boardRefs(m))
	}
	m.moveTo(at)
	return m, act
}

// M on a PR merged before its last push was reviewed asks to dismiss the
// flag, whether or not the PR is muted already (forced before the merge):
// y sends the mute request, any other key sends nothing.
func TestPRBoardMuteOnAFlaggedMergedPRAsksToDismissTheFlag(t *testing.T) {
	for _, tc := range []struct{ ref, label string }{
		{pmMergedUnreviewed, "talkable#11990"},
		{mmFlagMuted, "talkable#11996"},
	} {
		want := "Dismiss the merged-unreviewed flag on " + tc.label + "? (r still runs a post-merge review)"
		m, act := muteMergedBoard(t, tc.ref)
		m, cmd := send(t, m, keyMsg("M"))
		if hasMsg[actionDoneMsg](execCmd(cmd)) || len(act.calls) != 0 {
			t.Fatalf("%s: M alone ran %v", tc.ref, act.calls)
		}
		if m.confirm == nil || m.confirm.question != want {
			t.Fatalf("%s: M asked %+v, want %q", tc.ref, m.confirm, want)
		}
		mustContain(t, viewOf(m), want+" y/N", "y confirms, any other key cancels")

		if _, follow := boardAct(t, m, "y"); !slices.Equal(act.calls, []string{"mute " + tc.ref}) || !hasMsg[prbDataMsg](follow) {
			t.Errorf("%s: y called %v (refresh %v), want the mute request", tc.ref, act.calls, hasMsg[prbDataMsg](follow))
		}
		act.calls = nil
		for _, no := range []string{"n", "esc", "enter"} {
			c2, _ := send(t, m, keyMsg(no))
			if len(act.calls) != 0 || c2.busy != "" || c2.confirm != nil {
				t.Errorf("%s: %s acted or left the question up: %v (busy %q)", tc.ref, no, act.calls, c2.busy)
			}
			mustContain(t, viewOf(c2), "cancelled")
		}
	}
}

// M on a PR muted after it merged unreviewed asks to restore the flag, and y
// sends the unmute.
func TestPRBoardMuteOnADismissedMergedPRAsksToRestoreTheFlag(t *testing.T) {
	const want = "Restore the merged-unreviewed flag on talkable#11993?"
	m, act := muteMergedBoard(t, mmDismissed)
	m, _ = send(t, m, keyMsg("M"))
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("M asked %+v, want %q", m.confirm, want)
	}
	mustContain(t, viewOf(m), want+" y/N")
	if len(act.calls) != 0 {
		t.Fatalf("asking ran %v", act.calls)
	}
	if _, follow := boardAct(t, m, "y"); !slices.Equal(act.calls, []string{"unmute " + mmDismissed}) || !hasMsg[prbDataMsg](follow) {
		t.Errorf("y called %v, want the unmute request and a refresh", act.calls)
	}
	act.calls = nil
	if c2, _ := send(t, m, keyMsg("n")); len(act.calls) != 0 || c2.confirm != nil {
		t.Errorf("n acted: %v", act.calls)
	}
}

// M on any other merged or closed PR asks nothing and sends nothing: it says
// there is nothing to mute, and a y after it does nothing either.
func TestPRBoardMuteOnAPlainMergedOrClosedPRFlashesAndSendsNothing(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{pmMergedReviewed, "talkable#11992 is merged: nothing to mute"},
		{pmClosedUnmerged, "talkable#11991 is closed: nothing to mute"},
		{mmMergedMute, "talkable#11994 is merged: nothing to mute"},
		{mmClosedMute, "talkable#11995 is closed: nothing to mute"},
	} {
		m, act := muteMergedBoard(t, tc.ref)
		m, cmd := send(t, m, keyMsg("M"))
		if hasMsg[actionDoneMsg](execCmd(cmd)) || len(act.calls) != 0 || m.confirm != nil || m.busy != "" {
			t.Fatalf("%s: M asked or ran: %v (confirm %+v, busy %q)", tc.ref, act.calls, m.confirm, m.busy)
		}
		if m.flash != tc.want {
			t.Errorf("%s: flashed %q, want %q", tc.ref, m.flash, tc.want)
		}
		mustContain(t, viewOf(m), tc.want)
		mustNotContain(t, viewOf(m), "y/N")
		if m, _ = send(t, m, keyMsg("y")); len(act.calls) != 0 || m.busy != "" {
			t.Errorf("%s: a y after the flash ran %v", tc.ref, act.calls)
		}
	}
}

// An open PR keeps the questions it had: M mutes it, U unmutes it once it
// is muted (on one that is not muted U says so instead of asking).
func TestPRBoardMuteOnAnOpenPRKeepsItsQuestions(t *testing.T) {
	muted := muteMergedRows()
	for i := range muted {
		if prRef(muted[i]) == "talkable/talkable#11950" {
			muted[i].Muted = true
		}
	}
	for _, tc := range []struct {
		key, want string
		rows      []PRBoardRow
	}{
		{"M", "Mute talkable#11950: stop automatic reviews of it?", nil},
		{"U", "Unmute talkable#11950: resume automatic reviews of it?", muted},
	} {
		m, act := muteMergedBoard(t, "talkable/talkable#11950")
		if tc.rows != nil {
			m, _ = send(t, m, prbDataMsg{rows: tc.rows})
		}
		m, _ = send(t, m, keyMsg(tc.key))
		if m.confirm == nil || m.confirm.question != tc.want {
			t.Errorf("%s asked %+v, want %q", tc.key, m.confirm, tc.want)
		}
		if len(act.calls) != 0 {
			t.Errorf("%s alone ran %v", tc.key, act.calls)
		}
	}
}

// The right-click menu says what M does on a merged row and runs the same
// question through the key; on a merged or closed row with nothing to
// dismiss the item is dimmed and does nothing.
func TestPRBoardMenuMuteAsksTheSameQuestionOnMergedPRs(t *testing.T) {
	for _, tc := range []struct {
		ref, label, question, call string
	}{
		{pmMergedUnreviewed, "dismiss merged flag", "Dismiss the merged-unreviewed flag on talkable#11990? (r still runs a post-merge review)", "mute " + pmMergedUnreviewed},
		{mmFlagMuted, "dismiss merged flag", "Dismiss the merged-unreviewed flag on talkable#11996? (r still runs a post-merge review)", "mute " + mmFlagMuted},
		{mmDismissed, "restore merged flag", "Restore the merged-unreviewed flag on talkable#11993?", "unmute " + mmDismissed},
	} {
		m, act := muteMergedBoard(t, tc.ref)
		y := postMergeRowY(t, m, tc.ref)
		m, _ = send(t, m, rightClick(40, y))
		got := menuState(m.menuItems())
		if enabled, ok := got[tc.label]; !ok || !enabled {
			t.Fatalf("%s: menu %v, want %q enabled", tc.ref, got, tc.label)
		}
		if _, ok := got["mute"]; ok {
			t.Errorf("%s: the menu still says mute: %v", tc.ref, got)
		}
		mustContain(t, viewOf(m), tc.label)

		// the item's key, and enter on the highlighted item, ask the same
		onItem := m
		onItem.menu.sel = slices.IndexFunc(m.menuItems(), func(it menuItem) bool { return it.label == tc.label })
		for how, asked := range map[string]prBoardModel{"M": sendOne(t, m, "M"), "enter": sendOne(t, onItem, "enter")} {
			if asked.menu.open || asked.confirm == nil || asked.confirm.question != tc.question {
				t.Fatalf("%s: %s in the menu asked %+v (menu open %v), want %q", tc.ref, how, asked.confirm, asked.menu.open, tc.question)
			}
			if len(act.calls) != 0 {
				t.Fatalf("%s: asking ran %v", tc.ref, act.calls)
			}
			if _, _ = boardAct(t, asked, "y"); !slices.Equal(act.calls, []string{tc.call}) {
				t.Errorf("%s: %s then y called %v, want [%s]", tc.ref, how, act.calls, tc.call)
			}
			act.calls = nil
		}
	}

	for _, ref := range []string{pmMergedReviewed, pmClosedUnmerged, mmMergedMute, mmClosedMute} {
		m, act := muteMergedBoard(t, ref)
		y := postMergeRowY(t, m, ref)
		m, _ = send(t, m, rightClick(40, y))
		if got := menuState(m.menuItems()); got["mute"] || got["dismiss merged flag"] || got["restore merged flag"] {
			t.Errorf("%s: the mute item is enabled: %v", ref, got)
		}
		m, _ = send(t, m, keyMsg("M"))
		if m.confirm != nil || len(act.calls) != 0 {
			t.Errorf("%s: a dimmed mute item acted: %+v %v", ref, m.confirm, act.calls)
		}
	}

	// an open PR's menu is as before
	m, _ := muteMergedBoard(t, "talkable/talkable#11950")
	m, _ = send(t, m, rightClick(40, boardRowY(m, slices.Index(boardRefs(m), "talkable/talkable#11950"))))
	if got := menuState(m.menuItems()); !got["mute"] {
		t.Errorf("an open PR's menu: %v", got)
	}
}

// The card of a PR whose flag is dismissed says so in one dim line and offers
// M as the way back; a flagged PR's card offers to dismiss; a plain merged or
// closed PR's card offers neither mute nor unmute (there is nothing to undo),
// and an open one only what applies to it.
func TestPRBoardCardSaysTheFlagIsDismissed(t *testing.T) {
	m, _ := muteMergedBoard(t, mmDismissed)
	p := m.painter()
	card := func(ref string) string {
		for _, r := range muteMergedRows() {
			if prRef(r) == ref {
				return ansi.Strip(strings.Join(p.cardContent(sanitizeRow(r), 140), "\n"))
			}
		}
		t.Fatalf("no fixture row %s", ref)
		return ""
	}
	const line = "merged-unreviewed flag dismissed (M restores it)"
	dismissed := card(mmDismissed)
	mustContain(t, dismissed, line, "restore merged flag")
	mustNotContain(t, dismissed, "Merged ", "before magnum reviewed", "dismiss merged flag", "M mute")
	if n := strings.Count(dismissed, "flag dismissed"); n != 1 {
		t.Errorf("the card says it %d times", n)
	}

	flagged := card(pmMergedUnreviewed)
	mustContain(t, flagged, "before magnum reviewed its last push", "dismiss merged flag")
	mustNotContain(t, flagged, "flag dismissed", "restore merged flag", "M mute")

	for _, ref := range []string{pmMergedReviewed, pmClosedUnmerged, mmMergedMute, mmClosedMute} {
		c := card(ref)
		mustNotContain(t, c, "flag dismissed", "dismiss merged flag", "restore merged flag", "M mute", "unmute")
	}
	open := card("talkable/talkable#11950") // open, not muted
	mustContain(t, open, "M mute")
	mustNotContain(t, open, "U unmute")
}

// The key help says what M does on a merged PR.
func TestPRBoardHelpSaysWhatMuteDoesOnAMergedPR(t *testing.T) {
	m, _, _ := newBoard(t, 120, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "mute / unmute (asks y/N); merged: dismiss flag")
}
