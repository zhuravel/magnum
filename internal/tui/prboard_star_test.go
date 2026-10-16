package tui

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// starRows marks the rows refs names starred at at.
func starRows(rows []PRBoardRow, at time.Time, refs ...string) []PRBoardRow {
	for i := range rows {
		if slices.Contains(refs, prRef(rows[i])) {
			rows[i].Starred, rows[i].StarredAt = true, at
		}
	}
	return rows
}

// placeholderLogins is rows with the people's logins the design log may
// quote (zhuravel, alice, rev-ann, bob-rev) in place of the fixture's.
func placeholderLogins(rows []PRBoardRow) []PRBoardRow {
	to := map[string]string{"ann": "rev-ann", "bob": "bob-rev", "cat": "alice", "dan": "rev-ann", "eve": "bob-rev",
		"frank": "bob-rev", "gina": "alice", "hal": "alice"}
	login := func(s string) string { return cmp.Or(to[s], s) }
	out := slices.Clone(rows)
	for i := range out {
		r := &out[i]
		r.Author = login(r.Author)
		r.Assignees = slices.Clone(r.Assignees)
		for j := range r.Assignees {
			r.Assignees[j] = login(r.Assignees[j])
		}
		r.Reviewers = slices.Clone(r.Reviewers)
		for j := range r.Reviewers {
			r.Reviewers[j].Login = login(r.Reviewers[j].Login)
		}
	}
	return out
}

// unstarredOf is rows without the starred ones.
func unstarredOf(rows []PRBoardRow) []PRBoardRow {
	return slices.DeleteFunc(slices.Clone(rows), func(r PRBoardRow) bool { return r.Starred })
}

// Starred rows lead the board in every sort and either direction, newest
// update first, ahead of the rows that need the operator's approval; the
// others keep the sort as if no row were starred.
func TestStarredRowsLeadEverySortAndDirection(t *testing.T) {
	rows := boardRows()
	for i := range rows {
		if rows[i].Number == 11931 {
			rows[i].NeedsMe = NeedsMeApprove // leads the updated sort among the unstarred
		}
	}
	rows = starRows(rows, ago(time.Minute), "talkable/talkable#11000", "talkable/talkable#11902")
	stars := []string{"talkable/talkable#11902", "talkable/talkable#11000"} // updated 50h and 21d ago
	for _, by := range PRSorts() {
		for _, desc := range []bool{true, false} {
			got := refsOfFull(SortPRBoard(rows, by, desc))
			want := append(slices.Clone(stars), refsOfFull(SortPRBoard(unstarredOf(rows), by, desc))...)
			if !slices.Equal(got, want) {
				t.Errorf("%s desc=%v: %v, want %v", by, desc, got, want)
			}
		}
	}

	m, _, _ := newBoard(t, 200, 40, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: rows})
	for _, k := range []string{"", "S", "s", "S", "s", "s", "S", "s", "s", "s"} {
		if k != "" {
			m, _ = send(t, m, keyMsg(k))
		}
		refs := boardRefs(m)
		if !slices.Equal(refs[:2], stars) {
			t.Errorf("after %q (sort %s, desc %v): %v, want the stars %v first", k, m.sort, m.desc, refs, stars)
		}
		if want := refsOfFull(SortPRBoard(unstarredOf(rows), m.sort, m.desc)); !slices.Equal(refs[2:], want) {
			t.Errorf("after %q: the unstarred rows %v, want %v", k, refs[2:], want)
		}
	}
}

// Two starred rows compare by their last update, newest first, and a tie by
// the reference: neither the sort, its direction, the needs-you rule nor
// when each was starred moves them.
func TestTwoStarredRowsOrderByUpdatedThenReference(t *testing.T) {
	row := func(n int, state string, updated, starred time.Duration) PRBoardRow {
		r := PRBoardRow{Ref: "talkable/talkable#" + strconv.Itoa(n), Owner: "talkable", Repo: "talkable", Number: n, Title: "PR " + strconv.Itoa(n),
			State: state, GHState: "OPEN", ActivityAt: ago(updated)}
		if starred > 0 {
			r.Starred, r.StarredAt = true, ago(starred)
		}
		return r
	}
	needs := row(2, "reviewed", 2*time.Hour, 48*time.Hour)
	needs.NeedsMe = NeedsMeApprove
	rows := []PRBoardRow{
		row(4, "needs_attention", time.Minute, 0), // the newest update, the most urgent state: unstarred
		needs,
		row(1, "queued", 2*time.Hour, time.Hour),
		row(3, "baseline", time.Hour, time.Minute), // starred last, updated first
	}
	want := []string{"talkable/talkable#3", "talkable/talkable#1", "talkable/talkable#2", "talkable/talkable#4"}
	for _, by := range PRSorts() {
		for _, desc := range []bool{true, false} {
			if got := refsOfFull(SortPRBoard(rows, by, desc)); !slices.Equal(got, want) {
				t.Errorf("%s desc=%v: %v, want %v", by, desc, got, want)
			}
		}
	}
}

