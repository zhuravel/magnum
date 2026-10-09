-- How authors answered magnum's own findings (the retro, learn.OwnThreads): one row per review thread whose root
-- comment is magnum's and that someone else replied to, keyed by the root comment's link (source_url, the key a
-- re-run upserts by). It keeps where the finding was (comment_id, review_id, the run that posted the review, the
-- findings row on the same review within three lines of it, path and line), its title and priority, the commit the
-- review covered, the judge skill's hash the round ran with (skill_sha, from runs.prompt_text), and the classifier's
-- answer: the outcome (fixed, declined, deferred, argued, question, unrelated), for a declined or argued finding the
-- verdict (wrong, over_ranked, missing_context, right), and for a wrong, over-ranked or missing-context one a lesson,
-- its scope and the phrases that recognize the same finding again. Reply text is never stored. state is new until a
-- lesson was drawn from it (used) or someone dropped it (dismissed); a re-run keeps it. A findings row a round
-- records again is a new row: the link is cleared (ON DELETE SET NULL), the decline stays. Applied when PRAGMA
-- user_version < 23. Only adds a table and indexes: no deployed table is rewritten.

CREATE TABLE declines (
  id             INTEGER PRIMARY KEY,
  pr_id          INTEGER NOT NULL REFERENCES prs(id),
  source_url     TEXT NOT NULL UNIQUE,
  comment_id     INTEGER NOT NULL,
  review_id      INTEGER,
  run_id         TEXT REFERENCES runs(id),
  finding_row_id INTEGER REFERENCES findings(id) ON DELETE SET NULL,
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
CREATE INDEX declines_pr ON declines(pr_id);
CREATE INDEX declines_state ON declines(state, scope, verdict);
CREATE INDEX declines_skill ON declines(skill_sha);
