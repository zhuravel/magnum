package factory

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// leftTitles are the titles of sum's leftovers.
func leftTitles(sum ReviewSummary) []string {
	var out []string
	for _, i := range sum.Leftovers {
		out = append(out, i.Title)
	}
	return out
}

// blockNextRound holds the next round the loop starts (the reviewer's
// block) and ticks until it runs, without waiting for it.
func (r *reviewRig) blockNextRound() chan struct{} {
	r.t.Helper()
	ch := make(chan struct{})
	r.rv.mu.Lock()
	r.rv.block = ch
	n := len(r.rv.cases)
	r.rv.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for len(r.rv.calls()) == n && time.Now().Before(deadline) {
		r.tick()
		for k := 0; k < 50 && len(r.rv.calls()) == n; k++ {
			time.Sleep(time.Millisecond)
		}
	}
	if len(r.rv.calls()) != n+1 {
		r.t.Fatalf("%d rounds started, want round %d to run", len(r.rv.calls()), n+1)
	}
	return ch
}

// unblock lets the reviewer's rounds run.
func (r *reviewRig) unblock() {
	r.rv.mu.Lock()
	r.rv.block = nil
	r.rv.mu.Unlock()
}

// A round that ends when the task has run [factory.local_review] max_rounds
// rounds starts no other: a safe stop (no P0 or P1, a verdict that does not
// block, nothing the writer declined in its loop) passes the stage, the
// round's open findings left for the ready card's KNOWN row, whose line on
// the local review's end names the limit. Task 9 (PR-27708) ran 14 rounds
// over about 60 hours, each finding 2 to 5 new P2s.
func TestTheLocalReviewStopsAtMaxRoundsAndGoesToTheReadyCardWithItsKnownFindings(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...), result())
	r.cfg.Factory.LocalReview.MaxRounds = 2
	task, _ := r.reviewTask("PR-12511")
	first := r.toFixPrompt(task, 1)
	r.writerFixes(task, first.N, fixedCredit(1), nil)
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 2 || r.fixPrompts() != 1 {
		t.Fatalf("task %s at %s waits %q after %d rounds and %d fix turns, want past the review after 2 rounds and 1 fix",
			got.State, got.Stage, got.WaitsWhat, len(r.rv.calls()), r.fixPrompts())
	}
	if cs := r.cycles(task.ID); len(cs) != 2 || cs[1].Ended.IsZero() || cs[1].Exit != registry.ExitCap {
		t.Fatalf("cycles %+v, want 2, the last ended at the cap of rounds", cs)
	}
	sum := r.summary(task.ID)
	want := []string{capFinding(3, "P2").Title, capFinding(4, "P2").Title}
	if sum.Exit != exitRoundLimit || sum.Head != got.HeadSHA || sum.Limit != 2 || sum.TaskRounds != 2 || !slices.Equal(leftTitles(sum), want) {
		t.Fatalf("summary %+v, want exit %s at %s, limit 2 after 2 rounds, leftovers %q", sum, exitRoundLimit, short(got.HeadSHA), want)
	}
	known := knownLines(sum, false)
	if !slices.Contains(known, "P2 app/merger.rb:160: "+capFinding(3, "P2").Title) {
		t.Errorf("KNOWN %q, want round 2's P2s", known)
	}
	choice, why := readyRecommendation(ReadyCardFile{Head: got.HeadSHA, Review: sum}, nil)
	if choice != ChoicePush || !strings.Contains(why, "ended at the round limit (2 rounds)") {
		t.Errorf("recommendation %s (%q), want push naming the round limit", choice, why)
	}
	if body := knownList(sum); strings.Contains(body, "none of it blocks") || !strings.Contains(body, "round limit (2 rounds)") {
		t.Errorf("the PR body's Known list should name the limit, never say that none of it blocks:\n%s", body)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "review"), func(s string) bool { return strings.Contains(s, "ended at the round limit (2 rounds)") }) {
		t.Errorf("review events %q, want the end at the limit said", r.eventTexts(task.ID, "review"))
	}
	if alerts := r.eventTexts(task.ID, "alert"); len(alerts) != 0 {
		t.Errorf("alerts %q, want none: nothing waits on you", alerts)
	}
	r.taskFile(task.ID, reportFile, "## Cause\nthe merger maps the winners\n\n## Plan\n- map each winner\n")
	r.taskFile(task.ID, prBodyFile, "## Summary\n\nThe change.\n")
	if _, why := r.e.readyProblem(got); why != "" {
		t.Errorf("the ready card is refused: %s", why)
	}
}

