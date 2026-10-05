package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// queryPlan returns EXPLAIN QUERY PLAN's detail lines of query, joined.
func queryPlan(t *testing.T, st *Store, query string, args ...any) string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", query, err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "; ")
}

func indexNames(t *testing.T, st *Store, table string) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), "SELECT name FROM pragma_index_list(?) ORDER BY name", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

// events_subject(subject, at) and prs_gh_updated were never chosen by any
// query: events_subject_id serves every events lookup (it also gives the
// order), and the board and dispatch orderings start with an expression, so no
// index on gh_updated_at can serve them. Each cost a write per row. Migration
// 0011 drops both from a registry the previous binary wrote (user_version
// 10), keeps the data and the indexes the queries use, and a new registry
// never has them.
func TestMigrationV10ToV11DropsTheUnusedIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:10] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 10",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, state, identity, created_at, updated_at, gh_state, gh_updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u', 't', 'h1', '`+ts+`', 'reviewed', 'app', '`+ts+`', '`+ts+`', 'OPEN', '`+ts+`')`,
		`INSERT INTO events (id, at, level, subject, kind, message) VALUES (1, '`+ts+`', 'info', 'pr:1', 'k', 'm')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v10 setup: %v\n%s", err, q)
		}
	}
	var before int // the v10 registry has both indexes: the setup is what 0010 left
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name IN ('events_subject', 'prs_gh_updated')").Scan(&before); err != nil || before != 2 {
		t.Fatalf("v10 registry has %d of the two indexes, %v", before, err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v10 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 11 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	for table, gone := range map[string]string{"events": "events_subject", "prs": "prs_gh_updated"} {
		if names := indexNames(t, st, table); slices.Contains(names, gone) {
			t.Errorf("%s still has index %s: %v", table, gone, names)
		}
	}
	for table, kept := range map[string][]string{
		"events": {"events_kind_at", "events_subject_id"},
		"prs":    {"prs_state"},
	} {
		names := indexNames(t, st, table)
		for _, k := range kept {
			if !slices.Contains(names, k) {
				t.Errorf("%s lost index %s: %v", table, k, names)
			}
		}
	}
	if evs, err := st.EventsBySubject(ctx, "pr:1", 0); err != nil || len(evs) != 1 {
		t.Fatalf("events after migration = %v, %v", evs, err)
	}
	if pr, err := st.PRByID(ctx, 1); err != nil || pr.Number != 7 {
		t.Fatalf("pr after migration = %+v, %v", pr, err)
	}
}

// A new registry is built from the same migrations, so it has neither index,
// and the queries that touch events by subject still search an index.
func TestNewRegistryHasNoUnusedIndexesAndQueriesStillUseOne(t *testing.T) {
	st, _ := newStore(t)
	if names := indexNames(t, st, "events"); slices.Contains(names, "events_subject") {
		t.Errorf("events has events_subject: %v", names)
	}
	if names := indexNames(t, st, "prs"); slices.Contains(names, "prs_gh_updated") {
		t.Errorf("prs has prs_gh_updated: %v", names)
	}
	for _, q := range []string{
		"SELECT * FROM events WHERE subject = 'pr:1' ORDER BY id DESC",
		"SELECT * FROM events WHERE (subject = 'pr:1' OR subject GLOB 'slot:a:pr:1:*') AND id > 0 ORDER BY id",
		// the prune's per-subject check: any event of the subject inside the window
		"SELECT 1 FROM events a WHERE a.subject = 'slot:a' AND a.at >= '2026-01-01'",
	} {
		if plan := queryPlan(t, st, q); !strings.Contains(plan, "events_subject_id") || strings.Contains(plan, "SCAN events") {
			t.Errorf("%s\nplan: %s\nwant a search of events_subject_id", q, plan)
		}
	}
}
