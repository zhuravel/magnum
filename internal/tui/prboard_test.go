package tui

import (
	"context"
	"errors"
	"image/color"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// boardNow is local, as the board shows local clock times.
var boardNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)

func ago(d time.Duration) time.Time { return boardNow.Add(-d) }

// boardRows is a realistic board: mine and others' reviews, stale ones,
// a failure, a draft, a pin, a muted released PR and a second repository.
func boardRows() []PRBoardRow {
	return []PRBoardRow{
		{
			Ref: "talkable/talkable#11920", Owner: "talkable", Repo: "talkable", Number: 11920,
			Title: "Fix referral widget alignment on mobile Safari", Author: "ann", URL: "https://github.com/talkable/talkable/pull/11920",
			Labels: []string{"bug", "frontend"}, Assignees: []string{"bob"},
			State: "reviewed", GHState: "OPEN", UpdatedAt: ago(3 * time.Hour), HeadSHA: "abc1234def",
			LastReview: &ReviewInfo{Login: "zhuravel", Event: "APPROVED", SubmittedAt: ago(2 * time.Hour), CommitSHA: "abc1234def"},
			Reviewers: []ReviewerInfo{
				{Login: "cat", Verdict: "commented", SubmittedAt: ago(4 * time.Hour), CommitSHA: "9999999"},
				{Login: "bob", Verdict: "changes_requested", SubmittedAt: ago(26 * time.Hour), CommitSHA: "1111111", Stale: true},
				{Login: "zhuravel", Verdict: "approved", SubmittedAt: ago(2 * time.Hour), CommitSHA: "abc1234def"},
				{Login: "dan", Verdict: "pending", Requested: true},
			},
			SinceReview: &ReviewDelta{Base: "reviewed", BaseSHA: "abc1234def", Commits: 3, Files: 5, Additions: 41, Deletions: 7},
			Slot:        "~/Projects/talkable.review1", Pinned: true, RoundsToday: 2,
		},
		{
			Ref: "talkable/talkable#11931", Owner: "talkable", Repo: "talkable", Number: 11931,
			Title: "Bump rails from 7.1.4 to 7.2.1", Author: "dependabot[bot]", URL: "https://github.com/talkable/talkable/pull/11931",
			State: "queued", GHState: "OPEN", UpdatedAt: ago(20 * time.Minute), HeadSHA: "5555555",
			Reviewers:      []ReviewerInfo{{Login: "eve", Verdict: "pending", Requested: true}},
			SinceReview:    &ReviewDelta{Base: "base branch", BaseSHA: "0000000", Commits: 12, Files: 30, Additions: 900, Deletions: 40},
			NextEligibleAt: boardNow.Add(5 * time.Minute),
		},
		{
			Ref: "talkable/talkable#11902", Owner: "talkable", Repo: "talkable", Number: 11902,
			Title: "Referral analytics: add the campaign breakdown to the dashboard export", Author: "zhuravel",
			Labels: []string{"analytics"}, State: "needs_attention", GHState: "OPEN", UpdatedAt: ago(50 * time.Hour), HeadSHA: "7777777",
			LastReview: &ReviewInfo{Login: "talkable[bot]", Event: "CHANGES_REQUESTED", SubmittedAt: ago(26 * time.Hour), CommitSHA: "6666666", Stale: true},
			Reviewers: []ReviewerInfo{
				{Login: "ann", Verdict: "approved", SubmittedAt: ago(30 * time.Hour), CommitSHA: "6666666", Stale: true},
				{Login: "talkable[bot]", Verdict: "changes_requested", SubmittedAt: ago(26 * time.Hour), CommitSHA: "6666666", Stale: true},
			},
			SinceReview: &ReviewDelta{Base: "reviewed", BaseSHA: "6666666", Commits: 7, Files: 14, Additions: 1234, Deletions: 321},
			LastError:   "judge failed: codex exited with status 1 while reading the diff",
		},
		{
			Ref: "talkable/magnum#42", Owner: "talkable", Repo: "magnum", Number: 42,
			Title: "PR board screen", Author: "bob", Draft: true, State: "reviewing", GHState: "OPEN", UpdatedAt: ago(5 * time.Minute),
		},
		{
			Ref: "talkable/talkable#11800", Owner: "talkable", Repo: "talkable", Number: 11800,
			Title: "Drop the legacy coupon importer", Author: "frank", State: "released", GHState: "MERGED", UpdatedAt: ago(20 * 24 * time.Hour),
			Muted: true,
		},
		{
			Ref: "talkable/talkable#11950", Owner: "talkable", Repo: "talkable", Number: 11950,
			Title: "Add OAuth login for the merchant portal", Author: "gina", Assignees: []string{"ann", "zhuravel"},
			State: "rereview_pending", GHState: "OPEN", UpdatedAt: ago(90 * time.Minute), HeadSHA: "8888888",
			LastReview: &ReviewInfo{Login: "zhuravel", Event: "COMMENTED", SubmittedAt: ago(5 * time.Hour), CommitSHA: "4444444", Stale: true},
			Reviewers: []ReviewerInfo{
				{Login: "frank", Verdict: "approved", SubmittedAt: ago(time.Hour), CommitSHA: "8888888"},
				{Login: "zhuravel", Verdict: "commented", SubmittedAt: ago(5 * time.Hour), CommitSHA: "4444444", Stale: true},
			},
			SinceReview: &ReviewDelta{Base: "reviewed", BaseSHA: "4444444", Commits: 2, Files: 1, Additions: 10},
		},
		{
			Ref: "talkable/talkable#11000", Owner: "talkable", Repo: "talkable", Number: 11000,
			Title: "Legacy cleanup", Author: "hal", State: "baseline", GHState: "OPEN", UpdatedAt: ago(21 * 24 * time.Hour),
		},
	}
}

var boardSelf = []string{"zhuravel", "talkable[bot]"}

type fakeBoardSource struct {
	mu    sync.Mutex
	calls int
	rows  []PRBoardRow
	err   error
}

func (f *fakeBoardSource) Rows(context.Context) ([]PRBoardRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rows, f.err
}

func (f *fakeBoardSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newBoard builds a board w x h that already received boardRows.
func newBoard(t interface{ Helper() }, w, h int, opts PRBoardOptions) (prBoardModel, *fakeBoardSource, *fakeActions) {
	t.Helper()
	src := &fakeBoardSource{rows: boardRows()}
	act := &fakeActions{}
	opts.Now = func() time.Time { return boardNow }
	if opts.SelfLogins == nil {
		opts.SelfLogins = boardSelf
	}
	m := newPRBoardModel(context.Background(), src, act, opts)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	next, _ = next.(prBoardModel).Update(prbDataMsg{rows: src.rows})
	return next.(prBoardModel), src, act
}

// boardRefs are the refs of the rows on screen, top to bottom.
func boardRefs(m prBoardModel) []string {
	out := make([]string, len(m.view))
	for i, r := range m.view {
		out[i] = prRef(r)
	}
	return out
}

func refsOf(rows []PRBoardRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimPrefix(prRef(r), "talkable/")
	}
	return out
}

