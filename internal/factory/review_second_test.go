package factory

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/pipeline"
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
	for _, step := range []string{"event", "end"} {
		// "event": the decision is written, its event is not, the clean
		// cycle is open; "end": the event is written too.
		t.Run("stopped before its "+step, func(t *testing.T) {
			r := newReviewRig(t, rounds(2, result())...)
			r.e.beforeSecond = func(at string) error {
				if at == step {
					return errors.New("the daemon stops here")
				}
				return nil
			}
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
			if decided := r.reviewEvents(task.ID, registry.PhaseInfo, "a second opinion"); len(decided) != 1 ||
				!strings.Contains(decided[0].Text, "round 1 (cycle 1) is clean") {
				t.Fatalf("events %+v, want the decision said once", decided)
			}
		})
	}
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

// moveHead is a commit of yours on the task's branch, which the factory's
// checks then pass at: the task's head is the new one.
func (r *reviewRig) moveHead(task registry.Task) string {
	r.t.Helper()
	r.clock.Add(time.Minute)
	head := r.commit(task.JiraKey+" your change", map[string]string{"app/points.rb": "class Points\n  # yours\nend\n"})
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "local_review")
		u.Set("head_sha", head)
	}); err != nil {
		r.t.Fatal(err)
	}
	return head
}

// A pending second opinion holds only at the head its clean round
// reviewed: when the branch moves before it starts, it is void (once, with
// an event), the clean round's loop ends there, and a fresh loop runs at
// the new head from round 1 with round 1's roles and every one of cycles.
// The void one is not the task's second opinion: the fresh loop's clean
// round gets it.
func TestASecondOpinionIsVoidWhenTheHeadMovesBeforeItStarts(t *testing.T) {
	r := newReviewRig(t, rounds(3, result())...)
	r.cfg.Factory.LocalReview.Cycles = 1
	r.cfg.Usage.CodexSoft = 80
	task, head := r.wideReviewTask("PR-12404")
	r.run(6, func() bool { return r.firstDecided(task.ID) })
	r.used = 85 // the second opinion waits at codex_soft
	r.run(2, nil)
	if n := len(r.rv.calls()); n != 1 {
		t.Fatalf("%d rounds before the head moved, want 1", n)
	}
	moved := r.moveHead(task)
	r.run(2, nil)
	r.used = 40
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	calls := r.rv.calls()
	if got.Stage != "pr_body" || len(calls) != 3 {
		t.Fatalf("task %s at %s waits %q after %d rounds, want pr_body after 3", got.State, got.Stage, got.WaitsWhat, len(calls))
	}
	if !slices.Equal(calls[1].Roles, firstRoles) || calls[1].Round != 1 || calls[1].HeadSHA != moved ||
		!slices.Equal(calls[2].Roles, secondRoles) || calls[2].Round != 2 || calls[2].HeadSHA != moved {
		t.Fatalf("rounds %v (%d at %s, %d at %s), want round 1 of a fresh loop at %s, then its second opinion",
			roundsOf(calls), calls[1].Round, calls[1].HeadSHA, calls[2].Round, calls[2].HeadSHA, moved)
	}
	cs := r.cycles(task.ID)
	if len(cs) != 3 || cs[0].Exit != registry.ExitClean || cs[0].HeadBefore != head || cs[1].Exit != "" || cs[2].Exit != registry.ExitClean {
		t.Fatalf("cycles %+v, want round 1's loop ended clean at %s, then a fresh loop", cs, head)
	}
	void := r.reviewEvents(task.ID, registry.PhaseInfo, "is void")
	if len(void) != 1 || !strings.Contains(void[0].Text, "the branch moved from "+short(head)+" to "+short(moved)) {
		t.Fatalf("events %+v, want one that says the second opinion is void and why", void)
	}
	if sum := r.summary(task.ID); sum.Rounds != 2 || sum.SecondJudge != "claude-judge" || sum.SecondRound != 2 || sum.Head != moved {
		t.Fatalf("summary %+v, want the fresh loop's 2 rounds with its second opinion", sum)
	}
}

