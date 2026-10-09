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

// TestMigrationV15ToV16AddsPRFiles builds a registry the v15 binary would
// have written (user_version 15) with an open and a merged PR whose Details
// were fetched, and reopens it with Open: pr_files exists and is empty, and
// the open PR's details_at is cleared so the next poll fetches its Details
// (and its file list) once, while the merged PR keeps its own.
func TestMigrationV15ToV16AddsPRFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var setup []string
	for _, m := range migrations[:15] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 15",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, gh_state, details_at, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 'OPEN', '`+ts+`', '`+ts+`', '`+ts+`'),
		        (2, 1, 'PR_2', 8, 'u8', 'h2', '`+ts+`', 'closed', 'talkable-app', 'MERGED', '`+ts+`', '`+ts+`', '`+ts+`')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v15 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v15 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 16 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT count(*) FROM pr_files").Scan(&n); err != nil || n != 0 {
		t.Fatalf("pr_files rows = %d, %v", n, err)
	}
	open, _ := st.PRByID(ctx, 1)
	merged, _ := st.PRByID(ctx, 2)
	if open.DetailsAt != nil || merged.DetailsAt == nil {
		t.Fatalf("details_at: open %v, merged %v; want the open PR's cleared only", open.DetailsAt, merged.DetailsAt)
	}
	if _, ok, err := st.PRFilesOf(ctx, 1); ok || err != nil {
		t.Fatalf("PRFilesOf before any poll = %v, %v", ok, err)
	}
}

