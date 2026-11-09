package factory

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// stageEventsWith are task's stage events whose text starts with lead.
func (h *harness) stageEventsWith(task registry.Task, lead string) []registry.Event {
	h.t.Helper()
	return slices.DeleteFunc(h.events(task.ID, "stage"), func(ev registry.Event) bool { return !strings.HasPrefix(ev.Text, lead) })
}

// At or past [usage] codex_soft a stage's Codex role yields to magnum's
// reviews: its Claude codex_fallback launches in its place and gets its
// prompt, which names the role's own files, and the task stays one Codex
// works on. One stage event says who ran in whose place and why, and a
// restart launches, prompts and says nothing again. Under the cap nothing
// changes: fx-cause launches on Codex and no event says a thing.
func TestAtTheCodexSoftCapFxCauseClaudeRunsInFxCausesPlace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		used    float64
		restart bool
		runs    string
	}{
		{"under the cap", 79, false, "fx-cause"},
		{"at the cap", 80, false, "fx-cause-claude"},
		{"past the cap, then a restart", 95, true, "fx-cause-claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.codex.set(tc.used, nil)
			task := h.kindTask("PR-12401", "bug", "investigate")
			h.startStage(task)

			if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-repro", tc.runs, "fx-history"}) {
				t.Fatalf("launches %v, want fx-repro, %s and fx-history in one workspace", got, tc.runs)
			}
			dir := h.layout.FactoryTaskDir(task.ID)
			if ps := h.rt.promptsTo(tc.runs); len(ps) != 1 || !strings.Contains(ps[0].Text, filepath.Join(dir, "findings-fx-cause.md")) {
				t.Fatalf("%s's prompts %+v, want one naming findings-fx-cause.md", tc.runs, ps)
			}
			lead := "fx-cause-claude runs in fx-cause's place in attempt 1 of the investigate stage: "
			if tc.runs == "fx-cause" {
				if evs := h.stageEventsWith(task, lead); len(evs) != 0 {
					t.Fatalf("stage events %+v under the cap, want none of a yield", evs)
				}
				return
			}
			if n := len(h.rt.promptsTo("fx-cause")); n != 0 {
				t.Fatalf("fx-cause got %d prompts at the cap, want none", n)
			}
			if h.reload(task.ID).CodexNever {
				t.Fatal("the yield marked the task never for Codex")
			}
			if tc.restart {
				h.restart()
				h.startStage(task)
				if n, m := len(h.rt.launchedRoles()), len(h.rt.promptsTo("fx-cause-claude")); n != 1 || m != 1 {
					t.Fatalf("%d launches and %d prompts to the fallback after a restart, want the one of each before it", n, m)
				}
			}
			want := fmt.Sprintf("codex at %.0f%%, at or past [usage] codex_soft 80%%", tc.used)
			if evs := h.stageEventsWith(task, lead); len(evs) != 1 || !strings.Contains(evs[0].Text, want) ||
				!strings.Contains(evs[0].Text, "magnum's reviews come first") {
				t.Fatalf("stage events %+v, want one saying fx-cause-claude runs in fx-cause's place because %s", evs, want)
			}

			for _, role := range []string{"fx-repro", "fx-cause", "fx-history"} {
				h.taskFile(task.ID, "findings-"+role+".md", role+" found it in app/models/campaign.rb:88\n")
			}
			h.ended(task, "fx-repro", "fx-history", "fx-cause-claude")
			h.tick()
			if got := h.reload(task.ID); got.Stage != "judge" {
				t.Fatalf("task at %q, want the investigation passed on the fallback's findings", got.Stage)
			}
		})
	}
}

// At the soft cap either critic, of a wide plan or a narrow one, yields to
// fx-critic-claude, which writes critic.md and the critic's own questions
// file, and its critic.md passes the stage.
func TestAtTheCodexSoftCapFxCriticClaudeRunsInEachCriticsPlace(t *testing.T) {
	for _, tc := range []struct {
		stage, role, key, file string
	}{
		{"critic", "fx-critic", "PR-12402", "db/migrate/20261104090000_add_origin_to_rules.rb"},
		{"narrow_critic", "fx-critic-narrow", "PR-12403", "app/models/campaign.rb"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			h := newHarness(t)
			h.codex.set(92, nil)
			task := h.kindTask(tc.key, "feature", tc.stage)
			h.taskFile(task.ID, reportFile, reportNaming(tc.file))
			h.startStage(task)
			if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-critic-claude"}) {
				t.Fatalf("launches %v, want fx-critic-claude in %s's place", got, tc.role)
			}
			if ps := h.rt.promptsTo("fx-critic-claude"); len(ps) != 1 || !strings.Contains(ps[0].Text, roleQuestionsFile(tc.role)) {
				t.Fatalf("the fallback's prompts %+v, want one naming %s", ps, roleQuestionsFile(tc.role))
			}
			lead := "fx-critic-claude runs in " + tc.role + "'s place in attempt 1 of the " + tc.stage + " stage: codex at 92%"
			if evs := h.stageEventsWith(task, lead); len(evs) != 1 {
				t.Fatalf("stage events %+v, want one starting %q", h.events(task.ID, "stage"), lead)
			}
			h.taskFile(task.ID, criticFile, "The migration locks the rules table.\n")
			h.ended(task, "fx-critic-claude")
			h.tick()
			if got := h.reload(task.ID); got.Stage != "coding" {
				t.Fatalf("task at %q, want past the critic and the questions gate, at coding", got.Stage)
			}
		})
	}
}

