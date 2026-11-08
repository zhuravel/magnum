package tui

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The QA report the screen tests open: summary.md of the QA run that
// passed, in the task's directory.
const qaReport = "/home/op/.local/share/magnum/factory/tasks/3/qa/1-abc1234/summary.md"

// fixtureReadyCardQA is the ready card of a change QA walked: its QA line,
// the after side's shots and its report, QA's note under KNOWN, and the
// state it could not reach on the FLAGS line.
func fixtureReadyCardQA() ReadyCard {
	c := fixtureReadyCard()
	c.QA = "passed with 1 state not reached: 3 pages walked at abc1234, 1 note"
	c.Screenshots, c.ScreenshotsNote = 3, "the after side, from the copy's own server; e opens the QA report"
	c.QAReport = qaReport
	c.Flags = append(slices.Clone(c.Flags), "QA did not reach: the passkey autofill list (headless Chrome draws no autofill)")
	c.Leftovers = append(slices.Clone(c.Leftovers), "qa: P3 broken_layout: the table scrolls sideways")
	return c
}

// The ready card of a walked change draws its QA line after PROOF, the
// shots of the after side, and e for the QA shots; a card without QA draws
// no QA row and offers no e.
func TestTheReadyCardDrawsItsQALine(t *testing.T) {
	d := drawReady(fixtureReadyCardQA())
	frame := plainCard(d, 140)
	checkFactoryFrame(t, "ready-qa-140.txt", frame, 140)
	lines := strings.Split(frame, "\n")
	at := func(prefix string) int {
		return slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
	}
	if qa, proof, files := at(" QA "), at(" PROOF "), at(" FILES "); qa < 0 || qa < proof || qa > files {
		t.Errorf("the QA row is line %d, PROOF %d, FILES %d: want it after PROOF, before FILES:\n%s", qa, proof, files, frame)
	}
	mustContain(t, frame, "SCREENSHOTS  3, the after side, from the copy's own server; e opens the QA report", "e QA shots")
	bare := plainCard(drawReady(fixtureReadyCard()), 140)
	mustNotContain(t, bare, " QA ", "e QA shots", "e screenshots")
}

// testQAReady is the screen with PR-12360's ready card walked by QA, the
// pager swapped for one that records what it opens.
func testQAReady(t *testing.T) (*factoryHarness, *[]string) {
	t.Helper()
	h := testFactory(t, 140, 50)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[3]
		r := fixtureReadyCardQA()
		r.Recommended, r.Cursor = 0, 0
		c.Ready = &r
		d.Cards[3] = c
	})
	h.press("ctrl+r")
	var opened []string
	h.m.pager = func(_ context.Context, path string) *exec.Cmd {
		opened = append(opened, path)
		return exec.Command("true")
	}
	return h, &opened
}

// e on the ready card opens the QA run's report in the pager the fixer's
// report uses, asking nothing, and does not count as reading the diff; a
// card without a QA report says so and opens nothing.
func TestEOpensTheQAReportOnTheReadyCard(t *testing.T) {
	h, opened := testQAReady(t)
	h.open("PR-12360")
	mustContain(t, h.view(), "PR-12360 · ready to push", "e QA shots")
	h.press("e")
	if !slices.Equal(*opened, []string{qaReport}) || len(h.execs) != 1 {
		t.Fatalf("e opened %q with %d ExecProcess messages, want the QA report", *opened, len(h.execs))
	}
	if h.m.bar.asking != nil {
		t.Error("e asked a question")
	}
	h.feed(factoryPagerMsg{task: 3})
	if rs := h.m.readyState(); !rs.diffAt.IsZero() {
		t.Error("reading the QA report counted as reading the diff")
	}
	if menu := menuState(h.m.menuItems()); !menu["read the QA report"] {
		t.Errorf("the ready card's menu %v, want the QA report", menu)
	}

	b := testFactory(t, 140, 50)
	b.m.pager = h.m.pager
	b.open("PR-12360")
	mustNotContain(t, b.view(), "e QA shots", "e screenshots")
	b.press("e")
	mustContain(t, b.view(), "no QA report for PR-12360")
	if len(*opened) != 1 || len(b.execs) != 0 {
		t.Errorf("e on a card without QA opened %q", *opened)
	}
}

