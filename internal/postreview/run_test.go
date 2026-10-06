package postreview

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
)

// A review whose comments sit on added, context and deleted (LEFT) lines,
// one of them multi-line within a hunk, is posted once on the head with
// all its comments, the JSON on gh's stdin and none of the review's text in
// argv, and read back.
func TestRunPostsOnceWithTheJSONOnStdin(t *testing.T) {
	w := newWorld(t)
	file := review("REQUEST_CHANGES", "Blocking: 1 problem must be fixed before merging.",
		comment("app/x.rb", 13, ""),                                     // added
		comment("app/x.rb", 10, ""),                                     // context, RIGHT
		comment("app/x.rb", 14, `"side":"LEFT"`),                        // deleted
		comment("app/x.rb", 39, `"side":"LEFT"`),                        // context, LEFT
		comment("app/x.rb", 45, `"start_line":40,"start_side":"RIGHT"`), // one hunk
	)
	out := w.run(opts(), file)
	if out.Status != StatusPosted || out.ExitCode() != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if out.ReviewID != 900 || out.Event != "REQUEST_CHANGES" || out.State != "CHANGES_REQUESTED" || out.CommitID != head ||
		out.Comments == nil || *out.Comments != 5 || !out.MarkerAppended || out.EventDowngraded {
		t.Errorf("outcome = %+v", out)
	}
	if w.postCount() != 1 || len(w.sent) != 1 {
		t.Fatalf("%d POSTs", w.postCount())
	}
	sent := w.sent[0]
	if sent.CommitID != head || len(sent.Comments) != 5 || !strings.HasSuffix(sent.Body, RunMarker(runID, head)) {
		t.Errorf("sent = %+v", sent)
	}
	if c := sent.Comments[4]; c.Side != "RIGHT" || c.StartLine != 40 || c.StartSide != "RIGHT" {
		t.Errorf("multi-line comment = %+v", c)
	}
	post := w.fake.CallsWithPrefix("gh", "api", "-X", "POST")[0]
	if !post.Mutates || post.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" || !slices.Contains(post.Args, "--input") {
		t.Errorf("POST = %+v", post)
	}
	for _, a := range post.Args {
		if strings.Contains(a, "Blocking") || strings.Contains(a, "wrong total") || strings.Contains(a, "magnum:run") {
			t.Errorf("review text in argv: %q", post.Args)
		}
	}
	if w.calls("git") != 0 {
		t.Errorf("git ran for files with patches: %d calls", w.calls("git"))
	}
}

