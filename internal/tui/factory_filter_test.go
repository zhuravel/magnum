package tui

import (
	"slices"
	"strings"
	"testing"
)

// Home's / filter and O repository scope (R29): they change the view
// only, as the board's / and O do, and ask nothing.

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
// type; enter keeps the filter and gives the keys back, and esc clears it.
// The selection stays on its row while it shows, else goes to the nearest
// one. Nothing is asked or run.
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
	mustContain(t, v, `filter "COUPON" · 6 hidden`, "/ COUPON▏")
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
	mustContain(t, h.view(), `filter "COUPON csv" · 7 hidden`)

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
	h.press("enter", "esc") // esc on home clears a filter kept
	if h.m.filter.Value() != "" || len(h.shownKeys()) != 8 {
		t.Errorf("esc on home kept the filter %q", h.m.filter.Value())
	}
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

// O cycles the repository scope: every repository, then each one with rows
// (sorted), then every one again. The title line names the scope and how
// many rows it hides; the scope and the filter combine. A scope whose
// repository has no rows any more falls back to every repository.
func TestOCyclesTheRepositoryScope(t *testing.T) {
	h := testRepos(t)
	h.press("O")
	v := h.view()
	mustContain(t, v, "repo talkable · 2 hidden")
	mustNotContain(t, v, "Weekly gem upgrades")
	h.press("O")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4612", "#4611"}) {
		t.Errorf("scope widget shows %v", got)
	}
	mustContain(t, h.view(), "repo widget · 6 hidden")
	h.press("O")
	if len(h.shownKeys()) != 8 {
		t.Errorf("the third O shows %v, want every row", h.shownKeys())
	}
	mustNotContain(t, h.view(), "repo ", "hidden")

	h.press("O", "/")
	h.typeText("coupon")
	h.press("enter")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4602", "PR-12371"}) {
		t.Errorf("scope talkable and filter coupon show %v", got)
	}
	mustContain(t, h.view(), `repo talkable · filter "coupon" · 6 hidden`)
	h.press("O") // widget: no coupon there
	mustContain(t, h.view(), `Nothing matches "coupon" in widget. esc clears the filter.`)

	h.press("esc") // widget alone
	h.src.change(func(d *FactoryData) {
		d.Rows = slices.DeleteFunc(d.Rows, func(r FactoryRow) bool { return r.Repo == "example/widget" })
	})
	h.press("ctrl+r")
	if h.m.scope != "" || len(h.shownKeys()) != 6 {
		t.Errorf("scope %q with no rows of it left shows %v, want every repository", h.m.scope, h.shownKeys())
	}
	mustNotContain(t, h.view(), "repo widget")
	if h.act.calls() != 0 || h.m.bar.asking != nil {
		t.Error("the scope asked or ran something")
	}
}

// a walks every decision whatever the filter and the scope hide: no
// decision hides from it.
func TestTheWalkIgnoresTheFilterAndTheScope(t *testing.T) {
	h := testRepos(t)
	h.press("O", "O", "/") // widget alone, then gem: #4612 alone, no decision
	h.typeText("gem")
	h.press("enter")
	if got := h.shownKeys(); !slices.Equal(got, []string{"#4612"}) {
		t.Fatalf("shown %v, want #4612", got)
	}
	h.press("a")
	if h.m.view != fvQuestion || h.m.task != 2 {
		t.Fatalf("a opened view %v of %d, want PR-12345's questions, the oldest decision", h.m.view, h.m.task)
	}
	if !strings.Contains(h.view(), "PR-12345") {
		t.Error("the walk's card is not PR-12345's")
	}
}
