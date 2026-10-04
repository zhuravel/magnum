-- The PR author's association with the repository (GraphQL authorAssociation:
-- OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER,
-- MANNEQUIN, NONE), read with the poller's Details; NULL until the next Details
-- fetch, which the poller asks for once for every open PR without it. A branch
-- PR whose author is no longer an owner, member or collaborator was opened by
-- someone who left (skip_departed_authors). Applied when PRAGMA user_version < 6.
-- Only adds a nullable column: no deployed table is rewritten.
ALTER TABLE prs ADD COLUMN author_association TEXT;
