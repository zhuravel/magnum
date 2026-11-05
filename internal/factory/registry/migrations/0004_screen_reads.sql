-- The factory screen reads the registry every few seconds: the newest alerts across every task (the events of
-- a kind, newest first) and each task's abandoned prompts (its ledger). Both had only a whole-table scan, which
-- grows with the registry's age. Applied when PRAGMA user_version < 4.
CREATE INDEX events_kind ON events(kind, id);
CREATE INDEX ledger_task ON ledger(task_id, id);
