package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fixturePublished is home with the factory's published tasks as their PRs'
// verdicts in magnum's registry put them (factory.ScreenData's words): one
// whose PR needs you, one approved with nothing open, one magnum's fixer
// works on, one whose verdict is stale, one that waits on its reviewers,
// and one merged today.
func fixturePublished() factoryFrame {
	now := factoryNow
	return factoryFrame{
		Repos: []string{"example/talkable"},
		Rows: []FactoryRow{
			{
				ID: 1, Key: "PR-12320", GitHub: "#4530", Title: "Coupon rounding in the cart",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "2 items need you: 1 thread of a person and 1 skipped finding",
					Short: "2 items need you", OnYou: true}},
				// A task whose PR magnum's registry does not have (no PR field): its move is the task's. With a PR
				// the factory words it by the reason (reasonMove): "b opens it" for these two kinds of item.
				Move: "see the task card", MoveShort: "task card",
				Since: factoryAgo(now, 35*time.Minute),
				Help:  "you: 2 items need you: 1 thread of a person and 1 skipped finding",
			},
			{
				ID: 2, Key: "PR-12324", GitHub: "#4534", Title: "Referral export column order",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "nothing open at a1b2c3d · approved, merge is yours · 2 commits after the approval",
					Short: "nothing open at a1b2c3d", OnYou: true}},
				Move: "merge #4534 on GitHub", MoveShort: "merge it",
				Since: factoryAgo(now, 12*time.Minute),
				Help:  "you: nothing open at a1b2c3d · approved, merge is yours · 2 commits after the approval",
			},
			{
				ID: 3, Key: "PR-12321", GitHub: "#4531", Title: "Advocate offer email locale",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "magnum fixes #4531 (cycle 2 of 5)", Short: "magnum fixes #4531"}},
				Since:   factoryAgo(now, 9*time.Minute),
				Help:    "agent: magnum fixes #4531 (cycle 2 of 5)",
			},
			{
				ID: 4, Key: "PR-12323", GitHub: "#4533", Title: "Weekly gem upgrades",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "magnum", What: "looking at #4533"}},
				Since:   factoryAgo(now, 2*time.Hour),
				Help:    "magnum: looking at #4533",
			},
			{
				ID: 5, Key: "PR-12325", GitHub: "#4535", Title: "Deploy list on campaigns",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "reviewers", What: "nothing open at a1b2c3d · no approval yet", Short: "nothing open at a1b2c3d"}},
				Since:   factoryAgo(now, 3*time.Hour),
				Help:    "reviewers: nothing open at a1b2c3d · no approval yet",
			},
			{
				ID: 6, Key: "PR-12302", GitHub: "#4519", Title: "Remove old sync job",
				Group: GroupDone, Mark: MarkDone,
				WaitsOn: []WaitsOn{{Who: "nothing", What: "merged 13:20"}},
				Help:    "nothing: merged 13:20",
			},
		},
		Hints: []hint{{"enter", "task card"}, {"n", "new task"}, {"?", "more"}},
	}
}

// fixturePublishedCard is the task card of a published task whose PR needs
// you: the verdict's words on WAITING FOR and the links of what needs you
// under it.
func fixturePublishedCard(links int) TaskCard {
	c := TaskCard{
		Key: "PR-12320", Kind: "bug", Title: "Coupon rounding in the cart", PR: "#4530",
		Branch: "zhuravel-PR-12320-coupon-rounding", Started: cardAt(8, 40),
		WaitingFor: "you: 2 items need you: 1 thread of a person and 1 skipped finding",
		Stages: []CardStage{
			{Name: "start", State: "done", Proof: "Jira PR-12320 read", Finished: cardAt(8, 40)},
			{Name: "coding", State: "done", Proof: "spec red before, green after", Finished: cardAt(9, 35)},
			{Name: "pushed", State: "done", Proof: "#4530 opened", Finished: cardAt(10, 2)},
		},
		Cursor: -1,
	}
	for i := range links {
		c.Links = append(c.Links, fmt.Sprintf("https://github.com/example/talkable/pull/4530#discussion_r%d", 9100+i))
	}
	return c
}

// The published tasks' rows, by their PRs' verdicts, at the spec's three
// widths: the fixing row moves, the needs-you rows are yellow, and nothing
// is cut past the screen.
func TestFactoryFramesOfPublishedTasks(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		frame, _ := factoryFrameOf(fixturePublished(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		checkFactoryFrame(t, fmt.Sprintf("published-%d.txt", w), ansi.Strip(frame), w)
	}
}

// The task card lists the links of what needs you under WAITING FOR,
// without their scheme; past five it says how many more there are.
func TestTaskCardListsTheLinksThatNeedYou(t *testing.T) {
	checkCardGolden(t, "task-links", drawTask(fixturePublishedCard(2), 0))
	frame := plainCard(drawTask(fixturePublishedCard(7), 0), 100)
	for i := range 5 {
		if !strings.Contains(frame, fmt.Sprintf("github.com/example/talkable/pull/4530#discussion_r%d", 9100+i)) {
			t.Errorf("link %d is not on the card:\n%s", i, frame)
		}
	}
	if strings.Contains(frame, "discussion_r9105") || !strings.Contains(frame, "+2 more") {
		t.Errorf("the card with 7 links should draw 5 and say +2 more:\n%s", frame)
	}
	if strings.Contains(frame, "https://") {
		t.Errorf("a link keeps its scheme:\n%s", frame)
	}
}

// The published frames never draw a line wider than the screen.
func TestFactoryPublishedFramesNeverOverflow(t *testing.T) {
	for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
		for _, w := range factoryWidths(40) {
			frame, _ := factoryFrameOf(fixturePublished(), w, factoryHomeHeight, true, mode, factoryNow)
			for i, l := range strings.Split(frame, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%s %d: line %d is %d cells wide: %q", mode, w, i+1, lw, ansi.Strip(l))
				}
			}
			card := ansi.Strip(drawTask(fixturePublishedCard(7), 0)(w, 0, true, mode))
			for i, l := range strings.Split(card, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("card %s %d: line %d is %d cells wide: %q", mode, w, i+1, lw, l)
				}
			}
		}
	}
}