// boardAct presses keys, runs the action they start and feeds its result
// back, returning the model and the follow-up messages.
func boardAct(t *testing.T, m prBoardModel, names ...string) (prBoardModel, []tea.Msg) {
	t.Helper()
	m, cmd := send(t, m, keys(names...)...)
	var follow []tea.Msg
	for _, msg := range execCmd(cmd) {
		if am, ok := msg.(actionDoneMsg); ok {
			var next tea.Cmd
			m, next = send(t, m, am)
			follow = append(follow, execCmd(next)...)
		}
	}
	return m, follow
}

// lineWith is the first line of view holding s.
func lineWith(t *testing.T, view, s string) string {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(ansi.Strip(l), s) {
			return l
		}
	}
	t.Fatalf("no line holds %q:\n%s", s, ansi.Strip(view))
	return ""
}

func hasMsg[T any](msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(T); ok {
			return true
		}
	}
	return false
}

func TestSortPRBoardEachSort(t *testing.T) {
	rows := boardRows()
	cases := []struct {
		by   PRSort
		desc bool
		want []string
	}{
		{SortUpdated, true, []string{"magnum#42", "talkable#11931", "talkable#11950", "talkable#11920", "talkable#11902", "talkable#11800", "talkable#11000"}},
		{SortUpdated, false, []string{"talkable#11000", "talkable#11800", "talkable#11902", "talkable#11920", "talkable#11950", "talkable#11931", "magnum#42"}},
		// never reviewed: last either way, newest update first
		{SortLastReview, true, []string{"talkable#11920", "talkable#11950", "talkable#11902", "magnum#42", "talkable#11931", "talkable#11800", "talkable#11000"}},
		{SortLastReview, false, []string{"talkable#11902", "talkable#11950", "talkable#11920", "magnum#42", "talkable#11931", "talkable#11800", "talkable#11000"}},
		// the latest verdict by anyone: frank on #11950 an hour ago
		{SortReviewerActivity, true, []string{"talkable#11950", "talkable#11920", "talkable#11902", "magnum#42", "talkable#11931", "talkable#11800", "talkable#11000"}},
		{SortChanges, true, []string{"talkable#11902", "talkable#11931", "talkable#11920", "talkable#11950", "magnum#42", "talkable#11800", "talkable#11000"}},
		{SortChanges, false, []string{"talkable#11950", "talkable#11920", "talkable#11931", "talkable#11902", "magnum#42", "talkable#11800", "talkable#11000"}},
		{SortState, true, []string{"talkable#11902", "magnum#42", "talkable#11950", "talkable#11931", "talkable#11920", "talkable#11000", "talkable#11800"}},
		{SortState, false, []string{"talkable#11800", "talkable#11000", "talkable#11920", "talkable#11931", "talkable#11950", "magnum#42", "talkable#11902"}},
		{PRSort("bogus"), true, []string{"magnum#42", "talkable#11931", "talkable#11950", "talkable#11920", "talkable#11902", "talkable#11800", "talkable#11000"}},
	}
	for _, c := range cases {
		got := refsOf(SortPRBoard(rows, c.by, c.desc))
		if !slices.Equal(got, c.want) {
			t.Errorf("SortPRBoard(%s, desc=%t) =\n %v\nwant\n %v", c.by, c.desc, got, c.want)
		}
	}
	if refsOf(rows)[0] != "talkable#11920" {
		t.Error("SortPRBoard reordered its input")
	}
}

