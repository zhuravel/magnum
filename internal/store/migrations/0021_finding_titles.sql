-- A finding's title and the nearby mark, from the judge's provenance (skills/magnum-review/SKILL.md section 8): every
-- entry names its problem in a few words, and `nearby` marks a pre-existing P1 or P2 the judge proved at the head
-- in or near code the PR changes (section 7), which `magnum debt` lists. Rows recorded before have no title and are
-- not nearby. Applied when PRAGMA user_version < 21. Only adds columns: no deployed table is rewritten.

ALTER TABLE findings ADD COLUMN title TEXT;
ALTER TABLE findings ADD COLUMN nearby INTEGER NOT NULL DEFAULT 0 CHECK (nearby IN (0, 1));
