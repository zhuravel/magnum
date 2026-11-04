package factory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// dumpSchema is a bin/rails fake that dumps db/schema.rb as Rails does
// after a migration: the up's version, or none after a down.
func dumpSchema(t *testing.T) func(dir, script string) int {
	t.Helper()
	return func(dir, script string) int {
		dump := "version: none\n"
		if _, v, ok := strings.Cut(script, "VERSION="); ok && strings.Contains(script, ":up") {
			dump = "version: " + v + " (dumped)\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "db", "schema.rb"), []byte(dump), 0o644); err != nil {
			t.Error(err)
		}
		return 0
	}
}

// migrationTask starts the coding stage of a feature whose writer commits
// the fix, a migration and its db/schema.rb.
func (r *codingRig) migrationTask(key string) registry.Task {
	r.t.Helper()
	task := r.codingTask(key, "feature")
	r.startStage(task)
	r.fix(task)
	r.commit(key+" a column", map[string]string{
		"db/migrate/20261104090000_add_x_to_notes.rb": "class AddXToNotes < ActiveRecord::Migration[7.1]\n  def change\n" +
			"    add_column :notes, :x, :integer\n  end\nend\n",
		"db/schema.rb": "version: 20261104090000\n",
	})
	return task
}

func (r *codingRig) setDuring(fn func(dir string, run scriptRun)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.during = fn
}

// The precheck puts back what its commands changed only when nobody else
// could have changed the copy since it began: you entered the writer's pane
// in the middle of it, or typed into a pane of the task and left. It cannot
// tell its changes from yours then: it leaves them, and the task needs you,
// naming them.
func TestThePrecheckLeavesItsChangesWhenYouWereInTheCopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		enter func(r *codingRig)
	}{
		{"you are in the pane", func(r *codingRig) { r.rt.setOperatorIn("fx-writer") }},
		{"you typed and left", func(r *codingRig) { r.rt.setTouched("you typed into a pane of PR-12380 at 09:05:00") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			r.migrate = dumpSchema(t)
			r.cfg.Factory.Repos[0].Precheck.Migrations = []string{"bin/rails db:migrate:up VERSION={version}",
				"bin/rails db:migrate:down VERSION={version}", "bin/rails db:migrate:up VERSION={version}"}
			task := r.migrationTask("PR-12380")
			r.setDuring(func(_ string, run scriptRun) {
				if strings.HasPrefix(run.Script, "bin/rails") {
					tc.enter(r)
				}
			})
			r.endTurns(task, 1)
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "db/schema.rb") || !strings.Contains(got.WaitsWhat, "you") {
				t.Fatalf("task %s waits %q; want needs_you naming db/schema.rb and you", got.State, got.WaitsWhat)
			}
			if s := r.read("db/schema.rb"); s != "version: 20261104090000 (dumped)\n" {
				t.Fatalf("db/schema.rb = %q, want it left as it was", s)
			}
			if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
				t.Fatalf("the writer got %d prompts, want no fix turn for a change that may be yours", n)
			}
			if !hasEvent(r.events(task.ID, "check"), registry.PhaseFail, "db/schema.rb") {
				t.Fatalf("no fail event names the paths left: %+v", r.events(task.ID, "check"))
			}
		})
	}
}

// The proof's head run is no different: with you in the pane by its end, a
// tracked file it changed is left, and the task needs you.
func TestTheProofsHeadRunLeavesItsChangesWhenYouWereInTheCopy(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12381", "bug")
	r.startStage(task)
	r.fix(task)
	r.setDuring(func(dir string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" {
			r.write(dir, "README", "written during the head run\n")
			r.rt.setOperatorIn("fx-writer")
		}
	})
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") || !strings.Contains(got.WaitsWhat, "fx-writer pane") {
		t.Fatalf("task %s waits %q; want needs_you naming README and your pane", got.State, got.WaitsWhat)
	}
	if s := r.read("README"); s != "written during the head run\n" {
		t.Fatalf("README = %q, want it left", s)
	}
}

// A base run during which you came into the pane leaves the copy detached
// (its record stays): the task waits until you leave, then the branch comes
// back and the checks run again.
func TestABaseRunWithYouInThePaneLeavesTheCopyDetachedUntilYouLeave(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12382", "bug")
	r.startStage(task)
	head := r.fix(task)
	r.setDuring(func(_ string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") && run.Branch == "" {
			r.rt.setOperatorIn("fx-writer")
		}
	})
	r.ended(task, "fx-writer")
	r.tick()
	r.e.waitBackground()
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != "" {
		t.Fatalf("the copy was put back on %q under you", b)
	}
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), detachedFile)); err != nil {
		t.Fatalf("the base run's record went: %v", err)
	}
	r.tick()
	if got := r.reload(task.ID); got.WaitsWho != waitCopy || !strings.Contains(got.WaitsWhat, "fx-writer pane") {
		t.Fatalf("task waits %q: %q; want the copy's restore waiting on you", got.WaitsWho, got.WaitsWhat)
	}

	r.setDuring(nil)
	r.rt.setOperatorIn("")
	for i := 0; i < 8 && r.reload(task.ID).Stage == "coding"; i++ {
		r.tick()
		r.e.waitBackground()
	}
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q; want local_review once you left", got.State, got.Stage, got.WaitsWhat)
	}
	r.onBranch(task.Branch, head)
}

