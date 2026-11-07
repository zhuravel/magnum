package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fixtureYours is home with your own PRs from magnum's registry beside the
// tasks (factory.ScreenData's words): one PR needs you, one is approved
// with nothing open, one magnum's fixer works on, one waits for CI and one
// magnum leaves alone (a baseline), drawn dim.
func fixtureYours() factoryFrame {
	now := factoryNow
	return factoryFrame{
		Repo: "talkable",
		Rows: []FactoryRow{
			{
				ID: 2, Key: "PR-12345", Title: "Credit expiry reminder email",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "3 questions before coding", Short: "3 questions", OnYou: true}},
				Move:    "answer 3 questions", MoveShort: "answer them",
				Since: factoryAgo(now, 41*time.Minute),
				Help:  `3 questions before coding · first: "When does the reminder go out?"`,
			},
			{
				ID: -72, Key: "#4602", GitHub: "#4602", Jira: "PR-12400", Title: "Coupon rounding",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "1 item needs you: 1 thread of a person", Short: "1 item needs you", OnYou: true}},
				Move:    "b opens it, f fixes", MoveShort: "b or f",
				Since: factoryAgo(now, 25*time.Minute),
				Help:  "you: 1 item needs you: 1 thread of a person",
				PR:    &FactoryPR{Ref: "example/talkable#4602"},
			},
			{
				ID: -73, Key: "#4603", GitHub: "#4603", Title: "Referral export column order",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "nothing open at a1b2c3d · approved, merge is yours",
					Short: "nothing open at a1b2c3d", OnYou: true}},
				Move: "merge #4603 on GitHub", MoveShort: "merge it",
				Since: factoryAgo(now, 5*time.Minute),
				Help:  "you: nothing open at a1b2c3d · approved, merge is yours",
				PR:    &FactoryPR{Ref: "example/talkable#4603"},
			},
			{
				ID: 4, Key: "PR-12371", Title: "Coupon CSV export times out",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "finding the cause, 2 of 3 done", Short: "finding the cause"}},
				Since:   factoryAgo(now, 9*time.Minute),
				Help:    "finding the cause: 3 agents read the history and reproduce it",
			},
			{
				ID: -82, Key: "#4612", GitHub: "#4612", Title: "Weekly gem upgrades",
				Group: GroupYours, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "magnum fixes #4612 (cycle 1 of 5)", Short: "magnum fixes #4612"}},
				Since:   factoryAgo(now, 3*time.Hour),
				Help:    "agent: magnum fixes #4612 (cycle 1 of 5)",
				PR:      &FactoryPR{Ref: "example/talkable#4612", Fixing: true},
			},
			{
				ID: -81, Key: "#4611", GitHub: "#4611", Jira: "PR-12410", Title: "Cart totals in the widget",
				Group: GroupYours, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "CI", What: "CI at a1b2c3d"}},
				Since:   factoryAgo(now, 10*time.Minute),
				Help:    "CI: CI at a1b2c3d",
				PR:      &FactoryPR{Ref: "example/talkable#4611"},
			},
			{
				ID: -76, Key: "#4606", GitHub: "#4606", Title: "Old sync job cleanup",
				Group: GroupYours, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "#4606 is yours to follow: it was open before magnum first synced the repository",
					Short: "#4606 is yours to follow", OnYou: true}},
				Since: factoryAgo(now, 26*time.Hour),
				Help:  "you: #4606 is yours to follow: it was open before magnum first synced the repository",
				PR:    &FactoryPR{Ref: "example/talkable#4606", Off: "#4606 is yours to follow: it was open before magnum first synced the repository"},
			},
			{
				ID: 6, Key: "PR-12302", GitHub: "#4519", Title: "Remove old sync job",
				Group: GroupDone, Mark: MarkDone,
				WaitsOn: []WaitsOn{{Who: "nothing", What: "merged 13:20"}},
				Help:    "nothing: merged 13:20",
			},
		},
		Decisions: 3,
		Hints:     []hint{{"enter", "answer"}, {"a", "oldest ask"}, {"n", "new task"}, {"?", "more"}},
	}
}

// yoursData is fixtureYours as the screen's source serves it, each PR row
// with its page for b.
func yoursData() FactoryData {
	f := fixtureYours()
	cards := map[int64]FactoryCards{}
	for _, r := range f.Rows {
		c := TaskCard{Key: r.Key, Title: r.Title}
		if r.PR != nil {
			c.PR, c.PRURL = r.GitHub, "https://github.com/example/talkable/pull/"+strings.TrimPrefix(r.GitHub, "#")
		}
		cards[r.ID] = FactoryCards{Task: c}
	}
	q := fixtureQuestionCard()
	q.Current, q.Cursor, q.Revision = 0, -1, 3
	cards[2] = FactoryCards{Task: TaskCard{Key: "PR-12345", Title: q.Title}, Question: &q}
	return FactoryData{Repo: f.Repo, Rows: f.Rows, Decisions: f.Decisions, Cards: cards}
}

// testYours is the screen at w x h over yoursData.
func testYours(t *testing.T, w, h int) *factoryHarness {
	t.Helper()
	hs := testFactory(t, w, h)
	hs.src.change(func(d *FactoryData) { *d = yoursData() })
	hs.press("ctrl+r")
	return hs
}

// YOUR PRS is a group of its own, between the tasks that wait on others
// and those done today, at the spec's three widths: a baseline PR is dim
// with its reason, and nothing is cut past the screen.
func TestFactoryFramesWithYourPRs(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		frame, _ := factoryFrameOf(fixtureYours(), w, 22, true, IconsUnicode, factoryNow)
		checkFactoryFrame(t, fmt.Sprintf("yours-%d.txt", w), ansi.Strip(frame), w)
	}
}

