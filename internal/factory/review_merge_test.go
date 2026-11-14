package factory

import (
	"context"
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
)

// toCleanBase takes a bug task through its coding stage and its local
// review to its ready card while origin/master moves in a file the change
// does not touch (the card's fetch brings it to the copy, and nothing
// conflicts), then sends the card back with a note: the writer's turn is
// next. head is the head the local review passed.
func (r *readyRig) toCleanBase(key string) (registry.Task, string) {
	r.t.Helper()
	task := r.codingTask(key, "bug")
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb", "spec/models/"))
	r.taskFile(task.ID, acceptanceFile, acceptanceJSON)
	r.startStage(task)
	head := r.fix(task)
	r.ended(task, "fx-writer")
	r.run(16, func() bool { return r.reload(task.ID).Stage == "pr_body" && len(r.rt.promptsTo("fx-judge")) > 0 })
	r.moveMaster("README", "shop, now with a base change\n")
	r.taskFile(task.ID, prBodyFile, prBodyDraft)
	r.ended(task, "fx-judge")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "ready" || got.State != registry.TaskNeedsYou {
		r.t.Fatalf("task %s at %s waits %q, want its ready card", got.State, got.Stage, got.WaitsWhat)
	}
	if q := r.decideOn(task.ID, r.card(task.ID), ChoiceSendBack, "base", "merge origin/master into the branch"); q.State != registry.RequestDone {
		r.t.Fatalf("send back request %s: %q", q.State, q.Result)
	}
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	return r.reload(task.ID), head
}

// writerMerges is the writer's merge turn in the copy: `git merge
// origin/master` without its commit, then what alter does to the merge's
// files (nil: nothing), the merge committed, and the turn's end. The merge
// is the new head.
func (r *readyRig) writerMerges(task registry.Task, alter func()) string {
	r.t.Helper()
	r.clock.Add(time.Minute)
	r.gitOut(r.copyDir, "merge", "--quiet", "--no-commit", "--no-ff", "origin/master") // a conflict leaves it for alter
	if alter != nil {
		alter()
	}
	r.git(r.copyDir, "add", "-A")
	r.git(r.copyDir, "commit", "--quiet", "--no-edit")
	merged := r.git(r.copyDir, "rev-parse", "HEAD")
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	return merged
}

// judgeOf is the watch's judge of the rig's repository.
func (r *readyRig) judgeOf() string {
	r.t.Helper()
	return r.cfg.JudgeFor(r.cfg.WatchFor("example/shop")).Name
}

// summaryOf is the task's review-summary.json.
func (r *readyRig) summaryOf(id int64) ReviewSummary {
	r.t.Helper()
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(id, reviewSummaryFile)), &sum); err != nil {
		r.t.Fatal(err)
	}
	return sum
}

// The operator's decision of 2026-11-13 (task 9, PR-27708: the merge of
// the base after a conflict at the card got a full round of three
// reviewers, about 1.5 hours, for a change that was the merge alone): a
// head that differs from the head the local review passed only by a merge
// of origin/master that git's own merge makes alike gets no review round.
// The factory's proof and precheck still run at the merge, the stage
// passes with the review's end carried to the merge, and the ready card
// says "base merged cleanly: no review round".
func TestACleanBaseMergeGetsNoReviewRound(t *testing.T) {
	r := newReadyRig(t)
	task, reviewed := r.toCleanBase("PR-12401")
	merged := r.writerMerges(task, nil)
	r.toBody(task)

	if calls := r.rv.calls(); len(calls) != 1 || calls[0].HeadSHA != reviewed {
		t.Fatalf("local rounds %+v, want the one at %s and none at the merge", calls, short(reviewed))
	}
	if cs := r.cycles(task.ID); len(cs) != 1 {
		t.Fatalf("cycles %+v, want the first round's alone: the merge counts no round", cs)
	}
	var proof Proof
	if err := json.Unmarshal([]byte(r.readTask(task.ID, factoryProofFile)), &proof); err != nil || !proof.Holds(merged) {
		t.Fatalf("the factory's proof %+v (%v), want it at the merge %s", proof, err, merged)
	}
	if !precheckPassedAt(r.layout.FactoryTaskDir(task.ID), merged) {
		t.Fatal("the precheck did not pass at the merge")
	}
	sum := r.summaryOf(task.ID)
	if sum.Head != merged || sum.Exit != registry.ExitClean || sum.Rounds != 1 || sum.BaseMerge == nil || sum.BaseMerge.Kind != mergeClean ||
		sum.BaseMerge.Reviewed != reviewed || sum.BaseMerge.Head != merged {
		t.Fatalf("review-summary.json %+v (base merge %+v), want the clean review carried to the merge", sum, sum.BaseMerge)
	}
	c := r.card(task.ID)
	got := r.reload(task.ID)
	if c.Revision != 2 || c.Head != merged || got.State != registry.TaskNeedsYou || got.Stage != "ready" {
		t.Fatalf("card %+v; task %s at %s, want card 2 at the merge", c, got.State, got.Stage)
	}
	if _, why := ReadyRecommendation(r.ctx, r.reg, got, c); !strings.Contains(why, "base merged cleanly: no review round") {
		t.Fatalf("the card's recommendation %q, want it to name the clean merge", why)
	}
	if !slices.ContainsFunc(r.events(task.ID, "review"), func(ev registry.Event) bool {
		return strings.Contains(ev.Text, "no review round") && strings.Contains(ev.Text, short(merged))
	}) {
		t.Fatalf("review events %+v, want one that says the merge gets no review round", r.events(task.ID, "review"))
	}
}

