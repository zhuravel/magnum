package factoryread

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/escapes"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/sqlitetest"
)

var (
	opened   = strings.Repeat("a", 40)
	approved = strings.Repeat("b", 40)
)

// fixture is a factory registry at path with the factory's own schema
// (registrytest.Seed, every factory migration) and the rows the statements
// insert, written by a connection that stays open for the test, as the
// factory daemon's does: the file is in WAL mode with its -wal beside it.
func fixture(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "factory", "factory.db")
	registrytest.Seed(t, path)
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("fixture: %v\n%s", err, q)
		}
	}
	return path
}

func task(id string) string {
	return `INSERT INTO tasks (id, repo, state, created_at, updated_at) VALUES (` + id +
		`, 'talkable/talkable', 'published', '2026-11-07T10:00:00Z', '2026-11-07T10:00:00Z')`
}

func link(task, repo, number, openedHead, approvedHead string) string {
	return `INSERT INTO pr_links (task_id, repo, number, opened_head, approved_head, opened_at) VALUES (` + task + `, '` + repo +
		`', ` + number + `, '` + openedHead + `', '` + approvedHead + `', '2026-11-07T18:30:00Z')`
}

func finding(id, task, path, start, end, priority, last string) string {
	return `INSERT INTO review_findings (id, task_id, fingerprint, path, start_line, end_line, title_hash, priority, first_cycle, last_cycle)
		VALUES (` + id + `, ` + task + `, 'fp` + id + `', '` + path + `', ` + start + `, ` + end + `, 'hash` + id + `', '` + priority + `', 1, ` + last + `)`
}

func TestPublishedReadsTheLocalRoundOfAPullRequestTheFactoryPublished(t *testing.T) {
	path := fixture(t, task("1"), task("2"), task("3"),
		link("1", "talkable/talkable", "900011", opened, approved),
		link("2", "talkable/talkable", "900012", opened, approved),
		finding("31", "1", "app/models/coupon.rb", "40", "44", "P2", "2"),
		finding("32", "2", "app/models/other.rb", "5", "5", "P1", "1"),
		finding("33", "1", "spec/models/coupon_spec.rb", "12", "12", "", "1"))
	got, err := Published(context.Background(), path, "Talkable/Talkable", 900011)
	if err != nil {
		t.Fatal(err)
	}
	want := []Task{{ID: 1, OpenedHead: opened, ApprovedHead: approved, Findings: []escapes.Finding{
		{Source: escapes.SourceLocal, Ref: "1/31", Path: "app/models/coupon.rb", StartLine: 40, EndLine: 44, Priority: "P2",
			TitleHash: "hash31", Round: 2, Head: approved},
		{Source: escapes.SourceLocal, Ref: "1/33", Path: "spec/models/coupon_spec.rb", StartLine: 12, EndLine: 12,
			TitleHash: "hash33", Round: 1, Head: approved},
	}}}
	if !slices.EqualFunc(got, want, equalTask) {
		t.Errorf("Published = %+v\nwant %+v", got, want)
	}
}

func equalTask(a, b Task) bool {
	return a.ID == b.ID && a.OpenedHead == b.OpenedHead && a.ApprovedHead == b.ApprovedHead && slices.Equal(a.Findings, b.Findings)
}

