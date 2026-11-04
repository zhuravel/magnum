package factory

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// Your herdr session and the factory's each number their workspaces and
// panes from w1, so one id names two panes. The factory's live session in
// its w1:p1 never hides your agent in your w1:p1, nor your console in a pane
// of your w1, when they work in a copy: the copy is held, not reset under
// them.
func TestTheCopiesGuardSeesYourPaneThatSharesAFactoryPanesID(t *testing.T) {
	for _, tc := range []struct {
		name, hold string
		show       func(r *copiesRig, dir string)
	}{
		{"your agent", slots.HoldForeignAgent, func(r *copiesRig, dir string) {
			r.yourHerdr.pane("w1", "w1:p1", dir, "claude")
		}},
		{"your console", slots.HoldForegroundProcess, func(r *copiesRig, dir string) {
			r.yourHerdr.pane("w1", "w1:p2", dir, "", herdr.Process{PID: 77, Name: "ruby", Cmdline: "bin/rails console"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCopiesRig(t, 2)
			other, task := r.task("PR-1"), r.task("PR-2")
			busy := r.claim(other)
			c := r.claim(task)
			pr, err := r.owner(r.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.st.CreateSession(r.ctx, store.Session{PRID: pr.ID, Role: "fx-writer", State: store.SessionLive,
				HerdrWorkspaceID: new("w1"), HerdrPaneID: new("w1:p1"), Cwd: new(busy.Path)}); err != nil {
				t.Fatal(err)
			}
			r.factoryHerdr.pane("w1", "w1:p1", busy.Path, "mg-1-fx-writer")
			tc.show(r, filepath.Join(c.Path, "app"))

			err = r.c.Release(r.ctx, task, "stopped")
			if h, ok := slots.AsHold(err); !ok || h.Reason != tc.hold {
				t.Fatalf("Release = %v, want the guard's %s hold", err, tc.hold)
			}
			if sl := r.slot(c.Name); sl.State != store.SlotClaimed {
				t.Fatalf("%s = %s, want still claimed, not reset under you", c.Name, sl.State)
			}
		})
	}
}

// A pane of the factory's herdr session in a copy that no session row of
// the factory's names (someone opened it there) holds the copy; the shell a
// parked session left in it does not.
func TestAPaneOfTheFactorysSessionWithNoSessionRowHoldsTheCopy(t *testing.T) {
	r := newCopiesRig(t, 1)
	task := r.task("PR-1")
	c := r.claim(task)
	pr, err := r.owner(r.ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateSession(r.ctx, store.Session{PRID: pr.ID, Role: "fx-repro", State: store.SessionParked,
		HerdrWorkspaceID: new("w2"), HerdrPaneID: new("w2:p1"), Cwd: new(c.Path)}); err != nil {
		t.Fatal(err)
	}
	r.factoryHerdr.pane("w2", "w2:p1", c.Path, "")
	r.factoryHerdr.pane("w2", "w2:p2", filepath.Join(c.Path, "app"), "")

	err = r.c.Release(r.ctx, task, "stopped")
	if !errors.Is(err, ErrPaneInCopy) || !strings.Contains(err.Error(), "w2:p2") {
		t.Fatalf("Release = %v, want ErrPaneInCopy naming pane w2:p2", err)
	}
	if sl := r.slot(c.Name); sl.State != store.SlotClaimed {
		t.Fatalf("%s = %s, want still the task's", c.Name, sl.State)
	}
	r.factoryHerdr.clear()
	r.factoryHerdr.pane("w2", "w2:p1", c.Path, "")
	r.release(task)
}

// claimSubject is the steps' subject of the claim the copy name has now.
func (r *copiesRig) claimSubject(name string) string {
	r.t.Helper()
	a, err := r.st.OpenAssignmentBySlot(r.ctx, r.slot(name).ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return "slot:" + name + ":claim:" + strconv.FormatInt(a.ID, 10)
}

// stepPhases are the phases of each step of subject, in order.
func (r *copiesRig) stepPhases(subject string) map[string][]string {
	r.t.Helper()
	evs, err := r.st.EventsBySubject(r.ctx, subject, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range evs {
		if e.Kind == store.KindStep {
			out[store.Deref(e.Step)] = append(out[store.Deref(e.Step)], store.Deref(e.Phase))
		}
	}
	return out
}

// Each step of the claim protocol that destroys data or moves the copy is a
// step with begin and ok or fail rows under the claim's subject: a failed
// one runs again at the task's next claim, and the done ones do not.
func TestTheClaimsDestructiveStepsWriteBeginOkFail(t *testing.T) {
	r := newCopiesRig(t, 1)
	a := r.task("PR-1")
	r.claim(a)
	first := r.stepPhases(r.claimSubject("factory1"))
	for _, step := range []string{"install Gemfile.lock", "flush_redis"} {
		if got := first[step]; !slices.Equal(got, []string{"begin", "ok"}) {
			t.Errorf("step %s of the first claim = %v, want begin, ok (all: %v)", step, got, first)
		}
	}
	r.release(a)

	dev := "talkable_development__factory1"
	r.dbs.set("20261101000000", dev)
	r.mu.Lock()
	r.failRedis = 1
	r.mu.Unlock()
	b := r.task("PR-2")
	if _, err := r.c.Claim(r.ctx, b); err == nil || !strings.Contains(err.Error(), "Could not connect to Redis") {
		t.Fatalf("Claim with Redis down = %v, want the flush's failure", err)
	}
	subject := r.claimSubject("factory1")
	got := r.stepPhases(subject)
	if !slices.Equal(got["reset_db"], []string{"begin", "ok"}) || !slices.Equal(got["flush_redis"], []string{"begin", "fail"}) {
		t.Fatalf("steps of %s = %v, want reset_db begin, ok and flush_redis begin, fail", subject, got)
	}
	r.claim(b)
	got = r.stepPhases(subject)
	if !slices.Equal(got["reset_db"], []string{"begin", "ok"}) || !slices.Equal(got["flush_redis"], []string{"begin", "fail", "begin", "ok"}) {
		t.Fatalf("steps of %s after the next claim = %v, want the flush run again and reset_db not", subject, got)
	}
	if resets := r.calls("bin/rails db:schema:load", ""); len(resets) != 1 {
		t.Fatalf("reset_db ran %d times, want once", len(resets))
	}
}

// The guard runs again before each step of a claim that destroys data: a
// person's agent in the copy stops reset_db (and the flush after it), and
// the task's next claim, once the agent left, runs the guard again and then
// the reset.
func TestAClaimReRunsTheGuardBeforeItResetsTheDatabase(t *testing.T) {
	r := newCopiesRig(t, 1)
	a := r.task("PR-1")
	c := r.claim(a)
	r.release(a)
	r.dbs.set("20261101000000", "talkable_development__factory1")
	flushed := len(r.flushes())
	r.yourHerdr.pane("w1", "w1:p1", c.Path, "claude")

	b := r.task("PR-2")
	_, err := r.c.Claim(r.ctx, b)
	if h, ok := slots.AsHold(err); !ok || h.Reason != slots.HoldForeignAgent {
		t.Fatalf("Claim with your agent in the copy = %v, want the guard's foreign agent hold", err)
	}
	if resets := r.calls("bin/rails db:schema:load", ""); len(resets) != 0 {
		t.Fatalf("reset_db ran %d times under your agent", len(resets))
	}
	if got := len(r.flushes()); got != flushed {
		t.Fatalf("flushed %d databases under your agent", got-flushed)
	}

	r.yourHerdr.clear()
	r.claim(b)
	if resets := r.calls("bin/rails db:schema:load", ""); len(resets) != 1 {
		t.Fatalf("reset_db ran %d times once your agent left, want 1", len(resets))
	}
}

// A claim holds the copies' bookkeeping only to count and reserve: while a
// claim adds a copy (its setup runs), a release of another copy returns at
// once, and a claim of a free copy goes through.
func TestAReleaseDoesNotWaitForAnotherCopysClaim(t *testing.T) {
	r := newCopiesRig(t, 3)
	a := r.task("PR-1")
	r.claim(a)
	gate := make(chan struct{})
	var open sync.Once
	t.Cleanup(func() { open.Do(func() { close(gate) }) })
	r.mu.Lock()
	r.blocks["bin/worktree-setup factory2"] = gate
	r.mu.Unlock()

	b := r.task("PR-2")
	type claimed struct {
		c   Copy
		err error
	}
	slow := make(chan claimed, 1)
	go func() {
		c, err := r.c.Claim(r.ctx, b)
		slow <- claimed{c, err}
	}()
	select {
	case got := <-r.entered:
		if got != "bin/worktree-setup factory2" {
			t.Fatalf("blocked in %q, want factory2's setup", got)
		}
	case <-time.After(time.Minute):
		t.Fatal("the slow claim never reached factory2's setup")
	}

	released := make(chan error, 1)
	go func() { released <- r.c.Release(r.ctx, a, "released") }()
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the release of factory1 waited for the claim that adds factory2")
	}
	fast := make(chan claimed, 1)
	go func() {
		c, err := r.c.Claim(r.ctx, r.task("PR-3"))
		fast <- claimed{c, err}
	}()
	select {
	case got := <-fast:
		if got.err != nil || got.c.Name != "factory1" {
			t.Fatalf("the claim of a free copy = %+v, %v; want factory1", got.c, got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the claim of the free factory1 waited for the claim that adds factory2")
	}

	// Another claim that must add a copy meanwhile adds the next one, never
	// the one being added.
	other := make(chan claimed, 1)
	go func() {
		c, err := r.c.Claim(r.ctx, r.task("PR-4"))
		other <- claimed{c, err}
	}()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := r.st.SlotByName(r.ctx, "factory3"); err == nil {
			break // it chose its copy while factory2's setup still runs
		}
		if time.Now().After(deadline) {
			t.Fatal("the other claim never started to add factory3")
		}
	}

	open.Do(func() { close(gate) })
	if got := <-slow; got.err != nil || got.c.Name != "factory2" {
		t.Fatalf("the slow claim = %+v, %v; want factory2", got.c, got.err)
	}
	if got := <-other; got.err != nil || got.c.Name != "factory3" {
		t.Fatalf("the claim that added a copy meanwhile = %+v, %v; want factory3", got.c, got.err)
	}
	for _, name := range []string{"factory2", "factory3"} {
		if n := len(r.calls("bin/worktree-setup", filepath.Join(r.root, "talkable."+name))); n != 1 {
			t.Errorf("%s's setup ran %d times, want once", name, n)
		}
	}
}