// At the soft cap the feature's points twin, whose fallback fx-points the
// stage runs already, sits out: it never launches, fx-points reads the
// points alone and its findings end the stage, one event says so and why,
// and a restart launches no twin either.
func TestAtTheCodexSoftCapTheTwinSitsOutAndFxPointsEndsTheStage(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			h := newHarness(t)
			h.codex.set(95, nil)
			task := h.kindTask("PR-12404", "feature", "investigate")
			h.startStage(task)
			if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-points"}) {
				t.Fatalf("launches %v, want fx-points alone", got)
			}
			if restart {
				h.restart()
				h.startStage(task)
			}
			if n, m := len(h.rt.launchedRoles()), len(h.rt.promptsTo("fx-points")); n != 1 || m != 1 {
				t.Fatalf("%d launches and %d prompts to fx-points, want one of each", n, m)
			}
			if n := len(h.rt.promptsTo("fx-points-codex")); n != 0 {
				t.Fatalf("the twin got %d prompts, want none", n)
			}
			evs := h.stageEventsWith(task, "fx-points-codex sits out attempt 1 of the investigate stage: ")
			if len(evs) != 1 || !strings.Contains(evs[0].Text, "codex at 95%, at or past [usage] codex_soft 80%") ||
				!strings.Contains(evs[0].Text, "fx-points ends it alone") {
				t.Fatalf("stage events %+v, want one saying the twin sits out at the soft cap and fx-points ends the stage", h.events(task.ID, "stage"))
			}
			if got := h.reload(task.ID); got.WaitsWho != "" || got.CodexNever {
				t.Fatalf("task waits %q: %q (Codex never: %v), want no wait and no mark", got.WaitsWho, got.WaitsWhat, got.CodexNever)
			}

			h.taskFile(task.ID, "findings-fx-points.md", "## Point 1\nThe export exists in app/models/coupon_export.rb:12.\n")
			h.ended(task, "fx-points")
			h.tick()
			if got := h.reload(task.ID); got.Stage != "judge" {
				t.Fatalf("task at %q, want the investigation passed on fx-points' findings alone", got.Stage)
			}
		})
	}
}

// At the soft cap a Codex role with no codex_fallback waits, and its stage
// with it: no launch, and the task waits on Codex's budget in the local
// review's words, naming the role; once the budget is below the cap the
// stage launches as before and the wait ends.
func TestAtTheCodexSoftCapACodexRoleWithoutAFallbackWaits(t *testing.T) {
	h := newHarness(t)
	delete(h.cfg.Factory.CodexFallback, "fx-cause")
	h.codex.set(95, nil)
	task := h.kindTask("PR-12405", "bug", "investigate")
	h.startStage(task)
	if n := len(h.rt.launchedRoles()); n != 0 {
		t.Fatalf("%d launches, want none while fx-cause waits", n)
	}
	got := h.reload(task.ID)
	if got.State != registry.TaskActive || got.WaitsWho != waitUsage || !strings.HasPrefix(got.WaitsWhat, "codex at 95%, at or past [usage] codex_soft 80%") ||
		!strings.Contains(got.WaitsWhat, "fx-cause has no codex_fallback") || !strings.Contains(got.WaitsWhat, "investigate stage waits") {
		t.Fatalf("task %s waits %q: %q; want active, on Codex's budget, naming fx-cause and the stage", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if evs := h.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("advance failures %+v, want none: a wait is no failure", evs)
	}

	h.codex.set(60, nil)
	h.tick()
	if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause", "fx-history"}) {
		t.Fatalf("launches %v, want the stage on Codex once below the cap", got)
	}
	if got := h.reload(task.ID); got.WaitsWho != "" {
		t.Fatalf("task still waits %q: %q", got.WaitsWho, got.WaitsWhat)
	}
}

// A budget read that fails launches as before (the runtime's own gate reads
// it again), and logs once while the reads keep failing, not on each launch.
func TestAFailedCodexBudgetReadLaunchesAsBeforeAndLogsOnce(t *testing.T) {
	log := &syncBuffer{}
	h := newHarness(t, func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(log, nil)) })
	h.codex.set(0, errors.New("codex sessions: permission denied"))
	for _, key := range []string{"PR-12406", "PR-12407"} {
		h.startStage(h.kindTask(key, "bug", "investigate"))
	}
	if got := h.rt.launchedRoles(); len(got) != 2 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause", "fx-history"}) ||
		!slices.Equal(got[1], got[0]) {
		t.Fatalf("launches %v, want both stages launched on Codex as before", got)
	}
	if n := strings.Count(log.String(), "permission denied"); n != 1 {
		t.Fatalf("the log names the failed read %d times, want once:\n%s", n, log.String())
	}
}

