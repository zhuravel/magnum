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

// TestMigrationV25ToV26AcceptsWhiteboardRuns builds a registry the v25
// binary would have written (user_version 25, foreign keys on) with runs in
// gapped rowid order, a fix run among them, a finding with its title and
// nearby mark, and declines that reference a run and that finding, and
// reopens it with Open: the runs table takes the whiteboard job's run (kind
// whiteboard) and keeps every row, its rowid order and its indexes, and
// findings and declines keep their rows, columns, links, foreign keys and
// indexes (declines references both rebuilt tables, so a plain rebuild of
// runs would fail on its rows or null the finding link).
func TestMigrationV25ToV26AcceptsWhiteboardRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	ts1 := FormatTime(t0.Add(time.Minute))
	var setup []string
	for _, m := range migrations[:25] {
		setup = append(setup, m.sql)
	}
	setup = append(setup, "PRAGMA user_version = 25",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, simplify_done, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewing', 'talkable-app', 0, '`+ts+`', '`+ts+`')`,
		`INSERT INTO sessions (id, pr_id, role, generation, agent_name, agent_kind, env_json, state, started_at)
		 VALUES (10, 1, 'codex-judge', 1, 'mg-talkable-7-judge', 'codex', '{}', 'live', '`+ts+`')`,
		`INSERT INTO runs (rowid, id, pr_id, round, role, session_id, kind, target_sha, identity, reviewer_login, state, outcome, prompt_text, created_at)
		 VALUES (7, 'r-b', 1, 1, 'codex-judge', 10, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'posted', 'p', '`+ts+`'),
		        (9, 'r-a', 1, 1, 'claude-review', NULL, 'own_pass', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'ok', 'p', '`+ts+`'),
		        (3, 'r-c', 1, 1, 'codex-judge', 10, 'nudge', 'h1', 'talkable-app', 'talkable[bot]', 'working', NULL, 'p', '`+ts1+`'),
		        (12, 'r-f', 1, 2, 'claude-fix', NULL, 'fix', 'h1', 'zhuravel', 'zhuravel', 'ended', 'pushed', 'p', '`+ts1+`')`,
		`INSERT INTO findings (id, run_id, pr_id, round, finding_id, severity, path, line, verdict, created_at, title, nearby)
		 VALUES (4, 'r-b', 1, 1, 'F1', 'P2', 'app/models/order.rb', 42, 'posted', '`+ts+`', 'Order total ignores refunds', 1),
		        (8, 'r-b', 1, 1, 'F2', NULL, NULL, NULL, 'rejected', '`+ts+`', NULL, 0)`,
		`INSERT INTO declines (id, pr_id, source_url, comment_id, review_id, run_id, finding_row_id, path, line, title, priority,
		   reviewed_sha, skill_sha, outcome, verdict, lesson, scope, match_json, state, created_at, updated_at)
		 VALUES (5, 1, 'https://github.com/talkable/talkable/pull/7#discussion_r1', 1, 11, 'r-b', 4, 'app/models/order.rb', 42,
		   'Order total ignores refunds', 'P2', 'h1', 'sk1', 'declined', 'wrong', 'a lesson', 'repo', '["refund"]', 'used', '`+ts+`', '`+ts1+`'),
		        (6, 1, 'https://github.com/talkable/talkable/pull/7#discussion_r2', 2, NULL, NULL, NULL, NULL, NULL,
		   NULL, NULL, 'h1', NULL, 'fixed', NULL, NULL, NULL, '[]', 'new', '`+ts+`', '`+ts+`')`,
	)
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v25 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v25 database: %v", err)
	}
	defer st.Close()
	st.Clock = func() time.Time { return t0.Add(time.Hour) }
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 26 {
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
	want := []string{"r-b:initial:verified:posted", "r-a:own_pass:verified:ok", "r-c:nudge:working:", "r-f:fix:ended:pushed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runs after 0026 =\n%v\nwant (rowid order kept)\n%v", got, want)
	}
	var rowids string
	st.DB().QueryRowContext(ctx, "SELECT group_concat(id || '=' || rowid, ',') FROM (SELECT id, rowid FROM runs ORDER BY id)").Scan(&rowids)
	if rowids != "r-a=9,r-b=7,r-c=3,r-f=12" {
		t.Fatalf("run rowids = %s", rowids)
	}
	if Deref(runs[0].SessionID) != 10 || runs[1].SessionID != nil || runs[3].SessionID != nil {
		t.Fatalf("run session ids changed: %v %v %v", runs[0].SessionID, runs[1].SessionID, runs[3].SessionID)
	}

	// The whiteboard job's run is a run kind now; an unknown kind still is not.
	wb, err := st.CreateRun(ctx, Run{ID: "r-wb", PRID: 1, Round: 2, Role: "claude-whiteboard", Kind: RunWhiteboard, TargetSHA: "h2",
		Identity: "zhuravel", ReviewerLogin: "zhuravel", State: RunPending})
	if err != nil || wb.Kind != RunWhiteboard || RunWhiteboard != "whiteboard" {
		t.Fatalf("create whiteboard run = %+v, %v", wb, err)
	}
	back, err := st.RunByID(ctx, "r-wb")
	if err != nil || back.Kind != "whiteboard" || back.Role != "claude-whiteboard" || back.TargetSHA != "h2" {
		t.Fatalf("whiteboard run read back = %+v, %v", back, err)
	}
	if fix, err := st.CreateRun(ctx, Run{ID: "r-fix2", PRID: 1, Round: 3, Role: "claude-fix", Kind: RunFix, TargetSHA: "h2",
		Identity: "zhuravel", ReviewerLogin: "zhuravel", State: RunPending}); err != nil || fix.Kind != RunFix {
		t.Fatalf("a fix run after 0026 = %+v, %v", fix, err)
	}
	if _, err := st.CreateRun(ctx, Run{ID: "r-bad", PRID: 1, Round: 2, Role: RoleJudge, Kind: "bogus", TargetSHA: "h2",
		Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending}); err == nil {
		t.Fatal("a run of an unknown kind was accepted")
	}

	// findings keeps its rows, ids and columns (title and nearby came with 0021).
	var finding string
	if err := st.DB().QueryRowContext(ctx, `SELECT group_concat(id || ':' || run_id || ':' || finding_id || ':' || COALESCE(path, '-') || ':' ||
		COALESCE(title, '-') || ':' || nearby, ',') FROM (SELECT * FROM findings ORDER BY id)`).Scan(&finding); err != nil ||
		finding != "4:r-b:F1:app/models/order.rb:Order total ignores refunds:1,8:r-b:F2:-:-:0" {
		t.Fatalf("findings after 0026 = %q, %v", finding, err)
	}
	// declines keeps its rows, ids and links: the finding link was not nulled.
	var decline string
	if err := st.DB().QueryRowContext(ctx, `SELECT group_concat(id || ':' || COALESCE(run_id, '-') || ':' || COALESCE(finding_row_id, '-') || ':' ||
		COALESCE(title, '-') || ':' || outcome || ':' || COALESCE(verdict, '-') || ':' || state || ':' || updated_at, ',')
		FROM (SELECT * FROM declines ORDER BY id)`).Scan(&decline); err != nil ||
		decline != "5:r-b:4:Order total ignores refunds:declined:wrong:used:"+ts1+",6:-:-:-:fixed:-:new:"+ts {
		t.Fatalf("declines after 0026 = %q, %v", decline, err)
	}

	// The foreign keys still point at the rebuilt tables, not at a temp name.
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO findings (run_id, pr_id, round, finding_id, verdict, created_at)
		VALUES ('r-missing', 1, 1, 'F9', 'posted', '`+ts+`')`); err == nil {
		t.Fatal("a finding of a missing run was accepted: the foreign key is gone")
	}
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO declines (pr_id, source_url, comment_id, run_id, reviewed_sha, outcome, created_at, updated_at)
		VALUES (1, 'u-missing', 3, 'r-missing', 'h1', 'fixed', '`+ts+`', '`+ts+`')`); err == nil {
		t.Fatal("a decline of a missing run was accepted: the foreign key is gone")
	}
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO declines (pr_id, source_url, comment_id, outcome, reviewed_sha, created_at, updated_at)
		VALUES (1, 'u-bad', 3, 'bogus', 'h1', '`+ts+`', '`+ts+`')`); err == nil {
		t.Fatal("a decline of an unknown outcome was accepted: the CHECK is gone")
	}
	// ON DELETE SET NULL survives: deleting the finding clears the link and keeps the decline.
	if _, err := st.DB().ExecContext(ctx, "DELETE FROM findings WHERE id = 4"); err != nil {
		t.Fatalf("delete a finding a decline links to: %v", err)
	}
	var link sql.NullInt64
	if err := st.DB().QueryRowContext(ctx, "SELECT finding_row_id FROM declines WHERE id = 5").Scan(&link); err != nil || link.Valid {
		t.Fatalf("decline 5 link after its finding was deleted = %v, %v", link, err)
	}
	var violations int
	if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check: %v", err) // a failing query must not pass as no violation
	}
	if violations != 0 {
		t.Fatalf("foreign_key_check reports %d violations after 0026", violations)
	}

	for _, tbl := range []string{"runs", "findings", "declines"} {
		var ddl string
		if err := st.DB().QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", tbl).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "_new") || (tbl == "runs" && (!strings.Contains(ddl, "'whiteboard'") || !strings.Contains(ddl, "'fix'"))) {
			t.Fatalf("%s DDL:\n%s", tbl, ddl)
		}
	}
	wantIdx := map[string]bool{"runs_active": true, "runs_pr_created": false, "runs_created": false,
		"findings_created": false, "findings_pr": false, "declines_pr": false, "declines_state": false, "declines_skill": false}
	for _, tbl := range []string{"runs", "findings", "declines"} {
		for name, partial := range indexesOf(t, st, tbl) {
			if w, ok := wantIdx[name]; !ok || w != partial {
				t.Fatalf("index %s on %s: partial %v (known %v)", name, tbl, partial, ok)
			}
			delete(wantIdx, name)
		}
	}
	if len(wantIdx) != 0 {
		t.Fatalf("indexes missing after 0026: %v", wantIdx)
	}
}