// A merge whose conflicts the writer resolved gets one narrow round: the
// judge alone, on `git show --remerge-diff <merge>` (LocalCase.Remerge),
// never the watch's reviewers. Its clean end passes the stage with the
// first review's leftovers kept; the second opinion, which follows a
// clean round of the whole change, does not follow it; and the ready card
// names the narrow round and the files.
func TestABaseMergeWithConflictsGetsANarrowRoundOnItsResolutions(t *testing.T) {
	r := newReadyRig(t, result(nitFinding), result())
	task, reviewed := r.toConflict("PR-12402", nil)
	if got := r.reload(task.ID); got.Stage != "coding" {
		t.Fatalf("task %s at %s, want the merge turn", got.State, got.Stage)
	}
	r.cfg.Factory.LocalReview.SecondJudge = config.FactorySecondJudgeAlways // a clean round of a whole change would get one now
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	merged := r.writerMerges(task, func() {
		r.write(r.copyDir, "app/credit.rb", strings.Replace(creditRB, "    0\n", "    10\n", 1)+"# master's 5 come with the campaign\n")
	})
	r.toBody(task)

	calls := r.rv.calls()
	if len(calls) != 2 || calls[0].HeadSHA != reviewed {
		t.Fatalf("local rounds %+v, want the first round and one at the merge", calls)
	}
	if n := calls[1]; n.HeadSHA != merged || !n.Remerge || !slices.Equal(n.Roles, []string{r.judgeOf()}) || n.Round != 1 || n.BaseSHA == "" {
		t.Fatalf("the round at the merge %+v, want the judge %s alone on its remerge diff", n, r.judgeOf())
	}
	if calls[0].Remerge || len(calls[0].Roles) < 2 {
		t.Fatalf("the first round %+v, want the watch's roles on the whole change", calls[0])
	}
	sum := r.summaryOf(task.ID)
	if sum.Head != merged || sum.Exit != registry.ExitClean || sum.BaseMerge == nil || sum.BaseMerge.Kind != mergeNarrow ||
		!slices.Equal(sum.BaseMerge.Conflicts, []string{"app/credit.rb"}) || !slices.Equal(sum.BaseMerge.Files, []string{"app/credit.rb"}) {
		t.Fatalf("review-summary.json %+v (base merge %+v), want a clean end at the merge after the narrow round", sum, sum.BaseMerge)
	}
	if !slices.ContainsFunc(sum.Leftovers, func(i ReviewItem) bool { return i.Title == nitFinding.Title }) {
		t.Fatalf("leftovers %+v, want the first review's P3 kept", sum.Leftovers)
	}
	if !slices.ContainsFunc(r.events(task.ID, "review"), func(ev registry.Event) bool {
		return strings.HasPrefix(ev.Text, "round 1 (cycle 2) at "+short(merged)) && strings.Contains(ev.Text, "narrow")
	}) {
		t.Fatalf("review events %+v, want the narrow round's begin", r.events(task.ID, "review"))
	}
	c := r.card(task.ID)
	got := r.reload(task.ID)
	if c.Head != merged || got.State != registry.TaskNeedsYou || got.Stage != "ready" {
		t.Fatalf("card %+v; task %s at %s, want the card at the merge", c, got.State, got.Stage)
	}
	if _, why := ReadyRecommendation(r.ctx, r.reg, got, c); !strings.Contains(why, "narrow round") || !strings.Contains(why, "app/credit.rb") ||
		!strings.Contains(why, "conflicts") {
		t.Fatalf("the card's recommendation %q, want it to name the narrow round on the conflicts in app/credit.rb", why)
	}
}

