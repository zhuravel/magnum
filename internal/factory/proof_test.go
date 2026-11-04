package factory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

const pointsSpec = "spec/models/credit_points_spec.rb"

// onBranch fails the test unless the copy is on branch at head with no
// tracked change and no base run in progress.
func (r *codingRig) onBranch(branch, head string) {
	r.t.Helper()
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != branch {
		r.t.Fatalf("the copy is on %q, want %s", b, branch)
	}
	if got := r.git(r.copyDir, "rev-parse", "HEAD"); got != head {
		r.t.Fatalf("the copy's HEAD is %s, want %s", got, head)
	}
	if st := r.git(r.copyDir, "status", "--porcelain", "--untracked-files=no"); st != "" {
		r.t.Fatalf("the copy has tracked changes:\n%s", st)
	}
}

// The factory's own proof: the writer's spec, laid on a detached checkout
// of the base, fails there, and passes at the head; the commands, the exits,
// the output and the trees are recorded, as two checks and the proof
// artifact, and the copy is back on its branch.
func TestProofFailsOnTheBaseAndPassesAtTheHead(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12353", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)

	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil {
		t.Fatalf("runProof: %v", err)
	}
	if !p.Passed || p.HeadSHA != head || p.BaseSHA != base || !slices.Equal(p.Specs, []string{pointsSpec}) || !p.Holds(head) {
		t.Fatalf("proof %+v; want passed at %s from %s", p, head, base)
	}
	if p.Base.Exit == 0 || p.Head.Exit != 0 || !strings.Contains(p.Base.Command, "bin/specs "+pointsSpec) ||
		!strings.Contains(p.Base.Output, "rspec ./"+pointsSpec) || p.Head.Command != p.Base.Command {
		t.Fatalf("runs base %+v, head %+v", p.Base, p.Head)
	}
	if p.Base.Tree != r.git(r.copyDir, "rev-parse", base+"^{tree}") || p.Head.Tree != r.git(r.copyDir, "rev-parse", head+"^{tree}") {
		t.Fatalf("trees base %s, head %s", p.Base.Tree, p.Head.Tree)
	}
	runs := r.ran("bin/specs")
	if len(runs) != 2 {
		t.Fatalf("spec runs %+v, want the base's and the head's", runs)
	}
	if runs[0].Branch != "" || runs[0].Head != base || runs[0].Specs[pointsSpec] != "expect: 10 in app/credit.rb\n" {
		t.Fatalf("the base run saw %+v; want a detached %s with the head's spec", runs[0], base)
	}
	if runs[1].Branch != task.Branch || runs[1].Head != head {
		t.Fatalf("the head run saw %+v; want %s at %s", runs[1], task.Branch, head)
	}
	checks, err := r.reg.Checks(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 || checks[0].Kind != "spec" || checks[0].Source != "factory" || checks[0].Result != "fail" || checks[0].HeadSHA != base ||
		checks[1].Result != "pass" || checks[1].HeadSHA != head || checks[1].Command == "" {
		t.Fatalf("checks %+v; want the base's red run and the head's green one", checks)
	}
	var saved Proof
	if err := json.Unmarshal([]byte(r.readTask(task.ID, factoryProofFile)), &saved); err != nil || saved.HeadSHA != head || !saved.Passed {
		t.Fatalf("%s = %+v, %v", factoryProofFile, saved, err)
	}
	if as := r.artifacts(task.ID, "proof"); len(as) != 1 || as[0].Path != factoryProofFile {
		t.Fatalf("proof artifacts %+v", as)
	}
	r.onBranch(task.Branch, head)
}

// A spec that already passes on the base proves nothing about the change.
func TestASpecThatPassesOnTheBaseProvesNothing(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12354", "feature")
	head := r.commit("PR-12354 points", map[string]string{
		"app/credit.rb":                     strings.Replace(creditRB, "    0\n", "    10\n", 1),
		"spec/models/credit_points_spec.rb": "expect: def points in app/credit.rb\n",
	})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if p.Passed || p.Holds(head) || !strings.Contains(p.Why, "passes on the base") || !strings.Contains(p.Why, pointsSpec) {
		t.Fatalf("proof %+v; want it to fail: the spec passes on the base", p)
	}
	r.onBranch(task.Branch, head)

	// A bug or a feature that names no spec proves nothing either.
	r.taskFile(task.ID, proofFile, `{"specs": []}`)
	if p, err := r.e.runProof(r.ctx, task, r.cp); err != nil || p.Passed || !strings.Contains(p.Why, "names no spec") {
		t.Fatalf("a feature without a spec: %+v, %v", p, err)
	}
}

// A chore's specs stay green: its proof runs them at the head only (the
// kind's proof is "green"), and a chore that names none has nothing to run.
func TestProofNeedsNoRedRunForAChore(t *testing.T) {
	r := newCodingRig(t)
	if k := r.cfg.Factory.Kinds["chore"]; k.Proof != config.FactoryProofGreen {
		t.Fatalf("the chore kind's proof is %q, want green", k.Proof)
	}
	task := r.codingTask("PR-12355", "chore")
	head := r.commit("PR-12355 rename", map[string]string{"app/credit.rb": creditRB + "# renamed\n"})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_spec.rb"]}`)
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Passed || p.Base.Command != "" || p.Head.Exit != 0 || len(r.ran("bin/specs")) != 1 {
		t.Fatalf("a chore's proof %+v with spec runs %+v; want the head's green run only", p, r.ran("bin/specs"))
	}
	if checks, _ := r.reg.Checks(r.ctx, task.ID); len(checks) != 1 || checks[0].Result != "pass" {
		t.Fatalf("checks %+v, want one green run", checks)
	}
	r.taskFile(task.ID, proofFile, `{"specs": []}`)
	if p, err := r.e.runProof(r.ctx, task, r.cp); err != nil || !p.Passed || p.Head.Command != "" {
		t.Fatalf("a chore without specs: %+v, %v", p, err)
	}
	r.onBranch(task.Branch, head)
}

// The proof is of one head: a commit after it voids it, and the coding
// stage runs the proof again at the new head before it passes.
func TestANewHeadVoidsTheProof(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12356", "bug")
	r.startStage(task)
	first := r.fix(task)
	r.ended(task, "fx-writer")
	r.tick()
	r.e.waitBackground()

	second := r.commit("PR-12356 a word more", map[string]string{"README": "shop, with points\n"}) // after the proof ran
	r.tick()
	if got := r.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task at %s on a proof of %s while the head is %s; want coding", got.Stage, first, second)
	}
	r.e.waitBackground()
	r.tick()
	got := r.reload(task.ID)
	var p Proof
	if err := json.Unmarshal([]byte(r.readTask(task.ID, factoryProofFile)), &p); err != nil {
		t.Fatal(err)
	}
	if got.Stage != "local_review" || got.HeadSHA != second || p.HeadSHA != second || !p.Holds(second) || p.Holds(first) {
		t.Fatalf("task at %s, head %s, proof of %s; want local_review on a proof of %s", got.Stage, got.HeadSHA, p.HeadSHA, second)
	}
	if n := len(r.ran("bin/specs")); n < 4 {
		t.Fatalf("%d spec runs, want the proof run twice", n)
	}
}