func TestParsePRSort(t *testing.T) {
	for in, want := range map[string]PRSort{
		"": SortUpdated, "updated": SortUpdated, "Last_Review": SortLastReview, "last review": SortLastReview,
		"reviewer-activity": SortReviewerActivity, "CHANGES": SortChanges, "state": SortState,
	} {
		if got, err := ParsePRSort(in); err != nil || got != want {
			t.Errorf("ParsePRSort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParsePRSort("size"); err == nil || !strings.Contains(err.Error(), "last-review") {
		t.Errorf("ParsePRSort(size) error = %v, want one listing the sorts", err)
	}
	if got := PRSorts(); len(got) != 5 || got[0] != SortUpdated {
		t.Errorf("PRSorts() = %v", got)
	}
}

// s cycles the sorts (largest first again), S reverses, the title bar and
// the sorted column's heading say so, and the cursor stays on its PR.
func TestPRBoardSortKeys(t *testing.T) {
	m, _, _ := newBoard(t, 160, 30, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("j"), keyMsg("j")) // talkable#11950
	mustContain(t, viewOf(m), "sorted by updated ↓", "UPDATED ↓")

	m, _ = send(t, m, keyMsg("s"))
	v := viewOf(m)
	mustContain(t, v, "sorted by last review ↓", "LAST REVIEW ↓")
	mustNotContain(t, v, "UPDATED ↓")
	if got := boardRefs(m); got[0] != "talkable/talkable#11920" {
		t.Errorf("last review order starts with %s", got[0])
	}
	if r, _ := m.selected(); prRef(r) != "talkable/talkable#11950" {
		t.Errorf("cursor moved to %s after sorting", prRef(r))
	}

	m, _ = send(t, m, keyMsg("S"))
	mustContain(t, viewOf(m), "sorted by last review ↑", "LAST REVIEW ↑")
	if got := boardRefs(m); got[0] != "talkable/talkable#11902" {
		t.Errorf("reversed last review order starts with %s", got[0])
	}

	m, _ = send(t, m, keyMsg("s"), keyMsg("s"), keyMsg("s"))
	if m.sort != SortState || !m.desc {
		t.Fatalf("sort = %s desc=%t, want state desc", m.sort, m.desc)
	}
	mustContain(t, viewOf(m), "sorted by state ↓", "STATE ↓")
	m, _ = send(t, m, keyMsg("s"))
	if m.sort != SortUpdated {
		t.Errorf("s after state = %s, want updated", m.sort)
	}

	d, _, _ := newBoard(t, 160, 30, PRBoardOptions{DefaultSort: SortChanges})
	if got := boardRefs(d); got[0] != "talkable/talkable#11902" {
		t.Errorf("DefaultSort changes starts with %s", got[0])
	}
}

// Narrow screens hide Assignee, then Since review, then Author, then
// Updated, then CI; Findings stays as long as the reviewers do. No line is
// ever wider than the screen.
func TestPRBoardColumnsHideByWidth(t *testing.T) {
	cases := []struct {
		width         int
		shown, hidden []string
	}{
		{170, []string{"REPO", "#", "TITLE", "AUTHOR", "ASSIGNEE", "UPDATED", "STATE", "LAST REVIEW", "FINDINGS", "CI", "SINCE REVIEW", "REVIEWERS"}, nil},
		{120, []string{"TITLE", "UPDATED", "STATE", "LAST REVIEW", "FINDINGS", "CI", "REVIEWERS"}, []string{"ASSIGNEE", "SINCE REVIEW", "AUTHOR"}},
		{100, []string{"TITLE", "STATE", "LAST REVIEW", "FINDINGS", "REVIEWERS"}, []string{"ASSIGNEE", "SINCE REVIEW", "AUTHOR", "UPDATED", "CI"}},
	}
	for _, c := range cases {
		m, _, _ := newBoard(t, c.width, 20, PRBoardOptions{})
		v := viewOf(m)
		header := ansi.Strip(lineWith(t, m.View().Content, "TITLE"))
		for _, s := range c.shown {
			if !strings.Contains(header, s) {
				t.Errorf("width %d: header lacks %s: %q", c.width, s, header)
			}
		}
		for _, s := range c.hidden {
			if strings.Contains(header, s) {
				t.Errorf("width %d: header still has %s: %q", c.width, s, header)
			}
		}
		if w := maxLineWidth(v); w > c.width {
			t.Errorf("width %d: a line is %d cells wide:\n%s", c.width, w, v)
		}
		mustContain(t, v, "talkable#11920", "reviewed")
	}
	// at 160 the since-review numbers line up and keep their signs
	v := viewOf(func() prBoardModel { m, _, _ := newBoard(t, 160, 20, PRBoardOptions{}); return m }())
	mustContain(t, v, "12c  +900  −40", " 3c   +41   −7", " 7c +1.2k −321")
}

func TestPRBoardNoLineOverflows(t *testing.T) {
	for _, w := range []int{12, 20, 23, 40, 60, 80, 100, 133, 200} {
		for _, h := range []int{3, 5, 8, 24} {
			for _, k := range [][]string{nil, {"j", "j", "j", "enter"}, {"?"}, {"/"}, {"x"}, {"j", "j", "R"}} {
				m, _, _ := newBoard(t, w, h, PRBoardOptions{})
				m, _ = send(t, m, keys(k...)...)
				v := viewOf(m)
				if got := maxLineWidth(v); got > w {
					t.Errorf("%dx%d keys %v: line %d wide:\n%s", w, h, k, got, v)
				}
				if n := strings.Count(v, "\n") + 1; n > h {
					t.Errorf("%dx%d keys %v: %d lines", w, h, k, n)
				}
			}
		}
	}
	// a tiny screen still shows the question, and a narrow one its y/N
	m, _, _ := newBoard(t, 100, 4, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("x"))
	mustContain(t, viewOf(m), "Release magnum#42:", "y/N")
	m, _, _ = newBoard(t, 40, 10, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("R"))
	mustContain(t, viewOf(m), "Fresh review", "y/N")
	// a narrow title bar sheds words, not the sort
	n, _, _ := newBoard(t, 70, 20, PRBoardOptions{})
	title := strings.Split(viewOf(n), "\n")[0]
	mustContain(t, title, "6 open", "updated ↓")
	mustNotContain(t, title, "all repos")
}

// Paging keys inside the card scroll it; they never move the cursor to
// another PR (the action keys would then hit the wrong one).
func TestPRBoardCardPagingKeepsPR(t *testing.T) {
	m, _, act := newBoard(t, 120, 14, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("j"), keyMsg("enter")) // #11931
	for _, k := range []string{"pgdown", "space", "ctrl+d", "G", "end", "pgup", "ctrl+u", "g", "home", "j", "k"} {
		m, _ = send(t, m, keyMsg(k))
		if r, _ := m.selected(); prRef(r) != "talkable/talkable#11931" || m.mode != prbDetail {
			t.Fatalf("%s in the card moved to %s (mode %d)", k, prRef(r), m.mode)
		}
	}
	m, _ = send(t, m, keyMsg("G"))
	mustContain(t, viewOf(m), "ACTIONS")
	m, _ = send(t, m, keyMsg("?"))
	if m.mode != prbHelp {
		t.Fatal("? in the card did not open the help")
	}
	m, _ = send(t, m, keyMsg("esc"))
	if m.mode != prbDetail {
		t.Error("closing the help must return to the card")
	}
	m, _ = boardAct(t, m, "o")
	if act.last() != "open talkable/talkable#11931" {
		t.Errorf("o in the card opened %q", act.last())
	}
}

// The help fits any width (keys stack when narrow) and scrolls when it is
// taller than the screen.
func TestPRBoardHelpFitsAndScrolls(t *testing.T) {
	m, _, _ := newBoard(t, 100, 30, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("?"))
	var box []string
	for _, l := range strings.Split(viewOf(m), "\n") {
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "│") {
			box = append(box, s)
		}
	}
	if len(box) == 0 {
		t.Fatal("no help box")
	}
	for _, l := range box {
		if !strings.HasSuffix(l, "│") {
			t.Errorf("help line lost its right border: %q", l)
		}
	}
	s, _, _ := newBoard(t, 80, 24, PRBoardOptions{})
	s, _ = send(t, s, keyMsg("?"))
	v := viewOf(s)
	mustContain(t, v, "Move and view", "more")
	mustNotContain(t, v, "released")
	seen := v
	for range 40 {
		s, _ = send(t, s, keyMsg("j"))
		seen += viewOf(s)
	}
	mustContain(t, seen, "mute / unmute", "Mouse", "Shift-drag")
	s, _ = send(t, s, keyMsg("G"))
	mustContain(t, viewOf(s), "Legend", "released")
}

func TestPRBoardFilterKeepsSelectionAndRoom(t *testing.T) {
	m, _, _ := newBoard(t, 120, 24, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("j"), keyMsg("j"), keyMsg("/")) // #11950
	m, _ = send(t, m, typed("zzz")...)
	m, _ = send(t, m, keyMsg("backspace"), keyMsg("backspace"), keyMsg("backspace"))
	if r, _ := m.selected(); prRef(r) != "talkable/talkable#11950" {
		t.Errorf("after an empty filter the cursor is on %s", prRef(r))
	}
	n, _, _ := newBoard(t, 80, 24, PRBoardOptions{})
	n, _ = send(t, n, keyMsg("/"))
	n, _ = send(t, n, typed("fix referral widget alignm")...)
	mustContain(t, viewOf(n), "/ fix referral widget alignm", "1 of 7 match")
}

// GitHub's strings cannot smuggle escape sequences or control characters
// onto the screen; a row naming no PR cannot be released.
func TestPRBoardSanitizesAndGuards(t *testing.T) {
	m, _, act := newBoard(t, 120, 24, PRBoardOptions{})
	evil := PRBoardRow{Title: "\x1b[31mred\x1b[0m\x07 title\nnext", Author: "\x1b]8;;http://x\x07ann\x1b]8;;\x07", State: "queued", UpdatedAt: boardNow}
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{evil}})
	raw := m.View().Content
	if strings.Contains(raw, "\x1b[31mred") || strings.Contains(raw, "\x07") || strings.Contains(raw, "http://x") {
		t.Errorf("escape sequences reached the screen: %q", raw)
	}
	mustContain(t, viewOf(m), "red title next", "ann")
	m, _ = send(t, m, keyMsg("x"))
	mustContain(t, viewOf(m), "this row names no PR")
	m, _ = boardAct(t, m, "b")
	if act.last() != "" {
		t.Errorf("an action ran on a row naming no PR: %q", act.last())
	}
	if boardRows()[0].Reviewers[0].Login != "cat" {
		t.Error("sanitizing changed the caller's rows")
	}
}

