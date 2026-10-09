package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const (
	pmMergedUnreviewed = "talkable/talkable#11990" // merged before magnum reviewed its last push
	pmClosedUnmerged   = "talkable/talkable#11991" // closed without merging
	pmMergedReviewed   = "talkable/talkable#11992" // merged with its head reviewed
)

// postMergeBoard is a recentBoard with the cursor on ref and the actions it
// runs.
func postMergeBoard(t *testing.T, ref string) (prBoardModel, *fakeActions) {
	t.Helper()
	m := recentBoard(t, 220, 40, PRBoardOptions{})
	at := slices.Index(boardRefs(m), ref)
	if at < 0 {
		t.Fatalf("%s is not on the board: %v", ref, boardRefs(m))
	}
	m.moveTo(at)
	act, ok := m.act.(*fakeActions)
	if !ok {
		t.Fatalf("the board runs %T, want *fakeActions", m.act)
	}
	return m, act
}

// postMergeRowY is the screen line of ref's row (the recently closed rows
// follow a heading, which boardRowY does not count).
func postMergeRowY(t *testing.T, m prBoardModel, ref string) int {
	t.Helper()
	at := slices.Index(boardRefs(m), ref)
	for y := range m.height {
		if m.rowAt(y) == at {
			return y
		}
	}
	t.Fatalf("%s is not drawn", ref)
	return 0
}

// postMergeRow is the fixture row ref of recentRows.
func postMergeRow(t *testing.T, ref string) PRBoardRow {
	t.Helper()
	for _, r := range recentRows() {
		if prRef(r) == ref {
			return r
		}
	}
	t.Fatalf("no fixture row %s", ref)
	return PRBoardRow{}
}

// A PR GitHub merged before magnum reviewed its last push is reviewed after
// the merge: r, R and i ask for a post-merge review that only comments, y
// runs the same review as for an open PR, any other key cancels.
func TestPRBoardPostMergeReviewKeysAskTheirOwnQuestions(t *testing.T) {
	cases := []struct{ key, question, call string }{
		{"r", "Post-merge review talkable#11990 (comment only)?",
			"review talkable/talkable#11990 fresh=false simplify=false"},
		{"R", "Fresh post-merge review of talkable#11990 in new agent sessions (comment only)?",
			"review talkable/talkable#11990 fresh=true simplify=false"},
		{"i", "Post-merge review of talkable#11990, also running the simplify role (comment only)?",
			"review talkable/talkable#11990 fresh=false simplify=true"},
	}
	for _, c := range cases {
		m, act := postMergeBoard(t, pmMergedUnreviewed)
		m, cmd := send(t, m, keyMsg(c.key))
		if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 {
			t.Fatalf("%s alone ran an action: %v", c.key, act.calls)
		}
		if m.confirm == nil || m.confirm.question != c.question {
			t.Fatalf("%s asked %+v, want %q", c.key, m.confirm, c.question)
		}
		mustContain(t, viewOf(m), c.question+" y/N", "y confirms, any other key cancels")

		if _, _ = boardAct(t, m, "y"); !slices.Equal(act.calls, []string{c.call}) {
			t.Errorf("%s then y called %v, want [%s]", c.key, act.calls, c.call)
		}
		act.calls = nil
		for _, no := range []string{"n", "esc", "enter"} {
			c2, _ := send(t, m, keyMsg(no))
			if len(act.calls) != 0 || c2.busy != "" || c2.confirm != nil {
				t.Errorf("%s then %s acted or left the question up: %v (busy %q)", c.key, no, act.calls, c2.busy)
			}
			mustContain(t, viewOf(c2), "cancelled")
			mustNotContain(t, viewOf(c2), "y/N")
		}
	}
}

// A PR closed without merging has nothing to review: r, R and i say so at
// once, neither asking nor running anything.
func TestPRBoardRefusesToReviewAPRClosedWithoutMerging(t *testing.T) {
	const want = "talkable#11991 was closed without merging: only open or merged PRs are reviewed"
	for _, k := range []string{"r", "R", "i"} {
		m, act := postMergeBoard(t, pmClosedUnmerged)
		m, cmd := send(t, m, keyMsg(k))
		if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 || m.confirm != nil || m.busy != "" {
			t.Fatalf("%s on a closed PR asked or ran: %v (confirm %+v, busy %q)", k, act.calls, m.confirm, m.busy)
		}
		if m.flash != want || !m.flashErr {
			t.Errorf("%s flashed %q (error %v), want the error %q", k, m.flash, m.flashErr, want)
		}
		mustContain(t, viewOf(m), want)
		mustNotContain(t, viewOf(m), "y/N")
	}
}

