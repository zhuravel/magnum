package factory

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// answerLeft answers task id's restore-or-keep question with answer.
func (r *reviewRig) answerLeft(id int64, answer string) {
	r.t.Helper()
	got := r.reload(id)
	qs := r.newestQuestions(id)
	if got.State != registry.TaskNeedsYou || len(qs) != 1 || !slices.Equal(optionLabels(qs[0]), []string{optionRestoreCopy, optionKeepChange}) {
		r.t.Fatalf("task %s waits %q with questions %+v; want the restore-or-keep question", got.State, got.WaitsWhat, qs)
	}
	r.mustAnswer(AnswerPayload{TaskID: id, Revision: got.QuestionRev, Answers: map[int]string{0: answer}})
}

// putBackAlerts are task id's alerts that say the factory put a round's
// leftovers back by itself.
func (r *reviewRig) putBackAlerts(id int64) int {
	n := 0
	for _, ev := range r.events(id, "alert") {
		if strings.Contains(ev.Text, "put back by itself") {
			n++
		}
	}
	return n
}

// "Keep the change" holds while the change stays the same: the factory
// remembers the kept change by its fingerprint, so no later look at the
// copy counts it as a round's (nothing puts it back, nothing asks again),
// and its checks after the writer's fix ask the writer to commit it, never
// to undo it; the next round then reviews it, and its result stands. A
// kept change that changed since is a round's again.
func TestAKeptChangeStaysThroughTheLaterRoundsWhileItIsTheSame(t *testing.T) {
	var r *reviewRig
	r = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		r.put(c.Checkout, "README", "your edit, or a reviewer's\n")
		r.rt.setTouchedAt("you typed into a pane of PR-12401 at 10:00:00", r.clock.Now().Add(time.Second))
		return result(refundFinding)(c)
	}, result())
	task, _ := r.reviewTask("PR-12401")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	r.rt.setTouchedAt("", time.Time{})
	r.answerLeft(task.ID, optionKeepChange)
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })

	cy := r.cycles(task.ID)[0]
	if left, err := r.e.roundLeft(r.ctx, r.reload(task.ID), r.cp, cy.HeadBefore, cy.N); err != nil || !left.none() {
		t.Fatalf("a later look at the copy finds %s (%v); want none: the kept change is no round's", left, err)
	}
	r.write(r.copyDir, "README", "your edit, and a reviewer's more\n")
	if left, err := r.e.roundLeft(r.ctx, r.reload(task.ID), r.cp, cy.HeadBefore, cy.N); err != nil ||
		!slices.Equal(left.tracked, []string{"README"}) {
		t.Fatalf("a later look at the copy finds %s (%v); want README: a kept change that changed is a round's again", left, err)
	}
	r.write(r.copyDir, "README", "your edit, or a reviewer's\n")

	r.writerFixes(task, 1, fixedCredit(1), nil) // its commit takes app/credit.rb only
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.Contains(ps[1].Text, "the operator kept 1 tracked change in the copy (README): commit it") ||
		strings.Contains(ps[1].Text, "undo them") {
		t.Fatalf("the writer's prompts %+v; want a turn that commits the kept change, never undoes it", ps)
	}
	if r.read("README") != "your edit, or a reviewer's\n" {
		t.Fatal("the factory put back the change you kept")
	}
	r.clock.Add(time.Minute)
	r.commit(task.JiraKey+" keeps the operator's edit", map[string]string{"README": "your edit, or a reviewer's\n"})
	r.ended(task, "fx-writer")
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) || len(r.rv.calls()) < 2 {
		t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body on the later rounds' results", got.State, got.Stage,
			got.WaitsWhat, len(r.rv.calls()))
	}
	if got := r.git(r.copyDir, "show", "HEAD:README"); got != "your edit, or a reviewer's" || r.putBackAlerts(task.ID) != 0 {
		t.Fatalf("README at HEAD is %q with %d put-back alerts; want your kept change committed, never put back", got,
			r.putBackAlerts(task.ID))
	}
}

// A put-back waits for a runtime that cannot tell who works in the copy
// at most leftWaitBound, counted from that wait's first pass: then the
// factory asks you to restore the copy or keep the change, naming the wait.
// Restore puts it back though the runtime still cannot tell (your answer),
// and keep leaves it; the round's result stands either way.
func TestAPutBackThatCannotTellPastItsBoundAsksToRestoreOrKeep(t *testing.T) {
	for _, tc := range []struct{ answer, readme string }{
		{optionRestoreCopy, "shop\n"},
		{optionKeepChange, "a reviewer's mutant\n"},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant\n")
				r.rt.setAtWorkErr(errors.New("herdr: socket gone"))
			})
			task, _ := r.reviewTask("PR-12402")
			r.run(6, func() bool { return strings.Contains(r.reload(task.ID).WaitsWhat, "socket gone") })
			if got := r.reload(task.ID); got.State == registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "socket gone") {
				t.Fatalf("task %s waits %q; want a held wait first", got.State, got.WaitsWhat)
			}
			r.clock.Add(leftWaitBound - time.Minute)
			r.run(2, nil)
			if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
				t.Fatalf("task waits on you %q before the bound", got.WaitsWhat)
			}
			r.clock.Add(2 * time.Minute)
			r.run(4, func() bool { return r.reload(task.ID).State == registry.TaskNeedsYou })
			got := r.reload(task.ID)
			if qs := r.newestQuestions(task.ID); !strings.Contains(got.WaitsWhat, "waited 30m") || len(qs) != 1 ||
				!strings.Contains(qs[0].Text, "socket gone") {
				t.Fatalf("task %s waits %q with questions %+v; want the restore-or-keep question naming the wait", got.State,
					got.WaitsWhat, qs)
			}
			if r.read("README") != "a reviewer's mutant\n" || !r.saved(task.ID, "review-1-left.diff") {
				t.Fatal("the factory put back the change before your answer, or did not save it")
			}
			r.answerLeft(task.ID, tc.answer)
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) || len(r.rv.calls()) != 1 {
				t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body on the round's result", got.State, got.Stage,
					got.WaitsWhat, len(r.rv.calls()))
			}
			if got := r.read("README"); got != tc.readme {
				t.Fatalf("README is %q after your answer, want %q", got, tc.readme)
			}
		})
	}
}