// The narrow round's judge reads the remerge diff the factory made in a
// repository of its own, never one made in the copy: there the main
// clone's info/attributes, which every copy shares, can name a merge
// driver or `merge=union`, under which git's own merge of the parents
// conflicts nowhere and the remerge diff shows another change than the one
// the check classified. The round's case carries the Isolated remerge diff
// (LocalCase.RemergeDiff, which RunLocal writes as the round's
// remerge.diff), the conflict and its resolution in it.
func TestANarrowRoundReadsTheRemergeDiffTheFactoryMadeApartFromTheCopy(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toConflict("PR-12409", nil)
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	merged := r.writerMerges(task, func() {
		r.write(r.copyDir, "app/credit.rb", strings.Replace(creditRB, "    0\n", "    10\n", 1)+"# master's 5 come with the campaign\n")
	})
	r.write(r.main, ".git/info/attributes", "app/credit.rb merge=union\n")
	r.toBody(task)

	calls := r.rv.calls()
	if len(calls) != 2 || !calls[1].Remerge || calls[1].HeadSHA != merged {
		t.Fatalf("local rounds %+v, want the narrow round at the merge", calls)
	}
	var isolated string
	git := r.e.d.Git
	if err := git.Isolated(r.ctx, r.copyDir, func(iso string) error {
		var err error
		isolated, err = git.RemergeDiff(r.ctx, iso, merged, "app/credit.rb")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	inCopy := r.git(r.copyDir, "show", "--remerge-diff", "--format=", merged)
	if strings.Contains(inCopy, "CONFLICT") || !strings.Contains(isolated, "CONFLICT") {
		t.Fatalf("the rig's union attribute does not change the remerge diff: in the copy %q, apart from it %q", inCopy, isolated)
	}
	if got := calls[1].RemergeDiff; got != isolated || !strings.Contains(got, "+# master's 5 come with the campaign") {
		t.Fatalf("the narrow round's remerge diff %q, want the one made apart from the copy %q", got, isolated)
	}
	if calls[0].RemergeDiff != "" {
		t.Fatalf("the first round of the whole change carries a remerge diff %q", calls[0].RemergeDiff)
	}
}

// An evil merge (git's own merge is clean, and the merge commit changes
// more than it would) gets the narrow round on what git would not have
// merged: the files of its remerge diff, and the card says the merge made
// changes git's own merge would not.
func TestAnEvilMergeIsReviewedNarrowlyOnWhatGitWouldNotHaveMerged(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCleanBase("PR-12403")
	merged := r.writerMerges(task, func() { r.write(r.copyDir, "app/extra.rb", "class Extra\nend\n") })
	r.toBody(task)

	calls := r.rv.calls()
	if len(calls) != 2 || calls[1].HeadSHA != merged || !calls[1].Remerge || !slices.Equal(calls[1].Roles, []string{r.judgeOf()}) {
		t.Fatalf("local rounds %+v, want a narrow round of the judge at the merge", calls)
	}
	sum := r.summaryOf(task.ID)
	if sum.BaseMerge == nil || sum.BaseMerge.Kind != mergeNarrow || len(sum.BaseMerge.Conflicts) != 0 ||
		!slices.Equal(sum.BaseMerge.Files, []string{"app/extra.rb"}) {
		t.Fatalf("the base merge %+v, want a narrow check of app/extra.rb with no conflict", sum.BaseMerge)
	}
	if _, why := ReadyRecommendation(r.ctx, r.reg, r.reload(task.ID), r.card(task.ID)); !strings.Contains(why, "git's own merge would not make") ||
		!strings.Contains(why, "app/extra.rb") {
		t.Fatalf("the card's recommendation %q, want it to name the change git would not have merged", why)
	}
}

// Your finish-review at a head no round reviewed says what a clean merge of
// the base gets there: no round, and the review's end before the merge
// holds (the stage passes at once, the card saying the base merged
// cleanly, not that you ended the review). What the plan promised, the
// stage then does: no round at the merge.
func TestFinishReviewAtACleanBaseMergeSaysNoRoundRuns(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCleanBase("PR-12410")
	merged := r.writerMerges(task, nil)
	at := r.reload(task.ID)
	at.HeadSHA = merged // the head the coding stage's checks record next, before the review stage's first pass
	plan, err := FinishReviewPlan(r.ctx, r.reg, r.layout, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no round reviewed its head yet: one round runs", "only merges the base cleanly",
		"gets no round", "the review's end before the merge holds"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan %q lacks %q", plan, want)
		}
	}
	r.toBody(task)
	if calls := r.rv.calls(); len(calls) != 1 {
		t.Fatalf("local rounds %+v, want none at the clean merge", calls)
	}
}

// A merge with a commit beside it is no merge alone: the head gets a full
// round of the watch's roles on the whole change, as before, and an event
// says why.
func TestCommitsBesidesTheMergeGetAFullRound(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCleanBase("PR-12404")
	r.clock.Add(time.Minute)
	r.git(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	head := r.commit("PR-12404 one thing more", map[string]string{"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10\n", 1) + "# one thing more\n"})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.toBody(task)

	calls := r.rv.calls()
	if len(calls) != 2 || calls[1].HeadSHA != head || calls[1].Remerge || !slices.Equal(calls[1].Roles, calls[0].Roles) {
		t.Fatalf("local rounds %+v, want a full round of the first round's roles at %s", calls, short(head))
	}
	if sum := r.summaryOf(task.ID); sum.BaseMerge != nil {
		t.Fatalf("review-summary.json names a base merge %+v after a full round", sum.BaseMerge)
	}
	if !slices.ContainsFunc(r.events(task.ID, "review"), func(ev registry.Event) bool {
		return strings.Contains(ev.Text, "a full round") && strings.Contains(ev.Text, "besides the merge")
	}) {
		t.Fatalf("review events %+v, want one that says why the head gets a full round", r.events(task.ID, "review"))
	}
}

// The check reads git, never the writer's words, and takes only a merge of
// two parents whose first is the head the local review passed and whose
// second is a commit of origin/master, with a remerge diff git can produce
// that resolves every conflict git's merge leaves: anything else is a
// full round, with why.
func TestOnlyAMergeOfTheBaseIntoTheReviewedHeadSkipsTheFullRound(t *testing.T) {
	r := newReadyRig(t)
	task := r.codingTask("PR-12405", "bug")
	reviewed := r.fix(task)
	r.moveMaster("README", "shop, now with a base change\n")
	r.git(r.main, "fetch", "--quiet", "origin")
	check := func(head string) BaseMerge {
		t.Helper()
		task.HeadSHA = head
		bm, _, err := r.e.checkBaseMerge(r.ctx, task, r.cp, reviewed)
		if err != nil {
			t.Fatal(err)
		}
		return bm
	}
	at := func(rev string) { r.git(r.copyDir, "switch", "--quiet", "--force-create", task.Branch, rev) }

	r.git(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	clean := r.git(r.copyDir, "rev-parse", "HEAD")
	if bm := check(clean); bm.Kind != mergeClean || bm.BaseSHA != r.git(r.copyDir, "rev-parse", "origin/master") {
		t.Fatalf("a clean merge of the base: %+v", bm)
	}

	// a side branch off the base's first commit, merged in place of the base
	at(reviewed)
	r.git(r.copyDir, "switch", "--quiet", "-c", "side", "origin/master~1")
	r.commit("a side change", map[string]string{"lib/side.rb": "side\n"})
	at(reviewed)
	r.git(r.copyDir, "merge", "--quiet", "--no-edit", "side")
	if bm := check(r.git(r.copyDir, "rev-parse", "HEAD")); bm.Kind != mergeFull || !strings.Contains(bm.Why, "origin/master") {
		t.Fatalf("a merge of a side branch: %+v, want a full round naming the base", bm)
	}

	// the base merged into a commit after the reviewed head
	at(reviewed)
	r.commit("after the review", map[string]string{"app/credit.rb": creditRB + "# after\n"})
	r.git(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	if bm := check(r.git(r.copyDir, "rev-parse", "HEAD")); bm.Kind != mergeFull || !strings.Contains(bm.Why, short(reviewed)) {
		t.Fatalf("a merge into another head than the reviewed one: %+v, want a full round naming it", bm)
	}

	// a remerge diff git cannot produce
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "--remerge-diff") {
			return errors.New("fatal: no remerge diff")
		}
		return nil
	})
	if bm := check(clean); bm.Kind != mergeFull || !strings.Contains(bm.Why, "remerge") {
		t.Fatalf("a remerge diff git cannot produce: %+v, want a full round", bm)
	}
	r.setFailGit(nil)

	// conflict markers committed: the merge's markers are not git's own
	// (its labels differ), so the narrow round's diff shows them
	at(reviewed)
	r.moveMaster("app/credit.rb", strings.Replace(creditRB, "    0\n", "    5\n", 1))
	r.git(r.main, "fetch", "--quiet", "origin")
	r.gitOut(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	r.git(r.copyDir, "add", "-A")
	r.git(r.copyDir, "commit", "--quiet", "--no-edit")
	markers := r.git(r.copyDir, "rev-parse", "HEAD")
	if bm := check(markers); bm.Kind != mergeNarrow || !slices.Equal(bm.Conflicts, []string{"app/credit.rb"}) ||
		!slices.Equal(bm.Files, []string{"app/credit.rb"}) {
		t.Fatalf("conflict markers committed: %+v, want a narrow round on app/credit.rb", bm)
	}

	// a remerge diff git names the files of but cannot produce whole (past
	// execx.MaxOutput, say), which the narrow round would read
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "--remerge-diff") && slices.Contains(args, "--text") {
			return errors.New("fatal: no remerge diff")
		}
		return nil
	})
	if bm := check(markers); bm.Kind != mergeFull || !strings.Contains(bm.Why, "remerge diff") {
		t.Fatalf("a remerge diff git cannot produce whole: %+v, want a full round", bm)
	}
	r.setFailGit(nil)

	// an ordinary head: no merge, no record of one
	at(reviewed)
	head := r.commit("an ordinary change", map[string]string{"app/credit.rb": creditRB + "# ordinary\n"})
	task.HeadSHA = head
	if bm, ok, err := r.e.checkBaseMerge(r.ctx, task, r.cp, reviewed); err != nil || ok {
		t.Fatalf("an ordinary head: %+v, %v, %v; want no base merge", bm, ok, err)
	}

	// what the check makes of a merge of the base into the reviewed head
	for _, tc := range []struct {
		name             string
		same             bool
		conflicts, files []string
		kind, why        string
	}{
		{"git's own merge", true, nil, nil, mergeClean, ""},
		{"conflicts resolved", false, []string{"a.rb"}, []string{"a.rb", "b.rb"}, mergeNarrow, ""},
		{"an evil merge", false, nil, []string{"b.rb"}, mergeNarrow, ""},
		{"a conflict left as git's merge wrote it", false, []string{"a.rb", "db/schema.rb"}, []string{"a.rb"}, mergeFull, "db/schema.rb"},
		{"another tree with no file to show", false, nil, nil, mergeFull, "names no file"},
	} {
		if kind, why := mergeKind(tc.same, tc.conflicts, tc.files); kind != tc.kind || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
			t.Errorf("%s: %s (%q), want %s naming %q", tc.name, kind, why, tc.kind, tc.why)
		}
	}
}

