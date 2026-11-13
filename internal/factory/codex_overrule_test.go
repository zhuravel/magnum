package factory

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// overruleRemedy ends every wait of the factory's on Codex's reading.
const overruleRemedy = "; `magnum resume --tool codex` overrules a stale reading"

// You overrule a stale Codex reading with `magnum resume --tool codex`
// (task 9 of 2026-11-13 waited at its local review on "codex at 96%" after
// Codex's limit was reset). Magnum's registry marks the time, and the
// review daemon's caps wait for Codex's next reading. The factory honors
// the same mark: while Codex's newest reading is stamped at or before it,
// the local review held at codex_soft starts its round, and a stage's Codex
// role launches on Codex, as under the cap, with no fallback in its place
// and no wait.
func TestTheFactorysLocalReviewGoesOnAfterTheOperatorOverrulesCodexsReading(t *testing.T) {
	t.Run("the local review", func(t *testing.T) {
		r := newReviewRig(t, result())
		r.cfg.Usage.CodexSoft = 95
		r.used, r.at = 96, t0.Add(-time.Hour)
		task, _ := r.reviewTask("PR-12440")
		r.run(4, nil)
		if got := r.reload(task.ID); len(r.rv.calls()) != 0 || got.WaitsWho != waitUsage {
			t.Fatalf("%d rounds; task waits %q: %q, want it held on Codex's reading", len(r.rv.calls()), got.WaitsWho, got.WaitsWhat)
		}
		r.magnum.overrule(r.clock.Now(), nil) // `magnum resume --tool codex`, after Codex's reading
		r.run(6, func() bool { return len(r.rv.calls()) == 1 })
		if got := r.reload(task.ID); len(r.rv.calls()) != 1 || got.WaitsWho == waitUsage {
			t.Fatalf("%d rounds after the overrule; task waits %q: %q, want the round started", len(r.rv.calls()), got.WaitsWho, got.WaitsWhat)
		}
	})
	t.Run("a stage's Codex role", func(t *testing.T) {
		m := &fakeMagnum{}
		h := newHarness(t, func(d *Deps) { d.Magnum = m })
		h.codex.setAt(95, t0.Add(-time.Hour))
		m.overrule(t0, nil)
		task := h.kindTask("PR-12441", "bug", "investigate")
		h.startStage(task)
		if got := h.rt.launchedRoles(); len(got) != 1 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause", "fx-history"}) {
			t.Fatalf("launches %v, want fx-cause on Codex beside fx-repro and fx-history", got)
		}
		if evs := h.stageEventsWith(task, "fx-cause-claude runs in fx-cause's place"); len(evs) != 0 {
			t.Fatalf("stage events %+v, want no yield under the overrule", evs)
		}
		if got := h.reload(task.ID); got.WaitsWho != "" {
			t.Fatalf("task waits %q: %q, want no wait", got.WaitsWho, got.WaitsWhat)
		}
	})
}

// The overrule lasts until Codex reports again: a reading stamped after the
// mark is the one the factory's caps judge, as the review daemon's do (it
// deletes the mark then, and a mark it has not deleted yet changes
// nothing). A stage launched under the overrule runs fx-cause on Codex;
// once Codex reports 95% after the mark, the next stage's fx-cause yields
// to fx-cause-claude, and the local review waits again.
func TestAReadingAfterTheOverruleDecidesTheFactorysCodexHoldAgain(t *testing.T) {
	t.Run("a stage's Codex role", func(t *testing.T) {
		m := &fakeMagnum{}
		h := newHarness(t, func(d *Deps) { d.Magnum = m })
		m.overrule(t0, nil)
		h.codex.setAt(95, t0.Add(-time.Hour))
		h.startStage(h.kindTask("PR-12442", "bug", "investigate"))
		h.codex.setAt(95, t0.Add(time.Minute)) // Codex's next session reports
		task := h.kindTask("PR-12443", "bug", "investigate")
		h.startStage(task)
		got := h.rt.launchedRoles()
		if len(got) != 2 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause", "fx-history"}) ||
			!slices.Equal(got[1], []string{"fx-repro", "fx-cause-claude", "fx-history"}) {
			t.Fatalf("launches %v, want fx-cause on Codex under the overrule, then fx-cause-claude in its place", got)
		}
		if evs := h.stageEventsWith(task, "fx-cause-claude runs in fx-cause's place in attempt 1 of the investigate stage: codex at 95%"); len(evs) != 1 {
			t.Fatalf("stage events %+v, want one yield on Codex's new reading", h.events(task.ID, "stage"))
		}
	})
	t.Run("the local review", func(t *testing.T) {
		r := newReviewRig(t, result())
		r.cfg.Usage.CodexSoft = 80
		r.magnum.overrule(t0, nil)
		r.used, r.at = 85, t0.Add(-time.Hour)
		if what := r.e.codexSoft(r.ctx); what != "" {
			t.Fatalf("the local review waits %q on a reading you overruled, want no wait", what)
		}
		r.at = t0.Add(time.Minute)
		if what := r.e.codexSoft(r.ctx); !strings.HasPrefix(what, "codex at 85%, at or past [usage] codex_soft 80%") {
			t.Fatalf("the local review waits %q, want it held on Codex's reading after the overrule", what)
		}
	})
}

