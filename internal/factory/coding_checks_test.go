package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// setHang makes the copy's scripts that hang names never end on their own.
func (r *codingRig) setHang(hang func(run scriptRun) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hang = hang
}

func (r *codingRig) setFailGit(fail func(args []string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failGit = fail
}

// endTurns ends the writer's last run and runs the passes that take its
// end, n times or until the task leaves the live states.
func (r *codingRig) endTurns(task registry.Task, n int) {
	r.t.Helper()
	for range n {
		if got := r.reload(task.ID); !isLive(got) {
			return
		}
		r.clock.Add(time.Minute)
		r.ended(task, "fx-writer")
		r.tick()
		r.e.waitBackground()
		r.tick()
	}
}

// A spec that never ends on its own is a result of the proof, not a
// failure to run it: on the base it counts as red (a bug whose symptom is a
// hang hangs there), at the head the proof fails, and the writer gets a fix
// turn naming it; nothing is run again and again.
func TestAHangingSpecIsAResultNotARetry(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Precheck.Cap = config.Duration{Duration: 300 * time.Millisecond}
	task := r.codingTask("PR-12368", "bug")
	r.startStage(task)
	head := r.fix(task)
	r.setHang(func(run scriptRun) bool { return strings.HasPrefix(run.Script, "bin/specs") && run.Branch == "" }) // on the base
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil || !p.Passed || p.Base.Exit == 0 || !p.Base.Hung || !strings.Contains(p.Base.Output, "did not end within") {
		t.Fatalf("a spec that hangs on the base: %+v, %v; want a red base run and a proof", p, err)
	}
	r.onBranch(task.Branch, head)

	r.setHang(func(run scriptRun) bool { return strings.HasPrefix(run.Script, "bin/specs") }) // at the head too
	r.endTurns(task, 1)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "does not end at the head") ||
		!strings.Contains(ps[1].Text, factoryProofFile) {
		t.Fatalf("the writer's prompts %+v; want a fix turn for the spec that hangs at the head", ps)
	}
	if got := r.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho == waitPrecheck {
		t.Fatalf("task %s waits %q: %q; want active, at the writer's fix turn", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if n := len(r.events(task.ID, "check")); n != 2 {
		t.Fatalf("%d check events, want one check begun and ended", n)
	}
}

// A check that does not run to its end (git fails in it) runs again after
// a minute, 3 times at most; then the task needs you with the reason, and
// its heavy slot and the spec lock are free.
func TestAChecksRunThatFailsThreeTimesNeedsYou(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12369", "bug")
	r.startStage(task)
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	r.fix(task)
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "--detach") && slices.Contains(args, base) {
			return errors.New("fatal: cannot lock ref 'HEAD'")
		}
		return nil
	})
	r.ended(task, "fx-writer")
	for i := 0; i < 12 && isLive(r.reload(task.ID)); i++ {
		r.tick()
		r.e.waitBackground()
		r.clock.Add(2 * time.Minute)
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "did not run to their end 3 times") ||
		!strings.Contains(got.WaitsWhat, "cannot lock ref") {
		t.Fatalf("task %s waits %q; want needs_you after 3 checks that did not run to their end", got.State, got.WaitsWhat)
	}
	var begins int
	for _, ev := range r.events(task.ID, "check") {
		if ev.Phase == registry.PhaseBegin {
			begins++
		}
	}
	if begins != 3 || len(r.rt.promptsTo("fx-writer")) != 1 {
		t.Fatalf("%d checks begun, %d prompts; want 3 checks and no fix turn", begins, len(r.rt.promptsTo("fx-writer")))
	}
	r.e.heavyMu.Lock()
	heavy := len(r.e.heavyHeld)
	r.e.heavyMu.Unlock()
	if heavy != 0 {
		t.Fatalf("%d heavy slots held after the task went to you", heavy)
	}
	r.onBranch(task.Branch, r.git(r.copyDir, "rev-parse", task.Branch))
}

