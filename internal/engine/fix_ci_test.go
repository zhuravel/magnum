package engine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
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
	dry := h.fix(FixPayload{Mode: FixModeCI, DryRun: true})
	h.handle()
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
	if rec := h.ciRecordOf(); rec.count() != 2 || len(h.fixRuns(h.pr(2).ID)) != 2 {
		t.Errorf("a try by hand after a cannot: %d CI fixes counted, %d fix runs; want 2 of each", rec.count(), len(h.fixRuns(h.pr(2).ID)))
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
	h, mg := mergeHarness(t, withFixerApp, requiredCompletion, func(h *harness) { h.gh.defaultFiles = []string{"app/models/offer.rb"} })
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