// While Codex is paused on its usage limit (a run hit it), a new stage's
// Codex roles yield too: fx-cause-claude launches in fx-cause's place, and
// the event names the limit.
func TestWhileCodexIsPausedOnItsUsageLimitANewStagesCodexRoleYields(t *testing.T) {
	h := newHarness(t)
	h.rt.setPaused("codex", &runtime.WaitError{Who: "usage", What: "codex limit until 16:00"})
	task := h.kindTask("PR-12408", "bug", "investigate")
	h.startStage(task)
	if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause-claude", "fx-history"}) {
		t.Fatalf("launches %v, want fx-cause-claude in fx-cause's place", got)
	}
	if evs := h.stageEventsWith(task, "fx-cause-claude runs in fx-cause's place in attempt 1 of the investigate stage: "); len(evs) != 1 ||
		!strings.Contains(evs[0].Text, "codex limit until 16:00") {
		t.Fatalf("stage events %+v, want one naming Codex's usage limit", h.events(task.ID, "stage"))
	}
}

// A Codex run that hits its usage limit hands its role to its fallback at
// once, with no wait for the limit's reset, in an investigation and in a
// critic stage alike: the fallback launches in the role's place, and its
// prompt says what the limited run left is its work so far, to go on from.
// That is no refusal: the task is not marked, no Codex session of it is quit
// as a refused one's (only the limited role's own, which idles at its limit),
// and the limited run's file stays where it is. But it is no end of the
// role: the fallback's end holds only on a file it wrote in its own turn, so
// a fallback that ends without writing gets its one nudge, which says what
// the file is, and passes once it wrote it. One event says who runs in whose
// place and why. A restart before the pass that took the limit, or after the
// fallback's prompt, changes none of it.
func TestACodexUsageLimitHandsTheRoleToItsFallback(t *testing.T) {
	type stageCase struct {
		name, key, kind, stage, role, fallback, file, next string
	}
	for _, sc := range []stageCase{
		{"investigation", "PR-12409", "bug", "investigate", "fx-cause", "fx-cause-claude", "findings-fx-cause.md", "judge"},
		{"critic", "PR-12416", "feature", "critic", "fx-critic", "fx-critic-claude", criticFile, "coding"},
	} {
		for _, restart := range []string{"none", "before the handover", "after the fallback's prompt"} {
			t.Run(sc.name+", restart "+restart, func(t *testing.T) {
				h := newHarness(t)
				task := h.kindTask(sc.key, sc.kind, sc.stage)
				if sc.stage == "critic" {
					h.taskFile(task.ID, reportFile, reportNaming("db/migrate/20261104090000_add_origin_to_rules.rb"))
				}
				h.startStage(task)
				h.taskFile(task.ID, sc.file, "## First reading\nThe guard in app/models/campaign.rb:88")
				h.clock.Add(2 * time.Minute) // the limit comes later in the run
				h.observeEvents(runtime.Event{TaskID: task.ID, Role: sc.role, RunID: h.lastRun(sc.role), Kind: runtime.EventPaused,
					Reason: "usage: codex limit until 16:00"})
				switch restart {
				case "before the handover":
					h.restart()
					h.startStage(task)
				default:
					h.tick()
				}
				if restart == "after the fallback's prompt" {
					h.restart()
					h.startStage(task)
				}

				if got := h.rt.launchedRoles(); len(got) != 2 || !slices.Equal(got[1], []string{sc.fallback}) {
					t.Fatalf("launches %v, want %s after the first", got, sc.fallback)
				}
				dir := h.layout.FactoryTaskDir(task.ID)
				ps := h.rt.promptsTo(sc.fallback)
				if len(ps) != 1 || !strings.Contains(ps[0].Text, filepath.Join(dir, sc.file)) ||
					!strings.Contains(ps[0].Text, "usage limit") || !strings.Contains(ps[0].Text, "go on from") {
					t.Fatalf("the fallback's prompts %+v, want one naming %s as %s's work to go on from", ps, sc.file, sc.role)
				}
				if b, err := os.ReadFile(filepath.Join(dir, sc.file)); err != nil || !strings.Contains(string(b), "First reading") ||
					fsx.Exists(filepath.Join(dir, sc.file+refusedSuffix)) {
					t.Fatalf("%s reads %q (%v), want the limited run's text kept, none set aside", sc.file, b, err)
				}
				got := h.reload(task.ID)
				if got.CodexNever || got.WaitsWho != "" || len(h.events(task.ID, "codex_never")) != 0 || len(h.rt.codexQuits()) != 0 {
					t.Fatalf("task (Codex never %v) waits %q: %q with codex_never events %+v and QuitCodex %v; want no mark, no wait, no refusal's quit",
						got.CodexNever, got.WaitsWho, got.WaitsWhat, h.events(task.ID, "codex_never"), h.rt.codexQuits())
				}
				if q := h.rt.roleQuitList(); !slices.Equal(q, []string{sc.key + "/" + sc.role}) {
					t.Fatalf("QuitRole calls %v, want %s's session quit, once", q, sc.role)
				}
				lead := sc.fallback + " runs in " + sc.role + "'s place in attempt 1 of the " + sc.stage + " stage: "
				if evs := h.stageEventsWith(task, lead); len(evs) != 1 || !strings.Contains(evs[0].Text, "usage limit") ||
					!strings.Contains(evs[0].Text, "codex limit until 16:00") {
					t.Fatalf("stage events %+v, want one naming %s's usage limit", h.events(task.ID, "stage"), sc.role)
				}

				if sc.stage == "investigate" {
					h.taskFile(task.ID, "findings-fx-repro.md", "bin/rspec spec/models/campaign_copy_spec.rb:12 fails\n")
					h.taskFile(task.ID, "findings-fx-history.md", "The guard came with the copy feature in 2024 (abc1234).\n")
					h.ended(task, "fx-repro", "fx-history")
				}
				h.ended(task, sc.fallback) // it wrote nothing in its turn
				h.tick()
				ps = h.rt.promptsTo(sc.fallback)
				if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, nudgeLead) || !strings.Contains(ps[1].Text, sc.file) ||
					!strings.Contains(ps[1].Text, "what "+sc.role+"'s run on Codex left") {
					t.Fatalf("the fallback's prompts %+v, want its nudge naming %s as what %s's run left", ps, sc.file, sc.role)
				}
				if got := h.reload(task.ID); got.Stage != sc.stage {
					t.Fatalf("task at %q, want the stage held until the fallback writes its own %s", got.Stage, sc.file)
				}
				h.taskFile(task.ID, sc.file, "## Reading\nThe guard in app/models/campaign.rb:88 drops the points, checked.\n")
				h.ended(task, sc.fallback)
				h.tick()
				if got := h.reload(task.ID); got.Stage != sc.next {
					t.Fatalf("task at %q, want past the stage, at %s, on the fallback's own %s", got.Stage, sc.next, sc.file)
				}
			})
		}
	}
}

