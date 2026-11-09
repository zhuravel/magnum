package registry_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/store"
)

// writeVersion6 writes a factory registry as a version 6 binary left it at
// path: every migration up to 0006_tracking, one task and one check of its
// precheck.
func writeVersion6(t *testing.T, path string) {
	t.Helper()
	ctx := t.Context()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 6; v++ {
		if _, err := tx.ExecContext(ctx, registry.MigrationSQLForTest(v)); err != nil {
			t.Fatalf("migration %d: %v", v, err)
		}
	}
	ts := store.FormatTime(t0)
	for _, q := range []string{
		"PRAGMA user_version = 6",
		`INSERT INTO tasks (id, jira_key, kind, repo, title, state, pr_number, created_at, updated_at)
			VALUES (1, 'PR-12390', 'bug', 'example/app', 'First', 'published', 4530, '` + ts + `', '` + ts + `')`,
		`INSERT INTO checks (task_id, head_sha, kind, result, source, at) VALUES (1, 'abc123', 'spec', 'pass', 'factory', '` + ts + `')`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("version 6 file: %s: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// The escapes' tables come with the registry's migrations: on the tests'
// template, and up from a file a version 6 binary left, whose rows stay and
// whose task has no check until the daemon makes one.
func TestTheEscapesMigrationAppliesOnTheTemplateAndAVersion6File(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	var n int
	if err := r.DBForTest().QueryRowContext(t.Context(),
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('escape_checks', 'escapes')").Scan(&n); err != nil || n != 2 {
		t.Fatalf("the template's escapes tables: %d, %v; want 2", n, err)
	}

	path := filepath.Join(t.TempDir(), "factory.db")
	writeVersion6(t, path)
	old, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatalf("Open of a version 6 file: %v", err)
	}
	defer old.Close()
	ctx := t.Context()
	if v, err := old.SchemaVersion(ctx); err != nil || v != registry.LatestSchemaVersion() || v != 7 {
		t.Fatalf("SchemaVersion = %d, %v; want 7, the latest (%d)", v, err, registry.LatestSchemaVersion())
	}
	if _, err := old.EscapeCheck(ctx, 1); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("EscapeCheck of an older task = %v, want ErrNotFound", err)
	}
	if es, err := old.Escapes(ctx, 1); err != nil || len(es) != 0 {
		t.Errorf("Escapes of an older task = %+v, %v; want none", es, err)
	}
	if cs, err := old.Checks(ctx, 1); err != nil || len(cs) != 1 || cs[0].Kind != registry.CheckSpec || cs[0].HeadSHA != "abc123" {
		t.Errorf("the older task's checks = %+v, %v; want its spec check kept", cs, err)
	}
	if ok, err := old.RecordGitHubCI(ctx, 1, "abc123", registry.CheckFail, ""); err != nil || !ok {
		t.Errorf("the first CI row of the migrated file: %v, %v; want it written", ok, err)
	}
}

// localFinding stores a finding of task's local review in a cycle of its
// own and returns it.
func localFinding(t *testing.T, r *registry.Registry, task registry.Task, path string, line int) registry.Finding {
	t.Helper()
	cy, err := r.BeginCycle(t.Context(), task.ID, "abc123", "skill")
	if err != nil {
		t.Fatal(err)
	}
	f, err := r.SeeFinding(t.Context(), cy.ID, registry.Finding{Fingerprint: path + ":" + strconv.Itoa(line), Path: path,
		StartLine: line, EndLine: line, TitleHash: "0123456789ab", Priority: "P2"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A task's escape check is written once, with its escapes, in one
// transaction: a second write of the same task writes nothing and says so,
// whatever it holds. Each escape keeps magnum's finding, its place, its
// priority, its title's hash and why, and the nearest local finding and
// its distance for far only; a row that breaks those rules is refused and
// leaves nothing behind.
func TestAnEscapeCheckIsWrittenOnceWithItsEscapes(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12391")
	near := localFinding(t, r, task, "app/models/credit.rb", 40)

	check := registry.EscapeCheck{TaskID: task.ID, RunID: "r-20261103T120000-7", Round: 2, HeadSHA: "def456", Posted: 3, Comparable: 1}
	escapes := []registry.Escape{
		{FindingID: 71, Path: "app/models/credit.rb", StartLine: 12, EndLine: 12, Priority: "P2", TitleHash: "aaaaaaaaaaaa", Reason: registry.EscapeNoLocal},
		{FindingID: 72, Path: "app/models/credit.rb", StartLine: 44, EndLine: 44, Priority: "P1", TitleHash: "bbbbbbbbbbbb", Reason: registry.EscapeFar,
			NearestID: near.ID, Distance: 4},
		{FindingID: 73, Priority: "P0", TitleHash: "cccccccccccc", Reason: registry.EscapeUnplaced},
	}
	for name, bad := range map[string]struct {
		c  registry.EscapeCheck
		es []registry.Escape
	}{
		"no run":              {registry.EscapeCheck{TaskID: task.ID, HeadSHA: "def456"}, nil},
		"no head":             {registry.EscapeCheck{TaskID: task.ID, RunID: "r-1"}, nil},
		"a P3":                {check, []registry.Escape{{FindingID: 74, Priority: "P3", Reason: registry.EscapeNoLocal}}},
		"far without nearest": {check, []registry.Escape{{FindingID: 74, Priority: "P2", Reason: registry.EscapeFar}}},
		"nearest not far":     {check, []registry.Escape{{FindingID: 74, Priority: "P2", Reason: registry.EscapeNoLocal, NearestID: near.ID, Distance: 5}}},
		"another reason":      {check, []registry.Escape{{FindingID: 74, Priority: "P2", Reason: "missed"}}},
		"one finding twice":   {check, []registry.Escape{escapes[0], escapes[0]}},
	} {
		if ok, err := r.RecordEscapeCheck(ctx, bad.c, bad.es); err == nil || ok {
			t.Errorf("%s: %v, %v; want it refused", name, ok, err)
		}
	}
	if _, err := r.EscapeCheck(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("a refused check left a row: %v", err)
	}
	if ok, err := r.RecordEscapeCheck(ctx, registry.EscapeCheck{TaskID: 999, RunID: "r-1", HeadSHA: "def456"}, nil); !errors.Is(err, registry.ErrNotFound) || ok {
		t.Errorf("a check of no task: %v, %v; want ErrNotFound", ok, err)
	}

	clk.Add(time.Hour)
	if ok, err := r.RecordEscapeCheck(ctx, check, escapes); err != nil || !ok {
		t.Fatalf("RecordEscapeCheck = %v, %v; want it written", ok, err)
	}
	want := check
	want.At = t0.Add(time.Hour)
	if got, err := r.EscapeCheck(ctx, task.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("EscapeCheck = %+v, %v\nwant %+v", got, err, want)
	}
	got, err := r.Escapes(ctx, task.ID)
	if err != nil || len(got) != len(escapes) {
		t.Fatalf("Escapes = %+v, %v", got, err)
	}
	for i, e := range got {
		w := escapes[i]
		w.ID, w.TaskID, w.At = e.ID, task.ID, t0.Add(time.Hour)
		if e.ID == 0 || !reflect.DeepEqual(e, w) {
			t.Errorf("escape %d = %+v\nwant %+v", i, e, w)
		}
	}

	clk.Add(time.Hour)
	again := check
	again.RunID, again.Posted = "r-20261103T140000-9", 9
	if ok, err := r.RecordEscapeCheck(ctx, again, []registry.Escape{{FindingID: 99, Priority: "P2", Reason: registry.EscapeUnplaced}}); err != nil || ok {
		t.Fatalf("a second check of the task = %v, %v; want nothing written", ok, err)
	}
	if got, _ := r.EscapeCheck(ctx, task.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("after a second check: %+v, want the first %+v", got, want)
	}
	if es, _ := r.Escapes(ctx, task.ID); len(es) != len(escapes) {
		t.Errorf("after a second check: %d escapes, want %d", len(es), len(escapes))
	}
}

// GitHub's CI on a task's PR is one check of kind ci from source github
// per head and result: the same again writes nothing, a head that failed
// and then passed has both rows, and only a pass or a failure is a result
// (a cause only on a failure).
func TestGitHubsCIIsRecordedOncePerHeadAndResult(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12392")
	for _, step := range []struct {
		head, result, cause string
		want                bool
	}{
		{"abc123", registry.CheckFail, "", true},
		{"abc123", registry.CheckFail, "", false},
		{"abc123", registry.CheckFail, "not_ours", false},
		{"abc123", registry.CheckPass, "", true},
		{"def456", registry.CheckFail, "not_ours", true},
	} {
		if ok, err := r.RecordGitHubCI(ctx, task.ID, step.head, step.result, step.cause); err != nil || ok != step.want {
			t.Errorf("RecordGitHubCI(%s, %s, %q) = %v, %v; want %v", step.head, step.result, step.cause, ok, err, step.want)
		}
	}
	for name, bad := range map[string][3]string{
		"pending":       {"abc123", "pending", ""},
		"skipped":       {"abc123", registry.CheckSkipped, ""},
		"a pass's case": {"abc123", registry.CheckPass, "not_ours"},
		"no head":       {"", registry.CheckPass, ""},
	} {
		if ok, err := r.RecordGitHubCI(ctx, task.ID, bad[0], bad[1], bad[2]); err == nil || ok {
			t.Errorf("%s: %v, %v; want it refused", name, ok, err)
		}
	}
	if ok, err := r.RecordGitHubCI(ctx, 999, "abc123", registry.CheckPass, ""); !errors.Is(err, registry.ErrNotFound) || ok {
		t.Errorf("CI of no task: %v, %v; want ErrNotFound", ok, err)
	}
	cs, err := r.Checks(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got [][3]string
	for _, c := range cs {
		if c.Kind != registry.CheckCI || c.Source != "github" {
			t.Errorf("check %+v: want kind ci from github", c)
		}
		got = append(got, [3]string{c.HeadSHA, c.Result, c.Cause})
	}
	want := [][3]string{{"abc123", registry.CheckFail, ""}, {"abc123", registry.CheckPass, ""}, {"def456", registry.CheckFail, "not_ours"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CI checks %v, want %v", got, want)
	}
	// A check of another kind or source is not held to one per head.
	for range 2 {
		if _, err := r.RecordCheck(ctx, registry.Check{TaskID: task.ID, HeadSHA: "abc123", Kind: registry.CheckCI, Result: registry.CheckFail,
			Source: "writer"}); err != nil {
			t.Errorf("the writer's claim of CI: %v", err)
		}
	}
}
