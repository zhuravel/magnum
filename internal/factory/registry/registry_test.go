package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

func TestOpenCreatesAPrivateMigratedDatabase(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "factory.db")
	r, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	v, err := r.SchemaVersion(t.Context())
	if err != nil || v != registry.LatestSchemaVersion() || v < 1 {
		t.Errorf("SchemaVersion = %d, %v; want the latest (%d)", v, err, registry.LatestSchemaVersion())
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s mode = %v, %v; want %v", p, st.Mode().Perm(), err, want)
		}
	}
	var mode string
	if err := r.DBForTest().QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	var fk int
	if err := r.DBForTest().QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
}

func TestOpenKeepsWhatAnEarlierOpenWrote(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "factory.db")
	first, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	task := createTask(t, first, "PR-12345")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatalf("reopening a current database: %v", err)
	}
	defer second.Close()
	if got, err := second.TaskByID(t.Context(), task.ID); err != nil || got.JiraKey != "PR-12345" {
		t.Errorf("TaskByID after a reopen = %+v, %v", got, err)
	}
}

func TestOpenRefusesADatabaseNewerThanTheBinary(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "factory.db")
	r, err := registry.Open(path, registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DBForTest().ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := registry.Open(path, registry.Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("Open of a newer database = %v, want a refusal that says newer", err)
	}
	if _, err := registry.Open("", registry.Options{}); err == nil {
		t.Error("Open with an empty path succeeded")
	}
	if _, err := registry.Open(filepath.Join(t.TempDir(), "a?b.db"), registry.Options{}); err == nil {
		t.Error("Open with a '?' in the path succeeded")
	}
}

func TestRegistrytestGivesEachTestItsOwnMigratedCopy(t *testing.T) {
	t.Parallel()
	a, b := registrytest.Open(t), registrytest.Open(t)
	createTask(t, a, "PR-12301")
	if tasks, err := b.Tasks(t.Context()); err != nil || len(tasks) != 0 {
		t.Errorf("the second copy holds %d tasks, %v; want none", len(tasks), err)
	}
	v, err := b.SchemaVersion(t.Context())
	if err != nil || v != registry.LatestSchemaVersion() {
		t.Errorf("template schema version = %d, %v", v, err)
	}
}

func TestSchemaRefusesWhatTheTypedMethodsNeverWrite(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	db := r.DBForTest()
	bad := []string{
		"INSERT INTO tasks (state, created_at, updated_at) VALUES ('paused', 'x', 'x')",
		"INSERT INTO attempts (id, task_id, stage, n, state, started_at) VALUES ('t1-a-1', 1, 'a', 1, 'running', 'x')", // no task 1: foreign key
		"INSERT INTO requests (kind, state, created_at) VALUES ('start', 'queued', 'x')",
		"INSERT INTO events (at, kind, phase) VALUES ('x', 'push', 'started')",
	}
	for _, q := range bad {
		if _, err := db.ExecContext(t.Context(), q); err == nil {
			t.Errorf("schema accepted: %s", q)
		}
	}
	// The partial index is the key rule's last line of defence.
	const insert = "INSERT INTO tasks (jira_key, state, created_at, updated_at) VALUES ('PR-12301', ?, 'x', 'x')"
	if _, err := db.ExecContext(t.Context(), insert, "active"); err != nil {
		t.Fatalf("first task for a key: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), insert, "failed"); err == nil {
		t.Error("the schema holds two unfinished tasks for one Jira key")
	}
	if _, err := db.ExecContext(t.Context(), insert, "done"); err != nil {
		t.Errorf("the schema refuses a done task next to an active one: %v", err)
	}
}
