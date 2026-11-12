package factory

import (
	"encoding/json"
	"errors"
	"regexp"
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
// remembers the kept change by its path and its file's content, so no later
// look at the copy counts it as a round's (nothing puts it back, nothing
// asks again), an index change (`git add`) leaves it kept, and every writer
// prompt of the attempt, its first included, names it to commit, never to
// undo, as do the checks after the writer's fix; the next round then
// reviews it, and its result stands. A kept change that changed since is a
// round's again.
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
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 || !strings.Contains(ps[0].Text, keptNoteLead) ||
		!strings.Contains(ps[0].Text, "(README)") || !strings.Contains(ps[0].Text, "never undo them") {
		t.Fatalf("the writer's first prompt after your keep %+v; want the kept change named, to commit and never undo", ps)
	}

	cy := r.cycles(task.ID)[0]
	r.git(r.copyDir, "add", "README") // an index change: the kept change is the same file
	if left, err := r.e.roundLeft(r.ctx, r.reload(task.ID), r.cp, cy.HeadBefore, cy.N); err != nil || !left.none() {
		t.Fatalf("a later look at the copy finds %s (%v); want none: the kept change is no round's", left, err)
	}
	r.git(r.copyDir, "reset", "--quiet", "--", "README")
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
		strings.Contains(ps[1].Text, "commit them or undo them") {
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
			first := true
			r := leavingRig(t, func(r *reviewRig, dir string) {
				if first { // the first round alone: a later one finds the runtime answering again
					first = false
					r.put(dir, "README", "a reviewer's mutant\n")
					r.rt.setAtWorkErr(errors.New("herdr: socket gone"))
				}
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
			rounds := 1
			if tc.answer == optionKeepChange { // the writer commits what you kept; no round follows
				r.rt.setAtWorkErr(nil)
				r.commitsKept(task, 1, tc.readme)
			}
			r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) || len(r.rv.calls()) != rounds {
				t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body after %d", got.State, got.Stage, got.WaitsWhat,
					len(r.rv.calls()), rounds)
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

// A round that moved the task's branch back, behind the head it reviewed,
// is never offered to be kept: keeping it would drop the writer's commits.
// The question offers to restore the copy (the branch moves forward to the
// reviewed head) or to drop the task, and restore lets the round's result
// stand.
func TestABranchMovedBehindTheReviewedHeadOffersOnlyRestoreOrDrop(t *testing.T) {
	var rig *reviewRig
	rig = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		rig.gitIn(c.Checkout, "reset", "--quiet", "--keep", "HEAD~1")
		return result()(c)
	})
	r := rig
	task, head := r.reviewTask("PR-12406")
	r.run(5, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	qs := r.newestQuestions(task.ID)
	if got.State != registry.TaskNeedsYou || len(qs) != 1 || !slices.Equal(optionLabels(qs[0]), []string{optionRestoreCopy, optionDrop}) ||
		!strings.Contains(qs[0].Options[0].Means, "forward to "+short(head)) {
		t.Fatalf("task %s waits %q with %+v; want restore (forward to %s) or drop, never keep", got.State, got.WaitsWhat, qs, short(head))
	}
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionRestoreCopy}})
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || r.git(r.copyDir, "rev-parse", task.Branch) != head {
		t.Fatalf("task %s at %s waits %q, the branch at %s; want pr_body with the branch back at %s", got.State, got.Stage,
			got.WaitsWhat, r.git(r.copyDir, "rev-parse", task.Branch), head)
	}
}

// A kept change that leaves the copy without a commit (undone, put back by
// hand) leaves the kept list on the loop, with an event and an alert that
// say so and name the diff saved when you kept it, and the writer's
// prompts no longer name it. The checks' read of it, off the loop, writes
// nothing.
func TestAKeptChangeGoneWithoutACommitIsSaidSo(t *testing.T) {
	r, got := leftAfterYouTyped(t, "PR-12407")
	r.rt.setTouchedAt("", time.Time{})
	r.answerLeft(got.ID, optionKeepChange)
	r.run(2, nil)
	if rec, err := readStops(r.layout.FactoryTaskDir(got.ID)); err != nil || len(rec.Kept) != 1 {
		t.Fatalf("stops.json keeps %+v (%v); want the one kept change", rec.Kept, err)
	}
	r.git(r.copyDir, "checkout", "--", "README")
	r.e.dirtyWords(r.ctx, r.reload(got.ID), r.cp, []string{"app/credit.rb"}) // the checks' read, off the loop: it writes nothing
	if rec, _ := readStops(r.layout.FactoryTaskDir(got.ID)); len(rec.Kept) != 1 ||
		slices.ContainsFunc(r.events(got.ID, "review"), func(e registry.Event) bool { return strings.Contains(e.Text, "without a commit") }) {
		t.Fatalf("the checks' read wrote stops.json (%+v) or an event; want it read only", rec.Kept)
	}
	if note := r.e.keptNote(r.ctx, r.reload(got.ID), r.cp); note != "" {
		t.Fatalf("the kept note %q after the change is gone; want none", note)
	}
	if rec, _ := readStops(r.layout.FactoryTaskDir(got.ID)); len(rec.Kept) != 0 {
		t.Fatalf("stops.json still keeps %+v", rec.Kept)
	}
	gone := func(e registry.Event) bool {
		return strings.Contains(e.Text, "README") && strings.Contains(e.Text, "without a commit") &&
			regexp.MustCompile(`review-1-left(-\d+)?\.diff`).MatchString(e.Text)
	}
	if !slices.ContainsFunc(r.events(got.ID, "review"), gone) || !slices.ContainsFunc(r.events(got.ID, "alert"), gone) {
		t.Fatal("no event and alert say the kept change is gone without a commit, naming its saved diff")
	}
}

