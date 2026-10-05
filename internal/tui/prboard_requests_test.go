package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// requestRows are PRs with every kind of review request: one asked of me (and
// earlier of someone else), one asked of someone else only, one never asked,
// and one asked of me five days ago and of a team an hour ago. "zhuravel" is
// me (boardSelf).
func requestRows() []PRBoardRow {
	ask := func(to, by string, d time.Duration) RequestInfo {
		return RequestInfo{To: to, By: by, At: ago(d), Mine: to == "zhuravel"}
	}
	row := func(n int, title string, toMe, last *RequestInfo, all ...RequestInfo) PRBoardRow {
		return PRBoardRow{Ref: "example#" + strconv.Itoa(n), Owner: "talkable", Repo: "example", Number: n, Title: title,
			State: "reviewed", GHState: "OPEN", UpdatedAt: ago(time.Duration(n) * time.Minute), HeadSHA: "abcdef1234",
			RequestedToMe: toMe, LastRequest: last, Requests: all}
	}
	me, other := ask("zhuravel", "alice", 2*time.Hour), ask("bob", "alice", 72*time.Hour)
	team, old := ask("team:core", "", time.Hour), ask("zhuravel", "alice", 5*24*time.Hour)
	return []PRBoardRow{
		row(30, "Asked of me", &me, &me, me, other),
		row(31, "Asked of someone else", nil, &other, other),
		row(32, "Never asked", nil, nil),
		row(33, "Asked of me, then of a team", &old, &team, team, old),
	}
}

// The REQUESTED column shows the star and the age of the latest request to me,
// else the age of the latest request to anyone, dimmed; a dash when there is
// none. Ages line up behind the star when some row has one, in every icon mode.
func TestPRBoardRequestedCell(t *testing.T) {
	want := map[IconMode][]string{
		IconsUnicode: {"★ 2h", "  3d", "—", "★ 5d"},
		IconsNerd:    {"\uf51f 2h", "  3d", "—", "\uf51f 5d"},
		IconsASCII:   {"* 2h", "  3d", "-", "* 5d"},
	}
	rows := requestRows()
	pal := newPRBPalette(defaultStyles)
	for mode, cells := range want {
		p := newPRBPainter(defaultStyles, pal, newGlyphs(mode), boardNow, nil, rows, SortUpdated, true)
		for i, r := range rows {
			if got := ansi.Strip(p.requestedCell(r).render(nil)); got != cells[i] {
				t.Errorf("%s %s: requested cell %q, want %q", mode, prRef(r), got, cells[i])
			}
		}
	}

	p := newPRBPainter(defaultStyles, pal, newGlyphs(IconsUnicode), boardNow, nil, rows, SortUpdated, true)
	mine, other := p.requestedCell(rows[0]), p.requestedCell(rows[1])
	if mine[0].st.GetForeground() != pal.mine.GetForeground() {
		t.Error("a request to me: the star is not in the mine style")
	}
	if mine[1].st.GetForeground() == pal.mine.GetForeground() {
		t.Error("a request to me: the age is in the mine style too")
	}
	if last := other[len(other)-1]; last.st.GetForeground() != defaultStyles.Dim.GetForeground() {
		t.Error("a request to someone else: the age is not dim")
	}

	// Nobody asks me: nothing to line the ages up behind.
	p = newPRBPainter(defaultStyles, pal, newGlyphs(IconsUnicode), boardNow, nil, rows[1:3], SortUpdated, true)
	if got := ansi.Strip(p.requestedCell(rows[1]).render(nil)); got != "3d" {
		t.Errorf("no request to me on the board: cell %q, want %q", got, "3d")
	}

	// Self logins make a request mine only through RequestedToMe: the summary decides.
	r := PRBoardRow{LastRequest: &RequestInfo{To: "zhuravel", At: ago(time.Minute)}}
	if got := ansi.Strip(p.requestedCell(r).render(nil)); got != "1m" {
		t.Errorf("a last request alone: cell %q, want its age", got)
	}
	if got := ansi.Strip(p.requestedCell(PRBoardRow{LastRequest: &RequestInfo{To: "bob"}}).render(nil)); got != "—" {
		t.Errorf("a request without a time: cell %q, want a dash", got)
	}
}

// The requested sort puts the newest request first, the one the column shows
// (mine, else the latest of all); PRs without a request come last either way.
func TestSortPRBoardRequested(t *testing.T) {
	rows := requestRows()
	for _, c := range []struct {
		desc bool
		want []string
	}{
		{true, []string{"example#30", "example#31", "example#33", "example#32"}},
		{false, []string{"example#33", "example#31", "example#30", "example#32"}},
	} {
		if got := refsOf(SortPRBoard(rows, SortRequested, c.desc)); !slices.Equal(got, c.want) {
			t.Errorf("SortPRBoard(requested, desc=%t) =\n %v\nwant\n %v", c.desc, got, c.want)
		}
	}
	if sortColumn(SortRequested) != colRequested {
		t.Error("the requested sort is not drawn on the REQUESTED heading")
	}
}

