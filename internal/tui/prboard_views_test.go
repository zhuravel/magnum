package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestFilterPRBoardViews(t *testing.T) {
	sorted := func(rows []PRBoardRow) []string { return refsOf(SortPRBoard(rows, SortUpdated, true)) }
	for _, tc := range []struct {
		view PRView
		self []string
		want []string
	}{
		{ViewAll, boardSelf, []string{"magnum#42", "talkable#11931", "talkable#11950", "talkable#11920", "talkable#11902", "talkable#11800", "talkable#11000"}},
		// Everything magnum touched: not the baseline row.
		{ViewMagnum, boardSelf, []string{"magnum#42", "talkable#11931", "talkable#11950", "talkable#11920", "talkable#11902", "talkable#11800"}},
		// Assigned to zhuravel.
		{ViewMine, boardSelf, []string{"talkable#11950"}},
		// dan's review is requested on #11920; bob is its assignee.
		{ViewMine, []string{"dan", "bob"}, []string{"talkable#11920"}},
		// Approved, nothing blocking, open, not a draft: #11920 and #11902
		// carry a CHANGES_REQUESTED, #11800 is merged.
		{ViewReady, boardSelf, []string{"talkable#11950"}},
	} {
		got := sorted(FilterPRBoard(boardRows(), tc.view, tc.self))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s (self %v) = %v, want %v", tc.view, tc.self, got, tc.want)
		}
	}
	// bob authored magnum#42, but authorship does not make it "mine";
	// only the request and the assignment count.
	if got := sorted(FilterPRBoard(boardRows(), ViewMine, []string{"bob"})); !slices.Equal(got, []string{"talkable#11920"}) {
		t.Errorf("mine for bob = %v", got)
	}
}

