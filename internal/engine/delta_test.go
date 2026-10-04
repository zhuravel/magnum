package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// yamlCommentPatch is a delta that only rewrites YAML comment lines (the
// live case: a sizing comment reworded in one config file).
const yamlCommentPatch = `@@ -10,7 +10,7 @@ workers:
   default:
-    # sizing: 2 workers per queue
+    # sizing: two workers per queue, see the capacity notes
     count: 2
@@ -30,4 +30,5 @@ limits:
   memory: 512Mi
-  # keep below the node budget
+  # keep below the node budget (512Mi
+  # leaves headroom for the sidecar)
   cpu: 500m`

var (
	yamlComments = []github.FileDelta{{Path: "config/workers.yml", Status: "modified", Patch: yamlCommentPatch}}
	rubyMixed    = []github.FileDelta{{Path: "app/models/coupon.rb", Status: "modified", Patch: "@@ -1,4 +1,4 @@\n class Coupon\n-  # old note\n+  # new note\n-  LIMIT = 5\n+  LIMIT = 6\n end"}}
	docsOnly     = []github.FileDelta{{Path: "README.md", Status: "modified", Patch: "@@ -1,2 +1,2 @@\n # App\n-Old words.\n+New words."},
		{Path: "docs/setup.md", Status: "modified", Patch: "@@ -3 +3 @@\n-step one\n+step 1"}}
	whitespaceOnly = []github.FileDelta{{Path: "lib/tasks/sync.rake", Status: "modified", Patch: "@@ -1,4 +1,5 @@\n task :sync do\n-    run!\n+  run!\n+\n end"}}
	truncated      = []github.FileDelta{{Path: "config/workers.yml", Status: "modified", Patch: yamlCommentPatch, Truncated: true}}
)

// reviewedThenPushed is PR #2 reviewed at b1 (by the App, with event), then
// pushed to b2 whose delta is files, after one poll.
func reviewedThenPushed(t *testing.T, h *harness, event string, files []github.FileDelta) store.PR {
	t.Helper()
	if event != "" {
		approving(h, event)
	}
	h.reviewedPR(2, "b1")
	if files != nil {
		h.gh.mu.Lock()
		if h.gh.files == nil {
			h.gh.files = map[string][]github.FileDelta{}
		}
		h.gh.files["b1...b2"] = files
		h.gh.mu.Unlock()
	}
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
	pollPR(h, time.Minute, 2, "b2")
	return h.pr(2)
}

func trivialEvents(t *testing.T, h *harness) []store.Event {
	t.Helper()
	return kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "pr.trivial_delta")
}

// The live case: a push of YAML comment lines after a review is not
// re-reviewed; the review stands for the new head.
func TestCommentOnlyPushIsNotReReviewed(t *testing.T) {
	h := newHarness(t)
	pr := reviewedThenPushed(t, h, "", yamlComments)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" || deref(pr.LastReviewID) != 101 || deref(pr.LastReviewEvent) != "COMMENTED" {
		t.Fatalf("after a comment-only push: state %s reviewed_sha %q review %v %q", pr.State, deref(pr.ReviewedSHA), pr.LastReviewID, deref(pr.LastReviewEvent))
	}
	evs := trivialEvents(t, h)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "comments only (1 file)") {
		t.Fatalf("pr.trivial_delta events: %+v", evs)
	}
	var data struct {
		Classes  []string
		Files    int
		From, To string
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || !slices.Equal(data.Classes, []string{DeltaComments}) || data.Files != 1 || data.From != "b1" || data.To != "b2" {
		t.Fatalf("event data %s (%v)", evs[0].Data, err)
	}
	v, ok := kvValue(h, KVPRTrivial(pr.ID))
	skip, parsed := ParseTrivialSkip(v)
	if !ok || !parsed || skip.Note() != "comment-only push skipped (b1 → b2)" {
		t.Fatalf("%s = %q (note %q)", KVPRTrivial(pr.ID), v, skip.Note())
	}
	// Nothing is queued: an hour later no second round ran.
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want only the first", n)
	}
}