// Every reviewer shows a verdict glyph in the verdict's color; mine come
// first with ★, stale ones are dimmed and marked ⟳.
func TestPRBoardVerdictGlyphsAndColors(t *testing.T) {
	m, _, _ := newBoard(t, 240, 20, PRBoardOptions{})
	raw := m.View().Content
	v := viewOf(m)
	mustContain(t, v, "★zhuravel✔ bob✗⟳ cat💬 dan◌", "★talkable✗⟳ ann✔⟳", "★zhuravel💬⟳ frank✔", "eve◌")

	row := lineWith(t, raw, "#11920")
	p := m.pal
	for name, want := range map[string]string{
		"green ✔":        p.green.Render("✔"),
		"green approved": p.green.Render(" approved"),
		"yellow 💬":       p.yellow.Render("💬"),
		"dim stale ✗":    m.st.Dim.Render("✗"),
		"yellow ⟳":       p.yellow.Render("⟳"),
		"dim ◌":          m.st.Dim.Render("◌"),
		"magenta ★":      p.mine.Render("★"),
	} {
		if !strings.Contains(row, want) {
			t.Errorf("#11920 row lacks %s (%q):\n%q", name, want, row)
		}
	}
	if !hasStyled(row, p.add, "+41") || !hasStyled(row, p.del, "−7") {
		t.Errorf("#11920 since review not green/red:\n%q", row)
	}
	// a fresh changes-requested verdict is red (on a row off the cursor)
	r := boardRows()[2]
	r.Reviewers[0].Stale, r.Reviewers[1].Stale, r.LastReview.Stale = false, false, false
	m2, _, _ := newBoard(t, 240, 20, PRBoardOptions{})
	m2, _ = send(t, m2, prbDataMsg{rows: []PRBoardRow{boardRows()[3], r}})
	if row := lineWith(t, m2.View().Content, "#11902"); !strings.Contains(row, p.red.Render("✗")) || strings.Contains(ansi.Strip(row), "⟳") {
		t.Errorf("fresh changes requested not red or still stale:\n%q", row)
	}

	a, _, _ := newBoard(t, 240, 20, PRBoardOptions{Icons: IconsASCII})
	av := viewOf(a)
	mustContain(t, av, "*zhuravel:+ bob:x~ cat:c dan:?", "pin Fix referral", "> magnum#42")
	for _, g := range []string{"✔", "✗", "💬", "◌", "⟳", "★", "📌", "▌", "─"} {
		if strings.Contains(av, g) {
			t.Errorf("ASCII view has %q", g)
		}
	}
}

// The last review column: ★ when mine, verdict and age, dimmed with ⟳ when
// the head moved since; "—" when there is none.
func TestPRBoardLastReviewAndStale(t *testing.T) {
	m, _, _ := newBoard(t, 200, 20, PRBoardOptions{})
	raw := m.View().Content
	mustContain(t, ansi.Strip(lineWith(t, raw, "#11920")), "★ ✔  approved 2h")
	stale := lineWith(t, raw, "#11902")
	mustContain(t, ansi.Strip(stale), "★ ✗  changes 1d ⟳")
	if !strings.Contains(stale, m.st.Dim.Render(" changes")) || !strings.Contains(stale, m.pal.yellow.Render(" ⟳")) {
		t.Errorf("stale review not dimmed with a yellow ⟳:\n%q", stale)
	}
	if strings.Contains(stale, m.pal.red.Render(" changes")) {
		t.Error("stale review kept its verdict color")
	}
	mustContain(t, viewOf(m), "2 stale", "1 failing", "1 pinned", "1 attention", "1 re-review")
	// a review that is not mine still lines up with the ★ ones
	r := boardRows()[0]
	r.LastReview.Login, r.LastReview.Mine = "ann", false
	mustContain(t, ansi.Strip(RenderPRBoard([]PRBoardRow{r, boardRows()[2]}, 0, PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf})),
		"     ✔  approved 2h", "★ ✗  changes 1d ⟳")
	// SelfLogins decide what is mine when Mine is not set
	n, _, _ := newBoard(t, 200, 20, PRBoardOptions{SelfLogins: []string{"@ANN"}})
	mustContain(t, viewOf(n), "★ann✔⟳")
}

func TestPRBoardFilter(t *testing.T) {
	m, _, _ := newBoard(t, 160, 24, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("/"))
	if !m.filtering {
		t.Fatal("/ did not open the filter")
	}
	m, _ = send(t, m, typed("oauth")...)
	if got := boardRefs(m); !slices.Equal(got, []string{"talkable/talkable#11950"}) {
		t.Fatalf("filter oauth = %v", got)
	}
	v := viewOf(m)
	mustContain(t, v, `1 match "oauth"`, "/ oauth", "1 of 7 match")
	mustNotContain(t, v, "#11920")

	m, _ = send(t, m, keyMsg("enter")) // keep it and leave the input
	if m.filtering || m.filter.Value() != "oauth" {
		t.Fatalf("enter: filtering=%t value=%q", m.filtering, m.filter.Value())
	}
	mustContain(t, viewOf(m), "esc clear oauth")
	m, _ = send(t, m, keyMsg("esc"))
	if len(m.view) != 7 || m.filter.Value() != "" {
		t.Fatalf("esc did not clear the filter: %v", boardRefs(m))
	}

	for q, want := range map[string][]string{
		"frank":       {"talkable/talkable#11950", "talkable/talkable#11800"}, // a reviewer, an author
		"frontend":    {"talkable/talkable#11920"},                            // a label
		"magnum#42":   {"talkable/magnum#42"},                                 // a ref
		"11920 dan":   {"talkable/talkable#11920"},                            // every word must match
		"rfrlwdgt":    {"talkable/talkable#11920"},                            // fuzzy: letters in order
		"nothing-xyz": {},
	} {
		f, _, _ := newBoard(t, 160, 24, PRBoardOptions{})
		f, _ = send(t, f, keyMsg("/"))
		f, _ = send(t, f, typed(q)...)
		if got := boardRefs(f); !slices.Equal(got, want) {
			t.Errorf("filter %q = %v, want %v", q, got, want)
		}
	}
	e, _, _ := newBoard(t, 160, 24, PRBoardOptions{})
	e, _ = send(t, e, keyMsg("/"))
	e, _ = send(t, e, typed("zzzz")...)
	mustContain(t, viewOf(e), `no PR matches "zzzz"`)
	e, _ = send(t, e, keyMsg("esc"))
	if e.filtering || len(e.view) != 7 {
		t.Error("esc in the filter input must clear it and leave")
	}
	p, _ := send(t, func() prBoardModel { m, _, _ := newBoard(t, 160, 24, PRBoardOptions{}); return m }(), keyMsg("/"), tea.PasteMsg{Content: "rails"})
	if got := boardRefs(p); !slices.Equal(got, []string{"talkable/talkable#11931"}) {
		t.Errorf("pasted filter = %v", got)
	}
}

