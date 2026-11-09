package factory

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The roles of the rig's watch: round 1's, a later round's, and a later
// round's with judge_fallback in the Codex judge's place (a second opinion).
var (
	firstRoles  = []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}
	laterRoles  = []string{"codex-judge", "claude-review", "codex-review"}
	secondRoles = []string{"claude-judge", "claude-review", "codex-review"}
)

// wideReviewTask is reviewTask on a wide change: the writer's fix touches
// three files.
func (r *reviewRig) wideReviewTask(key string) (registry.Task, string) {
	r.t.Helper()
	task := r.codingTask(key, "bug")
	head := r.commit(task.JiraKey+" Credit#points counts the activities", map[string]string{
		"app/credit.rb":                     strings.Replace(creditRB, "    0\n", "    10\n", 1),
		"app/points.rb":                     "class Points\nend\n",
		"spec/models/credit_points_spec.rb": "expect: 10 in app/credit.rb\n",
	})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "local_review")
		u.Set("head_sha", head)
	}); err != nil {
		r.t.Fatal(err)
	}
	return r.reload(task.ID), head
}

// reviewEvents are the task's review events whose text holds word.
func (r *reviewRig) reviewEvents(id int64, phase, word string) []registry.Event {
	r.t.Helper()
	return slices.DeleteFunc(r.events(id, "review"), func(e registry.Event) bool {
		return (phase != "" && e.Phase != phase) || !strings.Contains(e.Text, word)
	})
}

// summary is the task's review-summary.json.
func (r *reviewRig) summary(id int64) ReviewSummary {
	r.t.Helper()
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(id, reviewSummaryFile)), &sum); err != nil {
		r.t.Fatal(err)
	}
	return sum
}

// roundsOf are the roles of each round the reviewer ran.
func roundsOf(calls []evalrun.LocalCase) [][]string {
	out := make([][]string, len(calls))
	for i, c := range calls {
		out[i] = c.Roles
	}
	return out
}

// firstDecided reports whether cycle 1 of task id ended: its round was read.
func (r *reviewRig) firstDecided(id int64) bool {
	cs := r.cycles(id)
	return len(cs) == 1 && !cs[0].Ended.IsZero()
}

// A clean round of the Codex judge on a wide change is followed by one more
// round before the stage ends: the same later roles with judge_fallback
// (claude-judge) in the judge's place, at the same head. Its clean result
// ends the stage clean, the summary names the second opinion and keeps the
// first round's P3, and the events, the card's wait and each round's begin
// event say which judge ran.
func TestACleanRoundOnAWideChangeGetsASecondOpinionFromTheOtherJudge(t *testing.T) {
	r := newReviewRig(t, result(nitFinding), result())
	task, head := r.wideReviewTask("PR-12391")
	r.run(6, func() bool { return r.firstDecided(task.ID) })
	if !r.firstDecided(task.ID) {
		t.Fatalf("cycles %+v, want round 1 read", r.cycles(task.ID))
	}
	r.rv.mu.Lock()
	r.rv.block = make(chan struct{})
	r.rv.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.rv.calls()) < 2 && time.Now().Before(deadline) {
		r.tick()
		time.Sleep(time.Millisecond)
	}
	waiting := r.reload(task.ID)
	r.rv.mu.Lock()
	close(r.rv.block)
	r.rv.mu.Unlock()
	if waiting.WaitsWho != waitReview || waiting.WaitsWhat != "round 2 of the local review runs (a second opinion: claude-judge, on claude, in place of codex-judge)" {
		t.Errorf("while round 2 runs the task waits %q: %q", waiting.WaitsWho, waiting.WaitsWhat)
	}
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	calls := r.rv.calls()
	if got.Stage != "pr_body" || len(calls) != 2 {
		t.Fatalf("task at %s waits %q after %d rounds, want pr_body after 2", got.Stage, got.WaitsWhat, len(calls))
	}
	if !slices.Equal(calls[0].Roles, firstRoles) || !slices.Equal(calls[1].Roles, secondRoles) || calls[1].Round != 2 || calls[1].HeadSHA != head {
		t.Fatalf("rounds %v (round %d at %s), want round 2 with claude-judge at %s", roundsOf(calls), calls[1].Round, calls[1].HeadSHA, head)
	}
	cs := r.cycles(task.ID)
	if len(cs) != 2 || cs[0].Exit != "" || cs[0].HeadAfter != head || cs[1].Exit != registry.ExitClean || cs[1].HeadBefore != head {
		t.Fatalf("cycles %+v, want round 1 going on at %s and round 2 clean", cs, head)
	}
	sum := r.summary(task.ID)
	if sum.Exit != registry.ExitClean || sum.Rounds != 2 || sum.SecondJudge != "claude-judge" || sum.SecondRound != 2 ||
		!slices.ContainsFunc(sum.Leftovers, func(i ReviewItem) bool { return i.Title == nitFinding.Title }) {
		t.Fatalf("summary %+v, want clean after 2 rounds, round 2 by claude-judge, round 1's P3 kept", sum)
	}
	decided := r.reviewEvents(task.ID, registry.PhaseInfo, "a second opinion")
	if len(decided) != 1 || !strings.Contains(decided[0].Text, "round 2 with claude-judge (claude) in place of codex-judge (codex)") ||
		!strings.Contains(decided[0].Text, "touches 3 files") {
		t.Fatalf("events %+v, want one that says the second opinion runs, with which judge, and why", decided)
	}
	begun := r.reviewEvents(task.ID, registry.PhaseBegin, "")
	if len(begun) != 2 || !strings.HasSuffix(begun[0].Text, ": codex-judge, claude-review, codex-review, claude-simplify; judge codex-judge (codex)") ||
		!strings.HasSuffix(begun[1].Text, ": claude-judge, claude-review, codex-review; judge claude-judge (claude), a second opinion in place of codex-judge (codex)") {
		t.Fatalf("begin events %+v, want each round's judge and its kind", begun)
	}
}

