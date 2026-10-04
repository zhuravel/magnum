-- Finding provenance from the judge's result file (skills/magnum-review/SKILL.md section 8), one row
-- per finding the judge judged in a posted round: where it came from, whether it was posted and why
-- it was rejected. `magnum stats` reads it. Applied when PRAGMA user_version < 5. Only adds a table
-- and indexes: no deployed table is rewritten.

CREATE TABLE findings (
  id           INTEGER PRIMARY KEY,
  run_id       TEXT NOT NULL REFERENCES runs(id),
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
CREATE INDEX findings_created ON findings(created_at);
CREATE INDEX findings_pr ON findings(pr_id, round);

-- `magnum stats` reads runs and events by time: runs created in a window, events of a few kinds.
CREATE INDEX IF NOT EXISTS runs_created ON runs(created_at);
CREATE INDEX IF NOT EXISTS events_kind_at ON events(kind, at);