// A change the factory's checks refuse gets fix turns, each naming what
// failed and the file of the runs, up to the stage's prompt cap
// (runtime.MaxStagePrompts); then the task needs you with the last failure.
func TestAChangeThatFailsTheChecksGetsFixTurnsUpToTheStagesCap(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12370", "bug")
	r.lint = func([]string) string { return "app/credit.rb:3:5: C: Style/NumericLiterals: on the changed line\n" }
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, 2*runtime.MaxStagePrompts)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != runtime.MaxStagePrompts {
		t.Fatalf("the writer got %d prompts, want %d: its first and the fix turns", len(ps), runtime.MaxStagePrompts)
	}
	report := filepath.Join(r.layout.FactoryTaskDir(task.ID), precheckFile)
	for _, p := range ps[1:] {
		if !strings.HasPrefix(p.Text, fixLead) || !strings.Contains(p.Text, report) || !strings.Contains(p.Text, "lint: 1 offense") {
			t.Fatalf("a fix turn %q; want what failed and %s", p.Text, report)
		}
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "does not pass after 5 fix turns") ||
		!strings.Contains(got.WaitsWhat, "lint: 1 offense on the changed lines") {
		t.Fatalf("task %s waits %q; want needs_you naming the last failure", got.State, got.WaitsWhat)
	}
}

// A fix turn for a change the precheck refused names the command that
// failed, as it ran (the failing check's command, which precheck.md shows),
// and asks the writer to run it until it passes before it commits: two fix
// turns in a row ended in "jest: pnpm exited 1" when the text only said
// "fix it", at about 17 minutes per precheck run.
func TestAFixTurnNamesTheCommandThatFailed(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12392", "bug")
	r.lint = func([]string) string { return "app/credit.rb:3:5: C: Style/NumericLiterals: on the changed line\n" }
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, 1)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) {
		t.Fatalf("the writer's prompts %+v, want a fix turn", ps)
	}
	checks, err := r.reg.Checks(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(checks, func(c registry.Check) bool { return c.Kind == registry.CheckLint && c.Result == registry.CheckFail })
	if i < 0 || !strings.HasPrefix(checks[i].Command, "bin/lint ") {
		t.Fatalf("checks %+v, want the lint's failure", checks)
	}
	for _, want := range []string{"`" + checks[i].Command + "`", "until it passes", precheckFile} {
		if !strings.Contains(ps[1].Text, want) {
			t.Errorf("the fix turn %q lacks %q", ps[1].Text, want)
		}
	}
	if strings.Index(ps[1].Text, "until it passes") > strings.Index(ps[1].Text, "commit it") {
		t.Errorf("the fix turn %q asks for the commit before the run that passes", ps[1].Text)
	}
}

// A fix turn whose failure names no command that ran (a proof.json naming
// a spec the head lacks) keeps its words: no command to run. A long
// command is clipped: precheck.md has it whole.
func TestAFixTurnWithoutAFailedCommandNamesNone(t *testing.T) {
	p := &endProblem{file: factoryProofFile, why: "the factory's proof: proof.json names spec/x_spec.rb, which the head does not have", change: true}
	if text := fixText("/state/factory/tasks/7", p); strings.Contains(text, "until it passes") || strings.Contains(text, "``") {
		t.Fatalf("fix text %q names a command", text)
	}
	p.file, p.command = precheckFile, strings.Repeat("bin/jest 'app/javascript/a.js' ", 40)
	text := fixText("/state/factory/tasks/7", p)
	if !strings.Contains(text, "until it passes") || len(text) > 1200 {
		t.Fatalf("fix text of a long command (%d bytes): %q", len(text), text)
	}
}

// A proof.json that is missing or malformed keeps its one nudge (Review
// Focus 4): then the task needs you, naming the file.
func TestAMalformedProofFileGetsOneNudgeThenNeedsYou(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12371", "bug")
	r.startStage(task)
	r.fix(task)
	r.taskFile(task.ID, proofFile, `{"specs": "spec/models/credit_points_spec.rb"}`)
	r.endTurns(task, 3)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, nudgeLead) || !strings.Contains(ps[1].Text, proofFile) {
		t.Fatalf("the writer's prompts %+v; want one nudge about proof.json", ps)
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || got.WaitsWhat != "the fx-writer agent wrote no valid proof.json" {
		t.Fatalf("task %s waits %q; want needs_you: the fx-writer agent wrote no valid proof.json", got.State, got.WaitsWhat)
	}
}

