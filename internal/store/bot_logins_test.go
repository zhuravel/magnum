package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV7ToV8BotLogins reopens a registry the previous binary wrote
// (user_version 7), where last_review_login lost the "[bot]" of the App that
// posted: it takes the posting run's reviewer_login back, a review without a
// run keeps its login, and every open PR fetches its Details again.
func TestMigrationV7ToV8BotLogins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:7] {
		stmts = append(stmts, m.sql)
	}
	pr := func(id int64, ghState, login string, reviewID int64) string {
		return `INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, state, identity, created_at, updated_at,
		 gh_state, last_review_login, last_review_id, details_at)
		 VALUES (` + itoa(id) + `, 1, 'PR_` + itoa(id) + `', ` + itoa(id) + `, 'u', 't', 'h1', '` + ts + `', 'reviewed', 'app', '` + ts + `', '` + ts + `',
		 '` + ghState + `', '` + login + `', ` + itoa(reviewID) + `, '` + ts + `')`
	}
	run := func(id string, prID int64, login string, reviewID int64) string {
		return `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, review_id, prompt_text, created_at)
		 VALUES ('` + id + `', ` + itoa(prID) + `, 1, 'judge', 'initial', 'h1', 'app', '` + login + `', 'verified', ` + itoa(reviewID) + `, 'p', '` + ts + `')`
	}
	stmts = append(stmts, "PRAGMA user_version = 7",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		pr(1, "OPEN", "zhuravel", 100), run("r1", 1, "zhuravel[bot]", 100),
		pr(2, "OPEN", "zhuravel", 200), run("r2", 2, "zhuravel", 200),
		pr(3, "MERGED", "zhuravel", 300), // a manual verdict: no run
		run("r3", 3, "zhuravel[bot]", 299))
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v7 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v7 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 8 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	for id, want := range map[int64]struct {
		login   string
		details bool
	}{1: {"zhuravel[bot]", false}, 2: {"zhuravel", false}, 3: {"zhuravel", true}} {
		got, err := st.PRByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if Deref(got.LastReviewLogin) != want.login || (got.DetailsAt != nil) != want.details {
			t.Errorf("pr %d: last_review_login %q, details_at %v; want %q, kept %v", id, Deref(got.LastReviewLogin), got.DetailsAt, want.login, want.details)
		}
	}
}
