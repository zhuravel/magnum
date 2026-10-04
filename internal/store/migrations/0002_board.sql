-- PR board fields, kept fresh by the poller. Applied when PRAGMA user_version < 2.
-- 0001 is deployed: this file only adds columns, never rewrites a table.

-- Logins of the PR's assignees (JSON array).
ALTER TABLE prs ADD COLUMN assignees_json TEXT NOT NULL DEFAULT '[]';
-- Requested reviewers (JSON array): user logins, teams as "team:<slug>".
ALTER TABLE prs ADD COLUMN requested_reviewers_json TEXT NOT NULL DEFAULT '[]';
-- GitHub's latest review per reviewer (JSON array of {login, state, submitted_at, commit_sha}).
ALTER TABLE prs ADD COLUMN latest_reviews_json TEXT NOT NULL DEFAULT '[]';
-- Size of the change since the last review (JSON {source, base, head, commits, files, additions,
-- deletions, error, computed_at}); NULL until the poller computed it.
ALTER TABLE prs ADD COLUMN since_review_json TEXT;
-- Login (no "[bot]" suffix) that posted the review recorded in reviewed_sha / last_review_event.
ALTER TABLE prs ADD COLUMN last_review_login TEXT;
-- The base branch's tip when Details were last fetched.
ALTER TABLE prs ADD COLUMN base_sha TEXT;
-- When the poller last stored Details for the PR; NULL makes the next poll fetch them once.
ALTER TABLE prs ADD COLUMN details_at TEXT;

CREATE INDEX prs_gh_updated ON prs(gh_updated_at);

-- Reviews recorded before this migration: the reviewer of the PR's latest verified judge run.
UPDATE prs SET last_review_login = (
  SELECT CASE WHEN r.reviewer_login LIKE '%[bot]' THEN substr(r.reviewer_login, 1, length(r.reviewer_login) - 5)
              ELSE r.reviewer_login END
  FROM runs r
  WHERE r.pr_id = prs.id AND r.role = 'judge' AND r.state = 'verified' AND r.review_id IS NOT NULL
  ORDER BY r.verified_at DESC, r.created_at DESC
  LIMIT 1
)
WHERE reviewed_sha IS NOT NULL;
