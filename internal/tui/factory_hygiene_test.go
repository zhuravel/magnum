package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The help glosses only what the screen draws: no staging merge or QA,
// CI-fix tries, "say one line", learning view or rating, which no
// data fills and no key offers yet; the staging cells a published task's
// PR gets from the staging status (latest, CI, deploying, outdated) it
// glosses; the stages it glosses are the built-in kinds' (judge,
// reproduction, publish), not the spec's later ones (tests, merge master,
// cleanup). Before, it listed all of spec 9.E's words.
func TestTheHelpListsOnlyWhatExists(t *testing.T) {
	h := testFactory(t, 140, 50)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	words := map[string]bool{}
	for l := range strings.SplitSeq(help, "\n") {
		if w, _, ok := strings.Cut(strings.TrimSpace(l), "  "); ok {
			words[w] = true
		}
	}
	for _, gone := range []string{"void, bart, bastion", "merging", "CI 12/41", "QA 4/9",
		"QA ✓ / QA ✗", "bart: no QA", "bastion waits for void QA", "code failed", "runner died", "try 1 of 2", "out of fix tries",
		"ready to merge", "access needed", "say one line", "you're in it", "no output 9m", "your PR, written by hand",
		"lesson", "rule", "skill edit", "test case", "replay", "used in", "repeats seen after the rule", "rule to approve",
		"rate + / −", "codex limit: waits till 16:00", "runbook", "start", "tests", "merge master", "merge", "cleanup", "pushed"} {
		if words[gone] {
			t.Errorf("the help glosses %q, which the screen never draws", gone)
		}
	}
	for _, kept := range []string{"investigate", "judge", "critic", "narrow critic", "questions", "reproduction", "coding", "local review",
		"pr body", "ready", "publish", "step in", "memory full: new tasks wait", "you pushed 11:02", "push + PR",
		"VOID, BART", "latest", "CI, CI ✗", "deploying", "outdated", "– and ?"} {
		if !words[kept] {
			t.Errorf("the help lacks %q", kept)
		}
	}
}

// A title that starts with the issue key the row's key cell shows drops
// it ("[PR-12400] Coupon rounding" beside PR-12400 reads "Coupon
// rounding"), so the key is not said twice; where the key cell shows your
// PR's number instead (below 120 columns), the title keeps it. A key that
// only starts like the title's (PR-1234 before PR-12345) stays.
func TestATitleDropsTheKeyItsKeyCellShows(t *testing.T) {
	f := fixtureYours()
	for i, r := range f.Rows {
		switch r.Key {
		case "#4602":
			f.Rows[i].Title = "[PR-12400] Coupon rounding"
		case "PR-12371":
			f.Rows[i].Title = "PR-12371: Coupon CSV export times out"
		case "PR-12345":
			f.Rows[i].Title = "PR-123456 Credit expiry reminder email"
		}
	}
	line := func(frame, key string) string {
		for l := range strings.SplitSeq(ansi.Strip(frame), "\n") {
			if strings.Contains(l, " "+key+" ") {
				return l
			}
		}
		t.Fatalf("no row %s in:\n%s", key, ansi.Strip(frame))
		return ""
	}
	wide, _ := factoryFrameOf(f, 140, 22, true, IconsUnicode, factoryNow)
	if l := line(wide, "PR-12400"); strings.Contains(l, "[PR-12400]") || !strings.Contains(l, "Coupon rounding") {
		t.Errorf("at 140 the row says its key twice: %q", l)
	}
	if l := line(wide, "PR-12371"); strings.Contains(l, "PR-12371:") || !strings.Contains(l, "Coupon CSV export times out") {
		t.Errorf("at 140 the task's row says its key twice: %q", l)
	}
	if l := line(wide, "PR-12345"); !strings.Contains(l, "PR-123456 Credit expiry") {
		t.Errorf("a longer key in the title was cut: %q", l)
	}
	narrow, _ := factoryFrameOf(f, 100, 22, true, IconsUnicode, factoryNow)
	if l := line(narrow, "#4602"); !strings.Contains(l, "[PR-12400] Coupon") {
		t.Errorf("at 100 the row of your PR keyed by its number lost its title's key: %q", l)
	}
}

// The help names a on the task card and on your PR's card, which take it
// (the walk's next card, or its first), as the README and the cards'
// footers do. Before, its Task card section and its line for your PR's
// card left it out.
func TestTheHelpNamesAOnTheCards(t *testing.T) {
	h := testFactory(t, 140, 50)
	help := ansi.Strip(strings.Join(h.m.helpContent(200), "\n"))
	_, card, ok := strings.Cut(help, "\nTask card\n")
	card, _, _ = strings.Cut(card, "\n\n")
	if !ok {
		t.Fatalf("the help has no Task card section:\n%s", help)
	}
	var keyA, yours bool
	for l := range strings.SplitSeq(card, "\n") {
		l = strings.TrimSpace(l)
		keyA = keyA || strings.HasPrefix(l, "a ")
		if strings.HasPrefix(l, "your PR's card") {
			yours = strings.Contains(l, " a ")
		}
	}
	if !keyA || !yours {
		t.Errorf("the Task card section names a: %v, on your PR's card: %v:\n%s", keyA, yours, card)
	}
}

// The menu of your PR's row opens its card first, as enter and a double
// click do, and names a as the walk it is (the oldest that needs you, a
// PR's card included), on a task's row too. Before, it had no card item,
// and a read "oldest decision".
func TestTheMenuOfYourPRsRowOpensItsCard(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("g")
	h.selectRow("#4602")
	items := h.m.menuItems()
	if len(items) == 0 || items[0].key != "enter" || !items[0].ok || items[0].label != "open its card" {
		t.Errorf("the menu of your PR's row starts %+v, want its card", items)
	}
	for _, key := range []string{"#4602", "PR-12345"} {
		h.press("g")
		h.selectRow(key)
		i := slices.IndexFunc(h.m.menuItems(), func(it menuItem) bool { return it.key == "a" })
		if i < 0 || h.m.menuItems()[i].label != "oldest that needs you" {
			t.Errorf("%s's menu: a reads %+v, want the oldest that needs you", key, h.m.menuItems())
		}
	}
}
