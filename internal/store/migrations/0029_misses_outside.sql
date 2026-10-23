-- Where a retro's comment was made and why it is outside. comment_sha is the commit the comment was made on (a
-- thread's original commit, a review's commit), which reviewed_sha is not when the comment moved to a reviewed commit
-- whose file it did not change, or is outside (then reviewed_sha is the review before it, or its own commit when none
-- was). outside_reason says why a comment of class outside is: before_review (made before magnum's first review of
-- the PR), file_changed (on a later commit whose file changed since the review before it), not_descendant (on a commit
-- that does not descend from that review's), no_compare (GitHub could not compare the two: a 404, a cut file list, no
-- commit), body_unreviewed (a review body on a commit magnum did not review, after one it did) or classifier (the
-- classifier's answer); NULL for any other class. Rows stored before have both NULL. The reasons are checked in Go
-- (store.UpsertMiss), not by a CHECK, so a new one needs no rebuild of the table notes_proposal_misses references.
-- Applied when PRAGMA user_version < 29. Only adds columns: no deployed table is rewritten.

ALTER TABLE misses ADD COLUMN comment_sha TEXT;
ALTER TABLE misses ADD COLUMN outside_reason TEXT;
