package factory

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// finish sends your finish-review of task id, as the CLI and the screen
// do, and returns the daemon's answer; it fails the test when the daemon
// refuses.
func (r *reviewRig) finish(id int64) string {
	r.t.Helper()
	q := r.finishRequest(id)
	if q.State != registry.RequestDone {
		r.t.Fatalf("your finish-review: %+v", q)
	}
	return q.Result
}

// finishRequest sends your finish-review of task id and returns the
// request as the daemon answered it.
func (h *harness) finishRequest(id int64) registry.Request {
	h.t.Helper()
	qid, err := h.reg.EnqueueRequest(h.ctx, ReqFinishReview, FinishReviewPayload{TaskID: id, Surface: "cli"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.e.handleRequests(h.ctx)
	return h.request(qid)
}

// The findings' titles of items.
func titlesOf(items []ReviewItem) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.Title)
	}
	return out
}

// `magnum factory finish-review` lets the round in flight finish, never
// cutting it, and starts no round after it: a safe stop passes the stage
// with the round's open findings under the ready card's KNOWN, the card
// saying the review was ended by you. Your request is answered once.
func TestFinishReviewLetsTheRoundFinishThenGoesToTheReadyCard(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...), result())
	task, _ := r.reviewTask("PR-12521")
	first := r.toFixPrompt(task, 1)
	r.writerFixes(task, first.N, fixedCredit(1), nil)
	r.run(8, func() bool { cs := r.cycles(task.ID); return len(cs) == 1 && !cs[0].Ended.IsZero() })
	ch := r.blockNextRound()
	answer := r.finish(task.ID)
	for _, want := range []string{"PR-12521", "round 2 of its local review runs: it finishes", "no round starts after it", "ended by you"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the answer %q lacks %q", answer, want)
		}
	}
	if cs := r.cycles(task.ID); len(cs) != 2 || !cs[1].Ended.IsZero() || !isLive(r.reload(task.ID)) {
		t.Fatalf("cycles %+v, task %s: want round 2 still running", cs, r.reload(task.ID).State)
	}
	r.unblock()
	close(ch)
	r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 2 || r.fixPrompts() != 1 {
		t.Fatalf("task %s at %s waits %q after %d rounds and %d fix turns, want past the review at round 2's end",
			got.State, got.Stage, got.WaitsWhat, len(r.rv.calls()), r.fixPrompts())
	}
	if cs := r.cycles(task.ID); len(cs) != 2 || cs[1].Exit != registry.ExitOperator {
		t.Fatalf("cycles %+v, want round 2's ended by the operator", cs)
	}
	sum := r.summary(task.ID)
	if want := []string{capFinding(3, "P2").Title, capFinding(4, "P2").Title}; sum.Exit != exitByYou || sum.Head != got.HeadSHA ||
		!slices.Equal(leftTitles(sum), want) || len(sum.FixedAfter) != 0 {
		t.Fatalf("summary %+v, want exit %s at %s with round 2's findings left", sum, exitByYou, short(got.HeadSHA))
	}
	choice, why := readyRecommendation(ReadyCardFile{Head: got.HeadSHA, Review: sum}, nil)
	if choice != ChoicePush || !strings.Contains(why, "ended by you after 2 rounds") {
		t.Errorf("recommendation %s (%q), want push saying you ended it", choice, why)
	}
	if fileIn(r.layout.FactoryTaskDir(task.ID), finishFile) {
		t.Error("your request outlived its answer")
	}
	r.taskFile(task.ID, reportFile, "## Cause\nthe merger maps the winners\n\n## Plan\n- map each winner\n")
	r.taskFile(task.ID, prBodyFile, "## Summary\n\nThe change.\n")
	if _, why := r.e.readyProblem(got); why != "" {
		t.Errorf("the ready card is refused: %s", why)
	}
}

// Between rounds (the writer fixed the last round's findings, and the next
// round waits) your finish-review starts no round: a safe stop passes the
// stage at once at the writer's head, the last round's findings it fixed
// under the card's FIXED AFTER, as no round reviewed them.
func TestFinishReviewBetweenRoundsPassesAtTheWritersFix(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result())
	task, _ := r.reviewTask("PR-12522")
	first := r.toFixPrompt(task, 1)
	r.magnum.set("magnum reviews example/shop#9 now")
	r.writerFixes(task, first.N, fixedCredit(1), nil)
	fixed := r.git(r.copyDir, "rev-parse", "HEAD")
	r.run(8, func() bool { return r.reload(task.ID).WaitsWho == waitMagnum })
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Ended.IsZero() || r.reload(task.ID).HeadSHA != fixed {
		t.Fatalf("cycles %+v, head %s: want round 1's fix closed at %s", cs, short(r.reload(task.ID).HeadSHA), short(fixed))
	}
	if answer := r.finish(task.ID); !strings.Contains(answer, "no round runs") {
		t.Errorf("the answer %q, want it to say no round runs", answer)
	}
	r.run(4, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 1 || got.HeadSHA != fixed {
		t.Fatalf("task %s at %s after %d rounds, want past the review at the writer's fix with no round more", got.State, got.Stage, len(r.rv.calls()))
	}
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitOperator {
		t.Fatalf("cycles %+v, want round 1's loop closed by the operator", cs)
	}
	sum := r.summary(task.ID)
	if want := []string{capFinding(1, "P2").Title, capFinding(2, "P2").Title}; sum.Exit != exitByYou || sum.Head != fixed ||
		!slices.Equal(titlesOf(sum.FixedAfter), want) || len(sum.Leftovers) != 0 {
		t.Fatalf("summary %+v, want exit %s at the fix %s with round 1's findings fixed after", sum, exitByYou, short(fixed))
	}
	if _, why := readyRecommendation(ReadyCardFile{Head: fixed, Review: sum}, nil); !strings.Contains(why, "ended by you") ||
		!strings.Contains(why, "FIXED AFTER") {
		t.Errorf("recommendation %q, want it ended by you, the fixes after its last round named", why)
	}
	if lines := fixedAfterLines(sum, false); len(lines) != 2 {
		t.Errorf("FIXED AFTER %q, want round 1's two findings", lines)
	}
}