// A merged PR whose merged head magnum already reviewed has nothing more to
// review: r, R and i say which head, neither asking nor running anything.
func TestPRBoardRefusesToReviewAMergedHeadTwice(t *testing.T) {
	const want = "talkable#11992: its merged head d4d4d4d was already reviewed"
	for _, k := range []string{"r", "R", "i"} {
		m, act := postMergeBoard(t, pmMergedReviewed)
		m, cmd := send(t, m, keyMsg(k))
		if hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 || m.confirm != nil || m.busy != "" {
			t.Fatalf("%s on a reviewed merged PR asked or ran: %v (confirm %+v, busy %q)", k, act.calls, m.confirm, m.busy)
		}
		if m.flash != want || !m.flashErr {
			t.Errorf("%s flashed %q (error %v), want the error %q", k, m.flash, m.flashErr, want)
		}
		mustContain(t, viewOf(m), want)
	}
}

// An open PR is asked about as before: the post-merge questions and refusals
// are for merged and closed ones only.
func TestPRBoardOpenPRKeepsItsReviewQuestion(t *testing.T) {
	m, _ := postMergeBoard(t, "talkable/talkable#11950")
	m, _ = send(t, m, keyMsg("r"))
	if m.confirm == nil || !strings.HasPrefix(m.confirm.question, "Review talkable#11950 now (") {
		t.Errorf("an open PR asked %+v", m.confirm)
	}
}

