package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestMigrationV1ToV2KeepsData builds a registry the deployed binary would
// have written (0001 only, user_version 1) and reopens it with Open.
func TestMigrationV1ToV2KeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	for _, q := range []string{
		migrations[0].sql,
		"PRAGMA user_version = 1",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '` + ts + `')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, labels_json, state, identity,
		   reviewed_sha, last_review_event, reviewed_at, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'https://github.com/talkable/talkable/pull/7', 'seven', 'h2', '` + ts + `', '["WIP"]',
		   'reviewed', 'talkable-app', 'h1', 'COMMENTED', '` + ts + `', '` + ts + `', '` + ts + `')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at)
		 VALUES (2, 1, 'PR_2', 8, 'u8', 'x1', '` + ts + `', 'baseline', 'talkable-app', '` + ts + `', '` + ts + `')`,
		`INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, review_id, prompt_text, created_at, verified_at)
		 VALUES ('r1', 1, 1, 'judge', 'initial', 'h0', 'zhuravel', 'zhuravel', 'verified', 10, 'p', '` + ts + `', '` + FormatTime(t0.Add(-time.Hour)) + `'),
		        ('r2', 1, 2, 'judge', 'rereview', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 11, 'p', '` + ts + `', '` + ts + `'),
		        ('r3', 1, 2, 'claude', 'rereview', 'h1', 'talkable-app', 'someone', 'verified', NULL, 'p', '` + ts + `', '` + FormatTime(t0.Add(time.Hour)) + `')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v1 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 2 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	var cols []string
	rows, err := st.DB().QueryContext(ctx, "SELECT name FROM pragma_table_info('prs')")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if !reflect.DeepEqual(cols, prColumns) {
		t.Fatalf("prs columns after 0002 =\n%v\nwant (prColumns)\n%v", cols, prColumns)
	}

	pr, err := st.PRByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if Deref(pr.Title) != "seven" || pr.State != PRReviewed || Deref(pr.ReviewedSHA) != "h1" ||
		!reflect.DeepEqual(pr.Labels, []string{"WIP"}) || Deref(pr.LastReviewEvent) != "COMMENTED" {
		t.Fatalf("v1 data changed: %+v", pr)
	}
	if !reflect.DeepEqual(pr.Assignees, []string{}) || !reflect.DeepEqual(pr.RequestedReviewers, []string{}) ||
		!reflect.DeepEqual(pr.LatestReviews, []LatestReview{}) || pr.SinceReview != nil || pr.BaseSHA != nil || pr.DetailsAt != nil {
		t.Fatalf("new columns must default to empty: %+v", pr)
	}
	// Backfilled from the latest verified judge run with a review, "[bot]" stripped.
	if Deref(pr.LastReviewLogin) != "talkable" {
		t.Fatalf("last_review_login = %v", pr.LastReviewLogin)
	}
	if other, _ := st.PRByID(ctx, 2); other.LastReviewLogin != nil {
		t.Fatalf("unreviewed PR got a login: %q", *other.LastReviewLogin)
	}
	board, err := st.Board(ctx, BoardFilter{})
	if err != nil || len(board) != 2 {
		t.Fatalf("board on a migrated registry = %+v, %v", board, err)
	}
}

