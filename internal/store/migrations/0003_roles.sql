-- Free-form role names. Applied when PRAGMA user_version < 3.
-- Roles are defined in config now (codex-judge, claude-review, droid-simplify, …),
-- so sessions.role and runs.role lose their CHECK (role IN (…)) and only have
-- to be non-empty. SQLite cannot drop a CHECK, so both tables are rebuilt:
-- create *_new with the same columns, copy, drop the old table, rename.
--
-- Foreign keys: migrations run inside one transaction, where PRAGMA
-- foreign_keys cannot be switched off, so the rebuild is ordered to stay
-- valid with enforcement on. runs_new points at sessions_new, so every copied
-- run is checked against the copied sessions; runs (the only table that
-- references sessions) is dropped before sessions, so the implicit DELETE of
-- DROP TABLE orphans nothing; renaming sessions_new to sessions rewrites the
-- REFERENCES clause in runs_new (SQLite >= 3.26, legacy_alter_table off).
--
-- runs has a TEXT primary key: its rowid is copied explicitly because run ids
-- (MAX(rowid) + 1) and run ordering (created_at, rowid) depend on it.

CREATE TABLE sessions_new (
  id                 INTEGER PRIMARY KEY,
  pr_id              INTEGER NOT NULL REFERENCES prs(id),
  role               TEXT NOT NULL CHECK (role <> ''),
  generation         INTEGER NOT NULL DEFAULT 1,
  agent_name         TEXT,
  agent_kind         TEXT,
  session_id         TEXT,
  resumed_from       TEXT,
  herdr_workspace_id TEXT,
  herdr_tab_id       TEXT,
  herdr_pane_id      TEXT,
  cwd                TEXT,
  env_json           TEXT NOT NULL DEFAULT '{}',
  state              TEXT NOT NULL CHECK (state IN ('starting','live','parked','lost','closed')),
  agent_status       TEXT,
  agent_status_at    TEXT,
  idle_ticks         INTEGER NOT NULL DEFAULT 0,
  started_at         TEXT NOT NULL,
  last_prompt_at     TEXT,
  closed_at          TEXT
);
INSERT INTO sessions_new (id, pr_id, role, generation, agent_name, agent_kind, session_id, resumed_from,
  herdr_workspace_id, herdr_tab_id, herdr_pane_id, cwd, env_json, state, agent_status, agent_status_at,
  idle_ticks, started_at, last_prompt_at, closed_at)
SELECT id, pr_id, role, generation, agent_name, agent_kind, session_id, resumed_from,
  herdr_workspace_id, herdr_tab_id, herdr_pane_id, cwd, env_json, state, agent_status, agent_status_at,
  idle_ticks, started_at, last_prompt_at, closed_at
FROM sessions;

CREATE TABLE runs_new (
  id                TEXT PRIMARY KEY,
  pr_id             INTEGER NOT NULL REFERENCES prs(id),
  round             INTEGER NOT NULL,
  role              TEXT NOT NULL CHECK (role <> ''),
  session_id        INTEGER REFERENCES sessions_new(id),
  kind              TEXT NOT NULL CHECK (kind IN ('initial','rereview','continue','nudge','recovery')),
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

DROP TABLE runs;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
ALTER TABLE runs_new RENAME TO runs;

CREATE UNIQUE INDEX sessions_live_role ON sessions(pr_id, role) WHERE state IN ('starting','live');
CREATE UNIQUE INDEX sessions_live_agent ON sessions(agent_name) WHERE agent_name IS NOT NULL AND state IN ('starting','live');
CREATE INDEX runs_active ON runs(state) WHERE state IN ('pending','submitted','working','ended');

-- The four built-in roles get their config labels. The mapping is one-to-one,
-- so sessions_live_role cannot collide.
UPDATE sessions SET role = CASE role
    WHEN 'judge'        THEN 'codex-judge'
    WHEN 'claude'       THEN 'claude-review'
    WHEN 'codex_review' THEN 'codex-review'
    WHEN 'simplify'     THEN 'claude-simplify'
  END
WHERE role IN ('judge','claude','codex_review','simplify');
UPDATE runs SET role = CASE role
    WHEN 'judge'        THEN 'codex-judge'
    WHEN 'claude'       THEN 'claude-review'
    WHEN 'codex_review' THEN 'codex-review'
    WHEN 'simplify'     THEN 'claude-simplify'
  END
WHERE role IN ('judge','claude','codex_review','simplify');
