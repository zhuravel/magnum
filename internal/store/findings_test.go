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

// mustRunAt creates a run of pr in round with role, created at t0+d.
func mustRunAt(t *testing.T, st *Store, prID int64, round int, role string, d time.Duration) Run {
	t.Helper()
	r, err := st.CreateRun(context.Background(), Run{PRID: prID, Round: round, Role: role, Kind: RunInitial, State: RunVerified,
		TargetSHA: "h1", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "p", CreatedAt: t0.Add(d)})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

func TestRecordFindingsReplacesTheRunsRows(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	run := mustRunAt(t, st, pr.ID, 1, RoleJudge, 0)

	fs := []Finding{
		{FindingID: "F1", Severity: "P2", Path: "app/a.rb", Line: 12, Sources: []string{"claude-review", "judge"}, Verdict: FindingPosted},
		{FindingID: "F2", Severity: "P3", Sources: []string{"codex-review"}, Verdict: FindingRejected, ReasonCode: "style_only"},
	}
	if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, fs); err != nil {
		t.Fatal(err)
	}
	// Recording the same round again replaces its rows.
	clk.Add(time.Minute)
	fs[1].ReasonCode = "speculative"
	if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, fs); err != nil {
		t.Fatal(err)
	}
	got, err := st.FindingsSince(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("findings = %+v", got)
	}
	want := Finding{ID: got[0].ID, RunID: run.ID, PRID: pr.ID, Round: 1, FindingID: "F1", Severity: "P2", Path: "app/a.rb", Line: 12,
		Sources: []string{"claude-review", "judge"}, Verdict: FindingPosted, CreatedAt: t0.Add(time.Minute), Repo: "talkable/talkable"}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("finding = %+v\nwant %+v", got[0], want)
	}
	if f := got[1]; f.Path != "" || f.Line != 0 || f.ReasonCode != "speculative" || !reflect.DeepEqual(f.Sources, []string{"codex-review"}) {
		t.Fatalf("body finding = %+v", f)
	}
	if after, _ := st.FindingsSince(ctx, t0.Add(2*time.Minute)); len(after) != 0 {
		t.Fatalf("since after the round = %+v", after)
	}
	// An empty list clears the run's rows.
	if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.FindingsSince(ctx, t0.Add(-time.Hour)); len(left) != 0 {
		t.Fatalf("after clearing = %+v", left)
	}
}