// The help's words gloss what a published task's row says: DONE TODAY is
// a merge of today (a push is not done any more), magnum: is magnum at the
// PR in any way (a review, a look, a wait for its daemon or a reopen), and
// reviewers: has its own gloss.
func TestTheHelpGlossesThePublishedRowsWords(t *testing.T) {
	h := testFactory(t, 140, 50)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	mustContain(t, help, "done today  its PR merged today",
		"magnum:  magnum reviews or reads the PR, or waits for its daemon or a reopen",
		"reviewers:  the PR's reviewers must review, approve or settle")
	mustNotContain(t, help, "pushed today", "a magnum review round is running")
}

// From 120 columns every row with a PR shows its number in a column of its
// own, between the key and the title, a task's and your PR's alike; the
// key is then the Jira key. Before, the number showed only when the whole
// wide layout fitted, about 210 columns with a long title.
func TestFactoryRowsKeepTheirPRNumberFrom120Columns(t *testing.T) {
	for name, f := range map[string]factoryFrame{"published": fixturePublished(), "repos": fixtureTwoRepos()} {
		for _, w := range []int{120, 140, 160, 200} {
			frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
			at := -1
			for i, line := range factoryRowLines(t, frame, f.Rows) {
				r := f.Rows[i]
				if r.GitHub == "" {
					continue
				}
				x, title := strings.Index(line, r.GitHub+" "), strings.Index(line, r.Title[:10])
				if x >= 0 {
					x = ansi.StringWidth(line[:x])
				}
				switch {
				case x < 0 || title < 0 || x > ansi.StringWidth(line[:title]):
					t.Errorf("%s at %d: the row of %s has no PR column with %s: %q", name, w, r.Key, r.GitHub, line)
				case at >= 0 && x != at:
					t.Errorf("%s at %d: %s's number at %d, the others' at %d: %q", name, w, r.Key, x, at, line)
				default:
					at = x
				}
				if key := r.jiraKey(); key != "" && !strings.Contains(line, key+" ") {
					t.Errorf("%s at %d: the row of %s does not show its Jira key %s: %q", name, w, r.Key, key, line)
				}
			}
		}
	}
}

// A long title no longer costs a wide screen its wide layout: the layout
// squeezes the title, down to 24 cells, and keeps the long blockers and
// the staging cells' ages. Before, a 93-cell title needed every column at
// its natural width, so 200 columns drew the narrow forms.
func TestFactoryTheWideLayoutSqueezesALongTitle(t *testing.T) {
	f := fixtureHome()
	f.Rows[1].Title = "Credit expiry reminder email goes out a week before the credit expires, in the advocate's locale"
	frame, _ := factoryFrameOf(f, 200, factoryHomeHeight, true, IconsUnicode, factoryNow)
	plain := ansi.Strip(frame)
	mustContain(t, plain, "agent: fixing 2 conflicts in bart", "conflict 11m", "3 questions before coding · first: when is it sent?")
	line := factoryRowLines(t, frame, f.Rows[:len(f.Rows)])[1]
	if strings.Contains(line, f.Rows[1].Title) || !strings.Contains(line, "Credit expiry reminder email goes") {
		t.Errorf("the long title is not squeezed to at least 24 cells: %q", line)
	}
}

// The task card's facts start with the repository and the PR's number
// ("example/talkable #4530"), then the branch; without a PR yet, the
// repository alone. Before, the card named no repository.
func TestTaskCardNamesItsRepositoryFirst(t *testing.T) {
	c := fixturePublishedCard(0)
	c.Repo = "example/talkable"
	facts := strings.Split(plainCard(drawTask(c, 0), 140), "\n")[1]
	if !strings.HasPrefix(facts, " example/talkable #4530 · zhuravel-PR-12320-coupon-rounding · started") {
		t.Errorf("the facts %q do not start with the repository and its PR", facts)
	}
	c.PR = ""
	facts = strings.Split(plainCard(drawTask(c, 0), 140), "\n")[1]
	if !strings.HasPrefix(facts, " example/talkable · zhuravel-PR-12320-coupon-rounding · started") {
		t.Errorf("the facts %q of a card without a PR do not start with the repository", facts)
	}
}