// The post-merge question names what the variant does, comment only.
func TestPostMergeQuestionTexts(t *testing.T) {
	for _, c := range []struct {
		o    ReviewOpts
		want string
	}{
		{ReviewOpts{}, "Post-merge review example#7 (comment only)?"},
		{ReviewOpts{Fresh: true}, "Fresh post-merge review of example#7 in new agent sessions (comment only)?"},
		{ReviewOpts{Simplify: true}, "Post-merge review of example#7, also running the simplify role (comment only)?"},
	} {
		if got := postMergeQuestion("example#7", c.o); got != c.want {
			t.Errorf("postMergeQuestion(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

// While a post-merge round waits or runs the PR's pill shows the round's
// state, not "merged"; the rows magnum has closed or released, or
// that need attention, keep what GitHub did.
func TestRowStateOfAMergedPRShowsItsPostMergeRound(t *testing.T) {
	for _, s := range []string{"queued", "rereview_pending", "claiming", "reviewing", "verifying", "paused", " Rereview-Pending "} {
		if got := rowState(PRBoardRow{State: s, GHState: "MERGED"}); got != s {
			t.Errorf("merged, state %q: pill %q, want the state", s, got)
		}
	}
	for _, c := range []struct {
		r    PRBoardRow
		want string
	}{
		{PRBoardRow{State: "closed", GHState: "MERGED"}, "merged"},
		{PRBoardRow{State: "released", GHState: "merged"}, "merged"},
		{PRBoardRow{State: "needs_attention", GHState: "MERGED"}, "merged"},
		{PRBoardRow{State: "closed", GHState: "MERGED", MergedUnreviewed: true}, "merged_unreviewed"},
		{PRBoardRow{State: "queued", GHState: "CLOSED"}, "closed"},
		{PRBoardRow{State: "queued", GHState: "OPEN"}, "queued"},
	} {
		if got := rowState(c.r); got != c.want {
			t.Errorf("state %q, GitHub %q: pill %q, want %q", c.r.State, c.r.GHState, got, c.want)
		}
	}
}

// A merged PR in a post-merge round shows the round's state pill, then
// "post-merge" and what holds it, in the table; not "merged".
func TestPRBoardStateCellOfAPostMergeRound(t *testing.T) {
	m := recentBoard(t, 220, 40, PRBoardOptions{})
	p := m.painter()
	row := postMergeRow(t, pmMergedUnreviewed)
	row.State, row.Wait = "rereview_pending", "post-merge review · next tick"
	row.WaitDetail = "forced post-merge review waits: the next tick starts it"

	cell := ansi.Strip(p.stateWaitCell(sanitizeRow(row)).render(nil))
	if !strings.Contains(cell, "re-review") || !strings.HasSuffix(cell, " post-merge · next tick") || strings.Contains(cell, "merged") {
		t.Errorf("state cell = %q", cell)
	}

	rows := recentRows()
	for i := range rows {
		if prRef(rows[i]) == pmMergedUnreviewed {
			rows[i] = row
		}
	}
	m, _ = send(t, m, prbDataMsg{rows: rows})
	line := ansi.Strip(lineWith(t, m.View().Content, "#11990"))
	mustContain(t, line, "re-review", "post-merge · next tick")
	mustNotContain(t, line, "merged", "unreviewed")

	// Running: the working pill, and "post-merge" with nothing held.
	row.State, row.Wait, row.WaitDetail = "reviewing", "", ""
	cell = ansi.Strip(p.stateWaitCell(sanitizeRow(row)).render(nil))
	if !strings.Contains(cell, "reviewing") || !strings.HasSuffix(cell, " post-merge") || strings.Contains(cell, "merged") {
		t.Errorf("state cell while reviewing = %q", cell)
	}
	if !workingState(rowState(row)) {
		t.Error("the pill of a merged PR under review does not spin")
	}

	// A merged PR without a round is as before.
	plain := ansi.Strip(p.stateWaitCell(PRBoardRow{State: "closed", GHState: "MERGED"}).render(nil))
	if !strings.Contains(plain, "merged") || strings.Contains(plain, "post-merge") {
		t.Errorf("a merged PR without a round shows %q", plain)
	}
}

// The right-click menu offers the review items on a merged PR that can get
// a post-merge review, and dims them on one closed without merging and on
// one whose merged head was reviewed.
func TestPRBoardMenuOffersPostMergeReview(t *testing.T) {
	for _, c := range []struct {
		ref string
		ok  bool
	}{{pmMergedUnreviewed, true}, {pmClosedUnmerged, false}, {pmMergedReviewed, false}} {
		m, _ := postMergeBoard(t, c.ref)
		for _, it := range m.menuItems() {
			if slices.Contains([]string{"r", "R", "i"}, it.key) && it.ok != c.ok {
				t.Errorf("%s: %q enabled = %v, want %v", c.ref, it.label, it.ok, c.ok)
			}
		}
	}
}

// The menu's review item on a merged PR asks the post-merge question, as the
// key does; on one closed without merging it is dimmed and does nothing.
func TestPRBoardMenuReviewOnAMergedPRAsksPostMerge(t *testing.T) {
	m, act := postMergeBoard(t, pmMergedUnreviewed)
	y := postMergeRowY(t, m, pmMergedUnreviewed)
	m, _ = send(t, m, rightClick(40, y), keyMsg("enter"))
	if m.confirm == nil || m.confirm.question != "Post-merge review talkable#11990 (comment only)?" {
		t.Fatalf("the menu's review asked %+v", m.confirm)
	}
	if len(act.calls) != 0 {
		t.Errorf("asking ran %v", act.calls)
	}

	m, act = postMergeBoard(t, pmClosedUnmerged)
	y = postMergeRowY(t, m, pmClosedUnmerged)
	m, _ = send(t, m, rightClick(40, y), keyMsg("enter"))
	if m.confirm != nil || len(act.calls) != 0 || m.flash != "" {
		t.Errorf("a dimmed review item acted: %+v, %v, %q", m.confirm, act.calls, m.flash)
	}
}

// The card names the post-merge review under ACTIONS, and offers no review
// when there is nothing to review.
func TestPRBoardCardActionsOfMergedAndClosedPRs(t *testing.T) {
	m := recentBoard(t, 160, 60, PRBoardOptions{})
	p := m.painter()
	actions := func(ref string) string {
		card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(postMergeRow(t, ref)), 140), "\n"))
		_, acts, ok := strings.Cut(card, "ACTIONS")
		if !ok {
			t.Fatalf("%s: no ACTIONS section:\n%s", ref, card)
		}
		return acts
	}
	mustContain(t, actions(pmMergedUnreviewed), "post-merge review", "fresh post-merge review", "simplify", "open pane")
	for _, ref := range []string{pmClosedUnmerged, pmMergedReviewed} {
		a := actions(ref)
		mustContain(t, a, "open pane", "browser")
		mustNotContain(t, a, "r review", "fresh", "simplify", "post-merge") // K kill review stays
	}
	open := actions("talkable/talkable#11950")
	mustContain(t, open, "fresh review", "simplify")
	mustNotContain(t, open, "post-merge")
}

// The key help says what r does on a merged PR, and the two columns of keys
// still fit side by side in the narrowest box the help test draws.
func TestPRBoardHelpMentionsPostMergeReview(t *testing.T) {
	m, _, _ := newBoard(t, 120, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "review now (asks y/N); post-merge if merged")
	for _, l := range strings.Split(viewOf(m), "\n") {
		if strings.Contains(l, "review now (asks y/N)") && !strings.Contains(l, "Move and view") && !strings.Contains(l, "j/k") {
			t.Errorf("the help stacked its columns, the r line stands alone: %q", l)
		}
	}
}
