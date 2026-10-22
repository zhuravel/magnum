-- The notes critic ([notes] critic): an agent that reviews a curation proposal as the operator does with `magnum notes
-- <repo> --review` and applies, rejects or re-curates it. notes_critiques keeps every critique, whatever became of it, so
-- `magnum notes <repo> --decisions` shows the critic's record after its run directory is pruned: the proposal and its
-- repository, the run's id and directory, when it started and ended, the model it ran on, the fingerprint of the state
-- it reviewed (target_sha256, which its decision must name and an apply must still write), its outcome (applied,
-- rejected, recurated; undecided: no valid decision after a nudge, which counts toward the two attempts a proposal
-- gets; stopped: a shutdown, a pause, an agent that went away, uncounted; changed: the notes changed before its apply,
-- so it is critiqued again, uncounted; overtaken: the operator decided the proposal first; NULL while it runs), the
-- decision it wrote, its one-line reason, its checked and losses lists (JSON arrays of one-line items) and magnum's
-- note on the outcome (detail: the problems of an undecided answer, why it stopped). No retention prunes it. Applied
-- when PRAGMA user_version < 28. Only adds a table and indexes: no deployed table is rewritten.

CREATE TABLE notes_critiques (
  id            INTEGER PRIMARY KEY,
  proposal_id   INTEGER NOT NULL REFERENCES notes_proposals(id),
  repo_id       INTEGER NOT NULL REFERENCES repos(id),
  run_id        TEXT NOT NULL,
  run_dir       TEXT,
  started_at    TEXT NOT NULL,
  ended_at      TEXT,
  model         TEXT,
  target_sha256 TEXT,
  outcome       TEXT CHECK (outcome IN ('applied','rejected','recurated','undecided','stopped','changed','overtaken')),
  decision      TEXT CHECK (decision IN ('apply','reject','recurate')),
  reason        TEXT,
  checked_json  TEXT,
  losses_json   TEXT,
  detail        TEXT
);
CREATE INDEX notes_critiques_proposal ON notes_critiques(proposal_id, id);
CREATE INDEX notes_critiques_repo ON notes_critiques(repo_id, id);
CREATE INDEX notes_critiques_started ON notes_critiques(started_at);
