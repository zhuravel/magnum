package factory

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
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
			lead := "fx-cause-claude runs in fx-cause's place in the investigate stage: "
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
			lead := "fx-critic-claude runs in " + tc.role + "'s place in the " + tc.stage + " stage: codex at 92%"
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
			evs := h.stageEventsWith(task, "fx-points-codex sits out the investigate stage: ")
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
	if evs := h.stageEventsWith(task, "fx-cause-claude runs in fx-cause's place in the investigate stage: "); len(evs) != 1 ||
		!strings.Contains(evs[0].Text, "codex limit until 16:00") {
		t.Fatalf("stage events %+v, want one naming Codex's usage limit", h.events(task.ID, "stage"))
	}
}

// A Codex run that hits its usage limit hands its role to its fallback at
// once, with no wait for the limit's reset: fx-cause-claude launches in
// fx-cause's place, and its prompt says what fx-cause's run left is its
// work so far. That is no refusal: the task is not marked, no Codex session
// of it is quit as a refused one's (only fx-cause's own, which idles at its
// limit), and fx-cause's findings stay where they are, the run's own work,
// on which the stage may pass. One event says who runs in whose place and
// why, also when a restart came before the pass that took the limit.
func TestACodexUsageLimitHandsFxCauseToItsFallback(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			h := newHarness(t)
			task := h.kindTask("PR-12409", "bug", "investigate")
			h.startStage(task)
			h.taskFile(task.ID, "findings-fx-cause.md", "## Cause\nThe guard in app/models/campaign.rb:88 drops the points.\n")
			h.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-cause", RunID: h.lastRun("fx-cause"), Kind: runtime.EventPaused,
				Reason: "usage: codex limit until 16:00"})
			if restart {
				h.restart()
				h.startStage(task)
			} else {
				h.tick()
			}

			if got := h.rt.launchedRoles(); len(got) != 2 || !slices.Equal(got[1], []string{"fx-cause-claude"}) {
				t.Fatalf("launches %v, want fx-cause-claude after the first", got)
			}
			dir := h.layout.FactoryTaskDir(task.ID)
			ps := h.rt.promptsTo("fx-cause-claude")
			if len(ps) != 1 || !strings.Contains(ps[0].Text, filepath.Join(dir, "findings-fx-cause.md")) ||
				!strings.Contains(ps[0].Text, "usage limit") || !strings.Contains(ps[0].Text, "go on from") {
				t.Fatalf("the fallback's prompts %+v, want one naming findings-fx-cause.md as fx-cause's work to go on from", ps)
			}
			if b, err := os.ReadFile(filepath.Join(dir, "findings-fx-cause.md")); err != nil || !strings.Contains(string(b), "drops the points") ||
				fsx.Exists(filepath.Join(dir, "findings-fx-cause.md"+refusedSuffix)) {
				t.Fatalf("findings-fx-cause.md reads %q (%v), want the limited run's findings kept, none set aside", b, err)
			}
			got := h.reload(task.ID)
			if got.CodexNever || got.WaitsWho != "" || len(h.events(task.ID, "codex_never")) != 0 || len(h.rt.codexQuits()) != 0 {
				t.Fatalf("task (Codex never %v) waits %q: %q with codex_never events %+v and QuitCodex %v; want no mark, no wait, no refusal's quit",
					got.CodexNever, got.WaitsWho, got.WaitsWhat, h.events(task.ID, "codex_never"), h.rt.codexQuits())
			}
			if q := h.rt.roleQuitList(); !slices.Equal(q, []string{"PR-12409/fx-cause"}) {
				t.Fatalf("QuitRole calls %v, want fx-cause's session quit, once", q)
			}
			evs := h.stageEventsWith(task, "fx-cause-claude runs in fx-cause's place in the investigate stage: ")
			if len(evs) != 1 || !strings.Contains(evs[0].Text, "usage limit") || !strings.Contains(evs[0].Text, "codex limit until 16:00") {
				t.Fatalf("stage events %+v, want one naming fx-cause's usage limit", h.events(task.ID, "stage"))
			}

			h.taskFile(task.ID, "findings-fx-repro.md", "bin/rspec spec/models/campaign_copy_spec.rb:12 fails\n")
			h.taskFile(task.ID, "findings-fx-history.md", "The guard came with the copy feature in 2024 (abc1234).\n")
			h.ended(task, "fx-repro", "fx-history", "fx-cause-claude")
			h.tick()
			if got := h.reload(task.ID); got.Stage != "judge" {
				t.Fatalf("task at %q, want the investigation passed on the findings fx-cause wrote and its fallback kept", got.Stage)
			}
		})
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
			evs := h.stageEventsWith(task, "fx-points-codex sits out the investigate stage: ")
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
