package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Home's / filter (R29) and O's owner scope: they change the view only, as
// the board's / and O do, and ask nothing.

// typeText types text a key at a time.
func (h *factoryHarness) typeText(text string) {
	h.t.Helper()
	for _, r := range text {
		h.press(string(r))
	}
}

// shownKeys are the keys of home's rows in the order drawn.
func (h *factoryHarness) shownKeys() []string {
	var out []string
	for _, id := range h.m.order() {
		r, _ := h.m.row(id)
		out = append(out, r.Key)
	}
	return out
}

// testRepos is testYours with its rows in two repositories: #4611 and
// #4612 of example/widget, the rest of example/talkable.
func testRepos(t *testing.T) *factoryHarness {
	t.Helper()
	h := testYours(t, 160, 40)
	h.src.change(func(d *FactoryData) {
		d.Repos = []string{"example/talkable", "example/widget"}
		for i, r := range d.Rows {
			d.Rows[i].Repo = "example/talkable"
			if r.Key == "#4611" || r.Key == "#4612" {
				d.Rows[i].Repo = "example/widget"
			}
		}
	})
	h.press("ctrl+r")
	return h
}

// / filters home's rows as you type: each word must be in a row's key,
// title, repository or PR number, in any case. The title line says the
// filter and how many rows it hides, the line under the rule shows what you
// type; enter (or a click) keeps the filter and gives the keys back, and
// esc clears it. The selection stays on its row while it shows, else goes
// to the nearest one in home's order. A NEEDS YOU the filter empties says
// nothing of what it hides. Nothing is asked or run.
func TestSlashFiltersHomesRows(t *testing.T) {
	h := testRepos(t)
	h.press("g")
	h.selectRow("#4611")
	h.press("/")
	h.typeText("COUPON")
	if got, want := h.shownKeys(), []string{"#4602", "PR-12371"}; !slices.Equal(got, want) {
		t.Fatalf("filter COUPON shows %v, want %v", got, want)
	}
	v := h.view()
	mustContain(t, v, `filter "COUPON" · 6 hidden`, "/ COUPON")
	mustContain(t, h.marked(), "/ COUPON⟦ ⟧")
	mustNotContain(t, v, "Weekly gem upgrades")
	if r, _ := h.m.selectedRow(); r.Key != "PR-12371" {
		t.Errorf("the cursor is on %s, want PR-12371, the shown row nearest #4611", r.Key)
	}
	h.press("space")
	h.typeText("csv")
	if got := h.shownKeys(); !slices.Equal(got, []string{"PR-12371"}) {
		t.Errorf("filter %q shows %v, want PR-12371", h.m.filter.Value(), got)
	}
	h.press("enter")
	if h.m.filtering {
		t.Fatal("enter did not give the keys back")
	}
	h.press("q") // a key again, not text: but not q, which quits
	if !h.quit {
		t.Error("q after enter did not reach the keys")
	}
	h.quit = false
	mustContain(t, h.view(), `filter "COUPON csv" · 7 hidden`, "esc clear filter")

	h.press("/", "esc") // esc clears it
	if h.m.filtering || h.m.filter.Value() != "" || len(h.shownKeys()) != 8 {
		t.Errorf("esc: filtering %v, filter %q, rows %v; want every row again", h.m.filtering, h.m.filter.Value(), h.shownKeys())
	}
	if r, _ := h.m.selectedRow(); r.Key != "PR-12371" {
		t.Errorf("the cursor is on %s after esc, want PR-12371, which still shows", r.Key)
	}
	h.press("/")
	h.typeText("4611")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4611"}) {
		t.Errorf("filter %q shows %v, want #4611 by its PR number", h.m.filter.Value(), got)
	}
	h.press("esc")
	if r, _ := h.m.selectedRow(); r.Key != "#4611" {
		t.Errorf("the cursor is on %s after esc, want #4611, the row it moved to", r.Key)
	}
	mustNotContain(t, h.view(), "hidden")

	h.press("/")
	h.typeText("widget")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4612", "#4611"}) {
		t.Errorf("filter widget shows %v, want the rows of example/widget", got)
	}
	// Rows none of which needs you leave NEEDS YOU empty, but a filter may
	// hide what does: no "Nothing waits on you." over them.
	mustNotContain(t, h.view(), "Nothing waits on you.")
	h.press("enter", "esc") // esc on home clears a filter kept
	if h.m.filter.Value() != "" || len(h.shownKeys()) != 8 {
		t.Errorf("esc on home kept the filter %q", h.m.filter.Value())
	}

	// The nearest shown row is the nearest in home's order, the next first:
	// from #4603 (NEEDS YOU) it is #4606 (LEFT ALONE), four rows down, not
	// the row at the cursor's old place among the two shown.
	h.press("g")
	h.selectRow("#4603")
	h.press("/")
	h.typeText("sync")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4606", "PR-12302"}) {
		t.Fatalf("filter sync shows %v", got)
	}
	if r, _ := h.m.selectedRow(); r.Key != "#4606" {
		t.Errorf("the cursor is on %s, want #4606, the shown row nearest #4603", r.Key)
	}
	// A click hands the keys back from the filter, which it keeps.
	h.feed(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseLeft})
	h.press("q")
	if !h.quit || h.m.filter.Value() != "sync" {
		t.Errorf("q after a click: quit %v, filter %q; want the keys back and the filter kept", h.quit, h.m.filter.Value())
	}
	h.quit = false
	if h.act.calls() != 0 || h.m.bar.asking != nil {
		t.Error("the filter asked or ran something")
	}
}

