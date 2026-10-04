package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMigrationV3ToV4Indexes builds a registry the v3 binary wrote (0001-0003,
// user_version 3) with some rows and reopens it with Open: the lookup indexes
// appear, the data stays, and the hot queries use them.
func TestMigrationV3ToV4Indexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	for _, q := range []string{
		migrations[0].sql,
		migrations[1].sql,
		migrations[2].sql,
		"PRAGMA user_version = 3",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '` + ts + `')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '` + ts + `', 'reviewing', 'talkable-app', '` + ts + `', '` + ts + `')`,
		`INSERT INTO sessions (id, pr_id, role, generation, agent_kind, env_json, state, started_at)
		 VALUES (10, 1, 'codex-judge', 1, 'codex', '{}', 'live', '` + ts + `')`,
		`INSERT INTO runs (id, pr_id, round, role, session_id, kind, target_sha, identity, reviewer_login, state, prompt_text, created_at)
		 VALUES ('r-a', 1, 1, 'codex-judge', 10, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'p', '` + ts + `')`,
		`INSERT INTO requests (id, kind, state, created_at) VALUES (1, 'kick', 'pending', '` + ts + `')`,
		`INSERT INTO events (id, at, level, subject, kind, message) VALUES (1, '` + ts + `', 'info', 'pr:1', 'k', 'm')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v3 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v3 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 4 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}

	want := map[string]struct{ table, cols string }{
		"runs_pr_created":        {"runs", "pr_id,created_at"},
		"sessions_pr_state":      {"sessions", "pr_id,state"},
		"requests_state_created": {"requests", "state,created_at"},
		"events_subject_id":      {"events", "subject,id"},
	}
	for name, w := range want {
		var table string
		var cols string
		err := st.DB().QueryRowContext(ctx, `SELECT m.tbl_name, (SELECT group_concat(name, ',') FROM
			(SELECT name FROM pragma_index_info(?1) ORDER BY seqno)) FROM sqlite_master m WHERE m.type = 'index' AND m.name = ?1`, name).Scan(&table, &cols)
		if err != nil || table != w.table || cols != w.cols {
			t.Fatalf("index %s = table %q cols %q, %v; want %s(%s)", name, table, cols, err, w.table, w.cols)
		}
	}

	// Data survived and the hot lookups are served by the new indexes.
	if runs, err := st.RunsByPR(ctx, 1); err != nil || len(runs) != 1 {
		t.Fatalf("RunsByPR = %v, %v", runs, err)
	}
	plans := []struct{ index, query string }{
		{"runs_pr_created", "SELECT * FROM runs WHERE pr_id = 1 ORDER BY created_at, rowid"},
		{"sessions_pr_state", "SELECT * FROM sessions WHERE pr_id = 1 AND state IN ('starting', 'live')"},
		{"requests_state_created", "SELECT * FROM requests WHERE state = 'pending' ORDER BY id"},
		{"events_subject_id", "SELECT * FROM events WHERE subject = 'pr:1' ORDER BY id DESC"},
	}
	for _, p := range plans {
		rows, err := st.DB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+p.query)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		rows.Close()
		if plan := strings.Join(details, "; "); !strings.Contains(plan, p.index) {
			t.Errorf("%s\nplan: %s\nwant it to use %s", p.query, plan, p.index)
		}
	}
}

// TestMigration0004IsIdempotent applies the 0004 statements a second time:
// IF NOT EXISTS makes a re-run harmless.
func TestMigration0004IsIdempotent(t *testing.T) {
	st, _ := newStore(t)
	if _, err := st.DB().Exec(migrations[3].sql); err != nil {
		t.Fatalf("0004 applied twice: %v", err)
	}
}

func TestSchemaChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if changed, err := st.SchemaChanged(ctx); err != nil || changed {
		t.Fatalf("fresh Open: SchemaChanged = %v, %v", changed, err)
	}

	// A second handle (a newer binary) migrates the database further.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(LatestSchemaVersion()+1)); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SchemaChanged(ctx); err != nil || !changed {
		t.Fatalf("after another handle bumped user_version: SchemaChanged = %v, %v", changed, err)
	}

	// Back to what this Store saw at Open: unchanged again.
	if _, err := other.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(LatestSchemaVersion())); err != nil {
		t.Fatal(err)
	}
	if changed, _ := st.SchemaChanged(ctx); changed {
		t.Fatal("SchemaChanged stays true after the version returned to the opened one")
	}
}

// TestOpenCurrentSchemaDoesNotTakeTheWriteLock holds the write lock from a
// second handle and opens the same database: with the schema current Open
// only reads, so it must not wait out the 5 s busy timeout.
func TestOpenCurrentSchemaDoesNotTakeTheWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	ctx := context.Background()
	holder, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")

	start := time.Now()
	second, err := Open(path)
	if err != nil {
		t.Fatalf("Open while another handle holds the write lock: %v", err)
	}
	defer second.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Open waited %v for the write lock", d)
	}
	if v, err := second.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, %v", v, err)
	}
}