// A on the task card accepts what the writer declined of QA's defects, as
// `magnum factory retry --accept-qa` does: only where the factory offers
// it, naming what it accepts, behind y/N; any other key cancels.
func TestAAcceptsQAsDeclinesBehindYN(t *testing.T) {
	h := testFactory(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[5]
		c.Task = fixtureRetryCard()
		c.Task.AcceptQA = "1 QA defect the writer declined at abc1234 (flow on /settings/security)"
		d.Cards[5] = c
	})
	h.press("ctrl+r")
	h.open("PR-12330")
	mustContain(t, h.view(), "A accept QA")
	h.press("A")
	mustContain(t, cardSquash(h.view()), "Accept for PR-12393: 1 QA defect the writer declined at abc1234 (flow on /settings/security), "+
		"which the PR body then names as accepted by you?")
	h.press("n")
	if h.act.calls() != 0 {
		t.Fatal("A accepted before y")
	}
	h.press("A", "y")
	if !slices.Equal(h.act.acceptsQA, []int64{5}) || len(h.act.retries) != 0 {
		t.Errorf("accepts %v, retries %v, want one accept of task 5", h.act.acceptsQA, h.act.retries)
	}
	if menu := menuState(h.m.menuItems()); !menu["accept for QA"] {
		t.Errorf("the task card's menu %v, want the accept", menu)
	}

	plain := testFactory(t, 140, 40)
	plain.src.change(func(d *FactoryData) {
		c := d.Cards[5]
		c.Task = fixtureRetryCard()
		d.Cards[5] = c
	})
	plain.press("ctrl+r")
	plain.open("PR-12330")
	mustNotContain(t, plain.view(), "A accept QA")
	plain.press("A")
	if plain.act.calls() != 0 || plain.m.bar.asking != nil {
		t.Error("A on a task with nothing to accept asked or ran something")
	}
	if menu := menuState(plain.m.menuItems()); menu["accept for QA"] {
		t.Errorf("the task card's menu %v offers the accept", menu)
	}
}

// The help names e on the ready card and A on the task card.
func TestTheHelpNamesTheQAKeys(t *testing.T) {
	h := testFactory(t, 140, 40)
	help := ansi.Strip(strings.Join(h.m.helpContent(140), "\n"))
	mustContain(t, help, "e the QA run's report (summary.md) in the pager, when QA walked the change (asks nothing)",
		"A accept the QA defects the writer declined, or the fixes QA did not walk again, at the task's head (y/N)")
}

// testQAStop is the screen with PR-12330's task card at a stop of QA's fix
// loop on the writer's declines: A to accept them, and the run's report,
// which e opens in the pager (recorded).
func testQAStop(t *testing.T) (*factoryHarness, *[]string) {
	t.Helper()
	h := testFactory(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[5]
		c.Task = fixtureRetryCard()
		c.Task.AcceptQA = "1 QA defect the writer declined at abc1234 (flow on /settings/security)"
		c.Task.QAReport = qaReport
		d.Cards[5] = c
	})
	h.press("ctrl+r")
	var opened []string
	h.m.pager = func(_ context.Context, path string) *exec.Cmd {
		opened = append(opened, path)
		return exec.Command("true")
	}
	return h, &opened
}

// At a stop on the writer's declines, the task card's e opens that run's
// report in the same pager, asking nothing, so you see the shots before you
// press A; the hints and the menu offer it by the report's name.
func TestEOpensTheQAReportOnTheTaskCardAtADeclineStop(t *testing.T) {
	h, opened := testQAStop(t)
	h.open("PR-12330")
	mustContain(t, h.view(), "e summary.md", "A accept QA")
	h.press("e")
	if !slices.Equal(*opened, []string{qaReport}) || len(h.execs) != 1 || h.m.bar.asking != nil {
		t.Fatalf("e opened %q with %d ExecProcess messages (asking %v), want the QA report", *opened, len(h.execs), h.m.bar.asking != nil)
	}
	if menu := menuState(h.m.menuItems()); !menu["read the QA report"] {
		t.Errorf("the task card's menu %v, want the QA report", menu)
	}
}

// The menu's accept asks the same y/N as A does, and only y accepts.
func TestTheMenusAcceptAsksYN(t *testing.T) {
	h, _ := testQAStop(t)
	h.open("PR-12330")
	h.feed(tea.MouseClickMsg{X: 20, Y: 10, Button: tea.MouseRight})
	i := slices.IndexFunc(h.m.menuItems(), func(it menuItem) bool { return it.label == "accept for QA" })
	if !h.m.menu.open || i < 0 {
		t.Fatalf("no menu with the accept: %+v", h.m.menuItems())
	}
	for range i {
		h.press("j")
	}
	h.press("enter")
	mustContain(t, cardSquash(h.view()), "Accept for PR-12393:", "y/N")
	if h.act.calls() != 0 {
		t.Fatal("the menu accepted without y")
	}
	h.press("y")
	if !slices.Equal(h.act.acceptsQA, []int64{5}) {
		t.Errorf("accepts %v, want task 5's", h.act.acceptsQA)
	}
}

// A KNOWN row that gives way names where its hidden lines are: QA's under
// Visual QA, the local review's under Known.
func TestACollapsedKnownRowNamesWhereQAsNotesAre(t *testing.T) {
	c := fixtureReadyCardQA()
	p := newCardPainter(140, newStyles(true), newGlyphs(IconsUnicode), cardQuestionNow)
	f := readyFit{flags: len(c.Flags), files: len(c.Files), known: 0}
	got := ansi.Strip(strings.Join(p.readyLists(c, f), "\n"))
	mustContain(t, got, "5 notes, listed under Known and Visual QA in the PR body")
	c = fixtureReadyCard()
	f = readyFit{flags: len(c.Flags), files: len(c.Files), known: 0}
	mustContain(t, ansi.Strip(strings.Join(p.readyLists(c, f), "\n")), "4 notes, listed under Known in the PR body")
}