func TestPRBoardDetailCard(t *testing.T) {
	m, _, _ := newBoard(t, 120, 50, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("j"), keyMsg("j"), keyMsg("j"), keyMsg("enter")) // #11920
	if m.mode != prbDetail {
		t.Fatal("enter did not open the card")
	}
	v := viewOf(m)
	mustContain(t, v,
		"talkable/talkable#11920", "reviewed", "📌 pinned",
		"Fix referral widget alignment on mobile Safari", "https://github.com/talkable/talkable/pull/11920",
		"Author", "ann", "Assignees", "bob", "Labels", "bug, frontend", "Updated", "3h ago",
		"Head", "abc1234", "Slot", "~/Projects/talkable.review1", "Notes", "no", "Rounds today", "2",
		"SINCE REVIEW", "3 commits · 5 files · +41 −7 since the reviewed head abc1234",
		"LAST REVIEW", "★ zhuravel  ✔ approved",
		"REVIEWERS (4)", "LOGIN", "VERDICT", "SUBMITTED", "COMMIT",
		"✗ changes requested", "1111111", "⟳ stale", "◌ requested", "review requested", "💬 commented",
		"ACTIONS", "esc back")
	mustNotContain(t, v, "TITLE ", "LAST ERROR")
	// mine first, then blockers
	table := v[strings.Index(v, "REVIEWERS (4)"):]
	if a, b, c := strings.Index(table, "★ zhuravel"), strings.Index(table, "bob "), strings.Index(table, "dan "); a < 0 || b < a || c < b {
		t.Errorf("reviewer table order wrong:\n%s", v)
	}

	m, _ = send(t, m, keyMsg("esc"))
	if m.mode != prbTable {
		t.Fatal("esc did not close the card")
	}
	mustContain(t, viewOf(m), "TITLE", "REVIEWERS")

	f, _, _ := newBoard(t, 100, 50, PRBoardOptions{})
	f, _ = send(t, f, keyMsg("j"), keyMsg("j"), keyMsg("j"), keyMsg("j"), keyMsg("enter")) // #11902
	mustContain(t, viewOf(f), "NEEDS YOU", "judge failed: codex exited", "⟳ stale: the head moved to 7777777 since",
		"attention", "since the reviewed head 6666666", "+1234 −321", "★ talkable[bot]  ✗ changes requested")

	q, _, _ := newBoard(t, 120, 50, PRBoardOptions{})
	q, _ = send(t, q, keyMsg("j"), keyMsg("enter")) // #11931: never reviewed
	mustContain(t, viewOf(q), "against the base branch: never reviewed", "in 5m", "◌ requested", "dependabot[bot]")

	// a short screen scrolls the card with j/k and says what is below
	s, _, _ := newBoard(t, 120, 16, PRBoardOptions{})
	s, _ = send(t, s, keyMsg("j"), keyMsg("j"), keyMsg("j"), keyMsg("enter"))
	mustContain(t, viewOf(s), "more")
	s, _ = send(t, s, keys(strings.Split(strings.Repeat("j", 60), "")...)...)
	mustContain(t, viewOf(s), "ACTIONS")
	if s.detailScroll > 40 {
		t.Errorf("card scroll ran past its content: %d", s.detailScroll)
	}
	s, _ = send(t, s, keyMsg("k"))
	if r, _ := s.selected(); prRef(r) != "talkable/talkable#11920" {
		t.Error("j/k in the card must scroll it, not move the cursor")
	}
}

func TestPRBoardActionKeys(t *testing.T) {
	cases := []struct {
		keys []string
		want string
	}{
		{[]string{"r", "y"}, "review talkable/magnum#42 again=false fresh=false simplify=false"},
		{[]string{"R", "y"}, "review talkable/magnum#42 again=false fresh=true simplify=false"},
		{[]string{"i", "Y"}, "review talkable/magnum#42 again=false fresh=false simplify=true"},
		{[]string{"o"}, "open talkable/magnum#42"},
		{[]string{"p"}, "pin talkable/magnum#42"},
		{[]string{"u"}, "unpin talkable/magnum#42"},
		{[]string{"M", "y"}, "mute talkable/magnum#42"},
		{[]string{"U", "Y"}, "unmute talkable/magnum#42"},
		{[]string{"a"}, "attention"},
		{[]string{"b"}, "browser https://github.com/talkable/magnum/pull/42"}, // no URL: built from the ref
		{[]string{"x", "y"}, "release talkable/magnum#42"},
		{[]string{"K", "y"}, "abort talkable/magnum#42"},
		{[]string{"I", "y"}, "ignore talkable/magnum#42"},
		{[]string{"j", "enter", "r", "y"}, "review talkable/talkable#11931 again=false fresh=false simplify=false"}, // from the card
		{[]string{"G", "o"}, "open talkable/talkable#11000"},
	}
	for _, c := range cases {
		m, src, act := newBoard(t, 160, 24, PRBoardOptions{})
		before := src.count()
		m, follow := boardAct(t, m, c.keys...)
		if got := act.last(); got != c.want {
			t.Errorf("keys %v called %q, want %q", c.keys, got, c.want)
			continue
		}
		flash := c.want + " ok" // the last line of the action's output
		if c.keys[0] == "b" {
			flash = "opened https://github.com/talkable/magnum/pull/42"
		}
		mustContain(t, viewOf(m), flash)
		var reloaded bool
		for _, msg := range follow {
			if _, ok := msg.(prbDataMsg); ok {
				reloaded = true
			}
		}
		if !reloaded || src.count() == before {
			t.Errorf("keys %v: no refresh after the action", c.keys)
		}
	}

	m, _, act := newBoard(t, 160, 24, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("x"))
	mustContain(t, viewOf(m), "Release magnum#42: hand back its slot now, sessions parked and worktree reset? y/N",
		"y confirms, any other key cancels")
	m, _ = boardAct(t, m, "n")
	if act.last() != "" {
		t.Errorf("n released anyway: %q", act.last())
	}
	mustContain(t, viewOf(m), "release talkable/magnum#42 cancelled")

	e, _, eact := newBoard(t, 160, 24, PRBoardOptions{})
	eact.err = errors.New("slot busy")
	e, _ = boardAct(t, e, "p")
	mustContain(t, viewOf(e), "pin talkable/magnum#42: slot busy")
	e, _ = send(t, e, flashExpireMsg{seq: e.flashSeq})
	mustNotContain(t, viewOf(e), "slot busy")

	n := newPRBoardModel(context.Background(), &fakeBoardSource{rows: boardRows()}, nil, PRBoardOptions{})
	n, _ = send(t, n, prbDataMsg{rows: boardRows()}, keyMsg("r"))
	mustContain(t, viewOf(n), "actions are not available here")
}

