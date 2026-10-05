package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

// TestMigrationV8ToV9ReviewRequests reopens a registry the previous binary
// wrote (user_version 8): every PR gets an empty review_requests_json, and
// every open PR fetches its Details again, so an older PR gets its request
// history; a closed or merged one keeps its details_at.
func TestMigrationV8ToV9ReviewRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:8] {
		stmts = append(stmts, m.sql)
	}
	pr := func(id int64, ghState string) string {
		return `INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, state, identity, created_at, updated_at,
		 gh_state, details_at)
		 VALUES (` + itoa(id) + `, 1, 'PR_` + itoa(id) + `', ` + itoa(id) + `, 'u', 't', 'h1', '` + ts + `', 'reviewed', 'app', '` + ts + `', '` + ts + `',
		 '` + ghState + `', '` + ts + `')`
	}
	stmts = append(stmts, "PRAGMA user_version = 8",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		pr(1, "OPEN"), pr(2, "MERGED"), pr(3, "CLOSED"))
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v8 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v8 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 9 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	var def string
	err = st.DB().QueryRowContext(ctx, "SELECT dflt_value FROM pragma_table_info('prs') WHERE name = 'review_requests_json' AND \"notnull\" = 1").Scan(&def)
	if err != nil || def != "'[]'" {
		t.Fatalf("review_requests_json column: default %q, %v; want a NOT NULL column defaulting to '[]'", def, err)
	}
	for id, wantDetails := range map[int64]bool{1: false, 2: true, 3: true} {
		got, err := st.PRByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.ReviewRequests, []ReviewRequest{}) {
			t.Errorf("pr %d: review requests = %#v, want an empty list", id, got.ReviewRequests)
		}
		if (got.DetailsAt != nil) != wantDetails {
			t.Errorf("pr %d (%s): details_at %v, want kept = %v", id, got.GHState, got.DetailsAt, wantDetails)
		}
	}
	board, err := st.Board(ctx, BoardFilter{})
	if err != nil || len(board) != 1 || !reflect.DeepEqual(board[0].ReviewRequests, []ReviewRequest{}) {
		t.Fatalf("board on a migrated registry = %+v, %v", board, err)
	}
}