// What the writer cannot fix goes to you at once, naming the configuration
// key: no fix turn is spent on it.
func TestAConfigurationProblemGoesStraightToYouNamingTheKey(t *testing.T) {
	cases := []struct {
		name string
		edit func(r *codingRig)
		want []string
	}{
		{"no specs command", func(r *codingRig) { r.cfg.Factory.Repos[0].Precheck.Specs = "" }, []string{"precheck.specs"}},
		{"a denied specs command", func(r *codingRig) { r.cfg.Factory.Repos[0].Precheck.Specs = "bin/remote-console {files}" },
			[]string{"precheck.specs", "remote console"}},
		{"a denied lint", func(r *codingRig) {
			r.cfg.Factory.Repos[0].Precheck.Lint = []string{"kubectl exec web-1 -- bin/lint {files}"}
		},
			[]string{"precheck.lint", "kubectl exec"}},
		{"lint output not by path:line", func(r *codingRig) { r.lint = func([]string) string { return "something is wrong\n" } },
			[]string{"precheck.lint", "path:line"}},
		{"a missing table_stats", func(r *codingRig) {
			r.cfg.Factory.Repos[0].TableStats, r.cfg.Factory.Repos[0].LargeTableRows = "config/table_stats.json", 1000
		}, []string{"table_stats", "config/table_stats.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			tc.edit(r)
			task := r.codingTask("PR-12372", "bug")
			r.startStage(task)
			r.fix(task)
			r.commit("PR-12372 a column", map[string]string{"db/migrate/20261104090000_add_x_to_notes.rb": "class AddXToNotes < " +
				"ActiveRecord::Migration[7.1]\n  def change\n    add_column :notes, :x, :integer\n  end\nend\n"})
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

// The factory never detaches a copy, nor restores one, while you are in a
// pane of the task: your edits there would be discarded. It waits, saying
// so.
func TestTheFactoryNeverTouchesACopyWhileYouAreInItsPane(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12373", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)
	r.copies.reserve(task.ID, "factory1")
	r.rt.setOperatorIn("fx-writer")
	if _, err := r.e.runProof(r.ctx, task, r.cp); !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "you're in the fx-writer pane") {
		t.Fatalf("runProof with you in the pane = %v, want a wait saying so", err)
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs while you were in the pane", n)
	}
	r.onBranch(task.Branch, head)

	r.rt.setOperatorIn("")
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.rt.setOperatorIn("fx-writer")
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if b := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); b != "" {
		t.Fatalf("the copy was restored to %q while you were in the pane", b)
	}
	if got := r.reload(task.ID); !strings.Contains(got.WaitsWhat, "you're in the fx-writer pane") {
		t.Fatalf("task waits %q: %q, want you named", got.WaitsWho, got.WaitsWhat)
	}
	r.rt.setOperatorIn("")
	r.tick()
	r.onBranch(task.Branch, head)
}

// A base run's record whose copy you put back on its branch yourself is
// dropped: nothing is switched or discarded again, and the NEEDS YOU that
// asked you to do it does not come back.
func TestARecordOfABaseRunIsDroppedOnceTheCopyIsBackOnItsBranch(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12374", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(task)
	r.copies.reserve(task.ID, "factory1")
	if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	r.write(r.copyDir, "README", "mine\n") // your change in the detached copy
	r.restart()
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "README") || !strings.Contains(got.WaitsWhat, pointsSpec) {
		t.Fatalf("task %s waits %q; want needs_you naming README and the laid spec", got.State, got.WaitsWhat)
	}

	// You keep your change and put the copy back, as the wait says.
	if err := os.Remove(filepath.Join(r.copyDir, pointsSpec)); err != nil {
		t.Fatal(err)
	}
	r.git(r.copyDir, "switch", "--quiet", task.Branch)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
		t.Fatalf("the task needs you again: %q", got.WaitsWhat)
	}
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), detachedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is left: %v", detachedFile, err)
	}
	if r.read("README") != "mine\n" || r.git(r.copyDir, "rev-parse", "HEAD") != head {
		t.Fatalf("your change or the branch moved: README %q", r.read("README"))
	}
	evs := r.events(task.ID, "proof")
	if last := evs[len(evs)-1]; last.Phase != registry.PhaseOK || !strings.Contains(last.Text, "back on "+task.Branch) {
		t.Fatalf("proof events %+v, want the record dropped with an ok", evs)
	}
}