// A named agent's turn in flight in the copy holds the put-back however
// long it runs: a turn of your own request in the writer's pane that runs
// past leftWaitBound has nothing restored under it, and nothing is asked
// while it runs (the runtime's watchdog judges a turn that makes no
// progress). Once it ended, the change may be the turn's, so the factory
// asks you, never restoring by itself.
func TestATurnInFlightPastTheBoundHasNothingRestoredUnderIt(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(dir, "README", "a reviewer's mutant, or the writer's\n")
		r.rt.setAtWork("fx-writer") // your request to the writer, at work in its pane
	})
	task, _ := r.reviewTask("PR-12403")
	r.run(6, func() bool { return strings.Contains(r.reload(task.ID).WaitsWhat, "fx-writer") })
	for range 3 {
		r.clock.Add(leftWaitBound)
		r.run(2, nil)
		if got := r.reload(task.ID); got.State == registry.TaskNeedsYou || r.read("README") != "a reviewer's mutant, or the writer's\n" {
			t.Fatalf("task %s waits %q, README %q while the writer's turn runs; want a wait, nothing restored", got.State,
				got.WaitsWhat, r.read("README"))
		}
	}
	r.rt.setAtWork("")
	r.rt.setTouchedAt("a prompt to the fx-writer agent at 10:05:00", r.clock.Now())
	r.run(4, func() bool { return r.reload(task.ID).State == registry.TaskNeedsYou })
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "a prompt to the fx-writer agent") ||
		r.read("README") != "a reviewer's mutant, or the writer's\n" {
		t.Fatalf("task %s waits %q, README %q after the turn; want the question, nothing restored", got.State, got.WaitsWhat,
			r.read("README"))
	}
}

// Your answer is for the cause it was asked for: a turn that started after
// it (a prompt to the writer, your input in a pane) is never run over.
// Restore, held when you gave it (the runtime could not tell who works in
// the copy), waits for that turn, then asks you again with the new cause.
func TestAnAnswerIsNeverRunOverATurnThatStartedAfterIt(t *testing.T) {
	r, got := leftAfterYouTyped(t, "PR-12404")
	r.rt.setTouchedAt("", time.Time{})
	r.rt.setAtWorkErr(errors.New("herdr: socket gone"))
	r.answerLeft(got.ID, optionRestoreCopy)
	r.run(2, nil)
	r.clock.Add(time.Minute)
	r.rt.setTouchedAt("a prompt to the fx-writer agent at 10:20:00", r.clock.Now())
	r.rt.setAtWorkErr(nil)
	r.rt.setAtWork("fx-writer")
	r.run(3, nil)
	if r.read("README") != "your edit, or a reviewer's\n" {
		t.Fatal("the factory restored the copy under a turn that started after your answer")
	}
	r.rt.setAtWork("")
	r.run(4, func() bool { return r.reload(got.ID).State == registry.TaskNeedsYou })
	after := r.reload(got.ID)
	if after.State != registry.TaskNeedsYou || !strings.Contains(after.WaitsWhat, "a prompt to the fx-writer agent at 10:20:00") ||
		r.read("README") != "your edit, or a reviewer's\n" {
		t.Fatalf("task %s waits %q, README %q; want the question again with the new cause, nothing restored", after.State,
			after.WaitsWhat, r.read("README"))
	}
}

// The wait for a runtime that cannot tell counts from its own first pass:
// one that ended (the runtime named who works in the copy) starts over, so
// a later wait on the same round asks only after its own leftWaitBound.
func TestAWaitThatEndedStartsItsBoundOver(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(dir, "README", "a reviewer's mutant\n")
		r.rt.setAtWorkErr(errors.New("herdr: socket gone"))
	})
	task, _ := r.reviewTask("PR-12405")
	r.run(6, func() bool { return strings.Contains(r.reload(task.ID).WaitsWhat, "socket gone") })
	r.clock.Add(20 * time.Minute)
	r.run(2, nil)
	r.rt.setAtWorkErr(nil)
	r.rt.setAtWork("fx-writer") // the runtime answers again: the wait for it ended
	r.run(2, nil)
	r.rt.setAtWork("")
	r.rt.setAtWorkErr(errors.New("herdr: socket gone again"))
	r.run(2, nil) // a new wait begins
	r.clock.Add(20 * time.Minute)
	r.run(2, nil)
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
		t.Fatalf("task waits on you %q 20 minutes into a new wait; want its own bound", got.WaitsWhat)
	}
	r.clock.Add(11 * time.Minute)
	r.run(4, func() bool { return r.reload(task.ID).State == registry.TaskNeedsYou })
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "waited 30m") {
		t.Fatalf("task %s waits %q; want the question after the new wait's own bound", got.State, got.WaitsWhat)
	}
}