func TestParsePRView(t *testing.T) {
	for in, want := range map[string]PRView{"": ViewAll, "All": ViewAll, " magnum ": ViewMagnum, "MINE": ViewMine, "ready": ViewReady} {
		if got, err := ParsePRView(in); err != nil || got != want {
			t.Errorf("ParsePRView(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParsePRView("everything"); err == nil || !strings.Contains(err.Error(), "all, magnum, mine, ready") {
		t.Errorf("unknown view error = %v", err)
	}
	if got := PRViews(); !slices.Equal(got, []PRView{ViewAll, ViewMagnum, ViewMine, ViewReady}) {
		t.Errorf("PRViews = %v", got)
	}
}

// v cycles the views, the title names the view and its row count, the
// filter narrows within it and the choice is reported for the next board.
func TestPRBoardViewKeyCycles(t *testing.T) {
	var heard []PRView
	m, _, _ := newBoard(t, 160, 24, PRBoardOptions{ViewChanged: func(v PRView) { heard = append(heard, v) }})
	mustContain(t, lineWith(t, viewOf(m), "magnum · pull requests"), "view all")

	m, _ = send(t, m, keyMsg("v"))
	if m.boardView != ViewMagnum || len(m.view) != 6 {
		t.Fatalf("after v: view %s with %d rows", m.boardView, len(m.view))
	}
	mustContain(t, lineWith(t, viewOf(m), "magnum · pull requests"), "view magnum 6")
	mustNotContain(t, viewOf(m), "#11000")

	m, _ = send(t, m, keyMsg("v"))
	if got := boardRefs(m); m.boardView != ViewMine || !slices.Equal(got, []string{"talkable/talkable#11950"}) {
		t.Fatalf("mine = %s %v", m.boardView, got)
	}
	m, _ = send(t, m, keyMsg("v"))
	if m.boardView != ViewReady {
		t.Fatalf("third v = %s", m.boardView)
	}
	m, _ = send(t, m, keyMsg("/"))
	m, _ = send(t, m, typed("rails")...)
	mustContain(t, viewOf(m), `no PR matches "rails"`, "0 of 1 match")
	m, _ = send(t, m, keyMsg("esc"), keyMsg("v"))
	if m.boardView != ViewAll || len(m.view) != 7 {
		t.Fatalf("fourth v = %s with %d rows", m.boardView, len(m.view))
	}
	if !slices.Equal(heard, []PRView{ViewMagnum, ViewMine, ViewReady, ViewAll}) {
		t.Fatalf("ViewChanged heard %v", heard)
	}

	// The default view, and an empty one says how to get out of it.
	e, _, _ := newBoard(t, 160, 24, PRBoardOptions{DefaultView: ViewMine, SelfLogins: []string{"nobody"}})
	mustContain(t, viewOf(e), "view mine 0", "no PR in the mine view · v shows the next view")
	mustContain(t, viewOf(e), "v view")
}

func TestPRBoardFilterQualifiers(t *testing.T) {
	for _, tc := range []struct {
		q    string
		self []string
		want []string
	}{
		{"state:needs", nil, []string{"talkable#11902"}},
		{"state:rereview", nil, []string{"talkable#11950"}},
		{"state:re-review", nil, []string{"talkable#11950"}},                     // the label the board shows
		{"state:queued,reviewing", nil, []string{"magnum#42", "talkable#11931"}}, // alternatives
		{"state:", nil, []string{"magnum#42", "talkable#11931", "talkable#11950", "talkable#11920", "talkable#11902", "talkable#11800", "talkable#11000"}},
		{"assignee:@me", nil, []string{"talkable#11950"}},
		{"assignee:bo", nil, []string{"talkable#11920"}},
		{"author:@me", nil, []string{"talkable#11902"}},
		{"Author:DEPENDABOT", nil, []string{"talkable#11931"}},
		{"review:requested", nil, []string{}},
		{"review:requested", []string{"dan"}, []string{"talkable#11920"}},
		{"review:req", []string{"eve"}, []string{"talkable#11931"}},
		{"state:rereview oauth", nil, []string{"talkable#11950"}}, // a qualifier and a word
		{"state:reviewed oauth", nil, []string{}},
		{"analytics:", nil, []string{"talkable#11902"}}, // not a qualifier: free text
	} {
		m, _, _ := newBoard(t, 160, 24, PRBoardOptions{SelfLogins: tc.self})
		m, _ = send(t, m, keyMsg("/"))
		m, _ = send(t, m, typed(tc.q)...)
		if got := refsOf(m.view); !slices.Equal(got, tc.want) {
			t.Errorf("filter %q (self %v) = %v, want %v", tc.q, tc.self, got, tc.want)
		}
	}
}

func TestPRBoardCardShowsLastRoundTimings(t *testing.T) {
	m, src, _ := newBoard(t, 120, 60, PRBoardOptions{})
	rows := boardRows()
	for i := range rows {
		if rows[i].Number == 11920 {
			rows[i].LastRound = &RoundTimings{Round: 3, Kind: "rereview", Total: 34*time.Minute + 10*time.Second, Running: true, Stages: []StageTiming{
				{Name: "fetch/checkout", Duration: 12 * time.Second},
				{Name: "claude-review", Duration: 18*time.Minute + 4*time.Second},
				{Name: "codex-review", Duration: 9 * time.Minute, Failed: true},
				{Name: "codex-judge\x1b[31m", Duration: 4 * time.Minute, Running: true},
			}}
		}
	}
	src.rows = rows
	m, _ = send(t, m, prbDataMsg{rows: rows})
	m, _ = send(t, m, keyMsg("j"), keyMsg("j"), keyMsg("j"), keyMsg("enter"))
	if r, _ := m.selected(); r.Number != 11920 {
		t.Fatalf("selected %d", r.Number)
	}
	v := viewOf(m)
	mustContain(t, v, "LAST ROUND (3, rereview)", "fetch/checkout 12s", "claude-review 18m04s", "codex-review 9m00s failed",
		"codex-judge 4m00s running", "total 34m10s running")
	if strings.Contains(m.View().Content, "\x1b[31m4m") {
		t.Error("a stage name's escape sequence reached the screen")
	}

	// No round ran: no section.
	m, _ = send(t, m, keyMsg("esc"), keyMsg("k"), keyMsg("enter"))
	mustNotContain(t, viewOf(m), "LAST ROUND")
}

func TestStageDurationAndTimingsText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 12 * time.Second: "12s", 18*time.Minute + 4*time.Second: "18m04s", 62*time.Minute + 30*time.Second: "1h02m",
	} {
		if got := StageDuration(d); got != want {
			t.Errorf("StageDuration(%s) = %q, want %q", d, got, want)
		}
	}
	got := TimingsText(RoundTimings{Total: 20 * time.Minute, Stages: []StageTiming{
		{Name: "fetch/checkout", Duration: 5 * time.Second}, {Name: "judge", Duration: time.Minute, Failed: true}, {Name: "verify", Duration: 0, Running: true},
	}})
	if want := "fetch/checkout 5s · judge 1m00s (failed) · verify 0s (running) · total 20m00s"; got != want {
		t.Errorf("TimingsText = %q, want %q", got, want)
	}
}

func TestRenderPRBoardDefaultView(t *testing.T) {
	out := ansi.Strip(RenderPRBoard(boardRows(), 200, PRBoardOptions{DefaultView: ViewReady, SelfLogins: boardSelf, Now: func() time.Time { return boardNow }}))
	mustContain(t, out, "view ready 1", "#11950")
	mustNotContain(t, out, "#11920", "#11931")
}