// The base's tip is the remote's, never the copy's origin/master alone:
// copies are worktrees of the main clone, so the writer's pane can move
// that ref. The writer commits its own change off origin/master, names it
// origin/master (update-ref) and merges it into the reviewed head: the
// check fetches the base first, finds the merged commit on no commit of
// the real origin/master, and the head gets a full round of the watch's
// roles, with an event naming origin/master. A fetch that fails is a full
// round too.
func TestABaseMergeOfACommitOnlyTheCopysRefNamesGetsAFullRound(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCleanBase("PR-12408")
	r.clock.Add(time.Minute)
	r.git(r.copyDir, "switch", "--quiet", "-c", "own", "origin/master")
	own := r.commit("the writer's own change", map[string]string{"lib/own.rb": "class Own\nend\n"})
	r.git(r.copyDir, "update-ref", "refs/remotes/origin/master", own)
	r.git(r.copyDir, "switch", "--quiet", task.Branch)
	r.git(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	merged := r.git(r.copyDir, "rev-parse", "HEAD")
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.toBody(task)

	calls := r.rv.calls() // the full round's clean end on the wider change gets its second opinion after it
	if len(calls) < 2 || calls[1].HeadSHA != merged || calls[1].Remerge || calls[1].Round != 1 || !slices.Equal(calls[1].Roles, calls[0].Roles) {
		for _, c := range calls {
			t.Logf("round %d at %s: %v (remerge %v)", c.Round, short(c.HeadSHA), c.Roles, c.Remerge)
		}
		t.Fatalf("local rounds above, want a full round of the first round's roles at %s", short(merged))
	}
	if !slices.ContainsFunc(r.events(task.ID, "review"), func(ev registry.Event) bool {
		return strings.Contains(ev.Text, "a full round") && strings.Contains(ev.Text, "is no commit of origin/master")
	}) {
		t.Fatalf("review events %+v, want one that says the merged commit is no commit of origin/master", r.events(task.ID, "review"))
	}

	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "fetch") {
			return errors.New("fatal: unable to access the remote")
		}
		return nil
	})
	task.HeadSHA = merged
	bm, ok, err := r.e.checkBaseMerge(r.ctx, task, r.cp, calls[0].HeadSHA)
	r.setFailGit(nil)
	if err != nil || !ok || bm.Kind != mergeFull || !strings.Contains(bm.Why, "fetch origin/master") {
		t.Fatalf("a fetch that fails: %+v, %v, %v; want a full round naming the fetch", bm, ok, err)
	}
}

