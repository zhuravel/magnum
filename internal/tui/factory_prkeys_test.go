package tui

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// publishedData is fixturePublished as the screen's source serves it: the
// rows of the tasks whose open PR magnum's registry has carry it
// (factory.ScreenData fills FactoryRow.PR), #4531 in a fix loop of
// magnum's, and every task has its card with its PR's page.
func publishedData() FactoryData {
	f := fixturePublished()
	prs := map[int64]*FactoryPR{
		1: {Ref: "example/talkable#4530"},
		2: {Ref: "example/talkable#4534"},
		3: {Ref: "example/talkable#4531", Fixing: true},
		4: {Ref: "example/talkable#4533"},
		5: {Ref: "example/talkable#4535"},
	}
	cards := map[int64]FactoryCards{}
	for i, r := range f.Rows {
		f.Rows[i].PR = prs[r.ID]
		cards[r.ID] = FactoryCards{Task: TaskCard{Key: r.Key, Title: r.Title, PR: r.GitHub,
			PRURL: "https://github.com/example/talkable/pull/" + strings.TrimPrefix(r.GitHub, "#")}}
	}
	return FactoryData{Repos: f.Repos, Rows: f.Rows, Cards: cards}
}

// testPublished is the screen at w x h over publishedData.
func testPublished(t *testing.T, w, h int) *factoryHarness {
	t.Helper()
	hs := testFactory(t, w, h)
	hs.src.change(func(d *FactoryData) { *d = publishedData() })
	hs.press("ctrl+r")
	return hs
}