// A stale record never switches a copy the task no longer holds: another
// task claimed it since, and works on its own branch there.
func TestARecordOfABaseRunNeverSwitchesAnotherTasksCopy(t *testing.T) {
	r := newCodingRig(t)
	a := r.codingTask("PR-12375", "bug")
	base := r.git(r.copyDir, "rev-parse", "HEAD")
	head := r.fix(a)
	r.copies.reserve(a.ID, "factory1")
	if err := r.e.detachForBase(r.ctx, a, r.cp, base, head, []string{pointsSpec}); err != nil {
		t.Fatal(err)
	}
	// a is stopped and its copy released; b claims it and works on its branch.
	if err := r.reg.TransitionTask(r.ctx, a.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.copies.mu.Lock()
	delete(r.copies.reserved, a.ID)
	r.copies.mu.Unlock()
	b := r.kindTask("PR-12376", "feature", "coding")
	if err := os.Remove(filepath.Join(r.copyDir, pointsSpec)); err != nil {
		t.Fatal(err)
	}
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", b.Branch, "origin/master")
	r.copies.reserve(b.ID, "factory1")

	r.restart()
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	if br := r.gitOut(r.copyDir, "symbolic-ref", "--quiet", "--short", "HEAD"); br != b.Branch {
		t.Fatalf("the copy is on %q, want %s's branch left alone", br, b.Branch)
	}
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(a.ID), detachedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s of the stopped task is left: %v", detachedFile, err)
	}
	evs := r.events(a.ID, "proof")
	if last := evs[len(evs)-1]; last.Phase != registry.PhaseInfo || !strings.Contains(last.Text, "no longer holds") {
		t.Fatalf("proof events %+v, want the record dropped with an info", evs)
	}
}

// The precheck leaves the copy as it found it: what its commands changed in
// tracked files (Rails dumps db/schema.rb after each migration) is restored
// and said, so the writer is never asked to commit the factory's leftover,
// and the stage passes.
func TestThePrecheckRestoresWhatItsCommandsChanged(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Precheck.Migrations = []string{"bin/rails db:migrate:up VERSION={version}",
		"bin/rails db:migrate:down VERSION={version}", "bin/rails db:migrate:up VERSION={version}"}
	r.migrate = func(dir, script string) int {
		dump := "version: none\n"
		if _, v, ok := strings.Cut(script, "VERSION="); ok && strings.Contains(script, ":up") {
			dump = "version: " + v + " (dumped)\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "db", "schema.rb"), []byte(dump), 0o644); err != nil {
			t.Error(err)
		}
		return 0
	}
	task := r.codingTask("PR-12377", "feature")
	r.startStage(task)
	r.fix(task)
	const schema = "version: 20261104090000\n"
	head := r.commit("PR-12377 a column", map[string]string{
		"db/migrate/20261104090000_add_x_to_notes.rb": "class AddXToNotes < ActiveRecord::Migration[7.1]\n  def change\n" +
			"    add_column :notes, :x, :integer\n  end\nend\n",
		"db/schema.rb": schema,
	})
	r.endTurns(task, 1)
	if got := r.reload(task.ID); got.Stage != "local_review" || len(r.rt.promptsTo("fx-writer")) != 1 {
		t.Fatalf("task %s at %s waits %q, %d prompts; want local_review on the first turn", got.State, got.Stage, got.WaitsWhat,
			len(r.rt.promptsTo("fx-writer")))
	}
	r.onBranch(task.Branch, head)
	if got := r.read("db/schema.rb"); got != schema {
		t.Fatalf("db/schema.rb = %q, want the committed one back", got)
	}
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, "Restored db/schema.rb") {
		t.Fatalf("%s does not say what it restored:\n%s", precheckFile, report)
	}
	if !slices.ContainsFunc(r.events(task.ID, "check"), func(e registry.Event) bool { return strings.Contains(e.Text, "db/schema.rb") }) {
		t.Fatalf("no check event names the restore of db/schema.rb: %+v", r.events(task.ID, "check"))
	}
}