// The chip counts the tasks and the PRs that need you apart ("1 task and
// 2 PRs need you"), and your PRs at work in a dim part of their own; with
// no task to decide it says "2 PRs need you".
func TestTheChipCountsPRsThatNeedYou(t *testing.T) {
	frame, _ := factoryFrameOf(fixtureYours(), 140, 22, true, IconsUnicode, factoryNow)
	chip := strings.Split(ansi.Strip(frame), "\n")[1]
	mustContain(t, chip, "1 task and 2 PRs need you, 3 decisions", "1 waits on others", "3 of your PRs at work", "1 done today")

	f := fixtureYours()
	f.Rows = f.Rows[1:] // the task with questions answered
	f.Decisions = 0
	frame, _ = factoryFrameOf(f, 140, 22, true, IconsUnicode, factoryNow)
	chip = strings.Split(ansi.Strip(frame), "\n")[1]
	mustContain(t, chip, "● 2 PRs need you ·")
	mustNotContain(t, chip, "task")
}

// A row of your PR that magnum leaves alone is drawn dim, its "you:" too:
// it asks nothing.
func TestAnOffPRRowIsDim(t *testing.T) {
	st := newStyles(true)
	p := newFactoryPainter(fixtureYours(), 140, st, newGlyphs(IconsUnicode), factoryNow)
	for _, r := range p.rows {
		if r.Key != "#4606" {
			continue
		}
		for _, sg := range p.waitsCell(r, formMid) {
			if sg.st.Render("x") != st.Dim.Render("x") {
				t.Errorf("the off row's %q is not dim", sg.text)
			}
		}
		return
	}
	t.Fatal("no row #4606")
}

// The cursor keys each PR row by its own id (magnum's, negated): j and k
// move over PR rows and tasks alike, and a new frame keeps the cursor on
// the same PR row though every PR row's id is below zero.
func TestPRRowsKeepTheirOwnCursor(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("g")
	var seen []string
	for range 7 {
		r, ok := h.m.selectedRow()
		if !ok {
			t.Fatalf("no row under the cursor:\n%s", h.view())
		}
		seen = append(seen, r.Key)
		h.press("j")
	}
	want := []string{"PR-12345", "#4602", "#4603", "PR-12371", "#4612", "#4611", "#4606"}
	if strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Fatalf("j walks %q, want %q", seen, want)
	}
	h.press("g", "j", "j", "j", "j", "j") // #4611
	h.src.change(func(d *FactoryData) {
		d.Rows = append(d.Rows[:4:4], append([]FactoryRow{{ID: -90, Key: "#4620", GitHub: "#4620", Title: "New one",
			Group: GroupYours, Mark: MarkWaits, Since: factoryAgo(factoryNow, 4*time.Hour), PR: &FactoryPR{Ref: "example/talkable#4620"}}},
			d.Rows[4:]...)...)
	})
	h.press("ctrl+r")
	if r, _ := h.m.selectedRow(); r.Key != "#4611" {
		t.Errorf("after a new frame the cursor is on %q, want #4611", r.Key)
	}
	h.press("k")
	if r, _ := h.m.selectedRow(); r.Key != "#4612" {
		t.Errorf("k from #4611 lands on %q, want #4612", r.Key)
	}
}

// a walks the task cards only: a PR row that needs you is no card, so the
// walk skips it, and once the cards are done the footer says how many of
// your PRs still need you and their keys.
func TestTheWalkSkipsPRRows(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("a")
	if h.m.view != fvQuestion || h.m.task != 2 {
		t.Fatalf("a opened view %v of task %d, want the questions of PR-12345", h.m.view, h.m.task)
	}
	h.src.change(func(d *FactoryData) {
		d.Rows = d.Rows[1:]
		delete(d.Cards, 2)
	})
	h.press("esc", "ctrl+r", "a")
	if h.m.view != fvHome {
		t.Fatalf("a with only PR rows that need you opened view %v", h.m.view)
	}
	mustContain(t, h.view(), "2 of your PRs need you: f, F or b on their rows")
}

// enter on your PR's row opens no card: the footer names its keys; o and t
// are a task's keys there and say so; b opens the PR's page.
func TestThePRRowsKeysOnHome(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("g", "j") // #4602
	h.press("enter")
	if h.m.view != fvHome {
		t.Fatalf("enter on a PR row opened view %v", h.m.view)
	}
	mustContain(t, h.view(), "your PR: f fix, F fix off, x stop the loop, b browser")
	h.press("o")
	mustContain(t, h.view(), "o and t act on a task's row: #4602 is your PR")
	h.press("t")
	mustContain(t, h.view(), "o and t act on a task's row: #4602 is your PR")
	if n := len(h.act.opens); n != 0 {
		t.Errorf("o on a PR row stepped into %d panes", n)
	}
	h.press("b")
	if got := h.act.opened(); len(got) != 1 || got[0] != "https://github.com/example/talkable/pull/4602" {
		t.Errorf("b on #4602 opened %q", got)
	}
}

// The help glosses YOUR PRS.
func TestTheHelpGlossesYourPRs(t *testing.T) {
	h := testFactory(t, 140, 50)
	mustContain(t, ansi.Strip(strings.Join(h.m.helpContent(140), "\n")),
		"YOUR PRS  your open PRs that no task has, from magnum's registry; one that needs you is in NEEDS YOU")
}
