package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/zhuravel/magnum/internal/engine"
)

// A plan's question (E's staging deploy, P's merge of the parent) goes up
// at once only where its key was pressed: home with its row selected (the
// filter not typed), or its row's own card, the menu closed and no other
// question or chooser up. Anywhere else the plan is held, and never comes
// up by itself: no y/N appears that the operator did not ask for with a key
// on that row. A marker on the rule says it is held; it never takes the
// place of another action's outcome or failure. E (or P) on its row puts
// its question up, instead of a new chooser or plan; on another row they
// act as usual, and the held plan stays. It is dropped, with a note, past
// heldPlanFor (the engine's StagingDeployWait), when its row leaves home,
// or when its PR merges or closes. Before: on the recap, y on "Send 2
// answers? y/N" deployed (or merged the parent) and sent no answers; then
// a held question came up by itself, in the update that ended another
// action, on another task's card or hours later, and its note hid the
// failures of later actions.

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

// expireNote ends the footer's note, as its timer does.
func (h *factoryHarness) expireNote() {
	h.t.Helper()
	h.feed(factoryExpireMsg{seq: h.m.bar.flashSeq})
}

// merges are the PRs MergeParent merged the parent of.
func (a *fakeFactoryActions) mergesOf() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.merges)
}

const (
	heldE      = "plan of PR-12345 ready: E on its row asks"
	heldP      = "plan of PR-12345 ready: P on its row on home asks"
	stagedHeld = "example/talkable#4545 void at abcdef1234 seen db/migrate/1_a.rb"
)

// The plan's question waits the engine's own wait for a deploy's answer.
func TestAHeldPlanWaitsAsLongAsTheEnginesDeployWait(t *testing.T) {
	if heldPlanFor != engine.StagingDeployWait {
		t.Errorf("a held plan waits %v, the engine's deploy %v", heldPlanFor, engine.StagingDeployWait)
	}
}

// On the recap, y sends the answers it asks for, never the plan that
// answered meanwhile. The send's outcome stays on screen, and nothing asks
// after it: a second y deploys nothing. Back on home, the marker says the
// plan is held; E on its row puts its question up, and y deploys.
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
	if len(h.act.answers) != 1 {
		t.Fatalf("y on the recap sent %d answers, want 1", len(h.act.answers))
	}
	if q := h.m.bar.question(); q != "" {
		t.Fatalf("after the send, the screen asks %q by itself", q)
	}
	mustContain(t, h.view(), "PR-12345: 2 answers; it goes on")
	h.press("y")
	h.wantStaging([]string{"example/talkable#4545 void"}, nil)

	if h.m.view != fvHome {
		h.press("esc")
	}
	h.expireNote()
	mustContain(t, h.view(), heldE)
	h.press("g")
	h.selectRow("PR-12345")
	h.press("E")
	mustContain(t, h.view(), stagingAsk("example/talkable#4545", []string{"void"})+" y/N")
	h.press("y")
	h.wantStaging([]string{"example/talkable#4545 void"}, []string{stagedHeld})
}

// P's plan waits the same way: y on the recap sends the answers and merges
// nothing, nothing asks after it, and P on its row asks the merge.
func TestAMergePlansQuestionNeverTakesTheRecapsY(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("P")
	h.toRecap()
	h.run(plan)
	v := h.view()
	mustContain(t, v, "Send 2 answers? y/N")
	mustNotContain(t, v, "Merge the parent of example/talkable#4545")
	h.press("y", "y")
	if len(h.act.answers) != 1 || len(h.act.mergesOf()) != 0 {
		t.Fatalf("y y on the recap sent %d answers and merged %v, want the answers alone", len(h.act.answers), h.act.mergesOf())
	}
	if h.m.view != fvHome {
		h.press("esc")
	}
	h.expireNote()
	mustContain(t, h.view(), heldP)
	h.press("g")
	h.selectRow("PR-12345")
	h.press("P")
	mustContain(t, h.view(), mergePlanAsk("example/talkable#4545")+" y/N")
	h.press("y")
	if got := h.act.mergesOf(); !slices.Equal(got, []string{"example/talkable#4545"}) {
		t.Errorf("merges %v, want the row's PR", got)
	}
}

