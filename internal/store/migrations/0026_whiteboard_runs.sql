-- The whiteboard job: magnum prompts the whiteboard role's agent to write the whiteboard of the head it reviewed. That
-- prompt is a run of its own, kind whiteboard, so runs.kind's CHECK takes the new value. Applied when PRAGMA
-- user_version < 26.
--
-- SQLite cannot change a CHECK, so runs is rebuilt as 0025 (and 0015) rebuilt it: create runs_new with the same
-- columns, copy (rowid included: run ordering (created_at, rowid) depends on it), drop runs, rename. Two tables
-- reference runs, and migrations run in one transaction with foreign keys on, so both are rebuilt with it: findings
-- (0005, with the title and nearby columns 0021 added) and declines (0023, whose run_id references runs and whose
-- finding_row_id references findings ON DELETE SET NULL, which dropping findings would otherwise run on every row).
-- findings_new and declines_new reference the new tables and are filled before any old table is dropped, declines is
-- dropped before findings and findings before runs (so the implicit DELETE of DROP TABLE orphans and clears nothing),
-- and renaming a new table to its name rewrites the REFERENCES clauses that name it (SQLite >= 3.26,
-- legacy_alter_table off). Ids are copied as they are (findings.id and declines.id are the rowids the links point
-- at). Their indexes come back under the same names.

CREATE TABLE runs_new (
  id                TEXT PRIMARY KEY,
  pr_id             INTEGER NOT NULL REFERENCES prs(id),
  round             INTEGER NOT NULL,
  role              TEXT NOT NULL CHECK (role <> ''),
  session_id        INTEGER REFERENCES sessions(id),
  kind              TEXT NOT NULL CHECK (kind IN ('initial','rereview','continue','nudge','recovery','own_pass','fix','whiteboard')),
  target_sha        TEXT NOT NULL,
  prev_reviewed_sha TEXT,
  identity          TEXT NOT NULL,
  reviewer_login    TEXT NOT NULL,
  state             TEXT NOT NULL CHECK (state IN ('pending','submitted','working','ended','verified','failed','abandoned')),
  outcome           TEXT,
  report_path       TEXT,
  review_id         INTEGER,
  review_event      TEXT,
  review_commit     TEXT,
  review_url        TEXT,
  result_json       TEXT,
  prompt_text       TEXT NOT NULL,
  created_at        TEXT NOT NULL,
  submitted_at      TEXT,
  working_seen_at   TEXT,
  ended_at          TEXT,
  verified_at       TEXT,
  error             TEXT
);
INSERT INTO runs_new (rowid, id, pr_id, round, role, session_id, kind, target_sha, prev_reviewed_sha, identity,
  reviewer_login, state, outcome, report_path, review_id, review_event, review_commit, review_url, result_json,
  prompt_text, created_at, submitted_at, working_seen_at, ended_at, verified_at, error)
SELECT rowid, id, pr_id, round, role, session_id, kind, target_sha, prev_reviewed_sha, identity,
  reviewer_login, state, outcome, report_path, review_id, review_event, review_commit, review_url, result_json,
  prompt_text, created_at, submitted_at, working_seen_at, ended_at, verified_at, error
FROM runs;

CREATE TABLE findings_new (
  id           INTEGER PRIMARY KEY,
  run_id       TEXT NOT NULL REFERENCES runs_new(id),
  pr_id        INTEGER NOT NULL REFERENCES prs(id),
  round        INTEGER NOT NULL,
  finding_id   TEXT NOT NULL,
  severity     TEXT,
  path         TEXT,
  line         INTEGER,
  sources_json TEXT NOT NULL DEFAULT '[]',
  verdict      TEXT NOT NULL CHECK (verdict IN ('posted','rejected')),
  reason_code  TEXT,
  created_at   TEXT NOT NULL,
  title        TEXT,
  nearby       INTEGER NOT NULL DEFAULT 0 CHECK (nearby IN (0, 1)),
  UNIQUE(run_id, finding_id)
);
INSERT INTO findings_new (id, run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, reason_code,
  created_at, title, nearby)
SELECT id, run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, reason_code,
  created_at, title, nearby
FROM findings;

CREATE TABLE declines_new (
  id             INTEGER PRIMARY KEY,
  pr_id          INTEGER NOT NULL REFERENCES prs(id),
  source_url     TEXT NOT NULL UNIQUE,
  comment_id     INTEGER NOT NULL,
  review_id      INTEGER,
  run_id         TEXT REFERENCES runs_new(id),
  finding_row_id INTEGER REFERENCES findings_new(id) ON DELETE SET NULL,
  path           TEXT,
  line           INTEGER,
  title          TEXT,
  priority       TEXT,
  reviewed_sha   TEXT NOT NULL,
  skill_sha      TEXT,
  outcome        TEXT NOT NULL CHECK (outcome IN ('fixed','declined','deferred','argued','question','unrelated')),
  verdict        TEXT CHECK (verdict IS NULL OR verdict IN ('wrong','over_ranked','missing_context','right')),
  lesson         TEXT,
  scope          TEXT CHECK (scope IS NULL OR scope IN ('repo','general')),
  match_json     TEXT NOT NULL DEFAULT '[]',
  state          TEXT NOT NULL DEFAULT 'new' CHECK (state IN ('new','used','dismissed')),
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
INSERT INTO declines_new (id, pr_id, source_url, comment_id, review_id, run_id, finding_row_id, path, line, title, priority,
  reviewed_sha, skill_sha, outcome, verdict, lesson, scope, match_json, state, created_at, updated_at)
SELECT id, pr_id, source_url, comment_id, review_id, run_id, finding_row_id, path, line, title, priority,
  reviewed_sha, skill_sha, outcome, verdict, lesson, scope, match_json, state, created_at, updated_at
FROM declines;

DROP TABLE declines;
DROP TABLE findings;
DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;
ALTER TABLE findings_new RENAME TO findings;
ALTER TABLE declines_new RENAME TO declines;

CREATE INDEX runs_active ON runs(state) WHERE state IN ('pending','submitted','working','ended');
CREATE INDEX runs_pr_created ON runs(pr_id, created_at);
CREATE INDEX runs_created ON runs(created_at);
CREATE INDEX findings_created ON findings(created_at);
CREATE INDEX findings_pr ON findings(pr_id, round);
CREATE INDEX declines_pr ON declines(pr_id);
CREATE INDEX declines_state ON declines(state, scope, verdict);
CREATE INDEX declines_skill ON declines(skill_sha);
