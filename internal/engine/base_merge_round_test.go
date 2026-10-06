package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// noThreshold lets a 2-line re-review run at once.
func noThreshold(h *harness) { h.cfg.Daemon.RereviewMinLines = 0 }

// mergedRereview reviews PR #2 at reviewedTip, then pushes mergedHead: a
// push that merges master's 46 lines and changes 2 lines of the PR's own
// code (ownAfterConflict), and runs its re-review.
func mergedRereview(t *testing.T, h *harness) {
	t.Helper()
	setPushFiles(h, true, "ahead", ownBefore, ownAfterConflict)
	h.reviewedPR(2, reviewedTip)
	pollPR(h, time.Minute, 2, mergedHead)
	h.wantState(2, store.PRRereviewPending)
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: mergedHead})
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
}

// Triage of a re-review after a push that merged master reads the PR's own
// diff: the file whose own change differs, as its diff against the base,
// not master's files (which would put the push above max_lines).
func TestTriageOfABaseMergeReadsThePRsOwnDiff(t *testing.T) {
	h, model := triageHarness(t, modelAnswers(`{"run": ["claude-review"], "reason": "logic"}`), noThreshold,
		func(h *harness) { h.cfg.Triage.MaxLines = 40 })
	mergedRereview(t, h)
	if len(model.Calls) != 2 {
		t.Fatalf("model calls = %d, want the first review and the re-review", len(model.Calls))
	}
	stdin := string(model.Calls[1].Stdin)
	for _, frag := range []string{"+++ b/app/models/coupon.rb", "+    LIMIT * 3", "(3 changed lines)", "merged or rebased onto the base branch"} {
		if !strings.Contains(stdin, frag) {
			t.Errorf("the prompt lacks %q:\n%s", frag, stdin)
		}
	}
	if strings.Contains(stdin, "lib/master_") || strings.Contains(stdin, "app/services/redeem.rb") {
		t.Errorf("the prompt has files whose own change did not change:\n%s", stdin)
	}
}

// A file the push took out of the PR's own diff reads, for triage, as its
// earlier change reverted; a rename undone has no such diff.
func TestAFileThePRNoLongerChangesReadsAsItsChangeReverted(t *testing.T) {
	d, ok := compareOwnDiffs(ownBefore, ownBefore[:1])
	if !ok || !slices.Equal(d.changed, []string{"app/services/redeem.rb"}) || len(d.files) != 1 {
		t.Fatalf("own diff %+v (%v)", d, ok)
	}
	want := github.FileDelta{Path: "app/services/redeem.rb", Status: "removed", Patch: "@@ -1,2 +0,0 @@\n-class Redeem\n-end"}
	if d.files[0] != want {
		t.Fatalf("undone file = %+v, want %+v", d.files[0], want)
	}
	coupon := undone(ownBefore[0])
	if coupon.Status != "modified" || coupon.Patch != "@@ -10,5 +10,4 @@ class Coupon\n   def limit\n+    LIMIT\n-    LIMIT * 2\n-    # doubled for the holidays\n   end" {
		t.Fatalf("undone modification = %+v", coupon)
	}
	if r := undone(github.FileDelta{Path: "b.rb", PreviousPath: "a.rb", Status: "renamed", Patch: "@@ -1 +1 @@\n-a\n+b"}); !r.Truncated {
		t.Fatalf("undone rename = %+v, want unread", r)
	}
}

// The simplify rerun measures the PR's own change after a merge of master:
// 2 own lines do not rerun it, master's 46 would have.
func TestRerunAfterABaseMergeMeasuresThePRsOwnChange(t *testing.T) {
	h := newHarness(t, noThreshold, func(h *harness) {
		for i := range h.cfg.Roles {
			if h.cfg.Roles[i].Name == "claude-simplify" {
				h.cfg.Roles[i].RerunMinLines = 30
			}
		}
	})
	mergedRereview(t, h)
	if in := h.rd.all()[1]; slices.Contains(in.Requested, "claude-simplify") {
		t.Fatalf("simplify reran on master's lines: requested %v", in.Requested)
	}
	if evs := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "round.rerun_role"); len(evs) != 0 {
		t.Fatalf("round.rerun_role events: %+v", evs)
	}
}

// The re-review after a push that merged the base branch tells its prompts
// so (RoundInput.BaseMerged); a plain push does not.
func TestRereviewInputSaysThePushMergedTheBase(t *testing.T) {
	h := newHarness(t, noThreshold)
	mergedRereview(t, h)
	if in := h.rd.all()[1]; !in.BaseMerged || in.ForcePushed {
		t.Fatalf("re-review input: base merged %v, force pushed %v", in.BaseMerged, in.ForcePushed)
	}

	h = newHarness(t, noThreshold)
	reviewedThenPushed(t, h, "", rubyMixed)
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	if ins := h.rd.all(); len(ins) != 2 || ins[1].BaseMerged {
		t.Fatalf("a plain push: rounds %d, base merged %v", len(ins), len(ins) == 2 && ins[1].BaseMerged)
	}
}