// A usage limit of the points twin hands its reading to fx-points, which
// the stage runs already: the twin sits out the rest of the stage, no
// fallback launches, what it wrote stays for the judge (no set-aside, no
// mark, no refusal's quit), one event says so, and fx-points' findings end
// the stage.
func TestACodexUsageLimitOfTheTwinLeavesFxPointsToEndTheStage(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12410", "feature", "investigate")
			h.startStage(task)
			h.taskFile(task.ID, "findings-fx-points-codex.md", "## Point 1\nThe export exists in app/models/coupon_export.rb:12.\n")
			h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-points-codex", RunID: h.lastRun("fx-points-codex"),
				Kind: runtime.EventPaused, Reason: "usage: codex limit until 16:00"})
			if restart {
				h.restart()
				h.startStage(task)
			} else {
				h.tick()
			}

			if n := len(h.rt.launchedRoles()); n != 1 {
				t.Fatalf("launches %v, want no fallback launched for the twin", h.rt.launchedRoles())
			}
			dir := h.layout.FactoryTaskDir(task.ID)
			if !fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md")) || fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md"+refusedSuffix)) {
				t.Fatal("the twin's findings were set aside, want them kept for the judge")
			}
			got := h.reload(task.ID)
			if got.CodexNever || got.WaitsWho != "" || len(h.rt.codexQuits()) != 0 {
				t.Fatalf("task (Codex never %v) waits %q: %q with QuitCodex %v; want no mark, no wait, no refusal's quit",
					got.CodexNever, got.WaitsWho, got.WaitsWhat, h.rt.codexQuits())
			}
			evs := h.stageEventsWith(task, "fx-points-codex sits out attempt 1 of the investigate stage: ")
			if len(evs) != 1 || !strings.Contains(evs[0].Text, "usage limit") || !strings.Contains(evs[0].Text, "fx-points ends it alone") {
				t.Fatalf("stage events %+v, want one saying the twin sits out on its usage limit", h.events(task.ID, "stage"))
			}

			h.taskFile(task.ID, "findings-fx-points.md", "## Point 1\nThe export exists in app/models/coupon_export.rb:12.\n")
			h.ended(task, "fx-points")
			h.tick()
			if got := h.reload(task.ID); got.Stage != "judge" {
				t.Fatalf("task at %q, want the investigation passed on fx-points' findings", got.Stage)
			}
			if !fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md")) {
				t.Fatal("the twin's findings are gone after the stage passed, want them for the judge")
			}
		})
	}
}

