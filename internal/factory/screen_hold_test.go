package factory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/tui"
)

// holds writes the holds the factory daemon last saved (holds.json), which
// the data reads.
func (r *screenRig) holds(hs ...Hold) {
	r.t.Helper()
	b, err := json.Marshal(append([]Hold{}, hs...))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.layout.FactoryHolds()), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.layout.FactoryHolds(), b, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// heldSince is when the holds of these tests began: 09:00, the same day as
// the screen's clock.
var heldSince = screenNow.Add(-5 * time.Minute)

// mysqlHold is the infrastructure hold the 2026-11-13 outage began, which
// stops every stage.
func mysqlHold() Hold {
	return Hold{Kind: HoldInfra, Reason: "MySQL does not answer", Since: heldSince}
}

// A hold that stops every stage is the wait of each active task that names
// none of its own: the row (WAITS ON, short form without the time), its
// task card (WAITING FOR) and the help line say "held: MySQL does not answer
// (since 09:00)". Before, such a task at its local review showed "magnum:
// local review", as if its step ran, while the daemon let no stage advance.
func TestTheScreenNamesTheDaemonsHoldOnATaskWithNoWait(t *testing.T) {
	r := newScreenRig(t)
	r.holds(mysqlHold())
	r.task("PR-12393", "bug", "At its local review", registry.TaskActive, "local_review", "", "")
	r.task("PR-12394", "bug", "At its coding", registry.TaskActive, "coding", "", "")

	d := r.data(screenNow)
	for _, key := range []string{"PR-12393", "PR-12394"} {
		row := rowOf(t, d, key)
		want := tui.WaitsOn{Who: "held", What: "MySQL does not answer (since 09:00)", Short: "MySQL does not answer"}
		if len(row.WaitsOn) != 1 || row.WaitsOn[0] != want || row.Mark != tui.MarkWaits {
			t.Errorf("%s: waits on %+v, mark %v, want [%+v] and waiting", key, row.WaitsOn, row.Mark, want)
		}
		if got := d.Cards[row.ID].Task.WaitingFor; got != "held: MySQL does not answer (since 09:00)" {
			t.Errorf("%s: the card waits for %q, want the hold", key, got)
		}
		if row.Help != "held: MySQL does not answer (since 09:00)" {
			t.Errorf("%s: the help line %q, want the hold", key, row.Help)
		}
	}

	// The herdr hold stops every stage too; a hold of another day says it.
	r.holds(Hold{Kind: HoldHerdr, Reason: "the factory session does not answer", Since: screenNow.Add(-26 * time.Hour)})
	d = r.data(screenNow)
	if got := rowOf(t, d, "PR-12393").WaitsOn[0].What; got != "the factory session does not answer (since Nov 3 07:05)" {
		t.Errorf("a herdr hold of yesterday: %q", got)
	}
}

// The holds that stop launches or one repository's copies, not every stage,
// are no task's wait: the disk and the memory, a repository's infrastructure
// hold and the pause name themselves where a stage meets them (a launch's
// own wait), and with the daemon down no hold is in force. The drains are a
// task's wait (TestTheScreenNamesTheDrainsOnATaskWithNoWait).
func TestOnlyAHoldThatStopsEveryStageOrADrainIsATasksWait(t *testing.T) {
	r := newScreenRig(t)
	r.task("PR-12393", "bug", "At its local review", registry.TaskActive, "local_review", "", "")
	for _, h := range []Hold{{Kind: HoldDisk, Reason: "3.0 GB free"}, {Kind: HoldMemory, Reason: "swap 91% used"},
		{Kind: HoldInfra, Repo: "example/talkable", Reason: "every copy broke"}, {Kind: HoldPause, Reason: "paused by you"}} {
		h.Since = heldSince
		r.holds(h)
		if w := rowOf(t, r.data(screenNow), "PR-12393").WaitsOn; len(w) != 1 || w[0].Who != "magnum" {
			t.Errorf("under the %s hold: waits on %+v, want its stage's step", h.Kind, w)
		}
	}

	r.holds(mysqlHold())
	d, err := ScreenData(r.ctx, r.reg, r.agents, screenNow, screenNow, ScreenOptions{Config: r.cfg, Layout: r.layout, DaemonDown: true})
	if err != nil {
		t.Fatal(err)
	}
	if w := rowOf(t, d, "PR-12393").WaitsOn; len(w) != 1 || w[0].Who != "magnum" {
		t.Errorf("with the daemon down: waits on %+v, want no hold of the last daemon's", w)
	}
}