// A NEEDS YOU whose cause is the configuration's or the machine's names
// the fix, then the command that lets the task go on once it is fixed:
// task 5 waited 127 minutes on a line that named neither.
func TestANeedsYouForTheChecksNamesTheFixAndTheRetry(t *testing.T) {
	cases := []struct {
		name string
		edit func(r *codingRig)
		want []string
	}{
		{"a missing lint", func(r *codingRig) { r.cfg.Factory.Repos[0].Precheck.Lint = []string{"bin/nolint {files}"} },
			[]string{"exited 127", "fix: make `bin/nolint` runnable", "`magnum factory install`"}},
		{"a denied lint", func(r *codingRig) {
			r.cfg.Factory.Repos[0].Precheck.Lint = []string{"kubectl exec web-1 -- bin/lint {files}"}
		}, []string{"denied", "fix: change precheck.lint", "`magnum factory install`"}},
		{"a lint that crashed", func(r *codingRig) { r.lintCrash = func([]string) string { return "Error: boom\n" } },
			[]string{"its stderr ends: Error: boom"}},
		{"a lint that printed no offense", func(r *codingRig) { r.lint = func([]string) string { return "something is wrong\n" } },
			[]string{"names no offense by path:line", "fix: have it print them so"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			tc.edit(r)
			task := r.codingTask("PR-12382", "bug")
			r.startStage(task)
			r.fix(task)
			r.endTurns(task, 2)
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou {
				t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
			}
			for _, w := range append(tc.want, fmt.Sprintf("; then `magnum factory retry %d`", got.ID)) {
				if !strings.Contains(got.WaitsWhat, w) {
					t.Errorf("the wait %q does not name %q", got.WaitsWhat, w)
				}
			}
		})
	}
}

// setLintCrash makes bin/lint crash with stderr, or, "", run as it does.
func (r *codingRig) setLintCrash(stderr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lintCrash = nil
	if stderr != "" {
		r.lintCrash = func([]string) string { return stderr }
	}
}

// A lint that crashed on its environment (rubocop without a UTF-8 locale)
// is neither the writer's to fix nor yours to retry: the infrastructure
// hold takes it, at no cost to the task, naming the line and the fix, and
// its next try runs the checks again; the first try after the fix lifts it.
func TestALintCrashOfTheEnvironmentGoesToTheInfraHold(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12383", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("invalid byte sequence in US-ASCII\n/gems/rubocop/lib/rubocop/cop/team.rb:12:in 'match?'\n")
	r.ended(task, "fx-writer")
	r.tick() // the end: the checks start off the loop
	r.e.waitBackground()
	r.tick() // their crash

	hd, held := r.e.holds[HoldInfra]
	if !held || !strings.Contains(hd.Reason, "invalid byte sequence in US-ASCII") || !strings.Contains(hd.Reason, "LANG") {
		t.Fatalf("hold %+v (%v), want an infrastructure hold naming the crash and the fix", hd, held)
	}
	if got := r.reload(task.ID); got.State != registry.TaskActive || !strings.Contains(got.WaitsWhat, "a UTF-8 locale") {
		t.Fatalf("task %s waits %q; want active, waiting on the hold", got.State, got.WaitsWhat)
	}
	if got := r.checksOf(task.ID, "lint"); !slices.Contains(got, "fail/env") {
		t.Fatalf("lint checks %q, want one failed on the environment", got)
	}

	r.setLintCrash("") // the locale is fixed
	for range 10 {
		if _, held := r.e.holds[HoldInfra]; !held {
			break
		}
		r.clock.Add(time.Minute)
		r.tick()
		r.e.waitBackground()
		r.tick()
	}
	if hd, held := r.e.holds[HoldInfra]; held {
		t.Fatalf("hold %+v outlived a try whose checks passed", hd)
	}
	if got := r.checksOf(task.ID, "lint"); got[len(got)-1] != "pass" {
		t.Fatalf("lint checks %q, want the last one passed", got)
	}
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
		t.Fatalf("the hold cost the task: it needs you, %q", got.WaitsWhat)
	}
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts, want no fix turn for the environment's crash", n)
	}
}

