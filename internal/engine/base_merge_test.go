package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// The PR's own diff against master as it was reviewed: a changed method and
// an added class.
var ownBefore = []github.FileDelta{
	{Path: "app/models/coupon.rb", Status: "modified",
		Patch: "@@ -10,4 +10,5 @@ class Coupon\n   def limit\n-    LIMIT\n+    LIMIT * 2\n+    # doubled for the holidays\n   end"},
	{Path: "app/services/redeem.rb", Status: "added", Patch: "@@ -0,0 +1,2 @@\n+class Redeem\n+end"},
}

// ownAfterMerge is the same own diff after master was merged in: master
// added lines above the change (the hunk header moved) and edited a context
// line, but the PR's added and removed lines are the same.
var ownAfterMerge = []github.FileDelta{
	{Path: "app/models/coupon.rb", Status: "modified",
		Patch: "@@ -14,4 +14,5 @@ class Coupon < ApplicationRecord\n   def limit # the cap\n-    LIMIT\n+    LIMIT * 2\n+    # doubled for the holidays\n   end"},
	{Path: "app/services/redeem.rb", Status: "added", Patch: "@@ -0,0 +1,2 @@\n+class Redeem\n+end"},
}

// ownAfterConflict is the own diff after a merge whose conflict resolution
// changed one of the PR's lines.
var ownAfterConflict = []github.FileDelta{
	{Path: "app/models/coupon.rb", Status: "modified",
		Patch: "@@ -14,4 +14,5 @@ class Coupon < ApplicationRecord\n   def limit # the cap\n-    LIMIT\n+    LIMIT * 3\n+    # doubled for the holidays\n   end"},
	ownAfterMerge[1],
}

// masterFiles is what reviewed...head lists after the merge: master's own
// work, 46 changed code lines in three files the PR never touched.
func masterFiles() []github.FileDelta {
	var out []github.FileDelta
	for i, n := range []int{20, 2, 1} {
		var b strings.Builder
		fmt.Fprintf(&b, "@@ -1,%d +1,%d @@", n, n)
		for j := range n {
			fmt.Fprintf(&b, "\n-old_%d = %d\n+new_%d = %d", j, j, j, j+1)
		}
		out = append(out, github.FileDelta{Path: fmt.Sprintf("lib/master_%d.rb", i), Status: "modified", Patch: b.String()})
	}
	return out
}

const (
	reviewedTip = "2cb4d7c000000000000000000000000000000000"
	mergedHead  = "2017f29000000000000000000000000000000000"
)

// baseMergePush reviews PR #2 at reviewedTip, then pushes mergedHead: a push
// of 13 commits that lists master's files, with merge (a merge commit) or
// status ("diverged" for a rebase), and the PR's own diff before and after
// (nil = GitHub does not answer).
func baseMergePush(t *testing.T, h *harness, merge bool, status string, before, after []github.FileDelta) store.PR {
	t.Helper()
	h.reviewedPR(2, reviewedTip)
	setPushFiles(h, merge, status, before, after)
	pollPR(h, time.Minute, 2, mergedHead)
	return h.pr(2)
}

func setPushFiles(h *harness, merge bool, status string, before, after []github.FileDelta) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	key := reviewedTip + "..." + mergedHead
	if h.gh.files == nil {
		h.gh.files = map[string][]github.FileDelta{}
	}
	h.gh.files[key] = masterFiles()
	h.gh.compare[key] = github.CompareStats{Commits: 13}
	h.gh.merges = map[string]bool{key: merge}
	h.gh.statuses = map[string]string{key: status}
	if before != nil {
		h.gh.files["master..."+reviewedTip] = before
	}
	if after != nil {
		h.gh.files["master..."+mergedHead] = after
	}
}

func ownDiffCalls(h *harness) []string {
	return callsWith(h.gh, "compare_files:talkable/talkable:master...")
}

