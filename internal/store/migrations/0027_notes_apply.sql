-- A notes proposal is applied in three steps that a crash can interrupt and the next daemon start finishes or reverts
-- (engine.ApplyNotesProposal, engine.recoverNotesApplies): a compare-and-set claims the pending proposal (state
-- applying, the new value of notes_proposals.state's CHECK), the notes and the harness are written, and the proposal
-- moves to applied with its version. The claim records what the recovery needs: prior_version_id, the version of the
-- notes as the claim found them (recorded as an import when the registry did not hold them), and target_sha256, the
-- fingerprint of the state the apply writes (the proposed state, or a stale proposal's merge with the notes' changes
-- since, which the recovery computes again from the base, the prior and the proposed versions). decided_by says who
-- decided a proposal: the operator (`magnum notes <repo> --review`), the notes critic, or magnum itself (an expiry, a
-- supersede, an invalid proposal); the claim sets it, so a decider that lost a race is told who won. Earlier rows are
-- backfilled: every applied and rejected proposal was the operator's (the CLI was the only decider), every expired,
-- invalid and superseded one magnum's. Applied when PRAGMA user_version < 27.
--
-- SQLite cannot change a CHECK, so notes_proposals is rebuilt as 0017 rebuilt it: create notes_proposals_new with the
-- old columns and the new ones, copy (ids kept), and rebuild notes_proposal_misses (0014), the only table that
-- references it, alongside: notes_proposal_misses_new references notes_proposals_new and is filled before either old
-- table is dropped, notes_proposal_misses is dropped first (so the implicit DELETE of DROP TABLE orphans nothing), and
-- renaming notes_proposals_new rewrites notes_proposal_misses_new's REFERENCES clause (SQLite >= 3.26,
-- legacy_alter_table off). notes_versions.proposal_id has no REFERENCES clause and keeps its values. The indexes come
-- back under the same names.

CREATE TABLE notes_proposals_new (
  id                 INTEGER PRIMARY KEY,
  repo_id            INTEGER NOT NULL REFERENCES repos(id),
  kind               TEXT NOT NULL CHECK (kind IN ('curation','restore')),
  trigger_reason     TEXT,
  base_version_id    INTEGER REFERENCES notes_versions(id),
  version_id         INTEGER REFERENCES notes_versions(id),
  applied_version_id INTEGER REFERENCES notes_versions(id),
  changes_json       TEXT,
  state              TEXT NOT NULL CHECK (state IN ('pending','applying','applied','rejected','expired','invalid','superseded')),
  reason             TEXT,
  model              TEXT,
  prompt_sha256      TEXT,
  scratch            TEXT,
  created_at         TEXT NOT NULL,
  decided_at         TEXT,
  decided_by         TEXT CHECK (decided_by IN ('operator','critic','magnum')),
  prior_version_id   INTEGER REFERENCES notes_versions(id),
  target_sha256      TEXT
);
INSERT INTO notes_proposals_new (id, repo_id, kind, trigger_reason, base_version_id, version_id, applied_version_id,
  changes_json, state, reason, model, prompt_sha256, scratch, created_at, decided_at, decided_by)
SELECT id, repo_id, kind, trigger_reason, base_version_id, version_id, applied_version_id,
  changes_json, state, reason, model, prompt_sha256, scratch, created_at, decided_at,
  CASE WHEN state IN ('applied','rejected') THEN 'operator' WHEN state IN ('expired','invalid','superseded') THEN 'magnum' END
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
