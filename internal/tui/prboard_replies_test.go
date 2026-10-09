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

// Replies on magnum's review its judge has not re-decided show as "↩N" in
// LAST REVIEW, the board's `r` re-decides them (the judge alone), and a
// thread magnum stopped arguing in flags the PR for the operator.

const repliesRef = "talkable/example#7"

// repliesRow is a PR magnum reviewed with n replies waiting: its head is the
// reviewed one, and the review was a changes request by the operator.
func repliesRow(n int) PRBoardRow {
	return PRBoardRow{
		Ref: repliesRef, Owner: "talkable", Repo: "example", Number: 7, Title: "Speed up the coupon export",
		Author: "alice", URL: "https://github.com/talkable/example/pull/7", State: "reviewed", GHState: "OPEN",
		ActivityAt: ago(time.Hour), HeadSHA: "abc1234def0",
		LastReview: &ReviewInfo{Login: "zhuravel", Event: "CHANGES_REQUESTED", SubmittedAt: ago(2 * time.Hour), CommitSHA: "abc1234def0", Mine: true},
		Replies:    n,
	}
}

// squash folds the card's wrapped lines into one line of single spaces.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// repliesBoard is a w x h board with exactly rows.
func repliesBoard(t *testing.T, w, h int, opts PRBoardOptions, rows ...PRBoardRow) (prBoardModel, *fakeActions) {
	t.Helper()
	act := &fakeActions{}
	opts.Now = func() time.Time { return boardNow }
	opts.SelfLogins = boardSelf
	m := testPRBoard(context.Background(), &fakeBoardSource{rows: rows}, act, opts)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	next, _ = next.(prBoardModel).Update(prbDataMsg{rows: rows})
	return next.(prBoardModel), act
}

func repliesPainter(mode IconMode, rows ...PRBoardRow) prbPainter { return needsMePainter(mode, rows) }

// The LAST REVIEW cell carries "↩N" after what it shows today, in the
// icon mode's own marker, and a row without replies is as it was.
func TestLastReviewCellMarksTheRepliesWaitingForTheJudge(t *testing.T) {
	row := repliesRow(2)
	plain := repliesRow(0)
	for _, c := range []struct {
		mode IconMode
		mark string
	}{{IconsUnicode, "↩2"}, {IconsNerd, "↩2"}, {IconsASCII, "<-2"}} {
		p := repliesPainter(c.mode, row)
		base := cellText(p.lastReviewCell(plain))
		if strings.Contains(base, "↩") || strings.Contains(base, "<-") {
			t.Fatalf("%s: a row without replies shows %q", c.mode, base)
		}
		got := p.lastReviewCell(row)
		if want := base + " " + c.mark; cellText(got) != want {
			t.Errorf("%s: cell %q, want %q", c.mode, cellText(got), want)
		}
		if w := ansi.StringWidth(c.mark); got[len(got)-1].text != " "+c.mark || w != len([]rune(c.mark)) {
			t.Errorf("%s: the marker is %q (width %d)", c.mode, got[len(got)-1].text, w)
		}
		if got[len(got)-1].st.GetForeground() != p.pal.yellow.GetForeground() {
			t.Errorf("%s: the marker is not yellow: %+v", c.mode, got[len(got)-1].st)
		}
	}
	// A row whose review the board does not show still shows the marker.
	noReview := repliesRow(3)
	noReview.LastReview = nil
	p := repliesPainter(IconsUnicode, noReview)
	if got := cellText(p.lastReviewCell(noReview)); got != "↩3" {
		t.Errorf("no review: cell %q, want %q", got, "↩3")
	}
}

