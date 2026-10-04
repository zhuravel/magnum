-- magnum registry. Timestamps are RFC3339 UTC TEXT. Applied when PRAGMA user_version < 1.

CREATE TABLE repos (
  id              INTEGER PRIMARY KEY,
  node_id         TEXT NOT NULL UNIQUE,
  owner           TEXT NOT NULL,
  name            TEXT NOT NULL,
  watch_owner     TEXT NOT NULL,
  clone_path      TEXT,
  default_branch  TEXT NOT NULL DEFAULT 'master',
  mode            TEXT NOT NULL CHECK (mode IN ('pool','per_pr')),
  first_synced_at TEXT,
  last_seen_at    TEXT NOT NULL,
  UNIQUE(owner, name)
);

CREATE TABLE prs (
  id                    INTEGER PRIMARY KEY,
  repo_id               INTEGER NOT NULL REFERENCES repos(id),
  node_id               TEXT NOT NULL UNIQUE,
  number                INTEGER NOT NULL,
  url                   TEXT NOT NULL,
  title                 TEXT,
  author_login          TEXT,
  author_type           TEXT,
  head_ref              TEXT,
  base_ref              TEXT,
  head_sha              TEXT NOT NULL,
  head_changed_at       TEXT NOT NULL,
  is_draft              INTEGER NOT NULL DEFAULT 0,
  is_cross_repo         INTEGER NOT NULL DEFAULT 0,
  review_requested      INTEGER NOT NULL DEFAULT 0,
  labels_json           TEXT NOT NULL DEFAULT '[]',
  gh_state              TEXT NOT NULL DEFAULT 'OPEN' CHECK (gh_state IN ('OPEN','CLOSED','MERGED','UNKNOWN')),
  gh_updated_at         TEXT,
  merged_at             TEXT,
  closed_at             TEXT,
  missing_since         TEXT,
  confirm_count         INTEGER NOT NULL DEFAULT 0,
  state                 TEXT NOT NULL CHECK (state IN ('baseline','ineligible','queued','claiming','reviewing','verifying','reviewed','rereview_pending','paused','needs_attention','closed','releasing','released')),
  skip_reason           TEXT,
  prev_state            TEXT,
  forced                INTEGER NOT NULL DEFAULT 0,
  identity              TEXT NOT NULL,
  reviewed_sha          TEXT,
  last_review_id        INTEGER,
  last_review_event     TEXT,
  reviewed_at           TEXT,
  last_round_started_at TEXT,
  rounds_today          INTEGER NOT NULL DEFAULT 0,
  rounds_day            TEXT,
  next_eligible_at      TEXT,
  pending_since         TEXT,
  release_after         TEXT,
  attempts              INTEGER NOT NULL DEFAULT 0,
  next_attempt_at       TEXT,
  last_error            TEXT,
  pinned                INTEGER NOT NULL DEFAULT 0,
  muted                 INTEGER NOT NULL DEFAULT 0,
  simplify_done         INTEGER NOT NULL DEFAULT 0,
  human_active_at       TEXT,
  created_at            TEXT NOT NULL,
  updated_at            TEXT NOT NULL,
  UNIQUE(repo_id, number)
);
CREATE INDEX prs_state ON prs(state);

CREATE TABLE slots (
  id                 INTEGER PRIMARY KEY,
  name               TEXT NOT NULL UNIQUE,
  repo_id            INTEGER REFERENCES repos(id),
  repo_full_name     TEXT NOT NULL,
  kind               TEXT NOT NULL CHECK (kind IN ('pool','per_pr','external')),
  path               TEXT NOT NULL UNIQUE,
  main_clone         TEXT NOT NULL,
  placeholder_branch TEXT,
  db_slug            TEXT,
  state              TEXT NOT NULL CHECK (state IN ('provisioning','free','claimed','busy','held','releasing','dirty_schema','broken','removing','removed','lost','observed')),
  pr_id              INTEGER REFERENCES prs(id),
  pinned             INTEGER NOT NULL DEFAULT 0,
  dirty_schema       INTEGER NOT NULL DEFAULT 0,
  checked_out_sha    TEXT,
  hold_reason        TEXT,
  lock_sha           TEXT,
  last_used_at       TEXT,
  last_error         TEXT,
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL
);
CREATE UNIQUE INDEX slots_pr ON slots(pr_id) WHERE pr_id IS NOT NULL;

CREATE TABLE assignments (
  id            INTEGER PRIMARY KEY,
  pr_id         INTEGER NOT NULL REFERENCES prs(id),
  slot_id       INTEGER NOT NULL REFERENCES slots(id),
  path          TEXT NOT NULL,
  db_slug       TEXT,
  db_names_json TEXT NOT NULL DEFAULT '[]',
  head_sha      TEXT,
  started_at    TEXT NOT NULL,
  ended_at      TEXT,
  end_reason    TEXT
);
CREATE UNIQUE INDEX assignments_open_slot ON assignments(slot_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX assignments_open_pr   ON assignments(pr_id)   WHERE ended_at IS NULL;

CREATE TABLE slot_databases (
  id            INTEGER PRIMARY KEY,
  slot_id       INTEGER REFERENCES slots(id),
  db_name       TEXT NOT NULL UNIQUE,
  slug          TEXT NOT NULL,
  size_mb       REAL,
  first_seen_at TEXT NOT NULL,
  last_seen_at  TEXT NOT NULL,
  dropped_at    TEXT,
  dropped_by    TEXT
);

CREATE TABLE sessions (
  id                 INTEGER PRIMARY KEY,
  pr_id              INTEGER NOT NULL REFERENCES prs(id),
  role               TEXT NOT NULL CHECK (role IN ('judge','claude','codex_review','simplify')),
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
CREATE UNIQUE INDEX sessions_live_role ON sessions(pr_id, role) WHERE state IN ('starting','live');
CREATE UNIQUE INDEX sessions_live_agent ON sessions(agent_name) WHERE agent_name IS NOT NULL AND state IN ('starting','live');

CREATE TABLE runs (
  id                TEXT PRIMARY KEY,
  pr_id             INTEGER NOT NULL REFERENCES prs(id),
  round             INTEGER NOT NULL,
  role              TEXT NOT NULL CHECK (role IN ('judge','claude','codex_review','simplify')),
  session_id        INTEGER REFERENCES sessions(id),
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
CREATE INDEX runs_active ON runs(state) WHERE state IN ('pending','submitted','working','ended');

CREATE TABLE requests (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}',
  state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','failed')),
  result       TEXT,
  created_at   TEXT NOT NULL,
  handled_at   TEXT
);

CREATE TABLE events (
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,
  level     TEXT NOT NULL,
  subject   TEXT,
  kind      TEXT NOT NULL,
  step      TEXT,
  phase     TEXT CHECK (phase IN ('begin','ok','fail') OR phase IS NULL),
  message   TEXT NOT NULL,
  data_json TEXT
);
CREATE INDEX events_subject ON events(subject, at);

CREATE TABLE kv (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE notifications (
  key          TEXT PRIMARY KEY,
  last_sent_at TEXT NOT NULL,
  count        INTEGER NOT NULL DEFAULT 1
);
