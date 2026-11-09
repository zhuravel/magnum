-- Who moved a task's Jira issue into its status, as the factory daemon last read it (jira.IssueStatus.MovedBy: the
-- author of the changelog entry jira_since is of): 'you' (its accountId is the account of [jira] user), 'other'
-- (another account, a person or Jira's automation), '' when unknown (no such entry, an entry without an author, or
-- your account could not be read). Never the author's name or id. The factory's shadow plans of its Jira moves tell
-- your move from another's by it. Applied when PRAGMA user_version < 8.
ALTER TABLE tracking ADD COLUMN moved_by TEXT NOT NULL DEFAULT '' CHECK (moved_by IN ('', 'you', 'other'));