// A crash of the environment that stays on the same head is not the
// machine's to hold for ever: after its tries (envCrashTries crashes on
// one head) the task needs you, naming the crash, the fix and the retry,
// and the hold lifts, so the other tasks go on.
func TestALocaleCrashThatStaysOnTheSameHeadNeedsYouAndLiftsTheHold(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12388", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("invalid byte sequence in US-ASCII\n")
	r.ended(task, "fx-writer")
	for range 20 {
		if r.reload(task.ID).State == registry.TaskNeedsYou {
			break
		}
		r.clock.Add(time.Minute)
		r.tick()
		r.e.waitBackground()
		r.tick()
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s waits %q after 20 minutes of crashes on one head; want needs_you", got.State, got.WaitsWhat)
	}
	for _, w := range []string{"a UTF-8 locale", "invalid byte sequence in US-ASCII", "LANG", fmt.Sprintf("then `magnum factory retry %d`", got.ID)} {
		if !strings.Contains(got.WaitsWhat, w) {
			t.Errorf("the wait %q does not name %q", got.WaitsWhat, w)
		}
	}
	if n := len(r.ran("bin/lint")); n != envCrashTries {
		t.Errorf("the lint ran %d times, want %d: the crash and its tries", n, envCrashTries)
	}
	r.clock.Add(time.Minute)
	r.tick()
	if hd, held := r.e.holds[HoldInfra]; held {
		t.Fatalf("hold %+v outlived the task that crashed: it holds the other tasks for nothing", hd)
	}
}

// installsRig is the coding rig whose repository installs its gems
// (`bundle install` when Gemfile.lock changes), which a heal runs.
func installsRig(t *testing.T) *codingRig {
	t.Helper()
	r := newCodingRig(t)
	r.cfg.Factory.Repos[0].Installs = []config.FactoryInstall{{Lock: "Gemfile.lock", Run: "bundle install"}}
	return r
}

// A lint crash the branch caused (the writer added a rubocop plugin to
// .rubocop.yml, or a require to a custom cop) holds nothing and needs
// nobody: once `bundle install` in the copy did not end it, the writer gets
// a fix turn naming the crash, and no other task waits.
func TestALintCrashTheBranchCausedGoesToTheWriterNotAHold(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		stderr func(dir string) string
	}{
		// rubocop rescues the LoadError itself (cli.rb) and prints its message,
		// then its backtrace, bare: no " (LoadError)" after the message.
		{"a plugin in .rubocop.yml", map[string]string{".rubocop.yml": "plugins:\n  - rubocop-points\n"}, func(string) string {
			return "cannot load such file -- rubocop-points\n" +
				"/usr/lib/ruby/3.4.0/rubygems.rb:136:in 'Kernel#require'\n" +
				"/gems/rubocop-1.92.1/lib/rubocop/feature_loader.rb:46:in 'RuboCop::FeatureLoader#load'\n"
		}},
		{"a require in a custom cop", map[string]string{"lib/cops/points_cop.rb": "require_relative 'helprs'\n"}, func(dir string) string {
			return "cannot load such file -- " + dir + "/lib/cops/helprs\n" +
				dir + "/lib/cops/points_cop.rb:1:in 'Kernel#require_relative'\n" +
				"/gems/rubocop-1.92.1/lib/rubocop/config_loader_resolver.rb:20:in 'block in resolve_requires'\n"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := installsRig(t)
			task := r.codingTask("PR-12389", "bug")
			r.startStage(task)
			r.fix(task)
			r.commit("PR-12389 the lint's own change", tc.files)
			r.setLintCrash(tc.stderr(r.copyDir))
			r.ended(task, "fx-writer")
			r.tick()
			r.e.waitBackground()
			r.tick()
			r.tick()
			if hd, held := r.e.holds[HoldInfra]; held {
				t.Fatalf("hold %+v: the branch's crash held every task", hd)
			}
			if n := len(r.ran("bundle install")); n != 1 {
				t.Fatalf("bundle install ran %d times, want the one heal before the crash counts", n)
			}
			ps := r.rt.promptsTo("fx-writer")
			if len(ps) != 2 || !strings.Contains(ps[1].Text, "crashed") || !strings.Contains(ps[1].Text, "cannot load such file") {
				t.Fatalf("the writer's prompts %d, the last %q; want a fix turn naming the crash", len(ps), ps[len(ps)-1].Text)
			}
			if got := r.reload(task.ID); got.State != registry.TaskActive {
				t.Fatalf("task %s waits %q; want the writer at work", got.State, got.WaitsWhat)
			}
		})
	}
}