// The base run detaches the copy; the branch and the head's tree come back
// after it, also when the run is stopped in the middle (a drain).
func TestProofRestoresTheBranchAfterTheBaseRun(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12357", "bug")
	head := r.fix(task)
	if _, err := r.e.runProof(r.ctx, task, r.cp); err != nil {
		t.Fatal(err)
	}
	r.onBranch(task.Branch, head)
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), detachedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is left: %v", detachedFile, err)
	}
	phases := phasesOf(r.events(task.ID, "proof"))
	if !slices.Equal(phases, []string{"begin", "ok"}) {
		t.Fatalf("proof events %q, want the base run's begin and ok", phases)
	}

	// Stopped during the base run: the run ends with its context, and the
	// branch is restored all the same.
	block := make(chan struct{})
	r.blocks["bin/specs"] = block
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() {
		_, err := r.e.runProof(ctx, task, r.cp)
		done <- err
	}()
	<-r.entered
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != "" {
		t.Fatalf("during the base run the copy is on %q, want detached", b)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a proof stopped in its base run passed")
	}
	r.onBranch(task.Branch, head)
	if phases := phasesOf(r.events(task.ID, "proof")); !slices.Equal(phases, []string{"begin", "ok", "begin", "fail"}) {
		t.Fatalf("proof events %q, want the stopped run's begin and fail", phases)
	}
}