// The board polls its source, keeps the last good rows when a load fails
// and says so in the footer; ctrl+r refreshes at once.
func TestPRBoardRefresh(t *testing.T) {
	src := &fakeBoardSource{rows: boardRows()}
	m := newPRBoardModel(context.Background(), src, nil, PRBoardOptions{Now: func() time.Time { return boardNow }, Refresh: time.Second})
	if _, ok := m.Init()().(tea.BatchMsg); !ok {
		t.Fatal("Init must batch its commands")
	}
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 20})
	mustContain(t, viewOf(m), "loading pull requests…", "loading…")

	msgs := execCmd(m.loadCmd())
	m, _ = send(t, m, msgs...)
	mustContain(t, viewOf(m), "6 open", "↻ 12:00:00", "talkable#11920")

	src.mu.Lock()
	src.err = errors.New("gh: rate limited")
	src.mu.Unlock()
	m, cmd := send(t, m, prbTickMsg{})
	m, _ = send(t, m, execCmd(cmd)...)
	v := viewOf(m)
	mustContain(t, v, "refresh failed: gh: rate limited (showing data from 12:00:00)", "talkable#11920")

	before := src.count()
	m, cmd = send(t, m, keyMsg("ctrl+r"))
	execCmd(cmd)
	if src.count() != before+1 {
		t.Error("ctrl+r did not load")
	}
	if m, cmd = send(t, m, keyMsg("ctrl+r")); cmd != nil {
		t.Error("a second ctrl+r while loading must not start another load")
	}

	bad := newPRBoardModel(context.Background(), &fakeBoardSource{err: errors.New("no registry")}, nil, PRBoardOptions{})
	bad, _ = send(t, bad, prbDataMsg{err: errors.New("no registry")})
	mustContain(t, viewOf(bad), "could not load pull requests: no registry")
	if err := RunPRBoard(context.Background(), nil, nil, PRBoardOptions{}); err == nil {
		t.Error("RunPRBoard(nil source) must fail")
	}
}

func TestPRBoardNavigationAndScroll(t *testing.T) {
	m, _, _ := newBoard(t, 120, 10, PRBoardOptions{}) // 4 rows fit
	v := viewOf(m)
	mustContain(t, v, "▼ 3 more", "1/7")
	mustNotContain(t, v, "▲")
	m, _ = send(t, m, keyMsg("G"))
	v = viewOf(m)
	mustContain(t, v, "▲ 3 more", "talkable#11000", "7/7")
	mustNotContain(t, v, "▼")
	m, _ = send(t, m, keyMsg("g"))
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("g: cursor %d scroll %d", m.cursor, m.scroll)
	}
	m, _ = send(t, m, keyMsg("pgdown"))
	if m.cursor != 3 {
		t.Errorf("pgdown moved to %d, want 3", m.cursor)
	}
	m, _ = send(t, m, keyMsg("k"), keyMsg("up"), keyMsg("pgup"))
	if m.cursor != 0 {
		t.Errorf("cursor %d after moving up", m.cursor)
	}
	// the cursor follows its PR across refreshes that reorder the rows
	m, _ = send(t, m, keyMsg("j"), keyMsg("j")) // #11950
	rows := boardRows()
	rows[5].UpdatedAt = boardNow // #11950 now the newest
	m, _ = send(t, m, prbDataMsg{rows: rows})
	if r, _ := m.selected(); prRef(r) != "talkable/talkable#11950" || m.cursor != 0 {
		t.Errorf("cursor on %s at %d after the refresh", prRef(r), m.cursor)
	}

	q, cmd := send(t, m, keyMsg("q"))
	if !isQuit(execCmd(cmd)) || q.mode != prbTable {
		t.Error("q must quit")
	}
	_, cmd = send(t, m, keyMsg("esc"))
	if !isQuit(execCmd(cmd)) {
		t.Error("esc with nothing to clear must quit")
	}
	_, cmd = send(t, m, keyMsg("ctrl+c"))
	if !isQuit(execCmd(cmd)) {
		t.Error("ctrl+c must quit")
	}
}

func TestPRBoardHelpOverlay(t *testing.T) {
	m, _, _ := newBoard(t, 120, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("?"))
	v := viewOf(m)
	mustContain(t, v, "Move and view", "Act on the PR", "Legend", "fresh review in new agent sessions (asks y/N)",
		"✔ approved", "✗ changes requested", "💬 commented", "◌ requested", "⟳ stale", "★ yours", "📌 pinned",
		"ctrl+r / F5", "refresh now", "review now (asks y/N)", "release (asks y/N)", "answer yes; any other key, enter too, cancels",
		"? close help")
	m, _ = send(t, m, keyMsg("?"))
	if m.mode != prbTable {
		t.Error("? did not close the help")
	}
}

func TestPRBoardRepoScopeAndOwners(t *testing.T) {
	m, _, _ := newBoard(t, 160, 24, PRBoardOptions{Repo: "magnum"})
	if got := boardRefs(m); !slices.Equal(got, []string{"talkable/magnum#42"}) {
		t.Fatalf("Repo magnum shows %v", got)
	}
	mustContain(t, viewOf(m), "· magnum ·", "1 open")
	o, _, _ := newBoard(t, 160, 24, PRBoardOptions{Repo: "talkable/talkable"})
	if len(o.view) != 6 {
		t.Errorf("Repo talkable/talkable shows %d rows", len(o.view))
	}

	rows := append(boardRows(), PRBoardRow{Ref: "acme/widgets#7", Title: "Widgets", State: "queued", UpdatedAt: ago(time.Minute)})
	out := ansi.Strip(RenderPRBoard(rows, 0, PRBoardOptions{Now: func() time.Time { return boardNow }}))
	mustContain(t, out, "acme/widgets#7", "talkable/talkable#11920", "all repos", "7 open")
	single := ansi.Strip(RenderPRBoard(boardRows(), 0, PRBoardOptions{Now: func() time.Time { return boardNow }}))
	mustNotContain(t, single, "talkable/talkable#")
}

func TestRenderPRBoard(t *testing.T) {
	now := func() time.Time { return boardNow }
	out := ansi.Strip(RenderPRBoard(boardRows(), 0, PRBoardOptions{Now: now, SelfLogins: boardSelf, DefaultSort: SortState}))
	lines := strings.Split(out, "\n")
	mustContain(t, lines[0], "magnum · pull requests", "all repos", "6 open", "sorted by state ↓")
	if !strings.Contains(lines[4], "#11902") || !strings.Contains(lines[len(lines)-1], "#11800") {
		t.Errorf("state order wrong:\n%s", out)
	}
	mustContain(t, out, "Referral analytics: add the campaign breakdown to the dashboard export") // no limit: whole titles
	narrow := RenderPRBoard(boardRows(), 100, PRBoardOptions{Now: now})
	if w := maxLineWidth(ansi.Strip(narrow)); w > 100 {
		t.Errorf("RenderPRBoard(100) is %d wide", w)
	}
	mustContain(t, ansi.Strip(RenderPRBoard(nil, 80, PRBoardOptions{Now: now})), "no open pull requests")
}

func TestPRBoardLightBackground(t *testing.T) {
	m, _, _ := newBoard(t, 120, 20, PRBoardOptions{})
	m, _ = send(t, m, tea.BackgroundColorMsg{Color: color.White})
	if m.st.dark || m.pal.selBg == newPRBPalette(newStyles(true)).selBg {
		t.Error("the board kept the dark palette on a light background")
	}
	if !m.View().AltScreen {
		t.Error("the board must ask for the alternate screen")
	}
}

