package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A plan's question (E's staging deploy, P's merge of the parent) goes up
// only where the screen draws it whole: home and the task card. Its plan
// runs in the background while the navigation keys work, so it can answer
// on a view with a question of its own (the recap's "Send 2 answers?"), on
// a card that draws three lines of a question (the question card, the
// ready card, whose push question adds "It frees …"), on the new task's
// form, where keys type, on the help or the log, whose rule draws one cut
// line, or on home while the filter is typed. There it waits, with a note,
// and comes up once home or the task card is free: whatever y confirms is
// the question on the screen. Before, the plan's question took the bar at
// once: on the recap, y on "Send 2 answers? y/N" deployed to staging (or
// merged the parent) and sent no answers.

// testPlanned is the factory's fixture screen at w x h with PR-12345 (a
// task that needs your answers) carrying a PR in magnum's registry, with
// three staging servers.
func testPlanned(t *testing.T, w, h int) *factoryHarness {
	t.Helper()
	hs := testFactory(t, w, h)
	hs.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "PR-12345" {
				d.Rows[i].PR = &FactoryPR{Ref: "example/talkable#4545"}
				d.Rows[i].Stagings = stagingCells("latest", "ci", "none")
			}
		}
	})
	hs.press("ctrl+r")
	return hs
}

// startHeld presses keys on PR-12345's row, the last of which starts a
// plan, and gives back the plan's command without running it: its answer
// comes when the test runs it.
func (h *factoryHarness) startHeld(keys ...string) tea.Cmd {
	h.t.Helper()
	h.press("g")
	h.selectRow("PR-12345")
	h.press(keys[:len(keys)-1]...)
	next, cmd := h.m.Update(keyMsg(keys[len(keys)-1]))
	h.m = next.(factoryModel)
	if h.m.bar.busy == "" {
		h.t.Fatalf("%v started no plan:\n%s", keys, h.view())
	}
	return cmd
}

// toRecap answers PR-12345's two questions from home: its recap asks "Send
// 2 answers?".
func (h *factoryHarness) toRecap() {
	h.t.Helper()
	h.press("enter", "2", "1")
	if h.m.view != fvRecap {
		h.t.Fatalf("view %v, want the recap:\n%s", h.m.view, h.view())
	}
}

// On the recap, y sends the answers it asks for, never the plan that
// answered meanwhile; once the recap is left for home, the plan's question
// is up there, and y deploys.
func TestAStagingPlansQuestionNeverTakesTheRecapsY(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("E", "1", "enter")
	h.toRecap()
	h.run(plan)
	v := h.view()
	mustContain(t, v, "Send 2 answers? y/N")
	mustNotContain(t, v, "Deploy example/talkable#4545's head")
	if q := h.m.bar.question(); !strings.HasPrefix(q, "Send 2 answers") {
		t.Fatalf("the bar asks %q on the recap, which draws Send 2 answers", q)
	}
	h.press("y")
	if len(h.act.answers) != 1 || len(h.act.stagingDeploys) != 0 {
		t.Fatalf("y on the recap sent %d answers and deployed %v, want the answers alone", len(h.act.answers), h.act.stagingDeploys)
	}
	if h.m.view != fvHome {
		h.press("esc")
	}
	want := stagingAsk("example/talkable#4545", []string{"void"}) + " y/N"
	mustContain(t, h.view(), want)
	h.press("y")
	h.wantStaging([]string{"example/talkable#4545 void"}, []string{"example/talkable#4545 void at abcdef1234 seen db/migrate/1_a.rb"})
}

// P's plan waits the same way: y on the recap sends the answers and merges
// nothing; on home the merge's question is up, and y merges.
func TestAMergePlansQuestionNeverTakesTheRecapsY(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("P")
	h.toRecap()
	h.run(plan)
	v := h.view()
	mustContain(t, v, "Send 2 answers? y/N")
	mustNotContain(t, v, "Merge the parent of example/talkable#4545")
	h.press("y")
	h.act.mu.Lock()
	answers, merges := len(h.act.answers), slices.Clone(h.act.merges)
	h.act.mu.Unlock()
	if answers != 1 || len(merges) != 0 {
		t.Fatalf("y on the recap sent %d answers and merged %v, want the answers alone", answers, merges)
	}
	if h.m.view != fvHome {
		h.press("esc")
	}
	mustContain(t, h.view(), mergePlanAsk("example/talkable#4545")+" y/N")
	h.press("y")
	h.act.mu.Lock()
	merges = slices.Clone(h.act.merges)
	h.act.mu.Unlock()
	if !slices.Equal(merges, []string{"example/talkable#4545"}) {
		t.Errorf("merges %v, want the row's PR", merges)
	}
}

