package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// stagingCells are a published task's three staging cells, read a minute
// before factoryNow, in the config's order.
func stagingCells(void, bart, bastion string) []StagingCell {
	seen := factoryAgo(factoryNow, time.Minute)
	return []StagingCell{{Name: "void", State: void, Seen: seen}, {Name: "bart", State: bart, Seen: seen},
		{Name: "bastion", State: bastion, Seen: seen}}
}

// fixtureStagings is home with three published tasks whose rows carry the
// staging cells the factory reads from magnum's staging status: every state
// it maps to (latest, CI, CI failed on the PR's change, deploying, outdated,
// none and unknown), and one task without a PR, which has none.
func fixtureStagings() factoryFrame {
	now := factoryNow
	return factoryFrame{
		Repos: []string{"example/talkable"},
		Rows: []FactoryRow{
			{
				ID: 1, Key: "PR-12320", GitHub: "#4530", Title: "Coupon rounding in the cart",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn:  []WaitsOn{{Who: "reviewers", What: "nothing open at a1b2c3d · no approval yet", Short: "no approval"}},
				Stagings: stagingCells("latest", "ci", "none"),
				Since:    factoryAgo(now, 3*time.Hour),
				Help:     "reviewers: nothing open at a1b2c3d · no approval yet",
			},
			{
				ID: 2, Key: "PR-12321", GitHub: "#4531", Title: "Advocate offer email locale",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn:  []WaitsOn{{Who: "magnum", What: "looking at #4531"}},
				Stagings: stagingCells("ci-fail", "deploying", "outdated"),
				Since:    factoryAgo(now, 2*time.Hour),
				Help:     "magnum: looking at #4531",
			},
			{
				ID: 3, Key: "PR-12322", GitHub: "#4532", Title: "Weekly gem upgrades",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn:  []WaitsOn{{Who: "rev-ann", What: "review of #4532"}},
				Stagings: stagingCells("unknown", "latest", "deploying"),
				Since:    factoryAgo(now, time.Hour),
				Help:     "rev-ann: review of #4532",
			},
			{
				ID: 4, Key: "PR-12371", Title: "Coupon CSV export timeout",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "agent", What: "finding the cause, 2 of 3 done", Short: "finding the cause"}},
				Since:   factoryAgo(now, 9*time.Minute),
				Help:    "agent: finding the cause, 2 of 3 done",
			},
		},
		Hints: []hint{{"enter", "task card"}, {"n", "new task"}, {"?", "more"}},
	}
}

// Rows with three staging cells, narrow and wide: a narrow screen draws
// the short forms, a wide one the long forms with each cell's age; a CI
// that failed on the PR's own change reads CI ✗ in the failure's color,
// latest in OK's, none dim, and the row without a PR has no cell.
func TestFactoryFramesOfStagingCells(t *testing.T) {
	for _, w := range []int{100, 200} {
		frame, _ := factoryFrameOf(fixtureStagings(), w, factoryHomeHeight, true, IconsUnicode, factoryNow)
		checkFactoryFrame(t, fmt.Sprintf("stagings-%d.txt", w), ansi.Strip(frame), w)
	}
	frame, _ := factoryFrameOf(fixtureStagings(), 140, factoryHomeHeight, true, IconsUnicode, factoryNow)
	checkFactoryFrame(t, "stagings-140-dark.ansi", frame, 140)
	ascii, _ := factoryFrameOf(fixtureStagings(), 100, factoryHomeHeight, true, IconsASCII, factoryNow)
	checkFactoryFrame(t, "stagings-100-ascii.txt", ansi.Strip(ascii), 100)
}

// The staging frames never draw a line wider than the screen.
func TestFactoryStagingFramesNeverOverflow(t *testing.T) {
	for _, mode := range []IconMode{IconsUnicode, IconsNerd, IconsASCII} {
		for _, w := range factoryWidths(40) {
			frame, _ := factoryFrameOf(fixtureStagings(), w, factoryHomeHeight, true, mode, factoryNow)
			for i, l := range strings.Split(frame, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%s %d: line %d is %d cells wide: %q", mode, w, i+1, lw, ansi.Strip(l))
				}
			}
			card := ansi.Strip(drawTask(fixtureStagingCard(), 0)(w, 0, true, mode))
			for i, l := range strings.Split(card, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("card %s %d: line %d is %d cells wide: %q", mode, w, i+1, lw, l)
				}
			}
		}
	}
}

// fixtureStagingCard is a published task's card with its PR's staging
// cells.
func fixtureStagingCard() TaskCard {
	c := fixturePublishedCard(0)
	c.WaitingFor = "reviewers: nothing open at a1b2c3d · no approval yet"
	c.Stagings = stagingCells("latest", "ci", "none")
	return c
}

// The task card says where its PR stands on each staging server in one
// line under the PR's facts, each with its state and age, as short as the
// card's other lines.
func TestTaskCardSaysWhereItsPRStandsOnEachStaging(t *testing.T) {
	checkCardGolden(t, "task-stagings", drawTask(fixtureStagingCard(), 0))
	frame := plainCard(drawTask(fixtureStagingCard(), 0), 100)
	if l := cardLineWith(t, frame, "STAGINGS"); strings.TrimSpace(l) != "STAGINGS     void: latest 1m · bart: CI 1m · bastion: none" {
		t.Errorf("the staging line is %q", l)
	}
	c := fixtureStagingCard()
	c.Stagings = nil
	if strings.Contains(plainCard(drawTask(c, 0), 100), "STAGINGS") {
		t.Error("a card without staging cells draws a STAGINGS line")
	}
}

// The help glosses the staging cells' words.
func TestTheHelpGlossesTheStagingCells(t *testing.T) {
	h := testFactory(t, 140, 80)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	mustContain(t, help, "VOID, BART", "CI ✗", "deploying", "outdated")
}
