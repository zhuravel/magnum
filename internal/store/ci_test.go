package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestMigrationV6ToV7CI reopens a registry the previous binary wrote (0001-0006,
// user_version 6): the ci columns appear empty and the PR keeps its data.
func TestMigrationV6ToV7CI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ts := FormatTime(t0)
	var stmts []string
	for _, m := range migrations[:6] {
		stmts = append(stmts, m.sql)
	}
	stmts = append(stmts, "PRAGMA user_version = 6",
		`INSERT INTO repos (id, node_id, owner, name, watch_owner, mode, last_seen_at) VALUES (1, 'R_1', 'talkable', 'talkable', 'talkable', 'pool', '`+ts+`')`,
		`INSERT INTO prs (id, repo_id, node_id, number, url, title, head_sha, head_changed_at, state, identity, created_at, updated_at, author_association)
		 VALUES (1, 1, 'PR_1', 7, 'u7', 'seven', 'h1', '`+ts+`', 'reviewed', 'talkable-app', '`+ts+`', '`+ts+`', 'MEMBER')`)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("v6 setup: %v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v6 database: %v", err)
	}
	defer st.Close()
	if v, _ := st.SchemaVersion(ctx); v != LatestSchemaVersion() || v < 7 {
		t.Fatalf("schema version = %d (latest %d)", v, LatestSchemaVersion())
	}
	pr, err := st.PRByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pr.CIState != nil || pr.CI != nil || Deref(pr.Title) != "seven" || Deref(pr.AuthorAssociation) != "MEMBER" || pr.State != PRReviewed {
		t.Fatalf("migrated PR = %+v", pr)
	}
	rows, err := st.Board(ctx, BoardFilter{})
	if err != nil || len(rows) != 1 || rows[0].CI != nil || rows[0].CIState != "" {
		t.Fatalf("board on a migrated registry = %+v, %v", rows, err)
	}
}

func ciFixture(sha, state string, checks ...CheckResult) *CIStatus {
	ci := &CIStatus{SHA: sha, State: state, Total: len(checks), Complete: true, Checks: checks}
	ci.Tally()
	return ci
}

// TestUpsertPRCI: the radar's rollup and the Details' checks are stored and
// read back, nil keeps them, and a CI change is written without counting as
// Changed (CI moving changes no eligibility).
func TestUpsertPRCI(t *testing.T) {
	st, c := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pending := ciFixture("h1", "PENDING", CheckResult{Name: "rspec (1)", State: CheckPassed, Workflow: "CI", At: t0.Add(-time.Hour)},
		CheckResult{Name: "jest", State: CheckPending, Workflow: "CI"},
		CheckResult{Name: "completion", State: CheckPending, At: t0}, CheckResult{Name: "deploy-preview", State: CheckSkipped, Workflow: "CI"})
	if pending.Passed != 1 || pending.Pending != 2 || pending.Skipped != 1 || pending.Failed != 0 || pending.AllSkipped {
		t.Fatalf("Tally = %+v", pending)
	}
	in := GitHubPR{RepoID: repo.ID, NodeID: "PR_9", Number: 9, URL: "u9", HeadSHA: "h1", InitialState: PRQueued, Identity: "talkable-app",
		DetailsAt: Ptr(t0), CIState: Ptr("PENDING"), CI: pending}
	res, err := st.UpsertPRFromGitHub(ctx, in)
	if err != nil || !res.New {
		t.Fatalf("insert = %+v, %v", res, err)
	}
	if Deref(res.PR.CIState) != "PENDING" || res.PR.CI == nil || !reflect.DeepEqual(*res.PR.CI, *pending) {
		t.Fatalf("inserted CI = %v %+v", res.PR.CIState, res.PR.CI)
	}

	// The same CI again: nothing written.
	c.Add(time.Minute)
	in.DetailsAt = Ptr(t0.Add(time.Minute))
	in.CI = ciFixture("h1", "PENDING", pending.Checks...)
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed || !res.PR.UpdatedAt.Equal(t0) || !res.PR.DetailsAt.Equal(t0) {
		t.Fatalf("no-op CI refresh = %+v, %v", res, err)
	}

	// The radar alone moves ci_state; ci_json stays.
	radar := GitHubPR{RepoID: repo.ID, NodeID: "PR_9", Number: 9, URL: "u9", HeadSHA: "h1", CIState: Ptr("FAILURE")}
	if res, err = st.UpsertPRFromGitHub(ctx, radar); err != nil || res.Changed || res.HeadChanged {
		t.Fatalf("radar CI = %+v, %v", res, err)
	}
	if Deref(res.PR.CIState) != "FAILURE" || res.PR.CI.State != "PENDING" || !res.PR.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after the radar: %v %+v", res.PR.CIState, res.PR.CI)
	}

	// Details with a failed check: written, details_at stamped, still not Changed.
	c.Add(time.Minute)
	failed := ciFixture("h1", "FAILURE", CheckResult{Name: "rspec (1)", State: CheckPassed, Workflow: "CI", At: t0.Add(-time.Hour)},
		CheckResult{Name: "jest", State: CheckFailed, Workflow: "CI", At: t0.Add(time.Minute)},
		CheckResult{Name: "completion", State: CheckPending, At: t0}, CheckResult{Name: "deploy-preview", State: CheckSkipped, Workflow: "CI"})
	in.CI, in.CIState, in.DetailsAt = failed, Ptr("FAILURE"), Ptr(t0.Add(2*time.Minute))
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || res.Changed {
		t.Fatalf("CI change = %+v, %v", res, err)
	}
	if !reflect.DeepEqual(*res.PR.CI, *failed) || res.PR.CI.Failed != 1 || !res.PR.DetailsAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("after the Details: %+v details_at %v", res.PR.CI, res.PR.DetailsAt)
	}

	// nil keeps both; "" (no checks) is a value, not nil.
	keep := in
	keep.CI, keep.CIState, keep.DetailsAt = nil, nil, nil
	if res, err = st.UpsertPRFromGitHub(ctx, keep); err != nil || res.PR.CI == nil || Deref(res.PR.CIState) != "FAILURE" {
		t.Fatalf("nil CI = %+v, %v", res.PR, err)
	}
	none := ciFixture("h2", "")
	in.HeadSHA, in.CI, in.CIState = "h2", none, Ptr("")
	if res, err = st.UpsertPRFromGitHub(ctx, in); err != nil || !res.HeadChanged {
		t.Fatalf("push = %+v, %v", res, err)
	}
	if res.PR.CIState == nil || *res.PR.CIState != "" || res.PR.CI.SHA != "h2" || res.PR.CI.Checks == nil || len(res.PR.CI.Checks) != 0 {
		t.Fatalf("no checks = %v %+v", res.PR.CIState, res.PR.CI)
	}

	rows, err := st.Board(ctx, BoardFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("board = %+v, %v", rows, err)
	}
	if rows[0].CIState != "" || rows[0].CI == nil || !reflect.DeepEqual(*rows[0].CI, *none) {
		t.Fatalf("board CI = %q %+v", rows[0].CIState, rows[0].CI)
	}
	in.CI, in.CIState = failed, Ptr("FAILURE")
	if _, err := st.UpsertPRFromGitHub(ctx, in); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.Board(ctx, BoardFilter{})
	if rows[0].CIState != "FAILURE" || rows[0].CI == nil || !reflect.DeepEqual(*rows[0].CI, *failed) {
		t.Fatalf("board CI = %q %+v", rows[0].CIState, rows[0].CI)
	}
}