// s reaches the requested sort, the title bar and the heading say so, and the
// column drops out before UPDATED on a narrow screen.
func TestPRBoardRequestedColumnAndSort(t *testing.T) {
	m := iconBoard(t, IconsUnicode, 170, 20, requestRows()...)
	v := viewOf(m)
	mustContain(t, v, "REQUESTED", "★ 2h", "★ 5d")
	if got := ansi.Strip(lineWith(t, m.View().Content, "Asked of someone else")); !strings.Contains(got, "  3d") {
		t.Errorf("the request to someone else: %q", got)
	}
	m, _ = send(t, m, keyMsg("s"), keyMsg("s"), keyMsg("s"))
	if m.sort != SortRequested || !m.desc {
		t.Fatalf("sort after s s s = %s desc=%t, want requested", m.sort, m.desc)
	}
	mustContain(t, viewOf(m), "sorted by requested ↓", "REQUESTED ↓")
	if got := boardRefs(m); got[0] != "example#30" || got[3] != "example#32" {
		t.Errorf("requested order = %v", got)
	}
}

// The card lists the latest request to each reviewer, newest first, mine
// starred, with who asked and how long ago; it wraps under its label and is
// absent when the PR shows no request.
func TestPRBoardCardListsRequests(t *testing.T) {
	card := func(mode IconMode, w, n int) string {
		t.Helper()
		m := iconBoard(t, mode, w, 80, requestRows()...)
		for range n {
			m, _ = send(t, m, keyMsg("j"))
		}
		m, _ = send(t, m, keyMsg("enter"))
		return viewOf(m)
	}
	mustContain(t, card(IconsUnicode, 120, 0), "Requested: ★ zhuravel by alice 2h ago · bob by alice 3d ago")
	mustContain(t, card(IconsNerd, 120, 0), "Requested: \uf51f zhuravel by alice 2h ago · bob by alice 3d ago")
	mustContain(t, card(IconsASCII, 120, 0), "Requested: * zhuravel by alice 2h ago | bob by alice 3d ago")
	mustContain(t, card(IconsUnicode, 120, 1), "Requested: bob by alice 3d ago")
	mustNotContain(t, card(IconsUnicode, 120, 2), "Requested:")
	// A request nobody is named for (a deleted account) and a team.
	mustContain(t, card(IconsUnicode, 120, 3), "Requested: team:core 1h ago · ★ zhuravel by alice 5d ago")

	v := card(IconsUnicode, 56, 0)
	mustContain(t, v, "Requested: ★ zhuravel by alice 2h ago", "bob by alice 3d ago")
	if w := maxLineWidth(v); w > 56 {
		t.Errorf("a line of the narrow card is %d cells wide:\n%s", w, v)
	}
	first, next := lineWith(t, v, "Requested:"), lineWith(t, v, "bob by alice")
	if strings.Contains(next, "Requested:") || strings.Index(first, "★") != strings.Index(next, "bob") {
		t.Errorf("the wrapped request is not under the first one:\n%s\n%s", first, next)
	}
}

// The help's legend explains the REQUESTED column.
func TestPRBoardHelpExplainsRequested(t *testing.T) {
	m := iconBoard(t, IconsUnicode, 200, 90, requestRows()...)
	m, _ = send(t, m, keyMsg("?"), keyMsg("G"))
	mustContain(t, viewOf(m), "REQUESTED   ★ 2h: a review asked of you 2h ago   3d the latest ask of someone else")
}

// A request's reviewer and asker reach the screen without escape sequences or
// control characters, and cleaning a row leaves its source untouched.
func TestPRBoardSanitizesRequests(t *testing.T) {
	dirty := RequestInfo{To: "zhu\x1b[31mravel", By: "al\x07ice", At: ago(time.Hour), Mine: true}
	r := PRBoardRow{Ref: "example#1", RequestedToMe: &dirty, LastRequest: &dirty, Requests: []RequestInfo{dirty}}
	got := sanitizeRow(r)
	for _, q := range []RequestInfo{*got.RequestedToMe, *got.LastRequest, got.Requests[0]} {
		if q.To != "zhuravel" || q.By != "al ice" || !q.Mine || !q.At.Equal(dirty.At) {
			t.Errorf("sanitized request = %+v", q)
		}
	}
	if dirty.To != "zhu\x1b[31mravel" || r.Requests[0].By != "al\x07ice" {
		t.Errorf("sanitizing changed the source row: %+v", dirty)
	}
}
