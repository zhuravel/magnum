package learn

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

var (
	t0      = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	shaA    = strings.Repeat("a", 40) // magnum's first review
	shaB    = strings.Repeat("b", 40) // a push magnum did not review
	shaC    = strings.Repeat("c", 40) // magnum's second review
	longish = "This drops the tenant scope, so a coupon from another site applies here."
)

// thread is a review thread whose root comment rev-ann made on commit sha
// at line, an hour after t0.
func thread(id int64, path string, line int, sha, body string) github.Thread {
	return github.Thread{Path: path, Line: line, OriginalLine: line, Comments: []github.ThreadComment{{
		ID: id, AuthorLogin: "rev-ann", AuthorType: "User", Body: body, CreatedAt: t0.Add(time.Hour),
		URL: "https://github.com/talkable/widgets/pull/7#discussion_r" + strconv.FormatInt(id, 10), OriginalCommitOid: sha, DiffHunk: "@@ -1 +1 @@",
	}}}
}

func input(threads []github.Thread, reviews []github.Review) Input {
	return Input{
		Author:   "alice",
		Reviewed: []Reviewed{{SHA: shaA, At: t0}},
		Own:      []string{"talkable[bot]", "zhuravel"},
		Threads:  threads, Reviews: reviews, MinChars: 20,
	}
}

