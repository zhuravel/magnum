package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The pages of the fixture's PR-12330 (task 5) and PR-12371 (task 4), as
// the source gives them on their task cards.
const (
	linkPR12330    = "https://github.com/example/talkable/pull/4527"
	linkIssue12330 = "https://example.atlassian.net/browse/PR-12330"
	linkPR12371    = "https://github.com/example/talkable/pull/4530"
	linkIssue12371 = "https://example.atlassian.net/browse/PR-12371"
)

// links gives task id's card its PR's page and its issue's key and page,
// and reads the factory again.
func (h *factoryHarness) links(id int64, prURL, issue, issueURL string) {
	h.t.Helper()
	h.src.change(func(d *FactoryData) {
		c := d.Cards[id]
		c.Task.PRURL, c.Task.Issue, c.Task.IssueURL = prURL, issue, issueURL
		d.Cards[id] = c
	})
	h.press("ctrl+r")
}

// b and t on home open the selected row's PR and its Jira issue in the
// browser, asking nothing, as the board's b and t do (spec 9.D); home stays.
func TestBAndTOnHomeOpenTheSelectedTasksPRAndJiraIssue(t *testing.T) {
	h := testFactory(t, 140, 50)
	h.links(5, linkPR12330, "PR-12330", linkIssue12330)
	h.links(4, linkPR12371, "PR-12371", linkIssue12371)
	h.press("g")
	h.selectRow("PR-12330")
	h.press("b")
	mustNotContain(t, h.view(), "y/N")
	h.press("t")
	if got := h.act.opened(); !slices.Equal(got, []string{linkPR12330, linkIssue12330}) {
		t.Fatalf("b and t on PR-12330 opened %q", got)
	}
	v := h.view()
	mustContain(t, v, "NEEDS YOU", "opened PR-12330: "+linkIssue12330)
	mustNotContain(t, v, "y/N", "STAGES")
	if h.act.calls() != 0 {
		t.Errorf("b and t sent %d requests to the factory", h.act.calls())
	}
}

// On the task card b and t open that task's PR and Jira issue, asking
// nothing, and the card stays; its hints offer them.
func TestBAndTOnTheTaskCardOpenThatTasksPRAndJiraIssue(t *testing.T) {
	h := testFactory(t, 140, 50)
	h.links(5, linkPR12330, "PR-12330", linkIssue12330)
	h.links(4, linkPR12371, "PR-12371", linkIssue12371)
	h.open("PR-12330")
	mustContain(t, h.view(), "d diff · b PR · t Jira")
	h.press("b", "t")
	if got := h.act.opened(); !slices.Equal(got, []string{linkPR12330, linkIssue12330}) {
		t.Fatalf("b and t on PR-12330's card opened %q", got)
	}
	v := h.view()
	mustContain(t, v, "PR-12330 · bug · Advocate offer email locale", "STAGES")
	mustNotContain(t, v, "y/N")
}

// A task with nothing to open says why in the footer and opens nothing: no
// PR before the factory published one, no Jira issue for a task of your
// words, no link for a key that no [board] trackers template names. The
// card leaves out the hints of what it cannot open.
func TestBAndTSayWhyATaskHasNothingToOpen(t *testing.T) {
	h := testFactory(t, 140, 50)
	h.links(2, "", "PR-12345", "")
	h.src.change(func(d *FactoryData) { // a task of your words: no key, its row named by its id
		i := slices.IndexFunc(d.Rows, func(r FactoryRow) bool { return r.ID == 4 })
		d.Rows[i].Key = "task 4"
		c := d.Cards[4]
		c.Task.Key = "task 4"
		d.Cards[4] = c
	})
	h.press("ctrl+r")
	h.press("g")
	h.selectRow("PR-12345")
	h.press("b")
	mustContain(t, h.view(), "no PR yet for PR-12345")
	h.press("t")
	mustContain(t, h.view(), "no link for PR-12345: no [board] trackers template names its key")
	h.press("g")
	h.selectRow("task 4")
	h.press("t")
	mustContain(t, h.view(), "task 4 has no Jira issue: it started from your words")
	h.press("enter")
	v := h.view()
	mustContain(t, v, "STAGES")
	mustNotContain(t, v, "b PR", "t Jira")
	h.press("b")
	mustContain(t, h.view(), "no PR yet for task 4")
	if got := h.act.opened(); len(got) != 0 {
		t.Errorf("a task with nothing to open opened %q", got)
	}
}

// The help lists b and t for home and the task card, and the mouse menus
// offer them, dimmed for a task with nothing to open.
func TestTheHelpAndTheMenusOfferBAndT(t *testing.T) {
	h := testFactory(t, 140, 50)
	h.links(5, linkPR12330, "PR-12330", linkIssue12330)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	mustContain(t, help, "b t the row's PR, its Jira issue in the browser (asks nothing)",
		"b t its PR, its Jira issue in the browser (asks nothing)")
	h.press("g")
	h.selectRow("PR-12330")
	if got := menuState(h.m.menuItems()); !got["open its PR"] || !got["open its Jira issue"] {
		t.Errorf("home's menu on PR-12330: %v", got)
	}
	h.press("g")
	h.selectRow("PR-12345")
	if got := menuState(h.m.menuItems()); got["open its PR"] || got["open its Jira issue"] {
		t.Errorf("home's menu on PR-12345, which has nothing to open: %v", got)
	}
	h.open("PR-12330")
	got := menuState(h.m.menuItems())
	if ok, has := got["open its PR"]; !ok || !has {
		t.Errorf("the task card's menu: %v", got)
	}
	if ok, has := got["open its Jira issue"]; !ok || !has {
		t.Errorf("the task card's menu: %v", got)
	}
}
