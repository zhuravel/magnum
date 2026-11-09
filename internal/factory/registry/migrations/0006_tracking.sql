-- Where each task's Jira issue stands, as the factory daemon last read it (jira.Client.StatusSince, read off the
-- loop at most every 10 minutes): the key the facts are of, the status's name (one line, cut), its id and Jira's
-- category of it (new, indeterminate, done; '' for another word), since when the issue holds it (NULL when
-- unknown) and whether the changelog was searched for that (a later read of the same status reads no changelog),
-- whether it has an assignee and whether that is you; read_at is the read these facts are from (NULL before one
-- worked), tried_at the last try, worked or not, and last_error that try's failure in the jira package's words, a
-- class and a code ('' after a read that worked), never ticket text. A failure keeps the facts of the read before.
-- One row per task, written whole. Applied when PRAGMA user_version < 6.
CREATE TABLE tracking (
  task_id        INTEGER PRIMARY KEY REFERENCES tasks(id),
  jira_key       TEXT NOT NULL,
  jira_status    TEXT NOT NULL DEFAULT '',
  jira_status_id TEXT NOT NULL DEFAULT '',
  jira_category  TEXT NOT NULL DEFAULT '',
  jira_since     TEXT,
  since_searched INTEGER NOT NULL DEFAULT 0 CHECK (since_searched IN (0,1)),
  assigned       INTEGER NOT NULL DEFAULT 0 CHECK (assigned IN (0,1)),
  assignee_you   INTEGER NOT NULL DEFAULT 0 CHECK (assignee_you IN (0,1) AND assignee_you <= assigned),
  read_at        TEXT,
  tried_at       TEXT NOT NULL,
  last_error     TEXT NOT NULL DEFAULT ''
);