// A finish-review whose stop is not safe (a P1 open) waits on you, at a
// round's end or between rounds, its words saying you ended the review.
// Your request is spent: your retry lets the loop go on, its findings to
// the writer first when the stop came at a round's end, and the next round
// reviews the fix.
func TestAnUnsafeFinishWaitsOnYou(t *testing.T) {
	t.Run("at a round's end", func(t *testing.T) {
		r := newReviewRig(t, result(capFindings(1, 2, 1)...), result())
		ch := make(chan struct{})
		r.rv.block = ch
		task, _ := r.reviewTask("PR-12523")
		r.startBlockedRound()
		r.finish(task.ID)
		r.unblock()
		close(ch)
		r.run(12, func() bool { return !isLive(r.reload(task.ID)) })
		got := r.reload(task.ID)
		if cs := r.cycles(task.ID); got.State != registry.TaskNeedsYou || len(cs) != 1 || cs[0].Exit != registry.ExitOperator {
			t.Fatalf("task %s waits %q; cycles %+v, want it stopped for you by the operator's end", got.State, got.WaitsWhat, cs)
		}
		if !strings.Contains(got.WaitsWhat, "you ended the local review after 1 round") || !strings.Contains(got.WaitsWhat, "not a safe stop: a P0 or P1 is open") {
			t.Errorf("the wait %q should say you ended it and why it waits", got.WaitsWhat)
		}
		r.letGoOn(task.ID)
		r.toFixPrompt(task, 1)
		r.writerFixes(task, 1, fixedCredit(1), nil)
		r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
		if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 || r.summary(task.ID).Exit != registry.ExitClean {
			t.Fatalf("task %s at %s after %d rounds, want a clean round 2 after the fix", got.State, got.Stage, len(r.rv.calls()))
		}
	})
	t.Run("between rounds", func(t *testing.T) {
		r := newReviewRig(t, result(capFindings(1, 2, 1)...), result())
		task, _ := r.reviewTask("PR-12524")
		first := r.toFixPrompt(task, 1)
		r.magnum.set("magnum reviews example/shop#9 now")
		r.writerFixes(task, first.N, fixedCredit(1), nil)
		r.run(8, func() bool { return r.reload(task.ID).WaitsWho == waitMagnum })
		r.finish(task.ID)
		r.run(2, func() bool { return !isLive(r.reload(task.ID)) })
		got := r.reload(task.ID)
		if cs := r.cycles(task.ID); got.State != registry.TaskNeedsYou || len(cs) != 1 || cs[0].Exit != registry.ExitOperator {
			t.Fatalf("task %s waits %q; cycles %+v, want it stopped for you by the operator's end", got.State, got.WaitsWhat, cs)
		}
		if !strings.Contains(got.WaitsWhat, "you ended the local review after 1 round") || !strings.Contains(got.WaitsWhat, "a P0 or P1 is open") {
			t.Errorf("the wait %q should say you ended it and why it waits", got.WaitsWhat)
		}
		r.magnum.set("")
		r.letGoOn(task.ID)
		r.run(12, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
		if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 || r.fixPrompts() != 1 {
			t.Fatalf("task %s at %s after %d rounds and %d fix turns, want round 2 on the fix, no fix turn more",
				got.State, got.Stage, len(r.rv.calls()), r.fixPrompts())
		}
	})
}

// finish-review refuses a task that is not in its local review at work,
// saying why: one at another stage, one that waits on you, one done.
func TestFinishReviewRefusesATaskOutsideItsLocalReview(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		task registry.Task
		want string
	}{
		{h.task("PR-12525", registry.TaskActive, "coding"), "PR-12525 is at its coding stage, not in its local review"},
		{h.task("PR-12526", registry.TaskNeedsYou, "local_review"), "PR-12526 waits on you"},
		{h.task("PR-12527", registry.TaskDone, "publish"), "PR-12527 is done"},
	} {
		q := h.finishRequest(tc.task.ID)
		if q.State != registry.RequestFailed || !strings.Contains(q.Result, tc.want) {
			t.Errorf("%s: request %s %q, want refused with %q", tc.task.JiraKey, q.State, q.Result, tc.want)
		}
		if fileIn(h.layout.FactoryTaskDir(tc.task.ID), finishFile) {
			t.Errorf("%s: a refused request left its mark", tc.task.JiraKey)
		}
	}
	at := h.task("PR-12528", registry.TaskActive, "local_review")
	if err := FinishReviewRefusal(h.cfg, at); err != nil {
		t.Errorf("a task at work in its local review is refused: %v", err)
	}
}