// A starred PR GitHub merged or closed leads the recently closed section,
// which stays the board's last block under its heading, in every sort; the
// rest of the section keeps its newest-closed-first order, and a starred
// open PR leads the open rows, not the section.
func TestAStarredMergedPRLeadsTheRecentSection(t *testing.T) {
	rows := starRows(recentRows(), ago(time.Minute), "talkable/talkable#11992", "talkable/talkable#11000")
	section := []string{"talkable/talkable#11992", "talkable/talkable#11991", "talkable/talkable#11990"} // #11992 closed 5h ago, the oldest
	for _, by := range PRSorts() {
		for _, desc := range []bool{true, false} {
			refs := refsOfFull(SortPRBoard(rows, by, desc))
			if refs[0] != "talkable/talkable#11000" || !slices.Equal(refs[7:], section) {
				t.Errorf("%s desc=%v: %v, want #11000 first and the section %v last", by, desc, refs, section)
			}
		}
	}

	m := recentBoard(t, 220, 40, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: rows})
	if m.section != 7 || !slices.Equal(boardRefs(m)[7:], section) {
		t.Fatalf("section at %d: %v", m.section, boardRefs(m))
	}
	lines := strings.Split(viewOf(m), "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "merged or closed in the last 24h") })
	if at < 1 || !strings.Contains(lines[at+1], "#11992") || !strings.Contains(lines[at+1], "✪ Speed up the campaign list query") {
		t.Fatalf("the heading is not followed by the starred merged PR:\n%s", strings.Join(lines, "\n"))
	}
}

// A star never shows a row the board hides: h hides a starred ignored or
// skipped PR, the owner scope a starred PR of another owner, a view and the
// filter the starred PRs they leave out.
func TestStarsNeverShowAHiddenRow(t *testing.T) {
	rows := append(boardRows(),
		PRBoardRow{Ref: "talkable/talkable#11801", Owner: "talkable", Repo: "talkable", Number: 11801, Title: "Ignored one",
			Author: "hal", State: "ignored", GHState: "OPEN", Muted: true, ActivityAt: ago(time.Hour)},
		PRBoardRow{Ref: "talkable/talkable#11802", Owner: "talkable", Repo: "talkable", Number: 11802, Title: "Skipped one",
			Author: "hal", State: "ineligible", SkipReason: "bot author", GHState: "OPEN", ActivityAt: ago(time.Hour)},
		PRBoardRow{Ref: "zhuravel/dotfiles#5", Owner: "zhuravel", Repo: "dotfiles", Number: 5, Title: "Another owner's",
			Author: "zhuravel", State: "reviewed", GHState: "OPEN", ActivityAt: ago(time.Hour)},
	)
	rows = starRows(rows, ago(time.Minute), "talkable/talkable#11801", "talkable/talkable#11802", "zhuravel/dotfiles#5", "talkable/talkable#11000")
	board := func(opts PRBoardOptions) prBoardModel {
		m, _, _ := newBoard(t, 200, 40, opts)
		m, _ = send(t, m, prbDataMsg{rows: rows})
		return m
	}
	for name, c := range map[string]struct {
		opts   PRBoardOptions
		keys   []string
		hidden []string
		first  string
	}{
		"h":      {PRBoardOptions{HideSkipped: true}, nil, []string{"talkable/talkable#11801", "talkable/talkable#11802"}, "zhuravel/dotfiles#5"},
		"owner":  {PRBoardOptions{DefaultOwner: "talkable"}, nil, []string{"zhuravel/dotfiles#5"}, "talkable/talkable#11801"},
		"view":   {PRBoardOptions{DefaultView: ViewMine}, nil, []string{"talkable/talkable#11000", "talkable/talkable#11801", "talkable/talkable#11802", "zhuravel/dotfiles#5"}, ""},
		"filter": {PRBoardOptions{}, []string{"/", "o", "a", "u", "t", "h", "enter"}, []string{"talkable/talkable#11000", "talkable/talkable#11801", "zhuravel/dotfiles#5"}, "talkable/talkable#11950"},
	} {
		m := board(c.opts)
		m, _ = send(t, m, keys(c.keys...)...)
		refs := boardRefs(m)
		for _, h := range c.hidden {
			if slices.Contains(refs, h) {
				t.Errorf("%s: the starred %s shows: %v", name, h, refs)
			}
		}
		if c.first != "" && (len(refs) == 0 || refs[0] != c.first) {
			t.Errorf("%s: rows %v, want %s first", name, refs, c.first)
		}
	}
}

