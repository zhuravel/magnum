-- The schema a pool slot's databases were last loaded from, so a round reloads them (reset_db) only when its
-- checkout needs another one and the release no longer reloads the base schema eagerly. schema_fp is the
-- fingerprint of the files under the pool's schema_paths at the commit they were loaded from (schema_sha);
-- schema_version is the version db/schema.rb declared there, which the release's schema_migrations check
-- compares with the development database. All three NULL = unknown: the next round reloads. Applied when
-- PRAGMA user_version < 12. Only adds nullable columns: no deployed table is rewritten.
ALTER TABLE slots ADD COLUMN schema_fp TEXT;
ALTER TABLE slots ADD COLUMN schema_sha TEXT;
ALTER TABLE slots ADD COLUMN schema_version TEXT;
