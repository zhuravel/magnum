package factory

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
)

// What a check of the factory keeps for you (changes it cannot tell from
// others', or that are not its own) stays kept: named in the reason whatever
// failed before it, never put back by the check that kept it, never handed
// to the writer as its change, and looked at again only when it can matter.

// copyReads counts the git status reads of the rig's copy from now on.
func (r *codingRig) copyReads() *atomic.Int32 {
	r.t.Helper()
	var n atomic.Int32
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, r.copyDir) && slices.Contains(args, "status") {
			n.Add(1)
		}
		return nil
	})
	return &n
}

// restoreEvents counts the proof events of task id's restores of a copy a
// base run left detached: their begins, their fails and their oks.
func (r *codingRig) restoreEvents(id int64) (begins, fails, oks int) {
	r.t.Helper()
	for _, ev := range r.events(id, "proof") {
		switch {
		case ev.Phase == registry.PhaseBegin && strings.HasPrefix(ev.Text, "restores "):
			begins++
		case ev.Phase == registry.PhaseFail && strings.Contains(ev.Text, "restore after a base run"):
			fails++
		case ev.Phase == registry.PhaseOK && strings.HasPrefix(ev.Text, "restored "):
			oks++
		}
	}
	return begins, fails, oks
}

// letGoOn is your "go on" to a task at NEEDS YOU.
func (r *codingRig) letGoOn(id int64) {
	r.t.Helper()
	if err := r.reg.TransitionTask(r.ctx, id, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		r.t.Fatal(err)
	}
}

// A copy a base run left detached with your edit of a laid spec is kept for
// you: one fail event and no begin it cannot end, and a task the loop does
// not advance has its copy looked at again only when a kept path changed or
// after keptRecheck, not on every tick. Once the laid spec is the head's
// again, the next tick puts the copy back.
func TestAKeptDetachedCopyIsSaidOnceAndLookedAtAgainOnlyWhenItChanges(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12401", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)
	r.copies.reserve(task.ID, "factory1")
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	r.write(r.copyDir, pointsSpec, "your edit of the laid spec\n")
	r.restart()
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, pointsSpec) {
		t.Fatalf("task %s waits %q; want needs_you naming the laid spec", got.State, got.WaitsWhat)
	}
	reads := r.copyReads()
	for range 5 {
		r.clock.Add(45 * time.Second)
		r.tick()
	}
	if b, f, _ := r.restoreEvents(task.ID); b != 0 || f != 1 {
		t.Errorf("%d begin and %d fail events; want no begin it cannot end and one fail", b, f)
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("the kept copy was read %d times in 5 ticks with nothing changed", n)
	}

	r.clock.Add(keptRecheck)
	r.tick()
	if reads.Load() == 0 {
		t.Error("the kept copy was not looked at again after keptRecheck")
	}
	if b, f, _ := r.restoreEvents(task.ID); b != 0 || f != 1 {
		t.Errorf("%d begin and %d fail events after the look again; want it said once", b, f)
	}

	r.write(r.copyDir, pointsSpec, r.git(r.copyDir, "show", head+":"+pointsSpec)+"\n")
	r.tick()
	r.onBranch(task.Branch, head)
	if b, _, ok := r.restoreEvents(task.ID); b != 1 || ok != 1 {
		t.Fatalf("%d begin and %d ok events; want the restore's begin and ok", b, ok)
	}
}

// The precheck's base run refuses to detach over a change it did not make
// (an edit outside herdr, after the precheck put its own back): it names the
// change for you, keeps it, and runs nothing more, its end put-back
// included.
func TestAPrecheckNeverPutsBackAChangeItNamedAsNotItsOwn(t *testing.T) {
	r := newCodingRig(t)
	r.redOnMaster()
	task := r.codingTask("PR-12402", "bug")
	r.fix(task)
	var headRan, edited atomic.Bool
	r.setDuring(func(_ string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" {
			headRan.Store(true)
		}
	})
	r.setFailGit(func(args []string) error {
		if headRan.Load() && slices.Contains(args, "diff") && !edited.Swap(true) { // the base run looks at the copy's tree
			r.write(r.copyDir, "README", "your edit\n")
		}
		return nil
	})
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !res.ForYou || !strings.Contains(res.Why, "README") {
		t.Fatalf("precheck %+v, %v; want it failed for you, naming README", res, err)
	}
	if s := r.read("README"); s != "your edit\n" {
		t.Fatalf("README = %q: the precheck named it as a change it did not make, then put it back", s)
	}
	if n := len(r.ran("bin/jest")); n != 0 {
		t.Fatalf("jest ran %d times after the precheck kept a change for you", n)
	}

	// Once the task goes on, it is still not the factory's to put back.
	r.copies.reserve(task.ID, "factory1")
	if why, err := r.e.settleKept(r.ctx, task); err != nil || !strings.Contains(why, "README") {
		t.Fatalf("settleKept = %q, %v; want the task to need you again, naming README", why, err)
	}
	if s := r.read("README"); s != "your edit\n" {
		t.Fatalf("README = %q after the task went on, want your edit kept", s)
	}
}

