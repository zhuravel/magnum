-- The factory's half of the retro's misses (DECISIONS "The review retro tags factory-origin misses and their stage").
-- origin is 'factory' for a comment on a pull request the factory published that its local review round did not
-- catch (learn.Build matches it against factory.db's review_findings, read only), NULL for every other comment. stage,
-- for a factory-origin miss the classifier placed, is where it went wrong: plan (the ticket or the operator's answers
-- decided the point, or the factory needed to ask), code (the plan was right, the code is wrong) or review (plan and
-- code are as written, and the local review should have caught it); NULL when the classifier gave none. posted_caught
-- is 1 for a comment magnum's posted round caught that the local round did not: recorded only as a factory-origin miss,
-- never what the review skill's editor, the notes curation or the learned corpus learn from (store.MissFilter leaves
-- it out). origin and stage are checked in Go (store.UpsertMiss), not by a CHECK, so a new value needs no rebuild of
-- the table notes_proposal_misses references. Rows stored before have NULL, NULL and 0. Applied when PRAGMA
-- user_version < 31. Only adds columns: no deployed table is rewritten.

ALTER TABLE misses ADD COLUMN origin TEXT;
ALTER TABLE misses ADD COLUMN stage TEXT;
ALTER TABLE misses ADD COLUMN posted_caught INTEGER NOT NULL DEFAULT 0 CHECK (posted_caught IN (0, 1));
