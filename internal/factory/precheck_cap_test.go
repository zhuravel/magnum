package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// capStops makes the scripts stop names print printed and hang, and fires
// the precheck's cap as the first of them starts: the cap stops it, never
// its own end. It returns the caps the precheck's timer was asked for.
func (r *codingRig) capStops(stop func(run scriptRun) bool, printed string) (caps func() []time.Duration) {
	r.t.Helper()
	fire, caps := r.capTimer()
	var once sync.Once
	r.mu.Lock()
	r.printed["bin/specs"] = printed
	r.mu.Unlock()
	r.setHang(func(run scriptRun) bool {
		if !stop(run) {
			return false
		}
		once.Do(func() { close(fire) })
		return true
	})
	return caps
}

// capTimer puts a timer the test fires (close the channel) on the
// precheck's cap, and returns the caps the precheck asked it for.
func (r *codingRig) capTimer() (fire chan time.Time, caps func() []time.Duration) {
	fire = make(chan time.Time)
	var mu sync.Mutex
	var asked []time.Duration
	r.e.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, d)
		return fire
	}
	return fire, func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// capFiresAt fires the precheck's cap as the first command at fires
// starts, and lets that command go on only once its own context ended or
// half a second passed: a command on the precheck's context sees the cap,
// one on a context the cap does not cut does not. It returns the bounds of
// the commands at fires names.
func (r *codingRig) capFiresAt(at func(c execx.Cmd) bool) (bounds func() []time.Duration) {
	r.t.Helper()
	fire, _ := r.capTimer()
	var once sync.Once
	var mu sync.Mutex
	var seen []time.Duration
	r.mu.Lock()
	r.onRun = func(ctx context.Context, c execx.Cmd) {
		if !at(c) {
			return
		}
		mu.Lock()
		seen = append(seen, c.Timeout)
		mu.Unlock()
		once.Do(func() {
			close(fire)
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		})
	}
	r.mu.Unlock()
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// precheckSpecs reports whether run is the precheck's spec run of the
// change at the head: it names app/credit.rb, which no proof run names.
func precheckSpecs(run scriptRun) bool {
	return strings.HasPrefix(run.Script, "bin/specs ") && strings.Contains(run.Script, "app/credit.rb") && run.Branch != ""
}

// scriptOf is the script a copy's command runs (`mise … sh -c <script>`),
// "" for another command.
func scriptOf(c execx.Cmd) string {
	if c.Name != "mise" || len(c.Args) == 0 {
		return ""
	}
	return c.Args[len(c.Args)-1]
}

// readPrecheckRecord is task id's precheck.json.
func (r *codingRig) readPrecheckRecord(id int64) precheckRecord {
	r.t.Helper()
	var rec precheckRecord
	if err := json.Unmarshal([]byte(r.readTask(id, precheckRecordFile)), &rec); err != nil {
		r.t.Fatal(err)
	}
	return rec
}

// A precheck's cap grows with the change: cap, then cap_per_file for each
// file the precheck checks (a deleted file is none), at most cap_max. Its
// timer and the bound of every command it runs are that cap, and
// precheck.md says how it was found. A repository without the two keys
// takes their defaults (15s and 30m).
func TestThePrecheckCapGrowsPerFileUpToCapMax(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12470", "bug")
	r.fix(task) // two files
	r.git(r.copyDir, "rm", "--quiet", "README")
	r.git(r.copyDir, "commit", "--quiet", "-m", "PR-12470 drops the README") // a deletion the precheck does not check
	var mu sync.Mutex
	var timers, bounds []time.Duration
	r.e.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		timers = append(timers, d)
		return nil // never fires
	}
	r.seenMise = func(c execx.Cmd) {
		mu.Lock()
		defer mu.Unlock()
		bounds = append(bounds, c.Timeout)
	}
	check := func(want time.Duration, how string) {
		t.Helper()
		mu.Lock()
		timers, bounds = nil, nil
		mu.Unlock()
		res, err := r.e.precheck(r.ctx, task, r.cp)
		if err != nil || !res.Passed {
			t.Fatalf("precheck %+v, %v; want passed", res, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(timers, []time.Duration{want}) {
			t.Errorf("the cap's timer is %v, want %s", timers, want)
		}
		if len(bounds) < 3 || slices.ContainsFunc(bounds, func(d time.Duration) bool { return d != want }) {
			t.Errorf("the commands' bounds are %v, want each %s (lint, specs, jest)", bounds, want)
		}
		if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, how) {
			t.Errorf("%s does not say %q:\n%s", precheckFile, how, report)
		}
	}
	check(10*time.Minute+30*time.Second, "Cap: 10m30s (10m + 15s × 2 files).")

	files := map[string]string{}
	for i := range 127 {
		files[fmt.Sprintf("docs/note_%03d.md", i)] = "a note\n"
	}
	r.commit("PR-12470 notes", files)
	check(30*time.Minute, "Cap: 30m (10m + 15s × 129 files is 42m15s, at most 30m).")

	pc := &r.cfg.Factory.Repos[0].Precheck
	pc.CapPerFile, pc.CapMax = config.Duration{Duration: time.Second}, config.Duration{Duration: time.Hour}
	check(12*time.Minute+9*time.Second, "Cap: 12m9s (10m + 1s × 129 files).")
}

// cap_per_file × the files never wraps: a cap_per_file far too large (the
// config refuses one above cap_max, a repository built by hand does not)
// still gives cap_max, never a negative cap that fires at once.
func TestAPrecheckCapNeverOverflows(t *testing.T) {
	repo := config.FactoryRepo{Precheck: config.FactoryPrecheck{Cap: config.Duration{Duration: 10 * time.Minute},
		CapPerFile: config.Duration{Duration: 10000 * time.Hour}, CapMax: config.Duration{Duration: 30 * time.Minute}}}
	for _, n := range []int{1, 300, 1 << 40} {
		if got, how := precheckCap(repo, n); got != 30*time.Minute || !strings.HasPrefix(how, "30m (10m + 10000h × ") ||
			!strings.HasSuffix(how, ", at most 30m)") {
			t.Errorf("the cap of %d files = %s (%s), want 30m", n, got, how)
		}
	}
	if got, _ := precheckCap(repo, 0); got != 10*time.Minute {
		t.Errorf("the cap of no file = %s, want the cap, 10m", got)
	}
}

// A precheck its cap stops in the specs with no other failure sends the
// writer no fix turn (it cannot fix a time limit): the task goes on as
// after a pass, precheck.json records the pass with what did not finish
// (the ready stage reads it), an event names the step the cap stopped, the
// cap and that the task goes on, and the spec run is recorded as skipped.
// The proof's spec runs keep the repository's cap.
func TestAnOverCapPrecheckWithNoOtherFailureGoesOnToTheLocalReview(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12471", "bug")
	r.startStage(task)
	head := r.fix(task)
	caps := r.capStops(precheckSpecs, "")
	var mu sync.Mutex
	var proofBounds []time.Duration
	r.seenMise = func(c execx.Cmd) {
		if strings.HasPrefix(scriptOf(c), "bin/specs ") && !strings.Contains(scriptOf(c), "app/credit.rb") {
			mu.Lock()
			proofBounds = append(proofBounds, c.Timeout)
			mu.Unlock()
		}
	}
	r.endTurns(task, 1)
	if got := caps(); !slices.Equal(got, []time.Duration{10*time.Minute + 30*time.Second}) {
		t.Fatalf("the precheck's caps %v, want one of 10m30s", got)
	}
	mu.Lock()
	if !slices.Equal(proofBounds, []time.Duration{10 * time.Minute, 10 * time.Minute}) {
		t.Errorf("the proof's spec runs are bounded by %v, want the repository's cap (10m) for its base and head runs", proofBounds)
	}
	mu.Unlock()
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 {
		t.Fatalf("the writer got %d prompts (%+v); want no fix turn for the cap", len(ps), ps)
	}
	if got := r.reload(task.ID); got.Stage != "local_review" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s waits %q; want it at the local review", got.State, got.Stage, got.WaitsWhat)
	}
	const unfinished = "the specs did not finish within 10m30s, and the jest tests did not run"
	evs := r.events(task.ID, "check")
	if !hasEvent(evs, registry.PhaseOK, "passed, but "+unfinished+" (the precheck's cap): the task goes on as after a pass") ||
		hasEvent(evs, registry.PhaseOK, "to the local review") {
		t.Fatalf("no event names the unfinished specs, the cap and that the task goes on, or it names the local review: %+v", evs)
	}
	if !precheckPassedAt(r.layout.FactoryTaskDir(task.ID), head) {
		t.Fatalf("precheck.json does not record the pass at %s:\n%s", head, r.readTask(task.ID, precheckRecordFile))
	}
	if rec := r.readPrecheckRecord(task.ID); rec.Unfinished != unfinished {
		t.Fatalf("precheck.json records unfinished %q, want %q", rec.Unfinished, unfinished)
	}
	if got := r.checksOf(task.ID, "spec"); len(got) == 0 || got[len(got)-1] != registry.CheckSkipped {
		t.Fatalf("spec checks %q; want the precheck's spec run recorded as skipped", got)
	}
	report := r.readTask(task.ID, precheckFile)
	if !strings.Contains(report, "Stopped: over its cap of 10m30s") || strings.Contains(report, "Failed") {
		t.Fatalf("%s does not say the cap stopped it, or says it failed:\n%s", precheckFile, report)
	}
}

