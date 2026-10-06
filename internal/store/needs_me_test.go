package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mineZhuravel is the operator's logins as the screens fold them: the user
// and the App named after them.
func mineZhuravel(login string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimPrefix(login, "@"), "[bot]"), "zhuravel")
}

// A PR magnum approved needs the operator when GitHub still waits for an
// approval that counts (an App's never does): open, not a draft, not their
// own, magnum's latest review on the current head and clean (an approval,
// or a clean comment from an identity that comments when clean), and
// GitHub's review decision REVIEW_REQUIRED ("approve"), or
// CHANGES_REQUESTED with every outstanding changes request the operator's
// own ("lift"). Anyone else's changes request, a decision GitHub has not
// told yet or a list of opinions cut at 100 leaves it alone.
func TestNeedsMeDecidesWhatTheOperatorsApprovalWouldFix(t *testing.T) {
	cr := func(login string) LatestReview {
		return LatestReview{Login: login, State: "CHANGES_REQUESTED", CommitSHA: "old"}
	}
	ok := func(login string) LatestReview { return LatestReview{Login: login, State: "APPROVED", CommitSHA: "h1"} }
	base := func() NeedsMeFacts {
		return NeedsMeFacts{GHState: GHOpen, Author: "alice", HeadSHA: "h1", ReviewedSHA: "h1", LastReviewEvent: "APPROVED",
			Gate: &ReviewGate{Decision: "REVIEW_REQUIRED", Opinions: []LatestReview{}, Complete: true}}
	}
	for _, tc := range []struct {
		name string
		edit func(*NeedsMeFacts)
		want string
	}{
		{"magnum approved, a review that counts is required", func(*NeedsMeFacts) {}, NeedsMeApprove},
		{"the event as posted", func(f *NeedsMeFacts) { f.LastReviewEvent = "approve" }, NeedsMeApprove},
		{"only the operator's changes request blocks it", func(f *NeedsMeFacts) {
			f.Gate = &ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []LatestReview{cr("zhuravel"), ok("rev-ann")}, Complete: true}
		}, NeedsMeLift},
		{"someone else's changes request", func(f *NeedsMeFacts) {
			f.Gate = &ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []LatestReview{cr("bob-rev")}, Complete: true}
		}, ""},
		{"the operator's and someone else's", func(f *NeedsMeFacts) {
			f.Gate = &ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []LatestReview{cr("zhuravel"), cr("bob-rev")}, Complete: true}
		}, ""},
		{"changes requested by nobody listed", func(f *NeedsMeFacts) {
			f.Gate = &ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []LatestReview{ok("rev-ann")}, Complete: true}
		}, ""},
		{"opinions cut at 100", func(f *NeedsMeFacts) {
			f.Gate = &ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []LatestReview{cr("zhuravel")}, Complete: false}
		}, ""},
		{"approved on GitHub", func(f *NeedsMeFacts) { f.Gate.Decision = "APPROVED" }, ""},
		{"no review required", func(f *NeedsMeFacts) { f.Gate.Decision = "" }, ""},
		{"GitHub not read yet", func(f *NeedsMeFacts) { f.Gate = nil }, ""},
		{"draft", func(f *NeedsMeFacts) { f.Draft = true }, ""},
		{"closed", func(f *NeedsMeFacts) { f.GHState = GHClosed }, ""},
		{"merged", func(f *NeedsMeFacts) { f.GHState = GHMerged }, ""},
		{"the operator's own PR", func(f *NeedsMeFacts) { f.Author = "Zhuravel" }, ""},
		{"a PR of the App named after the operator", func(f *NeedsMeFacts) { f.Author = "zhuravel[bot]" }, ""},
		{"a ghost author", func(f *NeedsMeFacts) { f.Author = "" }, NeedsMeApprove},
		{"stale head: magnum reviewed an older commit", func(f *NeedsMeFacts) { f.HeadSHA = "h2" }, ""},
		{"never reviewed", func(f *NeedsMeFacts) { f.ReviewedSHA = "" }, ""},
		{"magnum requested changes", func(f *NeedsMeFacts) { f.LastReviewEvent = "CHANGES_REQUESTED" }, ""},
		{"magnum's approval dismissed", func(f *NeedsMeFacts) { f.LastReviewEvent = "DISMISSED" }, ""},
		{"a comment from an identity that approves when clean", func(f *NeedsMeFacts) {
			f.LastReviewEvent, f.Verdict = "COMMENTED", VerdictClean
		}, ""},
		{"a clean comment from an identity that comments when clean", func(f *NeedsMeFacts) {
			f.LastReviewEvent, f.Verdict, f.CommentWhenClean = "COMMENTED", VerdictClean, true
		}, NeedsMeApprove},
		{"a non-blocking comment from that identity", func(f *NeedsMeFacts) {
			f.LastReviewEvent, f.Verdict, f.CommentWhenClean = "COMMENTED", VerdictNonBlocking, true
		}, ""},
		{"a comment whose verdict is unknown", func(f *NeedsMeFacts) {
			f.LastReviewEvent, f.CommentWhenClean = "COMMENTED", true
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.edit(&f)
			if got := NeedsMe(f, mineZhuravel); got != tc.want {
				t.Fatalf("NeedsMe = %q, want %q (%+v)", got, tc.want, f)
			}
		})
	}
}

