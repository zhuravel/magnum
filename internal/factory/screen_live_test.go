package factory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// liveRun writes task id's live.log as a run of its checks does: each step
// a command that began at its time, all but the last ended.
func (r *screenRig) liveRun(id int64, steps ...liveStep) string {
	r.t.Helper()
	at := screenNow
	l, err := newLiveLog(r.layout.FactoryTaskDir(id), func() time.Time { return at }, liveMax)
	if err != nil {
		r.t.Fatal(err)
	}
	defer l.close()
	for i, st := range steps {
		at = st.at
		out, errOut := l.command(st.label, "bin/run")
		_, _ = out.Write([]byte("output of " + st.label + "\n"))
		if i < len(steps)-1 || !st.running {
			l.ended(st.label, out, errOut, execx.Result{}, nil, time.Second)
		}
	}
	return livePath(r.layout.FactoryTaskDir(id))
}

// The wait of a task whose check runs says its step once, the step's own
// word first ("precheck: jest", "proof: runs … on the base"), never
// "precheck: precheck: jest", and carries when the command began (the
// painter's clock moves from it on every frame): read from live.log's last
// begin line, which the daemon writes as the command starts, so it is no
// poll late. Between commands (a lock's wait) the wait is the registry's,
// with no time. The help line and the task card name live.log, which o
// follows, and how to come back.
func TestTheWaitOfARunningCheckShowsItsStepOnceWithItsTime(t *testing.T) {
	r := newScreenRig(t)
	r.at(time.Hour)
	task := r.task("PR-12410", "bug", "Credit points", registry.TaskActive, "coding", waitPrecheck, "precheck: jest")

	row := rowOf(t, r.data(screenNow), "PR-12410")
	if want := (tui.WaitsOn{Who: "precheck", What: "jest"}); len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
		t.Fatalf("blockers %+v, want %+v", row.WaitsOn, want)
	}
	if row.Help != "precheck: jest" {
		t.Errorf("help %q, want the step once and no file to follow before live.log", row.Help)
	}

	began := screenNow.Add(-3 * time.Minute)
	path := r.liveRun(task.ID, liveStep{label: "precheck lint (bin/lint)", at: screenNow.Add(-5 * time.Minute)},
		liveStep{label: "precheck jest", at: began, running: true})
	d := r.data(screenNow)
	row, card := rowOf(t, d, "PR-12410"), d.Cards[task.ID].Task
	if want := (tui.WaitsOn{Who: "precheck", What: "jest", Since: began}); len(row.WaitsOn) != 1 || !sameBlocker(row.WaitsOn[0], want) {
		t.Fatalf("blockers %+v, want %+v", row.WaitsOn, want)
	}
	if card.WaitingFor != "precheck: jest" || card.WaitingStep != "precheck: jest" || !card.WaitingSince.Equal(began) || card.Follow != path {
		t.Errorf("card waits %q (step %q since %v), follows %q; want precheck: jest since %v, following %s",
			card.WaitingFor, card.WaitingStep, card.WaitingSince, card.Follow, began, path)
	}
	if want := "precheck: jest · o follows live.log (ctrl-c, then q, comes back)"; row.Help != want {
		t.Errorf("help %q, want %q", row.Help, want)
	}

	// The proof's base run: its own word, whatever the registry's wait
	// still says (the tick writes it, a poll late).
	r.liveRun(task.ID, liveStep{label: "proof runs spec/models/credit_spec.rb on the base", at: began, running: true})
	row = rowOf(t, r.data(screenNow), "PR-12410")
	if w := row.WaitsOn[0]; w.Who != "proof" || w.What != "runs spec/models/credit_spec.rb on the base" || !w.Since.Equal(began) {
		t.Errorf("the proof's blocker %+v", w)
	}

	// Between commands the registry's words, the step's word first, no time.
	r.liveRun(task.ID, liveStep{label: "precheck lint (bin/lint)", at: began})
	r.move(task, registry.TaskActive, waitPrecheck, "precheck: specs waits for the copy's databases (reviewer of round 2)")
	row = rowOf(t, r.data(screenNow), "PR-12410")
	if w := row.WaitsOn[0]; w.Who != "precheck" || w.What != "specs waits for the copy's databases (reviewer of round 2)" || !w.Since.IsZero() {
		t.Errorf("the blocker between commands %+v", w)
	}
}

// sameBlocker compares two blockers, their times by Equal.
func sameBlocker(a, b tui.WaitsOn) bool {
	return a.Who == b.Who && a.What == b.What && a.Short == b.Short && a.Long == b.Long && a.OnYou == b.OnYou && a.Since.Equal(b.Since)
}

// While an agent of the task works, o steps into its pane as before: the
// card names no file to follow, and the help line offers none.
func TestNoFileToFollowWhileAnAgentOfTheTaskWorks(t *testing.T) {
	r := newScreenRig(t)
	r.at(time.Hour)
	task := r.task("PR-12411", "bug", "Credit points", registry.TaskActive, "coding", waitPrecheck, "precheck: jest")
	r.liveRun(task.ID, liveStep{label: "precheck jest", at: screenNow.Add(-time.Minute), running: true})
	r.session(task, "fx-writer", "working", screenNow.Add(-10*time.Minute), screenNow.Add(-3*time.Second))
	d := r.data(screenNow)
	if card := d.Cards[task.ID].Task; card.Follow != "" {
		t.Errorf("the card follows %q while the writer works", card.Follow)
	}
	if row := rowOf(t, d, "PR-12411"); strings.Contains(row.Help, "o follows") {
		t.Errorf("help %q offers o's file while the writer works", row.Help)
	}
}

