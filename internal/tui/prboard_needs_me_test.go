package tui

import (
	"context"
	"image/color"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

const (
	nmApprove = "talkable/talkable#11960" // magnum approved; GitHub requires a review that counts
	nmLift    = "talkable/talkable#11961" // magnum approved; only the operator's changes request blocks it
)

// needsMeRows are boardRows with two PRs magnum approved that wait for the
// operator, both quiet for days.
func needsMeRows() []PRBoardRow {
	row := func(ref string, n int, kind, decision string, quiet time.Duration) PRBoardRow {
		return PRBoardRow{Ref: ref, Owner: "talkable", Repo: "talkable", Number: n, Title: "Speed up the coupon export",
			Author: "alice", URL: "https://github.com/" + strings.Replace(ref, "#", "/pull/", 1), State: "reviewed", GHState: "OPEN",
			ActivityAt: ago(quiet), HeadSHA: "f0f0f0f0f0",
			LastReview: &ReviewInfo{Login: "talkable[bot]", Event: "APPROVED", SubmittedAt: ago(quiet), CommitSHA: "f0f0f0f0f0"},
			NeedsMe:    kind, ReviewDecision: decision}
	}
	return append(boardRows(), row(nmApprove, 11960, NeedsMeApprove, "REVIEW_REQUIRED", 6*24*time.Hour),
		row(nmLift, 11961, NeedsMeLift, "CHANGES_REQUESTED", 8*24*time.Hour))
}

func needsMePainter(mode IconMode, rows []PRBoardRow) prbPainter {
	st := newStyles(true)
	return newPRBPainter(st, newPRBPalette(st), newGlyphs(mode), boardNow, selfSet(boardSelf), rows, SortUpdated, true)
}

// runeColors are the foreground colors of c, one per rune.
func runeColors(c cell) []color.Color {
	var out []color.Color
	for _, s := range c {
		for range []rune(s.text) {
			out = append(out, s.st.GetForeground())
		}
	}
	return out
}

// A PR magnum approved that waits for the operator says so in its state
// cell, "✔ needs you", or "✔ lift your ✗" when their own changes request
// is the only one blocking it (ASCII: "+" and "x"); a round in flight shows
// its own pill; a terminal without colors gets bold reverse video.
func TestTheNeedsYouCellSaysWhatTheOperatorsApprovalWouldFix(t *testing.T) {
	rows := needsMeRows()
	approve, lift := rows[len(rows)-2], rows[len(rows)-1]
	p := needsMePainter(IconsUnicode, rows)
	for _, c := range []struct {
		p    prbPainter
		r    PRBoardRow
		want string
	}{
		{p, approve, " ✔ needs you "},
		{p, lift, " ✔ lift your ✗ "},
		{needsMePainter(IconsASCII, rows), approve, " + needs you "},
		{needsMePainter(IconsASCII, rows), lift, " + lift your x "},
		{needsMePainter(IconsNerd, rows), lift, " \U000F012C lift your \ueb43 "}, // the review icons gh-dash uses
	} {
		got := c.p.stateWaitCell(c.r)
		var b strings.Builder
		for _, s := range got {
			b.WriteString(s.text)
		}
		if b.String() != c.want {
			t.Errorf("%s (%s): cell %q, want %q", c.r.Ref, c.p.g.mode, b.String(), c.want)
		}
		for _, s := range got {
			if !s.st.GetBold() || s.st.GetReverse() {
				t.Errorf("%s: a shimmering run is not bold foreground text: %+v", c.r.Ref, s)
			}
		}
	}

	running := approve
	running.State = "reviewing"
	if got := cellText(p.stateWaitCell(running)); strings.Contains(got, "needs you") || !strings.Contains(got, "reviewing") {
		t.Errorf("a round in flight: cell %q, want its reviewing pill", got)
	}

	p.colorless = true
	got := p.stateWaitCell(lift)
	if len(got) != 1 || got[0].text != " ✔ lift your ✗ " || !got[0].st.GetBold() || !got[0].st.GetReverse() {
		t.Errorf("colorless cell = %+v, want one bold reverse run", got)
	}
}

func cellText(c cell) string {
	var b strings.Builder
	for _, s := range c {
		b.WriteString(s.text)
	}
	return b.String()
}

// The shimmer is a rainbow of the six ANSI hues, each two cells wide,
// sliding one cell to the right every frame: deterministic given the frame,
// and back where it began after twelve.
func TestTheShimmerSlidesOneCellAFrame(t *testing.T) {
	rows := needsMeRows()
	approve := rows[len(rows)-2]
	p := needsMePainter(IconsUnicode, rows)
	at := func(frame int) []color.Color {
		q := p
		q.shimmer = frame
		return runeColors(q.needsMeCell(approve.NeedsMe))
	}
	f0 := at(0)
	rainbow := make([]color.Color, len(p.pal.rainbow))
	for i, s := range p.pal.rainbow {
		rainbow[i] = s.GetForeground()
	}
	if len(rainbow) != 6 || p.pal.shimmerCycle() != 12 {
		t.Fatalf("rainbow of %d colors, cycle %d", len(rainbow), p.pal.shimmerCycle())
	}
	for i, c := range f0 {
		if want := rainbow[(i/shimmerSpan)%len(rainbow)]; c != want {
			t.Fatalf("frame 0, cell %d: %v, want %v", i, c, want)
		}
	}
	for f := range 12 {
		cur, next := at(f), at(f+1)
		for i := 1; i < len(cur); i++ {
			if next[i] != cur[i-1] {
				t.Fatalf("frame %d→%d: cell %d is %v, want the color cell %d had (%v)", f, f+1, i, next[i], i-1, cur[i-1])
			}
		}
		if !slices.Equal(cur, at(f)) {
			t.Fatalf("frame %d is not deterministic", f)
		}
	}
	if !slices.Equal(at(12), f0) || !slices.Equal(at(-1), at(11)) {
		t.Fatal("the shimmer does not come round after 12 frames")
	}
	if slices.Equal(at(1), f0) {
		t.Fatal("the shimmer does not move")
	}
}

// The updated sort, the board's default, lists the PRs that need the
// operator first, either way, then the others in their order; the other
// sorts keep their order.
func TestNeedsYouRowsComeFirstInTheUpdatedSort(t *testing.T) {
	rows := needsMeRows()
	refs := func(rs []PRBoardRow) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = prRef(r)
		}
		return out
	}
	plain := func(by PRSort, desc bool) []string {
		rs := needsMeRows()
		for i := range rs {
			rs[i].NeedsMe = ""
		}
		return refs(SortPRBoard(rs, by, desc))
	}
	without := func(list []string) []string {
		return slices.DeleteFunc(slices.Clone(list), func(r string) bool { return r == nmApprove || r == nmLift })
	}
	for _, desc := range []bool{true, false} {
		got := refs(SortPRBoard(rows, SortUpdated, desc))
		first := []string{nmApprove, nmLift} // newer activity first
		if !desc {
			first = []string{nmLift, nmApprove}
		}
		if !slices.Equal(got[:2], first) || !slices.Equal(got[2:], without(plain(SortUpdated, desc))) {
			t.Errorf("updated desc=%v: %v", desc, got)
		}
	}
	for _, by := range []PRSort{SortLastReview, SortReviewerActivity, SortRequested, SortChanges, SortState} {
		if got, want := refs(SortPRBoard(rows, by, true)), plain(by, true); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", by, got, want)
		}
	}
}