func TestCIStatusTally(t *testing.T) {
	draft := ciFixture("h1", "SUCCESS", CheckResult{Name: "rspec (1)", State: CheckSkipped}, CheckResult{Name: "Completion", State: CheckSkipped})
	if !draft.AllSkipped || draft.Skipped != 2 || draft.Passed != 0 {
		t.Fatalf("a draft whose checks all skipped = %+v", draft)
	}
	if none := ciFixture("h1", ""); none.AllSkipped || none.Checks == nil {
		t.Fatalf("no checks = %+v", none)
	}
	mixed := ciFixture("h1", "SUCCESS", CheckResult{Name: "lint", State: CheckSkipped}, CheckResult{Name: "Completion", State: CheckPassed})
	if mixed.AllSkipped || mixed.Passed != 1 || mixed.Skipped != 1 {
		t.Fatalf("mixed = %+v", mixed)
	}
	// Timestamps compare as instants: the same check read back is equal.
	a := ciFixture("h1", "SUCCESS", CheckResult{Name: "Completion", State: CheckPassed, Workflow: "CI", At: t0})
	b := ciFixture("h1", "SUCCESS", CheckResult{Name: "Completion", State: CheckPassed, Workflow: "CI", At: t0.In(time.FixedZone("x", 3600))})
	if !a.equal(*b) {
		t.Fatal("equal instants in another zone must be equal")
	}
	b.Checks[0].Workflow = "Pronto"
	if a.equal(*b) {
		t.Fatal("another workflow must differ")
	}
}

// TestRequiredChecksAccessor: the configured list wins; else GitHub's cached
// list when GitHub told; a list GitHub would not give is unknown.
func TestRequiredChecksAccessor(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	if got, err := st.RequiredChecks(ctx, "talkable/talkable", nil); err != nil || got.Source != "" || got.Checks != nil {
		t.Fatalf("never read = %+v, %v", got, err)
	}
	cached := GitHubRequiredChecks{Branch: "master", Checks: []string{"Completion"}, Known: true, FetchedAt: t0, CheckedAt: t0}
	if err := st.SetGitHubRequiredChecks(ctx, "Talkable/Talkable", cached); err != nil {
		t.Fatal(err)
	}
	if g, ok, err := st.GitHubRequiredChecks(ctx, "talkable/talkable"); err != nil || !ok || !reflect.DeepEqual(g, cached) {
		t.Fatalf("cache = %+v %v %v", g, ok, err)
	}
	got, err := st.RequiredChecks(ctx, "talkable/talkable", nil)
	want := RequiredChecks{Checks: []string{"Completion"}, Source: RequiredFromGitHub, FetchedAt: t0}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GitHub's = %+v, %v", got, err)
	}
	got, _ = st.RequiredChecks(ctx, "talkable/talkable", []string{"completion", "workflow:CI"})
	if !reflect.DeepEqual(got, RequiredChecks{Checks: []string{"completion", "workflow:CI"}, Source: RequiredFromConfig}) {
		t.Fatalf("configured = %+v", got)
	}

	if err := st.SetGitHubRequiredChecks(ctx, "talkable/talkable", GitHubRequiredChecks{Branch: "master", Known: true, FetchedAt: t0, CheckedAt: t0}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.RequiredChecks(ctx, "talkable/talkable", nil)
	if got.Source != RequiredFromGitHub || got.Checks == nil || len(got.Checks) != 0 {
		t.Fatalf("GitHub requires none = %+v", got)
	}
	if err := st.SetGitHubRequiredChecks(ctx, "talkable/talkable", GitHubRequiredChecks{Branch: "master", FetchedAt: t0, CheckedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.RequiredChecks(ctx, "talkable/talkable", nil); got.Source != "" || got.Checks != nil {
		t.Fatalf("GitHub would not tell = %+v", got)
	}
}
