package factory

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
)

// lintNewOffenseEachRun makes bin/lint name another offense on the changed
// line at each run: a writer that makes progress.
func (r *codingRig) lintNewOffenseEachRun() {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := 0
	r.lint = func([]string) string {
		runs++
		return fmt.Sprintf("app/credit.rb:3:5: C: Style/Rule%d: on the changed line\n", runs)
	}
}

// turnsWithCommits ends the writer's turns, each after a commit of its
// own (a new head the checks run on), n times or until the task leaves the
// live states.
func (r *codingRig) turnsWithCommits(task registry.Task, n int) {
	r.t.Helper()
	for i := range n {
		if !isLive(r.reload(task.ID)) {
			return
		}
		r.commit(fmt.Sprintf("%s try %d", task.JiraKey, i+1), map[string]string{"README": fmt.Sprintf("shop, try %d\n", i+1)})
		r.endTurns(task, 1)
	}
}

// grantsOf are task id's grants of fix turns, by kind.
func (r *codingRig) grantsOf(id int64) map[string]int {
	r.t.Helper()
	rec, err := readFixTurns(r.layout.FactoryTaskDir(id))
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]int{}
	for _, g := range rec.Grants {
		out[g.Kind]++
	}
	return out
}

// freshPrompts are the writer's prompts that started a fresh session.
func (r *codingRig) freshPrompts() []sentPrompt {
	return slices.DeleteFunc(r.rt.promptsTo("fx-writer"), func(p sentPrompt) bool { return !p.Fresh })
}

// At its cap of fix turns a writer whose last turns made progress (another
// offense each turn) gets fixGoOnTurns more fix turns by itself, at most
// fix_go_on times in the stage, each an event and an alert; then its one
// fresh session, and then the task asks you.
func TestAtTheFixCapAWriterThatMakesProgressGoesOnAtMostFixGoOnTimes(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12570", "bug")
	r.lintNewOffenseEachRun()
	r.startStage(task)
	r.fix(task)
	r.turnsWithCommits(task, 30)
	want := runtime.MaxStagePrompts + r.cfg.Factory.FixGoOn*fixGoOnTurns + fixGoOnTurns
	if n := r.writerPrompts(); n != want {
		t.Fatalf("the writer got %d prompts, want %d: the cap's, %d go-ons of %d, then its fresh session's", n, want,
			r.cfg.Factory.FixGoOn, fixGoOnTurns)
	}
	if g := r.grantsOf(task.ID); g[grantGoOn] != r.cfg.Factory.FixGoOn || g[grantFresh] != 1 {
		t.Fatalf("grants %v, want %d go-ons and one fresh session", g, r.cfg.Factory.FixGoOn)
	}
	var goOns, alerts int
	for _, ev := range r.events(task.ID, "fix") {
		if strings.Contains(ev.Text, "made progress") {
			goOns++
		}
	}
	for _, ev := range r.events(task.ID, "alert") {
		if strings.Contains(ev.Text, "goes on by itself") {
			alerts++
		}
	}
	if goOns != r.cfg.Factory.FixGoOn || alerts != r.cfg.Factory.FixGoOn+1 {
		t.Fatalf("%d go-on events and %d alerts, want one of each for each grant", goOns, alerts)
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "give a direction or drop the task?") {
		t.Fatalf("task %s waits %q, want the question", got.State, got.WaitsWhat)
	}
}

// fix_go_on = 0 turns the go-ons off: a writer that makes progress gets its
// fresh session at the cap, then the question.
func TestFixGoOnZeroGivesNoGoOn(t *testing.T) {
	r := newCodingRig(t)
	r.cfg.Factory.FixGoOn = 0
	task := r.codingTask("PR-12571", "bug")
	r.lintNewOffenseEachRun()
	r.startStage(task)
	r.fix(task)
	r.turnsWithCommits(task, 30)
	if n := r.writerPrompts(); n != capPrompts || r.grantsOf(task.ID)[grantGoOn] != 0 {
		t.Fatalf("the writer got %d prompts and grants %v, want %d and no go-on", n, r.grantsOf(task.ID), capPrompts)
	}
}

// Without progress (the same offense its last 3 turns) the writer gets one
// fresh session at its cap, whose first prompt names the failure, what it
// tried (its commits on the branch) and the failing output; when that
// session makes no progress either, the task asks you: accept the failing
// check as known, give a direction, or drop the task, never "retry".
func TestWithoutProgressTheWriterGetsOneFreshSessionThenAQuestion(t *testing.T) {
	r := newCodingRig(t)
	task := r.toPromptCap("PR-12572")
	fresh := r.freshPrompts()
	if len(fresh) != 1 {
		t.Fatalf("%d fresh sessions, want one", len(fresh))
	}
	for _, want := range []string{fixLead, "lint: 1 offense on the changed lines", freshFixLead,
		"PR-12572 Credit#points counts the activities", "Style/NumericLiterals"} {
		if !strings.Contains(fresh[0].Text, want) {
			t.Fatalf("the fresh session's prompt lacks %q:\n%s", want, fresh[0].Text)
		}
	}
	got := r.reload(task.ID)
	qs := r.newestQuestions(task.ID)
	if len(qs) != 1 || !slices.Equal(optionLabels(qs[0]), []string{optionAccept, optionDirection, optionDrop}) ||
		strings.Contains(strings.ToLower(got.WaitsWhat), "retry") {
		t.Fatalf("questions %+v, wait %q; want accept, a direction and drop, and no retry", qs, got.WaitsWhat)
	}
}

