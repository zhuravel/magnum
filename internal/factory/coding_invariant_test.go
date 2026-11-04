package factory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The invariant of these tests: no git step the factory runs in a copy
// discards a change the factory did not make (a switch with
// --discard-changes, a restore, the removal of a spec file it laid). What it
// cannot vouch for it leaves, names for you, and writes a fail event.

// redOnMaster makes master's credit_spec red (an example no version
// passes), so a precheck's red spec runs again on the base.
func (r *codingRig) redOnMaster() {
	r.t.Helper()
	r.moveMaster("spec/models/credit_spec.rb", creditSpec+"expect: def bonus in app/credit.rb\n")
	r.git(r.main, "fetch", "--quiet", "origin")
}

func (r *codingRig) detached() bool {
	r.t.Helper()
	return r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD") == ""
}

func (r *codingRig) hasRecord(id int64) bool {
	_, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(id), detachedFile))
	return err == nil
}

// The precheck's head spec run leaves a tracked change, then you come into
// the pane before its base run: the base run does not start, and what the
// spec run left is neither put back under you nor handed to the writer as
// its change. The task needs you, naming it, and it stays after you left.
func TestAPrecheckThatMeetsYouBeforeItsBaseRunKeepsWhatItsSpecRunLeft(t *testing.T) {
	r := newCodingRig(t)
	r.redOnMaster()
	task := r.codingTask("PR-12391", "bug")
	r.startStage(task)
	r.fix(task)
	r.setDuring(func(dir string, run scriptRun) {
		switch {
		case strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" && strings.Contains(run.Script, "app/credit.rb"):
			r.write(dir, "README", "written during the head spec run\n")
			r.rt.setOperatorIn("fx-writer")
		case strings.HasPrefix(run.Script, "bin/jest"):
			r.rt.setOperatorIn("") // you leave before the precheck would end: what it left stays left
		}
	})
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") {
		t.Fatalf("task %s waits %q; want needs_you naming README", got.State, got.WaitsWhat)
	}
	if !hasEvent(r.events(task.ID, "check"), registry.PhaseFail, "README") {
		t.Fatalf("no fail event names README: %+v", r.events(task.ID, "check"))
	}
	r.setDuring(nil)
	r.rt.setOperatorIn("") // you leave
	for range 4 {
		r.tick()
		r.e.waitBackground()
	}
	if s := r.read("README"); s != "written during the head spec run\n" {
		t.Fatalf("README = %q, want it kept", s)
	}
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts, want none for a change that may be yours", n)
	}
}

// holdSpecLock holds the machine's spec lock, as another precheck running
// specs does; release frees it.
func (r *codingRig) holdSpecLock() (release func()) {
	r.t.Helper()
	lock := r.layout.FactorySpecsLock()
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		r.t.Fatal(err)
	}
	unlock, held, err := engine.AcquireLock(lock)
	if err != nil || held {
		r.t.Fatalf("AcquireLock = %v, %v", held, err)
	}
	return unlock
}

// The precheck's start is before its waits: you typed into the copy while it
// waited for the machine's spec lock and left, long enough ago that you are
// in no pane now; that edit is still not the precheck's to put back.
func TestThePrecheckCountsYourEditDuringItsLockWait(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12392", "bug")
	r.fix(task)
	release := r.holdSpecLock()
	type result struct {
		res PrecheckResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := r.e.precheck(r.ctx, task, r.cp)
		done <- result{res, err}
	}()
	waitFor(t, "the precheck to wait for the lock", func() bool { return strings.Contains(r.e.checkNote(task.ID), "another precheck") })
	r.clock.Add(time.Second)
	r.write(r.copyDir, "README", "your edit\n")
	r.rt.setTouchedAt("you typed into a pane of PR-12392 at 09:00:01", r.clock.Now())
	r.clock.Add(time.Hour) // your cooldown is long over
	release()
	got := <-done
	if got.err != nil || got.res.Passed || !got.res.ForYou || !strings.Contains(got.res.Why, "README") {
		t.Fatalf("precheck %+v, %v; want it failed for you, naming README", got.res, got.err)
	}
	if s := r.read("README"); s != "your edit\n" {
		t.Fatalf("README = %q, want your edit kept", s)
	}
}

// The proof never detaches a copy over a change it did not make: you typed
// into the copy while the proof waited for the spec lock (the tree it began
// on was clean). It keeps the change and names it for you.
func TestTheProofNeverDetachesOverAChangeItDidNotMake(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12393", "bug")
	head := r.fix(task)
	release := r.holdSpecLock()
	type result struct {
		p   Proof
		err error
	}
	done := make(chan result, 1)
	go func() {
		p, err := r.e.runProof(r.ctx, task, r.cp)
		done <- result{p, err}
	}()
	waitFor(t, "the proof to wait for the lock", func() bool { return strings.Contains(r.e.checkNote(task.ID), "another precheck") })
	r.write(r.copyDir, "README", "your edit\n")
	release()
	got := <-done
	if got.err != nil || got.p.Passed || !got.p.ForYou || !strings.Contains(got.p.Why, "README") {
		t.Fatalf("proof %+v, %v; want it failed for you, naming README", got.p, got.err)
	}
	if s := r.read("README"); s != "your edit\n" || r.detached() || r.git(r.copyDir, "rev-parse", "HEAD") != head {
		t.Fatalf("README = %q, detached %v; want your edit kept on the branch", s, r.detached())
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs, want no base run over your edit", n)
	}
	if !hasEvent(r.events(task.ID, "proof"), registry.PhaseFail, "README") {
		t.Fatalf("no fail event names README: %+v", r.events(task.ID, "proof"))
	}
}