// needsMeBoard is a board w x h that received needsMeRows, and its source.
func needsMeBoard(t *testing.T, w, h int, opts PRBoardOptions) (prBoardModel, tea.Cmd) {
	t.Helper()
	opts.Now = func() time.Time { return boardNow }
	opts.SelfLogins = boardSelf
	m := testPRBoard(context.Background(), &fakeBoardSource{rows: needsMeRows()}, &fakeActions{}, opts)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	next, cmd := next.(prBoardModel).Update(prbDataMsg{rows: needsMeRows()})
	return next.(prBoardModel), cmd
}

// shimmerTicks reports whether cmd (or a batch of it) schedules the
// shimmer's next frame.
func shimmerTicks(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case prbShimmerMsg:
		return true
	case tea.BatchMsg:
		return slices.ContainsFunc(msg, shimmerTicks)
	}
	return false
}

// The needs-you cells shimmer at four frames a second only while one is
// on screen: each frame draws them again and nothing else (the rows' key,
// which the layout and every other row are cached by, stays); a board
// without one, one scrolled off, the card, a terminal without colors and
// [board] shimmer = false start no frames and keep their cached frames.
func TestTheShimmerRunsOnlyWhileANeedsYouCellIsOnScreen(t *testing.T) {
	if prbShimmerEvery != 250*time.Millisecond {
		t.Fatalf("frame time %v, want 4 frames a second", prbShimmerEvery)
	}

	plain, _, _ := newBoard(t, 160, 30, PRBoardOptions{})
	if plain.shimmering || plain.shimmerOn() {
		t.Fatal("a board without a needs-you row shimmers")
	}
	before := plain.View().Content
	next, cmd := plain.Update(prbShimmerMsg{})
	if shimmerTicks(cmd) || next.(prBoardModel).View().Content != before || next.(prBoardModel).frameKey() != plain.frameKey() {
		t.Fatal("a shimmer frame changed a board without a needs-you row")
	}

	m, cmd := needsMeBoard(t, 160, 30, PRBoardOptions{})
	if !shimmerTicks(cmd) || !m.shimmering {
		t.Fatal("a needs-you row on screen started no shimmer")
	}
	f0, rows0 := m.View().Content, m.rowsKey(m.viewWidth())
	mustContain(t, viewOf(m), "✔ needs you", "✔ lift your ✗")
	next, cmd = m.Update(prbShimmerMsg{})
	m = next.(prBoardModel)
	if !shimmerTicks(cmd) || m.shimmer != 1 {
		t.Fatalf("the next frame: shimmer %d, next frame scheduled %v", m.shimmer, shimmerTicks(cmd))
	}
	if m.View().Content == f0 || viewOf(m) != ansi.Strip(f0) {
		t.Fatal("a shimmer frame must change the colors only")
	}
	if m.rowsKey(m.viewWidth()) != rows0 {
		t.Fatal("a shimmer frame changed the rows' key: every row is drawn again")
	}
	if m.View().Content != uncachedBoard(m) {
		t.Fatal("the cached shimmer frame differs from a fresh one")
	}

	// Scrolled off: the needs-you rows sort first, the cursor goes to the
	// bottom of a short screen.
	short, _ := needsMeBoard(t, 160, 9, PRBoardOptions{})
	next, _ = short.Update(keyMsg("G"))
	short = next.(prBoardModel)
	if short.shimmerOn() || short.frameKey().shimmer != 0 {
		t.Fatal("a needs-you row scrolled off still shimmers")
	}
	short.shimmering = true // the frame that was pending when it scrolled off
	next, cmd = short.Update(prbShimmerMsg{})
	if shimmerTicks(cmd) || next.(prBoardModel).shimmering {
		t.Fatal("the shimmer did not stop with no needs-you row on screen")
	}
	next, cmd = next.(prBoardModel).Update(keyMsg("g"))
	if !shimmerTicks(cmd) {
		t.Fatal("scrolling back to a needs-you row did not restart the shimmer")
	}

	card, _ := needsMeBoard(t, 160, 30, PRBoardOptions{})
	next, _ = card.Update(keyMsg("enter"))
	if next.(prBoardModel).shimmerOn() {
		t.Fatal("the card shimmers")
	}
	mustContain(t, lineWith(t, viewOf(next.(prBoardModel)), nmApprove), "reviewed", "✔ needs you") // the card's head, still

	off, cmd := needsMeBoard(t, 160, 30, PRBoardOptions{NoShimmer: true})
	if shimmerTicks(cmd) || off.shimmerOn() {
		t.Fatal("[board] shimmer = false shimmers")
	}
	mustContain(t, viewOf(off), "✔ needs you")

	mono, _ := needsMeBoard(t, 160, 30, PRBoardOptions{})
	next, cmd = mono.Update(tea.ColorProfileMsg{Profile: colorprofile.ASCII})
	mono = next.(prBoardModel)
	if shimmerTicks(cmd) || mono.shimmerOn() {
		t.Fatal("a terminal without colors shimmers")
	}
	mono.shimmering = false
	if line := lineWith(t, mono.View().Content, "needs you"); !strings.Contains(line, lipgloss.NewStyle().Bold(true).Reverse(true).Render(" ✔ needs you ")) {
		t.Errorf("colorless needs-you cell is not bold reverse video: %q", line)
	}
}

