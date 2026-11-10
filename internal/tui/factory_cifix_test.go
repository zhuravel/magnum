package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// c on the factory screen is the board's c (a CI fix of the PR's failed
// required check, fix_ci.go): the daemon's plan first (a dry run), its
// question as the y/N, and y starts the CI fix bound to what the plan asked
// about (CIFixAsked), on a row with a PR in magnum's registry (yours, or a
// published task's, which the factory opens as you) and on its card.

func (a *fakeFactoryActions) FixCIPlan(_ context.Context, ref string) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ciPlans = append(a.ciPlans, ref)
	if a.ciPlanErr != nil {
		return ActionResult{Text: "progress line\n" + a.ciPlanErr.Error()}, a.ciPlanErr
	}
	asked := ciFixAsked
	return ActionResult{Text: "dry run", Ask: ciFixAsk(ref), CIFix: &asked}, nil
}

func (a *fakeFactoryActions) FixCI(_ context.Context, ref string, asked CIFixAsked) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ciFixes = append(a.ciFixes, ciFixCall(ref, asked))
	return ActionResult{Text: "fixing the failed CI of " + ref}, nil
}

// ciCalls are the plans FixCIPlan answered and the CI fixes FixCI started.
func (a *fakeFactoryActions) ciCalls() (plans, fixes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ciPlans), slices.Clone(a.ciFixes)
}

// wantCI checks the plans and the CI fixes the screen asked for.
func (h *factoryHarness) wantCI(plans, fixes []string) {
	h.t.Helper()
	gotPlans, gotFixes := h.act.ciCalls()
	if !slices.Equal(gotPlans, plans) || !slices.Equal(gotFixes, fixes) {
		h.t.Errorf("CI fix plans %q and starts %q, want %q and %q", gotPlans, gotFixes, plans, fixes)
	}
}

// c on your PR's row asks the daemon's plan and puts it as the question:
// n starts nothing, and y starts the CI fix bound to that plan.
func TestCOnYourPRsRowAsksThePlanThenFixesItsCI(t *testing.T) {
	h := testYours(t, 200, 40)
	h.press("g")
	h.selectRow("#4602")
	h.press("c")
	mustContain(t, h.view(), ciFixAsk("example/talkable#4602")+" y/N")
	h.wantCI([]string{"example/talkable#4602"}, nil)
	h.press("n")
	h.wantCI([]string{"example/talkable#4602"}, nil)
	mustNotContain(t, h.view(), "y/N")
	h.press("c", "y")
	h.wantCI([]string{"example/talkable#4602", "example/talkable#4602"}, []string{ciFixCall("example/talkable#4602", ciFixAsked)})
	mustContain(t, h.view(), "fixing the failed CI of example/talkable#4602")
}

// c on your PR's card asks the same, and y there starts the one CI fix.
func TestCOnYourPRsCardAsksThePlanThenFixesItsCI(t *testing.T) {
	h := testYours(t, 200, 40)
	h.open("#4602")
	if h.m.view != fvTask {
		t.Fatalf("enter opened view %v, want the PR's card", h.m.view)
	}
	h.press("c")
	mustContain(t, h.view(), ciFixAsk("example/talkable#4602")+" y/N")
	h.press("n")
	h.wantCI([]string{"example/talkable#4602"}, nil)
	h.press("c", "y")
	h.wantCI([]string{"example/talkable#4602", "example/talkable#4602"}, []string{ciFixCall("example/talkable#4602", ciFixAsked)})
}