// A clean round that holds a doubt (a finding back twice after its fix,
// which its counts leave out) keeps it through its second opinion: the
// final summary still names it, though the second judge did not raise it.
func TestASecondOpinionKeepsTheCleanRoundsDoubts(t *testing.T) {
	uncounted := func(c evalrun.LocalCase) evalrun.LocalResult {
		return evalrun.LocalResult{Outcome: pipeline.OutcomeDryRun, Verdict: "clean", Findings: []evalrun.LocalFinding{refundFinding}}
	}
	r := newReviewRig(t, append(rounds(2, result(refundFinding)), uncounted, result())...)
	task, _ := r.wideReviewTask("PR-12405")
	for n := 1; n <= 2; n++ {
		r.run(6, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(n)) && len(r.rt.promptsTo("fx-writer")) >= n
		})
		r.writerFixes(task, n, fixedCredit(n), nil)
	}
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(calls) != 4 || !slices.Equal(calls[3].Roles, secondRoles) {
		t.Fatalf("task at %s waits %q after rounds %v, want pr_body after a second opinion", got.Stage, got.WaitsWhat, roundsOf(calls))
	}
	sum := r.summary(task.ID)
	if sum.Exit != registry.ExitClean || sum.SecondRound != 4 || len(sum.Doubts) != 1 || sum.Doubts[0].Title != refundFinding.Title {
		t.Fatalf("summary %+v, want round 3's doubt kept after the second opinion", sum)
	}
}

