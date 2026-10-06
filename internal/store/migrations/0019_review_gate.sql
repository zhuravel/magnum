-- What GitHub's merge gate says of a PR's reviews: its reviewDecision and the latest approval or changes request of
-- each reviewer with write access (latestOpinionatedReviews), which that decision counts. A GitHub App's review never
-- counts, so a PR magnum's App approved can still wait for the operator's approval (store.NeedsMe). JSON
-- (store.ReviewGate); NULL until the next Details fetch. details_at = NULL makes the next poll fetch every open PR's
-- Details once (and holds each from dispatch until then, as for a new PR); a closed or merged PR keeps none.
-- Applied when PRAGMA user_version < 19. Only adds a column: no deployed table is rewritten.

ALTER TABLE prs ADD COLUMN review_gate_json TEXT;

UPDATE prs SET details_at = NULL WHERE gh_state = 'OPEN';
