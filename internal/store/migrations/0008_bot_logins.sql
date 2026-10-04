-- Logins keep GitHub's "[bot]" suffix from now on (github.Account): GraphQL drops it, so an App named
-- like its owner ("zhuravel[bot]" for the user "zhuravel") read as the user, and the board, the
-- approval magnum follows and the review its re-reviews build on could take one for the other.
-- last_review_login of a review a run posted becomes that run's reviewer_login, which always kept the
-- suffix; a review without a run (a manual verdict) keeps what it has. latest_reviews_json and
-- requested_reviewers_json were stored without the suffix: details_at = NULL makes the next poll fetch
-- every open PR's Details once (and hold it from dispatch until then, as for a new PR). Applied when
-- PRAGMA user_version < 8. Only rewrites values: no table changes.
UPDATE prs SET last_review_login = (
  SELECT r.reviewer_login FROM runs r
  WHERE r.pr_id = prs.id AND r.review_id = prs.last_review_id
  ORDER BY r.created_at DESC
  LIMIT 1
)
WHERE last_review_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM runs r WHERE r.pr_id = prs.id AND r.review_id = prs.last_review_id);

UPDATE prs SET details_at = NULL WHERE gh_state = 'OPEN';