// A plan that answers on the help, the log, the question card, the ready
// card or the new task's form asks nothing there (no cut question, no
// "It frees" of the ready card's push after it, no key typed into the form
// that answers it): the footer says the question waits, and back on home
// it is up whole, every migration and the warning, before y.
func TestAPlansQuestionWaitsForAViewThatShowsItWhole(t *testing.T) {
	for _, c := range []struct {
		name string
		to   func(h *factoryHarness)
		back []string
	}{
		{"help", func(h *factoryHarness) { h.press("?") }, []string{"esc"}},
		{"log", func(h *factoryHarness) { h.press(logKey) }, []string{"esc"}},
		{"question card", func(h *factoryHarness) { h.press("enter") }, []string{"esc"}},
		{"ready card", func(h *factoryHarness) { h.selectRow("PR-12360"); h.press("enter") }, []string{"esc"}},
		{"new task", func(h *factoryHarness) { h.press("n") }, []string{"esc"}},
	} {
		h := testPlanned(t, 140, 40)
		h.act.stagingAsk = planQuestion
		plan := h.startHeld("E", "1", "enter")
		c.to(h)
		view := h.m.view
		h.run(plan)
		v := h.view()
		mustNotContain(t, v, "Deploy talkable#5's head", "It frees")
		if q := h.m.bar.question(); q != "" {
			t.Errorf("%s: the bar asks %q there", c.name, q)
		}
		mustContain(t, v, "comes up on home or the task card")
		if c.name == "new task" { // a y typed there is text, never the deploy's answer
			h.press("y")
			h.wantStaging([]string{"example/talkable#4545 void"}, nil)
		}
		if h.m.view != view {
			t.Fatalf("%s: the plan's answer left the view for %v", c.name, h.m.view)
		}
		h.press(c.back...)
		if h.m.view != fvHome {
			t.Fatalf("%s: %v did not go home but to %v", c.name, c.back, h.m.view)
		}
		if got := askedOn(t, h.view(), 140); got != planQuestion+" y/N" {
			t.Errorf("%s, then home: asks\n%q\nwant\n%q\n%s", c.name, got, planQuestion+" y/N", h.view())
		}
		h.press("y")
		h.wantStaging([]string{"example/talkable#4545 void"}, []string{"example/talkable#4545 void at abcdef1234 seen db/migrate/1_a.rb"})
	}
}

// On home, a plan that answers while the filter is typed waits too: a y
// typed then is the filter's text; once enter keeps the filter, the
// question is up.
func TestAPlansQuestionWaitsWhileTheFilterIsTyped(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("E", "1", "enter")
	h.press("/")
	h.run(plan)
	h.press("y")
	h.wantStaging([]string{"example/talkable#4545 void"}, nil)
	if got := h.m.filter.Value(); got != "y" {
		t.Errorf("the filter holds %q, want the y typed", got)
	}
	mustNotContain(t, h.view(), "Deploy example/talkable#4545's head")
	h.press("backspace", "enter")
	mustContain(t, h.view(), stagingAsk("example/talkable#4545", []string{"void"})+" y/N")
}

// The ready card's "It frees <copy>." follows its own decision's question
// alone, never another's on its footer.
func TestTheReadyCardFreesOnlyItsOwnPushsQuestion(t *testing.T) {
	c := fixtureReadyCard()
	c.Cursor, c.Ask, c.Frees = readyPush, "Deploy talkable#5's head 398a935 to void?", "talkable.factory3"
	c.askOther = true
	if foot := readyFooter(drawReady(c)(140, 0, true, IconsUnicode)); strings.Contains(foot, "It frees") {
		t.Errorf("another question's footer says what the push frees: %q", foot)
	}
	c.askOther = false
	if foot := readyFooter(drawReady(c)(140, 0, true, IconsUnicode)); !strings.Contains(foot, "It frees talkable.factory3") {
		t.Errorf("the push's question lost what it frees: %q", foot)
	}
}
