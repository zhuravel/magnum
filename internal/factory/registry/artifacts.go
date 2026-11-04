package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

var artifactKinds = map[string]bool{
	"base_sha": true, "ticket": true, "guidance": true, "answers": true, "report": true,
	"proof": true, "patch": true, "pr_body": true, "test_commands": true,
}

// Artifact is a file of a task's directory as the factory saw it: its kind
// (base_sha, ticket, guidance, answers, report, proof, patch, pr_body or
// test_commands), its path relative to the task directory, the sha256 of its
// content and its revision. A file that changes gets the next revision, so
// the report that the operator approved can be told from the one before it.
type Artifact struct {
	TaskID int64
	Kind   string
	Path   string
	SHA256 string
	Rev    int
	At     time.Time
}

// validSHA256 reports whether s is a lowercase hex sha256.
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RecordArtifact stores a as a new revision of its kind and path, unless the
// latest revision of that kind and path already has the same sha256 (a no-op:
// nothing is written and the first time stays). New content, including going
// back to earlier content, gets the next Rev, counted from 1 for each task,
// kind and path; a.Rev is ignored. The path must be relative to the task
// directory and clean, and the hash a lowercase hex sha256. At defaults to the
// clock and the task must exist (ErrNotFound).
func (r *Registry) RecordArtifact(ctx context.Context, a Artifact) error {
	if !artifactKinds[a.Kind] {
		return fmt.Errorf("record artifact: unknown kind %q", a.Kind)
	}
	if a.Path == "" || !filepath.IsLocal(a.Path) || path.Clean(a.Path) != a.Path {
		return fmt.Errorf("record artifact: path %q must be clean and relative to the task directory", a.Path)
	}
	if !validSHA256(a.SHA256) {
		return fmt.Errorf("record artifact %s: the hash must be a lowercase hex sha256", a.Path)
	}
	if a.At.IsZero() {
		a.At = r.now()
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		var latest string
		var rev int
		err := tx.QueryRowContext(ctx, "SELECT sha256, rev FROM artifacts WHERE task_id = ? AND kind = ? AND path = ? ORDER BY rev DESC LIMIT 1", a.TaskID, a.Kind, a.Path).Scan(&latest, &rev)
		switch {
		case err == nil && latest == a.SHA256:
			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("record artifact %s: %w", a.Path, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO artifacts (task_id, kind, path, sha256, rev, at) VALUES (?, ?, ?, ?, ?, ?)",
			a.TaskID, a.Kind, a.Path, a.SHA256, rev+1, store.FormatTime(a.At)); err != nil {
			return fmt.Errorf("record artifact %s for task %d: %w", a.Path, a.TaskID, mapErr(err))
		}
		return nil
	})
}

// Artifacts returns every revision of every artifact of a task, in the order
// they were recorded.
func (r *Registry) Artifacts(ctx context.Context, taskID int64) ([]Artifact, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT task_id, kind, path, sha256, rev, at FROM artifacts WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, fmt.Errorf("artifacts of task %d: %w", taskID, err)
	}
	as, err := collect(rows, func(sc scanner) (Artifact, error) {
		var a Artifact
		err := sc.Scan(&a.TaskID, &a.Kind, &a.Path, &a.SHA256, &a.Rev, timeCol{&a.At})
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts of task %d: %w", taskID, err)
	}
	return as, nil
}

// PRLink ties a task to its one PR: the repository, the number, the task's
// branch, the head the PR was opened with and the head the operator approved.
type PRLink struct {
	TaskID       int64
	Repo         string
	Number       int
	Branch       string
	OpenedHead   string
	ApprovedHead string
	Opened       time.Time
}

// LinkPR records the PR of task l.TaskID. A task has one PR, and a PR one task
// at a time: linking another number or repository to a task that has a PR, or a
// PR that another task has while that task holds its work (it is not done or
// stopped, holdsWork), is ErrConflict; linking the PR it already has is a no-op
// that leaves the first record as it was. The PR of a task that is done or
// stopped may be linked to another (adoption, milestone 2), and the ended
// task's link stays as it was. Repo and Number are required, Opened defaults to
// the clock, and the task must exist (ErrNotFound).
func (r *Registry) LinkPR(ctx context.Context, l PRLink) error {
	if l.Repo == "" || l.Number <= 0 {
		return errors.New("link pr: a repository and a PR number are required")
	}
	if l.Opened.IsZero() {
		l.Opened = r.now()
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		var repo string
		var number int
		err := tx.QueryRowContext(ctx, "SELECT repo, number FROM pr_links WHERE task_id = ?", l.TaskID).Scan(&repo, &number)
		switch {
		case err == nil && repo == l.Repo && number == l.Number:
			return nil
		case err == nil:
			return fmt.Errorf("link pr: %w: task %d already has %s#%d, not %s#%d", ErrConflict, l.TaskID, repo, number, l.Repo, l.Number)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("link pr: %w", err)
		}
		var holder int64
		err = tx.QueryRowContext(ctx, "SELECT task_id FROM pr_links WHERE repo = ? AND number = ? AND task_id IN (SELECT id FROM tasks WHERE "+
			holdsWork+") ORDER BY task_id LIMIT 1", l.Repo, l.Number).Scan(&holder)
		switch {
		case err == nil:
			return fmt.Errorf("link pr: %w: %s#%d is task %d's, which still holds it", ErrConflict, l.Repo, l.Number, holder)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("link pr: %w", err)
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO pr_links (task_id, repo, number, branch, opened_head, approved_head, opened_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			l.TaskID, l.Repo, l.Number, l.Branch, l.OpenedHead, l.ApprovedHead, store.FormatTime(l.Opened))
		if err != nil {
			return fmt.Errorf("link pr %s#%d to task %d: %w", l.Repo, l.Number, l.TaskID, mapErr(err))
		}
		return nil
	})
}

// LinkedPR returns the PR of a task; a task without one is ErrNotFound.
func (r *Registry) LinkedPR(ctx context.Context, taskID int64) (PRLink, error) {
	var l PRLink
	err := r.db.QueryRowContext(ctx, "SELECT task_id, repo, number, branch, opened_head, approved_head, opened_at FROM pr_links WHERE task_id = ?", taskID).
		Scan(&l.TaskID, &l.Repo, &l.Number, &l.Branch, &l.OpenedHead, &l.ApprovedHead, timeCol{&l.Opened})
	if err != nil {
		return PRLink{}, notFound(err, "PR of task", taskID)
	}
	return l, nil
}