// A commit that takes the kept file with more edits of it is a commit of
// it, never a loss: the event says it is committed with more edits, and no
// alert says it is gone.
func TestAKeptChangeCommittedWithMoreEditsIsNoLoss(t *testing.T) {
	r, got := leftAfterYouTyped(t, "PR-12412")
	r.rt.setTouchedAt("", time.Time{})
	r.answerLeft(got.ID, optionKeepChange)
	r.run(2, nil)
	r.clock.Add(time.Minute)
	r.commit("PR-12412 the operator's edit, and more", map[string]string{"README": "your edit, or a reviewer's, and more\n"})
	if note := r.e.keptNote(r.ctx, r.reload(got.ID), r.cp); note != "" {
		t.Fatalf("the kept note %q after its commit; want none", note)
	}
	if !slices.ContainsFunc(r.events(got.ID, "review"), func(e registry.Event) bool {
		return strings.Contains(e.Text, "README") && strings.Contains(e.Text, "committed with more edits")
	}) || slices.ContainsFunc(r.events(got.ID, "alert"), func(e registry.Event) bool { return strings.Contains(e.Text, "without a commit") }) {
		t.Fatal("want an event that the kept change is committed with more edits, and no alert of a loss")
	}
}

// commitsKept is the writer's short turn after a keep: review-<n>.json lists
// no finding, the prompt names the kept change; the writer commits it
// (readme, README's text) and replies.
func (r *reviewRig) commitsKept(task registry.Task, n int, readme string) {
	r.t.Helper()
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 1 || !strings.Contains(ps[0].Text, keptNoteLead) || !strings.Contains(ps[0].Text, "(README)") {
		r.t.Fatalf("the writer's prompts %+v; want one short turn naming the kept change", ps)
	}
	var rf ReviewFile
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewFile(n))), &rf); err != nil || len(rf.Findings) != 0 ||
		!slices.Equal(rf.Kept, []string{"README"}) {
		r.t.Fatalf("%s: %+v (%v); want no finding and the kept change", reviewFile(n), rf, err)
	}
	r.writerFixes(task, n, map[string]string{"README": readme}, nil)
}

// A change you keep after a round that blocks nothing is committed by the
// writer before the stage goes on, never left for the next stage to stop
// on: the clean round's summary (its P3 left open) goes to
// review-summary.json first, the writer gets one short turn
// (review-<n>.json with no finding, the kept change named, no round to
// follow), commits it, and once its proof and precheck pass the stage
// passes at the commit, with no round more: a keep never costs a round,
// even at the loop's cap of one.
func TestAKeptChangeAfterACleanRoundIsCommittedByTheWriter(t *testing.T) {
	var r *reviewRig
	r = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		r.put(c.Checkout, "README", "your edit, or a reviewer's\n")
		r.rt.setTouchedAt("you typed into a pane of PR-12408 at 10:00:00", r.clock.Now().Add(time.Second))
		return result(nitFinding)(c)
	})
	r.cfg.Factory.LocalReview.Cycles = 1
	task, _ := r.reviewTask("PR-12408")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	r.rt.setTouchedAt("", time.Time{})
	r.answerLeft(task.ID, optionKeepChange)
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil || len(sum.Leftovers) != 1 ||
		sum.Leftovers[0].Title != nitFinding.Title {
		t.Fatalf("%s before the commit turn: %+v (%v); want the clean round's P3 kept", reviewSummaryFile, sum, err)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 || !strings.Contains(ps[0].Text, "no review round follows") {
		t.Fatalf("the writer's prompts %+v; want the short turn, saying no round follows", ps)
	}
	r.commitsKept(task, 1, "your edit, or a reviewer's\n")
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	after := r.reload(task.ID)
	if after.Stage != "pr_body" || !isLive(after) || len(r.rv.calls()) != 1 {
		t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body after the one round, no round for the kept commit",
			after.State, after.Stage, after.WaitsWhat, len(r.rv.calls()))
	}
	if head := r.git(r.copyDir, "show", "HEAD:README"); head != "your edit, or a reviewer's" {
		t.Fatalf("README at HEAD is %q; want your kept change committed", head)
	}
	sum = ReviewSummary{}
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil || sum.Head != after.HeadSHA ||
		sum.Exit != registry.ExitClean || len(sum.Leftovers) != 1 {
		t.Fatalf("%s: %+v (%v); want it clean at the kept commit %s, its P3 kept", reviewSummaryFile, sum, err, after.HeadSHA)
	}
}