// A daemon that dies in the base run leaves the copy detached with the
// head's spec files on the base: the next start restores the branch and the
// head's tree before anything else runs in the copy, never while an agent
// works there, and never over a change the base run did not make.
func TestABaseRunCutShortByACrashRestoresTheBranchFirst(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12358", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)
	// What the base run does before its specs run, and no restore after it.
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != "" || r.read(pointsSpec) != "expect: 10 in app/credit.rb\n" {
		t.Fatalf("the base run's checkout: branch %q, spec %q", b, r.read(pointsSpec))
	}

	r.restart()
	r.rt.setAtWork("fx-writer") // an agent at work in the copy: nothing touches it
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != "" {
		t.Fatalf("the copy was restored to %q under an agent at work", b)
	}
	if n := len(r.rt.allPrompts()); n != 0 {
		t.Fatalf("%d prompts went out while the copy was detached", n)
	}
	if got := r.reload(task.ID); got.WaitsWho != waitCopy || !strings.Contains(got.WaitsWhat, "base run") {
		t.Fatalf("task waits %q: %q, want the copy's restore", got.WaitsWho, got.WaitsWhat)
	}

	r.rt.setAtWork("")
	r.tick()
	r.onBranch(task.Branch, head)
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), detachedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is left after the restore: %v", detachedFile, err)
	}
	evs := r.events(task.ID, "proof")
	if last := evs[len(evs)-1]; last.Phase != registry.PhaseOK || !strings.Contains(last.Text, "restored") {
		t.Fatalf("proof events %+v, want the restore's ok last", evs)
	}

	// A change the base run did not make (yours, in the detached copy) is
	// never discarded: the task waits on you.
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	r.write(r.copyDir, "README", "mine\n")
	r.restart()
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.read("README"); got != "mine\n" {
		t.Fatalf("your change went: README %q", got)
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") {
		t.Fatalf("task %s waits %q, want needs_you naming README", got.State, got.WaitsWhat)
	}
}

// The proof runs on what the branch holds: a tracked change not committed
// is refused before anything moves.
func TestProofRefusesAnUncommittedChange(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12359", "bug")
	head := r.fix(task)
	r.write(r.copyDir, "app/credit.rb", "class Credit; end # not committed\n")
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if p.Passed || !strings.Contains(p.Why, "tracked change") || !strings.Contains(p.Why, "app/credit.rb") {
		t.Fatalf("proof %+v, want it refused for the uncommitted app/credit.rb", p)
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs, want none", n)
	}
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != task.Branch || r.read("app/credit.rb") != "class Credit; end # not committed\n" {
		t.Fatalf("the refused proof moved the copy (%q) or its change", b)
	}
	if got := r.git(r.copyDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %s", got)
	}
}

// The proof never touches a copy an agent works in: it waits, at no cost.
func TestProofWaitsWhileAnAgentWorksInTheCopy(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12360", "bug")
	head := r.fix(task)
	r.rt.setAtWork("fx-writer")
	if _, err := r.e.runProof(r.ctx, task, r.cp); !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "fx-writer") {
		t.Fatalf("runProof under an agent at work = %v, want a wait naming it", err)
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs, want none", n)
	}
	r.onBranch(task.Branch, head)
}

// phasesOf are the phases of evs, in order.
func phasesOf(evs []registry.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Phase)
	}
	return out
}
