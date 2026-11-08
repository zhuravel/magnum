// Package factoryread is the review side's read of what the factory keeps:
// the local review round of a pull request the factory published, from its
// registry (factory.db), and the task's plan, ticket and answers, from the
// task's directory, for the review retro (DECISIONS "The review retro tags
// factory-origin misses and their stage"). It only reads: the registry is
// opened read only (mode=ro, never created, so nothing writes its file or
// its WAL), with a busy timeout, and closed after each read; a task's files
// are read inside an os.Root of its directory. It knows the factory's
// schema only as far as these reads need (pr_links, review_findings) and
// never migrates it: internal/factory/registry owns it.
package factoryread

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strconv"

	_ "modernc.org/sqlite" // the "sqlite" driver

	"github.com/zhuravel/magnum/internal/escapes"
)

// ErrNoRegistry is Published's error when the factory's registry does not
// exist: no factory ever ran on this machine.
var ErrNoRegistry = errors.New("the factory's registry does not exist")

// busyTimeoutMS is how long a read waits for the factory daemon's write.
const busyTimeoutMS = 2000

// Task is one factory task that published a pull request: the heads its PR
// opened on and its local review approved, and that review's findings.
type Task struct {
	ID           int64
	OpenedHead   string
	ApprovedHead string
	// Findings are the local review's findings, first seen first, each
	// escapes.SourceLocal with Ref "<task id>/<finding id>", Round its last
	// cycle (where the registry took its place from) and Head the task's
	// Head.
	Findings []escapes.Finding
}

// Head is the commit the task's local findings are taken to name: the head
// its local review approved, else the one its PR opened on.
func (t Task) Head() string { return cmp.Or(t.ApprovedHead, t.OpenedHead) }

// dsn opens the registry at path read only, never creating it.
func dsn(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path,
		RawQuery: "mode=ro&_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")&_pragma=temp_store(MEMORY)"}
	return u.String()
}

// Published returns the tasks of the registry at path that published
// pull request number of repo ("owner/name", any case), newest first, each
// with its local review's findings; none when the factory published no
// such pull request. A registry that does not exist is ErrNoRegistry; one
// that cannot be opened or read (not a database, a schema without these
// tables) is another error.
func Published(ctx context.Context, path, repo string, number int) ([]Task, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoRegistry
	} else if err != nil {
		return nil, fmt.Errorf("the factory's registry: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open the factory's registry: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// One statement, so one snapshot of the registry the factory writes.
	rows, err := db.QueryContext(ctx, `
SELECT l.task_id, l.opened_head, l.approved_head, f.id, f.path, f.start_line, f.end_line, f.priority, f.title_hash, f.last_cycle
FROM pr_links l LEFT JOIN review_findings f ON f.task_id = l.task_id
WHERE l.repo = ? COLLATE NOCASE AND l.number = ?
ORDER BY l.task_id DESC, f.id`, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read the factory's registry: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var id, start, end, cycle sql.NullInt64
		var path, priority, titleHash sql.NullString
		if err := rows.Scan(&t.ID, &t.OpenedHead, &t.ApprovedHead, &id, &path, &start, &end, &priority, &titleHash, &cycle); err != nil {
			return nil, fmt.Errorf("read the factory's registry: %w", err)
		}
		if n := len(out); n == 0 || out[n-1].ID != t.ID {
			out = append(out, t)
		}
		if !id.Valid {
			continue // a task whose local review found nothing
		}
		last := &out[len(out)-1]
		last.Findings = append(last.Findings, escapes.Finding{Source: escapes.SourceLocal,
			Ref:  strconv.FormatInt(t.ID, 10) + "/" + strconv.FormatInt(id.Int64, 10),
			Path: path.String, StartLine: int(start.Int64), EndLine: int(end.Int64), Priority: priority.String,
			TitleHash: titleHash.String, Round: int(cycle.Int64), Head: last.Head()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the factory's registry: %w", err)
	}
	return out, nil
}

// The kinds of a task's files the retro's classifier reads (TaskFile.Kind).
const (
	KindPlan    = "plan"    // the plan: the report the factory's judge wrote
	KindTicket  = "ticket"  // the Jira issue, or the prompt a task started from
	KindAnswers = "answers" // the operator's answers to the task's questions
)

// taskFiles are the names each kind may have in a task's directory, the
// first that exists winning (as the factory's local review reads its
// ticket).
var taskFiles = []struct {
	kind  string
	names []string
}{
	{KindPlan, []string{"report.md"}},
	{KindTicket, []string{"ticket.md", "prompt.md"}},
	{KindAnswers, []string{"answers.md"}},
}

// TaskFileMax is the largest task file read, in bytes.
const TaskFileMax = 1 << 20

// TaskFile is one of a task's files: its kind, its name in the task's
// directory and its bytes, or why it was not read (Skipped).
type TaskFile struct {
	Kind    string
	Name    string
	Data    []byte
	Skipped string
}

// TaskFiles reads the plan, the ticket and the answers in the task
// directory dir, in that order, read only and inside dir: a kind none of
// whose names exists is left out, and one larger than TaskFileMax or that
// cannot be read says why (Skipped). A directory that does not exist has no
// files.
func TaskFiles(dir string) []TaskFile {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	var out []TaskFile
	for _, k := range taskFiles {
		for _, name := range k.names {
			f, err := root.Open(name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			tf := TaskFile{Kind: k.kind, Name: name}
			if err == nil {
				tf.Data, err = io.ReadAll(io.LimitReader(f, TaskFileMax+1))
				f.Close()
			}
			switch {
			case err != nil:
				tf.Data, tf.Skipped = nil, "it cannot be read"
			case len(tf.Data) > TaskFileMax:
				tf.Data, tf.Skipped = nil, fmt.Sprintf("larger than %d KiB", TaskFileMax>>10)
			}
			out = append(out, tf)
			break
		}
	}
	return out
}
