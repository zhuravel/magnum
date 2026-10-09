package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestMigrationV17ToV18AddsActivityAt builds a registry the v17 binary would
// have written (user_version 17) with an open PR whose Details were fetched,
// a merged and a closed one, and reopens it with Open: prs.activity_at
// exists, the open PR has none yet and its details_at is cleared so the next
// poll reads its activity, while the merged and the closed PR keep their
// details_at and have their merge or close as their activity.
func TestMigrationV17ToV18AddsActivityAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	merged, closed := t0.Add(-2*time.Hour), t0.Add(-3*time.Hour)
	var setup []string
	for _, m := range migrations[:17] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 17",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, gh_state, gh_updated_at, merged_at, closed_at, details_at, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 'OPEN', '`+ts+`', NULL, NULL, '`+ts+`', '`+ts+`', '`+ts+`'),
		        (2, 1, 'PR_2', 8, 'u8', 'h2', '`+ts+`', 'closed', 'talkable-app', 'MERGED', '`+ts+`', '`+FormatTime(merged)+`', '`+FormatTime(merged)+`', '`+ts+`', '`+ts+`', '`+ts+`'),
		        (3, 1, 'PR_3', 9, 'u9', 'h3', '`+ts+`', 'closed', 'talkable-app', 'CLOSED', '`+ts+`', NULL, '`+FormatTime(closed)+`', '`+ts+`', '`+ts+`', '`+ts+`')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v17 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v17 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 18 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	open, _ := st.PRByID(ctx, 1)
	if open.ActivityAt != nil || open.DetailsAt != nil {
		t.Fatalf("open PR: activity %v, details_at %v; want neither until the next poll", open.ActivityAt, open.DetailsAt)
	}
	if a := open.Activity(); a == nil || !a.Equal(t0) {
		t.Fatalf("open PR's shown activity = %v, want GitHub's updatedAt %v", a, t0)
	}
	for id, want := range map[int64]time.Time{2: merged, 3: closed} {
		pr, _ := st.PRByID(ctx, id)
		if pr.ActivityAt == nil || !pr.ActivityAt.Equal(want) || pr.DetailsAt == nil {
			t.Fatalf("PR %d: activity %v, details_at %v; want %v and its details_at kept", id, pr.ActivityAt, pr.DetailsAt, want)
		}
	}
}

// A PR's activity time is the Details' activity, or the head move the
// poller saw when that is later: an upsert with only a new activity time
// writes it without counting as a GitHub change; a push the poller sees
// moves it to the poll even when the Details' activity is older (a commit
// dated before its push) and later Details keep that; an upsert without
// Details keeps it, and moves it for a push; the insertion of a PR, which
// sets head_changed_at too, is no push.
func TestUpsertKeepsTheLastActivity(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_7", Number: 7, URL: "u7", HeadSHA: "h1", GHState: GHOpen,
		GHUpdatedAt: new(t0), InitialState: PRBaseline, Identity: "talkable-app"}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	id := res.PR.ID
	check := func(when string, want time.Time) {
		t.Helper()
		pr, err := st.PRByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if pr.ActivityAt == nil || !pr.ActivityAt.Equal(want) {
			t.Fatalf("%s: activity_at = %v, want %v", when, pr.ActivityAt, want)
		}
		if a := pr.Activity(); a == nil || !a.Equal(want) {
			t.Fatalf("%s: Activity() = %v, want %v", when, a, want)
		}
	}
	if res.PR.ActivityAt != nil {
		t.Fatalf("a PR upserted without Details has activity %v", res.PR.ActivityAt)
	}
	if a := res.PR.Activity(); a == nil || !a.Equal(t0) {
		t.Fatalf("Activity() without one = %v, want GitHub's updatedAt", a)
	}

	// The Details' activity (a comment three days ago) for an unchanged
	// updatedAt: written, not a change. The insertion was no push.
	clk.Add(time.Hour)
	in.ActivityAt = new(t0.Add(-72 * time.Hour))
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed {
		t.Fatalf("activity only: changed %v, %v", res.Changed, err)
	}
	check("first Details", t0.Add(-72*time.Hour))

	// A push the poller sees now, whose commit is dated two days ago.
	clk.Add(time.Hour)
	in.HeadSHA, in.ActivityAt = "h2", new(t0.Add(-48*time.Hour))
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || !res.HeadChanged {
		t.Fatalf("push: head changed %v, %v", res.HeadChanged, err)
	}
	pushed := t0.Add(2 * time.Hour)
	check("push", pushed)

	// Later Details with the same activity keep the push.
	clk.Add(time.Hour)
	in.GHUpdatedAt = new(t0.Add(3 * time.Hour)) // a project field moved: no activity
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("invisible change", pushed)

	// A comment after the push.
	in.ActivityAt = new(t0.Add(3 * time.Hour))
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("comment", t0.Add(3*time.Hour))

	// No Details (their fetch failed): kept; a push without them moves it.
	clk.Add(time.Hour)
	in.ActivityAt = nil
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("no Details", t0.Add(3*time.Hour))
	in.HeadSHA = "h3"
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("push without Details", t0.Add(4*time.Hour))

	// A PR inserted with its Details has their activity.
	res, err = st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_8", Number: 8, URL: "u8", HeadSHA: "h8",
		GHState: GHOpen, GHUpdatedAt: new(t0), ActivityAt: new(t0.Add(-240 * time.Hour)), InitialState: PRQueued, Identity: "talkable-app"})
	if err != nil || res.PR.ActivityAt == nil || !res.PR.ActivityAt.Equal(t0.Add(-240*time.Hour)) {
		t.Fatalf("inserted with Details: activity %v, %v", res.PR.ActivityAt, err)
	}
}

// The board shows and orders the PRs by their activity time, else GitHub's
// updatedAt; UpdatedAt stays GitHub's.
func TestBoardOrdersByTheActivityTime(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	upsert := func(n int, updated time.Time, activity *time.Time) {
		t.Helper()
		if _, err := st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_" + itoa(int64(n)), Number: n,
			URL: "u" + itoa(int64(n)), HeadSHA: "h" + itoa(int64(n)), GHState: GHOpen, GHUpdatedAt: new(updated), ActivityAt: activity,
			InitialState: PRQueued, Identity: "talkable-app"}); err != nil {
			t.Fatal(err)
		}
	}
	upsert(1, t0, new(t0.Add(-120*time.Hour)))                    // only an invisible change since five days
	upsert(2, t0.Add(-time.Hour), new(t0.Add(-time.Hour)))        // a comment an hour ago
	upsert(3, t0.Add(-2*time.Hour), nil)                          // not read yet
	upsert(4, t0.Add(-30*time.Minute), new(t0.Add(-3*time.Hour))) // a label three hours ago
	rows, err := st.Board(ctx, BoardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	type shown struct {
		n                   int
		activity, updatedAt time.Time
	}
	var got []shown
	for _, r := range rows {
		got = append(got, shown{r.Number, r.ActivityAt, r.UpdatedAt})
	}
	want := []shown{
		{2, t0.Add(-time.Hour), t0.Add(-time.Hour)},
		{3, t0.Add(-2 * time.Hour), t0.Add(-2 * time.Hour)},
		{4, t0.Add(-3 * time.Hour), t0.Add(-30 * time.Minute)},
		{1, t0.Add(-120 * time.Hour), t0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("board = %+v\nwant %+v", got, want)
	}
}
