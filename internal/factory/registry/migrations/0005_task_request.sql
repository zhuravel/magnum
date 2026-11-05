-- The start request that made a task (requests.id), written in the task's own insert, so a start the daemon
-- handled and was killed before it completed the request makes no second task when the restart handles the
-- request again: the replay finds the task by its request and answers with it. 0 for a task made by no
-- request (an older row). Applied when PRAGMA user_version < 5.
ALTER TABLE tasks ADD COLUMN request_id INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX tasks_request ON tasks(request_id) WHERE request_id > 0;