// fullRoundAt fails the test unless the local rounds after the first are a
// full round at head: the first round's roles on the whole change, not
// the narrow round of a base merge (the full round's clean end on the
// wider change may get its second opinion after it).
func (r *readyRig) fullRoundAt(head string) {
	r.t.Helper()
	calls := r.rv.calls()
	if len(calls) < 2 || calls[1].HeadSHA != head || calls[1].Remerge || calls[1].Round != 1 || !slices.Equal(calls[1].Roles, calls[0].Roles) {
		for _, c := range calls {
			r.t.Logf("round %d at %s: %v (remerge %v)", c.Round, short(c.HeadSHA), c.Roles, c.Remerge)
		}
		r.t.Fatalf("local rounds above, want a full round of the first round's roles at %s", short(head))
	}
}

// fullRoundEvent fails the test unless a review event of task id says the
// head gets a full round, with each of words.
func (r *readyRig) fullRoundEvent(id int64, words ...string) {
	r.t.Helper()
	evs := r.events(id, "review")
	if !slices.ContainsFunc(evs, func(ev registry.Event) bool {
		return strings.Contains(ev.Text, "a full round") && !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(ev.Text, w) })
	}) {
		r.t.Fatalf("review events %+v, want one that says the head gets a full round, naming %q", evs, words)
	}
}

