-- Indexes for the per-PR and queue lookups that scanned whole tables.
-- Applied when PRAGMA user_version < 4. Indexes only: no table is rewritten.
-- IF NOT EXISTS keeps a re-run harmless.

-- RunsByPR, RoleRanBefore and the per-PR run subqueries of ClosedPastGrace and
-- EvictableSlots (WHERE pr_id = ? ORDER BY created_at).
CREATE INDEX IF NOT EXISTS runs_pr_created ON runs(pr_id, created_at);

-- SessionsByPR and LiveSessionByPRRole (WHERE pr_id = ? AND state IN (...)).
-- The partial unique index sessions_live_role cannot serve a bound IN list.
CREATE INDEX IF NOT EXISTS sessions_pr_state ON sessions(pr_id, state);

-- PendingRequests, NextPendingRequest and retention (WHERE state = ? ...).
CREATE INDEX IF NOT EXISTS requests_state_created ON requests(state, created_at);

-- EventsBySubject and StepDone (WHERE subject = ? ... ORDER BY id).
CREATE INDEX IF NOT EXISTS events_subject_id ON events(subject, id);