// --limit caps the live board after its view, filter and sort, as the
// printed rows do: the sort decides which rows show, and an old starred PR
// shows first rather than falling off before it floats.
func TestTheLiveBoardLimitsAfterSorting(t *testing.T) {
	m, _, _ := newBoard(t, 200, 40, PRBoardOptions{Limit: 3})
	if got, want := boardRefs(m), refsOfFull(SortPRBoard(boardRows(), SortUpdated, true))[:3]; !slices.Equal(got, want) {
		t.Fatalf("limit 3: %v, want %v", got, want)
	}
	m, _ = send(t, m, keyMsg("s")) // last review: the most recent reviews, not the most recent updates
	if got, want := boardRefs(m), refsOfFull(SortPRBoard(boardRows(), SortLastReview, true))[:3]; !slices.Equal(got, want) {
		t.Errorf("limit 3, last-review sort: %v, want %v", got, want)
	}

	rows := starRows(boardRows(), ago(time.Minute), "talkable/talkable#11000") // updated 21d ago, the oldest
	m, _, _ = newBoard(t, 200, 40, PRBoardOptions{Limit: 3})
	m, _ = send(t, m, prbDataMsg{rows: rows})
	want := append([]string{"talkable/talkable#11000"}, refsOfFull(SortPRBoard(unstarredOf(rows), SortUpdated, true))[:2]...)
	if got := boardRefs(m); !slices.Equal(got, want) {
		t.Errorf("limit 3 with an old star: %v, want %v", got, want)
	}
	m, _ = send(t, m, keyMsg("S"))
	want = append([]string{"talkable/talkable#11000"}, refsOfFull(SortPRBoard(unstarredOf(rows), SortUpdated, false))[:2]...)
	if got := boardRefs(m); !slices.Equal(got, want) {
		t.Errorf("limit 3 reversed: %v, want %v", got, want)
	}
	m, _ = send(t, m, keys("/", "r", "e", "f", "e", "r", "r", "a", "l", "enter")...)
	if got := boardRefs(m); !slices.Equal(got, []string{"talkable/talkable#11902", "talkable/talkable#11920"}) {
		t.Errorf("limit 3 after the filter: %v", got)
	}
}

// The star is the title's first tag, before the pin and the attention mark,
// in yellow with a space after it, in every mode; ★ stays the mark of yours.
// The summary counts the starred PRs after the pins, the card says when the
// PR was starred and how to unstar it, and the legend explains the mark.
func TestTheStarLeadsTheTitleTags(t *testing.T) {
	for mode, want := range map[IconMode]struct{ tags, mine string }{
		IconsUnicode: {"✪ 📌 ! Referral analytics", "★"},
		IconsNerd:    {"\uf4cd \uf435 \uf530 Referral analytics", "\uf51f"},
		IconsASCII:   {"star pin ! Referral analytics", "*"},
	} {
		rows := boardRows()
		for i := range rows {
			if rows[i].Number == 11902 {
				rows[i].Pinned, rows[i].Starred, rows[i].StarredAt = true, true, ago(2*time.Hour)
			}
		}
		m := iconBoard(t, mode, 240, 40, rows...)
		g, pal := newGlyphs(mode), m.pal
		raw := m.View().Content
		line := lineWith(t, raw, "#11902")
		if !strings.Contains(ansi.Strip(line), want.tags) {
			t.Errorf("%s: the title cell lacks %q:\n%s", mode, want.tags, ansi.Strip(line))
		}
		if !strings.Contains(line, pal.yellow.Render(g.star+" ")) {
			t.Errorf("%s: the star is not yellow: %q", mode, line)
		}
		if g.star == g.mine || g.mine != want.mine {
			t.Errorf("%s: star %q, mine %q", mode, g.star, g.mine)
		}
		mustContain(t, ansi.Strip(lineWith(t, raw, "pinned")), g.pin+" 2 pinned   "+g.star+" 1 starred")

		card, _ := send(t, m, keyMsg("g"), keyMsg("enter"))
		if r, _ := card.selected(); r.Number != 11902 {
			t.Fatalf("%s: the card is #%d's", mode, r.Number)
		}
		mustContain(t, viewOf(card), g.star+" starred 2h ago (* unstars)", "* unstar")

		p := m.painter()
		mustContain(t, ansi.Strip(strings.Join(p.helpContent(240), "\n")), g.star+" starred: floats to the top, * toggles; "+g.mine+" is still yours",
			"star or unstar: starred PRs lead their section")
	}
}

