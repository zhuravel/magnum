-- Replies to magnum's reviews (W33): the PR's reviews and issue comments that may answer magnum's latest review, as
-- the last Details read listed them: the author's own (not a bot's), and anyone's reply in one of magnum's threads
-- (JSON list of store.Reply: when, by whom, whether in a thread; never what they say). replies_read_at is when
-- magnum's judge last read the threads (the prompt of its last round that posted a review or replied), so the replies
-- after it are the ones not yet re-decided (store.PendingReplies). details_at = NULL makes the next poll read every
-- open PR's Details once; a closed or merged PR keeps none. Applied when PRAGMA user_version < 22. Only adds columns:
-- no deployed table is rewritten.

ALTER TABLE prs ADD COLUMN replies_json TEXT;
ALTER TABLE prs ADD COLUMN replies_read_at TEXT;

UPDATE prs SET details_at = NULL WHERE gh_state = 'OPEN';