// A gem the copy lacks is the copy's, not the machine's: the precheck runs
// the repository's installs in the copy once and the lint again, and a
// heal that works ends it there, with no hold and nobody asked.
func TestAMissingGemIsHealedInTheCopy(t *testing.T) {
	r := installsRig(t)
	task := r.codingTask("PR-12390", "bug")
	r.fix(task)
	r.lintCrash = func([]string) string {
		if len(r.ran("bundle install")) > 0 {
			return ""
		}
		return "Could not find rubocop-rails-2.30.0 in locally installed gems (Bundler::GemNotFound)\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed {
		t.Fatalf("precheck %+v, %v; want passed once the heal installed the gem", res, err)
	}
	if n := len(r.ran("bundle install")); n != 1 {
		t.Fatalf("bundle install ran %d times, want 1", n)
	}
	if got := r.checksOf(task.ID, "lint"); !slices.Equal(got, []string{"fail/env", "pass"}) {
		t.Fatalf("lint checks %q, want the crash, then the pass after the heal", got)
	}
}

// A gem the heal cannot bring back, in a branch that changed nothing the
// lint loads, needs you with the crash and the fix, and holds no other
// task.
func TestAMissingGemTheHealCannotFixNeedsYouWithoutAHold(t *testing.T) {
	r := installsRig(t)
	task := r.codingTask("PR-12391", "bug")
	r.fix(task)
	r.setLintCrash("Could not find rubocop-rails-2.30.0 in locally installed gems (Bundler::GemNotFound)\n")
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatalf("precheck error %v; want a failure for you, not a hold", err)
	}
	if res.Passed || !res.ForYou || !strings.Contains(res.Why, "a gem") || !strings.Contains(res.Why, "bundle install") {
		t.Fatalf("precheck %+v, want a failure for you naming the gem and the heal it tried", res)
	}
}

// Words of a crash that the lint prints on stdout are the branch's text
// (a script's echo, a source line), never a crash of the environment, and
// never in the ask: only stderr, matched whole, tells one.
func TestAnEnvironmentSignatureOnStdoutIsNoCrash(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12392", "bug")
	r.fix(task)
	r.lint = func([]string) string {
		return "scripts/setup.sh: echo \"yarn: command not found\"\ninvalid byte sequence in US-ASCII\n"
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatalf("precheck error %v; want no crash of the environment", err)
	}
	if !res.ForYou || strings.Contains(res.Why, "yarn") || strings.Contains(res.Why, "US-ASCII") {
		t.Fatalf("precheck %+v; want the ask, naming none of the lint's stdout", res)
	}
	if !strings.Contains(res.Why, "rubocop --format emacs") {
		t.Fatalf("the ask %q; want the format hint, its stderr having said nothing", res.Why)
	}
}