// The drains (`magnum factory install --drain`'s and the one for a reboot)
// record no wait in the tasks they hold back: a launch, a claim, a round or
// a check the drain stops sets none. So a task that names no wait drew its
// stage's step ("magnum: local review") as if it would run, though nothing
// new starts, and a reboot's drain can last long. Its row, its task card and
// the help line name the drain now, as they name a hold that stops every
// stage, and o says that nothing runs and why. A task's own wait still wins,
// an agent at work stays first, and a hold that stops every stage comes
// before a drain.
func TestTheScreenNamesTheDrainsOnATaskWithNoWait(t *testing.T) {
	r := newScreenRig(t)
	stage := r.task("PR-12393", "bug", "At its local review", registry.TaskActive, "local_review", "", "")
	r.task("PR-12390", "bug", "Waits for a copy", registry.TaskActive, "coding", waitCopy, "preparing factory3")
	working := r.task("PR-12395", "bug", "Writing", registry.TaskActive, "coding", "", "")
	r.session(working, "fx-writer", "working", screenNow.Add(-8*time.Minute), screenNow.Add(-20*time.Second))
	// The drains' reasons carry their start, as restart.go and control.go
	// write them, so the wait adds no "(since …)" of its own.
	drain := Hold{Kind: HoldDrain, Reason: "`magnum factory install --drain` (pid 4300, since 09:00:00): nothing new starts until the restart", Since: heldSince}
	reboot := Hold{Kind: HoldReboot, Reason: "draining for a reboot since Nov 3 09:00: nothing new starts; `magnum resume` ends it", Since: heldSince}
	for _, h := range []Hold{drain, reboot} {
		r.holds(h)
		d := r.data(screenNow)
		words := h.Reason
		row := rowOf(t, d, "PR-12393")
		want := tui.WaitsOn{Who: "held", What: words, Short: h.Reason}
		if len(row.WaitsOn) != 1 || row.WaitsOn[0] != want || row.Mark != tui.MarkWaits {
			t.Errorf("%s: waits on %+v, mark %v, want [%+v] and waiting", h.Kind, row.WaitsOn, row.Mark, want)
		}
		if got := d.Cards[stage.ID].Task.WaitingFor; got != "held: "+words {
			t.Errorf("%s: the card waits for %q, want the drain", h.Kind, got)
		}
		if row.Help != "held: "+words {
			t.Errorf("%s: the help line %q, want the drain", h.Kind, row.Help)
		}
		if got, want := d.Cards[stage.ID].Task.OpensNothing, "nothing runs: it waits on held: "+words; got != want {
			t.Errorf("%s: o says %q, want %q", h.Kind, got, want)
		}
		if w := rowOf(t, d, "PR-12390").WaitsOn; len(w) == 0 || w[0].Who != waitCopy || slices.ContainsFunc(w, func(w tui.WaitsOn) bool { return w.Who == "held" }) {
			t.Errorf("%s: the task that waits for a copy waits on %+v, want its own wait alone", h.Kind, w)
		}
		if row := rowOf(t, d, "PR-12395"); len(row.WaitsOn) != 2 || row.WaitsOn[0].Who != "agent" || row.WaitsOn[1] != want ||
			row.Mark != tui.MarkWorking {
			t.Errorf("%s: the task with an agent at work waits on %+v, mark %v, want the agent first, the drain after it", h.Kind, row.WaitsOn, row.Mark)
		}
	}

	r.holds(drain, mysqlHold()) // a hold that stops every stage says more than a drain
	if w := rowOf(t, r.data(screenNow), "PR-12393").WaitsOn; len(w) != 1 || w[0].What != "MySQL does not answer (since 09:00)" {
		t.Errorf("under a drain and the infra hold: waits on %+v, want the infra hold", w)
	}
}