// A change the precheck keeps for you is named whatever failed for you
// before it (a lint the shell cannot find), and it never comes back to the
// writer as a change to commit or undo. Once you left and let the task go on,
// it goes back under the guard (nobody else could have changed the copy
// since the check began, and it is as the check left it) or, when you typed
// into a pane since or changed it since, the task needs you again for it.
func TestAKeptChangeIsNamedWhateverFailedFirstAndNeverHandedToTheWriter(t *testing.T) {
	for _, tc := range []struct {
		name           string
		typed, changed bool
	}{{"you left without typing", false, false}, {"you typed since the check began", true, false},
		{"you changed it since the check left it", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			r.redOnMaster()
			r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/nolint {files}"} // exits 127: for you
			task := r.codingTask("PR-12403", "bug")
			r.startStage(task)
			head := r.fix(task)
			r.setDuring(func(dir string, run scriptRun) {
				if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" && strings.Contains(run.Script, "app/credit.rb") {
					r.write(dir, "README", "written during the head spec run\n")
					r.rt.setOperatorIn("fx-writer")
					if tc.typed {
						r.rt.setTouchedAt("you typed into a pane of PR-12403", r.clock.Now().Add(time.Second))
					}
				}
			})
			r.endTurns(task, 1)
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "precheck.lint") || !strings.Contains(got.WaitsWhat, "README") {
				t.Fatalf("task %s waits %q; want needs_you naming precheck.lint and README", got.State, got.WaitsWhat)
			}

			r.setDuring(nil)
			r.rt.setOperatorIn("") // you leave, fix the lint and let the task go on
			r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/lint {files}"}
			kept := "written during the head spec run\n"
			if tc.changed {
				kept = "yours now\n" // an editor outside herdr: nothing tells who
				r.write(r.copyDir, "README", kept)
			}
			r.letGoOn(task.ID)
			for i := 0; i < 6 && isLive(r.reload(task.ID)) && r.reload(task.ID).Stage == "coding"; i++ {
				r.clock.Add(time.Minute)
				r.tick()
				r.e.waitBackground()
			}
			for _, p := range r.rt.promptsTo("fx-writer") {
				if strings.Contains(p.Text, "README") || strings.Contains(p.Text, "git status shows") {
					t.Fatalf("the writer was handed what the factory kept for you: %q", p.Text)
				}
			}
			if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
				t.Fatalf("the writer got %d prompts, want its first only", n)
			}
			got = r.reload(task.ID)
			if tc.typed || tc.changed {
				if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") {
					t.Fatalf("task %s waits %q; want needs_you naming README again", got.State, got.WaitsWhat)
				}
				if s := r.read("README"); s != kept {
					t.Fatalf("README = %q, want it kept (%q)", s, kept)
				}
				return
			}
			if s := r.read("README"); s != "shop\n" {
				t.Fatalf("README = %q, want it put back once nobody else could have changed it", s)
			}
			evs := r.events(task.ID, "check")
			if !hasEvent(evs, registry.PhaseBegin, "README") || !hasEvent(evs, registry.PhaseOK, "README") {
				t.Fatalf("check events %+v; want the put-back's begin and ok", evs)
			}
			if got.State == registry.TaskNeedsYou {
				t.Fatalf("task needs you (%q) after the put-back", got.WaitsWhat)
			}
			r.git(r.copyDir, "rev-parse", head) // the branch's head is still there
		})
	}
}

