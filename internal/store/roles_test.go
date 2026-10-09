package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMigrationV2ToV3FreeFormRoles builds a registry the v2 binary would have
// written (0001 + 0002, user_version 2, foreign keys on) with the old role ids
// and reopens it with Open.
func TestMigrationV2ToV3FreeFormRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	ts1 := FormatTime(t0.Add(time.Minute))
	for _, q := range []string{
		migrations[0].sql,
		migrations[1].sql,
		"PRAGMA user_version = 2",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '` + ts + `')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, simplify_done, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '` + ts + `', 'reviewing', 'talkable-app', 1, '` + ts + `', '` + ts + `'),
		        (2, 1, 'PR_2', 8, 'u8', 'h2', '` + ts + `', 'reviewing', 'talkable-app', 0, '` + ts + `', '` + ts + `')`,
		`INSERT INTO sessions (id, pr_id, role, generation, agent_name, agent_kind, session_id, env_json, state, started_at)
		 VALUES (10, 1, 'judge', 2, 'mg-talkable-7-judge', 'codex', 'uuid-j', '{"A":"1"}', 'live', '` + ts + `'),
		        (11, 1, 'claude', 1, 'mg-talkable-7-claude', 'claude', 'id-c', '{}', 'parked', '` + ts + `'),
		        (12, 1, 'codex_review', 1, NULL, 'codex', NULL, '{}', 'closed', '` + ts + `'),
		        (13, 1, 'simplify', 1, 'mg-talkable-7-simplify', 'claude', NULL, '{}', 'starting', '` + ts + `'),
		        (14, 2, 'judge', 1, 'mg-talkable-8-judge', 'codex', NULL, '{}', 'lost', '` + ts + `')`,
		// Gapped rowids, out of id order, equal created_at: rowid decides run
		// order and the next generated run id (MAX(rowid) + 1), so 0003 must keep it.
		`INSERT INTO runs (rowid, id, pr_id, round, role, session_id, kind, target_sha, identity, reviewer_login, state, prompt_text, created_at)
		 VALUES (7, 'r-b', 1, 1, 'judge', 10, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'p', '` + ts + `'),
		        (9, 'r-a', 1, 1, 'claude', 11, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'ended', 'p', '` + ts + `'),
		        (3, 'r-c', 1, 1, 'codex_review', 12, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'failed', 'p', '` + ts1 + `'),
		        (12, 'r-d', 1, 1, 'simplify', NULL, 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'working', 'p', '` + ts1 + `')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v2 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v2 database: %v", err)
	}
	defer st.Close()
	st.Clock = func() time.Time { return t0.Add(time.Hour) }
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 3 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}

	// Roles renamed, everything else intact.
	wantSessions := map[int64]string{10: RoleJudge, 11: RoleClaude, 12: RoleCodexReview, 13: RoleSimplify, 14: RoleJudge}
	for id, role := range wantSessions {
		x, err := st.SessionByID(ctx, id)
		if err != nil || x.Role != role {
			t.Fatalf("session %d = %+v, %v; want role %s", id, x, err, role)
		}
	}
	if j, _ := st.SessionByID(ctx, 10); j.Generation != 2 || Deref(j.AgentName) != "mg-talkable-7-judge" ||
		Deref(j.SessionID) != "uuid-j" || j.Env["A"] != "1" || j.State != SessionLive || !j.StartedAt.Equal(t0) {
		t.Fatalf("session 10 data changed: %+v", j)
	}
	runs, err := st.RunsByPR(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.ID+":"+r.Role+":"+r.State)
	}
	want := []string{"r-b:" + RoleJudge + ":verified", "r-a:" + RoleClaude + ":ended",
		"r-c:" + RoleCodexReview + ":failed", "r-d:" + RoleSimplify + ":working"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runs after 0003 =\n%v\nwant (rowid order kept)\n%v", got, want)
	}
	var rowids string
	st.DB().QueryRowContext(ctx, "SELECT group_concat(id || '=' || rowid, ',') FROM (SELECT id, rowid FROM runs ORDER BY id)").Scan(&rowids)
	if rowids != "r-a=9,r-b=7,r-c=3,r-d=12" {
		t.Fatalf("run rowids = %s", rowids)
	}
	if Deref(runs[0].SessionID) != 10 || runs[3].SessionID != nil {
		t.Fatalf("run session ids changed: %v %v", runs[0].SessionID, runs[3].SessionID)
	}
	if pr, _ := st.PRByID(ctx, 1); !pr.SimplifyDone {
		t.Fatalf("prs.simplify_done lost: %+v", pr)
	}
	var old int
	st.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sessions WHERE role IN ('judge','claude','codex_review','simplify')) +
		(SELECT count(*) FROM runs WHERE role IN ('judge','claude','codex_review','simplify'))`).Scan(&old)
	if old != 0 {
		t.Fatalf("%d rows still use old role ids", old)
	}

	// Schema: no role enum, every index back, runs still references sessions.
	for _, tbl := range []string{"sessions", "runs"} {
		var ddl string
		if err := st.DB().QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", tbl).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, "'judge'") || strings.Contains(ddl, "_new") {
			t.Fatalf("%s DDL still has the role enum or a temp name:\n%s", tbl, ddl)
		}
	}
	type idx struct {
		unique, partial bool
	}
	// 0004 adds the two plain lookup indexes below and 0005 runs_created;
	// Open applied them too.
	wantIdx := map[string]idx{"sessions_live_role": {true, true}, "sessions_live_agent": {true, true}, "runs_active": {false, true},
		"sessions_pr_state": {false, false}, "runs_pr_created": {false, false}, "runs_created": {false, false}}
	for _, tbl := range []string{"sessions", "runs"} {
		rows, err := st.DB().QueryContext(ctx, "SELECT name, \"unique\", partial FROM pragma_index_list(?) WHERE origin = 'c'", tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var x idx
			if err := rows.Scan(&name, &x.unique, &x.partial); err != nil {
				t.Fatal(err)
			}
			if w, ok := wantIdx[name]; !ok || w != x {
				t.Fatalf("index %s on %s = %+v, want %+v (known %v)", name, tbl, x, w, ok)
			}
			delete(wantIdx, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if len(wantIdx) != 0 {
		t.Fatalf("indexes missing after 0003: %v", wantIdx)
	}
	var fkTable string
	if err := st.DB().QueryRowContext(ctx, "SELECT \"table\" FROM pragma_foreign_key_list('runs') WHERE \"from\" = 'session_id'").Scan(&fkTable); err != nil || fkTable != "sessions" {
		t.Fatalf("runs.session_id references %q, %v; want sessions", fkTable, err)
	}
	if rows, err := st.DB().QueryContext(ctx, "PRAGMA foreign_key_check"); err != nil {
		t.Fatal(err)
	} else {
		bad := rows.Next()
		err := rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bad {
			t.Fatal("foreign_key_check reports violations after 0003")
		}
	}
	if _, err := st.CreateRun(ctx, Run{PRID: 1, Round: 2, Role: RoleJudge, SessionID: new(int64(999)), Kind: RunRereview,
		TargetSHA: "h1", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending, PromptText: "p"}); err == nil {
		t.Fatal("a run pointing at a missing session must fail the foreign key")
	}

	// The partial unique indexes still allow one live session per (pr, role).
	if _, err := st.CreateSession(ctx, Session{PRID: 1, Role: RoleJudge, State: SessionLive}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second live %s on a migrated PR: %v", RoleJudge, err)
	}
	if _, err := st.CreateSession(ctx, Session{PRID: 1, Role: RoleSimplify, State: SessionStarting}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second starting %s on a migrated PR: %v", RoleSimplify, err)
	}
	if _, err := st.CreateSession(ctx, Session{PRID: 1, Role: "droid-simplify", AgentName: new("mg-talkable-7-judge"), State: SessionLive}); !errors.Is(err, ErrConflict) {
		t.Fatalf("live agent name reused: %v", err)
	}
	live, err := st.LiveSessionByPRRole(ctx, 1, RoleJudge)
	if err != nil || live.ID != 10 {
		t.Fatalf("LiveSessionByPRRole(%s) = %+v, %v", RoleJudge, live, err)
	}
	resumed, err := st.CreateSession(ctx, Session{PRID: 1, Role: RoleClaude, State: SessionLive})
	if err != nil || resumed.Generation != 2 {
		t.Fatalf("live %s next to its parked generation: %+v, %v", RoleClaude, resumed, err)
	}

	// Free-form roles from config work for sessions and runs.
	droid, err := st.CreateSession(ctx, Session{PRID: 1, Role: "droid-simplify", AgentName: new("mg-talkable-7-droid-simplify"), State: SessionLive})
	if err != nil || droid.Role != "droid-simplify" || droid.Generation != 1 {
		t.Fatalf("free-form session: %+v, %v", droid, err)
	}
	if _, err := st.CreateSession(ctx, Session{PRID: 1, Role: "droid-simplify", State: SessionStarting}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second live droid-simplify: %v", err)
	}
	if _, err := st.CreateSession(ctx, Session{PRID: 2, Role: "droid-simplify", State: SessionLive}); err != nil {
		t.Fatalf("same role on another PR: %v", err)
	}
	r, err := st.CreateRun(ctx, Run{PRID: 1, Round: 2, Role: "omp-review", Kind: RunInitial, TargetSHA: "h1",
		Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending, PromptText: "p"})
	if err != nil || r.Role != "omp-review" || r.ID != "r-20261003T130000-13" {
		t.Fatalf("free-form run: %+v, %v", r, err)
	}
	if _, err := st.DB().ExecContext(ctx, "UPDATE sessions SET role = '' WHERE id = 10"); err == nil {
		t.Fatal("an empty role must violate the CHECK")
	}
}

func TestCreateRequiresRole(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 1, PRReviewing)
	if _, err := st.CreateSession(ctx, Session{PRID: pr.ID, State: SessionLive}); err == nil {
		t.Fatal("session without a role")
	}
	if _, err := st.CreateRun(ctx, Run{PRID: pr.ID, Kind: RunInitial, TargetSHA: "x", State: RunPending}); err == nil {
		t.Fatal("run without a role")
	}
}

func TestRoleRanBefore(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 1, PRReviewing)
	other := mustPR(t, st, repo.ID, 2, PRReviewing)

	ran := func(prID int64, role string) bool {
		t.Helper()
		ok, err := st.RoleRanBefore(ctx, prID, role)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	newRun := func(prID int64, role string) Run {
		t.Helper()
		r, err := st.CreateRun(ctx, Run{PRID: prID, Round: 1, Role: role, Kind: RunInitial, TargetSHA: "h",
			Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunPending, PromptText: "p"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if ran(pr.ID, "droid-simplify") {
		t.Fatal("fresh PR has no history")
	}
	r := newRun(pr.ID, "droid-simplify")
	for _, state := range []string{RunSubmitted, RunWorking, RunFailed, RunAbandoned} {
		if err := st.TransitionRun(ctx, r.ID, nil, state, nil); err != nil {
			t.Fatal(err)
		}
		if ran(pr.ID, "droid-simplify") {
			t.Fatalf("a %s run does not count", state)
		}
	}
	if err := st.TransitionRun(ctx, r.ID, nil, RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	if !ran(pr.ID, "droid-simplify") {
		t.Fatal("an ended run counts")
	}
	v := newRun(pr.ID, RoleSimplify)
	if ran(pr.ID, RoleSimplify) {
		t.Fatal("a pending run does not count")
	}
	if err := st.TransitionRun(ctx, v.ID, nil, RunVerified, nil); err != nil {
		t.Fatal(err)
	}
	if !ran(pr.ID, RoleSimplify) || ran(pr.ID, RoleJudge) || ran(other.ID, RoleSimplify) || ran(other.ID, "droid-simplify") {
		t.Fatal("RoleRanBefore is per (pr, role)")
	}
}

// LastEndedRun is the session's turn that ended last, by ended_at: an own
// pass created after the judge's run but ended before it is not the latest,
// and runs of another session or still in flight do not count.
func TestLastEndedRunIsTheSessionsLatestTurn(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	pr := mustPR(t, st, mustRepo(t, st).ID, 1, PRReviewing)
	judge, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleJudge, State: SessionLive})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleClaude, State: SessionLive})
	if err != nil {
		t.Fatal(err)
	}
	newRun := func(sessionID int64, kind string) Run {
		t.Helper()
		r, err := st.CreateRun(ctx, Run{PRID: pr.ID, Round: 1, Role: RoleJudge, SessionID: &sessionID, Kind: kind, TargetSHA: "h",
			Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: RunWorking, PromptText: "p"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	end := func(id, state string) {
		t.Helper()
		if err := st.TransitionRun(ctx, id, nil, state, func(u *RunUpdate) { u.Set("ended_at", clk.Now()) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.LastEndedRun(ctx, pr.ID, judge.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no run yet: %v", err)
	}
	main := newRun(judge.ID, RunInitial)
	clk.Add(time.Second)
	own := newRun(judge.ID, RunOwnPass) // created after the judge's run, as a round does
	clk.Add(time.Minute)
	end(own.ID, RunEnded)
	if r, err := st.LastEndedRun(ctx, pr.ID, judge.ID); err != nil || r.ID != own.ID {
		t.Fatalf("after the own pass: %+v %v", r, err)
	}
	clk.Add(time.Minute)
	end(main.ID, RunVerified)
	clk.Add(time.Minute)
	end(newRun(other.ID, RunInitial).ID, RunVerified)
	newRun(judge.ID, RunNudge) // in flight: no ended_at
	if r, err := st.LastEndedRun(ctx, pr.ID, judge.ID); err != nil || r.ID != main.ID || r.EndedAt == nil {
		t.Fatalf("after the judge's run: %+v %v", r, err)
	}
}