// The base is the one GitHub reports, read through the factory's GitHub as
// you, never through the clone's remote config alone: the forced fetch goes
// where the main clone's remote.origin.url says, which the writer's pane
// can rewrite (or redirect with a url.<x>.insteadOf). The writer points it
// at a bare repository of its own whose master is its own commit, fetches,
// and merges that commit into the reviewed head. The fetch sets
// origin/master to that commit again, but it is no commit of master on
// GitHub, so the head gets a full round of the watch's roles, with an
// event that says so.
func TestABaseMergeOfARemoteTheWriterRewroteGetsAFullRound(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCleanBase("PR-12411")
	r.clock.Add(time.Minute)
	own := filepath.Join(r.root, "own.git")
	r.git(r.root, "clone", "--quiet", "--bare", r.origin, own)
	r.git(r.copyDir, "switch", "--quiet", "-c", "own", "origin/master")
	mine := r.commit("the writer's own change", map[string]string{"lib/own.rb": "class Own\nend\n"})
	r.git(r.copyDir, "push", "--quiet", own, "own:master")
	r.git(r.main, "config", "remote.origin.url", own)
	r.git(r.copyDir, "fetch", "--quiet", "origin")
	r.git(r.copyDir, "switch", "--quiet", task.Branch)
	r.git(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	merged := r.git(r.copyDir, "rev-parse", "HEAD")
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.toBody(task)

	r.fullRoundAt(merged)
	r.fullRoundEvent(task.ID, "is no commit of master on GitHub", short(mine))
	if got := r.git(r.copyDir, "rev-parse", "origin/master"); got != mine {
		t.Fatalf("origin/master is at %s after the check's fetch, want the writer's %s: the rig's rewritten remote is not what the fetch read",
			short(got), short(mine))
	}
}

// A read of the base on GitHub that fails is a full round, as a fetch that
// fails is: without it the check cannot tell a commit of the base from one
// of the writer's own. That round starts once GitHub reports the base its
// diff starts from (roundBase), and waits saying so until then. So is a
// base GitHub reports at a commit the copy has no object of (git cannot
// tell whether the merged commit is one of it), and a factory daemon with
// no GitHub to read the base on. The same merge with GitHub's base read is
// clean.
func TestAFailedReadOfGitHubsBaseGetsAFullRound(t *testing.T) {
	r := newReadyRig(t)
	task, reviewed := r.toCleanBase("PR-12412")
	r.gh.setBase("", errors.New("gh: HTTP 502: Bad Gateway"), nil)
	merged := r.writerMerges(task, nil)
	r.run(16, func() bool { return strings.Contains(r.reload(task.ID).WaitsWhat, "cannot start") })
	if got := r.reload(task.ID); len(r.rv.calls()) != 1 || !strings.Contains(got.WaitsWhat, "master on GitHub") {
		t.Fatalf("%d rounds; task waits %q: want the full round waiting on GitHub's base", len(r.rv.calls()), got.WaitsWhat)
	}
	r.gh.setBase("", nil, nil)
	r.clock.Add(time.Minute) // past the failed pass's poll
	r.toBody(task)

	r.fullRoundAt(merged)
	r.fullRoundEvent(task.ID, "master on GitHub", "HTTP 502")

	task.HeadSHA = merged
	check := func(what, kind, why string) {
		t.Helper()
		bm, ok, err := r.e.checkBaseMerge(r.ctx, task, r.cp, reviewed)
		if err != nil || !ok || bm.Kind != kind || !strings.Contains(bm.Why, why) {
			t.Fatalf("%s: %+v, %v, %v; want %s naming %q", what, bm, ok, err, kind, why)
		}
	}
	r.gh.setBase(strings.Repeat("ab", 20), nil, nil)
	check("a base at a commit the copy has no object of", mergeFull, "master on GitHub at abababababab")
	r.gh.setBase("", nil, nil)
	check("GitHub's base read", mergeClean, "")
	r.e.d.GitHub = nil
	check("no GitHub", mergeFull, "GitHub")
	r.e.d.GitHub = r.gh
}

// A merge of the base GitHub reports, or of an ancestor of it, stays a base
// merge: a clean one gets no round, as before. The base moves on GitHub as
// the check reads it, so the merged commit is an ancestor of the base GitHub
// reports, and the check reads GitHub before it fetches, which brings that
// newer commit to the copy.
func TestABaseMergeOfGitHubsBaseOrItsAncestorStaysClean(t *testing.T) {
	r := newReadyRig(t)
	task, reviewed := r.toCleanBase("PR-12413")
	merged := r.writerMerges(task, nil)
	r.toBody(task)

	if calls := r.rv.calls(); len(calls) != 1 {
		t.Fatalf("local rounds %+v, want none at the clean merge", calls)
	}
	onGitHub := r.git(r.origin, "rev-parse", "refs/heads/master")
	if sum := r.summaryOf(task.ID); sum.Head != merged || sum.BaseMerge == nil || sum.BaseMerge.Kind != mergeClean || sum.BaseMerge.BaseSHA != onGitHub {
		t.Fatalf("review-summary.json %+v (base merge %+v), want the clean merge of %s, GitHub's base", sum, sum.BaseMerge, short(onGitHub))
	}
	if r.gh.baseReads() == 0 {
		t.Fatal("the check never read the base on GitHub")
	}

	var moved string
	r.gh.setBase("", nil, func() {
		if moved == "" {
			moved = r.moveMaster("README", "shop, moved again on GitHub\n")
		}
	})
	task.HeadSHA = merged
	bm, ok, err := r.e.checkBaseMerge(r.ctx, task, r.cp, reviewed)
	if err != nil || !ok || bm.Kind != mergeClean || bm.BaseSHA != onGitHub || moved == "" {
		t.Fatalf("a merge of an ancestor of GitHub's base %s: %+v, %v, %v; want a clean merge of %s", short(moved), bm, ok, err, short(onGitHub))
	}
}

// The narrow round is a round of the task's life: it counts toward
// [factory.local_review] max_rounds, and at the limit its end decides as
// any round's does there. A P2 it finds at the task's second round, the
// limit, passes the stage on a safe stop with the finding left under KNOWN
// and no round after it; past the limit (max_rounds 1) the merge still gets
// its one narrow round, as a new head gets one.
func TestANarrowRoundCountsTowardMaxRounds(t *testing.T) {
	resolution := evalrun.LocalFinding{Severity: "P2", Title: "The merge drops master's 5 points", Path: "app/credit.rb", Line: 3}
	for _, tc := range []struct {
		name  string
		limit int
		words string
	}{{"at the limit", 2, "at the round limit (2 rounds)"}, {"past the limit", 1, "at the round limit (1 round; it ran 2)"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReadyRig(t, result(), result(resolution))
			r.cfg.Factory.LocalReview.MaxRounds = tc.limit
			task, _ := r.toConflict("PR-12406", nil)
			r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
			merged := r.writerMerges(task, func() { r.write(r.copyDir, "app/credit.rb", strings.Replace(creditRB, "    0\n", "    10\n", 1)) })
			r.toBody(task)

			calls := r.rv.calls()
			if len(calls) != 2 || !calls[1].Remerge || calls[1].HeadSHA != merged {
				t.Fatalf("local rounds %+v, want the first round and the narrow one at the merge, and none after", calls)
			}
			cs := r.cycles(task.ID)
			if len(cs) != 2 || cs[1].N != 2 || cs[1].HeadBefore != merged || cs[1].Exit != registry.ExitCap {
				t.Fatalf("cycles %+v, want the narrow round as the task's round 2, ended at the limit", cs)
			}
			sum := r.summaryOf(task.ID)
			if sum.Exit != exitRoundLimit || sum.TaskRounds != 2 || sum.Limit != tc.limit || sum.Head != merged ||
				!slices.ContainsFunc(sum.Leftovers, func(i ReviewItem) bool { return i.Title == resolution.Title }) {
				t.Fatalf("review-summary.json %+v, want the limit's end at the merge with the P2 under KNOWN", sum)
			}
			c := r.card(task.ID)
			if _, why := ReadyRecommendation(r.ctx, r.reg, r.reload(task.ID), c); !strings.Contains(why, tc.words) || !strings.Contains(why, "narrow round") {
				t.Fatalf("the card's recommendation %q, want %q and the narrow round", why, tc.words)
			}
		})
	}
}