func TestRecordFindingsRefusesBadRows(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	run := mustRunAt(t, st, pr.ID, 1, RoleJudge, 0)
	for name, fs := range map[string][]Finding{
		"no id":         {{Verdict: FindingPosted}},
		"verdict":       {{FindingID: "F1", Verdict: "accepted"}},
		"duplicate ids": {{FindingID: "F1", Verdict: FindingPosted}, {FindingID: "F1", Verdict: FindingRejected}},
	} {
		if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, fs); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := st.RecordFindings(ctx, "", pr.ID, 1, nil); err == nil {
		t.Error("no run id: want an error")
	}
	// A failed batch leaves nothing behind.
	if got, _ := st.FindingsSince(ctx, t0.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("rows after refused batches = %+v", got)
	}
	err := st.RecordFindings(ctx, run.ID, pr.ID, 1, []Finding{{FindingID: "F1", Verdict: FindingPosted}, {FindingID: "F1", Verdict: FindingPosted}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate ids = %v, want ErrConflict", err)
	}
}

func TestRunsOfRoundsSinceKeepsRoundsWhole(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	other := mustPR(t, st, repo.ID, 8, PRReviewed)
	old := mustRunAt(t, st, pr.ID, 1, RoleClaude, -48*time.Hour)            // round 1: before the window only
	began := mustRunAt(t, st, pr.ID, 2, RoleClaude, -2*time.Hour)           // round 2 began before the window
	cont := mustRunAt(t, st, pr.ID, 2, RoleJudge, time.Hour)                // ... and went on inside it
	fresh := mustRunAt(t, st, other.ID, 1, RoleCodexReview, 30*time.Minute) // another PR's round
	_ = old

	got, err := st.RunsOfRoundsSince(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
		if r.Repo != "talkable/talkable" || (r.PRID == pr.ID) != (r.Number == 7) {
			t.Errorf("run %s: repo %q number %d", r.ID, r.Repo, r.Number)
		}
	}
	if want := []string{began.ID, fresh.ID, cont.ID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("runs = %v, want %v", ids, want)
	}
}

func TestEventsOfKindsSince(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	sub := "pr:talkable/talkable#7"
	for i, e := range []Event{
		{At: t0.Add(-time.Hour), Kind: "round.restarted", Message: "old"},
		{At: t0, Kind: "round.restarted", Message: "a"},
		{At: t0.Add(time.Minute), Kind: "round.start", Message: "other kind"},
		{At: t0.Add(2 * time.Minute), Kind: "agent.prompt_denied", Message: "b"},
	} {
		e.Subject = &sub
		if _, err := st.AppendEvent(ctx, e); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	got, err := st.EventsOfKindsSince(ctx, t0, "round.restarted", "agent.prompt_denied")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, e := range got {
		msgs = append(msgs, e.Message)
	}
	if strings.Join(msgs, ",") != "a,b" {
		t.Fatalf("events = %v", msgs)
	}
	if none, err := st.EventsOfKindsSince(ctx, t0); none != nil || err != nil {
		t.Fatalf("no kinds = %v, %v", none, err)
	}
}

// TestMigrationV4ToV5Findings reopens a registry the v4 binary wrote: the
// findings table and the stats indexes appear and the runs stay.
func TestMigrationV4ToV5Findings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	stmts := []string{}
	for _, m := range migrations[:4] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 4",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, head_sha, head_changed_at, state, identity, created_at, updated_at)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'h1', '`+ts+`', 'reviewed', 'talkable-app', '`+ts+`', '`+ts+`')`,
		`INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, prompt_text, result_json, created_at)
		 VALUES ('r-a', 1, 1, 'codex-judge', 'initial', 'h1', 'talkable-app', 'talkable[bot]', 'verified', 'p', '{"status":"posted"}', '`+ts+`')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v4 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v4 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 5 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	for _, idx := range []string{"findings_created", "findings_pr", "runs_created", "events_kind_at"} {
		var n int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d, %v", idx, n, err)
		}
	}
	if err := st.RecordFindings(ctx, "r-a", 1, 1, []Finding{{FindingID: "F1", Verdict: FindingPosted, Sources: []string{"judge"}}}); err != nil {
		t.Fatalf("RecordFindings after the migration: %v", err)
	}
	runs, err := st.RunsOfRoundsSince(ctx, t0)
	if err != nil || len(runs) != 1 || Deref(runs[0].ResultJSON) != `{"status":"posted"}` {
		t.Fatalf("runs after the migration = %+v, %v", runs, err)
	}
}

func TestFindingsByPRReturnsOnlyThatPRsOldestFirst(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	other := mustPR(t, st, repo.ID, 8, PRReviewed)
	round1 := mustRunAt(t, st, pr.ID, 1, RoleJudge, 0)
	round2 := mustRunAt(t, st, pr.ID, 2, RoleJudge, time.Hour)
	otherRun := mustRunAt(t, st, other.ID, 1, RoleJudge, 0)

	// Inserted out of order: the list is by time, then by id.
	err := st.RecordFindings(ctx, round1.ID, pr.ID, 1, []Finding{
		{FindingID: "F1", Severity: "P2", Path: "app/a.rb", Line: 12, Sources: []string{"claude-review", "judge"}, Verdict: FindingPosted, CreatedAt: t0.Add(2 * time.Minute)},
		{FindingID: "F2", Sources: []string{"codex-review"}, Verdict: FindingRejected, ReasonCode: "style_only", CreatedAt: t0.Add(time.Minute)},
		{FindingID: "F3", Sources: []string{}, Verdict: FindingRejected, ReasonCode: "duplicate", CreatedAt: t0.Add(time.Minute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFindings(ctx, round2.ID, pr.ID, 2, []Finding{{FindingID: "F1", Sources: []string{"judge"}, Verdict: FindingPosted, CreatedAt: t0.Add(3 * time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFindings(ctx, otherRun.ID, other.ID, 1, []Finding{{FindingID: "F9", Sources: []string{"judge"}, Verdict: FindingPosted, CreatedAt: t0}}); err != nil {
		t.Fatal(err)
	}

	got, err := st.FindingsByPR(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range got {
		if f.PRID != pr.ID || f.Repo != "" {
			t.Errorf("finding %+v: pr %d, repo %q; want PR %d and no repo", f, f.PRID, f.Repo, pr.ID)
		}
		order = append(order, itoa(int64(f.Round))+f.FindingID)
	}
	if want := []string{"1F2", "1F3", "1F1", "2F1"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("findings = %v, want %v", order, want)
	}
	want := Finding{ID: got[2].ID, RunID: round1.ID, PRID: pr.ID, Round: 1, FindingID: "F1", Severity: "P2", Path: "app/a.rb", Line: 12,
		Sources: []string{"claude-review", "judge"}, Verdict: FindingPosted, CreatedAt: t0.Add(2 * time.Minute)}
	if !reflect.DeepEqual(got[2], want) {
		t.Fatalf("finding = %+v\nwant %+v", got[2], want)
	}
	if none, err := st.FindingsByPR(ctx, 999); err != nil || len(none) != 0 {
		t.Fatalf("findings of an unknown PR = %+v, %v", none, err)
	}
	// FindingsSince still names the repository.
	if all, _ := st.FindingsSince(ctx, t0.Add(-time.Hour)); len(all) != 5 || all[0].Repo != "talkable/talkable" {
		t.Fatalf("FindingsSince = %+v", all)
	}
}