func TestShortAgeAndCompactNum(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "now", 7 * time.Minute: "7m", 3 * time.Hour: "3h", 50 * time.Hour: "2d",
		20 * 24 * time.Hour: "2w", 90 * 24 * time.Hour: "3mo", 800 * 24 * time.Hour: "2y",
	} {
		if got := shortAge(d); got != want {
			t.Errorf("shortAge(%s) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1k", 1234: "1.2k", 12345: "12k", 1_260_000: "1.3M"} {
		if got := compactNum(n); got != want {
			t.Errorf("compactNum(%d) = %q, want %q", n, got, want)
		}
	}
}

// hasStyled reports whether raw holds text, padded on the left by up to
// six spaces, rendered in st.
func hasStyled(raw string, st lipgloss.Style, text string) bool {
	for pad := range 7 {
		if strings.Contains(raw, st.Render(strings.Repeat(" ", pad)+text)) {
			return true
		}
	}
	return false
}

var _ sync.Locker = (*sync.Mutex)(nil)

func TestPRBoardHelpNamesTheJudge(t *testing.T) {
	m, _, _ := newBoard(t, 140, 60, PRBoardOptions{Judge: "codex-judge"})
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "open the codex-judge pane")
	g, _, _ := newBoard(t, 140, 60, PRBoardOptions{})
	g, _ = send(t, g, keyMsg("?"))
	mustContain(t, viewOf(g), "open the judge pane")
}

// The review keys only ask: y runs the round once, any other key (enter
// too) cancels it.
func TestPRBoardReviewKeysAsk(t *testing.T) {
	for _, k := range []string{"r", "R", "i", "M", "U", "x"} {
		m, _, act := newBoard(t, 220, 24, PRBoardOptions{})
		m, cmd := send(t, m, keyMsg(k))
		if hasMsg[actionDoneMsg](execCmd(cmd)) || len(act.calls) != 0 {
			t.Fatalf("%s alone ran an action: %v", k, act.calls)
		}
		if m.confirm == nil || m.busy != "" {
			t.Fatalf("%s did not ask", k)
		}
		mustContain(t, viewOf(m), "y/N", "y confirms, any other key cancels")

		for _, yes := range []string{"y", "Y"} {
			_, _ = boardAct(t, m, yes)
		}
		if len(act.calls) != 2 {
			t.Errorf("%s then y, and %s then Y, called %v: want one call each", k, k, act.calls)
		}
		act.calls = nil
		for _, no := range []string{"enter", "n", "esc", "ctrl+r", "R"} {
			c, _ := send(t, m, keyMsg(no)) // run would set busy; the command is only the flash timer
			if len(act.calls) != 0 || c.busy != "" || c.loading {
				t.Errorf("%s then %s acted: %v (busy %q, loading %v)", k, no, act.calls, c.busy, c.loading)
			}
			if c.confirm != nil {
				t.Errorf("%s then %s left the question up", k, no)
			}
			mustContain(t, viewOf(c), "cancelled")
			mustNotContain(t, viewOf(c), "y/N")
		}
		// ctrl+c still quits
		if _, cmd := send(t, m, keyMsg("ctrl+c")); !isQuit(execCmd(cmd)) {
			t.Errorf("%s then ctrl+c did not quit", k)
		}
	}
}

// The question says what the key will do and what changed since the last
// review: nothing when the head is the reviewed one, else the commits.
func TestPRBoardConfirmationTexts(t *testing.T) {
	cases := []struct {
		keys []string
		want string
	}{
		// #11920: the head is the reviewed head
		{[]string{"j", "j", "j", "R"}, "Fresh review of talkable#11920 in new agent sessions " +
			"(no new commits since head abc1234 was reviewed 2h ago by zhuravel)?"},
		{[]string{"j", "j", "j", "r"}, "Review talkable#11920 now " +
			"(no new commits since head abc1234 was reviewed 2h ago by zhuravel)?"},
		// #11950: two commits since the review of 4444444
		{[]string{"j", "j", "r"}, "Review talkable#11950 now " +
			"(2 commits since the last review of 4444444 5h ago by zhuravel, head 8888888)?"},
		{[]string{"j", "j", "j", "j", "i"}, "Simplify review of talkable#11902, also running the simplify role " +
			"(7 commits since the last review of 6666666 26h ago by talkable[bot], head 7777777)?"},
		// #11931: never reviewed (its delta is from the base branch)
		{[]string{"j", "R"}, "Fresh review of talkable#11931 in new agent sessions (not reviewed yet, head 5555555)?"},
		{[]string{"r"}, "Review magnum#42 now (not reviewed yet)?"},
		{[]string{"M"}, "Mute magnum#42: stop automatic reviews of it?"},
		{[]string{"U"}, "Unmute magnum#42: resume automatic reviews of it?"},
		{[]string{"x"}, "Release magnum#42: hand back its slot now, sessions parked and worktree reset?"},
		{[]string{"K"}, "Kill the running review of magnum#42?"},
		{[]string{"I"}, "Ignore magnum#42: kill its review, mute it and free its slot?"},
	}
	for _, c := range cases {
		m, _, _ := newBoard(t, 220, 24, PRBoardOptions{})
		m, _ = send(t, m, keys(c.keys...)...)
		if m.confirm == nil {
			t.Fatalf("keys %v did not ask", c.keys)
		}
		if m.confirm.question != c.want {
			t.Errorf("keys %v asked\n%q, want\n%q", c.keys, m.confirm.question, c.want)
		}
		mustContain(t, viewOf(m), c.want+" y/N")
	}

	// a head that moved with no count known, and a review with no SHA
	r := PRBoardRow{HeadSHA: "1234567890", LastReview: &ReviewInfo{Login: "zhuravel[bot]", SubmittedAt: ago(20 * time.Minute), CommitSHA: "ffa3270aa"}}
	if got, want := boardReviewFacts(r, boardNow), "new commits since the last review of ffa3270 20m ago by zhuravel[bot], head 1234567"; got != want {
		t.Errorf("moved head: %q, want %q", got, want)
	}
	r.SinceReview = &ReviewDelta{Base: "reviewed", Commits: 1}
	if got, want := boardReviewFacts(r, boardNow), "1 commit since the last review of ffa3270 20m ago by zhuravel[bot], head 1234567"; got != want {
		t.Errorf("one commit: %q, want %q", got, want)
	}
	r.HeadSHA = "ffa3270aa"
	if got := reviewQuestion("example#730", ReviewOpts{Fresh: true}, boardReviewFacts(r, boardNow)); got !=
		"Fresh review of example#730 in new agent sessions (no new commits since head ffa3270 was reviewed 20m ago by zhuravel[bot])?" {
		t.Errorf("same head: %q", got)
	}

	// several owners: the question names the owner, as the table does
	m, _, _ := newBoard(t, 220, 24, PRBoardOptions{})
	rows := append(boardRows(), PRBoardRow{Ref: "acme/talkable#5", Owner: "acme", Repo: "talkable", Number: 5, State: "queued", UpdatedAt: boardNow})
	m, _ = send(t, m, prbDataMsg{rows: rows}, keyMsg("g"), keyMsg("r"))
	if m.confirm == nil || m.confirm.question != "Review acme/talkable#5 now (not reviewed yet)?" {
		t.Errorf("several owners asked %+v", m.confirm)
	}
}

