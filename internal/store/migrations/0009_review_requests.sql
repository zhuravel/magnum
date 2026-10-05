-- When each PR's review was requested, from whom and by whom, written by the poller from the Details'
-- review requests (the newest ten of the PR's timeline, oldest first): review_requests_json is a list
-- of {at, by, to} with `to` in Account form or "team:<slug>". The board shows when a review was last
-- requested and whether it was requested from the user, also for an old PR whose author asked again
-- recently. details_at = NULL makes the next poll fetch every open PR's Details once, so the older
-- PRs get their history (and holds each from dispatch until then, as for a new PR). Applied when
-- PRAGMA user_version < 9. Only adds a column with a constant default: no deployed table is rewritten.
ALTER TABLE prs ADD COLUMN review_requests_json TEXT NOT NULL DEFAULT '[]';

UPDATE prs SET details_at = NULL WHERE gh_state = 'OPEN';
