-- A PR's last activity, which the board's UPDATED column shows and sorts by: GitHub's updatedAt (gh_updated_at)
-- also moves for what a reviewer never sees (a project field, a resolved thread, someone's pending review, a
-- deleted comment). activity_at is the latest of the activity the Details read for a changed PR (a comment, a
-- review, a label, a review request, a draft change, a rename, a description edit, a base change, a close, reopen
-- or merge, the head commit, a force push) and the head moves the poller saw; NULL until the next Details fetch,
-- which the screens show as gh_updated_at. gh_updated_at stays what radar change detection and the dispatch order
-- read. details_at = NULL makes the next poll fetch every open PR's Details once (and holds each from dispatch until
-- then, as for a new PR); a closed or merged PR, whose Details are not read again, gets its close or merge.
-- Applied when PRAGMA user_version < 18. Only adds a column: no deployed table is rewritten.

ALTER TABLE prs ADD COLUMN activity_at TEXT;

UPDATE prs SET details_at = NULL WHERE gh_state = 'OPEN';
UPDATE prs SET activity_at = COALESCE(merged_at, closed_at) WHERE gh_state IN ('CLOSED', 'MERGED');