// Your own words on a question that offers only restore or drop (a keep
// would drop the task's commits) never take the restore, which resets a
// branch: the factory asks again, saying that only Restore or Drop are
// possible there, and moves nothing.
func TestYourWordsOnARestoreOrDropQuestionAskAgain(t *testing.T) {
	var rig *reviewRig
	rig = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		rig.gitIn(c.Checkout, "reset", "--quiet", "--keep", "HEAD~1")
		return result()(c)
	})
	r := rig
	task, head := r.reviewTask("PR-12409")
	r.run(5, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	moved := r.git(r.copyDir, "rev-parse", task.Branch)
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: "keep my branch as it is"}})
	r.run(4, func() bool { return r.reload(task.ID).QuestionRev != got.QuestionRev })
	after := r.reload(task.ID)
	qs := r.newestQuestions(task.ID)
	if after.State != registry.TaskNeedsYou || after.QuestionRev == got.QuestionRev || len(qs) != 1 ||
		!strings.Contains(qs[0].Text, "only Restore or Drop") || !slices.Equal(optionLabels(qs[0]), []string{optionRestoreCopy, optionDrop}) {
		t.Fatalf("task %s waits %q with %+v; want the question again, saying only Restore or Drop are possible", after.State,
			after.WaitsWhat, qs)
	}
	if now := r.git(r.copyDir, "rev-parse", task.Branch); now != moved || now == head {
		t.Fatalf("the branch is at %s after your words; want it left at %s", now, moved)
	}
}

// A named agent's run that the watchdog judged stuck (or its pane lost) is
// no live turn: the put-back waits for it as for a runtime that cannot
// tell, at most leftWaitBound, then asks you, and the restore names that
// turn: its edits in those paths go back too, saved, and its new files
// leave the copy.
func TestARunTheWatchdogJudgedStuckHoldsThePutBackAtMostTheBound(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(dir, "README", "a reviewer's mutant, or the writer's\n")
		r.rt.setAtWork("fx-writer")
		r.rt.setJudged("fx-writer", "the watchdog judged its turn stuck")
	})
	task, _ := r.reviewTask("PR-12410")
	r.run(6, func() bool { return strings.Contains(r.reload(task.ID).WaitsWhat, "fx-writer") })
	r.clock.Add(leftWaitBound - time.Minute)
	r.run(2, nil)
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou {
		t.Fatalf("task waits on you %q before the bound", got.WaitsWhat)
	}
	r.clock.Add(2 * time.Minute)
	r.run(4, func() bool { return r.reload(task.ID).State == registry.TaskNeedsYou })
	got := r.reload(task.ID)
	qs := r.newestQuestions(task.ID)
	if got.State != registry.TaskNeedsYou || len(qs) != 1 || !strings.Contains(qs[0].Text, "judged its turn stuck") ||
		!strings.Contains(qs[0].Options[0].Means, "fx-writer agent's turn has its edits in those paths go back too (saved to review-1-left.diff)") {
		t.Fatalf("task %s waits %q with %+v; want the question naming the stuck turn and what its restore takes", got.State,
			got.WaitsWhat, qs)
	}
	if r.read("README") != "a reviewer's mutant, or the writer's\n" {
		t.Fatal("the factory restored the copy before your answer")
	}
}

// An answer to the question of a change that may be yours is never run
// while the factory cannot tell who changed the copy since (a runtime whose
// TouchedSince fails): it asks again, with the warning of a factory that
// cannot tell, and your answer to that question runs the restore.
func TestAnAnswerIsNeverRunWhileTheFactoryCannotTellWhoChangedTheCopy(t *testing.T) {
	r, got := leftAfterYouTyped(t, "PR-12411")
	r.rt.setTouchedAt("", time.Time{})
	r.rt.setTouchedErr(errors.New("herdr: socket gone"))
	r.answerLeft(got.ID, optionRestoreCopy)
	r.run(4, func() bool { return r.reload(got.ID).QuestionRev != got.QuestionRev })
	again := r.reload(got.ID)
	qs := r.newestQuestions(got.ID)
	if again.State != registry.TaskNeedsYou || len(qs) != 1 || !strings.Contains(qs[0].Text, "cannot tell") ||
		r.read("README") != "your edit, or a reviewer's\n" {
		t.Fatalf("task %s waits %q with %+v, README %q; want the question again, cannot tell, nothing restored", again.State,
			again.WaitsWhat, qs, r.read("README"))
	}
	r.answerLeft(got.ID, optionRestoreCopy)
	r.run(6, func() bool { return r.reload(got.ID).Stage != "local_review" || !isLive(r.reload(got.ID)) })
	if r.read("README") != "shop\n" {
		t.Fatalf("README is %q after your answer to the cannot-tell question; want it restored", r.read("README"))
	}
}