// A put-back that fails stops the precheck: none of its commands runs on
// the tree it could not put back (no base run, no jest), and the failure is
// its error.
func TestAFailedPutBackStopsThePrecheck(t *testing.T) {
	r := newCodingRig(t)
	r.redOnMaster()
	task := r.codingTask("PR-12405", "bug")
	r.fix(task)
	r.setDuring(func(dir string, run scriptRun) {
		if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" && strings.Contains(run.Script, "app/credit.rb") {
			r.write(dir, "README", "written by the head spec run\n")
		}
	})
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "restore") && slices.Contains(args, "--staged") {
			return errors.New("fatal: Unable to create index.lock: File exists")
		}
		return nil
	})
	_, err := r.e.precheck(r.ctx, task, r.cp)
	if err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("precheck = %v; want the failed put-back as its error", err)
	}
	if n := len(r.ran("bin/jest")); n != 0 {
		t.Fatalf("jest ran %d times on a tree the precheck could not put back", n)
	}
	for _, run := range r.ran("bin/specs") {
		if run.Branch == "" {
			t.Fatalf("a base run ran on a tree the precheck could not put back: %+v", run)
		}
	}
}

// A check that waited (the machine's spec lock) looks at the copy again
// before it runs anything: an edit made while it waited (outside herdr, so
// nothing tells who) is not its own, and no command runs over it.
func TestNoCheckRunsACommandOverAnEditMadeWhileItWaited(t *testing.T) {
	t.Run("the precheck's migrations", func(t *testing.T) {
		r := newCodingRig(t)
		r.migrate = dumpSchema(t)
		task := r.migrationTask("PR-12406")
		release := r.holdSpecLock()
		done := make(chan PrecheckResult, 1)
		go func() {
			res, err := r.e.precheck(r.ctx, task, r.cp)
			if err != nil {
				t.Errorf("precheck: %v", err)
			}
			done <- res
		}()
		waitFor(t, "the precheck to wait for the lock", func() bool { return strings.Contains(r.e.checkNote(task.ID), "another precheck") })
		r.write(r.copyDir, "db/schema.rb", "your edit\n")
		release()
		res := <-done
		if res.Passed || !res.ForYou || !strings.Contains(res.Why, "db/schema.rb") {
			t.Fatalf("precheck %+v; want it failed for you, naming db/schema.rb", res)
		}
		if n := len(r.ran("bin/rails")); n != 0 {
			t.Fatalf("%d migrations ran over your edit", n)
		}
		if s := r.read("db/schema.rb"); s != "your edit\n" {
			t.Fatalf("db/schema.rb = %q, want your edit kept", s)
		}
	})
	t.Run("a chore's proof", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.codingTask("PR-12407", "chore")
		r.commit("PR-12407 rename", map[string]string{"app/credit.rb": creditRB + "# renamed\n"})
		r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_spec.rb"]}`)
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
		if n := len(r.ran("bin/specs")); n != 0 {
			t.Fatalf("%d spec runs over your edit", n)
		}
		if s := r.read("README"); s != "your edit\n" {
			t.Fatalf("README = %q, want your edit kept", s)
		}
	})
}

// detachedAtNeedsYou is task's copy left detached by a base run of head
// (the record there) while the task is at NEEDS YOU for another reason.
func (r *codingRig) detachedAtNeedsYou(key string) (registry.Task, string) {
	r.t.Helper()
	task := r.codingTask(key, "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)
	r.copies.reserve(task.ID, "factory1")
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", "something else")
	}); err != nil {
		r.t.Fatal(err)
	}
	return task, head
}

// The sweep of the copies base runs left detached respects the herdr check
// as the rest of the tick does: while herdr does not answer (nobody can say
// who is in the copy), no copy is touched; once it does, the copy comes
// back.
func TestACopyLeftDetachedWaitsWhileHerdrDoesNotAnswer(t *testing.T) {
	r := newCodingRig(t)
	task, head := r.detachedAtNeedsYou("PR-12408")
	var down atomic.Bool
	down.Store(true)
	r.e.d.HerdrUp = func(context.Context) error {
		if down.Load() {
			return errors.New("herdr: connection refused")
		}
		return nil
	}
	r.tick()
	if !r.detached() || !r.hasRecord(task.ID) {
		t.Fatalf("the copy is detached %v, record %v; want it left while herdr does not answer", r.detached(), r.hasRecord(task.ID))
	}
	down.Store(false)
	r.tick()
	r.onBranch(task.Branch, head)
}