// A stop at max_rounds that is not safe (a P1, or a finding the writer
// declined in its loop) waits on you, its words naming the limit, and goes
// on by itself at no cap. Your retry hands its findings to the writer, and
// one round reviews the fix: past the limit, that round's end stops again.
func TestAnUnsafeStopAtMaxRoundsWaitsOnYou(t *testing.T) {
	for _, tc := range []struct {
		name     string
		second   func(evalrun.LocalCase) evalrun.LocalResult
		declined map[string]string
	}{
		{"a P1", result(capFindings(3, 4, 4)...), nil},
		{"a declined finding", result(capFindings(3, 4)...), map[string]string{"F1": "a second query on every page load"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRig(t, result(capFindings(1, 2)...), tc.second, result(capFindings(5, 5)...))
			r.cfg.Factory.LocalReview.MaxRounds = 2
			task, _ := r.reviewTask("PR-12512")
			first := r.toFixPrompt(task, 1)
			r.writerFixes(task, first.N, fixedCredit(1), tc.declined)
			r.run(12, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			cs := r.cycles(task.ID)
			if got.State != registry.TaskNeedsYou || len(cs) != 2 || cs[1].Exit != registry.ExitCap {
				t.Fatalf("task %s waits %q; cycles %+v, want it stopped for you at round 2", got.State, got.WaitsWhat, cs)
			}
			if !strings.Contains(got.WaitsWhat, "at the round limit (2 rounds") || !strings.Contains(got.WaitsWhat, capFinding(3, "P2").Title) {
				t.Errorf("the wait %q should name the round limit and the findings", got.WaitsWhat)
			}
			if gs := r.goOnsOf(task.ID); len(gs) != 0 || r.fixPrompts() != 1 || fileIn(r.layout.FactoryTaskDir(task.ID), reviewSummaryFile) {
				t.Errorf("go-ons %+v, %d writer prompts, a summary: want none past round 1's fix", gs, r.fixPrompts())
			}

			r.letGoOn(task.ID)
			second := r.toFixPrompt(task, 2)
			if second.ID != cs[1].ID {
				t.Fatalf("your retry's fix turn is of cycle %d, want the stop's %d", second.ID, cs[1].ID)
			}
			r.writerFixes(task, second.N, fixedCredit(2), nil)
			r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			got = r.reload(task.ID)
			if got.Stage != "pr_body" || len(r.rv.calls()) != 3 {
				t.Fatalf("task %s at %s waits %q after %d rounds, want past the review after one round of the fix",
					got.State, got.Stage, got.WaitsWhat, len(r.rv.calls()))
			}
			if sum := r.summary(task.ID); sum.Exit != exitRoundLimit || sum.TaskRounds != 3 || sum.Head != got.HeadSHA {
				t.Errorf("summary %+v, want the round limit's end after 3 rounds at %s", sum, short(got.HeadSHA))
			}
		})
	}
}

// A task that ran more rounds than max_rounds before the limit reached it
// (task 9 ran 14) stops at the end of its round in flight: a daemon on a
// build with the limit runs that round again in its cycle, as any restart
// does, and starts no other.
func TestATaskPastMaxRoundsStopsAtTheEndOfItsCurrentRound(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...), result(capFindings(5, 6)...), result(capFindings(7, 8)...))
	r.cfg.Factory.LocalReview.Cycles, r.cfg.Factory.LocalReview.MaxRounds = 10, 0
	task, _ := r.reviewTask("PR-12513")
	for k := 1; k <= 3; k++ {
		cy := r.toFixPrompt(task, k)
		r.writerFixes(task, cy.N, fixedCredit(k), nil)
	}
	r.run(8, func() bool { cs := r.cycles(task.ID); return len(cs) == 3 && !cs[2].Ended.IsZero() })
	r.blockNextRound()
	r.e.stopBackground() // the install's drain cuts round 4
	r.unblock()
	r.cfg.Factory.LocalReview.MaxRounds = 2
	r.restart()
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if cs := r.cycles(task.ID); got.Stage != "pr_body" || len(cs) != 4 || r.fixPrompts() != 3 {
		t.Fatalf("task %s at %s waits %q after cycles %+v and %d fix turns, want past the review at round 4's end",
			got.State, got.Stage, got.WaitsWhat, cs, r.fixPrompts())
	}
	sum := r.summary(task.ID)
	if sum.Exit != exitRoundLimit || sum.TaskRounds != 4 || sum.Limit != 2 {
		t.Fatalf("summary %+v, want the round limit's end after 4 rounds", sum)
	}
	if _, why := readyRecommendation(ReadyCardFile{Head: got.HeadSHA, Review: sum}, nil); !strings.Contains(why, "ended at the round limit (2 rounds; it ran 4)") {
		t.Errorf("recommendation %q, want the limit and the rounds the task ran", why)
	}
}