// The frame cache follows the shimmer: every cached frame is the one a
// fresh render draws, frames move while a needs-you cell is on screen and
// stay put once none is.
func TestBoardFrameCacheFollowsTheShimmer(t *testing.T) {
	m, _ := needsMeBoard(t, 160, 9, PRBoardOptions{})
	steps := []step{
		{name: "shimmer frame", msgs: []tea.Msg{prbShimmerMsg{}}},
		{name: "another frame", msgs: []tea.Msg{prbShimmerMsg{}}},
		{name: "cursor down", msgs: keys("j")},
		{name: "a frame on the cursor's row", msgs: []tea.Msg{prbShimmerMsg{}}},
		{name: "bottom", msgs: keys("G")},
		{name: "a frame off screen", msgs: []tea.Msg{prbShimmerMsg{}}, same: true},
		{name: "top", msgs: keys("g")},
		{name: "a frame back on screen", msgs: []tea.Msg{prbShimmerMsg{}}},
		{name: "colorless", msgs: []tea.Msg{tea.ColorProfileMsg{Profile: colorprofile.ASCII}}},
		{name: "a frame without colors", msgs: []tea.Msg{prbShimmerMsg{}}, same: true},
	}
	runSteps(t, m, uncachedBoard, steps)
}

// The title says how many PRs magnum approved wait for the operator's
// approval: "3 need your ✓", "your ✓ ×3" on a narrow screen.
func TestTheTitleSaysHowManyNeedYourApproval(t *testing.T) {
	for n, want := range map[int]fact{1: {"1 needs your ✓", "your ✓ ×1"}, 3: {"3 need your ✓", "your ✓ ×3"}} {
		if got := (DaemonFacts{NeedsMe: n}).list(boardNow); !slices.Contains(got, want) {
			t.Errorf("NeedsMe %d: facts %v, want %v", n, got, want)
		}
	}
	if got := (DaemonFacts{}).list(boardNow); len(got) != 0 {
		t.Errorf("no PR needs you: facts %v", got)
	}
	m, _, _ := newBoard(t, 160, 30, PRBoardOptions{})
	next, _ := m.Update(prbDataMsg{rows: boardRows(), facts: DaemonFacts{NeedsMe: 3}})
	mustContain(t, strings.Split(viewOf(next.(prBoardModel)), "\n")[0], "3 need your ✓")

	// The status dashboard's title says it too.
	data := dashData()
	data.Facts = DaemonFacts{NeedsMe: 2}
	d := testDashboard(context.Background(), &fakeSource{data: data}, &fakeActions{}, DashboardOptions{Now: func() time.Time { return dashNow }})
	d, _ = send(t, d, tea.WindowSizeMsg{Width: 200, Height: 50}, dashDataMsg{data: data})
	mustContain(t, strings.Split(viewOf(d), "\n")[0], "2 need your ✓")
}

// A re-review that posted no new finding while earlier ones stay open is
// blocking (or not) because of those: its FINDINGS cell says how many are
// still open, not "clean" next to the changes-requested glyph.
func TestTheFindingsCellCountsStillOpenFindingsWhenNothingNewWasPosted(t *testing.T) {
	p := needsMePainter(IconsUnicode, nil)
	text := func(f *FindingsInfo) string {
		var b strings.Builder
		for _, s := range p.findingsCell(f) {
			b.WriteString(s.text)
		}
		return b.String()
	}
	for _, c := range []struct {
		f    *FindingsInfo
		want string
	}{
		{&FindingsInfo{Open: 3, Verdict: "blocking"}, "✗ 3 open"},
		{&FindingsInfo{Open: 1, Verdict: "non_blocking"}, p.g.nonBlocking + " 1 open"},
		{&FindingsInfo{Verdict: "clean"}, "✔ clean"},
		{&FindingsInfo{Counts: [4]int{0, 1, 0, 0}, Open: 2, Verdict: "blocking"}, "✗ P1"},
	} {
		if got := text(c.f); got != c.want {
			t.Errorf("findings %+v: cell %q, want %q", *c.f, got, c.want)
		}
	}
}