// The live case: the author merged master into a reviewed PR; the PR's own
// changes are what they were, so the review stands.
func TestBaseMergePushIsNotReReviewed(t *testing.T) {
	h := newHarness(t)
	pr := baseMergePush(t, h, true, "ahead", ownBefore, ownAfterMerge)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != mergedHead {
		t.Fatalf("state %s reviewed_sha %q, want reviewed at the merge", pr.State, deref(pr.ReviewedSHA))
	}
	evs := trivialEvents(t, h)
	want := "reviewed → reviewed: the push to 2017f29 only merges master (13 commits, the PR's own changes unchanged) since the review of 2cb4d7c; no re-review, the review stands"
	if len(evs) != 1 || evs[0].Message != want {
		t.Fatalf("pr.trivial_delta events: %+v\nwant %q", evs, want)
	}
	var data struct{ Classes []string }
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || !slices.Equal(data.Classes, []string{DeltaBase}) {
		t.Fatalf("event data %s (%v)", evs[0].Data, err)
	}
	v, _ := kvValue(h, KVPRTrivial(pr.ID))
	if skip, ok := ParseTrivialSkip(v); !ok || skip.Note() != "base-merge push skipped (2cb4d7c → 2017f29)" {
		t.Fatalf("%s = %q", KVPRTrivial(pr.ID), v)
	}
	if got := ownDiffCalls(h); len(got) != 2 {
		t.Fatalf("own diff compares = %q, want before and after", got)
	}
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: mergedHead})
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want only the first", n)
	}
}

// A merge whose resolution changed the PR's own code is re-reviewed, and
// the re-review threshold measures that change, not master's.
func TestBaseMergeThatChangesAPRFileIsReReviewed(t *testing.T) {
	h := newHarness(t)
	pr := baseMergePush(t, h, true, "ahead", ownBefore, ownAfterConflict)
	if pr.State != store.PRRereviewPending || deref(pr.ReviewedSHA) != reviewedTip || len(trivialEvents(t, h)) != 0 {
		t.Fatalf("state %s reviewed_sha %q, want a re-review", pr.State, deref(pr.ReviewedSHA))
	}
	rec, ok := h.e.deltaRecord(h.ctx, pr.ID)
	if !ok || rec.Lines != 2 || rec.AddedFiles != 0 || !rec.Complete || rec.Version != deltaRecordVersion || rec.To != mergedHead {
		t.Fatalf("delta record %+v, want the 2 lines of the PR's own change", rec)
	}
}

// A rebase onto master (GitHub: diverged) that leaves the PR's own changes
// alone settles like a merge.
func TestRebaseWithTheSameOwnChangesIsNotReReviewed(t *testing.T) {
	h := newHarness(t)
	pr := baseMergePush(t, h, false, "diverged", ownBefore, ownAfterMerge)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != mergedHead {
		t.Fatalf("state %s reviewed_sha %q", pr.State, deref(pr.ReviewedSHA))
	}
	if evs := trivialEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "the push to 2017f29 only rebases onto master (13 commits, the PR's own changes unchanged)") {
		t.Fatalf("pr.trivial_delta events: %+v", evs)
	}
}

// When the PR's own diff cannot be compared in full, the push is measured
// as before: reviewed...head, master's lines included.
func TestIncompleteOwnDiffFallsBack(t *testing.T) {
	truncatedAfter := slices.Clone(ownAfterMerge)
	truncatedAfter[1].Truncated = true
	capped := make([]github.FileDelta, len(ownAfterMerge))
	for i, f := range ownAfterMerge {
		f.Truncated = true // a listing at GitHub's file cap marks every file
		capped[i] = f
	}
	for _, tc := range []struct {
		name  string
		after []github.FileDelta
	}{
		{"a truncated patch", truncatedAfter},
		{"the file cap", capped},
		{"the compare fails", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pr := baseMergePush(t, h, true, "ahead", ownBefore, tc.after)
			if pr.State != store.PRRereviewPending || len(trivialEvents(t, h)) != 0 {
				t.Fatalf("state %s, want today's re-review", pr.State)
			}
			rec, ok := h.e.deltaRecord(h.ctx, pr.ID)
			if want := MeasureDelta(masterFiles()); !ok || rec.DeltaSize != want {
				t.Fatalf("delta record %+v, want reviewed...head's %+v", rec, want)
			}
		})
	}
}

