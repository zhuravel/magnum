-- The daily retro looks at PRs magnum reviewed after they closed. retro_prs is its per-PR record: one
-- row once a PR was looked at (how it came out and how many comments were candidates), so the retro
-- does not repeat a PR unless asked; a failed one is tried again until attempts (the failures in a
-- row) reaches three. A retro cut short (a shutdown, a classifier that cannot start, a usage limit)
-- writes no row for the PR it was on. misses holds the comments other reviewers made on those PRs (a
-- review thread or a review body; source_url is GitHub's link to it, the key a re-run upserts by) with
-- what the retro made of each: its class, whether magnum raised the point and the judge rejected it
-- (raised = 'rejected', with finding_ref and reason_code) or never raised it ('none'), a lesson and its
-- scope, and the lines and phrases that recognize the same point again. Applied when PRAGMA
-- user_version < 10. Only adds tables and indexes: no deployed table is rewritten.

CREATE TABLE retro_prs (
  pr_id      INTEGER PRIMARY KEY REFERENCES prs(id),
  retro_at   TEXT NOT NULL,
  day        TEXT NOT NULL,
  status     TEXT NOT NULL CHECK (status IN ('nothing','classified','unclassified','failed')),
  candidates INTEGER NOT NULL DEFAULT 0,
  error      TEXT,
  attempts   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE misses (
  id           INTEGER PRIMARY KEY,
  pr_id        INTEGER NOT NULL REFERENCES prs(id),
  source_url   TEXT NOT NULL UNIQUE,
  source_kind  TEXT NOT NULL CHECK (source_kind IN ('thread','review')),
  reviewer     TEXT NOT NULL,
  path         TEXT,
  line         INTEGER,
  reviewed_sha TEXT NOT NULL,
  class        TEXT NOT NULL CHECK (class IN ('unclassified','miss','not_issue','style','outside')),
  severity     TEXT,
  raised       TEXT NOT NULL CHECK (raised IN ('none','rejected')),
  finding_ref  TEXT,
  reason_code  TEXT,
  title        TEXT,
  lesson       TEXT,
  scope        TEXT CHECK (scope IS NULL OR scope IN ('repo','general')),
  lines_json   TEXT NOT NULL DEFAULT '[]',
  match_json   TEXT NOT NULL DEFAULT '[]',
  state        TEXT NOT NULL DEFAULT 'new' CHECK (state IN ('new','used','dismissed')),
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX misses_pr ON misses(pr_id);
CREATE INDEX misses_class_state ON misses(class, state);