// The column is capped at 22 cells: a row with replies gives up its age,
// then the stale mark, before the marker, and the column never grows past
// the cap for it.
func TestLastReviewCellKeepsTheRepliesMarkerWithinTheCap(t *testing.T) {
	row := repliesRow(12)
	row.LastReview = &ReviewInfo{Login: "zhuravel", Event: "DISMISSED", SubmittedAt: ago(12 * 24 * time.Hour), CommitSHA: "abc1234def0", Mine: true, Stale: true}
	p := repliesPainter(IconsUnicode, row)
	forms := p.lastReviewForms(row)
	if len(forms) != 3 {
		t.Fatalf("%d forms, want the full one, one without the age and one without the stale mark", len(forms))
	}
	full := cellText(forms[0])
	for _, want := range []string{"dismissed", "12d", "⟳", "↩12"} {
		if !strings.Contains(full, want) {
			t.Errorf("full form %q lacks %q", full, want)
		}
	}
	if w := forms[0].width(); w <= prbCap[colLastReview] {
		t.Fatalf("the full form is %d wide: the test needs one over the cap %d", w, prbCap[colLastReview])
	}
	cs := p.cells(row, [3]int{})
	at := func(w int) string { return cellText(cs.lastFit(w)) }
	if got := at(prbCap[colLastReview]); !strings.Contains(got, "↩12") || !strings.Contains(got, "⟳") || strings.Contains(got, "12d") {
		t.Errorf("at the cap %q: want the marker and the stale mark, no age", got)
	}
	if got := at(18); !strings.HasSuffix(got, "↩12") || strings.Contains(got, "⟳") || strings.Contains(got, "12d") {
		t.Errorf("at 18 cells %q: want the marker only", got)
	}
	if got := at(40); got != full {
		t.Errorf("with room %q, want the full form %q", got, full)
	}

	// On the board the column stays within the cap and the marker shows.
	lay := p.layout([]PRBoardRow{row}, 0)
	for i, c := range lay.cols {
		if c == colLastReview && lay.widths[i] > prbCap[colLastReview] {
			t.Errorf("LAST REVIEW is %d cells wide, over its cap %d", lay.widths[i], prbCap[colLastReview])
		}
	}
	line := ansi.Strip(p.rowLine(row, lay, 0, false, false))
	if !strings.Contains(line, "↩12") || strings.Contains(line, "12d") {
		t.Errorf("row %q: want the marker kept and the age dropped", line)
	}
}

// A row with no replies is cut as before: one form.
func TestLastReviewCellWithoutRepliesHasOneForm(t *testing.T) {
	row := repliesRow(0)
	p := repliesPainter(IconsUnicode, row)
	if n := len(p.lastReviewForms(row)); n != 1 {
		t.Errorf("%d forms", n)
	}
	if cs := p.cells(row, [3]int{}); len(cs.lastAlt) != 0 {
		t.Errorf("alternatives %d", len(cs.lastAlt))
	}
	if got := cellText(p.lastReviewCell(row)); strings.ContainsAny(got, "↩<") {
		t.Errorf("cell %q carries a marker", got)
	}
}

// On a whole board, the marker shows in the row's LAST REVIEW column and
// only there.
func TestBoardRowShowsTheRepliesMarker(t *testing.T) {
	m, _ := repliesBoard(t, 200, 12, PRBoardOptions{}, repliesRow(2))
	line := lineWith(t, viewOf(m), "Speed up the coupon export")
	if !strings.Contains(line, "changes ") || !strings.Contains(line, "↩2") {
		t.Errorf("row %q lacks the marker", line)
	}
	m, _ = repliesBoard(t, 200, 12, PRBoardOptions{Icons: IconsASCII}, repliesRow(2))
	if line := lineWith(t, viewOf(m), "Speed up the coupon export"); !strings.Contains(line, "<-2") {
		t.Errorf("ascii row %q lacks <-2", line)
	}
	m, _ = repliesBoard(t, 200, 12, PRBoardOptions{}, repliesRow(0))
	if line := lineWith(t, viewOf(m), "Speed up the coupon export"); strings.Contains(line, "↩") {
		t.Errorf("a row without replies shows the marker: %q", line)
	}
}

// The card says it in words under LAST REVIEW, plain in ASCII.
func TestCardCountsTheRepliesNotRedecidedYet(t *testing.T) {
	for _, c := range []struct {
		mode IconMode
		n    int
		want string
	}{
		{IconsUnicode, 2, "↩ 2 replies not re-decided yet"},
		{IconsUnicode, 1, "↩ 1 reply not re-decided yet"},
		{IconsASCII, 2, "2 replies not re-decided yet"},
	} {
		row := repliesRow(c.n)
		p := repliesPainter(c.mode, row)
		card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), 120), "\n"))
		mustContain(t, card, "LAST REVIEW", c.want)
		if c.mode == IconsASCII && strings.Contains(card, "↩") {
			t.Errorf("ascii card shows an arrow:\n%s", card)
		}
		if i, j := strings.Index(card, "LAST REVIEW"), strings.Index(card, c.want); j < i {
			t.Errorf("the replies line comes before LAST REVIEW:\n%s", card)
		}
	}
	row := repliesRow(0)
	p := repliesPainter(IconsUnicode, row)
	if card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), 120), "\n")); strings.Contains(card, "not re-decided") {
		t.Errorf("a row without replies says so:\n%s", card)
	}
}