// An anchor off the diff stops everything before any write: each bad
// comment is listed with why and the valid ranges of its file on its side.
func TestRunListsInvalidAnchorsAndPostsNothing(t *testing.T) {
	w := newWorld(t)
	file := review("COMMENT", "x",
		comment("app/x.rb", 12, ""),                // fine
		comment("app/x.rb", 20, ""),                // between the hunks
		comment("app/x.rb", 44, `"side":"LEFT"`),   // past the LEFT hunk
		comment("app/x.rb", 42, `"start_line":12`), // across two hunks
		comment("spec/x_spec.rb", 3, ""),           // not in the PR
	)
	out := w.run(opts(), file)
	if out.Status != StatusInvalidAnchors || out.ExitCode() != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	want := []BadAnchor{
		{Index: 1, Path: "app/x.rb", Line: 20, Side: "RIGHT", Why: "line 20 is not in the diff of app/x.rb on RIGHT", Valid: []string{"10-16", "40-45"}},
		{Index: 2, Path: "app/x.rb", Line: 44, Side: "LEFT", Why: "line 44 is not in the diff of app/x.rb on LEFT", Valid: []string{"10-15", "39-43"}},
		{Index: 3, Path: "app/x.rb", Line: 42, StartLine: 12, Side: "RIGHT",
			Why: "start_line 12 and line 42 are in different hunks: a comment's lines must be in one hunk", Valid: []string{"10-16", "40-45"}},
		{Index: 4, Path: "spec/x_spec.rb", Line: 3, Side: "RIGHT", Why: "the pull request does not change spec/x_spec.rb", Valid: []string{}},
	}
	if !slices.EqualFunc(out.InvalidAnchors, want, func(a, b BadAnchor) bool {
		return a.Index == b.Index && a.Path == b.Path && a.Line == b.Line && a.StartLine == b.StartLine && a.Side == b.Side &&
			a.Why == b.Why && slices.Equal(a.Valid, b.Valid)
	}) {
		t.Errorf("invalid anchors =\n%+v\nwant\n%+v", out.InvalidAnchors, want)
	}
	if w.postCount() != 0 || w.calls("gh", "api", "graphql") != 0 {
		t.Errorf("posted or listed reviews after invalid anchors: %d POSTs", w.postCount())
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"valid":[]`) {
		t.Errorf("a file outside the PR lists no ranges as []: %s", b)
	}
}

// A file GitHub lists without a patch (too large) is checked against git
// diff from the PR's merge base in the checkout.
func TestRunChecksAPatchlessFileWithGitDiff(t *testing.T) {
	w := newWorld(t)
	w.files = append(w.files, prFile{Filename: "db/structure.sql", Status: "modified"})
	w.gitDiffs["db/structure.sql"] = "diff --git a/db/structure.sql b/db/structure.sql\n--- a/db/structure.sql\n+++ b/db/structure.sql\n" +
		"@@ -100,3 +100,4 @@\n a\n+b\n c\n d"
	out := w.run(opts(), review("COMMENT", "x", comment("db/structure.sql", 101, ""), comment("db/structure.sql", 200, "")))
	if out.Status != StatusInvalidAnchors || len(out.InvalidAnchors) != 1 || out.InvalidAnchors[0].Index != 1 ||
		!slices.Equal(out.InvalidAnchors[0].Valid, []string{"100-103"}) {
		t.Fatalf("outcome = %+v", out)
	}
	diff := w.fake.CallsWithPrefix("git", "-C", ".", "diff")
	if len(diff) != 1 {
		t.Fatalf("%d git diffs", len(diff))
	}
	if args := diff[0].Args; !slices.Contains(args, mergeSHA) || !slices.Contains(args, head) || args[len(args)-1] != ":(top,literal)db/structure.sql" {
		t.Errorf("git diff args = %q", args)
	}
	if w.calls("git", "-C", ".", "merge-base") != 1 {
		t.Error("no merge base of base.sha and the head")
	}
}

// An anchor neither GitHub's patch nor the checkout can check (the base
// commit is not in the checkout) is kept: GitHub decides.
func TestRunKeepsAnAnchorNothingCanCheck(t *testing.T) {
	w := newWorld(t)
	w.files = append(w.files, prFile{Filename: "assets/logo.svg", Status: "modified"})
	w.missing[base] = true
	out := w.run(opts(), review("COMMENT", "x", comment("app/x.rb", 12, ""), comment("assets/logo.svg", 7, "")))
	if out.Status != StatusPosted || !slices.Equal(out.Unchecked, []int{1}) || len(w.sent) != 1 || len(w.sent[0].Comments) != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	if w.calls("git", "-C", ".", "diff") != 0 {
		t.Error("git diff ran without the base commit")
	}
}

// A binary file's git diff has no hunks: kept, GitHub decides.
func TestRunKeepsAnAnchorOnABinaryFile(t *testing.T) {
	w := newWorld(t)
	w.files = append(w.files, prFile{Filename: "logo.png", Status: "modified"})
	w.gitDiffs["logo.png"] = "diff --git a/logo.png b/logo.png\nBinary files a/logo.png and b/logo.png differ\n"
	out := w.run(opts(), review("COMMENT", "x", comment("logo.png", 1, "")))
	if out.Status != StatusPosted || !slices.Equal(out.Unchecked, []int{0}) {
		t.Fatalf("outcome = %+v", out)
	}
}

// A review by the reviewer login already carrying the run's marker is the
// review: nothing is posted, the existing one is read back.
func TestRunAlreadyPostedShortCircuitsThePost(t *testing.T) {
	w := newWorld(t)
	w.nodes = []node{
		{ID: 700, Body: "older\n<!-- magnum:run=r-20261006T120000-6 head=d4e5f6a -->", Author: "talkable", Bot: true},
		{ID: 701, Body: "copied marker\n<!-- magnum:run=" + runID + " head=d4e5f6a -->", Author: "alice"},
		{ID: 702, State: "PENDING", Body: "pending\n<!-- magnum:run=" + runID + " head=d4e5f6a -->", Author: "talkable", Bot: true},
		{ID: 703, State: "CHANGES_REQUESTED", Body: "Blocking.\n<!-- magnum:run=" + runID + " head=d4e5f6a -->", Author: "talkable", Bot: true},
	}
	w.readComments = 2
	out := w.run(opts(), review("REQUEST_CHANGES", "Blocking: 1 problem.", comment("app/x.rb", 12, "")))
	if out.Status != StatusAlreadyPosted || out.ExitCode() != 0 || out.ReviewID != 703 || out.Event != "REQUEST_CHANGES" ||
		out.State != "CHANGES_REQUESTED" || *out.Comments != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	if w.postCount() != 0 {
		t.Fatalf("%d POSTs after finding the marker", w.postCount())
	}
	if !strings.Contains(out.ReviewURL, "pullrequestreview-703") {
		t.Errorf("url = %s", out.ReviewURL)
	}
}

// A former login's review carrying the marker counts as posted.
func TestRunAlreadyPostedByAFormerLogin(t *testing.T) {
	w := newWorld(t)
	w.nodes = []node{{ID: 710, Body: "x\n<!-- magnum:run=" + runID + " head=d4e5f6a -->", Author: "zhuravel"}}
	w.readUser = "zhuravel"
	o := opts()
	o.FormerLogins = []string{"zhuravel"}
	if out := w.run(o, review("COMMENT", "x")); out.Status != StatusAlreadyPosted || out.ReviewID != 710 {
		t.Fatalf("outcome = %+v", out)
	}
	// Without the former login it is someone else's review: posted anew.
	w2 := newWorld(t)
	w2.nodes = w.nodes
	if out := w2.run(opts(), review("COMMENT", "x")); out.Status != StatusPosted || w2.postCount() != 1 {
		t.Fatalf("outcome = %+v", out)
	}
}

// A dry run checks everything and prints the request it would send; it
// posts nothing and reads nothing back.
func TestRunDryRunPostsNothing(t *testing.T) {
	w := newWorld(t)
	o := opts()
	o.DryRun = true
	out := w.run(o, review("APPROVE", "No problems found. LGTM :shipit:", comment("app/x.rb", 12, "")))
	if out.Status != StatusDryRun || out.ExitCode() != 0 || out.PlannedReview == nil {
		t.Fatalf("outcome = %+v", out)
	}
	p := out.PlannedReview
	if p.CommitID != head || p.Event != "APPROVE" || len(p.Comments) != 1 || !strings.HasSuffix(p.Body, RunMarker(runID, head)) {
		t.Errorf("planned = %+v", p)
	}
	if w.postCount() != 0 || w.calls("gh", "api", "repos/talkable/talkable/pulls/5/reviews/900") != 0 {
		t.Error("a dry run wrote or read back")
	}
	b, _ := json.Marshal(out)
	for _, key := range []string{`"planned_review":{"commit_id":"` + head, `"comments":[{"path":"app/x.rb","line":12,"side":"RIGHT"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON lacks %s: %s", key, b)
		}
	}
}