// A recovery round (the judge's session is gone) after a push that merged
// the base branch tells its prompt so too: the fresh judge reads the PR's
// own diff before and after the push, not the base branch's commits.
func TestARecoveryAfterABaseMergeSaysThePushMergedTheBase(t *testing.T) {
	h := newHarness(t, noThreshold)
	setPushFiles(h, true, "ahead", ownBefore, ownAfterConflict)
	h.reviewedPR(2, reviewedTip)
	pollPR(h, time.Minute, 2, mergedHead)
	h.wantState(2, store.PRRereviewPending)
	parkSessions(h, 2)
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: mergedHead})
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].Kind != pipeline.KindRecovery {
		t.Fatalf("rounds = %d (second %+v), want a recovery", len(ins), ins[len(ins)-1].Kind)
	}
	if !ins[1].BaseMerged || ins[1].ForcePushed {
		t.Fatalf("recovery input: base merged %v, force pushed %v", ins[1].BaseMerged, ins[1].ForcePushed)
	}
}

// A re-review restarted on a newer head learns whether the commits since
// the review merged the base branch up to that head.
func TestRestartOnAMergedHeadSaysThePushMergedTheBase(t *testing.T) {
	h := newHarness(t, noThreshold)
	setPushFiles(h, true, "ahead", ownBefore, ownAfterConflict)
	h.gh.mu.Lock()
	h.gh.files[reviewedTip+"...p1"] = rubyMixed
	h.gh.compare[reviewedTip+"...p1"] = github.CompareStats{Commits: 1}
	h.gh.mu.Unlock()
	h.reviewedPR(2, reviewedTip)
	pollPR(h, time.Minute, 2, "p1")
	h.advance(time.Hour)
	var switched bool
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.BaseMerged {
			t.Errorf("the plain push p1 reads as a base merge")
		}
		h.pushed(2, mergedHead)
		sw, err := in.Switch(context.Background(), mergedHead)
		if err != nil {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
		}
		switched = sw.BaseMerged
		return posted(sw.TargetSHA, 1), nil
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "p1"})
	h.tick()
	if !switched {
		t.Fatal("the restart on the merge does not say the push merged the base")
	}
}

// A force push back to an ancestor of the reviewed commit: GitHub calls the
// range "behind", with no commit and no file. That measures nothing (the
// review discusses code the push dropped), so the re-review is not held as
// a 0-line delta.
func TestAPushBackToAnAncestorIsNotASmallDelta(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.gh.mu.Lock()
	h.gh.files = map[string][]github.FileDelta{"b1...b0": {}}
	h.gh.statuses = map[string]string{"b1...b0": "behind"}
	h.gh.mu.Unlock()
	pollPR(h, 40*time.Minute, 2, "b0")
	pr := h.wantState(2, store.PRRereviewPending)
	if rec, ok := h.e.deltaRecord(h.ctx, pr.ID); ok {
		t.Fatalf("delta record %+v for a push GitHub cannot measure", rec)
	}
	if w := waitOf(t, h, 2); w.Reason == WaitDelta {
		t.Fatalf("wait = %+v, held as a small delta", w)
	}
}

// A merge of master that arrives during the review: the note on the review
// counts the PR's own commits that arrived (by SHA, as the PR's commits tab
// lists them: the merge commit and the PR's new one), not the 13 commits
// of master the merge brought; one that only merges master too.
func TestMovedHeadNoteCountsThePRsOwnCommitsAcrossABaseMerge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shas  []string
		after []github.FileDelta
		note  string
	}{
		{"a merge that changes the PR's code", []string{"c1", "c2", "c3", "mc"}, ownAfterConflict,
			"101:_Reviewed 2cb4d7c; 2 commits arrived during the review, re-review follows"},
		{"a merge only", []string{"c1", "c2", "mc"}, ownAfterMerge,
			"101:_Reviewed 2cb4d7c; 1 commit arrived during the review (base merge only), no re-review needed._"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, noThreshold)
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: reviewedTip})
			h.tick()
			h.advance(5 * time.Minute)
			h.rd.gate = make(chan struct{})
			before := len(h.rd.all())
			if err := h.e.Tick(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.awaitRound(before)
			h.advance(time.Minute)
			setPushFiles(h, true, "ahead", ownBefore, tc.after)
			h.gh.mu.Lock()
			h.gh.shas = map[string][]string{"master..." + reviewedTip: {"c1", "c2"}, "master..." + mergedHead: tc.shas}
			h.gh.compare["master..."+reviewedTip] = github.CompareStats{Commits: 2}
			h.gh.compare["master..."+mergedHead] = github.CompareStats{Commits: len(tc.shas)}
			h.gh.mu.Unlock()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: mergedHead})
			if err := h.e.Tick(h.ctx); err != nil {
				t.Fatal(err)
			}
			close(h.rd.gate)
			h.settle()
			if got := h.rd.appended(); len(got) != 1 || !strings.HasPrefix(got[0], tc.note) {
				t.Fatalf("review notes = %q, want %q", got, tc.note)
			}
		})
	}
}
