package engine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// wantFixRefused asks for a CI fix of PR #2 and fails unless the daemon
// refuses it with want in its words, starting no fixer.
func (h *harness) wantCIFixRefused(want string) {
	h.t.Helper()
	id := h.fix(FixPayload{Mode: FixModeCI})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), want) {
		h.t.Fatalf("request = %s %q, want refused with %q", r.State, deref(r.Result), want)
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		h.t.Fatalf("%d fix runs, want none", n)
	}
}

// `magnum fix --ci` (the screens' c) refuses with reasons: a coworker's PR,
// a head where no required check failed, a failure red on the base, the
// cap of 3 CI fixes, and a head whose review leaves work, which f fixes
// first.
func TestMagnumFixCIRefusesOthersPRsNothingFailedRedBaseAndTheCap(t *testing.T) {
	storetest.Parallel(t)
	t.Run("another's PR", func(t *testing.T) {
		h, _ := autofixHarness(t, "alice", findings(map[string]int{}, "COMMENTED"), requiredCompletion)
		h.tick()
		h.wantCIFixRefused("PR #2 is alice's, not yours: magnum fixes the CI of your own PRs alone")
	})
	t.Run("nothing failed", func(t *testing.T) {
		h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion)
		h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: []github.Check{ciCompletion(github.CheckPassed)}})
		h.tick()
		h.wantCIFixRefused("nothing to fix: no required check failed at a1a1a1a")
	})
	t.Run("red on the base", func(t *testing.T) {
		h, _ := ciFixHarness(t, github.CheckFailed)
		h.wantCIFixRefused("required check Completion failed at a1a1a1a, and on main at ")
		if n := len(h.ghCalls("job_log:")); n != 0 {
			t.Errorf("%d log reads for a failure red on the base", n)
		}
	})
	t.Run("the cap", func(t *testing.T) {
		h, _ := ciFixHarness(t, github.CheckPassed)
		var rec ciFixRecord
		for _, head := range []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333"} {
			rec.Starts = append(rec.Starts, ciFixStart{Head: head, Entries: []string{"Completion"}, Outcome: ciOutcomePushed, Pushed: head})
		}
		b, _ := json.Marshal(rec)
		if err := h.st.SetKV(h.ctx, kvPRCIFix(h.pr(2).ID), string(b)); err != nil {
			t.Fatal(err)
		}
		h.wantCIFixRefused("required check Completion failed at a1a1a1a after 3 CI fixes: the CI is yours")
	})
	t.Run("findings first", func(t *testing.T) {
		h, _ := ciFixHarnessWith(t, github.CheckPassed, findings(map[string]int{"P2": 1}, "COMMENTED"))
		h.wantCIFixRefused("leaves work: f fixes it first, then the CI after its push")
	})
}

// The plan its y/N asks names the fixer, the failed check, the head, the
// failed jobs, the slot, the login and the count, and starts nothing; the
// start bound to it refuses what moved since, and starts the CI fix on the
// plan as asked. A try by hand after a CI fix that said cannot at the head
// starts again, and counts.
func TestMagnumFixCIDryRunAnswersThePlan(t *testing.T) {
	h, git := ciFixHarness(t, github.CheckPassed)
	cannotFixer(h, git, "flaky")
	others := func() int {
		return len(h.ghCalls("threads:")) + len(h.ghCalls("issue_comments:")) + len(h.ghCalls("all_reviews:"))
	}
	before := others()
	dry := h.fix(FixPayload{Mode: FixModeCI, DryRun: true})
	h.handle()
	if n := others() - before; n != 0 {
		t.Errorf("the plan read %d lists of the other reviewers' items: a CI fix answers nobody", n)
	}
	plan, ok := ParseCIFixPlan(h.wantDone(dry))
	if !ok {
		t.Fatalf("answer %q is no plan", h.wantDone(dry))
	}
	if want := CIFixDry + "talkable/talkable#2: claude-fix fixes required check Completion at a1a1a1a (2 failed jobs) in review1 as zhuravel, CI fix 1 of 3"; plan.Text != want {
		t.Errorf("plan = %q, want %q", plan.Text, want)
	}
	if a := plan.Asked; a.Head != fixHead || strings.Join(a.Entries, ",") != "Completion" || a.Count != 0 {
		t.Errorf("asked = %+v", a)
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 || len(h.ciRecordOf().Starts) != 0 {
		t.Fatalf("the dry run started %d fixers or recorded a start", n)
	}
	moved := plan.Asked
	moved.Count = 1
	id := h.fix(FixPayload{Mode: FixModeCI, CIAsked: &moved})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "the PR had another CI fix since the question: ask again") {
		t.Fatalf("a moved count: %s %q", r.State, deref(r.Result))
	}
	id = h.fix(FixPayload{Mode: FixModeCI, CIAsked: &plan.Asked})
	h.handle()
	if res := h.wantDone(id); !strings.HasPrefix(res, "fixing the failed CI of talkable/talkable#2: claude-fix fixes the failed CI at a1a1a1a (2 failed jobs) in review1, CI fix 1 of 3") {
		t.Errorf("answer = %q", res)
	}
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].By != fixBy || rec.Starts[0].Outcome != ciOutcomeCannot {
		t.Fatalf("record = %+v", rec.Starts)
	}
	again := h.fix(FixPayload{Mode: FixModeCI})
	h.handle()
	h.wantDone(again)
	if rec := h.ciRecordOf(); rec.Count() != 2 || len(h.fixRuns(h.pr(2).ID)) != 2 {
		t.Errorf("a try by hand after a cannot: %d CI fixes counted, %d fix runs; want 2 of each", rec.Count(), len(h.fixRuns(h.pr(2).ID)))
	}
	stale := plan.Asked
	stale.Count, stale.At = 2, stale.At.Add(-time.Hour)
	id = h.fix(FixPayload{Mode: FixModeCI, CIAsked: &stale})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "old: ask again") {
		t.Errorf("an old question: %s %q", r.State, deref(r.Result))
	}
}