// retryAnswer is the daemon's answer to your retry of task id.
func (r *codingRig) retryAnswer(id int64) string {
	r.t.Helper()
	qid, err := r.reg.EnqueueRequest(r.ctx, ReqRetry, RetryPayload{TaskID: id, Surface: "cli"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.e.handleRequests(r.ctx)
	q, err := r.reg.RequestByID(r.ctx, qid)
	if err != nil || q.State != registry.RequestDone {
		r.t.Fatalf("your retry: %+v (%v)", q, err)
	}
	return q.Result
}

// Three retries of a lint crash ran the same command in the same
// environment at the same head, each sure to fail: a retry that changes
// nothing since the failure it retries says so, with the line the command
// printed, and still lets the task go on; once the command or its
// environment changed, it says nothing of it.
func TestARetryThatChangedNothingSinceTheFailureSaysSo(t *testing.T) {
	r := newCodingRig(t)
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	r.e.getenv = func(k string) string { return env[k] }
	task := r.codingTask("PR-12384", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("Error: boom\n")
	r.endTurns(task, 2)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
	}

	answer := r.retryAnswer(task.ID)
	if !strings.Contains(answer, "goes on") || !strings.Contains(answer, "nothing the factory sees changed since its last failure") ||
		!strings.Contains(answer, "Error: boom") || !strings.Contains(answer, "`magnum factory install`") {
		t.Fatalf("the retry answers %q, want it to go on, say nothing changed, name the line and that an edit needs an install", answer)
	}

	for i := 0; i < 6 && r.reload(task.ID).State != registry.TaskNeedsYou; i++ { // the same failure again
		r.clock.Add(time.Minute)
		r.tick()
		r.e.waitBackground()
		r.tick()
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s waits %q; want needs_you again", got.State, got.WaitsWhat)
	}
	r.e.waitBackground()
	env["LANG"] = "en_US.UTF-8" // you fixed the environment
	if answer := r.retryAnswer(task.ID); strings.Contains(answer, "changed since") {
		t.Fatalf("the retry after the environment changed answers %q", answer)
	}
}

// A retry after a commit at the branch says nothing of nothing changed: the
// head moved, so the checks run on another change.
func TestARetryAfterTheHeadMovedSaysNothingOfNothingChanged(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12393", "bug")
	r.startStage(task)
	r.fix(task)
	r.setLintCrash("Error: boom\n")
	r.endTurns(task, 2)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
	}
	r.commit("PR-12393 your fix of the lint", map[string]string{"bin/lint-notes": "fixed\n"})
	if answer := r.retryAnswer(task.ID); !strings.Contains(answer, "goes on") || strings.Contains(answer, "changed since") {
		t.Fatalf("the retry after a commit answers %q, want it to go on saying nothing of nothing changed", answer)
	}
}

// A heal whose installs failed (rubygems.org out of reach) is no lint that
// still crashed after it: the lint did not run again, and the NEEDS YOU
// says the installs failed, with the line that says why.
func TestAHealWhoseInstallsFailedSaysSo(t *testing.T) {
	r := installsRig(t)
	task := r.codingTask("PR-12394", "bug")
	r.fix(task)
	r.setLintCrash("Could not find rubocop-rails-2.30.0 in locally installed gems (Bundler::GemNotFound)\n")
	r.bundleFails = "Could not reach host index.rubygems.org. Check your network connection and try again.\n"
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatalf("precheck error %v; want a failure for you", err)
	}
	if !res.ForYou || !strings.Contains(res.Why, "the installs failed") || !strings.Contains(res.Why, "Could not reach host") ||
		strings.Contains(res.Why, "still crashed") {
		t.Fatalf("precheck %+v; want a failure for you saying the installs failed and why", res)
	}
	if n := len(r.ran("bin/lint")); n != 1 {
		t.Fatalf("the lint ran %d times, want once: a failed heal runs it no more", n)
	}
}

// A lint that echoes the files it lints to stderr (set -x, a progress
// line) and then misses a command is no crash of the branch: the change
// names the files, not the command. Only a change of the lint's own script
// makes a missing command the writer's.
func TestAMissingCommandIsNotTheBranchsForTheFilesTheLintEchoes(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12395", "bug")
	r.fix(task)
	r.setLintCrash("+ yarn eslint app/credit.rb spec/models/credit_points_spec.rb\nbin/lint: line 3: yarn: command not found\n")
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil {
		t.Fatalf("precheck error %v; want a failure for you", err)
	}
	if !res.ForYou || !strings.Contains(res.Why, "a command") {
		t.Fatalf("precheck %+v; want a failure for you for the missing command, not a fix turn for the writer", res)
	}
}