// A failure before the cap stands: a precheck whose lint failed and whose
// specs its cap then stopped sends the writer the lint's fix turn, with
// the note of the specs that did not finish.
func TestAnOverCapPrecheckAfterALintFailureSendsTheLintFixTurn(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12472", "bug")
	r.startStage(task)
	r.fix(task)
	r.lint = func([]string) string { return "app/credit.rb:3:5: C: Style/NumericLiterals: on the changed line\n" }
	r.capStops(precheckSpecs, "")
	r.endTurns(task, 1)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "lint: 1 offense on the changed lines") ||
		!strings.Contains(ps[1].Text, "the specs did not finish within 10m30s") {
		t.Fatalf("the writer's prompts %+v; want the lint's fix turn with the note of the unfinished specs", ps)
	}
	if strings.Contains(ps[1].Text, "over 10m") {
		t.Fatalf("the fix turn names the cap as the failure:\n%s", ps[1].Text)
	}
	if got := r.reload(task.ID); got.Stage != "coding" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s; want active at the coding stage, at the writer's fix turn", got.State, got.Stage)
	}
}

// On a cap stop precheck.md keeps the last 40 lines the stopped command
// printed on stdout, where it stopped (not its stderr after them),
// scrubbed (precheck.md is kept) and without what a terminal acts on, in a
// fence its lines cannot close: a password, an email, a colour code and a
// fence of the output never reach the file as they were.
func TestPrecheckMdKeepsTheStoppedCommandsLast40LinesScrubbed(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12473", "bug")
	r.fix(task)
	var out []string
	for i := 1; i <= 50; i++ {
		out = append(out, fmt.Sprintf("spec line %02d", i))
	}
	out[44] = "spec line 45 DB_PASSWORD=hunter2-s3cret mailed to alice@example.com"
	out[45] = "spec line 46 \x1b[31mred\x1b[0m and \x1b]0;a title\x07"
	out[46] = "```"
	r.capStops(precheckSpecs, strings.Join(out, "\n")+"\n")
	r.mu.Lock()
	r.printedErr["bin/specs"] = "warning: a deprecation the spec run printed last\n"
	r.mu.Unlock()
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed || res.Unfinished != "the specs did not finish within 10m30s, and the jest tests did not run" {
		t.Fatalf("precheck %+v, %v; want passed with the specs unfinished", res, err)
	}
	report := r.readTask(task.ID, precheckFile)
	if strings.Contains(report, "spec line 10") || !strings.Contains(report, "spec line 11") || !strings.Contains(report, "spec line 50") ||
		strings.Contains(report, "deprecation") {
		t.Fatalf("%s does not keep the last 40 lines of the stopped spec run's stdout:\n%s", precheckFile, report)
	}
	if strings.Contains(report, "hunter2") || strings.Contains(report, "alice@example.com") || !strings.Contains(report, "spec line 45") {
		t.Fatalf("%s keeps the output unscrubbed:\n%s", precheckFile, report)
	}
	if strings.ContainsAny(report, "\x1b\x07") || !strings.Contains(report, "spec line 46 red and ") {
		t.Fatalf("%s keeps what a terminal acts on:\n%q", precheckFile, report)
	}
	if !strings.Contains(report, "````\nspec line 11\n") || !strings.Contains(report, "\n```\nspec line 48") ||
		!strings.Contains(report, "spec line 50\n````\n") {
		t.Fatalf("%s does not fence the output with a fence its ``` line cannot close:\n%s", precheckFile, report)
	}
}

