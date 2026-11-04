-- The factory registry (a file of its own, apart from magnum's review registry).
-- Timestamps are RFC3339 UTC TEXT with a nine-digit fraction (store.FormatTime), so string order is time
-- order. Applied when PRAGMA user_version < 1.

-- One row per piece of work the factory takes: a Jira key or a free prompt. state says whether the factory
-- or someone else is on the move; waits_who and waits_what say who and for what, in the words of the screen.
CREATE TABLE tasks (
  id         INTEGER PRIMARY KEY,
  jira_key   TEXT NOT NULL DEFAULT '',
  prompt     TEXT NOT NULL DEFAULT '',
  guidance   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT '',   -- a configured kind (bug, feature, chore, ...); data, not a CHECK list
  repo       TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT 'queued'
             CHECK (state IN ('queued','active','needs_you','waiting','done','stopped','failed')),
  stage      TEXT NOT NULL DEFAULT '',
  branch     TEXT NOT NULL DEFAULT '',
  copy       TEXT NOT NULL DEFAULT '',
  head_sha   TEXT NOT NULL DEFAULT '',
  waits_who  TEXT NOT NULL DEFAULT '',
  waits_what TEXT NOT NULL DEFAULT '',
  attempt    INTEGER NOT NULL DEFAULT 0,
  pr_number  INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- One Jira key has at most one task that is not done or stopped (a failed task still holds the key).
CREATE UNIQUE INDEX tasks_active_key ON tasks(jira_key)
  WHERE jira_key != '' AND state NOT IN ('done','stopped');

-- One launch of a stage: the daemon writes the row (launching) before it opens a workspace, so a restart
-- can find what a crash left. The id is t<task>-<stage>-<n>; the herdr workspace label carries it.
CREATE TABLE attempts (
  id              TEXT PRIMARY KEY,
  task_id         INTEGER NOT NULL REFERENCES tasks(id),
  stage           TEXT NOT NULL,
  n               INTEGER NOT NULL,
  state           TEXT NOT NULL DEFAULT 'launching'
                  CHECK (state IN ('launching','running','ended','verified','failed','abandoned')),
  workspace_label TEXT NOT NULL DEFAULT '',
  error           TEXT NOT NULL DEFAULT '',
  started_at      TEXT NOT NULL,
  ended_at        TEXT,
  UNIQUE (task_id, stage, n)
);

-- The questions of a task, in revisions: a new revision voids the open questions of the older ones, and an
-- answer batch is bound to the newest revision. idx is the position within the revision.
CREATE TABLE questions (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER NOT NULL REFERENCES tasks(id),
  revision     INTEGER NOT NULL,
  idx          INTEGER NOT NULL,
  text         TEXT NOT NULL,
  context      TEXT NOT NULL DEFAULT '',
  example      TEXT NOT NULL DEFAULT '',
  options_json TEXT NOT NULL DEFAULT '[]',
  recommended  INTEGER NOT NULL DEFAULT -1,   -- an index into the options; -1 is "no recommendation"
  why          TEXT NOT NULL DEFAULT '',
  answer       TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','answered','voided')),
  asked_at     TEXT NOT NULL,
  answered_at  TEXT,
  UNIQUE (task_id, revision, idx)
);

-- What the operator decided on a card, bound to the head, the report and the diff the card showed.
CREATE TABLE decisions (
  id             INTEGER PRIMARY KEY,
  task_id        INTEGER NOT NULL REFERENCES tasks(id),
  kind           TEXT NOT NULL,
  choice         TEXT NOT NULL,
  note           TEXT NOT NULL DEFAULT '',
  head_sha       TEXT NOT NULL DEFAULT '',
  report_hash    TEXT NOT NULL DEFAULT '',
  diff_hash      TEXT NOT NULL DEFAULT '',
  diff_opened_at TEXT,
  at             TEXT NOT NULL
);
CREATE INDEX decisions_task ON decisions(task_id, id);

-- The prompt ledger: a row before a prompt is sent. delivered stays 'unknown' until the daemon (or its
-- recovery, reading the agent's transcript for the hash) settles it; 'abandoned' is a prompt the factory
-- gave up on (its run abandoned, or the operator let it go): neither proven to have arrived nor sent again.
CREATE TABLE ledger (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  attempt_id TEXT NOT NULL REFERENCES attempts(id),
  role       TEXT NOT NULL,
  run_id     TEXT NOT NULL DEFAULT '',
  hash       TEXT NOT NULL,
  text       TEXT NOT NULL,
  delivered  TEXT NOT NULL DEFAULT 'unknown' CHECK (delivered IN ('unknown','yes','no','abandoned')),
  sent_at    TEXT NOT NULL
);
CREATE INDEX ledger_undelivered ON ledger(id) WHERE delivered = 'unknown';

-- External effects (a push, a PR, a comment, a merge), one row per idempotency key. A key with an 'ok'
-- row is never run again; result holds what the effect made (a PR url, a commit sha).
CREATE TABLE effects (
  key      TEXT PRIMARY KEY,
  kind     TEXT NOT NULL,
  task_id  INTEGER NOT NULL REFERENCES tasks(id),
  state    TEXT NOT NULL DEFAULT 'begin' CHECK (state IN ('begin','ok','fail')),
  result   TEXT NOT NULL DEFAULT '',
  begun_at TEXT NOT NULL,
  ended_at TEXT
);
CREATE INDEX effects_begun ON effects(begun_at) WHERE state = 'begin';

-- The audit log: begin/ok/fail around every destructive step, info for the rest. task_id is NULL for the
-- daemon's own events.
CREATE TABLE events (
  id      INTEGER PRIMARY KEY,
  at      TEXT NOT NULL,
  task_id INTEGER REFERENCES tasks(id),
  kind    TEXT NOT NULL,
  phase   TEXT NOT NULL CHECK (phase IN ('begin','ok','fail','info')),
  text    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX events_task ON events(task_id, id);

-- CLI -> daemon work items, as magnum's requests table.
CREATE TABLE requests (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}',
  state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','failed')),
  result       TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  handled_at   TEXT
);
CREATE INDEX requests_pending ON requests(id) WHERE state = 'pending';