// max_rounds = 0 keeps the loop as it was before the limit: a task whose
// rounds keep finding new P2s runs past 8 of them, within its cycles, and
// its loop ends clean.
func TestMaxRoundsZeroKeepsTodaysLoop(t *testing.T) {
	answers := make([]func(evalrun.LocalCase) evalrun.LocalResult, 0, 10)
	for i := 1; i <= 9; i++ {
		answers = append(answers, result(capFindings(2*i-1, 2*i)...))
	}
	r := newReviewRig(t, append(answers, result())...)
	r.cfg.Factory.LocalReview.Cycles, r.cfg.Factory.LocalReview.MaxRounds = 10, 0
	task, _ := r.reviewTask("PR-12514")
	for k := 1; k <= 9; k++ {
		cy := r.toFixPrompt(task, k)
		r.writerFixes(task, cy.N, fixedCredit(k), nil)
	}
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 10 {
		t.Fatalf("task %s at %s waits %q after %d rounds, want past the review after 10", got.State, got.Stage, got.WaitsWhat, len(r.rv.calls()))
	}
	if sum := r.summary(task.ID); sum.Exit != registry.ExitClean || sum.Limit != 0 {
		t.Errorf("summary %+v, want a clean end", sum)
	}
	if fileIn(r.layout.FactoryTaskDir(task.ID), loopEndFile) {
		t.Error("a loop with no limit recorded an end at it")
	}
}

// A clean round at max_rounds ends the loop clean: the task's second
// opinion, one round more, does not run, and an event says why.
func TestNoSecondOpinionRunsPastMaxRounds(t *testing.T) {
	r := newReviewRig(t, result(unitFinding), result()) // the last answer repeats: the second opinion's too
	r.cfg.Factory.LocalReview.MaxRounds = 2
	task, _ := r.wideReviewTask("PR-12515")
	cy := r.toFixPrompt(task, 1)
	r.writerFixes(task, cy.N, fixedCredit(1), nil)
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s at %s waits %q after %d rounds, want past the review after 2", got.State, got.Stage, got.WaitsWhat, len(r.rv.calls()))
	}
	if sum := r.summary(task.ID); sum.Exit != registry.ExitClean || sum.SecondJudge != "" {
		t.Errorf("summary %+v, want a clean end with no second opinion", sum)
	}
	if skips := r.reviewEvents(task.ID, registry.PhaseInfo, "no second opinion after round 2"); len(skips) != 1 ||
		!strings.Contains(skips[0].Text, "max_rounds 2") {
		t.Errorf("events %+v, want the skip naming the round limit", skips)
	}
}