// The red examples of the change's spec run are judged on the base
// whatever the cap: the lookup of their files on the base and the base run
// of only those files run on a context the precheck's cap does not cut,
// bounded by the repository's cap. A cap that fires in either never makes
// a red example the change's nor leaves it unjudged: one red on the base
// too is not ours, one of a spec file the base lacks is the change's, and
// each leaves its row. The steps the cap kept from starting did not run.
func TestRedExamplesAreJudgedOnTheBaseWhateverTheCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(c execx.Cmd) bool
	}{
		{"the lookup of the red files on the base", func(c execx.Cmd) bool {
			return c.Name == "git" && slices.Contains(c.Args, "cat-file")
		}},
		{"the base run of the red files", func(c execx.Cmd) bool {
			return strings.HasPrefix(scriptOf(c), "bin/specs ") && !strings.Contains(scriptOf(c), "app/credit.rb")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			r.redOnMaster()
			task := r.codingTask("PR-12474", "bug")
			r.fix(task)
			head := r.commit("PR-12474 a red spec", map[string]string{"spec/models/credit_extra_spec.rb": "expect: def extra in app/credit.rb\n"})
			bounds := r.capFiresAt(tc.at)
			var mu sync.Mutex
			var baseBounds []time.Duration
			r.seenMise = func(c execx.Cmd) {
				if s := scriptOf(c); strings.HasPrefix(s, "bin/specs ") && !strings.Contains(s, "app/credit.rb") {
					mu.Lock()
					baseBounds = append(baseBounds, c.Timeout)
					mu.Unlock()
				}
			}
			res, err := r.e.precheck(r.ctx, task, r.cp)
			if err != nil || res.Passed || !strings.Contains(res.Why, "credit_extra_spec.rb") || strings.Contains(res.Why, "credit_spec.rb") {
				t.Fatalf("precheck %+v, %v; want the new red spec to fail it and the one red on the base too not", res, err)
			}
			if res.Unfinished != "the jest tests did not run: the cap of 10m45s ran out before them" {
				t.Fatalf("unfinished %q, want only jest, which did not run", res.Unfinished)
			}
			if len(bounds()) == 0 {
				t.Fatal("the cap never fired")
			}
			mu.Lock()
			if !slices.Equal(baseBounds, []time.Duration{10 * time.Minute}) {
				t.Errorf("the base run of the red files is bounded by %v, want one run of the repository's cap, 10m", baseBounds)
			}
			mu.Unlock()
			if got := r.checksOf(task.ID, "spec"); !slices.Equal(got, []string{"fail/not_ours", "fail/code"}) {
				t.Fatalf("spec checks %q, want the head's red run's rows: not ours, then the change's", got)
			}
			r.onBranch(task.Branch, head)
		})
	}
}

