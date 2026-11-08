package tui

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The factory screen's O is the PR board's owner scope: the user or
// organization owning a row's repository, in the board's order and words.

// ownersData is yoursData with its rows in repositories of the three owners
// ownerRows has on the board (talkable, example, alice), one of them
// spelled in capitals once, and one row (#4611) that names no repository.
func ownersData() FactoryData {
	d := yoursData()
	repos := map[string]string{
		"PR-12345": "talkable/talkable", "#4602": "example/widget", "#4603": "alice/dotfiles",
		"PR-12371": "talkable/talkable", "#4612": "example/widget", "#4611": "",
		"#4606": "Example/site", "PR-12302": "talkable/talkable",
	}
	for i, r := range d.Rows {
		d.Rows[i].Repo = repos[r.Key]
	}
	d.Repos = []string{"talkable/talkable"}
	return d
}

// testFactoryWith is testFactory with opts (the harness's clock and timers
// in them) on data.
func testFactoryWith(t *testing.T, w, h int, opts FactoryOptions, data FactoryData) *factoryHarness {
	t.Helper()
	clk := &testClock{at: factoryNow}
	src := &fakeFactorySource{data: data}
	act := &fakeFactoryActions{}
	opts.Now, opts.Tick = clk.now, instantTick
	hs := &factoryHarness{t: t, src: src, act: act, clock: clk, m: newFactoryModel(t.Context(), src, act, opts)}
	hs.feed(tea.WindowSizeMsg{Width: w, Height: h})
	hs.run(hs.m.Init())
	return hs
}

// testOwners is the screen on ownersData with opts.
func testOwners(t *testing.T, opts FactoryOptions) *factoryHarness {
	t.Helper()
	return testFactoryWith(t, 160, 40, opts, ownersData())
}

// title is the first line of the screen.
func (h *factoryHarness) title() string { return strings.Split(h.view(), "\n")[0] }

// O cycles the owners as the board's O does, over the same owners: every
// owner, then each owner with rows, daemon.default_repo's first and the
// rest alphabetically (case aside), then every owner again; the footer
// says which, in the board's words.
func TestOOnTheFactoryScreenCyclesTheBoardsOwners(t *testing.T) {
	for _, defaultRepo := range []string{"", "talkable/talkable", "Example/site", "nobody/else"} {
		board := ownerBoard(t, 200, 30, PRBoardOptions{DefaultRepo: defaultRepo})
		h := testOwners(t, FactoryOptions{DefaultRepo: defaultRepo})
		var want, got []string
		for range 4 {
			board, _ = send(t, board, keyMsg("O"))
			want = append(want, board.owner)
			h.press("O")
			got = append(got, h.m.owner)
			mustContain(t, h.view(), "owner: "+cmp.Or(h.m.owner, "all"))
		}
		if !slices.EqualFunc(got, want, strings.EqualFold) {
			t.Errorf("default repo %q: the factory's O cycles %q, the board's %q", defaultRepo, got, want)
		}
	}
}

// The title says the owner scope in the board's words and place: "all
// owners" while the rows span several owners, "owner alice" once O picks
// one, the name alone on a narrow title; rows of one owner say no "all
// owners". The scope hides the other owners' rows but never a row that
// names no repository, and the title counts the rows it hides. Before, O
// cycled the repositories and the title said "repo widget".
func TestTheFactoryTitleSaysTheOwnerScopeAsTheBoards(t *testing.T) {
	h := testOwners(t, FactoryOptions{})
	mustContain(t, h.title(), "magnum factory · all owners · talkable")
	h.press("O") // alice
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4603", "#4611"}) {
		t.Errorf("owner alice shows %v, want #4603 and #4611, which names no repository", got)
	}
	mustContain(t, h.title(), "magnum factory · owner alice · 6 hidden · talkable")
	mustNotContain(t, h.title(), "all owners", "repo ")
	h.press("O") // example, as its first row spells it: Example/site too
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4602", "#4612", "#4611", "#4606"}) {
		t.Errorf("owner example shows %v", got)
	}
	mustContain(t, h.title(), "owner example · 4 hidden")
	narrow, _ := renderFactory(h.m.homeFrame(), 50, 40, h.m.st, h.m.g, factoryNow)
	title := strings.Split(ansi.Strip(narrow), "\n")[0]
	mustContain(t, title, "magnum factory · example · 4 hidden")
	mustNotContain(t, title, "owner example")

	h.press("O", "/") // talkable, then a filter that matches nothing there
	h.typeText("dotfiles")
	h.press("enter")
	mustContain(t, h.view(), `Nothing matches "dotfiles" in talkable. esc clears the filter.`)

	one := ownersData()
	for i := range one.Rows {
		one.Rows[i].Repo = "example/talkable"
	}
	s := testFactoryWith(t, 160, 40, FactoryOptions{}, one)
	mustNotContain(t, s.title(), "all owners")
	s.press("O")
	mustContain(t, s.title(), "owner example")
	if h.act.calls() != 0 || h.m.bar.asking != nil || s.act.calls() != 0 {
		t.Error("the owner scope asked or ran something")
	}
}

