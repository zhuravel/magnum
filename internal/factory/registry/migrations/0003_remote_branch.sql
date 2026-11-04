-- The branch a task's PR is pushed to (Task.RemoteBranch), recorded when its ready card names it, so the
-- publish, its resume and the later stages push and look up the same branch: the local branch's own name,
-- or, for a task whose Jira key was linked after its branch was made, <login>-<KEY>-<slug> (the local
-- branch keeps its name). '' until a card named it. Applied when PRAGMA user_version < 3.
ALTER TABLE tasks ADD COLUMN remote_branch TEXT NOT NULL DEFAULT '';