// A panic in the sweep of a copy left detached is contained as a task's
// step is (safely): the tick goes on, and the panic is the task's event. A
// task the loop does not advance has the sweep try it again only when it is
// due (keptRecheck), not on every tick.
func TestAPanicInTheSweepOfADetachedCopyIsContained(t *testing.T) {
	r := newCodingRig(t)
	task, _ := r.detachedAtNeedsYou("PR-12409")
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, r.copyDir) && slices.Contains(args, "symbolic-ref") {
			panic("a bug in the sweep")
		}
		return nil
	})
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the tick panicked: %v", p)
			}
		}()
		for range 6 {
			r.clock.Add(45 * time.Second)
			r.tick()
		}
	}()
	if evs := r.events(task.ID, "panic"); len(evs) != 1 || !hasEvent(evs, registry.PhaseFail, "a bug in the sweep") {
		t.Fatalf("panic events %+v; want the sweep's, once in 6 ticks", evs)
	}
	r.clock.Add(keptRecheck)
	r.tick()
	r.setFailGit(nil)
	if n := len(r.events(task.ID, "panic")); n != 2 {
		t.Fatalf("%d panic events after keptRecheck; want the sweep tried again once", n)
	}
}

// failRestores makes git's put-back (`restore --staged`) fail while broken
// holds: every time, or (once) only the first, when you then come into the
// writer's pane.
func (r *codingRig) failRestores(once bool) (broken *atomic.Bool) {
	r.t.Helper()
	broken = &atomic.Bool{}
	broken.Store(true)
	var n atomic.Int32
	r.setFailGit(func(args []string) error {
		if !broken.Load() || !slices.Contains(args, "restore") || !slices.Contains(args, "--staged") {
			return nil
		}
		if n.Add(1) == 1 && once {
			r.rt.setOperatorIn("fx-writer")
			return errors.New("fatal: Unable to create index.lock: File exists")
		}
		if once {
			return nil
		}
		return errors.New("fatal: Unable to create index.lock: File exists")
	})
	return broken
}

// noTreeForTheWriter fails the test when the writer got more than its first
// prompt, or one that hands it the copy's tree.
func (r *codingRig) noTreeForTheWriter() {
	r.t.Helper()
	ps := r.rt.promptsTo("fx-writer")
	for _, p := range ps {
		if strings.Contains(p.Text, "README") || strings.Contains(p.Text, "git status shows") {
			r.t.Fatalf("the writer was handed the copy's tree: %.300q", p.Text)
		}
	}
	if len(ps) != 1 {
		r.t.Fatalf("the writer got %d prompts, want its first only", len(ps))
	}
}

// A put-back that fails never hands the tree to the writer, whatever ends
// the check after it: what the precheck then keeps (you came into the pane)
// is its result, and the task needs you naming it; a put-back that fails
// every time leaves its changes recorded (kept.json), nothing reads the tree
// as the writer's change while they are there, and the task needs you
// naming them and the failure. Once the put-back works and you let the task
// go on, they go back under the guard and the stage goes on.
func TestAFailedPutBackNeverHandsTheTreeToTheWriter(t *testing.T) {
	for _, tc := range []struct {
		name, says string
		once       bool
	}{{"it fails, then you come into the pane", "fx-writer pane", true}, {"it fails every time", "index.lock", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			r.redOnMaster()
			task := r.codingTask("PR-12414", "bug")
			r.startStage(task)
			r.fix(task)
			r.setDuring(func(dir string, run scriptRun) {
				if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" && strings.Contains(run.Script, "app/credit.rb") {
					r.write(dir, "README", "written during the head spec run\n")
				}
			})
			broken := r.failRestores(tc.once)
			r.endTurns(task, 1)
			r.rt.setOperatorIn("")
			for i := 0; i < 4 && isLive(r.reload(task.ID)); i++ {
				r.clock.Add(time.Minute)
				r.tick()
				r.e.waitBackground()
			}
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") || !strings.Contains(got.WaitsWhat, tc.says) {
				t.Fatalf("task %s waits %q; want needs_you naming README and %q", got.State, got.WaitsWhat, tc.says)
			}
			r.noTreeForTheWriter()
			if s := r.read("README"); s != "written during the head spec run\n" {
				t.Fatalf("README = %q, want it as the spec run left it", s)
			}

			broken.Store(false)
			r.setDuring(nil)
			r.letGoOn(task.ID)
			for i := 0; i < 4 && r.reload(task.ID).Stage == "coding" && isLive(r.reload(task.ID)); i++ {
				r.clock.Add(time.Minute)
				r.tick()
				r.e.waitBackground()
			}
			if s := r.read("README"); s != "shop\n" {
				t.Fatalf("README = %q after you let the task go on, want it put back", s)
			}
			if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
				t.Fatalf("task needs you (%q) after the put-back", got.WaitsWhat)
			}
			r.noTreeForTheWriter()
		})
	}
}