// While a hold of the machine refuses a stage's launch (the disk below its
// floor, after the copy's claim), the stage reads no Codex budget for it:
// the launch waits on the hold, and once the hold lifts the budget is read
// and the roles yield.
func TestALaunchAHoldRefusesReadsNoCodexBudget(t *testing.T) {
	var free atomic.Uint64
	free.Store(40 * gb)
	h, _ := diskHarness(t, &free)
	h.codex.set(95, nil)
	task := h.kindTask("PR-12414", "bug", "investigate")
	h.tick() // the claim of the copy begins, and ends off the loop
	h.e.waitBackground()
	free.Store(10 * gb)
	h.tick()
	h.codex.mu.Lock()
	reads := h.codex.reads
	h.codex.mu.Unlock()
	if n := len(h.rt.launchedRoles()); n != 0 || reads != 0 {
		t.Fatalf("%d launches and %d reads of Codex's budget under the disk hold, want none of either", n, reads)
	}
	if got := h.reload(task.ID); got.WaitsWho != HoldDisk || got.Copy == "" {
		t.Fatalf("task waits %q: %q with copy %q, want its copy taken and the launch on the disk hold", got.WaitsWho, got.WaitsWhat, got.Copy)
	}

	free.Store(40 * gb)
	h.tick()
	if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause-claude", "fx-history"}) {
		t.Fatalf("launches %v once the hold lifted, want fx-cause-claude in fx-cause's place", got)
	}
}

// A usage limit Codex shows as a dialog leaves its run in flight (the
// runtime pauses the kind and settles nothing). The handover abandons that
// run all the same, as a pause, and a late end of it is no end of the role;
// a restart before the fallback's launch keeps the handover, whose stage
// event is the attempt's record: the fallback launches once launches may,
// and Codex's session gets nothing more.
func TestADialogsUsageLimitKeepsItsHandoverAcrossARestart(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12415", "bug", "investigate")
	h.startStage(task)
	run := h.lastRun("fx-cause")
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: run, Kind: runtime.EventPaused,
		Reason: "usage: codex limit until 16:00"})
	h.rt.mu.Lock()
	h.rt.setRunLocked(run, store.RunWorking, "") // the dialog's pause: the run is still in flight
	h.rt.launchErr = &runtime.WaitError{Who: "memory", What: "the Mac's memory pressure is critical"}
	h.rt.mu.Unlock()
	h.tick()
	if state, why, err := h.rt.RunState(h.ctx, run); err != nil || state != store.RunAbandoned || !strings.HasPrefix(why, runtime.PausedPrefix+"usage: ") {
		t.Fatalf("fx-cause's run is %s (%q, %v), want abandoned on its usage pause", state, why, err)
	}
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: run, Kind: runtime.EventEnded})
	h.tick()
	if slices.ContainsFunc(h.rt.verifies(), func(v verifyCall) bool { return v.RunID == run }) {
		t.Fatalf("Verify calls %+v, want none of the handed-over Codex run", h.rt.verifies())
	}

	h.restart()
	h.rt.mu.Lock()
	h.rt.launchErr = nil
	h.rt.mu.Unlock()
	h.startStage(task)
	if n, m := len(h.rt.promptsTo("fx-cause-claude")), len(h.rt.promptsTo("fx-cause")); n != 1 || m != 1 {
		t.Fatalf("fx-cause-claude got %d prompts and fx-cause %d, want the fallback's first and no prompt more to Codex", n, m)
	}
}

