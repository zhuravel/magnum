package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// o on a row whose judge is parked (o restores it in a slot) asks y/N first,
// naming how long the restore holds the PR; only y sends the open. With
// open_hold 0 the question says the pin lasts until the unpin.
func TestOOnARowWhoseJudgeIsParkedAsksFirst(t *testing.T) {
	for _, c := range []struct {
		hold time.Duration
		want string
	}{
		{30 * time.Minute, "Restore example#7 in a slot to read it? It holds the PR until 30 minutes after your last input."},
		{0, "Restore example#7 in a slot to read it? It holds the PR until you unpin it."},
	} {
		row := repliesRow(0)
		row.OpenRestores, row.OpenHold = true, c.hold
		m, act := repliesBoard(t, 200, 12, PRBoardOptions{}, row)
		asked, _ := send(t, m, keyMsg("o"))
		if asked.confirm == nil || asked.confirm.question != c.want {
			t.Fatalf("o asked %+v\nwant %q", asked.confirm, c.want)
		}
		mustContain(t, viewOf(asked), c.want+" y/N")
		boardAct(t, asked, "n")
		if len(act.calls) != 0 {
			t.Fatalf("n opened: %v", act.calls)
		}
		again, _ := send(t, m, keyMsg("o"))
		boardAct(t, again, "y")
		if !slices.Equal(act.calls, []string{"open " + repliesRef}) {
			t.Fatalf("y called %v", act.calls)
		}
	}
}

// o on a row whose judge is live focuses its pane at once, as before.
func TestOOnALiveRowFocusesAtOnce(t *testing.T) {
	row := repliesRow(0)
	row.OpenHold = 30 * time.Minute
	m, act := repliesBoard(t, 200, 12, PRBoardOptions{}, row)
	asked, _ := boardAct(t, m, "o")
	if asked.confirm != nil {
		t.Fatalf("o asked %q on a live row", asked.confirm.question)
	}
	if !slices.Equal(act.calls, []string{"open " + repliesRef}) {
		t.Fatalf("o called %v", act.calls)
	}
}

// A PR an open holds shows as held with the hold's end within its one-line
// row: its state cell says "held → 15:20" where nothing else waits there,
// its card "held by open until 15:20", and the title line counts it held,
// apart from the lasting pins.
func TestAnOpensHoldShowsAsHeldWithItsEnd(t *testing.T) {
	held := repliesRow(0)
	held.Pinned, held.HeldUntil = true, boardNow.Add(20*time.Minute)
	pinned := repliesRow(0)
	pinned.Ref, pinned.Number, pinned.Pinned = "talkable/example#8", 8, true
	clock := untilClock(held.HeldUntil, boardNow)
	p := repliesPainter(IconsUnicode, held, pinned)
	if got := cellText(p.stateWaitCell(held)); !strings.HasSuffix(got, " held → "+clock) {
		t.Errorf("state cell %q, want the hold's end %s", got, clock)
	}
	if got := cellText(p.stateWaitCell(pinned)); strings.Contains(got, "held") {
		t.Errorf("a lasting pin's state cell says held: %q", got)
	}
	ended := held
	ended.HeldUntil = boardNow.Add(-time.Minute) // the next tick unpins it
	if got := cellText(p.stateWaitCell(ended)); strings.Contains(got, "held") {
		t.Errorf("an ended hold shows: %q", got)
	}
	card := squash(ansi.Strip(strings.Join(p.cardContent(sanitizeRow(held), 120), "\n")))
	mustContain(t, card, "held by open until "+clock+" (u hands it back, p keeps it)")
	if strings.Contains(card, " pinned") {
		t.Errorf("the held card says pinned:\n%s", card)
	}
	summary := ansi.Strip(p.summaryLine(200))
	mustContain(t, summary, "1 pinned · 1 held")
}