// c on a published task's row or card, whose PR the factory opened as you,
// asks its PR's plan the same way.
func TestCOnATasksPRRowAndCardAsksThePlan(t *testing.T) {
	h := testPublished(t, 200, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("c")
	mustContain(t, h.view(), ciFixAsk("example/talkable#4530")+" y/N")
	h.press("y")
	h.press("enter", "c")
	mustContain(t, h.view(), ciFixAsk("example/talkable#4530")+" y/N")
	h.press("y")
	want := ciFixCall("example/talkable#4530", ciFixAsked)
	h.wantCI([]string{"example/talkable#4530", "example/talkable#4530"}, []string{want, want})
}

// A refusal of the plan (the daemon's error: nothing failed, red on the
// base, the cap, an older daemon) shows as the footer's note and asks and
// starts nothing.
func TestAPlanTheDaemonRefusesAsksNothingAndShowsItsWords(t *testing.T) {
	for _, onCard := range []bool{false, true} {
		h := testYours(t, 200, 40)
		h.act.ciPlanErr = errNothingFailed
		h.press("g")
		h.selectRow("#4602")
		if onCard {
			h.press("enter")
		}
		h.press("c")
		v := h.view()
		mustContain(t, v, "Plan the CI fix of #4602 failed: nothing to fix: no required check failed at abcdef1")
		mustNotContain(t, v, "y/N")
		h.press("y")
		h.wantCI([]string{"example/talkable#4602"}, nil)
	}
}

// c where a CI fix does not apply does nothing but say why, before it asks
// the daemon anything: a row without a PR in magnum's registry (on home and
// on its card), a PR merged, and a PR a fix loop works on (the board's own
// refusals).
func TestCWhereTheCIFixDoesNotApplyAsksNothing(t *testing.T) {
	h := testYours(t, 200, 40)
	h.press("g")
	h.selectRow("PR-12371")
	h.press("c")
	mustContain(t, h.view(), "c acts on a PR in magnum's registry: PR-12371 has none there")
	h.press("enter", "c")
	mustContain(t, h.view(), "c acts on a PR in magnum's registry: PR-12371 has none there")

	h.facts("#4602", FactoryPRFacts{State: "released", GHState: "MERGED", Head: factoryHead, Reviewed: factoryHead})
	h.press("esc", "g")
	h.selectRow("#4602")
	h.press("c")
	mustContain(t, h.view(), "#4602 is merged: nothing to fix")
	h.selectRow("#4612") // a fix loop works on it
	h.press("c")
	mustContain(t, h.view(), "#4612: a fix loop works on it; K stops it")
	mustNotContain(t, h.view(), "y/N")
	h.wantCI(nil, nil)
	if h.act.calls() != 0 {
		t.Errorf("a refused c ran %d calls", h.act.calls())
	}
}

// Without actions c says the screen runs none.
func TestCWithoutActionsSaysTheScreenRunsNone(t *testing.T) {
	h := testYours(t, 200, 40)
	h.m.act = nil
	h.press("g")
	h.selectRow("#4602")
	h.press("c")
	mustContain(t, h.view(), noActions)
}

// A plan that answers away from its row is held, like E's and P's: the rule
// says c on its row asks it, nothing asks by itself, and c on its row puts
// the question up, whose y starts the CI fix bound to that plan.
func TestAHeldCIFixPlanWaitsForCOnItsRow(t *testing.T) {
	h := testPlanned(t, 200, 40)
	plan := h.startHeld("c")
	h.press("?")
	h.run(plan)
	if q := h.m.bar.question(); q != "" {
		t.Fatalf("the bar asks %q on the help", q)
	}
	h.press("esc")
	h.expireNote()
	mustContain(t, h.view(), "plan of PR-12345 ready: c on its row asks")
	h.press("y")
	h.wantCI([]string{"example/talkable#4545"}, nil)
	h.press("g")
	h.selectRow("PR-12345")
	h.press("c")
	mustContain(t, h.view(), ciFixAsk("example/talkable#4545")+" y/N")
	h.press("y")
	h.wantCI([]string{"example/talkable#4545"}, []string{ciFixCall("example/talkable#4545", ciFixAsked)})
}

// The right-click menu of a PR row and of a PR's card lists c as "fix CI",
// dimmed where c refuses, and an item asks the same plan and y/N as the key:
// a click never confirms.
func TestTheMenuListsFixCIAndAsksTheSameQuestion(t *testing.T) {
	h := testYours(t, 200, 40)
	rightClickRow := func(key string) {
		h.t.Helper()
		h.press("g")
		h.selectRow(key)
		r, _ := h.m.selectedRow()
		_, lines := renderFactory(h.m.homeFrame(), 200, 40, h.m.st, h.m.g, factoryNow)
		h.feed(tea.MouseClickMsg{X: 10, Y: lines[r.ID], Button: tea.MouseRight})
		if !h.m.menu.open {
			h.t.Fatalf("a right click on %s opened no menu:\n%s", key, h.view())
		}
	}
	item := func() (int, menuItem) {
		h.t.Helper()
		items := h.m.menuItems()
		i := slices.IndexFunc(items, func(it menuItem) bool { return it.key == "c" })
		if i < 0 {
			h.t.Fatalf("the menu lists no c: %+v", items)
		}
		return i, items[i]
	}
	for _, key := range []string{"#4602", "PR-12371", "#4612"} { // a live item, then two dimmed
		rightClickRow(key)
		_, it := item()
		if want := key == "#4602"; it.label != "fix CI" || it.ok != want {
			t.Errorf("c in the menu of %s: %+v, want fix CI, live %v", key, it, want)
		}
		h.press("esc")
	}

	rightClickRow("#4602")
	i, _ := item()
	for range i {
		h.press("j")
	}
	h.press("enter")
	mustContain(t, h.view(), ciFixAsk("example/talkable#4602")+" y/N")
	h.feed(tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseLeft})
	mustNotContain(t, h.view(), "y/N")
	h.wantCI([]string{"example/talkable#4602"}, nil)

	h.press("enter") // the card
	h.feed(tea.MouseClickMsg{X: 10, Y: 5, Button: tea.MouseRight})
	i, it := item()
	if !it.ok {
		t.Fatalf("c in the menu of your PR's card is dimmed: %+v", it)
	}
	for range i {
		h.press("j")
	}
	h.press("enter", "y")
	h.wantCI([]string{"example/talkable#4602", "example/talkable#4602"}, []string{ciFixCall("example/talkable#4602", ciFixAsked)})
}

// The help names c on home and on the task card, and your PR's card offers
// "c fix CI" in its hint bar where c acts: not on a PR merged or in a fix
// loop, and not when the screen runs no actions.
func TestTheHelpAndYourPRsCardOfferC(t *testing.T) {
	h := testYours(t, 200, 40)
	help := ansi.Strip(strings.Join(h.m.helpContent(240), "\n"))
	for _, sec := range []string{"Home", "Task card"} {
		if !slices.ContainsFunc(helpSection(t, help, sec), func(l string) bool {
			return strings.HasPrefix(l, "c ") && strings.Contains(l, "CI") && strings.Contains(l, "y/N")
		}) {
			t.Errorf("the help's %s lacks c, the CI fix:\n%s", sec, strings.Join(helpSection(t, help, sec), "\n"))
		}
	}
	mustContain(t, help, "f c F K r R z M U")
	h.open("#4602")
	mustContain(t, h.hintLine(), "c fix CI")

	h.facts("#4602", FactoryPRFacts{State: "released", GHState: "MERGED", Head: factoryHead, Reviewed: factoryHead})
	mustNotContain(t, h.hintLine(), "c fix CI")
	h.press("esc", "g")
	h.selectRow("#4612")
	h.press("enter")
	mustNotContain(t, h.hintLine(), "c fix CI")

	bare := testYours(t, 200, 40)
	bare.m.act = nil
	bare.open("#4602")
	mustNotContain(t, bare.hintLine(), "c fix CI")
}