// r on a PR whose head magnum reviewed and whose replies wait asks to have
// the judge alone re-decide them, and y sends that; every other row keeps
// today's question and review.
func TestReviewKeyRedecidesTheRepliesOnTheReviewedHead(t *testing.T) {
	moved := repliesRow(2)
	moved.HeadSHA = "fff9999aaa0"
	moved.LastReview.Stale = true
	moved.SinceReview = &ReviewDelta{Base: "reviewed", BaseSHA: "abc1234def0", Commits: 2, Files: 1, Additions: 3}
	running := repliesRow(2)
	running.State = "reviewing"
	merged := repliesRow(2)
	merged.GHState, merged.State = "MERGED", "released"
	for _, c := range []struct {
		name string
		row  PRBoardRow
		key  string
		want string // the question; "" = refused before asking
		call string
	}{
		{"replies waiting", repliesRow(2), "r",
			"Judge re-decides 2 replies on example#7 now (judge only; no new commits since head abc1234 was reviewed 2h ago by zhuravel)?",
			"review " + repliesRef + " fresh=false simplify=false replies=true"},
		{"one reply", repliesRow(1), "r",
			"Judge re-decides 1 reply on example#7 now (judge only; no new commits since head abc1234 was reviewed 2h ago by zhuravel)?",
			"review " + repliesRef + " fresh=false simplify=false replies=true"},
		{"the head moved", moved, "r",
			"Review example#7 now (2 commits since the last review of abc1234 2h ago by zhuravel, head fff9999)?",
			"review " + repliesRef + " fresh=false simplify=false"},
		{"no replies", repliesRow(0), "r",
			"Review example#7 now (no new commits since head abc1234 was reviewed 2h ago by zhuravel)?",
			"review " + repliesRef + " fresh=false simplify=false"},
		{"R stays a fresh review", repliesRow(2), "R",
			"Fresh review of example#7 in new agent sessions (no new commits since head abc1234 was reviewed 2h ago by zhuravel)?",
			"review " + repliesRef + " fresh=true simplify=false"},
		{"i stays a simplify review", repliesRow(2), "i",
			"Simplify review of example#7, also running the simplify role (no new commits since head abc1234 was reviewed 2h ago by zhuravel)?",
			"review " + repliesRef + " fresh=false simplify=true"},
		{"a round in flight", running, "r", "", ""},
		{"a merged PR", merged, "r", "", ""},
	} {
		m, act := repliesBoard(t, 200, 12, PRBoardOptions{}, c.row)
		asked, _ := send(t, m, keyMsg(c.key))
		if c.want == "" {
			if asked.confirm != nil || len(act.calls) != 0 {
				t.Errorf("%s: asked %+v, calls %v", c.name, asked.confirm, act.calls)
			}
			continue
		}
		if asked.confirm == nil || asked.confirm.question != c.want {
			t.Errorf("%s: asked %+v\nwant %q", c.name, asked.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(asked), c.want+" y/N")
		if len(act.calls) != 0 {
			t.Errorf("%s: acted before y: %v", c.name, act.calls)
		}
		boardAct(t, asked, "y")
		if !slices.Equal(act.calls, []string{c.call}) {
			t.Errorf("%s: y called %v, want %q", c.name, act.calls, c.call)
		}
	}
}

// Anything but y cancels the re-decision, as it does every question.
func TestReviewKeyOnRepliesCancelsWithoutY(t *testing.T) {
	m, act := repliesBoard(t, 200, 12, PRBoardOptions{}, repliesRow(2))
	asked, _ := send(t, m, keyMsg("r"))
	if asked.confirm == nil {
		t.Fatal("r did not ask")
	}
	boardAct(t, asked, "enter")
	if len(act.calls) != 0 {
		t.Errorf("enter acted: %v", act.calls)
	}
}

// The menu and the card's action list name r by what it does on the row.
func TestReviewActionIsLabelledByWhatItDoesOnARowWithReplies(t *testing.T) {
	boardRow := func(r PRBoardRow) actRow { return boardActRow(r, "", boardNow) }
	if got := actionLabel(actReview, boardRow(repliesRow(2))); got != "re-decide replies" {
		t.Errorf("with replies: %q", got)
	}
	moved := repliesRow(2)
	moved.HeadSHA = "fff9999aaa0"
	for name, r := range map[string]PRBoardRow{"no replies": repliesRow(0), "the head moved": moved} {
		if got := actionLabel(actReview, boardRow(r)); got != "review" {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := actionLabel(actFresh, boardRow(repliesRow(2))); got != "fresh review" {
		t.Errorf("R with replies: %q", got)
	}
	p := repliesPainter(IconsUnicode, repliesRow(2))
	card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(repliesRow(2)), 120), "\n"))
	mustContain(t, card, "ACTIONS", "re-decide replies")
	var items []string
	for _, it := range actionMenu(boardActs, boardRow(repliesRow(2)), true, nil) {
		items = append(items, it.label)
	}
	if !slices.Contains(items, "re-decide replies") || slices.Contains(items, "review") {
		t.Errorf("menu %v", items)
	}
}

