package tui

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// liveLogPath is the live.log of the fixture's task PR-12388 (row 6).
const liveLogPath = "/Users/alice/.local/share/magnum/factory/tasks/6/live.log"

// testLive is the screen over the fixture with PR-12388 a task whose
// check runs: jest, begun 3 minutes before the screen's clock, its card
// naming live.log, which o follows; the follow swapped for one that
// records what it opens.
func testLive(t *testing.T) (*factoryHarness, *[]string) {
	t.Helper()
	h := testFactory(t, 140, 40)
	began := factoryNow.Add(-3 * time.Minute)
	h.src.change(func(d *FactoryData) {
		for i := range d.Rows {
			if d.Rows[i].ID == 6 {
				d.Rows[i].WaitsOn = []WaitsOn{{Who: "precheck", What: "jest", Since: began}}
				d.Rows[i].Help = "precheck: jest · o follows live.log (ctrl-c, then q, comes back)"
			}
		}
		cards := d.Cards[6]
		cards.Task.WaitingFor, cards.Task.WaitingStep, cards.Task.WaitingSince = "precheck: jest", "precheck: jest", began
		cards.Task.Follow = liveLogPath
		d.Cards[6] = cards
	})
	h.press("ctrl+r")
	var followed []string
	h.m.follow = func(_ context.Context, path string) *exec.Cmd {
		followed = append(followed, path)
		return exec.Command("true")
	}
	return h, &followed
}

// The wait of a running check shows its step once with how long it has
// run ("precheck: jest · 3m", never "precheck: precheck: jest"), and the
// time moves with the screen's clock on every frame, with no new read of
// the factory: the step's start is in the data, the painter counts from
// it. The task card's WAITING FOR shows the same, and its OUTPUT line
// names the file o follows and how to come back.
func TestTheWaitLineShowsTheStepOnceWithItsRunningTime(t *testing.T) {
	h, _ := testLive(t)
	v := h.view()
	mustContain(t, v, "precheck: jest · 3m")
	mustNotContain(t, v, "precheck: precheck")
	reads := h.src.frames
	h.clock.add(2*time.Minute + 10*time.Second)
	mustContain(t, h.view(), "precheck: jest · 5m")
	if h.src.frames != reads {
		t.Errorf("the time moved with %d reads of the factory, want none", h.src.frames-reads)
	}
	h.press("g")
	h.selectRow("PR-12388")
	h.press("enter")
	mustContain(t, h.view(), "precheck: jest · 5m", "OUTPUT", liveLogPath+" · o follows it (ctrl-c, then q, comes back)")
}

// o on a task whose check runs, while no agent of it works, follows its
// live.log in the pager (less +F), asking nothing, on home and on the task
// card, and steps into no pane; the hints and the menu say so; the screen
// comes back when the pager ends.
func TestOOnATaskWhoseCheckRunsFollowsLiveLogInThePager(t *testing.T) {
	h, followed := testLive(t)
	h.press("g")
	h.selectRow("PR-12388")
	mustContain(t, h.view(), "o follow live.log")
	if got := menuState(h.m.menuItems()); !got["follow its output"] {
		t.Errorf("the menu of PR-12388: %v, want follow its output", got)
	}
	h.press("o")
	h.press("enter")
	mustContain(t, h.view(), "OUTPUT")
	h.press("o")
	if !slices.Equal(*followed, []string{liveLogPath, liveLogPath}) || len(h.execs) != 2 {
		t.Fatalf("o followed %q with %d ExecProcess messages, want live.log twice", *followed, len(h.execs))
	}
	if h.m.bar.asking != nil || len(h.act.opens) != 0 {
		t.Errorf("o asked (%v) or stepped into a pane (%v)", h.m.bar.asking != nil, h.act.opens)
	}
	h.feed(factoryPagerMsg{task: 6})
	mustContain(t, h.view(), "STAGES")
}

// o on a task whose agent works still steps into its pane: the data names
// no file to follow then, and the screen asks the factory to open the pane.
func TestOOnATaskWhoseAgentWorksStillStepsIntoItsPane(t *testing.T) {
	h, followed := testLive(t)
	h.press("g")
	h.selectRow("PR-12371")
	mustContain(t, h.view(), "o pane finding the cause")
	h.press("o")
	if len(*followed) != 0 || !slices.Equal(h.act.opens, []int64{4}) {
		t.Fatalf("o followed %q and opened %v, want the pane of task 4", *followed, h.act.opens)
	}
}

// The follow is less +F on the file, with -+F after your LESS, whatever
// $PAGER is: following is less's.
func TestTheFollowIsLessPlusF(t *testing.T) {
	t.Setenv("PAGER", "most")
	t.Setenv("LESS", "iR")
	cmd := defaultFollow(t.Context(), liveLogPath)
	if !slices.Equal(cmd.Args, []string{"less", "+F", liveLogPath}) || !slices.Contains(cmd.Env, "LESS=iR -+F") {
		t.Errorf("args %q, LESS in %v", cmd.Args, strings.Join(cmd.Env, " "))
	}
}