// A plain push (no merge commit, not diverged) costs the one comparison it
// always did.
func TestPlainPushMakesNoOwnDiffCalls(t *testing.T) {
	h := newHarness(t)
	pr := reviewedThenPushed(t, h, "", rubyMixed)
	if pr.State != store.PRRereviewPending {
		t.Fatalf("state %s", pr.State)
	}
	if got := callsWith(h.gh, "compare_files:"); len(got) != 1 || len(ownDiffCalls(h)) != 0 {
		t.Fatalf("compare calls = %q, want only b1...b2", got)
	}
}

// oldRecordPR leaves PR #2 rereview_pending after a base merge measured the
// old way (no merge seen, a record without a version), then makes GitHub
// report the merge and the PR's own diff: what a daemon from before
// DeltaBase left behind.
func oldRecordPR(t *testing.T, h *harness, after []github.FileDelta) store.PR {
	t.Helper()
	pr := baseMergePush(t, h, false, "ahead", nil, nil)
	if pr.State != store.PRRereviewPending {
		t.Fatalf("state %s, want rereview_pending", pr.State)
	}
	rec, _ := h.e.deltaRecord(h.ctx, pr.ID)
	old, _ := json.Marshal(map[string]any{"from": rec.From, "to": rec.To, "lines": rec.Lines, "added_files": rec.AddedFiles,
		"complete": rec.Complete, "since": rec.Since})
	if err := h.st.SetKV(h.ctx, KVPRDelta(pr.ID), string(old)); err != nil {
		t.Fatal(err)
	}
	setPushFiles(h, true, "ahead", ownBefore, after)
	return pr
}

// restart replaces the engine with a new one on the same registry, as a
// daemon restart does, and runs its startup and first tick.
func (h *harness) restart() {
	h.t.Helper()
	h.e = New(h.d)
	h.startup()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: mergedHead})
	h.tick()
}

// After the upgrade, a PR already queued for a re-review because of a base
// merge is checked again once and settles.
func TestOldBaseMergeRereviewIsRecheckedOnce(t *testing.T) {
	h := newHarness(t)
	oldRecordPR(t, h, ownAfterMerge)
	h.restart()
	pr := h.pr(2)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != mergedHead {
		t.Fatalf("state %s reviewed_sha %q, want settled", pr.State, deref(pr.ReviewedSHA))
	}
	if evs := trivialEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "rereview_pending → reviewed: the push to 2017f29 only merges master") {
		t.Fatalf("pr.trivial_delta events: %+v", evs)
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want no re-review", n)
	}
}

// One whose own diff did change is measured again once: the record gets
// the PR's own change and the version, and the next daemon leaves it be.
func TestOldRereviewWithAChangedOwnDiffIsRemeasuredOnce(t *testing.T) {
	h := newHarness(t)
	pr := oldRecordPR(t, h, ownAfterConflict)
	h.restart()
	if got := h.wantState(2, store.PRRereviewPending); deref(got.ReviewedSHA) != reviewedTip {
		t.Fatalf("reviewed_sha %q", deref(got.ReviewedSHA))
	}
	rec, ok := h.e.deltaRecord(h.ctx, pr.ID)
	if !ok || rec.Lines != 2 || rec.Version != deltaRecordVersion {
		t.Fatalf("delta record %+v, want the PR's own 2 lines", rec)
	}
	calls := len(callsWith(h.gh, "compare_files:"))
	h.restart()
	if got := len(callsWith(h.gh, "compare_files:")); got != calls {
		t.Fatalf("compare calls %d → %d: re-checked again", calls, got)
	}
}

// A forced PR is the user's: it is not re-checked, its round runs.
func TestOldForcedRereviewIsNotRechecked(t *testing.T) {
	h := newHarness(t)
	pr := oldRecordPR(t, h, ownAfterMerge)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if rec, ok := h.e.deltaRecord(h.ctx, pr.ID); !ok || rec.Version != 0 {
		t.Fatalf("delta record %+v: the forced PR was measured again", rec)
	}
	if evs := trivialEvents(t, h); len(evs) != 0 {
		t.Fatalf("pr.trivial_delta events: %+v", evs)
	}
	if ins := h.rd.all(); len(ins) != 2 || ins[1].TargetSHA != mergedHead {
		t.Fatalf("rounds = %+v, want the forced re-review of the merge", ins)
	}
}