// TestMigrationV18ToV19AddsTheReviewGate builds a registry the v18 binary
// would have written (user_version 18) with an open PR whose Details were
// fetched and a merged one, and reopens it with Open: prs.review_gate_json
// exists and is empty, and the open PR's details_at is cleared so the next
// poll reads its gate, while the merged one keeps it.
func TestMigrationV18ToV19AddsTheReviewGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var setup []string
	for _, m := range migrations[:18] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 18",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, gh_state, gh_updated_at, merged_at, closed_at, details_at, activity_at, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 'OPEN', '`+ts+`', NULL, NULL, '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`'),
		        (2, 1, 'PR_2', 8, 'u8', 'h2', '`+ts+`', 'closed', 'talkable-app', 'MERGED', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`', '`+ts+`')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v18 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v18 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 19 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	open, _ := st.PRByID(ctx, 1)
	if open.ReviewGate != nil || open.DetailsAt != nil {
		t.Fatalf("open PR: gate %+v, details_at %v; want neither until the next poll", open.ReviewGate, open.DetailsAt)
	}
	if open.ActivityAt == nil {
		t.Fatal("the open PR lost its activity time")
	}
	merged, _ := st.PRByID(ctx, 2)
	if merged.ReviewGate != nil || merged.DetailsAt == nil {
		t.Fatalf("merged PR: gate %+v, details_at %v; want no gate and its details_at kept", merged.ReviewGate, merged.DetailsAt)
	}
}

// The poller writes the review gate the Details read without counting it
// as a GitHub change (it moves no eligibility); an upsert without one keeps
// the stored gate, and a new one replaces it.
func TestUpsertKeepsTheReviewGate(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_7", Number: 7, URL: "u7", HeadSHA: "h1", GHState: GHOpen,
		GHUpdatedAt: Ptr(t0), InitialState: PRBaseline, Identity: "talkable-app",
		ReviewGate: &ReviewGate{Decision: "REVIEW_REQUIRED", Opinions: []LatestReview{}, Complete: true}}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if g := res.PR.ReviewGate; g == nil || g.Decision != "REVIEW_REQUIRED" || !g.Complete || g.Opinions == nil {
		t.Fatalf("inserted gate = %+v", g)
	}
	id := res.PR.ID

	lift := &ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true,
		Opinions: []LatestReview{{Login: "zhuravel", State: "CHANGES_REQUESTED", CommitSHA: "h0"}}}
	in.ReviewGate = lift
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed {
		t.Fatalf("a new gate: changed %v, %v", res.Changed, err)
	}
	if !reflect.DeepEqual(res.PR.ReviewGate, lift) {
		t.Fatalf("gate after the change = %+v, want %+v", res.PR.ReviewGate, lift)
	}

	in.ReviewGate = nil
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed {
		t.Fatalf("no gate: changed %v, %v", res.Changed, err)
	}
	pr, err := st.PRByID(ctx, id)
	if err != nil || !reflect.DeepEqual(pr.ReviewGate, lift) {
		t.Fatalf("gate after an upsert without one = %+v, %v; want it kept", pr.ReviewGate, err)
	}
	rows, err := st.Board(ctx, BoardFilter{})
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].ReviewGate, lift) {
		t.Fatalf("board row gate = %+v, %v", rows, err)
	}
}

// NeedsMePRs lists the open PRs that need the operator with their
// repository, reading magnum's latest round's verdict for a clean comment
// of an identity that comments when clean.
func TestNeedsMePRsListsThePRsWaitingForTheOperator(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	required := &ReviewGate{Decision: "REVIEW_REQUIRED", Opinions: []LatestReview{}, Complete: true}
	add := func(n int, author, event string, gate *ReviewGate, identity string) int64 {
		t.Helper()
		head := "h" + string(rune('0'+n))
		res, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_" + head, Number: n, URL: "https://github.com/talkable/talkable/pull/" + head,
			HeadSHA: head, GHState: GHOpen, AuthorLogin: Ptr(author), ReviewGate: gate, InitialState: PRReviewed, Identity: identity})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdatePR(ctx, res.PR.ID, func(u *PRUpdate) {
			u.Set("reviewed_sha", head)
			u.Set("last_review_event", event)
		}); err != nil {
			t.Fatal(err)
		}
		return res.PR.ID
	}
	approved := add(1, "alice", "APPROVED", required, "talkable-app")
	lift := add(2, "alice", "APPROVED", &ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true,
		Opinions: []LatestReview{{Login: "zhuravel", State: "CHANGES_REQUESTED", CommitSHA: "h0"}}}, "talkable-app")
	add(3, "zhuravel", "APPROVED", required, "talkable-app") // the operator's own
	add(4, "alice", "APPROVED", nil, "talkable-app")         // the gate not read yet
	clean := add(5, "alice", "COMMENTED", required, "commenter")
	add(6, "alice", "COMMENTED", required, "commenter") // its round was not clean
	for id, verdict := range map[int64]string{clean: VerdictClean, clean + 1: VerdictNonBlocking} {
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, outcome, review_id, prompt_text, result_json, created_at)
			VALUES (?, ?, 1, 'codex-judge', 'initial', 'h', 'commenter', 'talkable[bot]', 'verified', 'posted', 1, 'p', ?, ?)`,
			"r-"+verdict, id, `{"event":"COMMENT","verdict":"`+verdict+`","findings":{}}`, FormatTime(t0)); err != nil {
			t.Fatal(err)
		}
	}
	comments := func(repo, identity string) bool { return repo == "talkable/talkable" && identity == "commenter" }
	got, err := st.NeedsMePRs(ctx, comments, mineZhuravel)
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, n := range got {
		if n.Repo != "talkable/talkable" {
			t.Errorf("#%d repo %q", n.PR.Number, n.Repo)
		}
		have = append(have, n.NeedsMe+":"+n.PR.HeadSHA)
	}
	want := []string{NeedsMeApprove + ":h1", NeedsMeLift + ":h2", NeedsMeApprove + ":h5"}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("NeedsMePRs = %v, want %v", have, want)
	}
	if got[0].PR.ID != approved || got[1].PR.ID != lift || got[2].PR.ID != clean {
		t.Fatalf("ids %d %d %d", got[0].PR.ID, got[1].PR.ID, got[2].PR.ID)
	}
}