// A proof stopped in its head run (a drain) still puts back what its spec
// changed, with begin and ok events: the next check finds a clean tree.
func TestAProofStoppedInItsHeadRunPutsBackWhatItsSpecChanged(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12383", "bug")
	r.startStage(task)
	head := r.fix(task)
	started := make(chan struct{}, 1)
	r.setDuring(func(dir string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" {
			r.write(dir, "README", "written by the spec\n")
			started <- struct{}{}
		}
	})
	r.setHang(func(run scriptRun) bool { return strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" })
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() {
		_, err := r.e.runProof(ctx, task, r.cp)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a proof stopped in its head run passed")
	}
	r.onBranch(task.Branch, head)
	if s := r.read("README"); s != "shop\n" {
		t.Fatalf("README = %q, want the branch's back", s)
	}
	evs := r.events(task.ID, "check")
	if !hasEvent(evs, registry.PhaseBegin, "README") || !hasEvent(evs, registry.PhaseOK, "README") {
		t.Fatalf("check events %+v; want the restore's begin and ok", evs)
	}
}

// Fix turns count per attempt of the stage: your retry's new attempt gets
// its own, as the runtime's loop count starts again on a new attempt.
func TestAnotherAttemptOfTheCodingStageGetsItsOwnFixTurns(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12384", "bug")
	r.lint = func([]string) string { return "app/credit.rb:3:5: C: Style/NumericLiterals: on the changed line\n" }
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, 12)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s, want needs_you after its fix turns", got.State)
	}
	first := r.attempts(task.ID)[0]
	if err := r.reg.TransitionAttempt(r.ctx, first.ID, nil, registry.AttemptFailed, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		t.Fatal(err)
	}
	r.tick()                                                                           // your retry: a new attempt and its first prompt
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`) // its writer writes proof.json again
	as := r.attempts(task.ID)
	if len(as) != 2 {
		t.Fatalf("attempts %+v, want a second", as)
	}
	r.endTurns(task, 1)
	var second []sentPrompt
	for _, p := range r.rt.promptsTo("fx-writer") {
		if p.Attempt == as[1].ID {
			second = append(second, p)
		}
	}
	if len(second) != 2 || !strings.HasPrefix(second[1].Text, fixLead) {
		t.Fatalf("the second attempt's prompts %+v; want its first and a fix turn", second)
	}
	if got := r.reload(task.ID); got.State != registry.TaskActive {
		t.Fatalf("task %s waits %q; want active at its fix turn", got.State, got.WaitsWhat)
	}
}

// A command that did not start for a reason of the moment (EAGAIN on a
// loaded machine) is tried again with the checks, at no cost to the writer.
func TestATransientStartFailureIsTriedAgain(t *testing.T) {
	r := newCodingRig(t)
	var mu sync.Mutex
	failed := false
	r.startErr = func(run scriptRun) error {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(run.Script, "bin/specs") && !failed {
			failed = true
			return &os.SyscallError{Syscall: "fork/exec", Err: syscall.EAGAIN}
		}
		return nil
	}
	task := r.codingTask("PR-12385", "bug")
	r.startStage(task)
	r.fix(task)
	r.ended(task, "fx-writer")
	for i := 0; i < 10 && r.reload(task.ID).Stage == "coding" && isLive(r.reload(task.ID)); i++ {
		r.tick()
		r.e.waitBackground()
		r.clock.Add(2 * time.Minute)
	}
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q; want local_review after the try again", got.State, got.Stage, got.WaitsWhat)
	}
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts, want none for the machine's failure", n)
	}
}

// A command that is missing or not executable is the configuration's: the
// task needs you at once, naming the key.
func TestAMissingOrNotExecutableCommandGoesToYou(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(r *codingRig)
		want []string
	}{
		{"mise is not found", func(r *codingRig) {
			r.startErr = func(run scriptRun) error {
				if strings.HasPrefix(run.Script, "bin/specs") {
					return &exec.Error{Name: "mise", Err: exec.ErrNotFound}
				}
				return nil
			}
		}, []string{"precheck.specs", "missing or not executable"}},
		{"the shell finds no specs command", func(r *codingRig) { r.cfg.Factory.Repos[0].Precheck.Specs = "bin/nope {files}" },
			[]string{"precheck.specs", "exited 127"}},
		{"the shell finds no lint command", func(r *codingRig) { r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/nolint {files}"} },
			[]string{"precheck.lint", "exited 127"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			tc.edit(r)
			task := r.codingTask("PR-12386", "bug")
			r.startStage(task)
			r.fix(task)
			r.endTurns(task, 2)
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou {
				t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
			}
			for _, w := range tc.want {
				if !strings.Contains(got.WaitsWhat, w) {
					t.Errorf("the wait %q does not name %q", got.WaitsWhat, w)
				}
			}
			if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
				t.Fatalf("the writer got %d prompts, want no fix turn", n)
			}
		})
	}
}

// hasEvent reports whether evs hold one of phase whose text names what.
func hasEvent(evs []registry.Event, phase, what string) bool {
	for _, e := range evs {
		if e.Phase == phase && strings.Contains(e.Text, what) {
			return true
		}
	}
	return false
}
