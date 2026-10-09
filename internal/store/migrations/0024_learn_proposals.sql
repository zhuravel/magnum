-- The self-improving review loop's proposals (the skill editor, internal/engine/learn_edit.go). learn_proposals
-- keeps every change the editor drafted to the judge skill and the prompts, whatever became of it: why it ran
-- (trigger_reason: evidence, request, regression), the master commit it started from (base_sha), the branch it
-- was committed on and that commit (candidate_sha), the files it changed (files_json), its state (invalid, stopped,
-- gating, passed, held, failed, landed, rejected, expired; a row is made when its editor run ends), the gate's result (gate_json), a
-- reason (magnum's problems for an invalid one, the operator's for a rejection), the editor's model and the hash of
-- its prompt, its scratch directory and run directory, and when it was made, last began to wait for the landing or the
-- operator (waiting_since: passed or held, which its expiry counts from), decided and landed (landed_sha, the
-- commit master moved to); a revert names the version whose files it restores as they were before it (reverts). learn_proposal_inputs links a proposal to every input it was given, a general miss
-- (kind miss, misses.id) or a general declined finding (kind decline, declines.id), with what the editor did with
-- it: noted, with where the change covers it (location), or skipped, with a one-line reason; what the proposal's gate
-- made of the input's learned case (gate: improved, not_improved, or baseline when the baseline already covered it;
-- NULL when no gate compared it) and why the proposal's decision dismissed the input (dismissed). The inputs stay new
-- while a proposal waits; a landed proposal uses its noted ones, a rejected one dismisses an input that is in two
-- rejected proposals, a gate's decision one its baseline covered, and a failed one an input two failed gates compared
-- without improving it (misses.state, declines.state). learn_versions records every landed change: the proposal, the
-- commit, the hashes of the skill and the prompts it landed, when it landed and when the daemon first restarted on
-- it, which starts its measurement window. No retention prunes these tables. Applied when PRAGMA user_version < 24.
-- Only adds tables and indexes: no deployed table is rewritten.

CREATE TABLE learn_proposals (
  id             INTEGER PRIMARY KEY,
  trigger_reason TEXT NOT NULL CHECK (trigger_reason IN ('evidence','request','regression')),
  base_sha       TEXT NOT NULL,
  branch         TEXT NOT NULL,
  candidate_sha  TEXT,
  files_json     TEXT NOT NULL DEFAULT '[]',
  state          TEXT NOT NULL CHECK (state IN ('invalid','stopped','gating','passed','held','failed','landed',
                   'rejected','expired')),
  gate_json      TEXT,
  reason         TEXT,
  model          TEXT,
  prompt_sha256  TEXT,
  scratch        TEXT,
  run_dir        TEXT,
  created_at     TEXT NOT NULL,
  waiting_since  TEXT,
  decided_at     TEXT,
  landed_sha     TEXT,
  landed_at      TEXT,
  reverts        INTEGER REFERENCES learn_versions(id)
);
CREATE INDEX learn_proposals_state ON learn_proposals(state);

CREATE TABLE learn_proposal_inputs (
  proposal_id INTEGER NOT NULL REFERENCES learn_proposals(id),
  kind        TEXT NOT NULL CHECK (kind IN ('miss','decline')),
  input_id    INTEGER NOT NULL,
  outcome     TEXT NOT NULL CHECK (outcome IN ('noted','skipped')),
  location    TEXT,
  reason      TEXT,
  gate        TEXT CHECK (gate IN ('improved','not_improved','baseline')),
  dismissed   TEXT,
  PRIMARY KEY (proposal_id, kind, input_id)
);
CREATE INDEX learn_proposal_inputs_input ON learn_proposal_inputs(kind, input_id);

CREATE TABLE learn_versions (
  id           INTEGER PRIMARY KEY,
  proposal_id  INTEGER REFERENCES learn_proposals(id),
  landed_sha   TEXT NOT NULL,
  skill_sha    TEXT NOT NULL,
  prompts_sha  TEXT NOT NULL,
  landed_at    TEXT NOT NULL,
  restarted_at TEXT
);
