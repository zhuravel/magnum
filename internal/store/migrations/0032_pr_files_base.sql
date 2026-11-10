-- The base branch each PR's file list was read against (DECISIONS "A standing approval is gated only on a file list
-- of the base the PR names"). base_ref is the branch GitHub compared the head with when it listed the files (the
-- Details' baseRefName): the radar names a PR's base on every poll, while the list is the Details', so a Details
-- call that fails (or lists no files) after a retarget leaves the old base's list, which the files gate of a standing
-- approval must not take for the new base's. Rows stored before have NULL: a base magnum does not know. Applied when
-- PRAGMA user_version < 32. Only adds a column: no deployed table is rewritten.

ALTER TABLE pr_files ADD COLUMN base_ref TEXT;