// Every task that published the PR comes back, newest first: a task that
// adopted the PR of a stopped one has a pr_links row of its own. A task
// whose local review found nothing is there with no findings, and one
// without an approved head names its opened head.
func TestPublishedReturnsEveryTaskOfThePRNewestFirst(t *testing.T) {
	path := fixture(t, task("4"), task("5"),
		link("4", "talkable/talkable", "7", opened, ""),
		link("5", "talkable/talkable", "7", opened, approved),
		finding("41", "4", "app/a.rb", "3", "3", "P3", "1"))
	got, err := Published(context.Background(), path, "talkable/talkable", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 5 || len(got[0].Findings) != 0 || got[0].Head() != approved ||
		got[1].ID != 4 || got[1].Head() != opened || len(got[1].Findings) != 1 || got[1].Findings[0].Head != opened {
		t.Errorf("Published = %+v", got)
	}
}

func TestPublishedIsEmptyForAPullRequestTheFactoryDidNotPublish(t *testing.T) {
	path := fixture(t, task("1"), link("1", "talkable/talkable", "7", opened, approved))
	for _, tc := range []struct {
		repo   string
		number int
	}{{"talkable/talkable", 8}, {"example/app", 7}} {
		got, err := Published(context.Background(), path, tc.repo, tc.number)
		if err != nil || len(got) != 0 {
			t.Errorf("Published(%s#%d) = %+v, %v; want none", tc.repo, tc.number, got, err)
		}
	}
}

// A machine where the factory never ran has no registry: Published says so
// and creates nothing.
func TestPublishedSaysTheRegistryDoesNotExistAndCreatesNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.db")
	if _, err := Published(context.Background(), path, "talkable/talkable", 7); !errors.Is(err, ErrNoRegistry) {
		t.Errorf("Published = %v, want ErrNoRegistry", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("Published left %v in the directory", ents)
	}
}

func TestPublishedFailsOnAFileThatIsNotTheFactorysRegistry(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("not a database at all, just text that is long enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.db")
	db, err := sql.Open("sqlite", empty+"?"+sqlitetest.FastFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, path := range []string{junk, empty} {
		_, err := Published(context.Background(), path, "talkable/talkable", 7)
		if err == nil || errors.Is(err, ErrNoRegistry) {
			t.Errorf("Published(%s) = %v, want a read error", filepath.Base(path), err)
		}
	}
}

// The read writes nothing: the registry's file and its WAL keep every byte
// while the factory's writer holds them open.
func TestPublishedNeverWritesTheRegistryOrItsWAL(t *testing.T) {
	path := fixture(t, task("1"), link("1", "talkable/talkable", "7", opened, approved),
		finding("31", "1", "app/a.rb", "3", "3", "P2", "1"))
	read := func(p string) []byte {
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return b
	}
	db, wal := read(path), read(path+"-wal")
	if len(wal) == 0 {
		t.Fatal("the fixture has no WAL: the test proves nothing")
	}
	for range 3 {
		if got, err := Published(context.Background(), path, "talkable/talkable", 7); err != nil || len(got) != 1 {
			t.Fatalf("Published = %+v, %v", got, err)
		}
	}
	if !bytes.Equal(read(path), db) || !bytes.Equal(read(path+"-wal"), wal) {
		t.Error("Published changed the registry or its WAL")
	}
}

func TestTaskFilesReadsThePlanTheTicketAndTheAnswers(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("report.md", "# Plan\n")
	write("prompt.md", "Add a coupon limit.\n")
	write("answers.md", "Q1: yes\n")
	write("notes.md", "not the classifier's\n")
	got := TaskFiles(dir)
	want := []TaskFile{{Kind: KindPlan, Name: "report.md", Data: []byte("# Plan\n")},
		{Kind: KindTicket, Name: "prompt.md", Data: []byte("Add a coupon limit.\n")},
		{Kind: KindAnswers, Name: "answers.md", Data: []byte("Q1: yes\n")}}
	if !slices.EqualFunc(got, want, equalFile) {
		t.Errorf("TaskFiles = %+v, want %+v", got, want)
	}
	// A Jira task's ticket is ticket.md, which wins over a prompt.
	write("ticket.md", "TK-1 Coupon limit\n")
	if got := TaskFiles(dir); len(got) != 3 || got[1].Name != "ticket.md" || string(got[1].Data) != "TK-1 Coupon limit\n" {
		t.Errorf("TaskFiles with a ticket = %+v", got)
	}
}

func equalFile(a, b TaskFile) bool {
	return a.Kind == b.Kind && a.Name == b.Name && bytes.Equal(a.Data, b.Data) && a.Skipped == b.Skipped
}

func TestTaskFilesLeavesOutWhatIsMissingAndSkipsAFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), bytes.Repeat([]byte("x"), TaskFileMax+1), 0o600); err != nil {
		t.Fatal(err)
	}
	got := TaskFiles(dir)
	if len(got) != 1 || got[0].Kind != KindPlan || got[0].Data != nil || got[0].Skipped != "larger than 1024 KiB" {
		t.Errorf("TaskFiles = %+v", got)
	}
	if got := TaskFiles(filepath.Join(dir, "gone")); got != nil {
		t.Errorf("TaskFiles of a missing directory = %+v", got)
	}
}

// A task file that is a link out of the task's directory is not followed.
func TestTaskFilesReadsNothingOutsideTheTaskDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	got := TaskFiles(dir)
	if len(got) != 1 || got[0].Data != nil || got[0].Skipped == "" {
		t.Errorf("TaskFiles followed a link out of the directory: %+v", got)
	}
}
