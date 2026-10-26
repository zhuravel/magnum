package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// statsAutoApproveConfig makes the fixture's talkable watch approve
// talkable/talkable as the operator (auto_approve_as = "zhuravel").
func statsAutoApproveConfig(t *testing.T, f *inspFixture) {
	t.Helper()
	path := filepath.Join(f.Home, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const watch = "include = [\"talkable\"]\nidentity = \"talkable-app\"\n"
	cfg := strings.Replace(string(b), watch, watch+"auto_approve = [\"talkable\"]\nauto_approve_as = \"zhuravel\"\n", 1)
	if cfg == string(b) {
		t.Fatal("the fixture's talkable watch moved")
	}
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// statsMergedPR seeds PR number of fullName by alice, merged at merged on
// head, with GitHub's latest reviews and the merge gate's opinions as the
// last Details read them.
func statsMergedPR(t *testing.T, st *store.Store, fullName string, number int, merged time.Time, head string, latest, opinions []store.LatestReview) store.PR {
	t.Helper()
	_, pr := inspSeedPR(t, st, fullName, number, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("author_login", "alice")
		u.Set("gh_state", store.GHMerged)
		u.Set("merged_at", merged)
		u.Set("closed_at", merged)
		u.Set("head_sha", head)
		u.Set("latest_reviews_json", latest)
		if opinions != nil {
			u.Set("review_gate_json", &store.ReviewGate{Decision: "APPROVED", Opinions: opinions, Complete: true})
		}
	})
	return pr
}

// statsApproved is an approval of commit by login submitted at at (zero:
// the merge gate's opinions carry no time).
func statsApproved(login, commit string, at time.Time) store.LatestReview {
	return store.LatestReview{Login: login, State: "APPROVED", SubmittedAt: statsOpt(at), CommitSHA: commit}
}

// statsAutoApproval records an approval magnum posted as zhuravel of pr's
// head at posted (GitHub's review reviewID), then in state, ended by
// endedBy ("" = not ended).
func statsAutoApproval(t *testing.T, st *store.Store, pr store.PR, head string, reviewID int64, posted time.Time, state, endedBy string) store.AutoApproval {
	t.Helper()
	ctx := context.Background()
	a, err := st.InsertAutoApproval(ctx, store.AutoApproval{PRID: pr.ID, RunID: "run-review", HeadSHA: head, Identity: "zhuravel", Login: "zhuravel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionAutoApproval(ctx, a.ID, nil, state, func(u *store.AutoApprovalUpdate) {
		u.Set("review_id", reviewID)
		u.Set("posted_at", posted)
		if endedBy != "" {
			u.Set("ended_by", endedBy)
			u.Set("ended_at", posted.Add(time.Hour))
		}
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

// statsMiss records a retro miss of class miss on pr at sourceURL, raised
// by reviewer.
func statsMiss(t *testing.T, st *store.Store, pr store.PR, sourceURL, reviewer string) {
	t.Helper()
	if _, err := st.UpsertMiss(context.Background(), store.Miss{PRID: pr.ID, SourceURL: sourceURL, SourceKind: store.MissSourceThread,
		Reviewer: reviewer, ReviewedSHA: "h1", Class: store.MissMiss}); err != nil {
		t.Fatal(err)
	}
}

// For each repository magnum approves as the operator, the window's merges
// that rest on magnum's approval alone: magnum's automatic approval stood
// at the merge and no person approved, where a person's approval is one in
// GitHub's latest reviews or the merge gate's opinions that is not a bot's,
// not the author's and not one of magnum's own automatic approvals (the
// same login, the same head, submitted by the time magnum recorded it); the
// operator's approval by hand (later than magnum's, or posted with
// `magnum approve --as`) is a person's. Of those, the ones merged past the
// approved head, the ones of these merged unreviewed (a pr.merged_unreviewed
// event; a later head magnum reviewed is not), and the retro's misses on
// them: a miss on a PR a person approved, and magnum's own finding the
// author declined that the retro routed to the notes as a miss, are not
// counted. A repository without auto_approve and a merge before the window
// are left out.
func TestStatsApprovalsCountsMergesOnMagnumsApprovalAlone(t *testing.T) {
	f, st := statsConvFixture(t)
	statsAutoApproveConfig(t, f)
	const tk = "talkable/talkable"
	merged := statsOct(3, 12, 0)
	posted := statsOct(2, 12, 0)
	app := store.LatestReview{Login: "talkable[bot]", State: "COMMENTED", SubmittedAt: statsOpt(posted.Add(-time.Hour)), CommitSHA: "h1"}
	magnums := statsApproved("zhuravel", "h1", posted.Add(-2*time.Second))

	// A person and magnum.
	pr401 := statsMergedPR(t, st, tk, 401, merged, "h1", []store.LatestReview{app, magnums, statsApproved("rev-ann", "h1", posted)}, nil)
	statsAutoApproval(t, st, pr401, "h1", 4010, posted, store.AutoDismissed, store.AutoEndedClosed)
	statsMiss(t, st, pr401, pr401.URL+"#discussion_r1", "bob-rev") // a person approved: no merge on magnum's alone
	// Magnum only, at the merged head (its approval still standing: the
	// daemon has not seen the merge yet).
	pr402 := statsMergedPR(t, st, tk, 402, merged, "h1", []store.LatestReview{app, magnums}, nil)
	statsAutoApproval(t, st, pr402, "h1", 4020, posted, store.AutoStanding, "")
	// The author declined magnum's own finding on it as wrong for the
	// repository, which the retro routes to the notes as a miss of the
	// same link: magnum's false positive, no defect a reviewer caught.
	declined := pr402.URL + "#discussion_r9"
	if _, err := st.UpsertDecline(context.Background(), store.Decline{PRID: pr402.ID, SourceURL: declined, CommentID: 9, ReviewedSHA: "h1",
		Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Lesson: "the cache is rebuilt nightly", Scope: store.MissScopeRepo}); err != nil {
		t.Fatal(err)
	}
	statsMiss(t, st, pr402, declined, "alice")
	// Magnum only, merged past the approved head, unreviewed; one retro
	// miss on it (an outside comment is no miss).
	pr403 := statsMergedPR(t, st, tk, 403, merged, "h2", []store.LatestReview{app, magnums}, []store.LatestReview{statsApproved("zhuravel", "h1", time.Time{})})
	statsAutoApproval(t, st, pr403, "h1", 4030, posted, store.AutoDismissed, store.AutoEndedClosed)
	statsEvent(t, st, merged.Add(time.Minute), "pr:"+tk+"#403", statsKindMergedUnreviewed, "merged before magnum reviewed its last push", "")
	statsMiss(t, st, pr403, pr403.URL+"#discussion_r1", "bob-rev")
	if _, err := st.UpsertMiss(context.Background(), store.Miss{PRID: pr403.ID, SourceURL: pr403.URL + "#discussion_r2",
		SourceKind: store.MissSourceThread, Reviewer: "bob-rev", ReviewedSHA: "h1", Class: store.MissOutside}); err != nil {
		t.Fatal(err)
	}
	// Magnum only, merged past the approved head, but magnum reviewed the
	// merged head (clean, so it had nothing to approve again): no
	// pr.merged_unreviewed event.
	pr410 := statsMergedPR(t, st, tk, 410, merged, "h2", []store.LatestReview{app, magnums}, nil)
	statsAutoApproval(t, st, pr410, "h1", 4100, posted, store.AutoDismissed, store.AutoEndedClosed)
	// The operator approved the head by hand after magnum, and the PR
	// merged before magnum saw it: GitHub's latest review of theirs is the
	// later one, a person's (the gate's opinion of the same login and head
	// carries no time and reads as magnum's).
	pr404 := statsMergedPR(t, st, tk, 404, merged, "h1", []store.LatestReview{app, statsApproved("zhuravel", "h1", posted.Add(time.Hour))},
		[]store.LatestReview{statsApproved("zhuravel", "h1", time.Time{})})
	statsAutoApproval(t, st, pr404, "h1", 4040, posted, store.AutoDismissed, store.AutoEndedClosed)
	// Bot approvals and the author's own are no person's.
	pr405 := statsMergedPR(t, st, tk, 405, merged, "h1", []store.LatestReview{app, magnums, statsApproved("renovate[bot]", "h1", posted),
		statsApproved("deploy-bot", "h1", posted)}, []store.LatestReview{statsApproved("alice", "h1", time.Time{})})
	statsAutoApproval(t, st, pr405, "h1", 4050, posted, store.AutoDismissed, store.AutoEndedClosed)
	// An approval only in the merge gate's opinions counts.
	pr406 := statsMergedPR(t, st, tk, 406, merged, "h1", []store.LatestReview{app, magnums},
		[]store.LatestReview{statsApproved("zhuravel", "h1", time.Time{}), statsApproved("bob-rev", "h1", time.Time{})})
	statsAutoApproval(t, st, pr406, "h1", 4060, posted, store.AutoDismissed, store.AutoEndedClosed)
	// No approval of magnum's stood: one withdrawn after a later review,
	// and the operator's `magnum approve --as`, which is theirs by hand.
	pr407 := statsMergedPR(t, st, tk, 407, merged, "h1", []store.LatestReview{app, statsApproved("bob-rev", "h1", posted)}, nil)
	statsAutoApproval(t, st, pr407, "h0", 4070, posted.Add(-time.Hour), store.AutoDismissed, store.AutoEndedMagnum)
	pr408 := statsMergedPR(t, st, tk, 408, merged, "h1", []store.LatestReview{app, magnums}, nil)
	byHand := statsAutoApproval(t, st, pr408, "h1", 4080, posted, store.AutoDismissed, store.AutoEndedClosed)
	statsEvent(t, st, posted.Add(-time.Second), "pr:"+tk+"#408", statsKindManualApproval, "approving as zhuravel",
		`{"event":"APPROVE","sha":"h1","identity":"zhuravel","approval_id":`+itoa(byHand.ID)+`}`)
	// Merged before the window, and a merge in a repository without
	// auto_approve.
	pr409 := statsMergedPR(t, st, tk, 409, statsAt(time.September, 20, 12, 0), "h1", []store.LatestReview{app, magnums}, nil)
	statsAutoApproval(t, st, pr409, "h1", 4090, statsAt(time.September, 19, 12, 0), store.AutoDismissed, store.AutoEndedClosed)
	statsMergedPR(t, st, "example/widgets", 9, merged, "h1", nil, nil)
	st.Close()
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	r, err := statsGather(context.Background(), f.store(), f.Ctx.Config, statsNow.Add(-7*24*time.Hour), statsNow, "")
	if err != nil {
		t.Fatal(err)
	}
	// 401 to 406 and 410 merged on magnum's approval; 402, 403, 405 and 410
	// on it alone, 403 and 410 past its head, 403 unreviewed; one miss, 403's.
	want := []statsApprovals{{Repo: tk, Merged: 9, AutoApproved: 7, MagnumOnly: 4, PastApproval: 2, Unreviewed: 1, Misses: 1}}
	if got, w := statsJSON(t, r.Approvals), statsJSON(t, want); got != w {
		t.Fatalf("approvals =\n%s\nwant\n%s", got, w)
	}

	if code := f.run("stats"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines := statsLines(f.Out.String())
	i := slices.Index(lines, "APPROVALS")
	if i < 0 || len(lines) < i+3 || lines[i+1] != "REPO MERGED AUTO-APPROVED MAGNUM ONLY PAST APPROVAL UNREVIEWED MISSES" ||
		lines[i+2] != "talkable/talkable 9 7 4 (57%) 2 1 1" {
		t.Fatalf("approvals table in\n%s", f.Out.String())
	}
	if strings.Contains(f.Out.String(), "example/widgets") {
		t.Errorf("a repository without auto_approve in\n%s", f.Out.String())
	}

	if code := f.run("stats", "--json"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	approvals, _ := doc["approvals"].([]any)
	if len(approvals) != 1 {
		t.Fatalf("approvals = %v", doc["approvals"])
	}
	keys := []string{"auto_approved", "magnum_only", "merged", "misses", "past_approval", "repo", "unreviewed"}
	if got := statsKeys(statsObj(t, approvals[0])); !slices.Equal(got, keys) {
		t.Errorf("approval keys = %v, want %v", got, keys)
	}
}