// A recorded yield is the attempt's and is kept: a restart never runs a
// Codex role and its fallback on the same file, nor waits on a session that
// never comes. Its fallback's launch record lost, a restart before the
// fallback's first prompt sends that prompt to the fallback (launched again,
// which adopts a live agent), never to the Codex role. A launch that failed
// after a yield keeps the yield on its next try, whatever the budget says
// then. A Codex role with no record is decided again on each try of the
// launch, as no prompt has gone to anyone yet: a launch cut part-way under
// the cap and tried again at the cap yields it, the Codex session it may
// have started is quit, and the stage never waits for the budget's reset
// while a fallback exists. The fake launch holds a Codex role at the cap as
// the runtime's gate does (runtime.CodexBudgetWait).
func TestAnAttemptKeepsItsYieldAcrossARestartAndARetriedLaunch(t *testing.T) {
	t.Run("a lost launch record", func(t *testing.T) {
		h := newHarness(t)
		h.codex.set(95, nil)
		h.rt.mu.Lock()
		h.rt.noRecord = map[string]bool{"fx-cause-claude": true}
		h.rt.mu.Unlock()
		h.rt.setSendErr(runtime.ErrOperatorInPane) // the first prompts wait: you are in the panes
		task := h.kindTask("PR-12417", "bug", "investigate")
		h.startStage(task)
		h.restart()
		h.rt.setSendErr(nil)
		h.startStage(task)
		if n, m := len(h.rt.promptsTo("fx-cause")), len(h.rt.promptsTo("fx-cause-claude")); n != 0 || m != 1 {
			t.Fatalf("fx-cause got %d prompts and fx-cause-claude %d, want the fallback's first prompt only", n, m)
		}
	})
	for _, tc := range []struct {
		name          string
		first, second float64
		started       string // the role whose start went through before the launch failed
		want          string
		quit          bool
	}{
		{"a yield, then the budget below the cap", 95, 40, "", "fx-cause-claude", false},
		{"a Claude role started under the cap, then the budget at the cap", 40, 95, "fx-repro", "fx-cause-claude", false},
		{"the Codex role started under the cap, then the budget at the cap", 40, 95, "fx-cause", "fx-cause-claude", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.codex.set(tc.first, nil)
			h.rt.mu.Lock()
			h.rt.launchErr = errors.New("herdr: pane p3 does not answer")
			h.rt.mu.Unlock()
			task := h.kindTask("PR-12418", "bug", "investigate")
			h.startStage(task)
			if tc.started != "" { // the launch's first starts went through before it failed
				if err := h.reg.RecordRoleRun(h.ctx, registry.RoleRun{AttemptID: h.attempts(task.ID)[0].ID, Role: tc.started}); err != nil {
					t.Fatal(err)
				}
			}
			h.codex.set(tc.second, nil)
			h.rt.mu.Lock()
			h.rt.launchErr = nil
			h.rt.mu.Unlock()
			h.clock.Add(2 * time.Minute) // past the failed pass's retry pace
			h.tick()
			got := h.rt.launchedRoles()
			if len(got) != 2 || !slices.Equal(got[1], []string{"fx-repro", tc.want, "fx-history"}) {
				t.Fatalf("launches %v, want the retry to launch %s", got, tc.want)
			}
			if w := h.reload(task.ID); w.WaitsWho == waitUsage {
				t.Fatalf("task waits %q: %q, want no wait on Codex's budget while fx-cause has a fallback", w.WaitsWho, w.WaitsWhat)
			}
			if q := h.rt.roleQuitList(); tc.quit != slices.Contains(q, "PR-12418/fx-cause") {
				t.Fatalf("QuitRole calls %v, want fx-cause's idle Codex session quit: %v", q, tc.quit)
			}
			if n := len(h.rt.promptsTo("fx-cause")); n != 0 {
				t.Fatalf("fx-cause got %d prompts, want none", n)
			}
		})
	}
}

// A yield's record is written before anything of the attempt launches: a
// record that fails to write holds the launch, which goes on once a pass
// writes it, so a restart never rebuilds a yielded role on Codex with no
// session to prompt.
func TestAYieldWhoseRecordFailsToWriteHoldsTheLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.db")
	h := newHarness(t, func(d *Deps) { d.Registry = registrytest.OpenFile(t, path, registry.Options{Clock: d.Clock}) })
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER lose_yields BEFORE INSERT ON events WHEN NEW.text LIKE '%''s place in attempt %'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	h.codex.set(95, nil)
	task := h.kindTask("PR-12424", "bug", "investigate")
	h.startStage(task)
	if n, m := len(h.rt.launchedRoles()), len(h.rt.allPrompts()); n != 0 || m != 0 {
		t.Fatalf("%d launches and %d prompts with the yield unrecorded, want none of either", n, m)
	}

	if _, err := db.Exec(`DROP TRIGGER lose_yields`); err != nil {
		t.Fatal(err)
	}
	h.clock.Add(2 * time.Minute) // past the failed pass's retry pace
	h.tick()
	h.restart()
	h.startStage(task)
	if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause-claude", "fx-history"}) {
		t.Fatalf("launches %v, want fx-cause-claude in fx-cause's place once its yield is recorded", got)
	}
	if n, m := len(h.rt.promptsTo("fx-cause")), len(h.rt.promptsTo("fx-cause-claude")); n != 0 || m != 1 {
		t.Fatalf("fx-cause got %d prompts and fx-cause-claude %d after a restart, want the fallback's one", n, m)
	}
}

