-- Automatic approvals ([[watch]] auto_approve): an approval magnum posts as the operator's own account (auto_approve_as)
-- on a PR whose latest verified review of the head found nothing that must be fixed before merging, because GitHub
-- does not count a GitHub App's approval. One row per approval, from the post (posting) to its withdrawal (dismissed:
-- by magnum after a review that found blocking problems, by the operator, by someone else, or by GitHub as stale on
-- a push); run_id is the judge run whose review it follows (one approval per review). At most one row of a PR is
-- live (posting, standing or dismissing), which the partial unique index guards. auto_approve_holds are the PRs the
-- operator stopped it on (a dismissal of one of its approvals or a review of their own by hand; `magnum unapprove`):
-- held = 1 stops it, held = 0 records `magnum unapprove --resume`, which counts only what the operator did since at.
-- Applied when PRAGMA user_version < 20. Only adds tables: no deployed table is rewritten.

CREATE TABLE auto_approvals (
  id               INTEGER PRIMARY KEY,
  pr_id            INTEGER NOT NULL REFERENCES prs(id),
  run_id           TEXT NOT NULL,
  source_review_id INTEGER,
  source_url       TEXT,
  head_sha         TEXT NOT NULL,
  identity         TEXT NOT NULL,
  login            TEXT NOT NULL,
  state            TEXT NOT NULL CHECK (state IN ('posting', 'standing', 'dismissing', 'dismissed', 'failed')),
  review_id        INTEGER,
  review_url       TEXT,
  attempts         INTEGER NOT NULL DEFAULT 0,
  error            TEXT,
  ended_by         TEXT,
  end_reason       TEXT,
  created_at       TEXT NOT NULL,
  posted_at        TEXT,
  ended_at         TEXT,
  updated_at       TEXT NOT NULL
);

CREATE UNIQUE INDEX auto_approvals_live ON auto_approvals(pr_id) WHERE state IN ('posting', 'standing', 'dismissing');
CREATE INDEX auto_approvals_pr_run ON auto_approvals(pr_id, run_id);
CREATE INDEX auto_approvals_posted ON auto_approvals(posted_at);

CREATE TABLE auto_approve_holds (
  pr_id  INTEGER PRIMARY KEY REFERENCES prs(id),
  held   INTEGER NOT NULL,
  reason TEXT NOT NULL,
  at     TEXT NOT NULL
);
