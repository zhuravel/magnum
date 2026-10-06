-- The judge's own pass: magnum prompts the judge with the reviewers, for a pass of its own that it writes to
-- judge-own.md, and prompts it again (its usual initial, rereview or recovery run) once both ended. That first
-- prompt is a run of its own, kind own_pass, so runs.kind's CHECK takes the new value. Applied when PRAGMA
-- user_version < 15.
--
-- SQLite cannot change a CHECK, so runs is rebuilt as 0003 did: create runs_new with the same columns, copy
-- (rowid included: run ordering (created_at, rowid) depends on it), drop runs, rename. findings (0005) is the
-- only table that references runs, and migrations run in one transaction with foreign keys on, so it is rebuilt
-- with it: findings_new references runs_new and is filled before either old table is dropped, findings is
-- dropped before runs (so the implicit DELETE of DROP TABLE orphans nothing), and renaming runs_new to runs
-- rewrites findings_new's REFERENCES clause (SQLite >= 3.26, legacy_alter_table off). Their indexes come back
-- under the same names.

CREATE TABLE runs_new (
  id                TEXT PRIMARY KEY,
  pr_id             INTEGER NOT NULL REFERENCES prs(id),
  round             INTEGER NOT NULL,
  role              TEXT NOT NULL CHECK (role <> ''),
  session_id        INTEGER REFERENCES sessions(id),
  kind              TEXT NOT NULL CHECK (kind IN ('initial','rereview','continue','nudge','recovery','own_pass')),
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
  UNIQUE(run_id, finding_id)
);
INSERT INTO findings_new (id, run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, reason_code, created_at)
SELECT id, run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, reason_code, created_at
FROM findings;

DROP TABLE findings;
DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;
ALTER TABLE findings_new RENAME TO findings;

CREATE INDEX runs_active ON runs(state) WHERE state IN ('pending','submitted','working','ended');
CREATE INDEX runs_pr_created ON runs(pr_id, created_at);
CREATE INDEX runs_created ON runs(created_at);
CREATE INDEX findings_created ON findings(created_at);
CREATE INDEX findings_pr ON findings(pr_id, round);
