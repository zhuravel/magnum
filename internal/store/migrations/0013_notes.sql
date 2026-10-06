-- Repository notes in the registry. notes_versions keeps every state of a repository's notes and its harness
-- directory of QA scripts, forever (no retention prunes these tables): the source of the change (the judge of a
-- round, an applied curation, a person through `magnum notes --edit` or a restore, or an import of what magnum
-- found on disk without a record of it), the round's PR and run, the notes text gzip-compressed (body NULL = the
-- same text as an earlier version of the repository with this sha256, stored there) and, in
-- notes_version_files, the harness files by path, whose bodies are kept once per content in harness_blobs
-- (gzip). notes_proposals keeps every proposal for the notes, a curator's or a restore of an earlier version,
-- whatever became of it: the version it was based on, the proposed state (a version of source 'curation' for a
-- curation; the restored version for a restore), the curator's changes.json with a reason for every item it
-- kept, merged or removed, its state and the operator's reason for a rejection, the curator's model and the
-- hash of its prompt, and the version recorded when it was applied. notes_files and notes_usage count, per
-- harness file, the judge rounds it existed for and the rounds that used it, which marks the unused ones for
-- the curator. Applied when PRAGMA user_version < 13. Only adds tables and indexes: no deployed table is
-- rewritten.

CREATE TABLE notes_versions (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id),
  at          TEXT NOT NULL,
  source      TEXT NOT NULL CHECK (source IN ('judge','curation','human','import')),
  pr_id       INTEGER REFERENCES prs(id),
  run_id      TEXT,
  proposal_id INTEGER,
  bytes       INTEGER NOT NULL,
  sha256      TEXT NOT NULL,
  body        BLOB
);
CREATE INDEX notes_versions_repo ON notes_versions(repo_id, id);
CREATE INDEX notes_versions_sha ON notes_versions(repo_id, sha256);

CREATE TABLE harness_blobs (
  sha256 TEXT PRIMARY KEY,
  bytes  INTEGER NOT NULL,
  body   BLOB NOT NULL
);

CREATE TABLE notes_version_files (
  version_id INTEGER NOT NULL REFERENCES notes_versions(id),
  path       TEXT NOT NULL,
  sha256     TEXT NOT NULL REFERENCES harness_blobs(sha256),
  PRIMARY KEY (version_id, path)
);

CREATE TABLE notes_proposals (
  id                 INTEGER PRIMARY KEY,
  repo_id            INTEGER NOT NULL REFERENCES repos(id),
  kind               TEXT NOT NULL CHECK (kind IN ('curation','restore')),
  trigger_reason     TEXT,
  base_version_id    INTEGER REFERENCES notes_versions(id),
  version_id         INTEGER REFERENCES notes_versions(id),
  applied_version_id INTEGER REFERENCES notes_versions(id),
  changes_json       TEXT,
  state              TEXT NOT NULL CHECK (state IN ('pending','applied','rejected','expired','invalid')),
  reason             TEXT,
  model              TEXT,
  prompt_sha256      TEXT,
  scratch            TEXT,
  created_at         TEXT NOT NULL,
  decided_at         TEXT
);
CREATE INDEX notes_proposals_repo ON notes_proposals(repo_id, id);
CREATE INDEX notes_proposals_state ON notes_proposals(state);

CREATE TABLE notes_files (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  file       TEXT NOT NULL,
  first_seen TEXT NOT NULL,
  rounds     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo_id, file)
);

CREATE TABLE notes_usage (
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  file    TEXT NOT NULL,
  run_id  TEXT NOT NULL,
  used_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, file, run_id)
);