// A plan that answers on the help, the log, the question card, the ready
// card, the new task's form or home with another row selected asks
// nothing, there or after: the marker says it is held, a y does nothing
// with it, and back on home E on its row puts it up whole, every migration
// and the warning, before y.
func TestAPlansQuestionWaitsForItsRowsKey(t *testing.T) {
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
		{"another row", func(h *factoryHarness) { h.selectRow("PR-12360") }, nil},
	} {
		h := testPlanned(t, 140, 40)
		h.act.stagingAsk = planQuestion
		plan := h.startHeld("E", "1", "enter")
		c.to(h)
		view := h.m.view
		h.run(plan)
		v := h.view()
		mustNotContain(t, v, "Deploy talkable#5's head", "It frees")
		mustContain(t, v, heldE)
		if q := h.m.bar.question(); q != "" {
			t.Errorf("%s: the bar asks %q there", c.name, q)
		}
		if c.name == "new task" { // a y typed there is text, never the deploy's answer
			h.press("y")
		}
		if h.m.view != view {
			t.Fatalf("%s: the plan's answer left the view for %v", c.name, h.m.view)
		}
		h.press(c.back...)
		if h.m.view != fvHome {
			t.Fatalf("%s: %v did not go home but to %v", c.name, c.back, h.m.view)
		}
		if q := h.m.bar.question(); q != "" {
			t.Errorf("%s, then home: the screen asks %q by itself", c.name, q)
		}
		h.press("y")
		h.wantStaging([]string{"example/talkable#4545 void"}, nil)
		h.press("g")
		h.selectRow("PR-12345")
		h.press("E")
		if got := askedOn(t, h.view(), 140); got != planQuestion+" y/N" {
			t.Errorf("%s, then E on its row: asks\n%q\nwant\n%q\n%s", c.name, got, planQuestion+" y/N", h.view())
		}
		h.press("y")
		h.wantStaging([]string{"example/talkable#4545 void"}, []string{stagedHeld})
	}
}

// On home, a plan that answers while the filter is typed is held: a y typed
// then is the filter's text, and keeping the filter asks nothing; E on its
// row does.
func TestAPlansQuestionWaitsWhileTheFilterIsTyped(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("E", "1", "enter")
	h.press("/")
	h.run(plan)
	h.press("y")
	if got := h.m.filter.Value(); got != "y" {
		t.Errorf("the filter holds %q, want the y typed", got)
	}
	h.press("backspace", "enter")
	if q := h.m.bar.question(); q != "" {
		t.Fatalf("keeping the filter asks %q", q)
	}
	h.selectRow("PR-12345")
	h.press("E")
	mustContain(t, h.view(), stagingAsk("example/talkable#4545", []string{"void"})+" y/N")
	h.wantStaging([]string{"example/talkable#4545 void"}, nil)
}

// A plan held on the help, with another task's card under it, does not come
// up on that card: the card shows the marker, and asks nothing.
func TestAHeldPlanNeverComesUpOnAnotherTasksCard(t *testing.T) {
	h := testStaged(t, 140, 40)
	h.configEnvs("PR-12324", "void")
	h.press("g")
	h.selectRow("PR-12324")
	h.press("E", "1")
	next, plan := h.m.Update(keyMsg("enter"))
	h.m = next.(factoryModel)
	h.open("PR-12320")
	h.press("?")
	h.run(plan)
	h.press("esc")
	if h.m.view != fvTask || h.m.task != 1 {
		t.Fatalf("view %v of %d, want PR-12320's card", h.m.view, h.m.task)
	}
	v := h.view()
	mustNotContain(t, v, "example/talkable#4534's head")
	mustContain(t, v, "plan of PR-12324's #4534 ready: E on its row asks")
	h.press("y")
	h.wantStaging([]string{"example/talkable#4534 void"}, nil)
}

// E and P on another row act as usual, and the held plan stays: E opens
// that row's chooser, P plans that row's merge; the first row's E then
// asks its held question.
func TestEAndPOnAnotherRowLeaveAHeldPlan(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "1")
	next, plan := h.m.Update(keyMsg("enter"))
	h.m = next.(factoryModel)
	h.press("j")
	h.run(plan)
	h.press("g")
	h.selectRow("PR-12324")
	h.press("P") // another row's plan: its question goes up at once, there
	mustContain(t, h.view(), mergePlanAsk("example/talkable#4534")+" y/N")
	h.press("n")
	h.configEnvs("PR-12324", "void")
	h.press("g")
	h.selectRow("PR-12324")
	h.press("E")
	if h.m.bar.choose == nil || h.m.bar.choose.ref != "example/talkable#4534" {
		t.Fatalf("E on another row opened no chooser of its own:\n%s", h.view())
	}
	h.press("x")
	if len(h.m.bar.held) != 1 {
		t.Fatalf("the held plan is gone: %+v", h.m.bar.held)
	}
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E")
	mustContain(t, h.view(), stagingAsk("example/talkable#4530", []string{"void"})+" y/N")
	if plans, _ := h.act.stagingCalls(); len(plans) != 1 {
		t.Errorf("E on the held plan's row planned again: %v", plans)
	}
}