// A second opinion that finds a P2 goes on like any round with findings:
// the writer's fix turn, then the next round with the usual roles, and its
// clean result ends the stage. The second opinion runs once per task: the
// later clean round gets none.
func TestASecondOpinionThatFindsAP2GoesToTheWriterAndTheNextRound(t *testing.T) {
	r := newReviewRig(t, result(nitFinding), result(unitFinding), result())
	task, _ := r.wideReviewTask("PR-12392")
	dir := r.layout.FactoryTaskDir(task.ID)
	r.run(10, func() bool { return fileIn(dir, reviewFile(2)) && len(r.rt.promptsTo("fx-writer")) > 0 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, reviewFile(2)) || !strings.Contains(r.readTask(task.ID, reviewFile(2)), unitFinding.Title) {
		t.Fatalf("the writer's prompts %+v, want its fix turn naming %s", ps, reviewFile(2))
	}
	r.writerFixes(task, 2, fixedCredit(2), nil)
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	calls := r.rv.calls()
	if got.Stage != "pr_body" || len(calls) != 3 {
		t.Fatalf("task at %s waits %q after %d rounds, want pr_body after 3", got.Stage, got.WaitsWhat, len(calls))
	}
	if want := [][]string{firstRoles, secondRoles, laterRoles}; !slices.EqualFunc(roundsOf(calls), want, slices.Equal) {
		t.Fatalf("rounds %v, want %v", roundsOf(calls), want)
	}
	if cs := r.cycles(task.ID); len(cs) != 3 || cs[0].Exit != "" || cs[1].Exit != "" || cs[2].Exit != registry.ExitClean {
		t.Fatalf("cycles %+v", cs)
	}
	if decided := r.reviewEvents(task.ID, registry.PhaseInfo, "a second opinion"); len(decided) != 1 {
		t.Fatalf("events %+v, want the one second opinion", decided)
	}
	if sum := r.summary(task.ID); sum.Rounds != 3 || sum.SecondJudge != "claude-judge" || sum.SecondRound != 2 {
		t.Fatalf("summary %+v", sum)
	}
}

// The clean round may be the last one cycles allows: the second opinion
// runs anyway, outside the count. Its clean result ends the stage clean;
// one with a P2 has no round left, and the task needs you at the cap.
func TestTheSecondOpinionRunsPastTheLastRoundCyclesAllows(t *testing.T) {
	r := newReviewRig(t, rounds(2, result())...)
	r.cfg.Factory.LocalReview.Cycles = 1
	task, _ := r.wideReviewTask("PR-12393")
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task at %s waits %q after %d rounds, want pr_body after 2", got.Stage, got.WaitsWhat, len(r.rv.calls()))
	}

	r = newReviewRig(t, result(), result(unitFinding))
	r.cfg.Factory.LocalReview.Cycles = 1
	task, _ = r.wideReviewTask("PR-12394")
	r.run(10, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	cs := r.cycles(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "1 blocking finding after 2 review rounds" || len(cs) != 2 || cs[1].Exit != registry.ExitCap {
		t.Fatalf("task %s waits %q; cycles %+v", got.State, got.WaitsWhat, cs)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
		t.Fatalf("the writer got %d prompts past the cap, want none", len(ps))
	}
}

// second_judge decides where the second opinion runs: "wide" (the default)
// on a wide change only, so a narrow one ends at its clean round; "always"
// on a narrow change too, and its event says so; "never" on none. A change
// without one says nothing of it.
func TestTheSecondOpinionFollowsSecondJudge(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		wide          bool
		rounds        int
		events        string
	}{
		{"narrow", config.FactorySecondJudgeWide, false, 1, ""},
		{"always", config.FactorySecondJudgeAlways, false, 2, "second_judge is always"},
		{"never", config.FactorySecondJudgeNever, true, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRig(t, rounds(2, result())...)
			r.cfg.Factory.LocalReview.SecondJudge = tc.setting
			review := r.reviewTask
			if tc.wide {
				review = r.wideReviewTask
			}
			task, _ := review("PR-12395")
			r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
			if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != tc.rounds {
				t.Fatalf("task at %s waits %q after %d rounds, want pr_body after %d", got.Stage, got.WaitsWhat, len(r.rv.calls()), tc.rounds)
			}
			evs := r.reviewEvents(task.ID, registry.PhaseInfo, "second opinion")
			if tc.events == "" {
				if len(evs) != 0 {
					t.Fatalf("events %+v, want none of a second opinion", evs)
				}
				return
			}
			if len(evs) != 1 || !strings.Contains(evs[0].Text, tc.events) {
				t.Fatalf("events %+v, want one saying %q", evs, tc.events)
			}
		})
	}
}