// A branch's migrations run whatever the cap: CI never runs a migration
// down and up, so a cap that stopped the specs before them does not keep
// them from running, on a context the cap does not cut, each command
// bounded by the repository's cap. The note names the specs and jest only,
// and a migration that fails then fails the precheck.
func TestTheMigrationsRunAfterTheCapStoppedTheSpecs(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12476", "feature")
	r.fix(task)
	r.commit("PR-12476 a column", map[string]string{"db/migrate/20261110090000_add_x_to_credits.rb": "class AddXToCredits < ActiveRecord::Migration[7.1]\n" +
		"  def change\n    add_column :credits, :x, :integer\n  end\nend\n"})
	r.capStops(precheckSpecs, "")
	var mu sync.Mutex
	var bounds []time.Duration
	r.seenMise = func(c execx.Cmd) {
		if strings.HasPrefix(scriptOf(c), "bin/rails ") {
			mu.Lock()
			bounds = append(bounds, c.Timeout)
			mu.Unlock()
		}
	}
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed || res.Unfinished != "the specs did not finish within 10m45s, and the jest tests did not run" {
		t.Fatalf("precheck %+v, %v; want passed with the specs and jest named, the migrations run", res, err)
	}
	var scripts []string
	for _, s := range r.ran("bin/rails") {
		scripts = append(scripts, s.Script)
	}
	if want := []string{"bin/rails db:migrate:up VERSION=20261110090000", "bin/rails db:migrate:down VERSION=20261110090000"}; !slices.Equal(scripts, want) {
		t.Fatalf("migration scripts %q, want %q", scripts, want)
	}
	mu.Lock()
	if !slices.Equal(bounds, []time.Duration{10 * time.Minute, 10 * time.Minute}) {
		t.Errorf("the migrations are bounded by %v, want the repository's cap, 10m", bounds)
	}
	mu.Unlock()
	if got := r.checksOf(task.ID, "migration"); !slices.Equal(got, []string{"pass", "pass"}) {
		t.Fatalf("migration checks %q, want both passed", got)
	}

	r.migrate = func(_, script string) int {
		if strings.Contains(script, "down") {
			return 1
		}
		return 0
	}
	r.capStops(precheckSpecs, "")
	res, err = r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "20261110090000_add_x_to_credits.rb") || res.Unfinished == "" {
		t.Fatalf("precheck %+v, %v; want the failed migration to fail it, with the note", res, err)
	}
}