// A held plan is dropped, with a note, once it is heldPlanFor old, once its
// row leaves home, or once its PR merges; nothing runs, and E or P on its
// row plans again.
func TestAHeldPlanIsDroppedWhenItCannotHold(t *testing.T) {
	for _, c := range []struct {
		name, key, why string
		drop           func(h *factoryHarness)
	}{
		{"age", "E", "the plan waited 10 min", func(h *factoryHarness) {
			h.clock.at = h.clock.at.Add(heldPlanFor)
			h.press("j")
		}},
		{"age", "P", "the plan waited 10 min", func(h *factoryHarness) {
			h.clock.at = h.clock.at.Add(heldPlanFor + time.Minute)
			h.press("j")
		}},
		{"row gone", "E", "its row left home", func(h *factoryHarness) {
			h.src.change(func(d *FactoryData) {
				d.Rows = slices.DeleteFunc(d.Rows, func(r FactoryRow) bool { return r.Key == "PR-12345" })
			})
			h.press("ctrl+r")
		}},
		{"merged", "P", "its PR is merged", func(h *factoryHarness) { h.facts("PR-12345", FactoryPRFacts{GHState: "MERGED"}) }},
		{"closed", "P", "its PR is closed", func(h *factoryHarness) { h.facts("PR-12345", FactoryPRFacts{GHState: "CLOSED"}) }},
		{"closed", "E", "its PR is not open", func(h *factoryHarness) {
			h.src.change(func(d *FactoryData) {
				for i, r := range d.Rows {
					if r.Key == "PR-12345" {
						d.Rows[i].PR = nil
					}
				}
			})
			h.press("ctrl+r")
		}},
	} {
		h := testPlanned(t, 140, 40)
		var plan tea.Cmd
		if c.key == "E" {
			plan = h.startHeld("E", "1", "enter")
		} else {
			plan = h.startHeld("P")
		}
		h.press("?")
		h.run(plan)
		h.press("esc")
		if len(h.m.bar.held) != 1 || h.m.view != fvHome {
			t.Fatalf("%s %s: no plan held, or not home (%v)", c.name, c.key, h.m.view)
		}
		c.drop(h)
		if len(h.m.bar.held) != 0 {
			t.Errorf("%s %s: the plan is still held", c.name, c.key)
		}
		mustContain(t, h.view(), c.why, "nothing was run")
		mustNotContain(t, h.view(), "ready: "+c.key+" on its row asks")
		if q := h.m.bar.question(); q != "" {
			t.Errorf("%s %s: home asks %q", c.name, c.key, q)
		}
		if _, deploys := h.act.stagingCalls(); len(deploys) != 0 || len(h.act.mergesOf()) != 0 {
			t.Errorf("%s %s: the drop ran %v, %v", c.name, c.key, deploys, h.act.mergesOf())
		}
	}
}