// A CI fix merges the parent first as every loop's first cycle does: the
// daemon merges a base tip the head lacks and pushes it, and the fixer
// starts from that merge, with the failed CI's file copied beside its
// report there, the merge named in its prompt, and a push line that
// refuses a CI config change.
func TestACIFixMergesTheParentFirstThenNamesCIFailuresInThePrompt(t *testing.T) {
	h, mg := ciMergeHarness(t)
	fixer(h, mg.fixGit, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "0 threads", "- 101 ci: not fixed. infra: the runner lost its disk")
	})
	id := h.fix(FixPayload{Mode: FixModeCI})
	h.handle()
	h.wantDone(id)
	wantParentMergedBeforeTheFixer(t, h, mg)
	in := h.fixInput()
	dir := h.d.Layout.ReviewDir("talkable", "talkable", 2, fixParentMerge)
	if !in.CIOnly || in.CIFile != filepath.Join(dir, ciFailuresFile) || in.MergedSHA != fixBaseTip || !strings.HasSuffix(in.PushCommand, " --ci") {
		t.Fatalf("fix input = %+v, want the CI file beside the report at the merge, the merge, and the CI push line", in)
	}
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeMerged || rec.Starts[0].Pushed != fixParentMerge {
		t.Errorf("record = %+v, want the merge recorded as the CI fix's", rec.Starts)
	}
}

// ciMergeHarness is the operator's PR #2 of talkable/talkable on a fixer
// App, behind its base, whose required Completion failed at fixHead in run
// ciRunID (setFailedJobs) and passed on the base, reviewed clean; mods go
// to the harness before the engine is built.
func ciMergeHarness(t *testing.T, mods ...func(*harness)) (*harness, *mergeGit) {
	t.Helper()
	h, mg := mergeHarness(t, append([]func(*harness){withFixerApp, requiredCompletion, func(h *harness) { h.gh.defaultFiles = []string{"app/models/offer.rb"} }},
		mods...)...)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", ci: "FAILURE", checks: []github.Check{ciCompletion(github.CheckFailed)}})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick() // reviewed
	recordReviewResult(h, h.wantState(2, store.PRReviewed), map[string]any{"event": "COMMENTED", "findings": map[string]int{}})
	h.setHeadRuns(fixHead, failedCIRun(fixHead, 1))
	h.setFailedJobs()
	h.setBaseCI(github.CheckPassed)
	mg.behind = true
	return h, mg
}

// A CI fix whose fixer said it cannot, on the daemon's merge of the parent,
// keeps its class word beside the merge: its end names it, and the merge's
// head needs you with it rather than starting a CI fix for the same cause.
// A merge of the parent the daemon could not push goes to you as any
// loop's does, though the CI fix ends quietly.
func TestACIFixThatCannotOnTheParentsMergeKeepsItsClass(t *testing.T) {
	storetest.Parallel(t)
	t.Run("merged", func(t *testing.T) {
		h, mg := ciMergeHarness(t)
		fixer(h, mg.fixGit, "", func(run store.Run) string {
			return fixReport(run.ID, "nothing", "0 threads", "- 101 ci: not fixed. secret: the job needs a key the fork lacks")
		})
		id := h.fix(FixPayload{Mode: FixModeCI})
		h.handle()
		h.wantDone(id)
		rec := h.ciRecordOf()
		if len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeMerged || rec.Starts[0].Why != "secret" {
			t.Fatalf("record = %+v, want the merge with the class", rec.Starts)
		}
		if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.Contains(ev.Message, "a missing secret") || !strings.Contains(ev.Message, fixParentMerge[:7]) {
			t.Errorf("fix.ended = %q, %v; want the class and the merge", ev.Message, ok)
		}
		if at := rec.AtHead(fixParentMerge); at.Why != "secret" {
			t.Errorf("the merge's head reads %+v, want the CI fix that said cannot", at)
		}
		f := ciFailedFacts()
		f.ciFix.starts, f.ciFix.atHead = 1, ciFixStart{Head: "e0e0", Outcome: ciOutcomeMerged, Why: "secret", Pushed: fixHead}
		v, work, _ := decideOpen(f)
		wantNeedsCI(t, v, work, "required check Completion failed at a1a1a1a: the CI fix found a missing secret")
	})
	t.Run("the merge refused", func(t *testing.T) {
		h, mg := ciMergeHarness(t, withoutWorkflows)
		mg.workflows = []string{".github/workflows/ci.yml"}
		cannot := func(run store.Run) string {
			return fixReport(run.ID, "nothing", "0 threads", "- 101 ci: not fixed. infra: the runner lost its disk")
		}
		fixer(h, mg.fixGit, "", cannot)
		id := h.fix(FixPayload{Mode: FixModeCI})
		h.handle()
		h.wantDone(id)
		pr := h.wantState(2, store.PRNeedsAttention)
		if got := deref(pr.LastError); !strings.Contains(got, noWorkflowsRefusal) {
			t.Errorf("attention = %q, want it to say %q", got, noWorkflowsRefusal)
		}
		if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeCannot || rec.Starts[0].Why != "infra" {
			t.Errorf("record = %+v", rec.Starts)
		}
	})
}
