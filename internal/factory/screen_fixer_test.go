package factory

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The fixer's report and what it left you in the screen tests: one item
// on the PR's description and one finding it declined.
const fixerReportPath = "/home/op/.local/share/magnum/reviews/example/talkable/4560/a1b2c3d00000/claude-fix.md"

var (
	fixerNeeds = []string{`5485002407 description: change "fixes the cart total" to "rounds the cart total"`,
		"2101 P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?"}
	fixerItems = []string{`description: change "fixes the cart total" to "rounds the cart total"`,
		"P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?"}
)

// fixerAttention makes magnum's row of a PR say that its fix loop left
// needs to you: needs_attention of the fix kind, magnum's count of them, the
// fixer's report and its Needs you.
func fixerAttention(needs ...string) func(*PRState) {
	return func(v *PRState) {
		v.State, v.Attention = store.PRNeedsAttention, "fix"
		v.AttentionMsg = "fix: 2 findings need your call: " + fixerReportPath
		v.FixReport, v.FixNeeds = fixerReportPath, needs
	}
}

// The factory screen said only "magnum: fix" on the row of a PR whose
// fixer left one item to you (talkable#12215: a phrase of the PR body),
// and nothing on its card told what. A published task's row now says the
// fixer's first item without its comment id, cut to fit each form (the
// wide one names the fixer), and how many more; the help line lists every one, and the card
// says who left how many and that e opens the report, lists them under
// WAITING FOR and keeps the report for e. A verdict that says the PR needs
// your attention reads the same, with its links kept.
func TestAPublishedTasksFixAttentionSaysWhatTheFixerLeft(t *testing.T) {
	want := tui.WaitsOn{Who: tui.WaitsOnYou, OnYou: true,
		What:  `description: change "fixes the cart total" to "rounds the cart… · 1 more`,
		Short: `description: change "fixes the…`,
		Long:  `claude-fix: description: change "fixes the cart total" to "rounds the cart total" · 1 more`}
	help := `you: claude-fix left you 2 items: description: change "fixes the cart total" to "rounds the cart total" · ` +
		"P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?"
	links := []string{"https://github.com/example/talkable/pull/4560#discussion_r1"}
	for name, c := range map[string]struct {
		edit  func(*PRState)
		links []string
	}{
		"no verdict": {func(v *PRState) { v.OpenWatch = false; fixerAttention(fixerNeeds...)(v) }, nil},
		"its verdict": {func(v *PRState) {
			fixerAttention(fixerNeeds...)(v)
			v.HasOpen, v.Open = true, verdict(engine.OpenNeedsYou, "needs your attention (fix)")
			v.Open.Needs = []engine.OpenNeed{{Kind: "thread", URL: links[0]}}
		}, links},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			task := r.publishedTask("PR-12360", 4560)
			r.pr(4560, c.edit)
			d := r.data(time.Time{})
			row := rowOf(t, d, "PR-12360")
			if row.Group != tui.GroupNeedsYou || row.Mark != tui.MarkNeedsYou || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
				t.Errorf("the row: %+v\nwant NEEDS YOU with %+v", row, want)
			}
			if row.Help != help {
				t.Errorf("the help line %q\nwant %q", row.Help, help)
			}
			card := d.Cards[task.ID].Task
			if card.WaitingFor != "you: claude-fix left you 2 items: e opens its report" || !slices.Equal(card.FixNeeds, fixerItems) ||
				card.FixReport != fixerReportPath {
				t.Errorf("the card waits for %q, needs %q, report %q", card.WaitingFor, card.FixNeeds, card.FixReport)
			}
			if !slices.Equal(card.Links, c.links) {
				t.Errorf("the card's links %q, want the verdict's %q", card.Links, c.links)
			}
		})
	}
}

// Your own PR's row says the same of what its fixer left, and its help
// line lists every item, whether it needs your attention without a verdict
// or by its verdict; one item says no count.
func TestAnOwnPRsFixAttentionSaysWhatTheFixerLeft(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(81, 4611, func(v *PRState) { v.OpenWatch = false; fixerAttention(fixerNeeds[0])(v) })
	r.own(82, 4612, func(v *PRState) {
		fixerAttention(fixerNeeds...)(v)
		v.HasOpen, v.Open = true, verdict(engine.OpenNeedsYou, "needs your attention (fix)")
	})
	d := r.data(time.Time{})
	one := rowOf(t, d, "#4611")
	if w := one.WaitsOn[0]; one.Group != tui.GroupNeedsYou || !w.OnYou ||
		w.Long != `claude-fix: description: change "fixes the cart total" to "rounds the cart total"` ||
		one.Help != `you: claude-fix left you 1 item: description: change "fixes the cart total" to "rounds the cart total"` {
		t.Errorf("your PR with one item: %+v", one)
	}
	if c := d.Cards[-81].Task; c.WaitingFor != "you: claude-fix left you 1 item: e opens its report" ||
		!slices.Equal(c.FixNeeds, fixerItems[:1]) || c.FixReport != fixerReportPath {
		t.Errorf("its card waits for %q, needs %q, report %q", c.WaitingFor, c.FixNeeds, c.FixReport)
	}
	two := rowOf(t, d, "#4612")
	if w := two.WaitsOn[0]; two.Group != tui.GroupNeedsYou || w.Short != `description: change "fixes the…` ||
		two.Help != `you: claude-fix left you 2 items: description: change "fixes the cart total" to "rounds the cart total" · `+
			"P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?" {
		t.Errorf("your PR with two items by its verdict: %+v", two)
	}
	if two.Move == "" {
		t.Error("your PR that needs you lost its move")
	}
}

// A fix attention that does not ask for what the fixer left (a cap, a
// failed run) keeps magnum's words on the row and the card, and its card
// keeps the report for e; any other attention has none.
func TestAFixAttentionOfACapKeepsMagnumsWordsAndItsReport(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	task := r.publishedTask("PR-12361", 4561)
	r.pr(4561, func(v *PRState) {
		v.OpenWatch, v.State, v.Attention = false, store.PRNeedsAttention, "fix"
		v.AttentionMsg, v.FixReport = "fix: 2 fix cycles, 2 findings left", fixerReportPath
	})
	r.own(83, 4613, func(v *PRState) {
		v.OpenWatch, v.State, v.Attention, v.AttentionMsg = false, store.PRNeedsAttention, "blocked", "judge blocked: the slot's database is gone"
	})
	d := r.data(time.Time{})
	if w := rowOf(t, d, "PR-12361").WaitsOn[0]; w.What != "magnum: fix: 2 fix cycles, 2 findings left" {
		t.Errorf("a cap's row: %+v, want magnum's words", w)
	}
	if c := d.Cards[task.ID].Task; c.FixReport != fixerReportPath || c.FixNeeds != nil ||
		c.WaitingFor != "you: magnum: fix: 2 fix cycles, 2 findings left" {
		t.Errorf("a cap's card waits for %q, needs %q, report %q: want magnum's words and the report", c.WaitingFor, c.FixNeeds, c.FixReport)
	}
	if c := d.Cards[-83].Task; c.FixReport != "" || c.FixNeeds != nil {
		t.Errorf("a blocked judge's card: needs %q, report %q, want none", c.FixNeeds, c.FixReport)
	}
}
