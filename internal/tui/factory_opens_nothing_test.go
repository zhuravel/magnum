package tui

import (
	"slices"
	"testing"
)

// opensNothingNote is what the data says of a task whose o opens nothing
// while a hold stops every stage.
const opensNothingNote = "nothing runs: the factory holds every stage (MySQL does not answer since 17:03)"

// o on a task with no agent at work, no live log and no server opened
// nothing and said nothing, so the operator could not tell what the factory
// was doing (the 2026-11-13 outage: a task at its local review, MySQL down).
// The data says why it opens nothing (TaskCard.OpensNothing), and o puts
// that on the message line, on home and on the task card, asking nothing and
// stepping into no pane; a task whose card says nothing is stepped into as
// before.
func TestTheScreensOpenKeySaysWhyNothingOpens(t *testing.T) {
	h := testFactory(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		cards := d.Cards[6] // PR-12388
		cards.Task.OpensNothing = opensNothingNote
		d.Cards[6] = cards
	})
	h.press("ctrl+r")
	h.press("g")
	h.selectRow("PR-12388")

	h.press("o")
	mustContain(t, h.view(), opensNothingNote)
	if h.m.bar.asking != nil || len(h.act.opens) != 0 || h.m.bar.busy != "" {
		t.Fatalf("o on home asked (%v), stepped into a pane %v or started an action (%q), want only the note",
			h.m.bar.asking != nil, h.act.opens, h.m.bar.busy)
	}

	h.press("enter") // the task card: its key does the same
	mustContain(t, h.view(), "STAGES")
	h.m.bar.flash = "" // the note of home's o is gone: the card's own o says it again
	mustNotContain(t, h.view(), opensNothingNote)
	h.press("o")
	mustContain(t, h.view(), opensNothingNote)
	if len(h.act.opens) != 0 {
		t.Fatalf("o on the card stepped into a pane %v", h.act.opens)
	}
	h.press("esc")

	h.press("g")
	h.selectRow("PR-12371") // an agent works: its card says nothing, and o steps into its pane
	h.press("o")
	if !slices.Equal(h.act.opens, []int64{4}) {
		t.Fatalf("o on a task whose card says nothing opened %v, want the pane of task 4", h.act.opens)
	}
}
