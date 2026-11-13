package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// finishAsk is the y/N a task card offers S with (its FinishReview).
const finishAsk = "End PR-12393's local review: round 4 of its local review runs: it finishes, then no round starts after it. " +
	"A safe stop goes to the ready card; any other waits on you?"

// The task card's S ends the task's local review behind the y/N that says
// what happens (the card's FinishReview, as `magnum factory finish-review`
// asks it): only y sends it. The hints, the menu and the help offer it;
// a card that does not offer it asks nothing and sends nothing.
func TestTheFactoryScreensFinishKeyAsksYN(t *testing.T) {
	h := testFactory(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[5]
		c.Task = fixtureRetryCard()
		c.Task.Retry, c.Task.FinishReview = false, finishAsk
		d.Cards[5] = c
	})
	h.press("ctrl+r")
	h.open("PR-12330")
	mustContain(t, h.view(), "S end review")
	h.press("S")
	mustContain(t, cardSquash(h.view()), "End PR-12393's local review: round 4 of its local review runs: it finishes")
	h.press("n")
	if h.act.calls() != 0 || len(h.act.finishes) != 0 {
		t.Fatal("S ended the review before y")
	}
	h.press("S", "y")
	if !slices.Equal(h.act.finishes, []int64{5}) || h.act.calls() != 1 {
		t.Errorf("finishes %v (%d calls), want one of task 5", h.act.finishes, h.act.calls())
	}
	if menu := menuState(h.m.menuItems()); !menu["end the local review"] {
		t.Errorf("the task card's menu %v, want the end of its local review", menu)
	}

	plain := testFactory(t, 140, 40)
	plain.src.change(func(d *FactoryData) {
		c := d.Cards[5]
		c.Task = fixtureRetryCard()
		d.Cards[5] = c
	})
	plain.press("ctrl+r")
	plain.open("PR-12330")
	mustNotContain(t, plain.view(), "S end review")
	plain.press("S")
	if plain.act.calls() != 0 || plain.m.bar.asking != nil {
		t.Error("S on a task with no local review at work asked or ran something")
	}
	mustContain(t, plain.view(), "S ends a local review at work")
	if menu := menuState(plain.m.menuItems()); menu["end the local review"] {
		t.Errorf("the task card's menu %v offers the end of a local review", menu)
	}

	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	mustContain(t, help, "S end its local review now, as magnum factory finish-review: the round in flight finishes, none starts after it (y/N)")
}