// A blind replay (--local-base with a dry run) checks the anchors against
// the checkout's diff of the pinned range and calls GitHub not at all.
func TestRunLocalBaseReadsNothingFromGitHub(t *testing.T) {
	w := newWorld(t)
	w.gitDiffs["app/x.rb"] = appPatch
	o := opts()
	o.DryRun, o.LocalBase = true, base
	out := w.run(o, review("COMMENT", "x", comment("app/x.rb", 12, ""), comment("app/x.rb", 30, ""), comment("lib/y.rb", 1, "")))
	if out.Status != StatusInvalidAnchors || len(out.InvalidAnchors) != 2 ||
		out.InvalidAnchors[1].Why != "the pull request does not change lib/y.rb" {
		t.Fatalf("outcome = %+v", out)
	}
	if w.calls("gh") != 0 {
		t.Errorf("%d gh calls in a blind replay", w.calls("gh"))
	}
	if args := w.fake.CallsWithPrefix("git", "-C", ".", "diff")[0].Args; !slices.Contains(args, base) {
		t.Errorf("git diff args = %q", args)
	}
	ok := w.run(o, review("COMMENT", "x", comment("app/x.rb", 12, "")))
	if ok.Status != StatusDryRun || w.calls("gh") != 0 {
		t.Fatalf("outcome = %+v", ok)
	}
	o.DryRun = false
	if out := w.run(o, review("COMMENT", "x")); out.Status != StatusError || !strings.Contains(out.Message, "dry run only") {
		t.Errorf("--local-base without --dry-run: %+v", out)
	}
}