// ctrl+r and F5 refresh and do nothing else: no action, no question.
func TestPRBoardRefreshKeysOnlyRefresh(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{keyMsg("ctrl+r"), {Code: tea.KeyF5}} {
		m, src, act := newBoard(t, 160, 24, PRBoardOptions{})
		before := src.count()
		m, cmd := send(t, m, k)
		msgs := execCmd(cmd)
		if !hasMsg[prbDataMsg](msgs) || src.count() != before+1 {
			t.Errorf("%s did not refresh", k)
		}
		if hasMsg[actionDoneMsg](msgs) || len(act.calls) != 0 || m.confirm != nil || m.mode != prbTable {
			t.Errorf("%s did more than refresh: calls %v, asking %v, mode %v", k, act.calls, m.confirm != nil, m.mode)
		}
	}
}

// A long repository name is cut from the left so the PR number always shows.
func TestRefColumnTruncatesFromTheLeft(t *testing.T) {
	c := cell{{"talkable/", lipgloss.Style{}}, {"talkable-shopify-extensions", lipgloss.Style{}}, {"#1234", lipgloss.Style{}}}
	got := c.fitLeft(18).render(nil)
	if !strings.HasSuffix(got, "#1234") || !strings.HasPrefix(got, "…") || ansi.StringWidth(got) != 18 {
		t.Fatalf("fitLeft(18) = %q (width %d)", got, ansi.StringWidth(got))
	}
	if got := c.fitLeft(60).render(nil); got != "talkable/talkable-shopify-extensions#1234" {
		t.Fatalf("fitLeft wide = %q", got)
	}
	if got := c.fitLeft(5).render(nil); got != "…1234" {
		t.Fatalf("fitLeft(5) = %q", got)
	}
}

// An ignored PR reads as ignored: greyed, its title struck through, no
// separate "muted" tag. Unmute is what undoes an ignore, so U asks to stop
// ignoring it, the menu and the card say so, and I is not offered again.
func TestPRBoardIgnoredRowsAreStruckAndUnmutedWithU(t *testing.T) {
	m, _, act := newBoard(t, 160, 24, PRBoardOptions{})
	ignored := PRBoardRow{Ref: "talkable/talkable#7", Owner: "talkable", Repo: "talkable", Number: 7, Title: "Old work",
		State: "ignored", Muted: true, GHState: "OPEN", UpdatedAt: ago(time.Hour)}
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{ignored}})

	var row string
	for _, l := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(l, "#7") {
			row = l
		}
	}
	if !strings.Contains(row, ";9mO") || strings.Contains(ansi.Strip(row), "muted") {
		t.Fatalf("ignored row is not struck through, or still tagged muted: %q", row)
	}

	items := map[string]bool{}
	for _, it := range m.menuItems() {
		items[it.label] = it.ok
	}
	if !items["unmute (stop ignoring)"] || items["ignore"] {
		t.Fatalf("menu on an ignored row: %v", items)
	}

	m, _ = send(t, m, keyMsg("U"))
	mustContain(t, viewOf(m), "Unmute talkable#7: stop ignoring it and review it on its next push?")
	m, _ = boardAct(t, m, "y")
	if got := act.last(); got != "unmute talkable/talkable#7" {
		t.Fatalf("U on an ignored row called %q", got)
	}

	c, _, _ := newBoard(t, 120, 50, PRBoardOptions{})
	c, _ = send(t, c, prbDataMsg{rows: []PRBoardRow{ignored}})
	c, _ = send(t, c, keyMsg("enter"))
	v := viewOf(c)
	mustContain(t, v, "U unmute: stop ignoring")
	mustNotContain(t, v, "I ignore")
}

// What magnum's latest review concluded shows even where it could only
// comment: the FINDINGS column (verdict glyph, findings by priority,
// simplifications), the card's decision and counts, and A / C post the
// reviewer's own verdict after a question that recalls the findings.
func TestPRBoardShowsFindingsAndPostsAVerdict(t *testing.T) {
	m, _, act := newBoard(t, 170, 24, PRBoardOptions{})
	row := PRBoardRow{Ref: "talkable/talkable#8", Owner: "talkable", Repo: "talkable", Number: 8, Title: "Refund metrics",
		State: "reviewed", GHState: "OPEN", UpdatedAt: ago(time.Hour), HeadSHA: "abcdef1234",
		LastReview: &ReviewInfo{Login: "talkable[bot]", Event: "COMMENTED", SubmittedAt: ago(time.Hour), CommitSHA: "abcdef1234"},
		Findings:   &FindingsInfo{Counts: [4]int{0, 1, 3, 0}, Simplifications: 4, Open: 1, Verdict: "blocking", Posted: "COMMENT", SHA: "abcdef1234"}}
	clean := row
	clean.Ref, clean.Number, clean.Title = "talkable/talkable#9", 9, "Copy fix"
	clean.Findings = &FindingsInfo{Verdict: "clean", Posted: "COMMENT", SHA: "abcdef1234"}
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{row, clean}})
	v := viewOf(m)
	mustContain(t, v, "FINDINGS", "✗ P1 P2×3 ✂4", "✔ clean")

	c, _, _ := newBoard(t, 170, 70, PRBoardOptions{})
	c, _ = send(t, c, prbDataMsg{rows: []PRBoardRow{row, clean}})
	c, _ = send(t, c, keyMsg("enter"))
	mustContain(t, viewOf(c), "FINDINGS", "Decision: request changes; posted as comment (A approves, C requests changes)",
		"P0 0 · P1 1 · P2 3 · P3 0 · 4 simplifications suggested", "Earlier findings: 0 fixed, 1 still open, 0 answered",
		"A approve", "C request changes")

	m, _ = send(t, m, keyMsg("C"))
	mustContain(t, viewOf(m), "Request changes on talkable#8 at abcdef1? magnum found 1 P1, 3 P2")
	m, _ = boardAct(t, m, "y")
	if got := act.last(); got != "request-changes talkable/talkable#8" {
		t.Fatalf("C called %q", got)
	}
	m, _ = send(t, m, keyMsg("j"), keyMsg("A"))
	mustContain(t, viewOf(m), "Approve talkable#9 at abcdef1? magnum found no findings")
	m, _ = boardAct(t, m, "y")
	if got := act.last(); got != "approve talkable/talkable#9" {
		t.Fatalf("A called %q", got)
	}
}

// A PR open before magnum watched its repository reads "not reviewed", and
// its card says what starts a review; A has nothing to approve on it.
func TestPRBoardNotReviewedRows(t *testing.T) {
	m, _, act := newBoard(t, 170, 30, PRBoardOptions{})
	row := PRBoardRow{Ref: "talkable/talkable#5", Owner: "talkable", Repo: "talkable", Number: 5, Title: "Old feature",
		State: "baseline", GHState: "OPEN", UpdatedAt: ago(time.Hour)}
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{row}})
	mustContain(t, viewOf(m), "not reviewed")
	m, _ = send(t, m, keyMsg("A"))
	mustContain(t, viewOf(m), "magnum has not reviewed this PR: nothing to approve on")
	if got := act.last(); got != "" {
		t.Fatalf("A on an unreviewed PR called %q", got)
	}
	c, _ := send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(c), "NOT REVIEWED", "Open before magnum began watching this repository")
	mustNotContain(t, viewOf(c), "A approve")
}