func TestTrivialPushClasses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []github.FileDelta
		trivial bool
	}{
		{"ruby comment plus code", rubyMixed, false},
		{"docs only", docsOnly, true},
		{"whitespace only", whitespaceOnly, true},
		{"truncated patch", truncated, false},
		{"compare failed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pr := reviewedThenPushed(t, h, "", tc.files)
			if tc.trivial {
				if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" || len(trivialEvents(t, h)) != 1 {
					t.Fatalf("state %s reviewed_sha %q", pr.State, deref(pr.ReviewedSHA))
				}
				return
			}
			if pr.State != store.PRRereviewPending || deref(pr.ReviewedSHA) != "b1" || len(trivialEvents(t, h)) != 0 {
				t.Fatalf("state %s reviewed_sha %q, want a re-review of b2", pr.State, deref(pr.ReviewedSHA))
			}
			if _, ok := kvValue(h, KVPRTrivial(pr.ID)); ok {
				t.Fatal("trivial note recorded for a push that is re-reviewed")
			}
		})
	}
}

// skip_trivial_deltas = [] (daemon or watch) re-reviews every push; without
// a re-review threshold (rereview_min_lines = 0) it does not even ask
// GitHub for the patches.
func TestTrivialSkipCanBeTurnedOff(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		name    string
		mod     func(h *harness)
		patches bool // the threshold still measures the delta
	}{
		{"daemon", func(h *harness) {
			h.cfg.Daemon.SkipTrivialDeltas = []string{}
			h.cfg.Daemon.RereviewMinLines = 0
		}, false},
		{"watch", func(h *harness) {
			for i := range h.cfg.Watches {
				h.cfg.Watches[i].SkipTrivialDeltas = []string{}
				h.cfg.Watches[i].RereviewMinLines = &zero
			}
		}, false},
		{"watch without comments", func(h *harness) {
			for i := range h.cfg.Watches {
				h.cfg.Watches[i].SkipTrivialDeltas = []string{DeltaDocs}
			}
		}, true},
		{"skip off, threshold on", func(h *harness) { h.cfg.Daemon.SkipTrivialDeltas = []string{} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			pr := reviewedThenPushed(t, h, "", yamlComments)
			if pr.State != store.PRRereviewPending {
				t.Fatalf("state %s, want rereview_pending", pr.State)
			}
			if got := h.gh.count("compare_files:") != 0; got != tc.patches {
				t.Fatalf("patches fetched = %v, want %v: %v", got, tc.patches, h.gh.calls)
			}
		})
	}
}

// The trivial check runs before the approval follows the head: an App
// approval survives a comment-only push.
func TestTrivialPushKeepsTheAppApproval(t *testing.T) {
	h, app := newApprovalHarness(t)
	pr := reviewedThenPushed(t, h, "APPROVED", yamlComments)
	if pr.State != store.PRReviewed || deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("state %s event %q reviewed_sha %q", pr.State, deref(pr.LastReviewEvent), deref(pr.ReviewedSHA))
	}
	if n := len(callsWith(h.gh, "dismiss:")); n != 0 || len(app.comparesMade()) != 0 {
		t.Fatalf("dismissals %d, approval compares %v", n, app.comparesMade())
	}
	// Later polls leave it alone too.
	pollPR(h, time.Minute, 2, "b2")
	if n := len(callsWith(h.gh, "dismiss:")); n != 0 {
		t.Fatalf("dismissed on a later poll")
	}
}

// A forced `magnum review` always runs.
func TestForcedPRIsReviewedEvenWhenThePushIsTrivial(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	if err := h.st.UpdatePR(h.ctx, h.pr(2).ID, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": yamlComments}
	pollPR(h, time.Minute, 2, "b2") // a forced PR is due at once: the same tick runs its round
	if _, skipped := h.e.getKV(h.ctx, KVPRTrivial(h.pr(2).ID)); skipped {
		t.Fatal("a forced PR's push was skipped as trivial")
	}
	if ins := h.rd.all(); len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %+v, want the forced re-review of b2", ins)
	}
	if len(trivialEvents(t, h)) != 0 {
		t.Fatal("pr.trivial_delta recorded for a forced PR")
	}
}