// While a plan is held, every action's outcome and failure shows as it
// does without one: a push the ready card's y sends that fails keeps its
// failure in the footer, and so does a send of answers on the recap; the
// held plan's marker comes back once the footer is free.
func TestAHeldPlanNeverHidesAnotherActionsOutcome(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("E", "1", "enter")
	h.selectRow("PR-12360")
	h.press("enter")
	if h.m.view != fvReady {
		t.Fatalf("view %v, want the ready card", h.m.view)
	}
	h.run(plan)
	h.act.decideErr = errors.New("the push was rejected")
	h.press("1", "y")
	mustContain(t, h.view(), "the push was rejected")
	mustNotContain(t, h.view(), heldE)

	r := testPlanned(t, 140, 40)
	plan = r.startHeld("P")
	r.toRecap()
	r.run(plan)
	r.act.answerErr = errors.New("the factory is not running")
	r.press("y")
	v := r.view()
	mustContain(t, v, "the factory is not running")
	mustNotContain(t, v, heldP)

	o := testPlanned(t, 140, 40)
	plan = o.startHeld("E", "1", "enter")
	o.press("?")
	o.run(plan)
	o.press("esc")
	o.selectRow("PR-12360")
	o.press("o") // steps into the pane: its own outcome, not the plan's again
	mustContain(t, o.view(), "focused its pane")
	mustNotContain(t, o.view(), heldE)
	o.expireNote()
	mustContain(t, o.view(), heldE)
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

// failSource makes the screen's next reads of the factory fail with err
// (nil: they work again).
func (h *factoryHarness) failSource(err error) {
	h.t.Helper()
	h.src.mu.Lock()
	h.src.err = err
	h.src.mu.Unlock()
}

// A plan's question goes up at once only on the view and the row where its
// key was pressed: E on home's row whose plan answers on that row's card,
// E on the card whose plan answers back on home, and P on home whose plan
// answers on the card are held, and E (P on home) on the row asks them.
// Before, home on the row and the row's card were one place.
func TestAPlansQuestionGoesUpAtOnceOnlyOnTheViewWhereItsKeyWasPressed(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "1")
	next, plan := h.m.Update(keyMsg("enter"))
	h.m = next.(factoryModel)
	h.press("enter") // the row's card
	h.run(plan)
	if h.m.view != fvTask || h.m.bar.question() != "" {
		t.Fatalf("E on home, answered on the card: view %v asks %q, want it held", h.m.view, h.m.bar.question())
	}
	mustContain(t, h.view(), "plan of PR-12320's #4530 ready: E on its row asks")
	h.press("E")
	mustContain(t, h.view(), stagingAsk("example/talkable#4530", []string{"void"})+" y/N")
	h.press("n")

	c := testStaged(t, 160, 40)
	c.open("PR-12320")
	c.press("E", "1")
	next, plan = c.m.Update(keyMsg("enter"))
	c.m = next.(factoryModel)
	c.press("esc") // home, the row under the cursor
	c.run(plan)
	if r, _ := c.m.selectedRow(); c.m.view != fvHome || r.Key != "PR-12320" || c.m.bar.question() != "" {
		t.Fatalf("E on the card, answered on home: view %v on %s asks %q, want it held", c.m.view, r.Key, c.m.bar.question())
	}
	c.press("E")
	mustContain(t, c.view(), stagingAsk("example/talkable#4530", []string{"void"})+" y/N")
	c.press("y")
	c.wantStaging([]string{"example/talkable#4530 void"}, []string{stagedDeploy})

	p := testStaged(t, 160, 40)
	p.press("g")
	p.selectRow("PR-12320")
	next, plan = p.m.Update(keyMsg("P"))
	p.m = next.(factoryModel)
	p.press("enter")
	p.run(plan)
	if p.m.bar.question() != "" {
		t.Fatalf("P on home, answered on the card, asks %q there", p.m.bar.question())
	}
	mustContain(t, p.view(), "plan of PR-12320's #4530 ready: P on its row on home asks")
	p.press("P") // no key of the card: it stays held
	if p.m.bar.question() != "" || len(p.m.bar.held) != 1 {
		t.Fatalf("P on the card asks %q, held %d", p.m.bar.question(), len(p.m.bar.held))
	}
	p.press("esc", "P")
	mustContain(t, p.view(), mergePlanAsk("example/talkable#4530"))
}

// Home's refresh failure wins over the held plan's marker on its rule, and
// while the refresh fails no held plan is dropped on the stale rows; once a
// refresh works the marker is back, and a row it no longer has drops its
// plan.
func TestARefreshFailureWinsOverTheHeldMarker(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("E", "1", "enter")
	h.press("?")
	h.run(plan)
	h.press("esc")
	h.expireNote()
	mustContain(t, h.view(), heldE)
	h.failSource(errors.New("the registry is locked"))
	h.press("ctrl+r")
	h.expireNote()
	v := h.view()
	mustContain(t, v, "refresh failed: the registry is locked")
	mustNotContain(t, v, heldE)
	if len(h.m.bar.held) != 1 {
		t.Fatalf("a failed refresh dropped the held plan")
	}
	h.failSource(nil)
	h.press("ctrl+r")
	mustContain(t, h.view(), heldE)
	h.src.change(func(d *FactoryData) {
		d.Rows = slices.DeleteFunc(d.Rows, func(r FactoryRow) bool { return r.Key == "PR-12345" })
	})
	h.press("ctrl+r")
	if len(h.m.bar.held) != 0 {
		t.Errorf("a refresh without the row kept its plan")
	}
}

// A plan that answers away from its row keeps a failure the footer holds
// (the pager's): the failure stays until a key, and the marker comes after.
func TestAHeldPlanKeepsAFailureInTheFooter(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("E", "1", "enter")
	h.press("?")
	h.feed(factoryPagerMsg{err: errors.New("less: not found")})
	h.run(plan)
	v := h.view()
	mustContain(t, v, "less: not found")
	mustNotContain(t, v, heldE)
	h.press("j")
	mustContain(t, h.view(), heldE)
}

