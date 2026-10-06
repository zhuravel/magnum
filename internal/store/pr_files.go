package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PRFiles is the paths a PR changes at one head (pr_files): the first page of
// at most 100 that the poller's Details read (a rename lists its new path).
type PRFiles struct {
	HeadSHA   string    // the head the paths belong to
	Paths     []string  // never nil once stored
	Truncated bool      // the PR changes more files than Paths lists
	FetchedAt time.Time // when the list was written (set by the store)
}

// savePRFiles writes f as prID's file list when the PR has none or its list
// belongs to another head: a list is refreshed only when the head moves
// (inside the upsert's transaction).
func (s *Store) savePRFiles(ctx context.Context, tx *sql.Tx, prID int64, f PRFiles) error {
	if f.HeadSHA == "" {
		return nil
	}
	var head string
	err := tx.QueryRowContext(ctx, "SELECT head_sha FROM pr_files WHERE pr_id = ?", prID).Scan(&head)
	switch {
	case err == nil && head == f.HeadSHA:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	paths := f.Paths
	if paths == nil {
		paths = []string{}
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pr_files (pr_id, head_sha, paths_json, truncated, fetched_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (pr_id) DO UPDATE SET head_sha = excluded.head_sha, paths_json = excluded.paths_json,
  truncated = excluded.truncated, fetched_at = excluded.fetched_at`,
		prID, f.HeadSHA, string(b), f.Truncated, FormatTime(s.now()))
	return err
}

// scanPRFiles scans head_sha, paths_json, truncated and fetched_at.
func scanPRFiles(dest *PRFiles) []any {
	return []any{&dest.HeadSHA, jsonCol(&dest.Paths), &dest.Truncated, timeCol(&dest.FetchedAt)}
}

// PRFilesOf returns the PR's stored file list; ok is false when it has none.
func (s *Store) PRFilesOf(ctx context.Context, prID int64) (PRFiles, bool, error) {
	var f PRFiles
	err := s.db.QueryRowContext(ctx, "SELECT head_sha, paths_json, truncated, fetched_at FROM pr_files WHERE pr_id = ?", prID).
		Scan(scanPRFiles(&f)...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PRFiles{}, false, nil
	case err != nil:
		return PRFiles{}, false, fmt.Errorf("files of pr %d: %w", prID, err)
	}
	return f, true, nil
}

// FilesPR is a PR with its stored file list (PRsWithFiles).
type FilesPR struct {
	ID          int64
	Number      int
	URL         string
	GHState     string // GHOpen or GHMerged
	IsDraft     bool
	MergedAt    *time.Time
	ReviewedSHA *string // the head magnum last reviewed; nil = never reviewed
	Files       PRFiles
}

// PRsWithFiles returns the PRs of the repository that have a file list and
// are open (drafts included) or were merged at or after mergedSince, except
// the PR except, by number.
func (s *Store) PRsWithFiles(ctx context.Context, repoID, except int64, mergedSince time.Time) ([]FilesPR, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.number, p.url, p.gh_state, p.is_draft, p.merged_at, p.reviewed_sha,
  f.head_sha, f.paths_json, f.truncated, f.fetched_at
FROM prs p JOIN pr_files f ON f.pr_id = p.id
WHERE p.repo_id = ? AND p.id != ? AND (p.gh_state = 'OPEN' OR (p.gh_state = 'MERGED' AND p.merged_at >= ?))
ORDER BY p.number`, repoID, except, FormatTime(mergedSince))
	if err != nil {
		return nil, fmt.Errorf("prs with files of repo %d: %w", repoID, err)
	}
	out, err := collect(rows, func(sc scanner) (FilesPR, error) {
		var p FilesPR
		dest := append([]any{&p.ID, &p.Number, &p.URL, &p.GHState, &p.IsDraft, nullTime(&p.MergedAt), &p.ReviewedSHA}, scanPRFiles(&p.Files)...)
		return p, sc.Scan(dest...)
	})
	if err != nil {
		return nil, fmt.Errorf("prs with files of repo %d: %w", repoID, err)
	}
	return out, nil
}

// PostedFindingPaths returns, per PR of prIDs that has any, how many findings
// magnum posted on each path over all its rounds (findings without a path
// and rejected candidates excluded).
func (s *Store) PostedFindingPaths(ctx context.Context, prIDs []int64) (map[int64]map[string]int, error) {
	out := map[int64]map[string]int{}
	if len(prIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pr_id, path, count(*) FROM findings
WHERE pr_id IN (`+placeholders(len(prIDs))+`) AND verdict = 'posted' AND path IS NOT NULL AND path != ''
GROUP BY pr_id, path`, int64Args(prIDs)...)
	if err != nil {
		return nil, fmt.Errorf("posted finding paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var path string
		var n int
		if err := rows.Scan(&id, &path, &n); err != nil {
			return nil, fmt.Errorf("posted finding paths: %w", err)
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][path] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posted finding paths: %w", err)
	}
	return out, nil
}