// The proof's end follows the same rule: a head run that did not run to its
// end (a start failure of the moment) while you came into the pane leaves
// what it changed for you, and that is the proof's result, for you, not its
// failure to run.
func TestAProofsKeptChangeWinsOverItsFailureToRun(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12415", "chore")
	r.commit("PR-12415 rename", map[string]string{"app/credit.rb": creditRB + "# renamed\n"})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_spec.rb"]}`)
	r.mu.Lock()
	r.startErr = func(run scriptRun) error {
		if !strings.HasPrefix(run.Script, "bin/specs") {
			return nil
		}
		r.write(r.copyDir, "README", "written as the run started\n")
		r.rt.setOperatorIn("fx-writer")
		return syscall.EAGAIN
	}
	r.mu.Unlock()
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil || p.Passed || !p.ForYou || !strings.Contains(p.Why, "README") {
		t.Fatalf("proof %+v, %v; want it failed for you, naming README", p, err)
	}
	if s := r.read("README"); s != "written as the run started\n" {
		t.Fatalf("README = %q, want it kept", s)
	}
	if !r.e.keptRecorded(task.ID) {
		t.Fatal("no record of the change kept for you")
	}
}

// What the factory keeps for you is never handed on because its record
// failed: an unreadable kept.json makes the task wait on you, saying so,
// before anything runs; a kept.json the factory could not write keeps the
// task waiting on you, saying so, until it can, and then the record is
// settled as any other.
func TestAKeptRecordThatCannotBeReadOrWrittenWaitsOnYou(t *testing.T) {
	t.Run("an unreadable kept.json", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.codingTask("PR-12416", "bug")
		r.write(r.copyDir, "README", "left by a check\n")
		r.taskFile(task.ID, keptFile, "{not json")
		r.startStage(task)
		got := r.reload(task.ID)
		if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, keptFile) {
			t.Fatalf("task %s waits %q; want needs_you naming %s", got.State, got.WaitsWhat, keptFile)
		}
		if n := len(r.rt.promptsTo("fx-writer")); n != 0 {
			t.Fatalf("the writer got %d prompts beside an unreadable %s", n, keptFile)
		}
	})
	t.Run("a kept.json the factory could not write", func(t *testing.T) {
		r := newCodingRig(t)
		r.redOnMaster()
		var broken atomic.Bool
		broken.Store(true)
		r.e.keptWrite = func(path string, b []byte) error {
			if broken.Load() {
				return errors.New("no space left on device")
			}
			return fsx.WriteFileAtomic(path, b, 0o600)
		}
		task := r.codingTask("PR-12417", "bug")
		r.startStage(task)
		r.fix(task)
		r.setDuring(func(dir string, run scriptRun) {
			if strings.HasPrefix(run.Script, "bin/specs") && run.Branch != "" && strings.Contains(run.Script, "app/credit.rb") {
				r.write(dir, "README", "written during the head spec run\n")
				r.rt.setOperatorIn("fx-writer")
			}
		})
		r.endTurns(task, 1)
		if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") {
			t.Fatalf("task %s waits %q; want needs_you naming README", got.State, got.WaitsWhat)
		}
		r.setDuring(nil)
		r.rt.setOperatorIn("")
		r.letGoOn(task.ID)
		for range 3 {
			r.clock.Add(time.Minute)
			r.tick()
			r.e.waitBackground()
		}
		got := r.reload(task.ID)
		if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") || !strings.Contains(got.WaitsWhat, "no space left") {
			t.Fatalf("task %s waits %q; want needs_you naming README and why its record failed", got.State, got.WaitsWhat)
		}
		r.noTreeForTheWriter()
		if s := r.read("README"); s != "written during the head spec run\n" {
			t.Fatalf("README = %q, want it kept", s)
		}

		broken.Store(false)
		r.letGoOn(task.ID)
		for i := 0; i < 4 && r.reload(task.ID).Stage == "coding" && isLive(r.reload(task.ID)); i++ {
			r.clock.Add(time.Minute)
			r.tick()
			r.e.waitBackground()
		}
		if s := r.read("README"); s != "shop\n" {
			t.Fatalf("README = %q once the record could be written, want it put back", s)
		}
		r.noTreeForTheWriter()
	})
}
