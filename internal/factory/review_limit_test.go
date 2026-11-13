package factory

import (
	"encoding/json"
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

// After a safe end at max_rounds, the review comes back with no new commit
// (a send back the writer committed nothing for) and passes at once, with
// no round; a new head (a send back's commits, your own) gets exactly one
// round, the task's next, whose blocking end stops at the limit again and
// goes to the ready card counting the task's rounds: the count is the
// task's, not the loop's, and the old summary's head passes no other.
func TestANewHeadAfterTheRoundLimitGetsOneRound(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...), result(capFindings(5, 6)...), result())
	r.cfg.Factory.LocalReview.MaxRounds = 2
	task, _ := r.reviewTask("PR-12519")
	first := r.toFixPrompt(task, 1)
	r.writerFixes(task, first.N, fixedCredit(1), nil)
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	ended := r.reload(task.ID)
	if ended.Stage != "pr_body" || len(r.rv.calls()) != 2 || r.summary(task.ID).Exit != exitRoundLimit {
		t.Fatalf("task %s at %s after %d rounds, want past the review at the limit after 2", ended.State, ended.Stage, len(r.rv.calls()))
	}

	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) { u.Set("stage", "local_review") }); err != nil {
		t.Fatal(err)
	}
	r.run(4, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 || got.HeadSHA != ended.HeadSHA {
		t.Fatalf("task at %s after %d rounds, want it past the review again at %s with no round", got.Stage, len(r.rv.calls()), short(ended.HeadSHA))
	}

	moved := r.moveHead(task)
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	cs := r.cycles(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 3 || r.fixPrompts() != 1 || len(cs) != 3 || cs[2].HeadBefore != moved || cs[2].Exit != registry.ExitCap {
		t.Fatalf("task %s at %s waits %q after %d rounds, %d fix turns and cycles %+v, want one round at %s that ends at the limit",
			got.State, got.Stage, got.WaitsWhat, len(r.rv.calls()), r.fixPrompts(), cs, short(moved))
	}
	sum := r.summary(task.ID)
	if want := []string{capFinding(5, "P2").Title, capFinding(6, "P2").Title}; sum.Exit != exitRoundLimit || sum.Head != moved || sum.TaskRounds != 3 ||
		sum.Limit != 2 || !slices.Equal(leftTitles(sum), want) {
		t.Fatalf("summary %+v, want the limit's end after 3 rounds at %s with round 3's findings left", sum, short(moved))
	}
	if _, why := readyRecommendation(ReadyCardFile{Head: moved, Review: sum}, nil); !strings.Contains(why, "round limit (2 rounds; it ran 3)") {
		t.Errorf("recommendation %q, want the limit and the task's 3 rounds", why)
	}
}

// A product question still open at max_rounds makes the stop unsafe: only
// you answer it, so the task waits on you rather than passing the question
// to the ready card's KNOWN unasked (where the card's w would not send it,
// as one you decided). Your retry asks it first, as the cap's retry does.
func TestAnOpenProductQuestionAtMaxRoundsWaitsOnYou(t *testing.T) {
	q := evalrun.LocalFinding{Severity: "P2", Title: "Should a refund take back the points it gave?", Path: "app/credit.rb", Line: 12, Question: true,
		Body: "The ticket says nothing of refunds."}
	r := newReviewRig(t, result(capFinding(1, "P2"), q))
	r.cfg.Factory.LocalReview.MaxRounds = 1
	task, _ := r.reviewTask("PR-12518")
	r.run(8, func() bool { return !isLive(r.reload(task.ID)) || r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	if cs := r.cycles(task.ID); got.State != registry.TaskNeedsYou || len(cs) != 1 || cs[0].Exit != registry.ExitCap {
		t.Fatalf("task %s at %s waits %q; cycles %+v, want it stopped for you at round 1", got.State, got.Stage, got.WaitsWhat, cs)
	}
	for _, want := range []string{"at the round limit (1 round)", "not a safe stop: a product question is open"} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the wait %q lacks %q", got.WaitsWhat, want)
		}
	}
	if fileIn(r.layout.FactoryTaskDir(task.ID), reviewSummaryFile) {
		t.Error("a summary was written: the stage passed with the question unasked")
	}
	r.letGoOn(task.ID)
	r.run(4, func() bool { return !isLive(r.reload(task.ID)) })
	qs, err := r.reg.Questions(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Text != q.Title || qs[0].State != registry.QuestionOpen || r.fixPrompts() != 0 {
		t.Fatalf("questions %+v and %d writer prompts after your retry, want the question asked before any fix", qs, r.fixPrompts())
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

// A review that passed at the round limit passed at its head for QA too:
// QA's fix moves the head, one round reviews it and ends at the limit
// again, and QA's fresh run at the fix comes next, then pr_body. QA read
// only a clean or an oscillation cycle as a pass, so a task past the limit
// whose QA found defects went back to the review at every pass and never
// reached a QA run at the fix (task 9 runs QA after its local review).
func TestAReviewThatPassedAtMaxRoundsLetsQAWalkItsFix(t *testing.T) {
	r := newQARig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...))
	r.cfg.Factory.LocalReview.MaxRounds = 1
	task, head := r.reviewTask("PR-12516")
	r.taskFile(task.ID, acceptanceFile, qaAcceptance)
	if got := r.toQAAt(task, 1); got != head {
		t.Fatalf("QA at %s, want the head round 1 reviewed %s", short(got), short(head))
	}
	if sum := r.summary(task.ID); sum.Exit != exitRoundLimit || sum.Head != head {
		t.Fatalf("summary %+v, want the round limit's end at %s", sum, short(head))
	}
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	fix := r.toQAAt(task, 2)
	if sum := r.summary(task.ID); fix == head || sum.Exit != exitRoundLimit || sum.Head != fix || sum.TaskRounds != 2 {
		t.Fatalf("summary %+v, want the round limit's end after 2 rounds at the fix %s", sum, short(fix))
	}
	r.toWalker(task)
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 2 || run.Head != fix {
		t.Fatalf("qa-run.json %+v, want run 2 at the fix %s", run, short(fix))
	}
	r.walk(task, oneSecurityPage(), cleanProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s at %s waits %q: %q after %d review rounds, want pr_body after 2", got.State, got.Stage, got.WaitsWho, got.WaitsWhat,
			len(r.rv.calls()))
	}
	if evs := r.eventsWith(task.ID, "qa", "back to local_review"); len(evs) != 1 {
		t.Errorf("qa events %q, want one move back to the review, for the fix", evs)
	}
	r.qaFree(task.ID)
}

// Every end that passes the review stage with its cycle ended cap or
// operator passes it for QA at the summary's head: the round limit's, yours
// and the writer's fix at the cap; a stop for you (exit cap) or another
// head does not.
func TestQAReadsAReviewThatPassedWithNoCleanCycleAsPassed(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12517")
	for _, tc := range []struct {
		exit, head string
		want       bool
	}{
		{exitRoundLimit, head, true},
		{exitByYou, head, true},
		{exitCapFixed, head, true},
		{registry.ExitCap, head, false},
		{exitRoundLimit, "0123456789abcdef0123456789abcdef01234567", false},
	} {
		b, err := json.Marshal(ReviewSummary{Exit: tc.exit, Rounds: 1, Head: tc.head})
		if err != nil {
			t.Fatal(err)
		}
		r.taskFile(task.ID, reviewSummaryFile, string(b))
		if got, err := r.e.qaFixReviewed(r.ctx, task); err != nil || got != tc.want {
			t.Errorf("a summary of exit %s at %s: reviewed %v (%v), want %v", tc.exit, short(tc.head), got, err, tc.want)
		}
	}
}
