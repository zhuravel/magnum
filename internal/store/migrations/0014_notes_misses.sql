-- The retro's misses feed the notes curation. notes_proposal_misses links a curation proposal to every miss of its
-- repository (class miss, scope repo) it was given, with what the curator did with it: noted, with the "## "
-- section of the proposed notes that now covers it, or skipped, with a one-line reason. The miss itself stays new
-- while the proposal waits; it becomes used when the proposal is applied, goes back to new when it is rejected or
-- expires, and is dismissed once it was in two rejected proposals (misses.state). The links stay whatever becomes
-- of the proposal, so `magnum misses` names the proposal that used a miss and the next curation reads why earlier
-- ones were rejected; no retention applies. Applied when PRAGMA user_version < 14. Only adds a table and an index:
-- no deployed table is rewritten.

CREATE TABLE notes_proposal_misses (
  proposal_id INTEGER NOT NULL REFERENCES notes_proposals(id),
  miss_id     INTEGER NOT NULL REFERENCES misses(id),
  outcome     TEXT NOT NULL CHECK (outcome IN ('noted','skipped')),
  section     TEXT,
  reason      TEXT,
  PRIMARY KEY (proposal_id, miss_id)
);
CREATE INDEX notes_proposal_misses_miss ON notes_proposal_misses(miss_id);
