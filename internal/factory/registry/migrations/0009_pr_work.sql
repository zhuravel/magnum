-- The factory's record of the fix work on one of your PRs (DECISIONS "The factory becomes the only fixer"): one row
-- per PR (its repository, owner/name in any case, and its number), most with no task, since the factory fixes every
-- own PR and published only some of them. fix_off is your opt-out of its fixes (fixwork.FixOff's JSON; '' while it
-- fixes the PR). phase is where its fix loop is: idle (none), fixing (the fixer works on the head the cycle fixes)
-- or reviewing (it pushed, and magnum re-reviews the pushed head); phase_at when it entered it, head the head the
-- cycle fixes (in reviewing, the head it pushed), cycle the cycle of at most cap. The lease is the factory's hold on
-- the PR while it works: who holds it (lease_holder) and until when (lease_until), both set or both empty; magnum
-- reads it, read only, so a lease that ran out (a factory that stopped) holds no review. pushed_json are the heads
-- the factory pushed, with when ([]fixwork.CIPush), ci_fix_json, ci_dispatch_json and ci_rerun_json its CI records
-- (fixwork.CIFixRecord, CIStart, CIRerun; '{}' for none) and ledger_json the open ledger (fixwork.Ledger).
-- flipped_at is when magnum handed the PR's fixes to the factory, imported_at when the factory read magnum's records
-- of it (once), worked_at when it first worked the PR since (a start, a push, a reply, a ledger change): a PR with no
-- worked_at may go back to magnum. Applied when PRAGMA user_version < 9.
CREATE TABLE pr_work (
  id               INTEGER PRIMARY KEY,
  repo             TEXT NOT NULL COLLATE NOCASE CHECK (repo != ''),
  number           INTEGER NOT NULL CHECK (number > 0),
  task_id          INTEGER REFERENCES tasks(id),
  fix_off          TEXT NOT NULL DEFAULT '',
  phase            TEXT NOT NULL DEFAULT 'idle' CHECK (phase IN ('idle','fixing','reviewing')),
  phase_at         TEXT,
  head             TEXT NOT NULL DEFAULT '',
  cycle            INTEGER NOT NULL DEFAULT 0 CHECK (cycle >= 0),
  cap              INTEGER NOT NULL DEFAULT 0 CHECK (cap >= 0),
  lease_holder     TEXT NOT NULL DEFAULT '',
  lease_until      TEXT,
  pushed_json      TEXT NOT NULL DEFAULT '[]',
  ci_fix_json      TEXT NOT NULL DEFAULT '{}',
  ci_dispatch_json TEXT NOT NULL DEFAULT '{}',
  ci_rerun_json    TEXT NOT NULL DEFAULT '{}',
  ledger_json      TEXT NOT NULL DEFAULT '{}',
  flipped_at       TEXT,
  imported_at      TEXT,
  worked_at        TEXT,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  UNIQUE (repo, number),
  CHECK ((lease_holder = '') = (lease_until IS NULL)),
  CHECK (phase = 'idle' OR cycle >= 1)
);
CREATE INDEX pr_work_task ON pr_work(task_id) WHERE task_id IS NOT NULL;
CREATE INDEX pr_work_busy ON pr_work(id) WHERE phase != 'idle';
