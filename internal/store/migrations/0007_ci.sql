-- The CI of each PR's head commit, written by the poller. ci_state is GitHub's check rollup (SUCCESS,
-- FAILURE, PENDING, ERROR, EXPECTED; '' when the head has no checks) as last seen: the radar reads it
-- every poll for a PR from a branch of the repository, its Details otherwise. ci_json is the last
-- Details' view of the checks (store.CIStatus: the commit, counts by state and the latest run of each
-- workflow's check with its state and time), from which the board shows "65/65 passed" or the failed
-- checks. Both are NULL until the next Details fetch, which the poller asks for once for every open PR
-- without ci_json. Applied when PRAGMA user_version < 7. Only adds nullable columns: no deployed table
-- is rewritten.
ALTER TABLE prs ADD COLUMN ci_state TEXT;
ALTER TABLE prs ADD COLUMN ci_json TEXT;