func TestUpsertPRBoardFields(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	sub := t0.Add(-time.Hour)
	reviews := []LatestReview{{Login: "rev-ann", State: "APPROVED", SubmittedAt: &sub, CommitSHA: "h1"}}
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_9", Number: 9, URL: "u9", HeadSHA: "h1", InitialState: PRQueued, Identity: "zhuravel",
		Assignees: []string{"zhuravel"}, RequestedReviewers: []string{"team:engineers", "rev-ann"}, LatestReviews: reviews,
		BaseSHA: Ptr("b1"), DetailsAt: Ptr(t0)}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil || !res.New {
		t.Fatalf("insert = %+v, %v", res, err)
	}
	pr := res.PR
	if !reflect.DeepEqual(pr.Assignees, []string{"zhuravel"}) || !reflect.DeepEqual(pr.RequestedReviewers, []string{"team:engineers", "rev-ann"}) ||
		len(pr.LatestReviews) != 1 || !pr.LatestReviews[0].equal(reviews[0]) || Deref(pr.BaseSHA) != "b1" ||
		pr.DetailsAt == nil || !pr.DetailsAt.Equal(t0) {
		t.Fatalf("inserted board fields: %+v", pr)
	}

	// Same values with a newer DetailsAt: nothing changed, nothing written.
	c.Add(time.Minute)
	sub2 := sub // equal instant, different pointer
	in.LatestReviews = []LatestReview{{Login: "rev-ann", State: "APPROVED", SubmittedAt: &sub2, CommitSHA: "h1"}}
	in.DetailsAt = Ptr(t0.Add(time.Minute))
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed || !res.PR.DetailsAt.Equal(t0) || !res.PR.UpdatedAt.Equal(t0) {
		t.Fatalf("no-op refresh = %+v, %v", res, err)
	}

	// nil board fields keep the stored values.
	keep := in
	keep.Assignees, keep.RequestedReviewers, keep.LatestReviews, keep.BaseSHA, keep.DetailsAt = nil, nil, nil, nil, nil
	if res, err = st.UpsertPRFromGitHub(ctx, keep); err != nil || res.Changed || len(res.PR.Assignees) != 1 {
		t.Fatalf("nil fields = %+v, %v", res, err)
	}

	// A new review state is a change and stamps details_at.
	in.LatestReviews = []LatestReview{{Login: "rev-ann", State: "CHANGES_REQUESTED", SubmittedAt: &sub, CommitSHA: "h1"}}
	in.Assignees = []string{}
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || !res.Changed {
		t.Fatalf("review change = %+v, %v", res, err)
	}
	if res.PR.LatestReviews[0].State != "CHANGES_REQUESTED" || len(res.PR.Assignees) != 0 || !res.PR.DetailsAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after change: %+v", res.PR)
	}

	// since_review_json and last_review_login go through UpdatePR.
	since := SinceReview{Source: SinceFromReviewed, Base: "h0", Head: "h1", Commits: 2, Files: -1, Additions: 10, Deletions: 3, ComputedAt: t0}
	if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) {
		u.Set("since_review_json", since)
		u.Set("last_review_login", "talkable")
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PRByID(ctx, pr.ID)
	if got.SinceReview == nil || *got.SinceReview != since || Deref(got.LastReviewLogin) != "talkable" {
		t.Fatalf("since review = %+v login %v", got.SinceReview, got.LastReviewLogin)
	}
	if err := st.UpdatePR(ctx, pr.ID, func(u *PRUpdate) { u.Set("since_review_json", (*SinceReview)(nil)) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PRByID(ctx, pr.ID); got.SinceReview != nil {
		t.Fatalf("nil *SinceReview must store NULL: %+v", got.SinceReview)
	}
}

func TestBoard(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	other, err := st.UpsertRepo(ctx, Repo{NodeID: "R_2", Owner: "zhuravel", Name: "widgets", Mode: RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	upsert := func(repoID int64, n int, state string, updated time.Time, mod func(*GitHubPR)) PR {
		t.Helper()
		in := GitHubPR{RepoID: repoID, NodeID: "PR_" + itoa(repoID) + "_" + itoa(int64(n)), Number: n, URL: "u" + itoa(int64(n)),
			HeadSHA: "head" + itoa(int64(n)), Title: Ptr("PR " + itoa(int64(n))), AuthorLogin: Ptr("alice"),
			GHState: GHOpen, GHUpdatedAt: Ptr(updated), InitialState: state, Identity: "talkable-app"}
		if mod != nil {
			mod(&in)
		}
		res, err := st.UpsertPRFromGitHub(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res.PR
	}
	sub := t0.Add(-2 * time.Hour)
	held := upsert(repo.ID, 1, PRReviewed, t0.Add(-time.Hour), func(in *GitHubPR) {
		in.Assignees = []string{"zhuravel"}
		in.RequestedReviewers = []string{"team:engineers"}
		in.ReviewRequested = Ptr(true)
		in.LatestReviews = []LatestReview{{Login: "talkable", State: "COMMENTED", SubmittedAt: &sub, CommitSHA: "head0"}}
		in.Labels = []string{"WIP"}
		in.IsDraft = true
	})
	upsert(repo.ID, 2, PRQueued, t0, nil)
	upsert(other.ID, 3, PRIneligible, t0.Add(-3*time.Hour), nil)
	closed := upsert(repo.ID, 4, PRClosed, t0.Add(time.Hour), nil)
	merged := upsert(repo.ID, 5, PRReviewed, t0.Add(2*time.Hour), func(in *GitHubPR) { in.GHState = GHMerged })
	noTime := upsert(repo.ID, 6, PRBaseline, t0, func(in *GitHubPR) { in.GHUpdatedAt = nil })

	since := SinceReview{Source: SinceFromReviewed, Base: "head0", Head: "head1", Commits: 1, Files: 2, Additions: 5, Deletions: 1, ComputedAt: t0}
	if err := st.UpdatePR(ctx, held.ID, func(u *PRUpdate) {
		u.Set("reviewed_sha", "head0")
		u.Set("last_review_event", "COMMENTED")
		u.Set("reviewed_at", t0.Add(-2*time.Hour))
		u.Set("last_review_login", "talkable")
		u.Set("since_review_json", since)
		u.Set("rounds_today", 2)
		u.Set("rounds_day", DayKey(t0))
		u.Set("pinned", true)
		u.Set("next_eligible_at", t0.Add(30*time.Minute))
		u.Set("last_error", "boom")
	}); err != nil {
		t.Fatal(err)
	}
	sl := mustSlot(t, st, "review1", SlotFree)
	if _, err := st.AssignSlot(ctx, held.ID, sl.ID); err != nil {
		t.Fatal(err)
	}

	rows, err := st.Board(ctx, BoardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, r := range rows {
		refs = append(refs, r.Ref)
	}
	want := []string{"talkable/talkable#2", "talkable/talkable#1", "zhuravel/widgets#3", "talkable/talkable#6"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("board order = %v, want %v (closed/merged hidden, never-updated last)", refs, want)
	}
	r := rows[1]
	wantRow := BoardRow{
		PRID: held.ID, Ref: "talkable/talkable#1", Owner: "talkable", Name: "talkable", Number: 1, Title: "PR 1", Author: "alice",
		URL: "u1", Draft: true, Labels: []string{"WIP"}, Assignees: []string{"zhuravel"}, RequestedReviewers: []string{"team:engineers"},
		ReviewRequested: true, LatestReviews: held.LatestReviews, SinceReview: &since, State: PRReviewed, GHState: GHOpen,
		UpdatedAt: t0.Add(-time.Hour), HeadSHA: "head1", ReviewedSHA: "head0", LastReviewEvent: "COMMENTED",
		LastReviewAt: t0.Add(-2 * time.Hour), LastReviewLogin: "talkable", Identity: "talkable-app", Slot: "review1",
		SlotPath: sl.Path, Pinned: true, NextEligibleAt: t0.Add(30 * time.Minute), LastError: "boom", RoundsToday: 2,
	}
	if !reflect.DeepEqual(r, wantRow) {
		t.Fatalf("row =\n%+v\nwant\n%+v", r, wantRow)
	}
	if q := rows[0]; q.Slot != "" || q.SinceReview != nil || q.Assignees == nil || q.LatestReviews == nil || q.RoundsToday != 0 {
		t.Fatalf("bare row = %+v", q)
	}

	// rounds_today from an earlier day reads as 0.
	c.Add(48 * time.Hour)
	rows, _ = st.Board(ctx, BoardFilter{Repo: "TALKABLE/talkable", Limit: 2})
	if len(rows) != 2 || rows[1].Number != 1 || rows[1].RoundsToday != 0 {
		t.Fatalf("repo filter + limit + stale rounds = %+v", rows)
	}

	rows, _ = st.Board(ctx, BoardFilter{IncludeClosed: true})
	if len(rows) != 6 || rows[0].PRID != merged.ID || rows[1].PRID != closed.ID || rows[5].PRID != noTime.ID {
		t.Fatalf("include closed = %d rows, first %+v", len(rows), rows)
	}
	rows, _ = st.Board(ctx, BoardFilter{States: []string{PRClosed, PRIneligible}})
	if len(rows) != 2 || rows[0].Number != 4 || rows[1].Number != 3 {
		t.Fatalf("states filter = %+v", rows)
	}
	rows, _ = st.Board(ctx, BoardFilter{Repo: "widgets"})
	if len(rows) != 1 || rows[0].Ref != "zhuravel/widgets#3" {
		t.Fatalf("name-only filter = %+v", rows)
	}
	rows, err = st.Board(ctx, BoardFilter{Repo: "nobody/nothing"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("unknown repo = %+v, %v", rows, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
