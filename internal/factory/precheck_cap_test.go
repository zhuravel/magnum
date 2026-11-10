package factory

import (
	"fmt"
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
	fire := make(chan time.Time)
	var once sync.Once
	var mu sync.Mutex
	var asked []time.Duration
	r.e.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, d)
		return fire
	}
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
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// precheckSpecs reports whether run is the precheck's spec run of the
// change at the head: it names app/credit.rb, which no proof run names.
func precheckSpecs(run scriptRun) bool {
	return strings.HasPrefix(run.Script, "bin/specs ") && strings.Contains(run.Script, "app/credit.rb") && run.Branch != ""
}

// A precheck's cap grows with the change: cap, then cap_per_file for each
// file the precheck checks, at most cap_max. Its timer and the bound of
// every command it runs are that cap, and precheck.md says how it was
// found. A repository without the two keys takes their defaults (15s and
// 30m).
func TestThePrecheckCapGrowsPerFileUpToCapMax(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12470", "bug")
	r.fix(task) // two files
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

// A precheck its cap stops with no other failure sends the writer no fix
// turn (it cannot fix a time limit): the task goes on to the local review
// as after a pass, an event names the step the cap stopped, the cap and
// that the task goes on, and the spec run is recorded as skipped.
func TestAnOverCapPrecheckWithNoOtherFailureGoesOnToTheLocalReview(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12471", "bug")
	r.startStage(task)
	r.fix(task)
	caps := r.capStops(precheckSpecs, "")
	r.endTurns(task, 1)
	if got := caps(); !slices.Equal(got, []time.Duration{10*time.Minute + 30*time.Second}) {
		t.Fatalf("the precheck's caps %v, want one of 10m30s", got)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 {
		t.Fatalf("the writer got %d prompts (%+v); want no fix turn for the cap", len(ps), ps)
	}
	if got := r.reload(task.ID); got.Stage != "local_review" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s waits %q; want it at the local review", got.State, got.Stage, got.WaitsWhat)
	}
	evs := r.events(task.ID, "check")
	if !hasEvent(evs, registry.PhaseOK, "the specs did not finish within 10m30s") || !hasEvent(evs, registry.PhaseOK, "the task goes on") {
		t.Fatalf("no event names the unfinished specs, the cap and that the task goes on: %+v", evs)
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

// On a cap stop precheck.md keeps the last 40 lines of what the stopped
// command printed, scrubbed (precheck.md is kept): a password and an email
// in them never reach the file.
func TestPrecheckMdKeepsTheStoppedCommandsLast40LinesScrubbed(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12473", "bug")
	r.fix(task)
	var out []string
	for i := 1; i <= 50; i++ {
		out = append(out, fmt.Sprintf("spec line %02d", i))
	}
	out[44] = "spec line 45 DB_PASSWORD=hunter2-s3cret mailed to alice@example.com"
	r.capStops(precheckSpecs, strings.Join(out, "\n")+"\n")
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed || res.Unfinished != "the specs did not finish within 10m30s, and the jest tests did not run" {
		t.Fatalf("precheck %+v, %v; want passed with the specs unfinished", res, err)
	}
	report := r.readTask(task.ID, precheckFile)
	if strings.Contains(report, "spec line 10") || !strings.Contains(report, "spec line 11") || !strings.Contains(report, "spec line 50") {
		t.Fatalf("%s does not keep the last 40 lines of the stopped spec run:\n%s", precheckFile, report)
	}
	if strings.Contains(report, "hunter2") || strings.Contains(report, "alice@example.com") || !strings.Contains(report, "spec line 45") {
		t.Fatalf("%s keeps the output unscrubbed:\n%s", precheckFile, report)
	}
}

// A spec red at the head whose run on the base the cap stopped is not
// judged: it stays in precheck.md and in the note, and fails nothing. One
// whose spec file the base lacks needs no base run: it is the change's,
// and fails the precheck as before.
func TestARedSpecWhoseBaseRunTheCapStoppedIsNotJudged(t *testing.T) {
	r := newCodingRig(t)
	r.redOnMaster()
	task := r.codingTask("PR-12474", "bug")
	head := r.fix(task)
	r.capStops(func(run scriptRun) bool {
		return strings.HasPrefix(run.Script, "bin/specs ") && run.Branch == "" && strings.Contains(run.Script, "credit_spec.rb")
	}, "")
	res, err := r.e.precheck(r.ctx, task, r.cp)
	if err != nil || !res.Passed || !strings.Contains(res.Unfinished, "the specs did not finish within 10m30s") ||
		!strings.Contains(res.Unfinished, "1 example red at the head did not run on the base") {
		t.Fatalf("precheck %+v, %v; want passed, the red spec unjudged and named", res, err)
	}
	r.onBranch(task.Branch, head)
	if report := r.readTask(task.ID, precheckFile); !strings.Contains(report, "credit_spec.rb:2") || !strings.Contains(report, "not judged") {
		t.Fatalf("%s does not name the red spec it could not judge:\n%s", precheckFile, report)
	}

	// A new spec file, red at the head: the change's, whatever the base run.
	r.capStops(func(run scriptRun) bool {
		return strings.HasPrefix(run.Script, "bin/specs ") && run.Branch == "" && strings.Contains(run.Script, "credit_spec.rb")
	}, "")
	head = r.commit("PR-12474 a red spec", map[string]string{"spec/models/credit_extra_spec.rb": "expect: def extra in app/credit.rb\n"})
	res, err = r.e.precheck(r.ctx, task, r.cp)
	if err != nil || res.Passed || !strings.Contains(res.Why, "credit_extra_spec.rb") || strings.Contains(res.Why, "credit_spec.rb") ||
		!strings.Contains(res.Unfinished, "did not run on the base") {
		t.Fatalf("precheck %+v, %v; want the new red spec to fail it, the other unjudged", res, err)
	}
	r.onBranch(task.Branch, head)
}
