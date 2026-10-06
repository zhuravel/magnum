package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Migration 0021 adds findings.title and findings.nearby to a registry the
// previous binary wrote (user_version 20): its findings stay, untitled and
// not nearby, and the ones recorded after it keep both.
func TestMigrationV20ToV21AddsFindingTitles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:20] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 20",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', '`+ts+`', '`+ts+`')`,
		`INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, prompt_text, created_at)
		 VALUES ('r-a', 1, 1, 'codex-judge', 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'p', '`+ts+`')`,
		`INSERT INTO findings (run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, reason_code, created_at)
		 VALUES ('r-a', 1, 1, 'F1', 'P2', 'lib/legacy.rb', 3, '["claude-review"]', 'rejected', 'pre_existing', '`+ts+`')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v20 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v20 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 21 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	old, err := st.FindingsByPR(ctx, 1)
	if err != nil || len(old) != 1 || old[0].Title != "" || old[0].Nearby || old[0].Path != "lib/legacy.rb" || old[0].ReasonCode != "pre_existing" {
		t.Fatalf("the finding recorded before = %+v, %v", old, err)
	}
	if err := st.RecordFindings(ctx, "r-a", 1, 1, []Finding{{FindingID: "F1", Title: "Export skips site check", Nearby: true,
		Severity: "P2", Verdict: FindingRejected, ReasonCode: "pre_existing", Sources: []string{"judge"}}}); err != nil {
		t.Fatalf("RecordFindings after the migration: %v", err)
	}
	if got, err := st.FindingsByPR(ctx, 1); err != nil || len(got) != 1 || got[0].Title != "Export skips site check" || !got[0].Nearby {
		t.Fatalf("the finding recorded after = %+v, %v", got, err)
	}
}

// RecordFindings keeps each finding's title and nearby mark, and
// FindingsSince reads them back.
func TestRecordFindingsKeepsTitleAndNearby(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	run := mustRunAt(t, st, pr.ID, 1, RoleJudge, 0)
	if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, []Finding{
		{FindingID: "F1", Title: "Total skips tax", Severity: "P2", Sources: []string{"judge"}, Verdict: FindingPosted},
		{FindingID: "F2", Title: "Export skips site check", Nearby: true, Severity: "P1", Path: "lib/export.rb", Line: 31,
			Sources: []string{"claude-review"}, Verdict: FindingRejected, ReasonCode: "pre_existing"},
		{FindingID: "F3", Severity: "P3", Sources: []string{"codex-review"}, Verdict: FindingRejected, ReasonCode: "speculative"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.FindingsSince(ctx, t0.Add(-time.Hour))
	if err != nil || len(got) != 3 {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	for i, want := range []struct {
		title  string
		nearby bool
	}{{"Total skips tax", false}, {"Export skips site check", true}, {"", false}} {
		if got[i].Title != want.title || got[i].Nearby != want.nearby {
			t.Errorf("finding %s: title %q nearby %v, want %q %v", got[i].FindingID, got[i].Title, got[i].Nearby, want.title, want.nearby)
		}
	}
}

// PreExistingFindings lists the P1 and P2 problems the judge rejected as
// pre-existing, across PRs and repositories, newest first (then by id),
// each with its PR's repository and number; other priorities and reasons,
// and posted findings, stay out.
func TestPreExistingFindingsAreTheRejectedP1AndP2NewestFirst(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	talkable := mustRepo(t, st)
	widgets, err := st.UpsertRepo(ctx, Repo{NodeID: "R_2", Owner: "example", Name: "widgets", WatchOwner: "example",
		DefaultBranch: "main", Mode: RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	pr7 := mustPR(t, st, talkable.ID, 7, PRReviewed)
	pr3 := mustPR(t, st, widgets.ID, 3, PRReviewed)
	run7 := mustRunAt(t, st, pr7.ID, 1, RoleJudge, 0)
	run3 := mustRunAt(t, st, pr3.ID, 1, RoleJudge, 0)
	pre := func(id, sev string, at time.Duration) Finding {
		return Finding{FindingID: id, Severity: sev, Path: "lib/" + id + ".rb", Sources: []string{"judge"}, Verdict: FindingRejected,
			ReasonCode: "pre_existing", CreatedAt: t0.Add(at)}
	}
	if err := st.RecordFindings(ctx, run7.ID, pr7.ID, 1, []Finding{
		pre("A", "P2", time.Minute),
		pre("B", "P1", 3*time.Minute),
		pre("C", "P3", 4*time.Minute), // a P3 is not debt
		{FindingID: "D", Severity: "P2", Sources: []string{"judge"}, Verdict: FindingRejected, ReasonCode: "speculative", CreatedAt: t0.Add(5 * time.Minute)},
		{FindingID: "E", Severity: "P1", Sources: []string{"judge"}, Verdict: FindingPosted, CreatedAt: t0.Add(5 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFindings(ctx, run3.ID, pr3.ID, 1, []Finding{pre("F", "P2", 2*time.Minute), pre("G", "P0", 6*time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := st.PreExistingFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range got {
		order = append(order, f.FindingID+" "+f.Repo+"#"+strconv.Itoa(f.Number))
	}
	want := []string{"B talkable/talkable#7", "F example/widgets#3", "A talkable/talkable#7"}
	if len(order) != len(want) {
		t.Fatalf("pre-existing findings = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("pre-existing findings = %v, want %v", order, want)
		}
	}
}
