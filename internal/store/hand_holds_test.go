package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// Migration 30 lifts the holds a comment or an approval by hand made, which
// no longer stop auto-approval (2026-11-03: only a changes request by hand,
// a dismissed review of the operator's and a dismissal of one of magnum's
// approvals do), so the next pass reads the PR's reviews from the start.
// The other holds and a resumed row stay.
func TestMigrationV29ToV30LiftsTheHoldsOfACommentOrAnApprovalByHand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	holds := []struct {
		held   int
		reason string
		stays  bool
	}{
		{1, "you commented on it by hand (https://github.com/talkable/talkable/pull/2#pullrequestreview-40)", false},
		{1, "you approved it by hand (https://github.com/talkable/talkable/pull/3#pullrequestreview-41)", false},
		{1, "you requested changes by hand (https://github.com/talkable/talkable/pull/4#pullrequestreview-42)", true},
		{1, "you reviewed it by hand (https://github.com/talkable/talkable/pull/5#pullrequestreview-43)", true},
		{1, "you dismissed it (review 9001)", true},
		{1, "magnum unapprove", true},
		{0, "resumed with magnum unapprove --resume", true},
	}
	stmts := []string{}
	for _, m := range migrations[:29] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 29",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`)
	for i, h := range holds {
		id := strconv.Itoa(i + 1)
		stmts = append(stmts,
			`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, simplify_done, created_at, updated_at, gh_state)
			 VALUES (`+id+`, 1, 'PR_`+id+`', `+id+`, 'u`+id+`', 'h1', '`+ts+`', 'reviewed', 'talkable-app', 0, '`+ts+`', '`+ts+`', 'OPEN')`,
			`INSERT INTO auto_approve_holds (pr_id, held, reason, at) VALUES (`+id+`, `+strconv.Itoa(h.held)+`, '`+h.reason+`', '`+ts+`')`)
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v29 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v29 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 30 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	ids := make([]int64, len(holds))
	for i := range holds {
		ids[i] = int64(i + 1)
	}
	got, err := st.AutoApproveHolds(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range holds {
		kept, ok := got[int64(i+1)]
		if ok != h.stays || (ok && (kept.Reason != h.reason || kept.Held != (h.held == 1))) {
			t.Errorf("hold %q (held %d): after the migration %+v (present %v), want it kept %v", h.reason, h.held, kept, ok, h.stays)
		}
	}
	if !slices.ContainsFunc(migrations, func(m migration) bool { return m.version == 30 }) {
		t.Fatal("no migration 30")
	}
}
