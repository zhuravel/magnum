package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMigrationV14ToV15AcceptsOwnPassRuns builds a registry the v14 binary
// would have written (user_version 14, foreign keys on) with runs in gapped
// rowid order and a finding that references one, and reopens it with Open:
// the runs table takes the judge's own pass (kind own_pass), keeps every row,
// its rowid order and its indexes, and findings still references runs.
func TestMigrationV14ToV15AcceptsOwnPassRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	ts1 := FormatTime(t0.Add(time.Minute))
	var setup []string
	for _, m := range migrations[:14] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 14",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, simplify_done, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewing', 'talkable-app', 0, '`+ts+`', '`+ts+`')`,
		`INSERT INTO sessions (id, pr_id, role, generation, agent_name, agent_kind, env_json, state, started_at)
		 VALUES (10, 1, 'codex-judge', 1, 'mg-talkable-7-judge', 'codex', '{}', 'live', '`+ts+`')`,
		`INSERT INTO runs (rowid, id, pr_id, round, role, session_id, kind, target_sha, identity, reviewer_login, state, outcome, prompt_text, created_at)
		 VALUES (7, 'r-b', 1, 1, 'codex-judge', 10, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'posted', 'p', '`+ts+`'),
		        (9, 'r-a', 1, 1, 'claude-review', NULL, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'ok', 'p', '`+ts+`'),
		        (3, 'r-c', 1, 1, 'codex-judge', 10, 'nudge', 'h1', 'talkable-app', 'talkable[bot]', 'working', NULL, 'p', '`+ts1+`')`,
		`INSERT INTO findings (run_id, pr_id, round, finding_id, severity, path, line, verdict, created_at)
		 VALUES ('r-b', 1, 1, 'F1', 'P2', 'app/models/order.rb', 42, 'posted', '`+ts+`')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v14 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v14 database: %v", err)
	}
	defer st.Close()
	st.Clock = func() time.Time { return t0.Add(time.Hour) }
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 15 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}

	runs, err := st.RunsByPR(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.ID+":"+r.Kind+":"+r.State+":"+Deref(r.Outcome))
	}
	want := []string{"r-b:initial:verified:posted", "r-a:initial:verified:ok", "r-c:nudge:working:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runs after 0015 =\n%v\nwant (rowid order kept)\n%v", got, want)
	}
	var rowids string
	st.DB().QueryRowContext(ctx, "SELECT group_concat(id || '=' || rowid, ',') FROM (SELECT id, rowid FROM runs ORDER BY id)").Scan(&rowids)
	if rowids != "r-a=9,r-b=7,r-c=3" {
		t.Fatalf("run rowids = %s", rowids)
	}
	if Deref(runs[0].SessionID) != 10 || runs[1].SessionID != nil {
		t.Fatalf("run session ids changed: %v %v", runs[0].SessionID, runs[1].SessionID)
	}
	var finding string
	if err := st.DB().QueryRowContext(ctx, "SELECT run_id || ':' || finding_id || ':' || path FROM findings").Scan(&finding); err != nil || finding != "r-b:F1:app/models/order.rb" {
		t.Fatalf("finding after 0015 = %q, %v", finding, err)
	}

	// The judge's own pass is a run kind now; an unknown kind still is not.
	own, err := st.CreateRun(ctx, Run{ID: "r-own", PRID: 1, Round: 2, Role: RoleJudge, Kind: RunOwnPass, TargetSHA: "h2",
		Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending})
	if err != nil || own.Kind != RunOwnPass {
		t.Fatalf("create own_pass run = %+v, %v", own, err)
	}
	if _, err := st.CreateRun(ctx, Run{ID: "r-bad", PRID: 1, Round: 2, Role: RoleJudge, Kind: "bogus", TargetSHA: "h2",
		Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending}); err == nil {
		t.Fatal("a run of an unknown kind was accepted")
	}
	// findings still references runs (the rebuilt table, not a temp name).
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO findings (run_id, pr_id, round, finding_id, verdict, created_at)
		VALUES ('r-missing', 1, 1, 'F9', 'posted', '`+ts+`')`); err == nil {
		t.Fatal("a finding of a missing run was accepted: the foreign key is gone")
	}
	for _, tbl := range []string{"runs", "findings"} {
		var ddl string
		if err := st.DB().QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", tbl).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "_new") || (tbl == "runs" && !strings.Contains(ddl, "'own_pass'")) {
			t.Fatalf("%s DDL:\n%s", tbl, ddl)
		}
	}
	wantIdx := map[string]bool{"runs_active": true, "runs_pr_created": false, "runs_created": false,
		"findings_created": false, "findings_pr": false}
	for _, tbl := range []string{"runs", "findings"} {
		rows, err := st.DB().QueryContext(ctx, "SELECT name, partial FROM pragma_index_list(?) WHERE origin = 'c'", tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var partial bool
			rows.Scan(&name, &partial)
			if w, ok := wantIdx[name]; !ok || w != partial {
				t.Fatalf("index %s on %s: partial %v (known %v)", name, tbl, partial, ok)
			}
			delete(wantIdx, name)
		}
		rows.Close()
	}
	if len(wantIdx) != 0 {
		t.Fatalf("indexes missing after 0015: %v", wantIdx)
	}
}