// A thread magnum stopped arguing in flags the PR as an error does: the
// title cell's "!" (also when nothing failed), and the card's NEEDS YOU says
// what to do; a real error keeps its own text beside it.
func TestStalemateFlagsTheRowAndTheCardSaysWhatToDo(t *testing.T) {
	url1 := "https://github.com/talkable/example/pull/7#discussion_r101"
	url2 := "https://github.com/talkable/example/pull/7#discussion_r102"
	row := repliesRow(0)
	row.Stalemate = []string{url1}

	for _, mode := range iconModes {
		p := repliesPainter(mode, row)
		if got := cellText(p.titleCell(row)); !strings.HasPrefix(got, p.g.errMark+" ") {
			t.Errorf("%s: title cell %q lacks the attention mark", mode, got)
		}
		quiet := repliesRow(0)
		if got := cellText(p.titleCell(quiet)); strings.HasPrefix(got, p.g.errMark+" ") {
			t.Errorf("%s: a row without a stalemate has the mark: %q", mode, got)
		}
	}
	m, _ := repliesBoard(t, 200, 12, PRBoardOptions{}, row)
	if line := lineWith(t, viewOf(m), "Speed up the coupon export"); !strings.Contains(line, "! Speed up") {
		t.Errorf("board row %q lacks the mark before its title", line)
	}

	p := repliesPainter(IconsUnicode, row)
	card := squash(ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), 140), "\n")))
	mustContain(t, card, "NEEDS YOU",
		"magnum stopped arguing after two rebuttals in 1 thread: "+url1+"; decide it (r has the judge re-decide, A/C post your verdict)")
	mustNotContain(t, card, "LAST ERROR")

	// Two threads: the first URL in the sentence, the rest listed under it.
	row.Stalemate = []string{url1, url2}
	card = squash(ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), 140), "\n")))
	mustContain(t, card, "in 2 threads: "+url1+"…; decide it (r has the judge re-decide, A/C post your verdict)", url2)

	// A row without one has neither the section nor the sentence.
	plain := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(repliesRow(0)), 140), "\n"))
	mustNotContain(t, plain, "NEEDS YOU", "stopped arguing")
}

// A stalemate beside a real error shares NEEDS YOU of a parked PR, or leads
// the section of one that is not parked, and never overwrites the error.
func TestStalemateKeepsARealErrorBesideIt(t *testing.T) {
	url := "https://github.com/talkable/example/pull/7#discussion_r101"
	count := func(s, sub string) int { return strings.Count(s, sub) }

	parked := repliesRow(0)
	parked.State = "needs_attention"
	parked.LastError, parked.ErrorFix = "judge failed: codex exited with status 1", "`magnum review example#7` tries again"
	parked.Stalemate = []string{url}
	p := repliesPainter(IconsUnicode, parked)
	card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(parked), 140), "\n"))
	mustContain(t, card, "stopped arguing after two rebuttals", "judge failed: codex exited with status 1", "fix: `magnum review example#7` tries again")
	if n := count(card, "NEEDS YOU"); n != 1 {
		t.Errorf("parked PR: %d NEEDS YOU headings\n%s", n, card)
	}
	if i, j := strings.Index(card, "stopped arguing"), strings.Index(card, "judge failed"); i > j {
		t.Errorf("the stalemate comes after the error:\n%s", card)
	}

	retrying := repliesRow(0)
	retrying.LastError = "compare since the last review: HTTP 502"
	retrying.Stalemate = []string{url}
	card = ansi.Strip(strings.Join(p.cardContent(sanitizeRow(retrying), 140), "\n"))
	mustContain(t, card, "NEEDS YOU", "stopped arguing after two rebuttals", "LAST ERROR", "compare since the last review: HTTP 502")

	// The error alone is as it was.
	solo := parked
	solo.Stalemate = nil
	card = ansi.Strip(strings.Join(p.cardContent(sanitizeRow(solo), 140), "\n"))
	mustNotContain(t, card, "stopped arguing")
	if n := count(card, "NEEDS YOU"); n != 1 {
		t.Errorf("error alone: %d NEEDS YOU headings\n%s", n, card)
	}
}

// The threads' URLs come from GitHub: escape sequences in one never reach
// the screen.
func TestStalemateURLsAreSanitized(t *testing.T) {
	row := repliesRow(0)
	row.Stalemate = []string{"https://github.com/talkable/example/pull/7#r1\x1b[31m\x07evil"}
	got := sanitizeRow(row)
	if strings.ContainsAny(got.Stalemate[0], "\x1b\x07") {
		t.Errorf("sanitized %q", got.Stalemate[0])
	}
	if strings.Contains(row.Stalemate[0], "evil") && !strings.Contains(row.Stalemate[0], "\x1b") {
		t.Fatal("the test row lost its escape")
	}
	m, _ := repliesBoard(t, 200, 12, PRBoardOptions{}, row)
	if raw := m.View().Content; strings.Contains(raw, "\x1b[31mevil") || strings.Contains(raw, "\x07") {
		t.Errorf("escape sequences reached the screen: %q", raw)
	}
}
