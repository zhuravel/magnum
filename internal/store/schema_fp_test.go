package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Migration 0012 gives slots the schema their databases were last loaded
// from (schema_fp, schema_sha, schema_version). A slot the previous binary
// wrote (user_version 11) keeps its row and reads as unknown (all NULL), so
// its next round reloads; the columns then take writes like any other.
func TestMigrationV11ToV12RecordsTheSlotSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:11] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 11",
		`INSERT INTO slots (id, name, repo_full_name, kind, path, main_clone, state, dirty_schema, created_at, updated_at)
		 VALUES (1, 'review1', 'talkable/talkable', 'pool', '/tmp/r1', '/tmp/main', 'free', 1, '`+ts+`', '`+ts+`')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v11 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v11 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 12 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	sl, err := st.SlotByName(ctx, "review1")
	if err != nil {
		t.Fatal(err)
	}
	if sl.SchemaFP != nil || sl.SchemaSHA != nil || sl.SchemaVersion != nil || !sl.DirtySchema || sl.State != SlotFree {
		t.Fatalf("migrated slot = fp %v sha %v version %v dirty %v state %s", sl.SchemaFP, sl.SchemaSHA, sl.SchemaVersion, sl.DirtySchema, sl.State)
	}
	if err := st.UpdateSlotFields(ctx, sl.ID, func(u *SlotUpdate) {
		u.Set("schema_fp", "f1")
		u.Set("schema_sha", "abc1234")
		u.Set("schema_version", "20260914085954")
	}); err != nil {
		t.Fatal(err)
	}
	sl, _ = st.SlotByName(ctx, "review1")
	if Deref(sl.SchemaFP) != "f1" || Deref(sl.SchemaSHA) != "abc1234" || Deref(sl.SchemaVersion) != "20260914085954" {
		t.Fatalf("slot schema = %v %v %v", sl.SchemaFP, sl.SchemaSHA, sl.SchemaVersion)
	}
}