// A second opinion counts as the task's one only once its cycle ends clean
// or with findings: one that ended unavailable runs again at the next clean
// round of a wide change, after your go on, and its event says so.
func TestASecondOpinionThatEndedUnavailableRunsAgain(t *testing.T) {
	r := newReviewRig(t, result(nitFinding), outcome(pipeline.OutcomeError), outcome(pipeline.OutcomeTimeout), result(),
		outcome(pipeline.OutcomeDryRun))
	task, _ := r.wideReviewTask("PR-12406")
	r.run(10, func() bool { return !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.HasPrefix(got.WaitsWhat, "review unavailable") || len(r.rv.calls()) != 3 {
		t.Fatalf("task %s waits %q after %d rounds, want the second opinion unavailable", got.State, got.WaitsWhat, len(r.rv.calls()))
	}
	r.letGoOn(task.ID)
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if want := [][]string{firstRoles, secondRoles, secondRoles, firstRoles, secondRoles}; !slices.EqualFunc(roundsOf(calls), want, slices.Equal) {
		t.Fatalf("rounds %v, want %v", roundsOf(calls), want)
	}
	if cs := r.cycles(task.ID); len(cs) != 4 || cs[1].Exit != registry.ExitUnavailable || cs[3].Exit != registry.ExitClean {
		t.Fatalf("cycles %+v", cs)
	}
	again := r.reviewEvents(task.ID, registry.PhaseInfo, "a second opinion runs again")
	if len(again) != 1 || !strings.Contains(again[0].Text, "round 1 (cycle 3) is clean") || !strings.Contains(again[0].Text, "cycle 2's ended unavailable") {
		t.Fatalf("events %+v, want one that says the second opinion runs again and why", again)
	}
}

// A pending second opinion whose judge can no longer run at the clean
// round's head (second_judge "never", no judge_fallback of another kind, or
// a watch's judge that runs on Claude now) does not start: one event says why, the stage ends clean on that round
// with the summary it would have written, no further round runs, the task
// never needs you for it, and the wait the pending opinion had (on magnum's
// real round) is gone with it.
func TestAPendingSecondOpinionWithoutItsJudgeEndsTheStageClean(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		set       func(r *reviewRig)
	}{
		{"never", "second_judge is never now", func(r *reviewRig) { r.cfg.Factory.LocalReview.SecondJudge = config.FactorySecondJudgeNever }},
		{"no fallback", "judge_fallback names no judge of another kind", func(r *reviewRig) { r.cfg.Pipeline.JudgeFallback = "" }},
		{"a claude judge now", "the review's judge claude-judge runs on claude", func(r *reviewRig) {
			r.cfg.Watches[0].Roles = []string{"claude-judge", "claude-review", "codex-review"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRig(t, result(nitFinding), result())
			task, head := r.wideReviewTask("PR-12407")
			r.run(6, func() bool { return r.firstDecided(task.ID) })
			r.magnum.set("magnum reviews example/shop#1") // the pending opinion waits on magnum's real round
			r.run(2, nil)
			if got := r.reload(task.ID); got.WaitsWho != waitMagnum {
				t.Fatalf("the pending second opinion waits %q: %q, want magnum", got.WaitsWho, got.WaitsWhat)
			}
			tc.set(r)
			r.run(10, func() bool { return !isLive(r.reload(task.ID)) || r.reload(task.ID).Stage != "local_review" })
			r.run(2, nil)
			got := r.reload(task.ID)
			if got.WaitsWho != "" || got.WaitsWhat != "" {
				t.Errorf("the task at %s waits %q: %q after the stage passed, want nothing", got.Stage, got.WaitsWho, got.WaitsWhat)
			}
			if got.Stage != "pr_body" || len(r.rv.calls()) != 1 {
				t.Fatalf("task %s at %s waits %q after rounds %v, want pr_body after round 1 alone", got.State, got.Stage, got.WaitsWhat,
					roundsOf(r.rv.calls()))
			}
			if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitClean || cs[0].HeadBefore != head {
				t.Fatalf("cycles %+v, want round 1's clean", cs)
			}
			dropped := r.reviewEvents(task.ID, registry.PhaseInfo, "does not run")
			if len(dropped) != 1 || !strings.Contains(dropped[0].Text, tc.why) || !strings.Contains(dropped[0].Text, "ends clean on round 1") {
				t.Fatalf("events %+v, want one saying %q", dropped, tc.why)
			}
			if skips := r.reviewEvents(task.ID, "", "no second opinion"); len(skips) != 0 {
				t.Fatalf("events %+v, want no skip beside the drop", skips)
			}
			sum := r.summary(task.ID)
			if sum.Exit != registry.ExitClean || sum.Rounds != 1 || sum.Head != head || sum.SecondJudge != "" ||
				!slices.ContainsFunc(sum.Leftovers, func(i ReviewItem) bool { return i.Title == nitFinding.Title }) {
				t.Fatalf("summary %+v, want round 1's own: clean after 1 round with its P3", sum)
			}
		})
	}
}

// readSecond is the task's review-second.json.
func (r *reviewRig) readSecond(id int64) secondOpinion {
	r.t.Helper()
	so, ok := readSecondOpinion(r.layout.FactoryTaskDir(id))
	if !ok {
		r.t.Fatal("the task has no second opinion")
	}
	return so
}

// "never" set while a stop left the decision written and the clean cycle
// open drops the second opinion once the restarted daemon ends that cycle:
// one drop event, a record that is not live, the stage passed on the clean
// round, and a later loop's round 1 at a new head runs with round 1's roles,
// never as the second opinion.
func TestNeverSetBeforeTheCleanCycleEndsDropsTheSecondOpinion(t *testing.T) {
	r := newReviewRig(t, rounds(3, result())...)
	r.e.beforeSecond = func(at string) error {
		if at == "end" {
			return errors.New("the daemon stops here")
		}
		return nil
	}
	task, _ := r.wideReviewTask("PR-12408")
	r.run(6, func() bool { return fileIn(r.layout.FactoryTaskDir(task.ID), secondOpinionFile) })
	if cs := r.cycles(task.ID); len(cs) != 1 || !cs[0].Ended.IsZero() {
		t.Fatalf("cycles %+v, want round 1's still open", cs)
	}
	r.cfg.Factory.LocalReview.SecondJudge = config.FactorySecondJudgeNever
	r.restart()
	r.run(8, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 1 {
		t.Fatalf("task at %s waits %q after %d rounds, want pr_body after 1", got.Stage, got.WaitsWhat, len(r.rv.calls()))
	}
	if dropped := r.reviewEvents(task.ID, registry.PhaseInfo, "does not run"); len(dropped) != 1 || !strings.Contains(dropped[0].Text, "never now") {
		t.Fatalf("events %+v, want one drop", dropped)
	}
	if so := r.readSecond(task.ID); so.live() {
		t.Fatalf("second opinion %+v, want it dropped", so)
	}
	r.cfg.Factory.LocalReview.SecondJudge = config.FactorySecondJudgeWide
	moved := r.moveHead(task)
	r.run(10, func() bool { return len(r.rv.calls()) >= 2 })
	if calls := r.rv.calls(); len(calls) < 2 || !slices.Equal(calls[1].Roles, firstRoles) || calls[1].Round != 1 || calls[1].HeadSHA != moved {
		t.Fatalf("rounds %v, want a later loop's round 1 with round 1's roles at %s", roundsOf(calls), moved)
	}
}

// A second opinion that began, was cut, and then lost its judge ends as a
// pending one does: its cut cycle ends clean at its own head with the clean
// round's summary, one event says why, and no further round runs, past
// cycles or not, so the task never needs you for it.
func TestACutSecondOpinionThatLostItsJudgeEndsTheStageClean(t *testing.T) {
	r := newReviewRig(t, result(nitFinding), result())
	r.cfg.Factory.LocalReview.Cycles = 1
	task, head := r.wideReviewTask("PR-12409")
	r.run(6, func() bool { return r.firstDecided(task.ID) })
	r.rv.mu.Lock()
	r.rv.block = make(chan struct{})
	r.rv.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.rv.calls()) < 2 && time.Now().Before(deadline) {
		r.tick()
		time.Sleep(time.Millisecond)
	}
	r.e.stopBackground() // a stop cuts the second opinion
	r.rv.mu.Lock()
	r.rv.block = nil
	r.rv.mu.Unlock()
	r.cfg.Factory.LocalReview.SecondJudge = config.FactorySecondJudgeNever
	r.restart()
	r.run(10, func() bool { return !isLive(r.reload(task.ID)) || r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s at %s waits %q after rounds %v, want pr_body with no third round", got.State, got.Stage, got.WaitsWhat,
			roundsOf(r.rv.calls()))
	}
	if cs := r.cycles(task.ID); len(cs) != 2 || cs[1].Exit != registry.ExitClean || cs[1].HeadBefore != head {
		t.Fatalf("cycles %+v, want the cut cycle clean at %s", cs, head)
	}
	dropped := r.reviewEvents(task.ID, registry.PhaseInfo, "does not run")
	if len(dropped) != 1 || !strings.Contains(dropped[0].Text, "cycle 2") || !strings.Contains(dropped[0].Text, "never now") {
		t.Fatalf("events %+v, want one drop of the cut second opinion", dropped)
	}
	if sum := r.summary(task.ID); sum.Exit != registry.ExitClean || sum.Rounds != 1 || sum.SecondJudge != "" ||
		!slices.ContainsFunc(sum.Leftovers, func(i ReviewItem) bool { return i.Title == nitFinding.Title }) {
		t.Fatalf("summary %+v, want the clean round's", sum)
	}
}

// A new round is the second opinion only while the task's newest cycle is
// the clean round it follows, ended with the loop going on, at that round's
// head: a newest cycle that ended at the cap, or another head, is no second
// opinion; a task without a head (no stage proved one) keeps it, as the
// void of a moved head (settleSecond) does.
func TestANewRoundIsTheSecondOpinionOnlyAfterItsCleanCycle(t *testing.T) {
	r := newReviewRig(t, rounds(2, result())...)
	task, head := r.wideReviewTask("PR-12410")
	r.run(6, func() bool { return r.firstDecided(task.ID) })
	w := r.cfg.WatchFor(task.Repo)
	for _, tc := range []struct {
		head   string
		second bool
	}{{head, true}, {"", true}, {strings.Repeat("e", 40), false}} {
		at := r.reload(task.ID)
		at.HeadSHA = tc.head
		if _, second, ended, err := r.e.secondRound(r.ctx, at, w, nil); err != nil || ended || second != tc.second {
			t.Errorf("at head %q: second %v, ended %v, %v; want second %v", tc.head, second, ended, err, tc.second)
		}
	}
	if err := r.reg.CloseLoop(r.ctx, r.cycles(task.ID)[0].ID, registry.ExitCap); err != nil {
		t.Fatal(err)
	}
	if _, second, _, err := r.e.secondRound(r.ctx, r.reload(task.ID), w, nil); err != nil || second {
		t.Errorf("after a newest cycle at the cap: second %v, %v; want no second opinion", second, err)
	}
}

// A stop inside dropCut after the record's write (dropped, dropped_at the
// cut cycle) and before that cycle's end is finished at the next pass, from
// the record alone, whatever the config says now: the cut cycle ends clean
// with one drop event, and no further round runs.
func TestADropCutStoppedAfterItsRecordFinishesAtTheNextPass(t *testing.T) {
	r := newReviewRig(t, result(nitFinding), result())
	task, head := r.wideReviewTask("PR-12411")
	r.run(6, func() bool { return r.firstDecided(task.ID) })
	r.rv.mu.Lock()
	r.rv.block = make(chan struct{})
	r.rv.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.rv.calls()) < 2 && time.Now().Before(deadline) {
		r.tick()
		time.Sleep(time.Millisecond)
	}
	r.e.stopBackground() // a stop cuts the second opinion
	r.rv.mu.Lock()
	r.rv.block = nil
	r.rv.mu.Unlock()
	so := r.readSecond(task.ID)
	so.Dropped, so.DroppedAt = "second_judge is never now", so.cycle()
	if err := writeJSON(filepath.Join(r.layout.FactoryTaskDir(task.ID), secondOpinionFile), so); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.run(10, func() bool { return !isLive(r.reload(task.ID)) || r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s at %s waits %q after rounds %v, want pr_body with no third round", got.State, got.Stage, got.WaitsWhat,
			roundsOf(r.rv.calls()))
	}
	if cs := r.cycles(task.ID); len(cs) != 2 || cs[1].Exit != registry.ExitClean || cs[1].HeadBefore != head {
		t.Fatalf("cycles %+v, want the cut cycle clean at %s", cs, head)
	}
	if dropped := r.reviewEvents(task.ID, registry.PhaseInfo, "does not run again"); len(dropped) != 1 {
		t.Fatalf("events %+v, want one drop of the cut second opinion", dropped)
	}
}