// A filter that matches nothing says so, with the key that clears it,
// instead of an empty home; the chips still count every row.
func TestAFilterThatMatchesNothingSaysSo(t *testing.T) {
	h := testRepos(t)
	h.press("/")
	h.typeText("nothing like it")
	h.press("enter")
	v := h.view()
	mustContain(t, v, `Nothing matches "nothing like it". esc clears the filter.`, "1 task and 2 PRs need you")
	mustNotContain(t, v, "No tasks yet", "Nothing waits on you.")
}

// A scope that shows no row says what O shows next: the next owner, or
// every one from the last.
func TestAnEmptyScopeSaysWhatONextShows(t *testing.T) {
	h := testOwners(t, FactoryOptions{})
	empty := func() string {
		f := h.m.homeFrame()
		f.Rows, f.Cursor = nil, -1 // the scope's rows all gone from view
		frame, _ := renderFactory(f, 160, 40, h.m.st, h.m.g, factoryNow)
		return ansi.Strip(frame)
	}
	h.press("O")
	mustContain(t, empty(), "Nothing of alice. O shows example.")
	h.press("O", "O")
	mustContain(t, empty(), "Nothing of talkable. O shows every owner.")
}

// a walks every decision whatever the filter and the owner scope hide,
// and the chips still count every row: no decision hides.
func TestTheWalkIgnoresTheFilterAndTheScope(t *testing.T) {
	h := testOwners(t, FactoryOptions{})
	h.press("O", "O", "/") // example alone, then gem: #4612 alone, no decision
	h.typeText("gem")
	h.press("enter")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4612"}) {
		t.Fatalf("shown %v, want #4612", got)
	}
	mustContain(t, h.view(), "1 task and 2 PRs need you")
	h.press("a")
	if h.m.view != fvQuestion || h.m.task != 2 {
		t.Fatalf("a opened view %v of %d, want PR-12345's questions, the oldest decision", h.m.view, h.m.task)
	}
	if !strings.Contains(h.view(), "PR-12345") {
		t.Error("the walk's card is not PR-12345's")
	}
}

// helpSection is the lines of the help's section title, trimmed.
func helpSection(t *testing.T, help, title string) []string {
	t.Helper()
	_, sec, ok := strings.Cut(help, "\n"+title+"\n")
	if !ok {
		t.Fatalf("the help has no %s section:\n%s", title, help)
	}
	sec, _, _ = strings.Cut(sec, "\n\n")
	var out []string
	for l := range strings.SplitSeq(sec, "\n") {
		out = append(out, strings.TrimSpace(l))
	}
	return out
}

// The help lists the new keys: r R, z and M U on home and on the task card
// (y/N, on a row with a PR), / and O on home, the r rule on the task card
// (r retries where the card offers it, else reviews), and the keys of your
// PR's card; it glosses the marks M and z leave on a row and what the title
// line says of the filter and the scope.
func TestTheHelpListsThePRKeysTheFilterAndTheScope(t *testing.T) {
	h := testFactory(t, 140, 50)
	help := ansi.Strip(strings.Join(h.m.helpContent(240), "\n"))
	has := func(lines []string, prefix string, words ...string) bool {
		return slices.ContainsFunc(lines, func(l string) bool {
			if !strings.HasPrefix(l, prefix) {
				return false
			}
			for _, w := range words {
				if !strings.Contains(l, w) {
					return false
				}
			}
			return true
		})
	}
	home := helpSection(t, help, "Home")
	for _, k := range [][]string{{"r R ", "review", "y/N"}, {"z ", "snooze", "y/N"}, {"M U ", "mute", "y/N"}, {"/ ", "filter", "esc"},
		{"O ", "owner", "board"}} {
		if !has(home, k[0], k[1:]...) {
			t.Errorf("the help's Home lacks %q with %q:\n%s", k[0], k[1:], strings.Join(home, "\n"))
		}
	}
	card := helpSection(t, help, "Task card")
	if !has(card, "r ", "retry", "review") || !has(card, "R z M U ", "y/N") || !has(card, "your PR's card", "r R z M U") {
		t.Errorf("the help's Task card lacks the r rule, R z M U or your PR's card's keys:\n%s", strings.Join(card, "\n"))
	}
	mustContain(t, help, "muted  ", "snoozed → 16:09  ", `filter "coupon" · 6 hidden  `)
	if l := lineOf(t, help, `filter "coupon" · 6 hidden  `); !strings.Contains(l, "O shows the next owner, after the last every one") {
		t.Errorf("the scope's gloss does not say where O moves: %q", l)
	}
}
