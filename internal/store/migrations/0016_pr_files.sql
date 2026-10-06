-- The paths each PR changes, so the judge learns which other PRs of the repository touch the same files
-- (related.json). pr_files holds one row per PR: paths_json, the first page of at most 100 paths GitHub lists in
-- the Details the poller reads for a changed PR, at head_sha, the head they belong to; truncated says the PR
-- changes more files than that page. The poller rewrites the row only when the head moved (or the PR has none);
-- the row stays after the PR closes or merges, and no retention applies, like the PR's. details_at = NULL makes
-- the next poll fetch every open PR's Details once, so the open PRs get their lists (and holds each from dispatch
-- until then, as for a new PR). Applied when PRAGMA user_version < 16. Only adds a table: no deployed table is
-- rewritten.

CREATE TABLE pr_files (
  pr_id      INTEGER PRIMARY KEY REFERENCES prs(id),
  head_sha   TEXT NOT NULL,
  paths_json TEXT NOT NULL DEFAULT '[]',
  truncated  INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  fetched_at TEXT NOT NULL
);

UPDATE prs SET details_at = NULL WHERE gh_state = 'OPEN';
