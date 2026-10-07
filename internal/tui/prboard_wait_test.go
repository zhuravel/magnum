package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A PR waiting for a round shows why in its state cell (compact) and its
// card (the sentence with the command that lifts it).
func TestPRBoardShowsWhyAPRWaits(t *testing.T) {
	m, _, _ := newBoard(t, 160, 50, PRBoardOptions{})
	p := m.painter()
	var row PRBoardRow
	for _, r := range boardRows() {
		if r.Number == 11950 {
			row = r
		}
	}
	row.Wait = "re-review · cap 6/6 → 00:00"
	row.WaitDetail = "re-review waits for the daily round cap (6 of 6 today) until 00:00; `magnum review talkable#11950` runs it now"

	cell := ansi.Strip(p.stateWaitCell(sanitizeRow(row)).render(nil))
	if !strings.Contains(cell, "re-review") || !strings.HasSuffix(cell, " cap 6/6 → 00:00") {
		t.Errorf("state cell = %q", cell)
	}
	plain := ansi.Strip(p.stateWaitCell(PRBoardRow{State: "reviewed"}).render(nil))
	if strings.Contains(plain, "·") || strings.Contains(plain, "→") {
		t.Errorf("a PR without a wait shows one: %q", plain)
	}

	card := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(row), 120), "\n"))
	mustContain(t, card, "Next review", "cap 6/6 → 00:00", "WAITING",
		"re-review waits for the daily round cap (6 of 6 today) until 00:00; `magnum review talkable#11950` runs it now")
	if c := ansi.Strip(strings.Join(p.cardContent(sanitizeRow(PRBoardRow{State: "reviewed"}), 120), "\n")); strings.Contains(c, "WAITING") {
		t.Errorf("a PR without a wait has a WAITING section:\n%s", c)
	}
}

// A round waiting on its pinned slot reads as a wait, not an error: the
// title has no error mark, the state cell says "pinned → u", and the card
// says who pinned it and that u unpins it.
func TestBoardShowsAPinWaitWithoutAnErrorMark(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	r := PRBoardRow{Ref: "talkable/talkable#5", Owner: "talkable", Repo: "talkable", Number: 5, Title: "Coupon export",
		State: "rereview_pending", GHState: "OPEN", Pinned: true, Slot: "review1",
		Wait:       "re-review · pinned → u",
		WaitDetail: "re-review waits: slot review1 is pinned by magnum open since 22:52; `magnum unpin talkable#5` lets it run",
		PinWait:    true, PinnedBy: "magnum open"}
	if title := ansi.Strip(p.titleCell(r).render(nil)); strings.Contains(title, p.g.errMark) {
		t.Errorf("title of a pin wait carries the error mark: %q", title)
	}
	if cell := ansi.Strip(p.stateWaitCell(r).render(nil)); !strings.HasSuffix(cell, " pinned → u") {
		t.Errorf("state cell = %q", cell)
	}
	card := ansi.Strip(strings.Join(p.cardContent(r, 160), "\n"))
	mustContain(t, card, "pinned by magnum open (u unpins)")
	if strings.Contains(card, "LAST ERROR") {
		t.Errorf("the card of a pin wait shows an error:\n%s", card)
	}
	r.PinnedBy = ""
	mustContain(t, ansi.Strip(strings.Join(p.cardContent(r, 160), "\n")), "pinned (u unpins)")
}

// A retry's state cell names the attempt and the cause, and a narrow column
// drops the cause first.
func TestBoardRetryCellDropsTheCauseWhenNarrow(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	r := PRBoardRow{Ref: "talkable/talkable#5", State: "rereview_pending", GHState: "OPEN",
		Wait: "re-review · retry 2/3 setup → 22:57", WaitNarrow: "re-review · retry 2/3 → 22:57"}
	cs := p.cells(r, [3]int{})
	if cell := ansi.Strip(cs.stateFit(80).render(nil)); !strings.HasSuffix(cell, " retry 2/3 setup → 22:57") {
		t.Errorf("wide state cell = %q", cell)
	}
	if len(cs.stateAlt) != 1 {
		t.Fatalf("state forms = %d, want the narrow one", len(cs.stateAlt))
	}
	w := cs.stateAlt[0].width()
	if cell := ansi.Strip(cs.stateFit(w).render(nil)); !strings.HasSuffix(cell, " retry 2/3 → 22:57") {
		t.Errorf("narrow state cell = %q", cell)
	}
	r.WaitNarrow = ""
	if cs := p.cells(r, [3]int{}); len(cs.stateAlt) != 0 {
		t.Errorf("a wait without a narrow form has %d", len(cs.stateAlt))
	}
}

// The needs-you pill gives way to a state that says more: a PR waiting in
// line, paused or needing attention shows that state's pill; a reviewed PR
// keeps "needs you".
func TestNeedsYouGivesWayToQueuedPausedAndAttention(t *testing.T) {
	m, _ := snoozeBoard(t, snzOpen, time.Time{})
	p := m.painter()
	for state, want := range map[string]string{
		"queued":           stateLabel("queued"),
		"rereview_pending": stateLabel("rereview_pending"),
		"paused":           stateLabel("paused"),
		"needs_attention":  stateLabel("needs_attention"),
		"reviewed":         "needs you",
	} {
		r := PRBoardRow{Ref: "talkable/talkable#5", State: state, GHState: "OPEN", NeedsMe: NeedsMeApprove}
		cell := ansi.Strip(p.stateWaitCell(r).render(nil))
		if !strings.Contains(cell, want) {
			t.Errorf("%s with needs-you: state cell %q, want %q", state, cell, want)
		}
		if state != "reviewed" && strings.Contains(cell, "needs you") {
			t.Errorf("%s: the needs-you pill hides the state: %q", state, cell)
		}
	}
}
