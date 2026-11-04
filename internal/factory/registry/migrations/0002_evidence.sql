-- Evidence records (spec 11.1): what the factory cannot read later because it did not write it down at the
-- time. Applied when PRAGMA user_version < 2. store.ApplyMigrations runs every pending migration in ONE
-- transaction, where PRAGMA foreign_keys cannot change, so the rebuild of tasks below defers the foreign
-- key checks to the commit instead of switching them off.

PRAGMA defer_foreign_keys = ON;

-- tasks gets the state 'published' (the PR is open and work goes on; done now means merged) and two
-- columns. SQLite cannot alter a CHECK, so this is the rebuild of SQLite's ALTER TABLE page (build, copy,
-- drop, rename), in an order that suits deferred foreign keys: the rows wait in tasks_old, the table is
-- dropped and made again under its own name, and the rows go back into it. The children (attempts,
-- questions, decisions, ledger, effects, events) name tasks(id), so dropping tasks counts each of their
-- rows as a violation, and putting each parent row back into the table named tasks takes that count
-- down again (renaming a table that was built under another name would not); the commit checks that
-- the count is zero.
--   question_rev  the newest question revision of the task, kept here so that a revision that asks nothing
--                 still counts (ReplaceQuestions); 0 = none yet.
--   codex_never   1 = Codex never works on this task again (a cybersecurity warning marked it).
CREATE TABLE tasks_old AS SELECT * FROM tasks;
DROP TABLE tasks;