// Your direction goes to the writer with its next fix turn, in your words,
// with RetryFixTurns fix turns; once they made no progress, the task asks
// you again. Accepting the failing check as known passes the check for the
// task, and the PR body names it.
func TestYourAnswerToTheFixQuestionGivesADirectionOrAcceptsTheCheck(t *testing.T) {
	t.Run("a direction", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.toPromptCap("PR-12573")
		got := r.reload(task.ID)
		r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev,
			Answers: map[int]string{0: optionDirection + ": number the rule as Style/NumericLiterals asks"}})
		r.passes(task, func() bool { return r.writerPrompts() > capPrompts })
		ps := r.rt.promptsTo("fx-writer")
		if len(ps) != capPrompts+1 || !strings.HasPrefix(ps[capPrompts].Text, fixLead) ||
			!strings.Contains(ps[capPrompts].Text, directionLead) || !strings.Contains(ps[capPrompts].Text, "number the rule") {
			t.Fatalf("the writer's prompts after your direction %+v; want a fix turn with your words", ps[capPrompts:])
		}
		r.endTurns(task, 2*RetryFixTurns)
		if n := r.writerPrompts(); n != capPrompts+RetryFixTurns || r.reload(task.ID).State != registry.TaskNeedsYou {
			t.Fatalf("the writer got %d prompts, want %d, then the question again", n, capPrompts+RetryFixTurns)
		}
		if strings.Contains(r.rt.promptsTo("fx-writer")[capPrompts+1].Text, directionLead) {
			t.Fatal("your direction went twice")
		}
	})
	t.Run("accept the check", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.toPromptCap("PR-12574")
		got := r.reload(task.ID)
		r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: got.QuestionRev, Answers: map[int]string{0: optionAccept}})
		r.passes(task, func() bool { return r.reload(task.ID).Stage != "coding" })
		if got := r.reload(task.ID); got.Stage == "coding" || r.writerPrompts() != capPrompts {
			t.Fatalf("task at %s waits %q with %d writer prompts, want coding passed with no fix turn", got.Stage, got.WaitsWhat,
				r.writerPrompts())
		}
		if s := stopsSentences(r.layout.FactoryTaskDir(task.ID)); !strings.Contains(s, "accepted the failure") ||
			!strings.Contains(s, "precheck.lint") {
			t.Fatalf("the PR body's sentences %q, want the accepted check named", s)
		}
	})
}

// A check fault (a jest run that printed nothing at all) is no turn of the
// writer's: no turn records it, it spends no fix turn and gives no go-on or
// fresh session; it asks you, and the writer's turns before it stay its
// last.
func TestACheckFaultNeverCountsAsAWritersTurnWithoutProgress(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12575", "bug")
	r.setLint(lintOffense)
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, 2) // two turns with the offense
	r.setLint("")
	r.mu.Lock()
	r.exits = map[string][]int{"bin/jest": {1, 1}}
	r.mu.Unlock()
	r.commit("PR-12575 the offense fixed", map[string]string{"README": "shop, with credits\n"})
	r.endTurns(task, 1)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "precheck.jest") {
		t.Fatalf("task %s waits %q, want jest's question", got.State, got.WaitsWhat)
	}
	rec, err := readFixTurns(r.layout.FactoryTaskDir(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Turns) != 2 || len(rec.Grants) != 0 {
		t.Fatalf("turns %+v and grants %+v, want the two offenses alone and no grant", rec.Turns, rec.Grants)
	}
	if n := r.writerPrompts(); n != 3 {
		t.Fatalf("the writer got %d prompts, want its first and its two fix turns, none for the fault", n)
	}
}

// The grants live in fix-turns.json: a restart right after the fresh
// session's prompt neither starts a second fresh session nor gives a turn
// more, and the task asks you after the same prompts as with no restart.
func TestARestartAtTheFixCapNeitherDoublesNorLosesTheFreshSession(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12576", "bug")
	r.setLint(lintOffense)
	r.startStage(task)
	r.fix(task)
	r.endTurns(task, 2*runtime.MaxStagePrompts)
	for len(r.freshPrompts()) == 0 && isLive(r.reload(task.ID)) {
		r.endTurns(task, 1)
	}
	r.restart()
	r.endTurns(task, 2*fixGoOnTurns)
	if n := r.writerPrompts(); n != capPrompts || len(r.freshPrompts()) != 1 {
		t.Fatalf("the writer got %d prompts, %d fresh, after a restart; want %d and one", n, len(r.freshPrompts()), capPrompts)
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.HasPrefix(got.WaitsWhat, capWait) {
		t.Fatalf("task %s waits %q, want the question", got.State, got.WaitsWhat)
	}
}

// Your retry at the fix question still gives RetryFixTurns fix turns, as
// before it, and spends none of the factory's grants: they stay as they
// were.
func TestYourRetryAtTheFixQuestionSpendsNoGrant(t *testing.T) {
	r := newCodingRig(t)
	task := r.toPromptCap("PR-12577")
	before := r.grantsOf(task.ID)
	r.retryFixTurns(task, capPrompts)
	if after := r.grantsOf(task.ID); after[grantGoOn] != before[grantGoOn] || after[grantFresh] != before[grantFresh] {
		t.Fatalf("grants %v after your retry, want %v", after, before)
	}
}