// A task's own wait is more specific than the daemon's hold, so it stays: a
// wait for a copy, a check that runs, a question on you, a published task's
// PR; and an agent at work stays first, the hold after it, so the row still
// says the task works.
func TestATasksOwnWaitWinsOverTheDaemonsHold(t *testing.T) {
	r := newScreenRig(t)
	r.holds(mysqlHold())
	r.task("PR-12390", "bug", "Waits for a copy", registry.TaskActive, "coding", waitCopy, "preparing factory3")
	r.task("PR-12391", "bug", "Its check runs", registry.TaskActive, "coding", waitPrecheck, "precheck: jest")
	ask := r.task("PR-12392", "bug", "Questions", registry.TaskActive, "questions", "", "")
	r.questions(ask, question("When?", "Seven days"))
	r.move(ask, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	working := r.task("PR-12395", "bug", "Writing", registry.TaskActive, "coding", "", "")
	r.session(working, "fx-writer", "working", screenNow.Add(-8*time.Minute), screenNow.Add(-20*time.Second))
	pub := r.task("PR-12396", "bug", "Published", registry.TaskActive, "publish", "", "")
	r.published(pub, 4519)

	d := r.data(screenNow)
	for key, who := range map[string]string{"PR-12390": waitCopy, "PR-12391": waitPrecheck, "PR-12392": waitsYou, "PR-12396": "magnum"} {
		row := rowOf(t, d, key)
		if len(row.WaitsOn) == 0 || row.WaitsOn[0].Who != who {
			t.Errorf("%s: waits on %+v, want its own wait on %s first", key, row.WaitsOn, who)
		}
		for _, w := range row.WaitsOn {
			if w.Who == "held" {
				t.Errorf("%s names the hold beside its own wait: %+v", key, row.WaitsOn)
			}
		}
	}
	row := rowOf(t, d, "PR-12395")
	if len(row.WaitsOn) != 2 || row.WaitsOn[0].Who != "agent" || row.WaitsOn[1].Who != "held" || row.Mark != tui.MarkWorking {
		t.Errorf("the task with an agent at work: waits on %+v, mark %v, want the agent first, the hold after it, and working", row.WaitsOn, row.Mark)
	}
	if got := d.Cards[row.ID].Task.WaitingFor; !strings.HasPrefix(got, "agent: ") || !strings.HasSuffix(got, " · held: MySQL does not answer (since 09:00)") {
		t.Errorf("the card of the task with an agent at work waits for %q", got)
	}
}

// The task card says why o opens nothing: no agent's pane is open, the
// factory runs no output to follow, and either a hold stops every stage (it
// names the hold) or the task waits on something else (it names that wait, or
// its stage). A task whose agent is live, or whose check's log follows, says
// nothing: o opens it.
func TestTheTaskCardSaysWhyOOpensNothing(t *testing.T) {
	r := newScreenRig(t)
	stage := r.task("PR-12393", "bug", "At its local review", registry.TaskActive, "local_review", "", "")
	ask := r.task("PR-12392", "bug", "Questions", registry.TaskActive, "questions", "", "")
	ask = r.questions(ask, question("When?", "Seven days"))
	r.move(ask, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	working := r.task("PR-12395", "bug", "Writing", registry.TaskActive, "coding", "", "")
	r.session(working, "fx-writer", "working", screenNow.Add(-8*time.Minute), screenNow.Add(-20*time.Second))
	idle := r.task("PR-12397", "bug", "Idle agent", registry.TaskActive, "coding", "", "")
	r.session(idle, "fx-writer", "idle", screenNow.Add(-8*time.Minute), screenNow.Add(-6*time.Minute))
	check := r.task("PR-12398", "bug", "Its check runs", registry.TaskActive, "coding", waitPrecheck, "precheck: jest")
	r.file(check.ID, liveFile, "")

	opens := func(d tui.FactoryData, id int64) string { return d.Cards[id].Task.OpensNothing }
	d := r.data(screenNow)
	if got, want := opens(d, stage.ID), "nothing runs: it waits on magnum: local review"; got != want {
		t.Errorf("a task at its local review: %q, want %q", got, want)
	}
	if got, want := opens(d, ask.ID), "no pane to open: it waits on you: 1 question before coding"; got != want {
		t.Errorf("a task that waits on you: %q, want %q", got, want)
	}
	for name, id := range map[string]int64{"agent at work": working.ID, "idle live agent": idle.ID, "check whose log follows": check.ID} {
		if got := opens(d, id); got != "" {
			t.Errorf("a task with an %s: %q, want none (o opens something)", name, got)
		}
	}

	r.holds(mysqlHold())
	d = r.data(screenNow)
	if got, want := opens(d, stage.ID), "nothing runs: the factory holds every stage (MySQL does not answer since 09:00)"; got != want {
		t.Errorf("under the hold: %q, want %q", got, want)
	}
	if got, want := opens(d, ask.ID), "no pane to open: it waits on you: 1 question before coding"; got != want {
		t.Errorf("a task that waits on you, under the hold: %q, want its own wait", got)
	}
	if got := opens(d, working.ID); got != "" {
		t.Errorf("a task with an agent at work, under the hold: %q, want none", got)
	}
}

// o said "nothing runs" on every task it opened nothing on, though a round
// of the local review runs in magnum's review session (no factory pane, no
// follow file), a copy's claim runs before its log exists, and magnum's
// review of a published task's PR runs in magnum's daemon; before, it said
// the task had no pane open, which was true. "nothing runs" is said only
// where it is so: a hold of the daemon's stops the task, the task waits on
// its stage's own step (magnum's, which names no wait of the task's), or the
// task ended. Otherwise o says there is no pane to open and what the task
// waits on.
func TestOSaysNothingRunsOnlyWhereNothingDoes(t *testing.T) {
	r := newScreenRig(t)
	round := r.task("PR-12380", "bug", "Its round runs", registry.TaskActive, "local_review", waitReview, "round 2 of the local review runs")
	claim := r.task("PR-12381", "bug", "Its copy is claimed", registry.TaskActive, "coding", waitCopy, "preparing factory3")
	ask := r.task("PR-12382", "bug", "Questions", registry.TaskActive, "questions", "", "")
	ask = r.questions(ask, question("When?", "Seven days"))
	r.move(ask, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	pub := r.task("PR-12383", "bug", "Published", registry.TaskActive, "publish", "", "")
	r.published(pub, 4519)
	stage := r.task("PR-12384", "bug", "At its local review", registry.TaskActive, "local_review", "", "")
	merged := r.task("PR-12385", "chore", "Merged", registry.TaskActive, "publish", "", "")
	r.move(merged, registry.TaskDone, "", "")

	d := r.data(screenNow)
	opens := func(id int64) string { return d.Cards[id].Task.OpensNothing }
	for id, want := range map[int64]string{
		round.ID: "no pane to open: it waits on review: round 2 of the local review runs",
		claim.ID: "no pane to open: it waits on copy: preparing factory3",
		ask.ID:   "no pane to open: it waits on you: 1 question before coding",
		pub.ID:   "no pane to open: it waits on magnum: review of #4519",
		stage.ID: "nothing runs: it waits on magnum: local review",
	} {
		if got := opens(id); got != want {
			t.Errorf("task %d: o says %q, want %q", id, got, want)
		}
	}
	if got := opens(merged.ID); !strings.HasPrefix(got, "nothing runs: merged ") {
		t.Errorf("a task that ended: o says %q, want nothing runs", got)
	}

	r.holds(mysqlHold())
	d = r.data(screenNow)
	if got, want := opens(stage.ID), "nothing runs: the factory holds every stage (MySQL does not answer since 09:00)"; got != want {
		t.Errorf("under the hold: %q, want %q", got, want)
	}
	if got, want := opens(round.ID), "no pane to open: it waits on review: round 2 of the local review runs"; got != want {
		t.Errorf("a round that runs, under the hold: %q, want its own wait %q", got, want)
	}
}
