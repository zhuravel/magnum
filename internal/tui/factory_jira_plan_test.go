package tui

import (
	"strings"
	"testing"
)

// The task card's JIRA MOVE line is the factory's latest plan of a move of
// the task's Jira issue, in one line: the move planned and when (in
// shadow), then what was done after it ("you moved it" only for your own
// move, else where it moved); nothing to move; why the factory
// could not plan it; a plan that waits for its read. The words are the
// struct's and the painter's alone. A task without a plan has no line.
func TestTaskCardDrawsTheJiraPlanInOneLine(t *testing.T) {
	planned := func() *FactoryJiraPlan {
		return &FactoryJiraPlan{Milestone: "start", Mode: "shadow", Outcome: "planned", From: "To Do", To: "In Development",
			Transition: "Start progress", Begun: cardAt(14, 1), At: cardAt(14, 2)}
	}
	with := func(edit func(p *FactoryJiraPlan)) *FactoryJiraPlan {
		p := planned()
		edit(p)
		return p
	}
	for name, tc := range map[string]struct {
		plan *FactoryJiraPlan
		want string
	}{
		"planned": {planned(), "To Do → In Development (planned 14:02, shadow)"},
		"you moved it": {with(func(p *FactoryJiraPlan) {
			p.Moved, p.MovedTo, p.MovedAt, p.MovedBy = "target", "In Development", cardAt(14, 20), "you"
		}), "To Do → In Development (planned 14:02, shadow) · you moved it 14:20"},
		"someone else moved it": {with(func(p *FactoryJiraPlan) {
			p.Moved, p.MovedTo, p.MovedAt, p.MovedBy = "target", "In Development", cardAt(14, 20), "other"
		}), "To Do → In Development (planned 14:02, shadow) · moved to In Development 14:20"},
		"moved, its author unknown": {with(func(p *FactoryJiraPlan) { p.Moved, p.MovedTo, p.MovedAt = "target", "In Development", cardAt(14, 20) }),
			"To Do → In Development (planned 14:02, shadow) · moved to In Development 14:20"},
		"you moved it elsewhere": {with(func(p *FactoryJiraPlan) {
			p.Moved, p.MovedTo, p.MovedAt, p.MovedBy = "elsewhere", "Blocked", cardAt(14, 5), "you"
		}), "To Do → In Development (planned 14:02, shadow) · you moved it to Blocked 14:05"},
		"someone else moved it elsewhere": {with(func(p *FactoryJiraPlan) {
			p.Moved, p.MovedTo, p.MovedAt, p.MovedBy = "elsewhere", "Blocked", cardAt(14, 5), "other"
		}), "To Do → In Development (planned 14:02, shadow) · moved to Blocked 14:05"},
		"no move by the end": {with(func(p *FactoryJiraPlan) { p.Moved = "none" }),
			"To Do → In Development (planned 14:02, shadow) · no move by the task's end"},
		"waits for its read": {&FactoryJiraPlan{Milestone: "published", Mode: "shadow", To: "In Code Review", Begun: cardAt(14, 1)},
			"→ In Code Review: planning since 14:01 (shadow)"},
		"already there": {with(func(p *FactoryJiraPlan) { p.Outcome, p.From = "already", "In Development" }),
			"already In Development: nothing to move (14:02, shadow)"},
		"past it": {with(func(p *FactoryJiraPlan) { p.Outcome, p.From = "already", "In Code Review" }),
			"In Code Review, past In Development: nothing to move (14:02, shadow)"},
		"someone else's": {with(func(p *FactoryJiraPlan) { p.Outcome = "assigned_other" }),
			"→ In Development: assigned to someone else, move it yourself (14:02, shadow)"},
		"no transition": {with(func(p *FactoryJiraPlan) {
			p.Outcome, p.Transitions = "no_transition", []string{"Ready for review → In Code Review", "Resolve → Done"}
		}), "→ In Development: no transition to it, move it yourself (14:02, shadow) · Jira offers Ready for review → In Code Review, Resolve → Done"},
		"no transition at all": {with(func(p *FactoryJiraPlan) { p.Outcome = "no_transition" }),
			"→ In Development: Jira offers no transition, move it yourself (14:02, shadow)"},
		"fields": {with(func(p *FactoryJiraPlan) { p.Outcome = "needs_fields" }),
			"→ In Development: Jira wants fields for Start progress, move it yourself (14:02, shadow)"},
		"a done target": {with(func(p *FactoryJiraPlan) { p.Outcome = "target_done" }),
			"→ In Development: a done status, never the factory's move (14:02, shadow)"},
		"unread": {&FactoryJiraPlan{Milestone: "start", Mode: "shadow", Outcome: "unread", To: "In Development", Begun: cardAt(14, 1),
			Cause: "Jira did not answer within 30s"}, "→ In Development: not planned, its read failed: Jira did not answer within 30s (shadow)"},
		"a hostile name": {with(func(p *FactoryJiraPlan) { p.From = "To \x1b[31mDo" }), "To Do → In Development (planned 14:02, shadow)"},
	} {
		card := fixtureJiraCard()
		card.JiraPlan = tc.plan
		frame := plainCard(drawTask(card, 0), 160)
		if got := strings.TrimSpace(cardLineWith(t, frame, "JIRA MOVE")); got != "JIRA MOVE    "+tc.want {
			t.Errorf("%s: the JIRA MOVE line is %q, want %q", name, got, "JIRA MOVE    "+tc.want)
		}
		if raw := drawTask(card, 0)(160, 0, true, IconsUnicode); strings.Contains(raw, "\x1b[31mDo") {
			t.Errorf("%s: the plan's name reaches the terminal with its escape", name)
		}
	}
	card := fixtureJiraCard()
	if strings.Contains(plainCard(drawTask(card, 0), 140), "JIRA MOVE") {
		t.Error("a task without a plan draws a JIRA MOVE line")
	}
	card.JiraPlan = planned()
	for _, w := range []int{60, 100} {
		for l := range strings.SplitSeq(plainCard(drawTask(card, 0), w), "\n") {
			if n := len([]rune(l)); n > w {
				t.Errorf("at %d columns a line is %d wide: %q", w, n, l)
			}
		}
	}
}
