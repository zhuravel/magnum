package tui

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The fixer's report the screen tests open.
const fixerReport = "/home/op/.local/share/magnum/reviews/example/talkable/4560/a1b2c3d00000/claude-fix.md"

// fixtureFixer is home with a published task and your own PR whose fix
// loops left items to you, as factory.ScreenData words them: the fixer's
// first item on the row (its start alone on a narrow screen), how many
// more, every item on the help line, and e for the report in the hints.
func fixtureFixer() factoryFrame {
	now := factoryNow
	return factoryFrame{
		Repos: []string{"example/talkable"},
		Rows: []FactoryRow{
			{
				ID: 1, Key: "PR-12360", GitHub: "#4560", Title: "Coupon rounding in the cart",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, OnYou: true,
					What:  `description: change "fixes the cart total" to "rounds the cart… · 1 more`,
					Short: `description: change "fixes the…`,
					Long:  `claude-fix: description: change "fixes the cart total" to "rounds the cart total" · 1 more`}},
				Move: "e opens the report, b the PR", MoveShort: "e report · b PR",
				Since: factoryAgo(now, 35*time.Minute),
				Help: `you: claude-fix left you 2 items: description: change "fixes the cart total" to "rounds the cart total" · ` +
					"P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?",
			},
			{
				ID: -82, Key: "#4612", GitHub: "#4612", Title: "Weekly gem upgrades",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, OnYou: true,
					What:  "P1 app/models/offer.rb:42: not fixed. which rounding does the cart wa…",
					Short: "P1 app/models/offer.rb:42: not…",
					Long:  "claude-fix: P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?"}},
				Move: "e opens the report, b the PR", MoveShort: "e report · b PR",
				Since: factoryAgo(now, 25*time.Minute),
				Help:  "you: claude-fix left you 1 item: P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?",
				PR:    &FactoryPR{Ref: "example/talkable#4612"},
			},
			{
				ID: 3, Key: "PR-12321", GitHub: "#4531", Title: "Advocate offer email locale",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "magnum fixes #4531 (cycle 2 of 5)", Short: "magnum fixes #4531"}},
				Since:   factoryAgo(now, 9*time.Minute),
				Help:    "agent: magnum fixes #4531 (cycle 2 of 5)",
			},
			{
				ID: 6, Key: "PR-12302", GitHub: "#4519", Title: "Remove old sync job",
				Group: GroupDone, Mark: MarkDone,
				WaitsOn: []WaitsOn{{Who: "nothing", What: "merged 13:20"}},
				Help:    "nothing: merged 13:20",
			},
		},
		Hints: []hint{{"enter", "task card"}, {"e", "claude-fix.md"}, {"n", "new task"}, {"?", "more"}},
	}
}

// fixtureFixerCard is the task card of the published task whose fixer left
// you n items: who left how many under WAITING FOR, the items under it, and
// a link of what the PR's verdict says needs you.
func fixtureFixerCard(n int) TaskCard {
	c := fixturePublishedCard(1)
	c.Key, c.Title, c.PR = "PR-12360", "Coupon rounding in the cart", "#4560"
	c.WaitingFor = fmt.Sprintf("you: claude-fix left you %d items: e opens its report", n)
	c.FixReport = fixerReport
	items := []string{`description: change "fixes the cart total" to "rounds the cart total"`,
		"P1 app/models/offer.rb:42: not fixed. which rounding does the cart want? Both round half up today, the cart's spec says banker's rounding, and the email's copy says nothing"}
	for i := range n {
		if i < len(items) {
			c.FixNeeds = append(c.FixNeeds, items[i])
		} else {
			c.FixNeeds = append(c.FixNeeds, fmt.Sprintf("conflict: app/f%d.rb: which side wins?", i))
		}
	}
	return c
}

// The rows of a published task and of your PR whose fixer left you items
// say its first item at the spec's three widths, its start alone at 100
// columns, and nothing is cut past the screen. Before, both said "you:
// magnum: fix".
func TestFactoryFramesOfTheFixersItems(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		frame, _ := factoryFrameOf(fixtureFixer(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		checkFactoryFrame(t, fmt.Sprintf("fixer-%d.txt", w), ansi.Strip(frame), w)
	}
}

// The task card lists every item the fixer left you under WAITING FOR, one
// line each behind a dash, cut at the edge, before the links; past five it
// says how many more and that e opens the report.
func TestTaskCardListsWhatTheFixerLeft(t *testing.T) {
	checkCardGolden(t, "task-fixer", drawTask(fixtureFixerCard(2), 0))
	frame := plainCard(drawTask(fixtureFixerCard(7), 0), 100)
	for i := 2; i < 5; i++ {
		mustContain(t, frame, fmt.Sprintf("conflict: app/f%d.rb: which side wins?", i))
	}
	mustContain(t, frame, "+2 more: e opens the report")
	mustNotContain(t, frame, "app/f5.rb", "app/f6.rb")
	for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
		for _, w := range factoryWidths(40) {
			card := ansi.Strip(drawTask(fixtureFixerCard(7), 0)(w, 0, true, mode))
			for i, l := range strings.Split(card, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%s %d: line %d is %d cells wide: %q", mode, w, i+1, lw, l)
				}
			}
		}
	}
}