// The ready card's FLAGS line and the PR body name a precheck that passed
// unfinished at the task's head, in the factory's own words; a record of
// another head names nothing.
func TestTheCardAndThePRBodyNameAnUnfinishedPrecheck(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12477", "bug")
	head := r.fix(task)
	r.capStops(precheckSpecs, "")
	if res, err := r.e.precheck(r.ctx, task, r.cp); err != nil || !res.Passed || res.Unfinished == "" {
		t.Fatalf("precheck %+v, %v; want passed unfinished", res, err)
	}
	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	const flag = "precheck unfinished: the specs did not finish within 10m30s, and the jest tests did not run"
	if !slices.Contains(f.Lines(), flag) {
		t.Fatalf("FLAGS %q, want %q", f.Lines(), flag)
	}
	r.taskFile(task.ID, prBodyFile, "## Summary\n\nThe change.\n")
	task.HeadSHA = head
	if err := r.e.appendKnown(r.ctx, task); err != nil {
		t.Fatal(err)
	}
	const sentence = "The factory's precheck stopped at its cap: the specs did not finish within 10m30s, and the jest tests did not run, " +
		"so CI is their only run."
	if body := r.readTask(task.ID, prBodyFile); strings.Count(body, sentence) != 1 || !strings.HasPrefix(body, "## Summary\n\nThe change.\n") {
		t.Fatalf("pr-body.md does not name the unfinished precheck once:\n%s", body)
	}

	task.HeadSHA = "0123456789abcdef0123456789abcdef01234567" // another head
	if err := r.e.appendKnown(r.ctx, task); err != nil {
		t.Fatal(err)
	}
	if body := r.readTask(task.ID, prBodyFile); strings.Contains(body, "precheck") {
		t.Fatalf("pr-body.md names the precheck of another head:\n%s", body)
	}
	if err := os.Remove(filepath.Join(r.layout.FactoryTaskDir(task.ID), precheckRecordFile)); err != nil {
		t.Fatal(err)
	}
	if f, err := r.e.flags(r.ctx, task, r.cp); err != nil || slices.ContainsFunc(f.Lines(), func(l string) bool { return strings.HasPrefix(l, "precheck") }) {
		t.Fatalf("FLAGS %q, %v without a precheck record; want no precheck line", f.Lines(), err)
	}
}

// The PR body's turn gets precheck.json by its path, and the prompt says
// its precheck sentence never says a precheck passed that did not finish.
func TestThePRBodyPromptNamesThePrecheckRecord(t *testing.T) {
	cfg := config.Defaults()
	d := fullPromptData()
	for _, skill := range []string{"example:create-pr", ""} {
		d.PRBodySkill = skill
		text := renderFactory(t, cfg, "factory-pr-body.md", d)
		for _, want := range []string{d.Precheck, "`unfinished`", "never that it passed"} {
			if !strings.Contains(text, want) {
				t.Errorf("factory-pr-body.md (skill %q) lacks %q:\n%s", skill, want, text)
			}
		}
	}
	if d.Precheck == "" {
		t.Fatal("the full prompt data has no precheck record")
	}
}