// A copy left detached by a base run is put back without losing your edit
// to a spec file it laid: a laid file that is no longer the head's (new at
// the head, or one the base has) is kept, and the task needs you.
func TestARestoreOfADetachedCopyKeepsYourEditToALaidSpec(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12394", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	r.fix(task)
	head := r.commit("PR-12394 the old spec too", map[string]string{"spec/models/credit_spec.rb": creditSpec + "expect: 10 in app/credit.rb\n"})
	r.copies.reserve(task.ID, "factory1")
	specs := []string{pointsSpec, "spec/models/credit_spec.rb"}
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, specs); err != nil {
		t.Fatal(err)
	}
	r.write(r.copyDir, pointsSpec, "your edit of the new spec\n")
	r.write(r.copyDir, "spec/models/credit_spec.rb", "your edit of the old spec\n")
	r.restart()
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, pointsSpec) || !strings.Contains(got.WaitsWhat, "spec/models/credit_spec.rb") {
		t.Fatalf("task %s waits %q; want needs_you naming both laid specs", got.State, got.WaitsWhat)
	}
	if r.read(pointsSpec) != "your edit of the new spec\n" || r.read("spec/models/credit_spec.rb") != "your edit of the old spec\n" {
		t.Fatal("an edit of a laid spec was discarded")
	}
	if !r.detached() || !r.hasRecord(task.ID) {
		t.Fatal("the copy was switched, or its record dropped, over your edits")
	}

	// Laid files that are still the head's go back as before.
	r.write(r.copyDir, pointsSpec, r.git(r.copyDir, "show", head+":"+pointsSpec)+"\n")
	r.write(r.copyDir, "spec/models/credit_spec.rb", r.git(r.copyDir, "show", head+":spec/models/credit_spec.rb")+"\n")
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.onBranch(task.Branch, head)
}

// A copy the factory left detached is put back whatever its task's state:
// a precheck whose lint failed for you and whose base run you then came
// into keeps the task waiting on the copy, not at NEEDS YOU, until the copy
// is back; a task at NEEDS YOU for another reason gets its copy back too,
// its wait untouched.
func TestACopyLeftDetachedIsRestoredWhateverTheTasksState(t *testing.T) {
	r := newCodingRig(t)
	r.redOnMaster()
	r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/nolint {files}"} // exits 127: for you
	task := r.codingTask("PR-12395", "bug")
	r.startStage(task)
	head := r.fix(task)
	r.setDuring(func(_ string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") && run.Branch == "" && strings.Contains(run.Script, "credit_spec.rb") &&
			!strings.Contains(run.Script, pointsSpec) {
			r.rt.setOperatorIn("fx-writer") // you come in during the precheck's base run
		}
	})
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	if !r.detached() || !r.hasRecord(task.ID) {
		t.Fatalf("the copy is detached %v, record %v; want it held detached while you are in it", r.detached(), r.hasRecord(task.ID))
	}
	if got.State == registry.TaskNeedsYou {
		t.Fatalf("task needs you (%q) while its copy is left detached", got.WaitsWhat)
	}
	r.setDuring(nil)
	r.rt.setOperatorIn("")
	for i := 0; i < 8 && r.reload(task.ID).State != registry.TaskNeedsYou; i++ {
		r.tick()
		r.e.waitBackground()
	}
	got = r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "precheck.lint") {
		t.Fatalf("task %s waits %q; want needs_you naming precheck.lint once the copy is back", got.State, got.WaitsWhat)
	}
	r.onBranch(task.Branch, head)
	if r.hasRecord(task.ID) {
		t.Fatal("the record of the base run is left")
	}

	// A task at NEEDS YOU whose copy a base run left detached gets it back.
	base := r.git(r.copyDir, "merge-base", "HEAD", "origin/master")
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.onBranch(task.Branch, head)
	if got2 := r.reload(task.ID); got2.State != registry.TaskNeedsYou || got2.WaitsWhat != got.WaitsWhat {
		t.Fatalf("task %s waits %q; want its NEEDS YOU untouched (%q)", got2.State, got2.WaitsWhat, got.WaitsWhat)
	}
}

// A failure for you does not hide what else the precheck met: the cap's
// stop stays in precheck.md, and a restore that fails is the precheck's
// error, never swallowed.
func TestAFailureForYouHidesNeitherTheCapNorAFailedRestore(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/nolint {files}"} // exits 127: for you
	task := r.codingTask("PR-12396", "bug")
	r.fix(task)
	fire := make(chan time.Time)
	r.e.after = func(time.Duration) <-chan time.Time { return fire }
	r.blocks["bin/specs"] = make(chan struct{})
	done := make(chan PrecheckResult, 1)
	go func() {
		res, _ := r.e.precheck(r.ctx, task, r.cp)
		done <- res
	}()
	<-r.entered
	close(fire)
	res := <-done
	if res.Passed || !res.ForYou || !strings.Contains(res.Why, "precheck.lint") {
		t.Fatalf("precheck %+v; want the lint's failure for you", res)
	}
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, "Stopped: over its cap") {
		t.Fatalf("%s lost the cap's stop:\n%s", precheckFile, report)
	}
	delete(r.blocks, "bin/specs")

	r.e.after = nil
	r.setDuring(func(dir string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") {
			r.write(dir, "README", "written by the spec run\n")
		}
	})
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "restore") && slices.Contains(args, "--staged") {
			return errors.New("fatal: Unable to create index.lock: File exists")
		}
		return nil
	})
	if res, err := r.e.precheck(r.ctx, task, r.cp); err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("precheck = %+v, %v; want the failed restore as its error", res, err)
	}
	if !hasEvent(r.events(task.ID, "check"), registry.PhaseFail, "README") {
		t.Fatalf("no fail event names the failed restore: %+v", r.events(task.ID, "check"))
	}
}