// E and P on a held plan's row run the refusal a fresh press runs: a held
// P on a PR a fix loop now works on, and a held E on a PR whose repository
// no longer has staging envs, are dropped with the refusal's words, and
// nothing runs.
func TestRaisingAHeldPlanRunsTheKeysRefusal(t *testing.T) {
	h := testPlanned(t, 140, 40)
	plan := h.startHeld("P")
	h.press("?")
	h.run(plan)
	h.press("esc")
	h.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "PR-12345" {
				pr := *r.PR
				pr.Fixing = true
				d.Rows[i].PR = &pr
			}
		}
	})
	h.press("ctrl+r", "g")
	h.selectRow("PR-12345")
	h.press("P")
	v := h.view()
	mustContain(t, v, "Merge the parent of PR-12345: dropped", "nothing was run")
	mustNotContain(t, v, "y/N")
	h.press("y")
	if len(h.act.mergesOf()) != 0 || len(h.m.bar.held) != 0 {
		t.Errorf("merges %v, held %d", h.act.mergesOf(), len(h.m.bar.held))
	}

	e := testPlanned(t, 220, 40) // the refusal's words are long: the whole note fits
	plan = e.startHeld("E", "1", "enter")
	e.press("?")
	e.run(plan)
	e.press("esc")
	e.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "PR-12345" {
				d.Rows[i].Stagings = nil
			}
		}
	})
	e.press("ctrl+r", "g")
	e.selectRow("PR-12345")
	e.press("E")
	v = e.view()
	mustContain(t, v, "Deploy PR-12345 to staging: dropped", "has no staging envs", "nothing was run")
	e.press("y")
	e.wantStaging([]string{"example/talkable#4545 void"}, nil)
}

// The 10 minutes run until y: a plan's question that is up heldPlanFor
// after its plan answered is taken down with a note, and a y after that
// sends nothing, for E and for P; one raised by E on its row at 9 min 59
// s is taken down a second later.
func TestAPlansQuestionIsTakenDownOnceItsPlanIsTenMinutesOld(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "1", "enter")
	mustContain(t, h.view(), stagingAsk("example/talkable#4530", []string{"void"})+" y/N")
	h.clock.at = h.clock.at.Add(heldPlanFor)
	h.feed(factoryTickMsg{})
	if q := h.m.bar.question(); q != "" {
		t.Fatalf("the question is still up 10 min after its plan: %q", q)
	}
	mustContain(t, h.view(), "the plan waited 10 min", "nothing was run")
	h.press("y")
	h.wantStaging([]string{"example/talkable#4530 void"}, nil)

	p := testStaged(t, 160, 40)
	p.press("g")
	p.selectRow("PR-12320")
	p.press("P")
	p.clock.at = p.clock.at.Add(heldPlanFor + time.Minute)
	p.press("y") // the age is checked before the key: the y finds no question
	if got := p.act.mergesOf(); len(got) != 0 {
		t.Errorf("a y 11 min after the plan merged %v", got)
	}

	r := testStaged(t, 160, 40)
	r.press("g")
	r.selectRow("PR-12320")
	r.press("E", "1")
	next, plan := r.m.Update(keyMsg("enter"))
	r.m = next.(factoryModel)
	r.press("?")
	r.run(plan)
	r.press("esc")
	r.clock.at = r.clock.at.Add(heldPlanFor - time.Second)
	r.press("g")
	r.selectRow("PR-12320")
	r.press("E")
	if r.m.bar.question() == "" {
		t.Fatal("E at 9 min 59 s raised nothing")
	}
	r.clock.at = r.clock.at.Add(time.Second)
	r.press("y")
	r.wantStaging([]string{"example/talkable#4530 void"}, nil)
}

// P's question wraps over home's footer as E's does: at 80 cells all its
// words show, "with no conflict" among them, before y.
func TestPsQuestionShowsWholeOnHome(t *testing.T) {
	h := testStaged(t, 80, 30)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("P")
	v := h.view()
	asked := ""
	for l := range strings.SplitSeq(v, "\n") {
		if asked == "" && !strings.Contains(l, "Merge the parent of") {
			continue
		}
		l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "─"))
		l, _, _ = strings.Cut(l, "  y confirms")
		if asked != "" && !strings.HasSuffix(asked, "-") {
			asked += " "
		}
		asked += l
		if strings.HasSuffix(l, "y/N") {
			break
		}
	}
	if want := mergePlanAsk("example/talkable#4530") + " y/N"; asked != want {
		t.Errorf("home asks\n%q\nwant\n%q\n%s", asked, want, v)
	}
}