// The ready card's recommendation names the base merge's check beside the
// review's end: a clean merge that got no round, a narrow round on a
// merge's conflicts, one on changes git's own merge would not make, each
// with its files; a card of a review with no base merge says nothing of
// one.
func TestTheReadyCardNamesTheBaseMergeCheck(t *testing.T) {
	head := strings.Repeat("ab", 20)
	card := func(bm *BaseMerge) ReadyCardFile {
		return ReadyCardFile{Head: head, Review: ReviewSummary{Exit: registry.ExitClean, Rounds: 2, Head: head, BaseMerge: bm}}
	}
	for _, tc := range []struct {
		name      string
		bm        *BaseMerge
		want, not []string
	}{
		{"no merge", nil, []string{"the local review ended clean after 2 rounds"}, []string{"base merge", "narrow"}},
		{"clean", &BaseMerge{Head: head, Kind: mergeClean}, []string{"base merged cleanly: no review round", "ended clean after 2 rounds"}, []string{"narrow"}},
		{"conflicts", &BaseMerge{Head: head, Kind: mergeNarrow, Files: []string{"app/models/concerns/featurable.rb", "db/schema.rb"},
			Conflicts: []string{"app/models/concerns/featurable.rb", "db/schema.rb"}},
			[]string{"base merged with conflicts: a narrow round reviewed its resolutions in app/models/concerns/featurable.rb, db/schema.rb"}, nil},
		{"evil", &BaseMerge{Head: head, Kind: mergeNarrow, Files: []string{"app/extra.rb"}},
			[]string{"base merged with changes git's own merge would not make: a narrow round reviewed them in app/extra.rb"}, []string{"conflicts"}},
		{"many files", &BaseMerge{Head: head, Kind: mergeNarrow, Files: []string{"a.rb", "b.rb", "c.rb", "d.rb", "e.rb"}, Conflicts: []string{"a.rb"}},
			[]string{"in a.rb, b.rb, c.rb and 2 more"}, []string{"d.rb"}},
		{"a full round", &BaseMerge{Head: head, Kind: mergeFull, Why: "commits besides the merge"}, []string{"ended clean after 2 rounds"},
			[]string{"base merge", "narrow"}},
	} {
		choice, why := readyRecommendation(card(tc.bm), nil)
		if choice != ChoicePush {
			t.Errorf("%s: recommends %s, want push", tc.name, choice)
		}
		for _, w := range tc.want {
			if !strings.Contains(why, w) {
				t.Errorf("%s: %q lacks %q", tc.name, why, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(why, n) {
				t.Errorf("%s: %q names %q", tc.name, why, n)
			}
		}
	}
}

// A clean merge's pass is a pass of the review at the merge for everything
// that reads one: a pass of the review stage after a restart passes again
// with no round, and QA takes the merge as reviewed (qaFixReviewed), so it
// never sends the task back to a review that passes at once.
func TestACleanBaseMergesPassHoldsAfterARestartAndForQA(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCleanBase("PR-12407")
	merged := r.writerMerges(task, nil)
	r.toBody(task)
	if sum := r.summaryOf(task.ID); sum.Head != merged || sum.BaseMerge == nil || sum.BaseMerge.Kind != mergeClean {
		t.Fatalf("review-summary.json %+v, want the clean merge's pass", sum)
	}
	r.restart()
	got := r.reload(task.ID)
	r.setFailGit(func([]string) error { return errors.New("no git past the record") })
	handled, done, err := r.e.mergedBase(context.Background(), got, r.cp)
	r.setFailGit(nil)
	if err != nil || !handled || !done {
		t.Fatalf("the check of a fresh loop after the restart: %v, %v, %v; want it passed at the merge with no git read", handled, done, err)
	}
	if reviewed, err := r.e.qaFixReviewed(r.ctx, got); err != nil || !reviewed {
		t.Fatalf("QA reads the merge as reviewed: %v, %v; want true", reviewed, err)
	}
	if n := len(r.rv.calls()); n != 1 {
		t.Fatalf("%d local rounds, want the first alone", n)
	}
}
