-- A stale curation proposal (the notes changed since it was made) can be superseded: the operator, or the daemon
-- once such a proposal is a day old and its changes no longer merge with the notes', asks for a new curation that
-- starts from the notes now and reads the stale proposal (its notes, harness and reasons) as input, and the stale
-- one is kept, superseded. notes_proposals.state's CHECK takes the new value. Applied when PRAGMA user_version < 17.
--
-- SQLite cannot change a CHECK, so notes_proposals is rebuilt as 0015 rebuilt runs: create notes_proposals_new
-- with the same columns, copy (ids kept), and rebuild notes_proposal_misses (0014), the only table that references
-- it, alongside: notes_proposal_misses_new references notes_proposals_new and is filled before either old table is
-- dropped, notes_proposal_misses is dropped first (so the implicit DELETE of DROP TABLE orphans nothing), and
-- renaming notes_proposals_new rewrites notes_proposal_misses_new's REFERENCES clause (SQLite >= 3.26,
-- legacy_alter_table off). notes_versions.proposal_id has no REFERENCES clause and keeps its values. The indexes
-- come back under the same names.

CREATE TABLE notes_proposals_new (
  id                 INTEGER PRIMARY KEY,
  repo_id            INTEGER NOT NULL REFERENCES repos(id),
  kind               TEXT NOT NULL CHECK (kind IN ('curation','restore')),
  trigger_reason     TEXT,
  base_version_id    INTEGER REFERENCES notes_versions(id),
  version_id         INTEGER REFERENCES notes_versions(id),
  applied_version_id INTEGER REFERENCES notes_versions(id),
  changes_json       TEXT,
  state              TEXT NOT NULL CHECK (state IN ('pending','applied','rejected','expired','invalid','superseded')),
  reason             TEXT,
  model              TEXT,
  prompt_sha256      TEXT,
  scratch            TEXT,
  created_at         TEXT NOT NULL,
  decided_at         TEXT
);
INSERT INTO notes_proposals_new (id, repo_id, kind, trigger_reason, base_version_id, version_id, applied_version_id,
  changes_json, state, reason, model, prompt_sha256, scratch, created_at, decided_at)
SELECT id, repo_id, kind, trigger_reason, base_version_id, version_id, applied_version_id,
  changes_json, state, reason, model, prompt_sha256, scratch, created_at, decided_at
FROM notes_proposals;

CREATE TABLE notes_proposal_misses_new (
  proposal_id INTEGER NOT NULL REFERENCES notes_proposals_new(id),
  miss_id     INTEGER NOT NULL REFERENCES misses(id),
  outcome     TEXT NOT NULL CHECK (outcome IN ('noted','skipped')),
  section     TEXT,
  reason      TEXT,
  PRIMARY KEY (proposal_id, miss_id)
);
INSERT INTO notes_proposal_misses_new (proposal_id, miss_id, outcome, section, reason)
SELECT proposal_id, miss_id, outcome, section, reason FROM notes_proposal_misses;

DROP TABLE notes_proposal_misses;
DROP TABLE notes_proposals;
ALTER TABLE notes_proposals_new RENAME TO notes_proposals;
ALTER TABLE notes_proposal_misses_new RENAME TO notes_proposal_misses;

CREATE INDEX notes_proposals_repo ON notes_proposals(repo_id, id);
CREATE INDEX notes_proposals_state ON notes_proposals(state);
CREATE INDEX notes_proposal_misses_miss ON notes_proposal_misses(miss_id);