func ids(cs []Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

// TestBuildKeepsOtherReviewersThreadRootsOnReviewedCommits: the root of a
// thread another reviewer started on a commit magnum reviewed is a
// candidate with that commit, its lines, hunk and body; replies never are.
func TestBuildKeepsOtherReviewersThreadRootsOnReviewedCommits(t *testing.T) {
	th := thread(11, "app/models/coupon.rb", 42, shaA, longish)
	th.OriginalStartLine = 40
	th.Comments = append(th.Comments, github.ThreadComment{ID: 12, AuthorLogin: "bob-rev", AuthorType: "User",
		Body: "Agreed, and the same goes for the gift cards below.", URL: "https://example.com/r12"})
	res, err := Build(input([]github.Thread{th}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Candidates); !slices.Equal(got, []string{"t11"}) {
		t.Fatalf("candidates = %v, want [t11] (replies are never candidates)", got)
	}
	c := res.Candidates[0]
	if c.Kind != KindThread || c.ReviewedSHA != shaA || c.CommentSHA != shaA || c.Path != "app/models/coupon.rb" ||
		c.StartLine != 40 || c.Line != 42 || c.DiffHunk == "" || c.Body != longish || c.Reviewer != "rev-ann" ||
		c.Raised != store.MissRaisedNone || !slices.Equal(c.Lines(), []int{40, 42}) {
		t.Fatalf("candidate = %+v", c)
	}
}

// TestBuildSkipsTheAuthorMagnumAndBots: the author's and magnum's own
// comments (any configured or former login, Account form) are never
// candidates; another bot's only with IncludeBots.
func TestBuildSkipsTheAuthorMagnumAndBots(t *testing.T) {
	mk := func(id int64, login, typ string) github.Thread {
		th := thread(id, "a.rb", 1, shaA, longish)
		th.Comments[0].AuthorLogin, th.Comments[0].AuthorType = login, typ
		return th
	}
	threads := []github.Thread{
		mk(1, "alice", "User"),       // the author
		mk(2, "talkable", "Bot"),     // magnum's App ("talkable[bot]")
		mk(3, "ZHURAVEL", "User"),    // magnum's user identity, any case
		mk(4, "lint-bot", "Bot"),     // another bot
		mk(5, "", ""),                // a ghost
		mk(6, "talkable", "User"),    // a user named like the App: another account
		mk(7, "rev-ann", "User"),     // a reviewer
		mk(8, "dependabot", "Bot"),   // another bot
		mk(9, "copilot[bot]", "Bot"), // a bot already in Account form
	}
	res, err := Build(input(threads, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Candidates); !slices.Equal(got, []string{"t6", "t7"}) {
		t.Fatalf("candidates = %v, want [t6 t7]", got)
	}
	in := input(threads, nil)
	in.IncludeBots = true
	if res, _ = Build(in); !slices.Equal(ids(res.Candidates), []string{"t4", "t6", "t7", "t8", "t9"}) {
		t.Fatalf("with include_bots: %v", ids(res.Candidates))
	}
	if res.Candidates[0].Reviewer != "lint-bot[bot]" {
		t.Fatalf("a bot reviewer is stored in Account form: %q", res.Candidates[0].Reviewer)
	}
}

// TestBuildDropsShortCommentsAndApprovals: what is left of a comment
// without quotes and code blocks must reach MinChars, and a comment that
// only approves teaches nothing.
func TestBuildDropsShortCommentsAndApprovals(t *testing.T) {
	bodies := map[int64]string{
		1: "nit: rename",
		2: "> " + longish + "\n\nTrue.",
		3: "```suggestion\n" + longish + "\n```",
		4: "LGTM, looks good to me! Thanks a lot 👍",
		5: "~~~\n" + longish + "\n~~~\nsee above",
		6: longish,
		7: "Looks good, but this drops the tenant scope on coupons.",
	}
	var threads []github.Thread
	for id := int64(1); id <= 7; id++ {
		threads = append(threads, thread(id, "a.rb", int(id)*20, shaA, bodies[id]))
	}
	res, err := Build(input(threads, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Candidates); !slices.Equal(got, []string{"t6", "t7"}) || res.Dropped != 5 {
		t.Fatalf("candidates = %v, dropped %d; want [t6 t7], 5", got, res.Dropped)
	}
}

// TestBuildMovesCommentsToTheReviewedCommitWhenTheFileIsUnchanged: a
// comment on a later commit applies to the newest commit magnum reviewed
// before it only when GitHub's comparison shows the file unchanged; when it
// changed, or the comparison is cut off, the comment is outside and never
// classified.
func TestBuildMovesCommentsToTheReviewedCommitWhenTheFileIsUnchanged(t *testing.T) {
	threads := []github.Thread{
		thread(1, "same.rb", 5, shaB, longish),
		thread(2, "moved.rb", 5, shaB, longish),
	}
	var asked [][2]string
	in := input(threads, nil)
	in.Compare = func(from, to string) (Comparison, error) {
		asked = append(asked, [2]string{from, to})
		return Comparison{Descendant: true, Paths: []string{"moved.rb"}, Complete: true}, nil
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(res.Candidates), []string{"t1"}) || !slices.Equal(ids(res.Outside), []string{"t2"}) {
		t.Fatalf("candidates %v, outside %v", ids(res.Candidates), ids(res.Outside))
	}
	if c := res.Candidates[0]; c.ReviewedSHA != shaA || c.CommentSHA != shaB || c.Line != 5 {
		t.Fatalf("moved candidate = %+v", c)
	}
	if len(asked) != 1 || asked[0] != [2]string{shaA, shaB} {
		t.Fatalf("comparisons = %v, want one of A...B", asked)
	}

	in.Compare = func(string, string) (Comparison, error) {
		return Comparison{Descendant: true, Paths: []string{"moved.rb"}}, nil
	}
	if res, _ = Build(in); len(res.Candidates) != 0 || len(res.Outside) != 2 {
		t.Fatalf("an incomplete comparison proves nothing: candidates %v", ids(res.Candidates))
	}
	in.Compare = func(string, string) (Comparison, error) { return Comparison{}, errors.New("HTTP 502") }
	if _, err := Build(in); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a failing comparison: %v", err)
	}
	// Only a review magnum posted before the comment counts.
	in = input([]github.Thread{thread(3, "same.rb", 5, shaB, longish)}, nil)
	in.Reviewed = []Reviewed{{SHA: shaA, At: t0.Add(2 * time.Hour)}}
	in.Compare = func(string, string) (Comparison, error) { t.Fatal("compared"); return Comparison{}, nil }
	if res, _ = Build(in); !slices.Equal(ids(res.Outside), []string{"t3"}) {
		t.Fatalf("a comment before any review of magnum: outside %v", ids(res.Outside))
	}
}

// TestBuildPutsACommentOutsideWhenGitHubCannotCompare: GitHub answers 404
// for a comparison whose commit is gone (a force push, a deleted branch):
// the comment on it is outside, as for a comparison that proves nothing,
// the comparison is asked once, and the rest of the pull request is built.
func TestBuildPutsACommentOutsideWhenGitHubCannotCompare(t *testing.T) {
	threads := []github.Thread{
		thread(1, "a.rb", 5, shaB, longish),
		thread(2, "b.rb", 5, shaB, longish),
		thread(3, "c.rb", 5, shaA, longish),
	}
	asked := 0
	in := input(threads, nil)
	in.Compare = func(string, string) (Comparison, error) {
		asked++
		return Comparison{}, &github.APIError{Op: "compare", Status: 404, Message: "Not Found"}
	}
	res, err := Build(in)
	if err != nil {
		t.Fatalf("a comparison GitHub cannot make failed the build: %v", err)
	}
	if !slices.Equal(ids(res.Candidates), []string{"t3"}) || !slices.Equal(ids(res.Outside), []string{"t1", "t2"}) {
		t.Fatalf("candidates %v, outside %v", ids(res.Candidates), ids(res.Outside))
	}
	if asked != 1 {
		t.Errorf("comparisons asked = %d, want 1", asked)
	}
}

// TestBuildPicksTheNewestReviewBeforeTheComment: of the reviewed commits,
// the comparison starts from the newest one magnum posted before the
// comment.
func TestBuildPicksTheNewestReviewBeforeTheComment(t *testing.T) {
	in := input([]github.Thread{thread(1, "a.rb", 5, shaB, longish)}, nil)
	in.Reviewed = []Reviewed{{SHA: shaC, At: t0.Add(30 * time.Minute)}, {SHA: shaA, At: t0}, {SHA: "late", At: t0.Add(3 * time.Hour)}}
	in.Compare = func(from, to string) (Comparison, error) {
		if from != shaC {
			t.Fatalf("compared from %s, want the newest review before the comment", from)
		}
		return Comparison{Descendant: true, Complete: true}, nil
	}
	res, err := Build(in)
	if err != nil || len(res.Candidates) != 1 || res.Candidates[0].ReviewedSHA != shaC {
		t.Fatalf("Build = %+v, %v", res, err)
	}
}

// TestBuildDropsCaughtAndMarksRejectedFindings: a comment within NearLines
// of a finding magnum posted on the same path was caught and is not kept;
// one near a finding the judge rejected is kept as raised, with the
// finding's reference and reason.
func TestBuildDropsCaughtAndMarksRejectedFindings(t *testing.T) {
	in := input([]github.Thread{
		thread(1, "a.rb", 45, shaA, longish), // 3 lines from the posted finding
		thread(2, "a.rb", 60, shaA, longish), // 1 line from the rejected one
		thread(3, "b.rb", 42, shaA, longish), // another path
		thread(4, "a.rb", 49, shaA, longish), // 7 lines away from both
	}, nil)
	in.Findings = []store.Finding{
		{RunID: "r-1", FindingID: "F1", Path: "a.rb", Line: 42, Verdict: store.FindingPosted},
		{RunID: "r-1", FindingID: "F2", Path: "a.rb", Line: 61, Verdict: store.FindingRejected, ReasonCode: "not_reproducible"},
		{RunID: "r-1", FindingID: "F3", Path: "a.rb", Line: 0, Verdict: store.FindingPosted}, // a body finding
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Caught != 1 || !slices.Equal(ids(res.Candidates), []string{"t2", "t3", "t4"}) {
		t.Fatalf("caught %d, candidates %v", res.Caught, ids(res.Candidates))
	}
	if c := res.Candidates[0]; c.Raised != store.MissRaisedRejected || c.FindingRef != "r-1/F2" || c.ReasonCode != "not_reproducible" {
		t.Fatalf("raised candidate = %+v", c)
	}
	if c := res.Candidates[2]; c.Raised != store.MissRaisedNone || c.FindingRef != "" {
		t.Fatalf("far candidate = %+v", c)
	}
}

// TestBuildKeepsReviewBodiesOnReviewedCommits: a review body (commented,
// changes requested, or an approval that says something) by another
// reviewer is a candidate without a path when its commit was reviewed, and
// outside otherwise; pending and dismissed reviews and empty bodies are
// not.
func TestBuildKeepsReviewBodiesOnReviewedCommits(t *testing.T) {
	rv := func(id int64, state, sha, body string) github.Review {
		return github.Review{DatabaseID: id, State: state, Body: body, CommitOid: sha, AuthorLogin: "rev-ann", AuthorType: "User",
			SubmittedAt: t0.Add(time.Hour), URL: "https://github.com/talkable/widgets/pull/7#pullrequestreview-" + strconv.FormatInt(id, 10)}
	}
	res, err := Build(input(nil, []github.Review{
		rv(1, "COMMENTED", shaA, longish),
		rv(2, "CHANGES_REQUESTED", shaA, longish),
		rv(3, "APPROVED", shaA, longish),
		rv(4, "APPROVED", shaA, ""),
		rv(5, "DISMISSED", shaA, longish),
		rv(6, "PENDING", shaA, longish),
		rv(7, "COMMENTED", shaB, longish),
		rv(8, "APPROVED", shaA, "LGTM, thanks a lot for the fix!"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(res.Candidates), []string{"r1", "r2", "r3"}) || !slices.Equal(ids(res.Outside), []string{"r7"}) || res.Dropped != 1 {
		t.Fatalf("candidates %v, outside %v, dropped %d", ids(res.Candidates), ids(res.Outside), res.Dropped)
	}
	if c := res.Candidates[0]; c.Kind != KindReview || c.Path != "" || c.Line != 0 || c.ReviewedSHA != shaA || c.Lines() != nil {
		t.Fatalf("review candidate = %+v", c)
	}
	if o := res.Outside[0]; o.ReviewedSHA != shaA || o.CommentSHA != shaB {
		t.Fatalf("outside review = %+v", o)
	}
}

// TestBuildCountsLaterCommentsOnlyOnDescendants: GitHub's comparison is
// three-dot, so a commit older than the reviewed one, or one on a history
// that was force-pushed away, lists no files; only a comment whose commit
// descends from the reviewed one, with its file unlisted, moves there.
func TestBuildCountsLaterCommentsOnlyOnDescendants(t *testing.T) {
	for name, tc := range map[string]struct {
		cmp  Comparison
		kept bool
	}{
		"descendant, file unchanged": {Comparison{Descendant: true, Complete: true}, true},
		"ancestor":                   {Comparison{Complete: true}, false}, // GitHub: behind
		"diverged (force push)":      {Comparison{Complete: true}, false},
		"descendant, file changed":   {Comparison{Descendant: true, Complete: true, Paths: []string{"same.rb"}}, false},
	} {
		in := input([]github.Thread{thread(1, "same.rb", 5, shaB, longish)}, nil)
		in.Compare = func(string, string) (Comparison, error) { return tc.cmp, nil }
		res, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if kept := len(res.Candidates) == 1; kept != tc.kept || len(res.Candidates)+len(res.Outside) != 1 {
			t.Errorf("%s: candidates %v, outside %v; want kept = %v", name, ids(res.Candidates), ids(res.Outside), tc.kept)
		}
	}
}

// TestBuildDropsTheLinesOfDeletedLineComments: a comment on the left side
// of the diff (deleted lines) is numbered in the old file, so it keeps its
// path and hunk but no line, and is never near a finding.
func TestBuildDropsTheLinesOfDeletedLineComments(t *testing.T) {
	th := thread(1, "a.rb", 42, shaA, longish)
	th.DiffSide, th.OriginalStartLine = "LEFT", 40
	in := input([]github.Thread{th}, nil)
	in.Findings = []store.Finding{{RunID: "r-1", FindingID: "F1", Path: "a.rb", Line: 42, Verdict: store.FindingPosted}}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Caught != 0 || len(res.Candidates) != 1 {
		t.Fatalf("caught %d, candidates %v", res.Caught, ids(res.Candidates))
	}
	if c := res.Candidates[0]; c.Line != 0 || c.StartLine != 0 || c.Lines() != nil || c.Path != "a.rb" || c.DiffHunk == "" || c.Side != "LEFT" {
		t.Fatalf("candidate = %+v", c)
	}
}