// A copy's preparation (a claim) shows the copy and since when the claim
// runs, from its begin event, without the setup's minutes of the daemon's
// words; o follows the log of the copy's slot scripts written last. A QA
// server's boot runs since its start (qa-run.json), and o follows the
// server's log.
func TestTheClaimAndTheBootShowTheirTimeAndTheirLogs(t *testing.T) {
	r := newScreenRig(t)
	r.at(time.Hour)
	claim := r.task("PR-12412", "bug", "Credit points", registry.TaskActive, "investigate", waitCopy, "preparing factory3 (setup 3m)")
	r.at(3 * time.Minute)
	if err := r.reg.AppendEvent(r.ctx, claim.ID, "claim", registry.PhaseBegin, "claims a copy, off the loop"); err != nil {
		t.Fatal(err)
	}
	r.move(claim, registry.TaskActive, waitCopy, "preparing factory3 (setup 3m)", func(u *registry.TaskUpdate) { u.Set("copy", "factory3") })
	logs := r.layout.Logs()
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	provision, slot := filepath.Join(logs, "provision-factory3.log"), filepath.Join(logs, "slot-factory3.log")
	for i, p := range []string{provision, slot} {
		if err := os.WriteFile(p, []byte("== begin setup\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := screenNow.Add(time.Duration(i-2) * time.Minute) // the slot log is the newer
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}

	boot := r.task("PR-12413", "feature", "Settings page", registry.TaskActive, "qa", waitQA, "the copy's server boots (up to 2m)")
	r.move(boot, registry.TaskActive, waitQA, "the copy's server boots (up to 2m)", func(u *registry.TaskUpdate) { u.Set("copy", "factory2") })
	started := screenNow.Add(-40 * time.Second)
	b, _ := json.Marshal(qaRun{N: 1, Head: "abc1234def", Dir: qaRunDir(1, "abc1234def"), Started: started})
	r.file(boot.ID, qaRunFile, string(b))
	server := serverLogIn(logs, "factory2")
	if err := os.WriteFile(server, []byte("Puma starting\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := r.data(screenNow)
	w := rowOf(t, d, "PR-12412").WaitsOn
	if len(w) != 1 || w[0].Who != "copy" || w[0].What != "preparing factory3" || !w[0].Since.Equal(screenNow.Add(-3*time.Minute)) {
		t.Errorf("the claim's blockers %+v", w)
	}
	if got := d.Cards[claim.ID].Task.Follow; got != slot {
		t.Errorf("the claim's card follows %q, want the newer slot log %s", got, slot)
	}
	w = rowOf(t, d, "PR-12413").WaitsOn
	if len(w) != 1 || w[0].Who != "qa" || w[0].What != "the copy's server boots (up to 2m)" || !w[0].Since.Equal(started) {
		t.Errorf("the boot's blockers %+v", w)
	}
	if got := d.Cards[boot.ID].Task.Follow; got != server {
		t.Errorf("the boot's card follows %q, want %s", got, server)
	}
	// A first claim's task has not noted its copy yet: the copy the claim
	// reserved for its owner row in the agents store names it.
	first := r.task("PR-12415", "bug", "First claim", registry.TaskActive, "investigate", waitCopy, "preparing a copy (setup <1m)")
	s := r.session(first, "fx-history", "idle", screenNow.Add(-time.Minute), screenNow.Add(-time.Minute))
	if _, err := r.agents.CreateSlot(r.ctx, store.Slot{Name: "factory4", RepoFullName: "example/talkable", Kind: store.SlotKindPool,
		Path: filepath.Join(t.TempDir(), "factory4"), State: store.SlotClaimed, PRID: &s.PRID}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "provision-factory4.log"), []byte("== begin setup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d = r.data(screenNow)
	if w := rowOf(t, d, "PR-12415").WaitsOn; len(w) != 1 || w[0].What != "preparing factory4" ||
		d.Cards[first.ID].Task.Follow != filepath.Join(logs, "provision-factory4.log") {
		t.Errorf("a first claim: %+v, follows %q", w, d.Cards[first.ID].Task.Follow)
	}
	// Any other wait keeps today's words and no file.
	other := r.task("PR-12414", "bug", "Other", registry.TaskActive, "coding", "heavy", "PR-12410's precheck runs")
	d = r.data(screenNow)
	if w := rowOf(t, d, "PR-12414").WaitsOn; !slices.Equal(w, []tui.WaitsOn{{Who: "heavy", What: "PR-12410's precheck runs"}}) ||
		d.Cards[other.ID].Task.Follow != "" {
		t.Errorf("another wait: %+v, follows %q", w, d.Cards[other.ID].Task.Follow)
	}
}
