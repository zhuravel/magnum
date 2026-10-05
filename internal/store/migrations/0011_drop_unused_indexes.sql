-- Two indexes no query ever chose. events_subject(subject, at) (0001) was shadowed by
-- events_subject_id(subject, id) (0004), which serves every lookup of a subject's events and gives their
-- order too; prs_gh_updated(gh_updated_at) (0002) cannot serve the board's or the dispatcher's ordering,
-- which begins with an expression (gh_updated_at IS NULL, COALESCE(gh_updated_at, created_at)). Each cost a
-- write for every event or PR row. EXPLAIN QUERY PLAN on every query in internal/store shows both unused;
-- the retention's per-subject check ("an event inside the window") reads the few rows of one subject through
-- events_subject_id. Applied when PRAGMA user_version < 11. Only drops indexes: no table is rewritten, and
-- IF EXISTS keeps a re-run harmless.

DROP INDEX IF EXISTS events_subject;
DROP INDEX IF EXISTS prs_gh_updated;