// * stars the cursor row at once, with no question, and the reload keeps
// the cursor on that PR wherever the star moves it; * again unstars it. The
// table's hints, the card and the menu say which one * does.
func TestStarTogglesWithoutAQuestionAndKeepsTheCursor(t *testing.T) {
	m, src, act := newBoard(t, 220, 30, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("G"))
	if r, _ := m.selected(); r.Number != 11000 {
		t.Fatalf("G: #%d", r.Number)
	}
	mustContain(t, viewOf(m), "* star")
	if items := menuState(m.menuItems()); !items["star"] {
		t.Errorf("the menu offers no star: %v", items)
	}

	src.mu.Lock()
	src.rows = starRows(boardRows(), boardNow, "talkable/talkable#11000")
	src.mu.Unlock()
	m, follow := boardAct(t, m, "*")
	if m.confirm != nil || act.last() != "star talkable/talkable#11000" {
		t.Fatalf("*: asked %+v, called %q", m.confirm, act.last())
	}
	mustContain(t, viewOf(m), "star talkable/talkable#11000 ok")
	m, _ = send(t, m, follow...)
	if refs := boardRefs(m); refs[0] != "talkable/talkable#11000" || m.cursor != 0 {
		t.Fatalf("after the star: rows %v, cursor %d", refs, m.cursor)
	}
	mustContain(t, viewOf(m), "* unstar")
	if items := menuState(m.menuItems()); !items["unstar"] || items["star"] {
		t.Errorf("the menu of a starred row: %v", items)
	}

	src.mu.Lock()
	src.rows = boardRows()
	src.mu.Unlock()
	m, follow = boardAct(t, m, "*")
	if m.confirm != nil || act.last() != "unstar talkable/talkable#11000" {
		t.Fatalf("* again: asked %+v, called %q", m.confirm, act.last())
	}
	m, _ = send(t, m, follow...)
	if r, _ := m.selected(); r.Number != 11000 || m.cursor != len(m.view)-1 {
		t.Errorf("after the unstar: cursor %d on #%d", m.cursor, r.Number)
	}

	// The dashboard has no star: * does nothing there.
	d, _, dact := newDash(t, 220, 50)
	d, cmd := send(t, d, keyMsg("*"))
	execCmd(t, cmd)
	if d.confirm != nil || len(dact.calls) != 0 {
		t.Errorf("the dashboard's *: asked %+v, called %v", d.confirm, dact.calls)
	}
}

// The board's rows as the operator approved them, through the real painter
// at 100, 140 and 200 columns in the three modes: a starred own PR (updated
// 50h ago, failed), a starred colleague's PR (updated 90m ago) and a starred
// merged PR lead their sections, the star the first title tag. `go test -run
// TestTheStarredBoardMockup -v` prints the frames the design log quotes.
func TestTheStarredBoardMockup(t *testing.T) {
	rows := slices.DeleteFunc(recentRows(), func(r PRBoardRow) bool { return r.Number == 11800 || r.Number == 11990 })
	rows = placeholderLogins(starRows(rows, ago(time.Hour), "talkable/talkable#11902", "talkable/talkable#11950", "talkable/talkable#11992"))
	want := []string{"talkable/talkable#11950", "talkable/talkable#11902"}
	for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
		g := newGlyphs(mode)
		for _, w := range []int{100, 140, 200} {
			m := recentBoard(t, w, 16, PRBoardOptions{Icons: mode})
			m, _ = send(t, m, prbDataMsg{rows: rows}, keyMsg("g"))
			refs := boardRefs(m)
			if !slices.Equal(refs[:2], want) || refs[len(refs)-2] != "talkable/talkable#11992" || m.section != len(refs)-2 {
				t.Errorf("%s %d: rows %v, section %d", mode, w, refs, m.section)
			}
			v := viewOf(m)
			for _, n := range []string{"#11950", "#11902", "#11992"} {
				_, title, _ := strings.Cut(lineWith(t, v, n), n)
				if !strings.HasPrefix(strings.TrimSpace(title), g.star+" ") {
					t.Errorf("%s %d: %s's title cell does not start with the star: %q", mode, w, n, title)
				}
			}
			t.Logf("%s, %d columns\n%s", mode, w, v)
		}
	}
}