// The screen opens in DefaultOwner's scope (case aside) and reports every
// change of it: O, and the board's rule, an owner with no rows left shows
// every owner again and says so, which is a change too. So an owner with
// no rows at the start opens every owner, as the board does.
func TestTheFactoryScreenOpensInTheOwnerAndReportsEveryChange(t *testing.T) {
	var heard []string
	hear := func(o string) { heard = append(heard, o) }
	h := testOwners(t, FactoryOptions{DefaultOwner: "Alice", OwnerChanged: hear})
	if got := h.shownKeys(); h.m.owner != "Alice" || !slices.Equal(got, []string{"#4603", "#4611"}) {
		t.Fatalf("opened in owner %q with %v, want Alice's #4603 and #4611", h.m.owner, got)
	}
	mustContain(t, h.title(), "owner Alice")
	h.press("O", "O", "O")
	if !slices.Equal(heard, []string{"example", "talkable", ""}) {
		t.Errorf("OwnerChanged heard %q", heard)
	}

	heard = nil
	h.press("O") // alice
	h.src.change(func(d *FactoryData) {
		d.Rows = slices.DeleteFunc(d.Rows, func(r FactoryRow) bool { return r.Repo == "alice/dotfiles" })
	})
	h.press("ctrl+r")
	if h.m.owner != "" || len(h.shownKeys()) != 7 || !slices.Equal(heard, []string{"alice", ""}) {
		t.Errorf("alice's last row left: owner %q, rows %v, heard %q; want every owner", h.m.owner, h.shownKeys(), heard)
	}
	mustContain(t, h.view(), "owner: all (alice has no tasks or PRs now)")

	heard = nil
	g := testOwners(t, FactoryOptions{DefaultOwner: "gone", OwnerChanged: hear})
	if g.m.owner != "" || len(g.shownKeys()) != 8 || !slices.Equal(heard, []string{""}) {
		t.Errorf("an owner with no rows: owner %q, rows %v, heard %q; want every owner", g.m.owner, g.shownKeys(), heard)
	}
	mustContain(t, g.view(), "owner: all (gone has no tasks or PRs now)")
	mustNotContain(t, g.title(), "owner gone")
}

// O on rows that name no owner says so and changes nothing.
func TestOWithNoOwnerToShowSaysSo(t *testing.T) {
	h := testFactory(t, 140, 40) // the fixture's rows name no repository
	h.press("O")
	if h.m.owner != "" {
		t.Errorf("owner %q, want every owner", h.m.owner)
	}
	mustContain(t, h.view(), "no owner to show alone")
}

// Home's hints offer O while the rows span several owners, as the board's
// offer "O owner"; rows of one owner, or of none, leave it out.
func TestHomeHintsOfferOWhileTheRowsSpanSeveralOwners(t *testing.T) {
	h := testOwners(t, FactoryOptions{})
	if !slices.Contains(h.m.homeHints(), hint{"O", "owner"}) {
		t.Errorf("rows of three owners: hints %v lack O owner", h.m.homeHints())
	}
	h.press("O") // alice alone: O still moves on
	if !slices.Contains(h.m.homeHints(), hint{"O", "owner"}) {
		t.Errorf("owner alice: hints %v lack O owner", h.m.homeHints())
	}
	if s := testFactory(t, 140, 40); slices.Contains(s.m.homeHints(), hint{"O", "owner"}) {
		t.Errorf("rows that name no owner: hints %v offer O", s.m.homeHints())
	}
}