func TestCompareOwnDiffs(t *testing.T) {
	coupon := func(header, context, line string) github.FileDelta {
		return github.FileDelta{Path: "app/models/coupon.rb", Status: "modified",
			Patch: header + "\n " + context + "\n-    LIMIT\n" + line + "\n   end"}
	}
	same := coupon("@@ -10,3 +10,3 @@", "def limit", "+    LIMIT * 2")
	for _, tc := range []struct {
		name          string
		before, after []github.FileDelta
		changed       []string
		lines         int
		ok            bool
	}{
		{"identical", []github.FileDelta{same}, []github.FileDelta{same}, nil, 0, true},
		{"the hunk header moved", []github.FileDelta{same}, []github.FileDelta{coupon("@@ -40,3 +40,3 @@ class Coupon", "def limit", "+    LIMIT * 2")}, nil, 0, true},
		{"a context line changed", []github.FileDelta{same}, []github.FileDelta{coupon("@@ -10,3 +10,3 @@", "def limit # master's note", "+    LIMIT * 2")}, nil, 0, true},
		{"an added line changed", []github.FileDelta{same}, []github.FileDelta{coupon("@@ -10,3 +10,3 @@", "def limit", "+    LIMIT * 3")}, []string{"app/models/coupon.rb"}, 2, true},
		{"only re-indented", []github.FileDelta{same}, []github.FileDelta{coupon("@@ -10,3 +10,3 @@", "def limit", "+      LIMIT * 2")}, []string{"app/models/coupon.rb"}, 0, true},
		{"a comment changed", []github.FileDelta{same}, []github.FileDelta{coupon("@@ -10,3 +10,3 @@", "def limit", "+    LIMIT * 2\n+    # why")}, []string{"app/models/coupon.rb"}, 0, true},
		{"a file the PR no longer changes", []github.FileDelta{same, ownBefore[1]}, []github.FileDelta{same}, []string{"app/services/redeem.rb"}, 2, true},
		{"a file the PR now adds", []github.FileDelta{same}, []github.FileDelta{same, ownBefore[1]}, []string{"app/services/redeem.rb"}, 0, true},
		{"in neither", nil, nil, nil, 0, true},
		{"a truncated patch", []github.FileDelta{same}, []github.FileDelta{{Path: same.Path, Status: "modified", Patch: same.Patch, Truncated: true}}, nil, 0, false},
		{"a binary file", []github.FileDelta{{Path: "public/logo.png", Status: "added", Truncated: true}}, []github.FileDelta{{Path: "public/logo.png", Status: "added", Truncated: true}}, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := compareOwnDiffs(tc.before, tc.after)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if !slices.Equal(d.changed, tc.changed) || d.size.Lines != tc.lines || !d.size.Complete {
				t.Fatalf("changed %q lines %d complete %v, want %q %d", d.changed, d.size.Lines, d.size.Complete, tc.changed, tc.lines)
			}
		})
	}
	if d, _ := compareOwnDiffs([]github.FileDelta{same}, []github.FileDelta{same, ownBefore[1]}); d.size.AddedFiles != 1 {
		t.Fatalf("added files = %d, want the file the PR now adds", d.size.AddedFiles)
	}
}

func TestOwnChange(t *testing.T) {
	patch := "@@ -1,3 +1,3 @@ module A\n context\n-old\n+new\n\\ No newline at end of file\n@@ -9 +9 @@\n tail\n\\ No newline at end of file"
	want := []string{"-old", "+new", `\ No newline at end of file`}
	if got := ownChange(patch); !slices.Equal(got, want) {
		t.Fatalf("own change = %q, want %q", got, want)
	}
}

func TestBaseMergeLabels(t *testing.T) {
	if got := DeltaLabel([]string{DeltaBase}); got != "base merge only" {
		t.Errorf("label = %q", got)
	}
	if !slices.Contains(DeltaClasses, DeltaBase) {
		t.Errorf("classes %q lack %q", DeltaClasses, DeltaBase)
	}
}