// The fixer's items are its own words, which can repeat the PR's text: the
// task card draws them with no control sequence of theirs left (a clipboard
// write, a screen erase, a cursor move, a bell), whatever the factory's
// side left in them, and their words stay.
func TestTaskCardCleansTheFixersItems(t *testing.T) {
	c := fixtureFixerCard(2)
	c.FixNeeds = []string{"description: change \x1b]52;c;cm0gLXJmIH4=\x07the total line",
		"P1 app/a.rb:1: not fixed.\x1b[2J\x1b[H which side\x00 wins?"}
	frame := drawTask(c, 0)(120, 0, true, IconsUnicode)
	for _, bad := range []string{"\x1b]52", "\x1b[2J", "\x1b[H", "\x07", "\x00"} {
		if strings.Contains(frame, bad) {
			t.Errorf("a control sequence of the fixer's items reached the card: %q", bad)
		}
	}
	plain := ansi.Strip(frame)
	mustContain(t, plain, "the total line", "which side")
}

// testFixer is the screen over yoursData with the fixer's report on your
// PR #4602's card and on the task card of PR-12371, the pager swapped for
// one that records what it opens.
func testFixer(t *testing.T) (*factoryHarness, *[]string) {
	t.Helper()
	h := testYours(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		for _, id := range []int64{-72, 4} {
			cards := d.Cards[id]
			cards.Task.FixReport = fixerReport
			d.Cards[id] = cards
		}
	})
	h.press("ctrl+r")
	var opened []string
	h.m.pager = func(_ context.Context, path string) *exec.Cmd {
		opened = append(opened, path)
		return exec.Command("true")
	}
	return h, &opened
}

// e opens the fixer's report in the pager, asking nothing, as d opens the
// diff: on your PR's row and on a task's row on home, and on the task card;
// the hints offer it by the report's name. A row with no report says so in
// the footer and opens nothing. Before, nothing on the screen opened the
// report the attention named.
func TestEOpensTheFixersReportInThePager(t *testing.T) {
	h, opened := testFixer(t)
	h.press("g")
	h.selectRow("#4602")
	mustContain(t, h.view(), "e claude-fix.md")
	h.press("e")
	h.selectRow("PR-12371")
	mustContain(t, h.view(), "e claude-fix.md")
	h.press("e")
	h.press("enter")
	mustContain(t, h.view(), "STAGES", "e claude-fix.md")
	h.press("e")
	if want := []string{fixerReport, fixerReport, fixerReport}; !slices.Equal(*opened, want) || len(h.execs) != 3 {
		t.Fatalf("e opened %q with %d ExecProcess messages, want %q", *opened, len(h.execs), want)
	}
	if h.m.bar.asking != nil {
		t.Error("e asked a question")
	}
	h.feed(factoryPagerMsg{task: 4})
	mustContain(t, h.view(), "STAGES")

	h.press("esc", "g")
	h.selectRow("#4603")
	mustNotContain(t, h.view(), "e claude-fix.md")
	h.press("e")
	mustContain(t, h.view(), "no report of magnum's fixer for #4603")
	if len(*opened) != 3 {
		t.Errorf("e on a row without a report opened %q", *opened)
	}
}

// The help names e on home and on the task card, and the right click's
// menu offers it on a row with a report, dimmed on one without.
func TestTheHelpAndTheMenuNameTheFixersReport(t *testing.T) {
	h, _ := testFixer(t)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	mustContain(t, help, "e the fixer's report (claude-fix.md) in the pager, on a row whose fix loop left you items, else QA's summary.md (asks nothing)",
		"e the report of magnum's fixer, when its fix loop left you items, else QA's summary.md where A is offered (asks nothing)",
		"claude-fix:  what magnum's fixer left you on the PR, from its report (e opens it)")
	h.press("g")
	h.selectRow("#4602")
	if got := menuState(h.m.menuItems()); !got["read the fixer's report"] {
		t.Errorf("the menu of #4602: %v, want the fixer's report", got)
	}
	h.selectRow("PR-12371")
	if got := menuState(h.m.menuItems()); !got["read the fixer's report"] {
		t.Errorf("the menu of PR-12371: %v, want the fixer's report", got)
	}
	h.press("enter")
	if got := menuState(h.m.menuItems()); !got["read the fixer's report"] {
		t.Errorf("the task card's menu: %v, want the fixer's report", got)
	}
	h.press("esc", "g")
	h.selectRow("#4603")
	if got, ok := menuState(h.m.menuItems())["read the fixer's report"]; !ok || got {
		t.Errorf("the menu of #4603: %v (listed %v), want the fixer's report dimmed", got, ok)
	}
}

// The pager is $PAGER (its words split, as $EDITOR's), else less, on the
// report, with -+F added to LESS so less does not quit at once on a short
// report.
func TestThePagerIsYoursElseLess(t *testing.T) {
	t.Setenv("PAGER", "most -s")
	t.Setenv("LESS", "")
	cmd := defaultPager(t.Context(), fixerReport)
	if !slices.Equal(cmd.Args, []string{"most", "-s", fixerReport}) || !slices.Contains(cmd.Env, "LESS=R -+F") {
		t.Errorf("$PAGER: args %q, LESS in %v", cmd.Args, cmd.Env)
	}
	t.Setenv("PAGER", " ")
	t.Setenv("LESS", "iR")
	cmd = defaultPager(t.Context(), fixerReport)
	if !slices.Equal(cmd.Args, []string{"less", fixerReport}) || !slices.Contains(cmd.Env, "LESS=iR -+F") {
		t.Errorf("no $PAGER: args %q, LESS in %v", cmd.Args, cmd.Env)
	}
}