// When the PR moved on while the judge worked, the anchors are checked
// against the comparison of its base with the reviewed head, not the PR's
// newer diff.
func TestRunChecksAMovedPRAgainstTheReviewedHead(t *testing.T) {
	w := newWorld(t)
	w.prHead = "ffffffffffffffffffffffffffffffffffffffff"
	w.files = nil // the newer diff would not have app/x.rb
	w.compareFiles = []prFile{{Filename: "app/x.rb", Status: "modified", Patch: patch(appPatch)}}
	out := w.run(opts(), review("COMMENT", "x", comment("app/x.rb", 12, "")))
	if out.Status != StatusPosted {
		t.Fatalf("outcome = %+v", out)
	}
	if w.calls("gh", "api", "repos/talkable/talkable/compare/"+base+"..."+head+"?per_page=1") != 1 {
		t.Errorf("no comparison of base...head: %v", w.fake.Calls)
	}
}

// GitHub refusing a verdict on the identity's own PR: the review goes out
// once more as COMMENT, and the outcome says so.
func TestRunRetriesASelfVerdictOnceAsComment(t *testing.T) {
	w := newWorld(t)
	w.posts = []func(execx.Cmd) (execx.Result, error){func(c execx.Cmd) (execx.Result, error) {
		return apiFail(c, 422, "Unprocessable Entity", "Review Can not approve your own pull request")
	}}
	out := w.run(opts(), review("APPROVE", "No problems found. LGTM :shipit:"))
	if out.Status != StatusPosted || !out.EventDowngraded || out.Event != "COMMENT" || out.State != "COMMENTED" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(w.sent) != 2 || w.sent[0].Event != "APPROVE" || w.sent[1].Event != "COMMENT" || w.sent[1].Body != w.sent[0].Body {
		t.Fatalf("sent = %+v", w.sent)
	}
	if w.calls("gh", "api", "graphql") != 1 {
		t.Errorf("the refusal needs no marker check: %d lists", w.calls("gh", "api", "graphql"))
	}
	// A second refusal is not retried again.
	w2 := newWorld(t)
	refuse := func(c execx.Cmd) (execx.Result, error) {
		return apiFail(c, 422, "Unprocessable Entity", "Can not request changes on your own pull request")
	}
	w2.posts = []func(execx.Cmd) (execx.Result, error){refuse, refuse}
	if out := w2.run(opts(), review("REQUEST_CHANGES", "Blocking.")); out.Status != StatusRejected || len(w2.sent) != 2 {
		t.Fatalf("outcome = %+v after %d POSTs", out, len(w2.sent))
	}
}

// Any other 4xx is reported as rejected with GitHub's message, after a
// second look finds no review carrying the marker; it is never retried.
func TestRunReportsARefusedReviewWithoutRetrying(t *testing.T) {
	w := newWorld(t)
	w.posts = []func(execx.Cmd) (execx.Result, error){func(c execx.Cmd) (execx.Result, error) {
		return apiFail(c, 422, "Unprocessable Entity", "Pull request review thread line must be part of the diff")
	}}
	out := w.run(opts(), review("COMMENT", "x", comment("app/x.rb", 12, "")))
	if out.Status != StatusRejected || out.ExitCode() != 1 || !strings.Contains(out.Message, "must be part of the diff") {
		t.Fatalf("outcome = %+v", out)
	}
	if w.postCount() != 1 || w.calls("gh", "api", "graphql") != 2 {
		t.Errorf("%d POSTs, %d lists; want 1 and 2 (before, and to confirm)", w.postCount(), w.calls("gh", "api", "graphql"))
	}
}