// No second opinion without its judge: a watch whose judge is not on Codex,
// a judge_fallback of no other kind, or a task Codex never works on again
// (its rounds ran with the fallback judge already) ends at its clean round,
// with one event that says why.
func TestNoSecondOpinionWithoutAJudgeOfTheOtherKind(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		set       func(r *reviewRig, task registry.Task)
	}{
		{"a claude judge", "the review's judge claude-judge runs on claude", func(r *reviewRig, _ registry.Task) {
			r.cfg.Watches[0].Roles = []string{"claude-judge", "claude-review", "codex-review"}
		}},
		{"no fallback", "judge_fallback names no judge of another kind than codex-judge's", func(r *reviewRig, _ registry.Task) {
			r.cfg.Pipeline.JudgeFallback = ""
		}},
		{"codex never", "its rounds ran with claude-judge already", func(r *reviewRig, task registry.Task) {
			if err := r.reg.TransitionTask(r.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("codex_never", true) }); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRig(t, rounds(2, result())...)
			task, _ := r.wideReviewTask("PR-12397")
			tc.set(r, task)
			r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
			if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 1 {
				t.Fatalf("task at %s waits %q after %d rounds, want pr_body after 1", got.Stage, got.WaitsWhat, len(r.rv.calls()))
			}
			evs := r.reviewEvents(task.ID, "", "second opinion")
			if len(evs) != 1 || !strings.Contains(evs[0].Text, "no second opinion after round 1 (cycle 1)") || !strings.Contains(evs[0].Text, tc.why) {
				t.Fatalf("events %+v, want one saying %q", evs, tc.why)
			}
		})
	}
}

