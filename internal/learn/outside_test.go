package learn

import (
	"strconv"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// outsideOf is the one outside comment of res, failing the test unless
// there is exactly one and nothing else.
func outsideOf(t *testing.T, name string, res Result) Candidate {
	t.Helper()
	if len(res.Outside) != 1 || len(res.Candidates) != 0 {
		t.Fatalf("%s: outside %v, candidates %v; want one outside comment", name, ids(res.Outside), ids(res.Candidates))
	}
	return res.Outside[0]
}

// TestBuildSaysWhyAThreadIsOutside: an inline comment Build stores outside
// says why, so a later count needs no GitHub call: made before magnum's
// first review (no comparison asked), on a later commit whose file changed
// since the review before it, on a commit that does not descend from that
// review's, or on one GitHub cannot compare it with (a 404, a cut file
// list, no comparison at all). Each keeps the commit it was made on, and the
// reviewed commit before it.
func TestBuildSaysWhyAThreadIsOutside(t *testing.T) {
	notFound := &github.APIError{Op: "compare", Status: 404, Message: "Not Found"}
	for name, tc := range map[string]struct {
		before bool // the comment came before magnum's first review
		cmp    Comparison
		err    error
		noCmp  bool // no Compare function
		want   string
	}{
		"before magnum's first review": {before: true, want: store.MissOutsideBeforeReview},
		"the file changed":             {cmp: Comparison{Descendant: true, Complete: true, Paths: []string{"a.rb"}}, want: store.MissOutsideFileChanged},
		"not a descendant":             {cmp: Comparison{Complete: true}, want: store.MissOutsideNotDescendant},
		"a 404":                        {err: notFound, want: store.MissOutsideNoCompare},
		"a cut file list":              {cmp: Comparison{Descendant: true}, want: store.MissOutsideNoCompare},
		"no comparison":                {noCmp: true, want: store.MissOutsideNoCompare},
	} {
		th := thread(1, "a.rb", 5, shaB, longish)
		if tc.before {
			th.Comments[0].CreatedAt = t0.Add(-time.Minute)
		}
		in := input([]github.Thread{th}, nil)
		in.Compare = func(string, string) (Comparison, error) {
			if tc.before {
				t.Fatalf("%s: compared", name)
			}
			return tc.cmp, tc.err
		}
		if tc.noCmp {
			in.Compare = nil
		}
		res, err := Build(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		o := outsideOf(t, name, res)
		wantReviewed := shaA
		if tc.before {
			wantReviewed = shaB // no review before it: its own commit
		}
		if o.OutsideReason != tc.want || o.CommentSHA != shaB || o.ReviewedSHA != wantReviewed {
			t.Errorf("%s: outside = reason %q, comment %s, reviewed %s; want %q, %s, %s", name, o.OutsideReason, short(o.CommentSHA),
				short(o.ReviewedSHA), tc.want, short(shaB), short(wantReviewed))
		}
	}

	// A thread GitHub gives no commit cannot be compared either.
	in := input([]github.Thread{thread(1, "a.rb", 5, "", longish)}, nil)
	in.Compare = func(string, string) (Comparison, error) {
		t.Fatal("compared without a commit")
		return Comparison{}, nil
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if o := outsideOf(t, "no commit", res); o.OutsideReason != store.MissOutsideNoCompare || o.CommentSHA != "" || o.ReviewedSHA != shaA {
		t.Errorf("no commit: outside = %+v", o)
	}
}

// TestBuildSaysWhyAReviewBodyIsOutside: a review body on a commit magnum
// did not review is outside because it came before magnum's first review,
// or else because a body has no file to compare (body_unreviewed); one on a
// reviewed commit is a candidate, and no candidate carries a reason.
func TestBuildSaysWhyAReviewBodyIsOutside(t *testing.T) {
	rv := func(id int64, sha string, at time.Time) github.Review {
		return github.Review{DatabaseID: id, State: "COMMENTED", Body: longish, CommitOid: sha, AuthorLogin: "rev-ann", AuthorType: "User",
			SubmittedAt: at, URL: "https://github.com/talkable/widgets/pull/7#pullrequestreview-" + strconv.FormatInt(id, 10)}
	}
	res, err := Build(input(nil, []github.Review{rv(1, shaB, t0.Add(-time.Hour)), rv(2, shaB, t0.Add(time.Hour)), rv(3, shaA, t0.Add(time.Hour))}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"r1": store.MissOutsideBeforeReview, "r2": store.MissOutsideBodyUnreviewed}
	if len(res.Outside) != 2 || len(res.Candidates) != 1 || res.Candidates[0].ID != "r3" || res.Candidates[0].OutsideReason != "" {
		t.Fatalf("outside %v, candidates %+v", ids(res.Outside), res.Candidates)
	}
	for _, o := range res.Outside {
		if o.OutsideReason != want[o.ID] || o.CommentSHA != shaB {
			t.Errorf("%s: reason %q, comment %s; want %q", o.ID, o.OutsideReason, short(o.CommentSHA), want[o.ID])
		}
	}
}