// A PR's file list is written with the Details that carry it when the PR has
// none or its head moved, and only then: a later list for the same head (a
// Details refresh for a label, a check, a review) leaves it, and an upsert
// without a list keeps it. Writing it never counts as a GitHub change.
func TestUpsertStoresTheFileListOnlyWhenTheHeadMoves(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_7", Number: 7, URL: "https://github.com/talkable/talkable/pull/7", HeadSHA: "h1",
		GHState: GHOpen, InitialState: PRQueued, Identity: "talkable-app"}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	id := res.PR.ID
	if _, ok, _ := st.PRFilesOf(ctx, id); ok {
		t.Fatal("a PR upserted without a list has one")
	}
	check := func(when string, want PRFiles) {
		t.Helper()
		got, ok, err := st.PRFilesOf(ctx, id)
		if err != nil || !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: PRFilesOf = %+v, %v, %v; want %+v", when, got, ok, err, want)
		}
	}

	// The first list for a known head (the migration's refetch): stored,
	// and nothing GitHub-derived changed.
	in.Files = &PRFiles{HeadSHA: "h1", Paths: []string{"app/models/order.rb", "spec/models/order_spec.rb"}}
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed {
		t.Fatalf("first list: changed %v, %v", res.Changed, err)
	}
	check("first list", PRFiles{HeadSHA: "h1", Paths: []string{"app/models/order.rb", "spec/models/order_spec.rb"}, FetchedAt: t0})

	// Another list for the same head is not written.
	clk.Add(time.Minute)
	in.Files = &PRFiles{HeadSHA: "h1", Paths: []string{"app/models/order.rb"}, Truncated: true}
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("same head", PRFiles{HeadSHA: "h1", Paths: []string{"app/models/order.rb", "spec/models/order_spec.rb"}, FetchedAt: t0})

	// A new head without a list (its Details failed) keeps the old one.
	in.HeadSHA, in.Files = "h2", nil
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("no list", PRFiles{HeadSHA: "h1", Paths: []string{"app/models/order.rb", "spec/models/order_spec.rb"}, FetchedAt: t0})

	// The new head's list replaces it, truncated flag and all.
	in.Files = &PRFiles{HeadSHA: "h2", Paths: []string{"app/a.rb"}, Truncated: true}
	if _, err = st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	check("new head", PRFiles{HeadSHA: "h2", Paths: []string{"app/a.rb"}, Truncated: true, FetchedAt: t0.Add(time.Minute)})

	// A PR inserted with its list has it at once; an empty list is a list.
	res, err = st.UpsertPRFromGitHub(ctx, GitHubPR{RepoID: repo.ID, NodeID: "PR_8", Number: 8, URL: "u8", HeadSHA: "h8",
		GHState: GHOpen, InitialState: PRQueued, Identity: "talkable-app", Files: &PRFiles{HeadSHA: "h8"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := st.PRFilesOf(ctx, res.PR.ID); err != nil || !ok || got.HeadSHA != "h8" || got.Paths == nil || len(got.Paths) != 0 {
		t.Fatalf("new PR's empty list = %#v, %v, %v", got, ok, err)
	}
}

// filesPR upserts PR number of repo with a file list at its head, then sets
// its GitHub state.
func filesPR(t *testing.T, st *Store, repoID int64, number int, draft bool, ghState string, mergedAt *time.Time, paths ...string) PR {
	t.Helper()
	ctx := context.Background()
	n := strconv.Itoa(number)
	in := GitHubPR{RepoID: repoID, NodeID: "PR_" + strconv.FormatInt(repoID, 10) + "_" + n, Number: number,
		URL: "https://github.com/talkable/talkable/pull/" + n, HeadSHA: "sha-" + n, IsDraft: draft, GHState: GHOpen,
		InitialState: PRQueued, Identity: "talkable-app"}
	if paths != nil {
		in.Files = &PRFiles{HeadSHA: "sha-" + n, Paths: paths, Truncated: number == 13}
	}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if ghState != GHOpen {
		if err := st.UpdatePR(ctx, res.PR.ID, func(u *PRUpdate) {
			u.Set("gh_state", ghState)
			if mergedAt != nil {
				u.Set("merged_at", *mergedAt)
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	pr, err := st.PRByID(ctx, res.PR.ID)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

// The related PRs' candidates are the repository's PRs with a file list
// that are open (drafts too) or merged since the lookback's start: not the
// PR itself, not one merged earlier, closed without a merge, without a list
// or in another repository.
func TestPRsWithFilesListsOpenAndRecentlyMergedPRs(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	other, err := st.UpsertRepo(ctx, Repo{NodeID: "R_2", Owner: "talkable", Name: "widgets", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	since := t0.Add(-14 * 24 * time.Hour)
	self := filesPR(t, st, repo.ID, 10, false, GHOpen, nil, "app/x.rb")
	filesPR(t, st, repo.ID, 11, true, GHOpen, nil, "app/x.rb")                             // an open draft
	filesPR(t, st, repo.ID, 12, false, GHMerged, new(t0.Add(-3*24*time.Hour)), "app/x.rb") // merged within
	filesPR(t, st, repo.ID, 13, false, GHMerged, new(since), "app/x.rb", "app/y.rb")       // merged right at the start
	filesPR(t, st, repo.ID, 14, false, GHMerged, new(since.Add(-time.Second)), "app/x.rb") // merged before it
	filesPR(t, st, repo.ID, 15, false, GHClosed, nil, "app/x.rb")                          // closed unmerged
	filesPR(t, st, repo.ID, 16, false, GHOpen, nil)                                        // no list
	filesPR(t, st, other.ID, 17, false, GHOpen, nil, "app/x.rb")                           // another repository
	got, err := st.PRsWithFiles(ctx, repo.ID, self.ID, since, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var sum []string
	for _, p := range got {
		s := strconv.Itoa(p.Number) + ":" + p.GHState
		if p.IsDraft {
			s += ":draft"
		}
		if p.MergedAt != nil {
			s += ":" + p.MergedAt.Format("01-02")
		}
		if p.Files.Truncated {
			s += ":truncated"
		}
		sum = append(sum, s)
	}
	want := []string{"11:OPEN:draft", "12:MERGED:09-30", "13:MERGED:09-19:truncated"}
	if !reflect.DeepEqual(sum, want) {
		t.Fatalf("PRsWithFiles = %v\nwant %v", sum, want)
	}
	if p := got[2]; p.URL != "https://github.com/talkable/talkable/pull/13" || p.Files.HeadSHA != "sha-13" ||
		!reflect.DeepEqual(p.Files.Paths, []string{"app/x.rb", "app/y.rb"}) {
		t.Fatalf("#13 = %+v", p)
	}
}

// An open PR without activity since activeSince is no candidate: its
// activity is the board's UPDATED (prs.activity_at, else GitHub's
// updatedAt), and an open PR with neither stays. A merged PR keeps the
// merge rule whatever its activity.
func TestPRsWithFilesLeavesOutOpenPRsIdleSinceActiveSince(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	activeSince := t0.Add(-30 * 24 * time.Hour)
	self := filesPR(t, st, repo.ID, 10, false, GHOpen, nil, "app/x.rb")
	set := func(p PR, col string, at time.Time) {
		t.Helper()
		if err := st.UpdatePR(ctx, p.ID, func(u *PRUpdate) { u.Set(col, at) }); err != nil {
			t.Fatal(err)
		}
	}
	set(filesPR(t, st, repo.ID, 11, false, GHOpen, nil, "app/x.rb"), "activity_at", t0.Add(-31*24*time.Hour)) // idle 31 days
	set(filesPR(t, st, repo.ID, 12, true, GHOpen, nil, "app/x.rb"), "activity_at", t0.Add(-29*24*time.Hour))  // a draft idle 29 days
	idleUpdated := filesPR(t, st, repo.ID, 13, false, GHOpen, nil, "app/x.rb")
	set(idleUpdated, "gh_updated_at", t0.Add(-40*24*time.Hour)) // no activity_at yet: GitHub's updatedAt
	both := filesPR(t, st, repo.ID, 14, false, GHOpen, nil, "app/x.rb")
	set(both, "gh_updated_at", t0.Add(-40*24*time.Hour))
	set(both, "activity_at", t0.Add(-time.Hour))                // activity_at wins over updatedAt
	filesPR(t, st, repo.ID, 15, false, GHOpen, nil, "app/x.rb") // activity unknown
	merged := filesPR(t, st, repo.ID, 16, false, GHMerged, new(t0.Add(-24*time.Hour)), "app/x.rb")
	set(merged, "activity_at", t0.Add(-60*24*time.Hour))
	got, err := st.PRsWithFiles(ctx, repo.ID, self.ID, t0.Add(-14*24*time.Hour), activeSince)
	if err != nil {
		t.Fatal(err)
	}
	var sum []string
	for _, p := range got {
		s := strconv.Itoa(p.Number)
		if p.ActivityAt != nil {
			s += ":" + p.ActivityAt.Format("01-02T15")
		}
		sum = append(sum, s)
	}
	if want := []string{"12:09-04T12", "14:10-03T11", "15", "16:08-04T12"}; !reflect.DeepEqual(sum, want) {
		t.Fatalf("PRsWithFiles = %v\nwant %v", sum, want)
	}
}

// PostedFindingPaths counts the findings magnum posted per path, over every
// round of each PR; rejected candidates and findings without a path do not
// count.
func TestPostedFindingPathsCountsPostedFindings(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	a := mustPR(t, st, repo.ID, 7, PRReviewed)
	b := mustPR(t, st, repo.ID, 8, PRReviewed)
	c := mustPR(t, st, repo.ID, 9, PRReviewed)
	for i, pr := range []PR{a, a, b} {
		run := mustRunAt(t, st, pr.ID, i+1, RoleJudge, time.Duration(i)*time.Minute).ID
		var fs []Finding
		switch i {
		case 0:
			fs = []Finding{{FindingID: "F1", Path: "app/x.rb", Verdict: FindingPosted}, {FindingID: "F2", Path: "app/x.rb", Verdict: FindingRejected},
				{FindingID: "F3", Path: "app/y.rb", Verdict: FindingPosted}, {FindingID: "F4", Verdict: FindingPosted}}
		case 1:
			fs = []Finding{{FindingID: "F1", Path: "app/x.rb", Verdict: FindingPosted}}
		case 2:
			fs = []Finding{{FindingID: "F1", Path: "app/z.rb", Verdict: FindingRejected}}
		}
		if err := st.RecordFindings(ctx, run, pr.ID, i+1, fs); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.PostedFindingPaths(ctx, []int64{a.ID, b.ID, c.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]map[string]int{a.ID: {"app/x.rb": 2, "app/y.rb": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PostedFindingPaths = %v, want %v", got, want)
	}
	if got, err := st.PostedFindingPaths(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no PRs = %v, %v", got, err)
	}
}
