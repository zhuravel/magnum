-- What the local review of a published task missed (DECISIONS "The factory counts what its local review missed"): at
-- magnum's first round on the task's PR that posted a review and began after the publish (pr_links.opened_at), the
-- round's posted P0 to P2 findings are matched against the task's review_findings by place (internal/escapes: the same
-- path, within 3 lines, at a head whose file reads the same). One escape_checks row per task, written once with its
-- escapes in one transaction and never again: magnum's run that posted the review (runs.id) and its round, the head it
-- reviewed, how many P0 to P2 findings it posted, how many of the task's local findings were placed at a head
-- comparable with it, and when the check ran. One escapes row per posted finding no local finding caught: magnum's
-- finding (findings.id), its path and lines, its priority, the hash of its title (the factory's own, as
-- review_findings keeps it), why (no_local, far or unplaced) and, for far only, the nearest local finding and how many
-- lines lie between them. Neither table holds a title, a body or any other text of magnum's registry or the PR.
-- (GitHub's CI on a published task's PR goes into the checks table as it is: kind ci, source github, one row per
-- task, head and result, which RecordGitHubCI keeps in its transaction.) Applied when PRAGMA user_version < 7.

CREATE TABLE escape_checks (
  task_id    INTEGER PRIMARY KEY REFERENCES tasks(id),
  run_id     TEXT NOT NULL CHECK (run_id != ''),
  round      INTEGER NOT NULL,
  head_sha   TEXT NOT NULL CHECK (head_sha != ''),
  posted     INTEGER NOT NULL DEFAULT 0 CHECK (posted >= 0),
  comparable INTEGER NOT NULL DEFAULT 0 CHECK (comparable >= 0),
  checked_at TEXT NOT NULL
);

CREATE TABLE escapes (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES escape_checks(task_id),
  finding_id INTEGER NOT NULL,
  path       TEXT NOT NULL DEFAULT '',
  start_line INTEGER NOT NULL DEFAULT 0,
  end_line   INTEGER NOT NULL DEFAULT 0,
  priority   TEXT NOT NULL CHECK (priority IN ('P0','P1','P2')),
  title_hash TEXT NOT NULL DEFAULT '',
  reason     TEXT NOT NULL CHECK (reason IN ('no_local','far','unplaced')),
  nearest_id INTEGER REFERENCES review_findings(id),
  distance   INTEGER CHECK (distance IS NULL OR distance > 0),
  found_at   TEXT NOT NULL,
  CHECK ((reason = 'far') = (nearest_id IS NOT NULL AND distance IS NOT NULL)),
  CHECK (reason = 'far' OR (nearest_id IS NULL AND distance IS NULL)),
  UNIQUE (task_id, finding_id)
);