// The live sequence: review 1 posts on a7b3f8c while the comment-only
// commit 602da9d already arrived during the judge's turn. The note says no
// re-review is needed, reviewed_sha moves to the head and nothing is queued.
func TestCommentOnlyCommitDuringTheReviewNeedsNoReReview(t *testing.T) {
	const reviewedHead, pushedHead = "a7b3f8c0000000000000000000000000000000aa", "602da9d0000000000000000000000000000000bb"
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: reviewedHead})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	// While the judge works on a7b3f8c, the poller sees 602da9d.
	h.advance(time.Minute)
	h.gh.compare[reviewedHead+"..."+pushedHead] = github.CompareStats{Commits: 1}
	h.gh.files = map[string][]github.FileDelta{reviewedHead + "..." + pushedHead: yamlComments}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: pushedHead})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRReviewing)
	close(h.rd.gate)
	h.settle()

	if got := h.rd.appended(); len(got) != 1 || got[0] != "101:_Reviewed a7b3f8c; 1 commit arrived during the review (comments only), no re-review needed._" {
		t.Fatalf("review notes = %q", got)
	}
	pr := h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != pushedHead || deref(pr.LastReviewID) != 101 {
		t.Fatalf("reviewed_sha %q review %v", deref(pr.ReviewedSHA), pr.LastReviewID)
	}
	if evs := trivialEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "during the review changes comments only") {
		t.Fatalf("pr.trivial_delta events: %+v", evs)
	}
	h.advance(time.Hour)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want no re-review", n)
	}
}

// A code commit during the review keeps today's note and re-review.
func TestCodeCommitDuringTheReviewKeepsTheReReview(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(time.Minute)
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": rubyMixed}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	close(h.rd.gate)
	h.settle()
	if got := h.rd.appended(); len(got) != 1 || got[0] != "101:_Reviewed b1; 1 commit arrived during the review, re-review follows._" {
		t.Fatalf("review notes = %q", got)
	}
	if pr := h.wantState(2, store.PRRereviewPending); deref(pr.ReviewedSHA) != "b1" {
		t.Fatalf("reviewed_sha %q", deref(pr.ReviewedSHA))
	}
}

// A later real review replaces the card's skip note.
func TestReviewOfTheHeadClearsTheTrivialNote(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Daemon.RereviewMinLines = 0 }) // rubyMixed is a small delta
	pr := reviewedThenPushed(t, h, "", yamlComments)
	if _, ok := kvValue(h, KVPRTrivial(pr.ID)); !ok {
		t.Fatal("no note after the trivial push")
	}
	h.gh.files["b2...b3"] = rubyMixed
	pollPR(h, time.Minute, 2, "b3")
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b3"})
	h.tick()
	if pr := h.wantState(2, store.PRReviewed); deref(pr.ReviewedSHA) != "b3" {
		t.Fatalf("reviewed_sha %q", deref(pr.ReviewedSHA))
	}
	if v, ok := kvValue(h, KVPRTrivial(pr.ID)); ok {
		t.Fatalf("note %q kept after the head was reviewed", v)
	}
}

func TestTrivialSkipNote(t *testing.T) {
	for classes, want := range map[string]string{
		"comments":            "comment-only push skipped (a7b3f8c → 602da9d)",
		"whitespace":          "whitespace-only push skipped (a7b3f8c → 602da9d)",
		"docs":                "docs-only push skipped (a7b3f8c → 602da9d)",
		"comments,whitespace": "trivial push skipped (a7b3f8c → 602da9d)",
	} {
		s := TrivialSkip{From: "a7b3f8c0000", To: "602da9d1111", Classes: strings.Split(classes, ",")}
		if got := s.Note(); got != want {
			t.Errorf("%s: %q, want %q", classes, got, want)
		}
	}
	if _, ok := ParseTrivialSkip("{"); ok {
		t.Error("ParseTrivialSkip accepted broken JSON")
	}
}