// A failure whose outcome is unclear (a 502, a dropped connection) is
// settled by the marker: a review carrying it is the posted review;
// without one the outcome is error, and nothing is posted twice.
func TestRunSettlesAnUnclearFailureByTheMarker(t *testing.T) {
	w := newWorld(t)
	w.posts = []func(execx.Cmd) (execx.Result, error){func(c execx.Cmd) (execx.Result, error) {
		// GitHub created the review, then the answer was lost.
		w.nodes = append(w.nodes, node{ID: 905, Body: w.sent[0].Body, Author: "talkable", Bot: true})
		return apiFail(c, 502, "Bad Gateway")
	}}
	out := w.run(opts(), review("COMMENT", "x", comment("app/x.rb", 12, "")))
	if out.Status != StatusPosted || out.ReviewID != 905 || w.postCount() != 1 || *out.Comments != 1 {
		t.Fatalf("outcome = %+v after %d POSTs", out, w.postCount())
	}

	w2 := newWorld(t)
	w2.posts = []func(execx.Cmd) (execx.Result, error){func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, &execx.RunError{Cmd: c, Err: errors.New("connection reset by peer")}
	}}
	out = w2.run(opts(), review("COMMENT", "x"))
	if out.Status != StatusError || out.ExitCode() != 1 || w2.postCount() != 1 || !strings.Contains(out.Message, "no review carries the run marker") {
		t.Fatalf("outcome = %+v after %d POSTs", out, w2.postCount())
	}
}

// The read-back must show the review as sent: the reviewer login, the head,
// the event's state, the marker and every comment.
func TestRunReportsReadBackMismatches(t *testing.T) {
	cases := []struct {
		name string
		set  func(w *world)
		want string
	}{
		{"login", func(w *world) { w.readUser = "alice" }, `its author is "alice", not "talkable[bot]"`},
		{"commit", func(w *world) { w.readCommit = base }, "its commit is \"" + base + "\", not the head"},
		{"comment count", func(w *world) { w.readComments = 1 }, "it has 1 inline comments, 2 were sent"},
		{"state", func(w *world) { w.readState = "COMMENTED" }, "its state is COMMENTED, not CHANGES_REQUESTED"},
		{"marker", func(w *world) { w.readBody = "edited" }, "its body lacks the run marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.set(w)
			out := w.run(opts(), review("REQUEST_CHANGES", "Blocking.", comment("app/x.rb", 12, ""), comment("app/x.rb", 13, "")))
			if out.Status != StatusReadbackMismatch || out.ExitCode() != 1 || out.ReviewID != 900 ||
				!strings.Contains(strings.Join(out.Mismatches, "; "), tc.want) {
				t.Fatalf("outcome = %+v, want a mismatch %q", out, tc.want)
			}
		})
	}
}

// The run's facts are checked before anything runs.
func TestRunRefusesBadOptions(t *testing.T) {
	for name, set := range map[string]func(o *Options){
		"short head":   func(o *Options) { o.HeadSHA = "d4e5f6a" },
		"no login":     func(o *Options) { o.Login = "" },
		"no run id":    func(o *Options) { o.RunID = "" },
		"bad owner":    func(o *Options) { o.Owner = "-x" },
		"no PR number": func(o *Options) { o.Number = 0 },
	} {
		o := opts()
		set(&o)
		w := newWorld(t)
		if out := w.run(o, review("COMMENT", "x")); out.Status != StatusError || out.ExitCode() != 1 || len(w.fake.Calls) != 0 {
			t.Errorf("%s: %+v", name, out)
		}
	}
}

// selfVerdict reads GitHub's error details, not only its message.
func TestSelfVerdictNeedsA422AboutTheOwnPR(t *testing.T) {
	if !selfVerdict(&github.APIError{Status: 422, Message: "Unprocessable Entity", Details: []string{"Can not approve your own pull request"}}) {
		t.Error("approve refusal not recognised")
	}
	if selfVerdict(&github.APIError{Status: 422, Message: "Unprocessable Entity", Details: []string{"line must be part of the diff"}}) ||
		selfVerdict(&github.APIError{Status: 403, Message: "Can not approve your own pull request"}) {
		t.Error("another refusal taken for a self-verdict")
	}
}