// f, F and K work on a task's row whose PR magnum's registry has, as on
// your PR's row and through the same request paths (Fix, FixOff, Abort):
// each asks y/N naming the task and its PR, and only y runs it; K with no
// fix loop says so. Before, every fix key refused a task's row ("f, F and
// x act on your PRs: PR-12320 is a task"), though magnum's loop runs on
// the factory's own PRs.
func TestTheFixKeysWorkOnATaskRowWithAPR(t *testing.T) {
	h := testPublished(t, 140, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("f")
	mustContain(t, h.view(), "Fix PR-12320's #4530: magnum's fixer takes what is open now? y/N")
	h.press("n")
	if h.act.calls() != 0 {
		t.Fatal("n ran the fix")
	}
	h.press("f", "y", "F")
	mustContain(t, h.view(), "Switch fix off for PR-12320's #4530: autofix leaves it to you? y/N")
	h.press("y", "K")
	v := h.view()
	mustContain(t, v, "no fix loop runs on PR-12320's #4530")
	mustNotContain(t, v, "y/N")
	h.selectRow("PR-12321")
	h.press("K")
	mustContain(t, h.view(), "Stop what magnum runs on PR-12321's #4531 (its fix loop and the re-review the loop forced)? y/N")
	h.press("y")
	if len(h.act.fixes) != 1 || h.act.fixes[0] != "example/talkable#4530" {
		t.Errorf("Fix calls %q, want example/talkable#4530", h.act.fixes)
	}
	if want := (factoryFixOff{"example/talkable#4530", true}); len(h.act.fixOffs) != 1 || h.act.fixOffs[0] != want {
		t.Errorf("FixOff calls %+v, want %+v", h.act.fixOffs, want)
	}
	if len(h.act.aborts) != 1 || h.act.aborts[0] != "example/talkable#4531" {
		t.Errorf("Abort calls %q, want example/talkable#4531", h.act.aborts)
	}
	if h.act.calls() != 3 {
		t.Errorf("%d calls, want the fix, the fix off and the abort", h.act.calls())
	}
}

// K stops magnum's loop on your PR's row too, as the board's K does.
func TestKOnAnOwnPRRowStopsTheLoop(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("g")
	h.selectRow("#4612")
	h.press("K")
	mustContain(t, h.view(), "Stop what magnum runs on #4612 (its fix loop and the re-review the loop forced)? y/N")
	h.press("y")
	if len(h.act.aborts) != 1 || h.act.aborts[0] != "example/talkable#4612" {
		t.Errorf("Abort calls %q, want example/talkable#4612", h.act.aborts)
	}
}

// The menu of a task's row lists f, F and K, dimmed on a row without a PR
// and K on one without a fix loop; each item asks the key's y/N.
func TestTheMenuOfATaskRowListsTheFixKeys(t *testing.T) {
	h := testPublished(t, 140, 40)
	h.press("g")
	h.selectRow("PR-12321")
	items := h.m.menuItems()
	for _, k := range []string{"f", "F", "K"} {
		if i := slices.IndexFunc(items, func(it menuItem) bool { return it.key == k }); i < 0 || !items[i].ok {
			t.Errorf("the menu of a task's row with a PR in a fix loop has no live %s: %+v", k, items)
		}
	}
	h.press("g")
	h.selectRow("PR-12320")
	items = h.m.menuItems()
	if i := slices.IndexFunc(items, func(it menuItem) bool { return it.key == "K" }); i < 0 || items[i].ok {
		t.Errorf("the menu of a task's row without a fix loop: K %+v, want it dimmed", items)
	}
	h.selectRow("PR-12302") // done: its PR merged
	items = h.m.menuItems()
	for _, k := range []string{"f", "F", "K"} {
		if i := slices.IndexFunc(items, func(it menuItem) bool { return it.key == k }); i < 0 || items[i].ok {
			t.Errorf("the menu of a task's row without a PR: %s %+v, want it dimmed", k, items)
		}
	}
}

// o on your PR's row steps into magnum's pane of it (the judge's, or the
// fixer's while its loop works), as the board's o: at once for a live pane;
// a parked one is restored in a slot, which holds the PR, so o asks the
// board's y/N first. On a task's row that waits on "magnum fixes #N" o
// opens magnum's pane too, not the factory's; t on your PR's row opens the
// Jira issue its title names. Before, o and t said "o and t act on a
// task's row" there, and o on a task's row stepped into the factory's pane
// though magnum's fixer was the agent at work.
func TestOAndTOnAPRRow(t *testing.T) {
	h := testYours(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "#4602" {
				pr := *r.PR
				pr.Restores, pr.Hold = true, 30*time.Minute
				d.Rows[i].PR = &pr
				c := d.Cards[r.ID]
				c.Task.Issue, c.Task.IssueURL = "PR-12400", "https://example.atlassian.net/browse/PR-12400"
				d.Cards[r.ID] = c
			}
		}
	})
	h.press("ctrl+r", "g")
	h.selectRow("#4602")
	mustContain(t, h.view(), "o magnum's pane")
	h.press("o")
	mustContain(t, h.view(), "Restore #4602 in a slot to read it? It holds the PR until 30 minutes after your last input. y/N")
	h.press("n")
	if len(h.act.openPRs) != 0 {
		t.Fatal("n restored the PR")
	}
	h.press("o", "y")
	h.press("t")
	h.selectRow("#4612")
	h.press("o")
	mustNotContain(t, h.view(), "y/N")
	if want := []string{"example/talkable#4602", "example/talkable#4612"}; !slices.Equal(h.act.openPRs, want) {
		t.Errorf("OpenPR calls %q, want %q", h.act.openPRs, want)
	}
	if got := h.act.opened(); len(got) != 1 || got[0] != "https://example.atlassian.net/browse/PR-12400" {
		t.Errorf("t on #4602 opened %q, want its Jira issue", got)
	}

	p := testPublished(t, 140, 40)
	p.press("g")
	p.selectRow("PR-12321") // agent: magnum fixes #4531
	p.press("o")
	if len(p.act.opens) != 0 || len(p.act.openPRs) != 1 || p.act.openPRs[0] != "example/talkable#4531" {
		t.Errorf("o on a task's row that magnum's fixer works on: factory panes %v, magnum's %q; want magnum's pane of #4531",
			p.act.opens, p.act.openPRs)
	}
}