// While the factory holds on Codex's reading, its wait says how to lift a
// stale one, as magnum's own soft-cap wait does: the task's wait, which the
// screen's wait line and `magnum factory status` print, ends with
// "`magnum resume --tool codex` overrules a stale reading", at the local
// review's codex_soft and at a stage whose Codex role has no fallback. A
// wait on Codex's usage pause, which a run's limit set and the resume does
// not lift, names no such command.
func TestTheFactorysCodexWaitNamesTheOverrule(t *testing.T) {
	t.Run("the local review", func(t *testing.T) {
		r := newReviewRig(t, result())
		r.cfg.Usage.CodexSoft = 80
		r.used = 85
		task, _ := r.reviewTask("PR-12445")
		r.run(4, nil)
		want := "codex at 85%, at or past [usage] codex_soft 80%: the local review yields to magnum's reviews" + overruleRemedy
		if got := r.reload(task.ID); got.WaitsWho != waitUsage || got.WaitsWhat != want {
			t.Fatalf("task waits %q: %q, want usage: %q", got.WaitsWho, got.WaitsWhat, want)
		}
	})
	t.Run("a stage's Codex role with no fallback", func(t *testing.T) {
		h := newHarness(t)
		delete(h.cfg.Factory.CodexFallback, "fx-cause")
		h.codex.set(95, nil)
		task := h.kindTask("PR-12446", "bug", "investigate")
		h.startStage(task)
		if got := h.reload(task.ID); got.WaitsWho != waitUsage ||
			!strings.HasSuffix(got.WaitsWhat, "so the investigate stage waits"+overruleRemedy) {
			t.Fatalf("task waits %q: %q, want the stage's wait ending %q", got.WaitsWho, got.WaitsWhat, overruleRemedy)
		}
	})
	t.Run("Codex's usage pause", func(t *testing.T) {
		h := newHarness(t)
		delete(h.cfg.Factory.CodexFallback, "fx-cause")
		h.rt.setPaused("codex", &runtime.WaitError{Who: "usage", What: "codex limit until 16:00"})
		task := h.kindTask("PR-12447", "bug", "investigate")
		h.startStage(task)
		if got := h.reload(task.ID); got.WaitsWho != waitUsage || !strings.Contains(got.WaitsWhat, "codex limit until 16:00") ||
			strings.Contains(got.WaitsWhat, "magnum resume") {
			t.Fatalf("task waits %q: %q, want the usage pause's wait with no resume in it", got.WaitsWho, got.WaitsWhat)
		}
	})
}

// A mark that cannot be read (magnum's registry of another schema, a disk
// error) keeps the factory's Codex holds as they were: a stage's Codex role
// yields on Codex's reading and the local review waits, and the log names
// the failure once while the reads keep failing, not at each launch.
func TestAnOverruleThatCannotBeReadKeepsTheFactorysCodexHoldAndLogsOnce(t *testing.T) {
	log := &syncBuffer{}
	m := &fakeMagnum{}
	h := newHarness(t, func(d *Deps) { d.Magnum, d.Log = m, slog.New(slog.NewTextHandler(log, nil)) })
	m.overrule(time.Time{}, errors.New("magnum's registry: disk I/O error"))
	h.codex.setAt(95, t0.Add(-time.Hour))
	for _, key := range []string{"PR-12448", "PR-12449"} {
		h.startStage(h.kindTask(key, "bug", "investigate"))
	}
	if got := h.rt.launchedRoles(); len(got) != 2 || !slices.Equal(got[0], []string{"fx-repro", "fx-cause-claude", "fx-history"}) ||
		!slices.Equal(got[1], got[0]) {
		t.Fatalf("launches %v, want fx-cause-claude in fx-cause's place in both stages", got)
	}
	if what := h.e.codexSoft(h.ctx); !strings.HasPrefix(what, "codex at 95%") {
		t.Fatalf("the local review waits %q, want it held on Codex's reading", what)
	}
	if n := strings.Count(log.String(), "disk I/O error"); n != 1 {
		t.Fatalf("the log names the failed read %d times, want once:\n%s", n, log.String())
	}
}
