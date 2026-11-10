package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// withKnown is the harness with the ready card of PR-12360 counting n known
// defects: the leftovers of the local review that are defects, which the
// card's w sends back to the writer.
func withKnown(t *testing.T, n int) *factoryHarness {
	t.Helper()
	h := testFactory(t, 100, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		r := *c.Ready
		r.Known = n
		c.Ready = &r
		d.Cards[3] = c
	})
	h.press("ctrl+r")
	h.open("PR-12360")
	return h
}

// w on a ready card with two known defects asks "Send 2 known defects back
// to the writer?", names the card's binding like any decision, and sends the
// send back with the reason known and no note (the factory writes it) on y.
func TestWSendsTheKnownDefectsBackToTheWriterOnY(t *testing.T) {
	h := withKnown(t, 2)
	h.press("w")
	mustContain(t, h.view(), "Send 2 known defects back to the writer? y/N")
	if h.act.calls() != 0 {
		t.Fatal("the send back ran before its y")
	}
	h.press("y")
	if len(h.act.decides) != 1 {
		t.Fatalf("decides = %+v", h.act.decides)
	}
	d := h.act.decides[0]
	if d.task != 3 || d.choice != FactorySendBack || d.reason != FactoryReasonKnown || d.note != "" {
		t.Errorf("the send back = %+v, want task 3, send_back, the reason known and no note", d)
	}
	if d.bound.HeadSHA != "9c4e2d1f00aa" || d.bound.ReportHash != "report-1" || d.bound.DiffHash != "diff-1" {
		t.Errorf("the send back's binding = %+v, want the card's head and hashes", d.bound)
	}
}

// Any other key than y cancels the question, and one defect is said in the
// singular.
func TestNDoesNotSendTheKnownDefectsBack(t *testing.T) {
	h := withKnown(t, 2)
	h.press("w", "n")
	mustContain(t, h.view(), "cancelled, nothing was run")
	mustNotContain(t, h.view(), "y/N")
	h.press("w", "j")
	if h.act.calls() != 0 {
		t.Errorf("a send back ran without y: %+v", h.act.decides)
	}
	h = withKnown(t, 1)
	h.press("w")
	mustContain(t, h.view(), "Send 1 known defect back to the writer? y/N")
}

// A card with no known defect neither shows w nor does anything with it.
func TestWDoesNothingOnACardWithoutKnownDefects(t *testing.T) {
	h := withKnown(t, 0)
	v := ansi.Strip(h.view())
	if strings.Contains(v, "w send ") {
		t.Errorf("the hints of a card with no known defect show w:\n%s", v)
	}
	h.press("w")
	mustNotContain(t, h.view(), "Send ")
	if h.act.calls() != 0 || h.m.bar.asking != nil {
		t.Errorf("w did something on a card with no known defect: %+v (asking %v)", h.act.decides, h.m.bar.asking != nil)
	}
	if slices.ContainsFunc(h.m.menuItems(), func(it menuItem) bool { return it.key == "w" }) {
		t.Errorf("the menu offers w: %+v", h.m.menuItems())
	}
}

// With known defects the hints and the menu offer w, the hint saying how many
// of the KNOWN lines it sends, and the menu's item asks the same question as
// the key.
func TestTheHintsAndTheMenuOfferWOnACardWithKnownDefects(t *testing.T) {
	h := withKnown(t, 2)
	mustContain(t, h.view(), "w send 2 of 4") // 2 of the 4 lines KNOWN lists
	h.feed(tea.MouseClickMsg{X: 20, Y: 10, Button: tea.MouseRight})
	i := slices.IndexFunc(h.m.menuItems(), func(it menuItem) bool { return it.key == "w" })
	if !h.m.menu.open || i < 0 || !h.m.menuItems()[i].ok || h.m.menuItems()[i].label != "send the known defects back" {
		t.Fatalf("no menu with w: %+v", h.m.menuItems())
	}
	for range i {
		h.press("j")
	}
	h.press("enter")
	mustContain(t, h.view(), "Send 2 known defects back to the writer? y/N")
	if h.act.calls() != 0 {
		t.Fatal("the menu sent the defects back without y")
	}
	h.press("y")
	if len(h.act.decides) != 1 || h.act.decides[0].reason != FactoryReasonKnown {
		t.Errorf("decides = %+v, want the send back for the known defects", h.act.decides)
	}
}

// The help names w among the ready card's keys.
func TestTheHelpNamesWOnTheReadyCard(t *testing.T) {
	h := testFactory(t, 140, 50)
	help := ansi.Strip(strings.Join(h.m.helpContent(200), "\n"))
	_, card, ok := strings.Cut(help, "\nReady card\n")
	card, _, _ = strings.Cut(card, "\n\n")
	if !ok {
		t.Fatalf("the help has no Ready card section:\n%s", help)
	}
	if !slices.ContainsFunc(strings.Split(card, "\n"), func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "w ") }) {
		t.Errorf("the Ready card section of the help does not name w:\n%s", card)
	}
}