// The second opinion's Codex reviewer keeps the local review's wait at
// [usage] codex_soft: past it the round waits, as any round with a Codex
// role does, and it runs once Codex is below it.
func TestTheSecondOpinionWaitsAtTheCodexSoftCap(t *testing.T) {
	r := newReviewRig(t, rounds(2, result())...)
	r.cfg.Usage.CodexSoft = 80
	task, _ := r.wideReviewTask("PR-12398")
	r.run(6, func() bool { return r.firstDecided(task.ID) })
	r.used = 85
	r.run(4, nil)
	if got := r.reload(task.ID); len(r.rv.calls()) != 1 || got.WaitsWho != "usage" || !strings.Contains(got.WaitsWhat, "codex at 85%") {
		t.Fatalf("%d rounds; task waits %q: %q", len(r.rv.calls()), got.WaitsWho, got.WaitsWhat)
	}
	r.used = 40
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if calls := r.rv.calls(); len(calls) != 2 || !slices.Equal(calls[1].Roles, secondRoles) {
		t.Fatalf("rounds %v, want the second opinion below the soft cap", roundsOf(calls))
	}
}

// A restart between the clean round and its second opinion starts the
// second opinion once: after the clean round's cycle ended; after the
// decision was written but before that cycle ended (the daemon stopping
// in between); and with the second opinion cut by a drain, which runs it
// again in its own cycle, with its judge.
func TestARestartBeforeTheSecondOpinionStartsItOnce(t *testing.T) {
	t.Run("after the clean cycle", func(t *testing.T) {
		r := newReviewRig(t, rounds(2, result())...)
		task, _ := r.wideReviewTask("PR-12399")
		r.run(6, func() bool { return r.firstDecided(task.ID) })
		r.restart()
		r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
		if calls := r.rv.calls(); len(calls) != 2 || !slices.Equal(calls[1].Roles, secondRoles) || len(r.cycles(task.ID)) != 2 {
			t.Fatalf("rounds %v; cycles %+v", roundsOf(calls), r.cycles(task.ID))
		}
	})
	t.Run("before the clean cycle ends", func(t *testing.T) {
		r := newReviewRig(t, rounds(2, result())...)
		r.e.beforeSecond = func() error { return errors.New("the daemon stops here") }
		task, _ := r.wideReviewTask("PR-12400")
		r.run(6, func() bool { return fileIn(r.layout.FactoryTaskDir(task.ID), secondOpinionFile) })
		if cs := r.cycles(task.ID); len(cs) != 1 || !cs[0].Ended.IsZero() {
			t.Fatalf("cycles %+v, want round 1's still open", cs)
		}
		r.restart()
		r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
		calls := r.rv.calls()
		if len(calls) != 2 || !slices.Equal(calls[1].Roles, secondRoles) || len(r.cycles(task.ID)) != 2 {
			t.Fatalf("rounds %v; cycles %+v", roundsOf(calls), r.cycles(task.ID))
		}
		if decided := r.reviewEvents(task.ID, registry.PhaseInfo, "a second opinion"); len(decided) != 1 {
			t.Fatalf("events %+v, want the decision said once", decided)
		}
	})
	t.Run("the second opinion cut", func(t *testing.T) {
		r := newReviewRig(t, rounds(2, result())...)
		task, _ := r.wideReviewTask("PR-12401")
		r.run(6, func() bool { return r.firstDecided(task.ID) })
		r.rv.mu.Lock()
		r.rv.block = make(chan struct{})
		r.rv.mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for len(r.rv.calls()) < 2 && time.Now().Before(deadline) {
			r.tick()
			time.Sleep(time.Millisecond)
		}
		r.e.stopBackground() // the drain cancels the second opinion
		r.rv.mu.Lock()
		r.rv.block = nil
		r.rv.mu.Unlock()
		r.restart()
		r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
		calls := r.rv.calls()
		if len(calls) != 3 || !slices.Equal(calls[2].Roles, secondRoles) || calls[2].Round != 2 {
			t.Fatalf("rounds %v, want round 2 again with claude-judge", roundsOf(calls))
		}
		if cs := r.cycles(task.ID); len(cs) != 2 || cs[1].Exit != registry.ExitClean {
			t.Fatalf("cycles %+v, want the second opinion's one cycle, clean", cs)
		}
	})
}