CREATE TABLE tasks (
  id           INTEGER PRIMARY KEY,
  jira_key     TEXT NOT NULL DEFAULT '',
  prompt       TEXT NOT NULL DEFAULT '',
  guidance     TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL DEFAULT '',   -- a configured kind (bug, feature, chore, ...); data, not a CHECK list
  repo         TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'queued'
               CHECK (state IN ('queued','active','needs_you','waiting','published','done','stopped','failed')),
  stage        TEXT NOT NULL DEFAULT '',
  branch       TEXT NOT NULL DEFAULT '',
  copy         TEXT NOT NULL DEFAULT '',
  head_sha     TEXT NOT NULL DEFAULT '',
  waits_who    TEXT NOT NULL DEFAULT '',
  waits_what   TEXT NOT NULL DEFAULT '',
  attempt      INTEGER NOT NULL DEFAULT 0,
  pr_number    INTEGER NOT NULL DEFAULT 0,
  question_rev INTEGER NOT NULL DEFAULT 0,
  codex_never  INTEGER NOT NULL DEFAULT 0 CHECK (codex_never IN (0,1)),
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

INSERT INTO tasks (id, jira_key, prompt, guidance, kind, repo, title, state, stage, branch, copy, head_sha,
                   waits_who, waits_what, attempt, pr_number, question_rev, created_at, updated_at)
SELECT id, jira_key, prompt, guidance, kind, repo, title, state, stage, branch, copy, head_sha,
       waits_who, waits_what, attempt, pr_number,
       COALESCE((SELECT MAX(revision) FROM questions WHERE questions.task_id = tasks_old.id), 0),
       created_at, updated_at
FROM tasks_old;

DROP TABLE tasks_old;

-- One Jira key has at most one task that is not done or stopped: a published or failed task still holds
-- the key. The same predicate is the holder query of CreateTask and TaskByKey (tasks.go, holdsKey); change
-- all three together.
CREATE UNIQUE INDEX tasks_active_key ON tasks(jira_key)
  WHERE jira_key != '' AND state NOT IN ('done','stopped');

-- Every move of a task between states, with the stage, who it waited on and the reason. CreateTask writes
-- the first span (from_state ''), TransitionTask one per move, in the same transaction.
CREATE TABLE task_spans (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  from_state TEXT NOT NULL DEFAULT ''
             CHECK (from_state IN ('','queued','active','needs_you','waiting','published','done','stopped','failed')),
  to_state   TEXT NOT NULL
             CHECK (to_state IN ('queued','active','needs_you','waiting','published','done','stopped','failed')),
  stage      TEXT NOT NULL DEFAULT '',
  waits_who  TEXT NOT NULL DEFAULT '',
  waits_what TEXT NOT NULL DEFAULT '',
  on_you     INTEGER NOT NULL DEFAULT 0 CHECK (on_you IN (0,1)),   -- 1 = waits_who is 'you'
  reason     TEXT NOT NULL DEFAULT '',
  at         TEXT NOT NULL
);
CREATE INDEX task_spans_task ON task_spans(task_id, id);

-- The tasks of a version 1 file have no first span yet: their current state is it.
INSERT INTO task_spans (task_id, from_state, to_state, stage, waits_who, waits_what, on_you, reason, at)
SELECT id, '', state, stage, waits_who, waits_what, waits_who = 'you', 'migrated from schema version 1', updated_at
FROM tasks;

-- The facts of one role of one stage attempt (an attempt runs several roles). RecordRoleRun upserts by
-- (attempt, role) and never clears a field that is set; '' and NULL mean "not known yet".
-- outcome: verified, failed:<cause>, voided, stopped, lost or paused.
CREATE TABLE attempt_roles (
  id              INTEGER PRIMARY KEY,
  attempt_id      TEXT NOT NULL REFERENCES attempts(id),
  role            TEXT NOT NULL,
  cli             TEXT NOT NULL DEFAULT '',
  model           TEXT NOT NULL DEFAULT '',
  cli_version     TEXT NOT NULL DEFAULT '',
  magnum_build    TEXT NOT NULL DEFAULT '',
  prompt_hash     TEXT NOT NULL DEFAULT '',
  skill_hash      TEXT NOT NULL DEFAULT '',
  config_hash     TEXT NOT NULL DEFAULT '',
  rules_json      TEXT NOT NULL DEFAULT '',   -- [{"id":..., "version":...}]: the rules the prompts carried
  session_id      TEXT NOT NULL DEFAULT '',
  transcript      TEXT NOT NULL DEFAULT '',
  launched_at     TEXT,
  first_output_at TEXT,
  ended_at        TEXT,
  verified_at     TEXT,
  end_fact        TEXT NOT NULL DEFAULT '',
  end_why         TEXT NOT NULL DEFAULT '',
  nudges          INTEGER NOT NULL DEFAULT 0,
  outcome         TEXT NOT NULL DEFAULT ''
                  CHECK (outcome IN ('','verified','voided','stopped','lost','paused') OR outcome GLOB 'failed:?*'),
  tokens_json     TEXT NOT NULL DEFAULT '',   -- {"<model>": tokens}
  active_secs     INTEGER NOT NULL DEFAULT 0,
  idle_secs       INTEGER NOT NULL DEFAULT 0,
  UNIQUE (attempt_id, role)
);

-- Everything the operator said or did to a task, with the proof of where it came from. The caller scrubs
-- the text. text and dropped_reason exclude each other: a dropped input keeps only why it was dropped.
-- before_text is the agent's two turns before the input, cut to 1 KiB. class stays '' until classified.
CREATE TABLE operator_inputs (
  id             INTEGER PRIMARY KEY,
  task_id        INTEGER NOT NULL REFERENCES tasks(id),
  kind           TEXT NOT NULL
                 CHECK (kind IN ('task_text','guidance','answer','decision','send_back','rating','stop','drop','retry','pane_typed','say_one_line')),
  surface        TEXT NOT NULL DEFAULT '',
  stage          TEXT NOT NULL DEFAULT '',
  role           TEXT NOT NULL DEFAULT '',
  text           TEXT NOT NULL DEFAULT '',
  dropped_reason TEXT NOT NULL DEFAULT '',
  origin         TEXT NOT NULL DEFAULT '',
  before_text    TEXT NOT NULL DEFAULT '' CHECK (length(CAST(before_text AS BLOB)) <= 1024),
  agent_state    TEXT NOT NULL DEFAULT '',
  class          TEXT NOT NULL DEFAULT '' CHECK (class IN ('','step_start','approval','correction','access','info')),
  at             TEXT NOT NULL,
  CHECK (text = '' OR dropped_reason = '')
);
CREATE INDEX operator_inputs_task ON operator_inputs(task_id, id);

-- Questions: who raised one and what it cited, when the card showed it, whether the answer took the
-- recommendation, the operator's flag, and the revision that voided it.
ALTER TABLE questions ADD COLUMN raised_by TEXT NOT NULL DEFAULT '';
ALTER TABLE questions ADD COLUMN evidence TEXT NOT NULL DEFAULT '';
ALTER TABLE questions ADD COLUMN shown_at TEXT;
ALTER TABLE questions ADD COLUMN took_recommendation INTEGER NOT NULL DEFAULT 0 CHECK (took_recommendation IN (0,1));
ALTER TABLE questions ADD COLUMN flag TEXT NOT NULL DEFAULT '' CHECK (flag IN ('','needless','needed','unclear'));
ALTER TABLE questions ADD COLUMN flag_why TEXT NOT NULL DEFAULT '';
ALTER TABLE questions ADD COLUMN voided_by INTEGER NOT NULL DEFAULT 0;

-- Every answer a question has had, the first one included, so that an edit loses nothing.
CREATE TABLE question_answers (
  id          INTEGER PRIMARY KEY,
  question_id INTEGER NOT NULL REFERENCES questions(id),
  answer      TEXT NOT NULL,
  at          TEXT NOT NULL
);
CREATE INDEX question_answers_question ON question_answers(question_id, id);

-- The answers and the voids of a version 1 file.
INSERT INTO question_answers (question_id, answer, at)
SELECT id, answer, COALESCE(answered_at, asked_at) FROM questions WHERE state = 'answered';
UPDATE questions SET took_recommendation = 1
WHERE state = 'answered' AND recommended >= 0
  AND trim(answer) = trim(json_extract(options_json, '$[' || recommended || '].label'));
UPDATE questions SET voided_by = COALESCE(
  (SELECT MIN(newer.revision) FROM questions AS newer WHERE newer.task_id = questions.task_id AND newer.revision > questions.revision),
  (SELECT question_rev FROM tasks WHERE tasks.id = questions.task_id))
WHERE state = 'voided';

-- Decisions: when the card was shown, how long the diff was read, whether the recommendation was shown
-- and followed, and the sha256 of the ready patch and of the proof snapshot (artifacts).
ALTER TABLE decisions ADD COLUMN shown_at TEXT;
ALTER TABLE decisions ADD COLUMN diff_dwell_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decisions ADD COLUMN recommendation_shown INTEGER NOT NULL DEFAULT 0 CHECK (recommendation_shown IN (0,1));
ALTER TABLE decisions ADD COLUMN recommendation_followed INTEGER NOT NULL DEFAULT 0 CHECK (recommendation_followed IN (0,1));
ALTER TABLE decisions ADD COLUMN patch TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN proof TEXT NOT NULL DEFAULT '';

-- + and - on a finished task, with the one key for the reason.
CREATE TABLE ratings (
  id                INTEGER PRIMARY KEY,
  task_id           INTEGER NOT NULL REFERENCES tasks(id),
  value             INTEGER NOT NULL CHECK (value IN (-1, 1)),
  reason_key        TEXT NOT NULL DEFAULT '',
  diff_opened_first INTEGER NOT NULL DEFAULT 0 CHECK (diff_opened_first IN (0,1)),
  at                TEXT NOT NULL
);
CREATE INDEX ratings_task ON ratings(task_id, id);

-- One round of the local magnum review (spec 6.5): the heads before and after, the change between them
-- (a patch artifact's sha256), the review skill's hash, the tokens and the exit. exit stays '' while the
-- loop goes on after the round.
CREATE TABLE review_cycles (
  id          INTEGER PRIMARY KEY,
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  n           INTEGER NOT NULL,
  head_before TEXT NOT NULL DEFAULT '',
  head_after  TEXT NOT NULL DEFAULT '',
  delta       TEXT NOT NULL DEFAULT '',
  skill_hash  TEXT NOT NULL DEFAULT '',
  started_at  TEXT NOT NULL,
  ended_at    TEXT,
  tokens      INTEGER NOT NULL DEFAULT 0,
  exit        TEXT NOT NULL DEFAULT '' CHECK (exit IN ('','clean','cap','operator','oscillation','unavailable')),
  UNIQUE (task_id, n)
);

-- A finding, followed across the rounds of a task by its fingerprint (path, lines and a hash of the title).
-- first_cycle and last_cycle are round numbers (review_cycles.n). fix_commit is the last commit that
-- claimed to fix it.
CREATE TABLE review_findings (
  id          INTEGER PRIMARY KEY,
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  fingerprint TEXT NOT NULL,
  path        TEXT NOT NULL DEFAULT '',
  start_line  INTEGER NOT NULL DEFAULT 0,
  end_line    INTEGER NOT NULL DEFAULT 0,
  title_hash  TEXT NOT NULL DEFAULT '',
  priority    TEXT NOT NULL DEFAULT '',
  class       TEXT NOT NULL DEFAULT '',
  sources_json TEXT NOT NULL DEFAULT '[]',
  verdict     TEXT NOT NULL DEFAULT '',
  verdict_why TEXT NOT NULL DEFAULT '',
  blocking    INTEGER NOT NULL DEFAULT 0 CHECK (blocking IN (0,1)),
  first_cycle INTEGER NOT NULL,
  last_cycle  INTEGER NOT NULL,
  outcome     TEXT NOT NULL DEFAULT 'open' CHECK (outcome IN ('open','fixed','waived','rebutted')),
  fix_commit  TEXT NOT NULL DEFAULT '',
  reopened    INTEGER NOT NULL DEFAULT 0,
  UNIQUE (task_id, fingerprint)
);

-- Files of a task's directory by path (relative to it) and sha256, in revisions: a revision is new content
-- for the same kind and path.
CREATE TABLE artifacts (
  id      INTEGER PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id),
  kind    TEXT NOT NULL
          CHECK (kind IN ('base_sha','ticket','guidance','answers','report','proof','patch','pr_body','test_commands')),
  path    TEXT NOT NULL,
  sha256  TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
  rev     INTEGER NOT NULL,
  at      TEXT NOT NULL,
  UNIQUE (task_id, kind, path, rev)
);

-- The one PR of a task. A PR belongs to one task that holds its work (not done or stopped): LinkPR refuses it
-- for another while that task does (tasks.go, holdsWork), and a task that adopts the PR of a stopped one
-- (milestone 2) gets a row of its own, so (repo, number) is indexed, not unique.
CREATE TABLE pr_links (
  task_id       INTEGER PRIMARY KEY REFERENCES tasks(id),
  repo          TEXT NOT NULL,
  number        INTEGER NOT NULL,
  branch        TEXT NOT NULL DEFAULT '',
  opened_head   TEXT NOT NULL DEFAULT '',
  approved_head TEXT NOT NULL DEFAULT '',
  opened_at     TEXT NOT NULL
);
CREATE INDEX pr_links_pr ON pr_links(repo, number);

-- One run of a check on a head: by the writer (its claim), the factory (its own run) or GitHub. cause is
-- set only on a failure, and may stay '' until someone classifies it.
CREATE TABLE checks (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  attempt_id TEXT REFERENCES attempts(id),
  head_sha   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL CHECK (kind IN ('spec','lint','review','ci','qa','migration')),
  result     TEXT NOT NULL CHECK (result IN ('pass','fail','skipped')),
  cause      TEXT NOT NULL DEFAULT '' CHECK (cause IN ('','code','infra','flaky','env','access','not_ours','unknown')),
  source     TEXT NOT NULL CHECK (source IN ('writer','factory','github')),
  command    TEXT NOT NULL DEFAULT '',
  exit_code  INTEGER NOT NULL DEFAULT 0,
  took_ms    INTEGER NOT NULL DEFAULT 0,
  at         TEXT NOT NULL,
  CHECK (cause = '' OR result = 'fail')
);
CREATE INDEX checks_task ON checks(task_id, id);

-- A task suspended while it waits for the operator (open lists what it waits for); at most one is open.
CREATE TABLE suspensions (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER NOT NULL REFERENCES tasks(id),
  stage        TEXT NOT NULL,
  attempt_id   TEXT REFERENCES attempts(id),
  role         TEXT NOT NULL DEFAULT '',
  open_json    TEXT NOT NULL DEFAULT '[]',
  suspended_at TEXT NOT NULL,
  resumed_at   TEXT
);
CREATE UNIQUE INDEX suspensions_open ON suspensions(task_id) WHERE resumed_at IS NULL;

-- An effect that ran again after a crash or a failure.
ALTER TABLE effects ADD COLUMN resumed INTEGER NOT NULL DEFAULT 0 CHECK (resumed IN (0,1));