// A Codex role whose prompt waits on Codex's usage pause (another run hit
// the limit) is handed over too, never left waiting for the reset: fx-cause's
// nudge waits on the pause, and fx-cause-claude launches in its place, with
// its first prompt, and an event that names the pause.
func TestAPromptThatWaitsOnCodexsUsagePauseHandsTheRoleOver(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12419", "bug", "investigate")
	h.startStage(task)
	h.taskFile(task.ID, "findings-fx-repro.md", "bin/rspec spec/models/campaign_copy_spec.rb:12 fails\n")
	h.taskFile(task.ID, "findings-fx-history.md", "The guard came with the copy feature in 2024 (abc1234).\n")
	h.rt.setSendErr(&runtime.WaitError{Who: "usage", What: "codex limit until 16:00"})
	h.ended(task, "fx-repro", "fx-history", "fx-cause") // fx-cause wrote no findings: its nudge is due
	h.tick()
	h.rt.setSendErr(nil)
	h.tick()
	if got := h.rt.launchedRoles(); len(got) != 2 || !slices.Equal(got[1], []string{"fx-cause-claude"}) {
		t.Fatalf("launches %v, want fx-cause-claude in fx-cause's place", got)
	}
	if n, m := len(h.rt.promptsTo("fx-cause")), len(h.rt.promptsTo("fx-cause-claude")); n != 1 || m != 1 {
		t.Fatalf("fx-cause got %d prompts and fx-cause-claude %d, want no nudge to Codex and the fallback's first prompt", n, m)
	}
	if evs := h.stageEventsWith(task, "fx-cause-claude runs in fx-cause's place in attempt 1 of the investigate stage: "); len(evs) != 1 ||
		!strings.Contains(evs[0].Text, "codex limit until 16:00") {
		t.Fatalf("stage events %+v, want one naming Codex's usage pause", h.events(task.ID, "stage"))
	}
	if got := h.reload(task.ID); got.WaitsWho != "" {
		t.Fatalf("task waits %q: %q, want no wait on Codex's pause", got.WaitsWho, got.WaitsWhat)
	}
}

// What an earlier attempt left is no work of this one: a twin that sits out
// at the soft cap has its files of before the attempt set aside as
// <name>.stale, out of the judge's findings-*.md, and a handover's note to the
// fallback names only what the limited run wrote in this attempt.
func TestAYieldTakesNoEarlierAttemptsFileForThisOnes(t *testing.T) {
	t.Run("a twin that sits out", func(t *testing.T) {
		h := newHarness(t)
		task := h.kindTask("PR-12420", "feature", "investigate")
		h.taskFile(task.ID, "findings-fx-points-codex.md", "## Point 1\nThe export, an earlier attempt's reading.\n")
		h.clock.Add(time.Hour)
		h.codex.set(95, nil)
		h.startStage(task)
		dir := h.layout.FactoryTaskDir(task.ID)
		if fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md")) || !fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md"+staleSuffix)) {
			t.Fatal("the twin's earlier findings are still in the judge's findings-*.md, want them set aside as stale")
		}
	})
	t.Run("a twin handed over before it wrote", func(t *testing.T) {
		h := newHarness(t)
		task := h.kindTask("PR-12428", "feature", "investigate")
		h.taskFile(task.ID, "findings-fx-points-codex.md", "## Point 1\nThe export, an earlier attempt's reading.\n")
		h.clock.Add(time.Hour)
		h.startStage(task)
		h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-points-codex", RunID: h.lastRun("fx-points-codex"),
			Kind: runtime.EventPaused, Reason: "usage: codex limit until 16:00"})
		h.tick()
		dir := h.layout.FactoryTaskDir(task.ID)
		if fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md")) || !fsx.Exists(filepath.Join(dir, "findings-fx-points-codex.md"+staleSuffix)) {
			t.Fatal("the twin's earlier findings are still in the judge's findings-*.md, want them set aside as stale")
		}
		evs := h.stageEventsWith(task, "fx-points-codex sits out attempt 1 of the investigate stage: ")
		if len(evs) != 1 || strings.Contains(evs[0].Text, "wrote stays") {
			t.Fatalf("stage events %+v, want one sit-out that claims no work of this attempt", h.events(task.ID, "stage"))
		}
	})
	t.Run("a handover's note", func(t *testing.T) {
		h := newHarness(t)
		task := h.kindTask("PR-12421", "bug", "investigate")
		h.taskFile(task.ID, "findings-fx-cause.md", "## Cause\nAn earlier attempt's cause.\n")
		h.clock.Add(time.Hour)
		h.startStage(task)
		h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: h.lastRun("fx-cause"), Kind: runtime.EventPaused,
			Reason: "usage: codex limit until 16:00"})
		h.tick()
		if ps := h.rt.promptsTo("fx-cause-claude"); len(ps) != 1 || strings.Contains(ps[0].Text, "ran on Codex in this stage before you") {
			t.Fatalf("the fallback's prompts %+v, want one with no note of an earlier attempt's file", ps)
		}
	})
}

// The judge's prompt names the findings file of a second reader that sat
// out after Codex's usage limit in the investigation's newest attempt, as it
// stood and perhaps unfinished, and says nothing of the kind when none did.
func TestTheJudgeIsToldWhichSecondReadersFindingsMayBeUnfinished(t *testing.T) {
	h := newHarness(t)
	task := h.kindTask("PR-12422", "feature", "investigate")
	h.startStage(task)
	h.taskFile(task.ID, "findings-fx-points-codex.md", "## Point 1\nThe export")
	h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-points-codex", RunID: h.lastRun("fx-points-codex"),
		Kind: runtime.EventPaused, Reason: "usage: codex limit until 16:00"})
	h.tick()
	h.taskFile(task.ID, "findings-fx-points.md", "## Point 1\nThe export exists in app/models/coupon_export.rb:12.\n")
	h.ended(task, "fx-points")
	h.tick()
	dir := h.layout.FactoryTaskDir(task.ID)
	ps := h.rt.promptsTo("fx-judge")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, filepath.Join(dir, "findings-fx-points-codex.md")) || !strings.Contains(ps[0].Text, "unfinished") {
		t.Fatalf("the judge's prompts %+v, want one naming findings-fx-points-codex.md as perhaps unfinished", ps)
	}
	plain := h.kindTask("PR-12426", "feature", "judge")
	if text := h.renderInStage(plain, "judge", "fx-judge"); strings.Contains(text, "unfinished") || strings.Contains(text, "sat out") {
		t.Fatalf("the judge's prompt of a task no reader of which sat out speaks of one:\n%s", text)
	}
}

// The fallback's first prompt names every file of the role's that the Codex
// run left in the attempt, its questions file too, and the role's end and
// its questions count only from that prompt: a fallback that rewrites its
// findings alone is nudged on the questions file, told that [] says none,
// and one that writes nothing asks you nothing the cut run wrote.
func TestAHandoverNamesTheQuestionsFileTheCodexRunLeft(t *testing.T) {
	for _, writes := range []string{"its findings", "nothing"} {
		t.Run("the fallback writes "+writes, func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12425", "bug", "investigate")
			h.startStage(task)
			h.taskFile(task.ID, "findings-fx-cause.md", "## Cause\nThe guard")
			h.taskFile(task.ID, "questions-fx-cause.json", questionsJSON(t, whoQuestion))
			h.clock.Add(2 * time.Minute)
			h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: h.lastRun("fx-cause"), Kind: runtime.EventPaused,
				Reason: "usage: codex limit until 16:00"})
			h.tick()
			dir := h.layout.FactoryTaskDir(task.ID)
			ps := h.rt.promptsTo("fx-cause-claude")
			_, note, _ := strings.Cut(ps[0].Text, "work so far: ")
			if len(ps) != 1 || !strings.Contains(note, filepath.Join(dir, "findings-fx-cause.md")) ||
				!strings.Contains(note, filepath.Join(dir, "questions-fx-cause.json")) {
				t.Fatalf("the fallback's prompts %+v, want its note to name both files the Codex run left", ps)
			}
			h.taskFile(task.ID, "findings-fx-repro.md", "bin/rspec spec/models/campaign_copy_spec.rb:12 fails\n")
			h.taskFile(task.ID, "findings-fx-history.md", "The guard came with the copy feature in 2024 (abc1234).\n")
			h.ended(task, "fx-repro", "fx-history")
			if writes == "its findings" {
				h.taskFile(task.ID, "findings-fx-cause.md", "## Cause\nThe guard in app/models/campaign.rb:88 drops the points.\n")
			}
			h.ended(task, "fx-cause-claude")
			h.tick()
			if got := h.reload(task.ID); got.QuestionRev != 0 || got.State != registry.TaskActive {
				t.Fatalf("task %s with question revision %d (waits %q), want no question of the cut run asked", got.State, got.QuestionRev, got.WaitsWhat)
			}
			ps = h.rt.promptsTo("fx-cause-claude")
			want := "findings-fx-cause.md"
			if writes == "its findings" {
				want = "questions-fx-cause.json"
			}
			if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, nudgeLead) || !strings.Contains(ps[1].Text, want) ||
				(want == "questions-fx-cause.json" && !strings.Contains(ps[1].Text, "`[]`")) {
				t.Fatalf("the fallback's prompts %+v, want its nudge on %s", ps, want)
			}
		})
	}
}

// A Codex session a handover left (its quit failed while you were in a pane
// of the task) whose usage limit shows again (a dialog's new spell, or a
// restart) sends nothing to the fallback: the event is of a run the role no
// longer has.
func TestALateUsageLimitOfALeftCodexSessionNeverReachesTheFallback(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12427", "bug", "investigate")
			h.startStage(task)
			codexRun := h.lastRun("fx-cause")
			limit := runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: codexRun, Kind: runtime.EventPaused, Reason: "usage: codex limit until 16:00"}
			h.observeEvents(limit)
			h.tick()
			if n := len(h.rt.promptsTo("fx-cause-claude")); n != 1 {
				t.Fatalf("fx-cause-claude got %d prompts, want its first", n)
			}
			if restart {
				h.restart()
				h.startStage(task)
			}
			h.observeEvents(limit)
			h.tick()
			if n := len(h.rt.promptsTo("fx-cause-claude")); n != 1 {
				t.Fatalf("fx-cause-claude got %d prompts after the left session's limit showed again, want its first only: %+v", n,
					h.rt.promptsTo("fx-cause-claude"))
			}
			if got := h.reload(task.ID); got.WaitsWho == waitUsage {
				t.Fatalf("task waits %q: %q, want no wait on a left session's limit", got.WaitsWho, got.WaitsWhat)
			}
		})
	}
}
