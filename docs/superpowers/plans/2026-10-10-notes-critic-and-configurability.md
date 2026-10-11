# Notes critic, pane-job model fallback and configurable policy values: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An agent on the strongest Claude model reviews every notes curation proposal and applies, rejects or re-curates it. When a background pane job's model hits its limit, the job switches to a fallback model. Sixteen policy values become configuration keys.

**Architecture:** The critic is a fourth pane job beside the retro's classifier, the curator and the skill editor. It reuses `paneAgent` and the learn-job serialization (`learnMu`). The CLI's review view and apply move into the engine, so the daemon and `magnum notes --review` share them. Model limits of pane jobs are recorded in the live registry and seeded into each job's scratch registry. Each policy value becomes a field of an existing configuration section with its old constant as the default.

**Tech Stack:** Go 1.27, SQLite (migrations under internal/store/migrations), Go templates for prompts, Bubble Tea v2 for screens, the existing fakes for herdr, git and agents.

**Spec:** docs/superpowers/specs/2026-10-10-notes-critic-and-configurability-design.md

## Global Constraints

- The repository is public. Only `zhuravel`, `alice`, `rev-ann`, `bob-rev` as logins and `talkable` or `example` as organizations in tests, fixtures and docs. `make check-private` runs in `make test`.
- A daemon built from this checkout is live. Never run `bin/magnum` from a test or an agent task. Never run git, mysql, herdr or launchctl against real directories or sockets. Tests open their registry with `storetest.Open`. A test that swaps a package variable or calls `t.Setenv` calls `storetest.Serial(t)` first.
- No new dependencies.
- Prompts load at daemon start. A new prompt file joins the `go:embed` list in prompts/prompts.go, the want list of `TestEveryDefaultPromptHasAGolden`, `TestRenderGolden`, `CheckPrompts` and the `SnapshotPrompts` call.
- `make build` must not run while a new migration is unlanded: the CLI refuses to migrate under the live daemon. Run `go build ./...` and `go test` only.
- Every behaviour change gets a design-log entry in docs/DECISIONS.md, appended at the end of `## Operations`, before `## Learned rules`. Entries are bullets `- **Title** (2026-10-10). text`.
- The defaults keep the critic off: `[notes] critic = false`. Nothing in `config.defaults.toml` names a private organization.
- Commit after each task. Do not push.
- Run `go test -count=1 -race ./<touched packages>` before `make test` at the end of each task. `make lint` before a task lands, fixing only the lines touched.

## Review Focus

1. A proposal decided by the operator while the critic runs. The daemon's `DecideNotesProposal` returns `store.ErrConflict`. Expected: an info event, no toast, no attempt counted. Pinned in Task 8.
2. A judge rewrite during the critique. Expected: a clean merge is applied as reviewed, a conflict counts as an attempt and the next tick critiques again. Pinned in Task 8.
3. Fable and Opus both limited. Expected: the critique stops uncounted, retries after `curateRetry`, and kind `claude` never pauses. Pinned in Task 4.
4. A daemon restart with a critique in flight. Expected: `KVNotesCritic` is cleared at start, the attempt is not counted, the counted attempts survive in the row. Pinned in Tasks 1 and 8.
5. A rejected proposal with misses, rejected by the critic. Expected: the misses stay `new`. An operator rejection still counts toward dismissal, and rows from before the migration count as the operator's. Pinned in Task 1.

---

### Task 1: Registry: deciders, critic columns and the oldest pending curation

**Files:**
- Create: `internal/store/migrations/0025_notes_critic.sql`
- Modify: `internal/store/notes.go:33-60` (constants), `:358-387` (struct, columns, scan), `:389-470` (input, create), `:471-503` (filter, list), `:517-558` (decide), `:562-575` (`NotesFileUse.Unused`)
- Modify: `internal/store/notes_misses.go:68-80` (`decideProposalMisses`)
- Test: `internal/store/notes_critic_test.go` (new), `internal/store/notes_test.go`

**Interfaces:**
- Consumes: the schema of 0017 (`notes_proposals` with 15 columns), `LatestSchemaVersion()`, `scanner`, `nullString`, `nullTime`, `FormatTime`.
- Produces:
  - constants `DecidedByOperator = "operator"`, `DecidedByCritic = "critic"`, `DecidedByMagnum = "magnum"`, `CriticAttemptLimit = 2`.
  - `NotesProposal` gains `DecidedBy string`, `CriticModel string`, `CriticDir string`, `CriticAttempts int`, `CriticAttemptAt *time.Time` (json tags `decided_by`, `critic_model`, `critic_dir`, `critic_attempts`, `critic_attempt_at`, all `omitempty`).
  - `NotesProposalInput` gains nothing. `CreateNotesProposal` sets `decided_by = 'magnum'` when `in.State != ProposalPending`.
  - `func (s *Store) DecideNotesProposal(ctx context.Context, id int64, from []string, to, reason string, by Decider, at time.Time, applied *NotesVersionInput) (NotesProposal, error)` where `type Decider struct { By string; Model string }`. `by.By` must be one of the three constants, else an error. It writes `decided_by` and `critic_model` (the model only when `By == DecidedByCritic`).
  - `decideProposalMisses(ctx, tx, id, to, by, at)` dismisses only when `by.By == DecidedByOperator`, and its count query reads `p.state = 'rejected' AND p.decided_by = 'operator'`.
  - `NotesProposalFilter` gains `Kind string` (empty = any) and `Oldest bool` (ascending id).
  - `func (s *Store) RecordCriticAttempt(ctx context.Context, id int64, dir string, at time.Time) (NotesProposal, error)`: `critic_attempts = critic_attempts + 1`, `critic_attempt_at = at`, `critic_dir = dir`, only while `state = 'pending'`, else `ErrConflict`.
  - `func (s *Store) SetCriticDir(ctx context.Context, id int64, dir string) error`: records the run directory of an attempt that did not count.
  - `func (u NotesFileUse) Unused(rounds int) bool` (was `Unused()`); `NotesUnusedRounds` stays as the default value.

- [ ] **Step 1: Write the failing migration test**

In `internal/store/notes_critic_test.go`, copy the shape of `TestMigrationV23ToV24LearnProposals` (internal/store/learn_proposals_test.go:20) as `TestMigrationV24ToV25NotesCritic`: apply `migrations[:24]`, set `PRAGMA user_version = 24`, insert one `notes_proposals` row per state (`applied`, `rejected`, `expired`, `invalid`, `superseded`, `pending`) with the 0017 column list, reopen with `Open`, assert `v == LatestSchemaVersion() && v >= 25`, then assert per row: applied and rejected have `decided_by = 'operator'`, expired, invalid and superseded have `'magnum'`, pending has NULL, every row has `critic_attempts = 0`, and an `UPDATE notes_proposals SET decided_by = 'robot'` fails the CHECK.

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/store -run TestMigrationV24ToV25NotesCritic -count=1`
Expected: FAIL (no 0025, schema version 24).

- [ ] **Step 3: Write the migration**

`0025_notes_critic.sql`, in the style of 0021 (comment, then `ALTER TABLE ... ADD COLUMN`):

```sql
ALTER TABLE notes_proposals ADD COLUMN decided_by TEXT CHECK (decided_by IN ('operator', 'critic', 'magnum'));
ALTER TABLE notes_proposals ADD COLUMN critic_model TEXT;
ALTER TABLE notes_proposals ADD COLUMN critic_dir TEXT;
ALTER TABLE notes_proposals ADD COLUMN critic_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notes_proposals ADD COLUMN critic_attempt_at TEXT;
UPDATE notes_proposals SET decided_by = 'operator' WHERE state IN ('applied', 'rejected');
UPDATE notes_proposals SET decided_by = 'magnum' WHERE state IN ('expired', 'invalid', 'superseded');
```

The comment says: applied when `PRAGMA user_version < 25`, only adds columns, and why the backfill is the operator's (every earlier apply and rejection was the CLI's).

- [ ] **Step 4: Run the migration test, then the whole store package**

Run: `go test ./internal/store -count=1`
Expected: the migration test passes. `scanNotesProposal` callers still pass because `proposalColumns` is unchanged so far.

- [ ] **Step 5: Write the failing store tests**

In `notes_critic_test.go`, with `storetest.Open`:
- `TestDecideNotesProposalRecordsWhoDecided`: create a pending curation proposal, decide it `rejected` with `Decider{By: DecidedByCritic, Model: "fable"}`, assert `DecidedBy == "critic"`, `CriticModel == "fable"`, `DecidedAt != nil`. Decide another with `Decider{By: "robot"}` and assert an error.
- `TestACriticRejectionDoesNotDismissAMiss`: one miss linked to two proposals (use `ProposalMiss{MissID, Outcome: MissNoted, Section: "Pitfalls"}` through `CreateNotesProposal`), reject both with the critic: the miss stays `MissNew`. Reject two with the operator: the miss is `MissDismissed`. Reject one with the operator and one with the critic: `MissNew`.
- `TestInvalidProposalsAreMagnums`: `CreateNotesProposal` with `State: ProposalInvalid` returns `DecidedBy == "magnum"`.
- `TestNotesProposalsListsTheOldestPendingCuration`: three pending proposals, one a restore; `NotesProposals(ctx, NotesProposalFilter{States: []string{ProposalPending}, Kind: ProposalCuration, Oldest: true, Limit: 1})` returns the oldest curation.
- `TestRecordCriticAttemptCountsAndSurvives`: `RecordCriticAttempt` twice returns `CriticAttempts == 2` with `CriticAttemptAt` set and `CriticDir` the last dir. On a decided proposal it returns `ErrConflict`.
- `TestUnusedTakesTheRounds`: `NotesFileUse{Rounds: 20, Uses: 0}.Unused(20)` is true, `.Unused(25)` false.

- [ ] **Step 6: Run them to see them fail**

Run: `go test ./internal/store -run 'TestDecide|TestACritic|TestInvalid|TestNotesProposalsLists|TestRecordCritic|TestUnused' -count=1`
Expected: compile errors (no `Decider`, no `Kind`/`Oldest`, no `RecordCriticAttempt`).

- [ ] **Step 7: Implement**

In `notes.go`: add the constants and `Decider`. Extend `proposalColumns` with `decided_by, critic_model, critic_dir, critic_attempts, critic_attempt_at` and `scanNotesProposal` with three `sql.NullString`, one int and one `nullTime`. In `CreateNotesProposal` add `decided_by` to the INSERT: `nil` when pending, `DecidedByMagnum` otherwise. In `NotesProposals`, add `AND kind = ?` when `f.Kind != ""` and `ORDER BY id ASC` when `f.Oldest`. Change `DecideNotesProposal`'s signature, validate `by.By` with `oneOf("decider", by.By, deciders)`, write `decided_by = ?, critic_model = COALESCE(?, critic_model)` (the model only for the critic), and pass `by` to `decideProposalMisses`. Add `RecordCriticAttempt` and `SetCriticDir` (`UPDATE ... WHERE id = ? AND state = 'pending'`, `RowsAffected() == 0` → `ErrConflict`). Change `Unused` to take `rounds int`.

Update every caller of `DecideNotesProposal` and `Unused` so the package and the repository compile: `internal/cli/notes_review.go:246, :281, :303, :597`, `internal/engine/notes_curate.go:925`, `internal/engine/notes_stale.go:154`, `internal/engine/notes_curate.go:809, :868`, `internal/cli/notes.go:309, :344`. For now the CLI passes `Decider{By: DecidedByOperator}` for apply and reject and `Decider{By: DecidedByMagnum}` for its expiries. The engine passes `DecidedByMagnum`. The `Unused` callers pass `store.NotesUnusedRounds` (Task 10 switches them to the key).

- [ ] **Step 8: Run the store, engine and CLI tests**

Run: `go build ./... && go test ./internal/store ./internal/engine ./internal/cli -count=1`
Expected: PASS. `docs/API.md` regenerates in Task 12.

- [ ] **Step 9: Commit**

```bash
git add internal/store internal/cli internal/engine
git commit -m "The registry records who decided a notes proposal (decided_by: operator, critic, magnum; migration 0025 backfills the earlier rows as the operator's and magnum's), a miss is dismissed only by the operator's rejections, a curation proposal can be listed oldest first by kind, a critic attempt is counted on the row (critic_attempts, critic_attempt_at, critic_dir), and NotesFileUse.Unused takes the rounds. No behaviour change for the CLI yet. No new dependencies."
```

---

### Task 2: Configuration: the critic keys, per-role fallback lists and the policy keys

**Files:**
- Modify: `internal/config/notes.go:24-42` (struct), `:122-126` (names), `:128-140` (defaults), `:143-147` (`notesModelFollowsKind`), `:152-156` (`NotesRole`), `:163-191` (`validateNotes`)
- Modify: `internal/config/learn.go:19-82` (struct), `:132-181` (`DefaultLearn`), `:119-127` (`learnModelFollowsKind`), `:183-205` (roles), `:272-305` (`validateEditor`)
- Modify: `internal/config/roles.go:25-150` (`Role`), `:274-288` (`DefaultRoles`)
- Modify: `internal/config/config.go:102-145` (`Daemon`), the `Defaults()` literal (`:766-800`), `internal/config/validate.go:160-200` (daemon checks), `:642-664` (one-word check), `:682` (`validateRole`)
- Modify: `internal/config/pipeline.go:22-55` (`Pipeline`), `:65-85` (`Related`, `RelatedFor`), `:289-323` (`mergeRole`: the new role keys)
- Modify: `config.defaults.toml:11-70` ([daemon]), `:158-206` ([learn]), `:214-231` ([notes]), `:240-256` ([pipeline]), `:430-444` (claude-simplify)
- Test: `internal/config/notes_test.go`, `learn_test.go`, `pipeline_test.go`, `defaults_toml_test.go` (existing, extended), `internal/config/critic_test.go` (new)

**Interfaces:**
- Produces, in `config.Notes`: `Critic bool` (`critic`), `CriticKind string` (`critic_kind`), `CriticModel string` (`critic_model`), `CriticEffort string` (`critic_effort`), `CriticArgs []string` (`critic_args`), `CriticPrompt string` (`critic_prompt`), `CriticTimeout Duration` (`critic_timeout`), `CriticFallbackModels []string` (`critic_fallback_models`), `CriticDeclines int` (`critic_declines`), `CriticPerDay int` (`critic_per_day`), `FallbackModels []string` (`fallback_models`), `UnusedRounds int` (`unused_rounds`), `ProposalTTL Duration` (`proposal_ttl`), `CurateInterval Duration` (`curate_interval`).
- Defaults: `Critic: false, CriticKind: KindClaude, CriticModel: "fable", CriticEffort: "xhigh", CriticPrompt: DefaultCriticPrompt, CriticTimeout: 30m, CriticFallbackModels: []string{"opus"}, CriticDeclines: 2, CriticPerDay: 4, UnusedRounds: 20, ProposalTTL: 168h, CurateInterval: 24h`. `FallbackModels` nil.
- `const DefaultCriticPrompt = "notes-critic.md"`, `const CriticRoleName = "notes-critic"`.
- `func (c *Config) CriticRole() Role`: `c.sessionRole(Role{Name: CriticRoleName, Kind: n.CriticKind, Model: n.CriticModel, Effort: n.CriticEffort, Args: n.CriticArgs, Prompt: n.CriticPrompt, Output: "decision.json", Timeout: n.CriticTimeout, FallbackModels: n.CriticFallbackModels})`.
- `NotesRole()` sets `FallbackModels: n.FallbackModels`. `LearnRole()` sets `FallbackModels: l.FallbackModels`, `EditorRole()` sets `FallbackModels: l.EditorFallbackModels`.
- `notesModelFollowsKind` gains: `if md.IsDefined("notes", "critic_kind") && !md.IsDefined("notes", "critic_model") { c.Notes.CriticModel = DefaultLearnModel(c.Notes.CriticKind) }`.
- `validateNotes` gains, in the same wording as the editor's checks: `notes.critic_kind %q is not a declared agent kind`, `validateSessionRole("notes critic", c.CriticRole())`, `notes.critic_prompt must name a prompt file`, `notes.critic_prompt: %w`, `notes.critic_timeout must be positive`, `notes.critic_declines must be positive`, `notes.critic_per_day must be positive`, `notes.unused_rounds must be positive`, `notes.proposal_ttl must be positive`, `notes.proposal_ttl must be at most 720h (the curation directories are kept 30 days)`, `notes.curate_interval must be positive`, and one-word checks on `fallback_models` and `critic_fallback_models` entries (`notes.fallback_models: %q is not one word`).
- In `config.Role`: `FallbackModels []string` (`fallback_models`) and `MaxPosted int` (`max_posted`). `validateRole` checks each fallback entry is one word (`role %s: fallback_models: %q is not one word`) and `max_posted >= 0`. `mergeRole`/`mergeDefined` copy both keys. `DefaultRoles()` sets `MaxPosted: 3` on claude-simplify, and `config.defaults.toml` adds `max_posted = 3` to its block (TestDefaultsEqualExplicitConfig compares them).
- In `config.Learn`: `FallbackModels []string` (`fallback_models`), `EditorFallbackModels []string` (`editor_fallback_models`), `Base string` (`base`, default `"master"`), `EditorMaxInputs int` (`editor_max_inputs`, 40), `GateAttempts int` (`gate_attempts`, 6), `GateMaxAge Duration` (`gate_max_age`, 72h). `validateEditor` checks `base` is one word, the two ints positive, the duration positive.
- In `config.Daemon`: `MaxCodexStarts int` (`max_codex_starts`, 3), `MaxRoundAttempts int` (`max_round_attempts`, 3), `RoundRetryMax Duration` (`round_retry_max`, 30m), `ApprovalCheckWait Duration` (`approval_check_wait`, 1h). Validation: positive.
- In `config.Pipeline`: `RelatedIdle Duration` (`related_idle`, 720h), `RelatedMaxPRs int` (`related_max_prs`, 10), `RelatedMaxPaths int` (`related_max_paths`, 20). `config.Related` gains `Idle time.Duration`, `MaxPRs int`, `MaxPaths int`, filled by `RelatedFor`. Validation: positive.
- A helper `oneWord(section, key string, values []string) []error` in validate.go, used by the kind, role, notes and learn checks (the kind's closure at validate.go:644 becomes a call to it).

- [ ] **Step 1: Write the failing tests**

- `internal/config/critic_test.go`: `TestCriticDefaults` (the `Defaults().Notes` critic fields equal the values above, and `CriticRole()` has `Name == CriticRoleName`, `Kind == "claude"`, `Model == "fable"`, `Effort == "xhigh"`, `Output == "decision.json"`, `FallbackModels == []string{"opus"}`), `TestCriticModelFollowsTheCriticKind` (a local config with `[notes] critic_kind = "codex"` and no `critic_model` gives `CriticModel == ""`, use `loadCommittedWithLocal` as `TestNotesOverlayKeepsTheKeysItDoesNotSet` does), `TestValidateCritic` (zero timeout, zero declines, zero per_day, a two-word fallback entry, `proposal_ttl = "800h"`, `unused_rounds = 0` → the messages above, through `wantError`).
- Extend `TestNotesDefaults`'s `want` literal with the new fields. Extend `TestLearnDefaults`, `withEditorDefaults` and `TestLearnOverlayKeepsTheKeysItDoesNotSet` with `Base`, `EditorMaxInputs`, `GateAttempts`, `GateMaxAge`.
- `TestRoleFallbackModelsAndMaxPosted` in `pipeline_test.go`: a `[[role]]` overlay with `fallback_models = ["opus"]` and `max_posted = 5` on `claude-simplify` reaches `RolesFor(nil)`, and `max_posted = -1` or `fallback_models = ["two words"]` fail validation.
- `TestPolicyKeysHaveDefaults` in `pipeline_test.go`: `Defaults().Daemon.MaxCodexStarts == 3`, `MaxRoundAttempts == 3`, `RoundRetryMax == 30m`, `ApprovalCheckWait == 1h`, `Pipeline.RelatedIdle == 720h`, `RelatedMaxPRs == 10`, `RelatedMaxPaths == 20`, and `RelatedFor(nil)` carries them.
- `TestGoDefaultsEqualTheEmbeddedDefaultsFile` needs no change, but it fails until `config.defaults.toml` carries every new written key.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/config -count=1`
Expected: compile errors on the new fields.

- [ ] **Step 3: Implement the fields, defaults, roles, validation and merge**

As in the Interfaces block. Keep each struct field's comment in the style of its neighbours (what it is, the default, the unit).

- [ ] **Step 4: Write the keys into config.defaults.toml**

Append to `[notes]` after `timeout`:

```toml
# fallback_models = []            # the curator's models when `model` hits its own limit, in order
                                  # ([] = the kind's fallback_models)
unused_rounds = 20                # judge rounds a harness file must go unused before a curation can drop it
proposal_ttl = "168h"             # a proposal nobody decided expires after this (at most 720h: the curation
                                  # directories are kept 30 days)
curate_interval = "24h"           # at most one automatic curation of a repository per interval; a stale proposal
                                  # whose changes conflict is superseded after it
critic = false                    # an agent reviews each curation proposal and applies, rejects or re-curates it;
                                  # false: proposals wait for `magnum notes <repo> --review`
critic_kind = "claude"            # the critic: a [kinds.<name>] that drives a session; checked like a [[role]]
critic_model = "fable"            # the strongest model: a second opinion on the curator's (sonnet) work
critic_effort = "xhigh"           # passed through the kind's `effort` args ("" = none)
# critic_args = []                # appended to the critic's launch args
critic_prompt = "notes-critic.md" # in prompts_dir; the embedded copy when absent
critic_timeout = "30m"            # per turn: a turn that takes longer is abandoned
critic_fallback_models = ["opus"] # when critic_model hits its own limit, in order; [] = the kind's list
critic_declines = 2               # rejections and re-curations by the critic in a row after which the
                                  # repository's next proposal waits for the operator
critic_per_day = 4                # critiques started per local day, over every repository
```

Change the `curate` comment's "At most one curation of a repository a day" to "At most one automatic curation per `curate_interval`" and "nothing changes until you accept it with `magnum notes <repo> --review`" to "nothing changes until you accept it with `magnum notes <repo> --review`, or the critic does".

In `[learn]`: `base = "master"`, `editor_max_inputs = 40`, `gate_attempts = 6`, `gate_max_age = "72h"`, `# fallback_models = []`, `# editor_fallback_models = []`, each with a one-line comment from the constant's comment.

In `[daemon]`: `max_codex_starts = 3`, `max_round_attempts = 3`, `round_retry_max = "30m"`, `approval_check_wait = "1h"`. Change the `agent_start_stagger` comment "(and at most 3 Codex launches at once)" to "(and at most max_codex_starts Codex launches at once)" and the `delta_check` comment "at most 1h" to "at most approval_check_wait".

In `[pipeline]`: `related_idle = "720h"`, `related_max_prs = 10`, `related_max_paths = 20`, and the `related_lookback` comment "never an open PR idle for 30 days" becomes "never an open PR idle for related_idle".

In `[[role]] claude-simplify`: `max_posted = 3` with the comment "the judge posts at most this many of its proposals", and the `prompt` comment "(the judge posts at most 3)" becomes "(the judge posts at most max_posted)".

- [ ] **Step 5: Run the config tests**

Run: `go test ./internal/config -count=1 && go build ./...`
Expected: PASS, including `TestGoDefaultsEqualTheEmbeddedDefaultsFile` and `TestDefaultsEqualExplicitConfig`.

- [ ] **Step 6: Commit**

```bash
git add internal/config config.defaults.toml
git commit -m "Configuration for the notes critic ([notes] critic, critic_kind, critic_model, critic_effort, critic_args, critic_prompt, critic_timeout, critic_fallback_models, critic_declines, critic_per_day; Config.CriticRole, CriticRoleName notes-critic), per-role fallback lists (fallback_models on [[role]], [notes], [learn] and the editor and critic), and the policy keys that were constants: [notes] unused_rounds, proposal_ttl, curate_interval; [learn] base, editor_max_inputs, gate_attempts, gate_max_age; [daemon] max_codex_starts, max_round_attempts, round_retry_max, approval_check_wait; [pipeline] related_idle, related_max_prs, related_max_paths; [[role]] max_posted (3 on claude-simplify). Keys only: the code reads them in later commits. No new dependencies."
```

---

### Task 3: Agents: per-role fallback lists, a live limit writer, cooldown doubling and the consent-dialog pattern

**Files:**
- Modify: `internal/agents/models.go:146-193` (`activeLimits`, `firstFallback`), `:195-213` (`ModelLimits`), `:251-316` (`NoteModelLimit`), `:318-332` (`FallbackModel`), `:640-659` (`startModel`), `:680-720` (`ensureModel`)
- Modify: `internal/config/kinds.go:319-323` (model_limit patterns)
- Test: `internal/agents/models_test.go` (existing patterns), `internal/agents/health_test.go`, `internal/config/model_limit_test.go`

**Interfaces:**
- Consumes: `config.Role.FallbackModels` (Task 2), `config.Kind.FallbackModels`, `KVModelLimits(kind)`, `KVModelLimited(kind, model)`.
- Produces:
  - `func fallbacksFor(role config.Role, k config.Kind) []string`: the role's list when not empty, else the kind's.
  - `func RecordModelLimit(ctx context.Context, st *store.Store, kind, model string, until time.Time, now time.Time) (time.Time, error)`: exported. The index and row logic of `NoteModelLimit` lines 281-301 moved here. It returns the effective `until`. Doubling: when the row already held an active limit recorded within the last 24 hours for the same model (a sibling kv `KVModelLimitedAt(kind, model)` written here with `now`), the new cooldown is doubled, capped at `MaxReset` (24h); a message with a reset time keeps the reset time. `NoteModelLimit` calls it.
  - `func StartModelFor(ctx context.Context, st *store.Store, role config.Role, k config.Kind, now time.Time) (model string, fallback bool, none bool)`: the body of `startModel` as a package function reading `st`, with `none == true` when the role's model is limited and no fallback is free (today `startModel` then returns the limited model). `Manager.startModel` calls it and keeps its two-value shape.
  - `startModel`'s guard becomes `len(k.Model) == 0 || len(fallbacksFor(role, k)) == 0` (the switch command is not needed for a fresh launch).
  - `ModelLimits`, `NoteModelLimit` (the `Using` line), `FallbackModel`, `ensureModel` use `fallbacksFor(role, k)`.
  - A new default `model_limit` pattern in kinds.go: `continuing on (?P<model>fable)\b[\w .-]* uses usage credits`.

- [ ] **Step 1: Write the failing tests**

In `internal/agents/models_test.go`, following the file's existing fixtures for a manager over `storetest.Open`:
- `TestARoleFallbackListWinsOverTheKinds`: kind `claude` with `["opus", "sonnet"]`, a role with `FallbackModels: []string{"opus"}`; with fable and opus limited, `FallbackModel` returns `false` and `StartModelFor` returns `none == true`; with only fable limited, both return `opus`.
- `TestRecordModelLimitDoublesARepeatWithinADay`: record fable at `now` with a 5h cooldown → until `now+5h`; record again at `now+6h` (the first expired) → `now+6h+10h`; a third at `now+17h` → capped at `now+17h+24h`. A record with a reset time keeps it.
- `TestStartModelSkipsALimitSeededInTheStore`: `SetKV` the two keys for fable in the store, `StartModelFor` for a fable role returns `opus, true, false`.

In `internal/agents/health_test.go`, add a fixture line `Fable limit reached · continuing on Fable 5.1 uses usage credits, and the prompt to confirm went unanswered — nothing was sent · answer it where this session is running, or /model to change` classified as `HealthModelLimit` with `Model == "fable"` through `defaultHealth` (the existing table style).

In `internal/config/model_limit_test.go`, extend `TestResetModelLimitPatternsSeparateModelFromAccountLimits` with the same line.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/agents ./internal/config -run 'Fallback|RecordModelLimit|StartModel|ModelLimit' -count=1`
Expected: compile errors, then the fixture failing.

- [ ] **Step 3: Implement**

As in the Interfaces block. `NoteModelLimit` keeps its signature and event. The pattern goes at the end of the `ModelLimit` list in `DefaultHealthPatterns`.

- [ ] **Step 4: Run the agents, config and pipeline tests under race**

Run: `go test -count=1 -race ./internal/agents ./internal/config ./internal/pipeline`
Expected: PASS (the pipeline's fallback tests still pass: rounds go through `FallbackModel` and `NoteModelLimit`).

- [ ] **Step 5: Commit**

```bash
git add internal/agents internal/config
git commit -m "A role's fallback_models wins over its kind's in every model-limit path (fallbacksFor: startModel, ensureModel, FallbackModel, NoteModelLimit, ModelLimits), a fresh launch needs no switch_model to start on a fallback, RecordModelLimit writes a limit for a kind and model into any registry (NoteModelLimit calls it) and doubles the cooldown of a limit repeated within a day up to MaxReset, StartModelFor says when no fallback is free, and the Fable usage-credits consent wording classifies as a model limit of fable. No new dependencies."
```

---

### Task 4: Pane jobs: the live limit record, the in-job restart and the blocked-dialog wait

**Files:**
- Modify: `internal/engine/retro_pane.go:62-69` (`paneAgents`), `:74-94` (`paneDeps`), `:148-169` (`paneAgent`), `:171-178` (`paneTurn`), `:188-230` (`Classify`), `:284-328` (`setup`), `:460-488` (`wait`), `:529-560` (`health`), `:595-640` (`release`)
- Modify: `internal/engine/notes_curate.go:136-200` (`paneCurator.Curate`, `CurateResult`), `:661-666` (the pause branch), `:675-676` (the proposal's model)
- Modify: `internal/engine/learn_edit.go:97-102` (`EditResult`), `:151-250` (`paneEditor.Edit`), `:509` (the model), `:589-593` (the pause branch)
- Modify: `internal/engine/retro.go:119-130` (`ClassifyResult`), `:619-623` (the pause branch)
- Modify: `internal/engine/engine.go:492-501` (`FromApp`: `Store: a.Store` in `paneDeps`)
- Test: `internal/engine/retro_pane_test.go` (existing fakes), `internal/engine/notes_curate_test.go`

**Interfaces:**
- Consumes: `agents.RecordModelLimit`, `agents.StartModelFor`, `agents.KVModelLimits`, `agents.KVModelLimited`, `agents.KVSessionModel` (Task 3), `store.Session.AgentStatus` and `AgentStatusAt`.
- Produces:
  - `paneDeps.Store *store.Store` (the live registry; nil in tests that do not need it).
  - `const paneBlockedWait = 2 * time.Minute`.
  - `paneAgents` gains `NoteModelLimit(ctx context.Context, s store.Session, h agents.Health) (agents.ModelLimit, error)`.
  - `paneTurn` gains `blocked bool` (the turn ended because the session stayed `blocked` for `paneBlockedWait`; escape sent, run abandoned).
  - `type paneHealth struct { Pause *pipeline.Pause; Limited *agents.Health }`. `health` returns `paneHealth`: `Limited` set for `HealthModelLimit` (the `Pause` nil), `Pause` for the other three kinds.
  - `func (c *paneAgent) seedLimits(ctx context.Context) error`: called by `setup` after `store.Open`, copies `KVModelLimits(kind)` and each `KVModelLimited(kind, model)` from `c.d.Store` into `c.st` when `c.d.Store != nil`.
  - `func (c *paneAgent) recordLimit(ctx context.Context, s store.Session, h agents.Health)`: `c.mgr.NoteModelLimit` on the scratch registry, then `agents.RecordModelLimit` on `c.d.Store` with the same kind, model and until.
  - `func (c *paneAgent) restartOnFallback(ctx context.Context) (bool, error)`: `release`, then `agents.StartModelFor` on `c.st` for `c.role`: when `none`, return `false, nil`; else `ensureAgent` and return `true, err`. At most once per job (`c.restarted bool`).
  - `func (c *paneAgent) model(ctx context.Context) string`: the session's `KVSessionModel` from `c.st` when set, else `c.d.Config.RoleModel(c.role)`.
  - `ClassifyResult`, `CurateResult`, `EditResult` gain `Model string`, set by each driver from `c.model(ctx)` on return.
  - Each driver loop: after `turn`, when `h := c.health(...)` has `Limited`: `c.recordLimit(ctx, s, *h.Limited)`; `ok, err := c.restartOnFallback(ctx)`; when `ok`, resubmit the same `text` without counting a nudge (`continue` with `attempt` unchanged); when not ok, return the job's down error wrapped as `fmt.Errorf("%w: %s is limited and no fallback model is free", ErrCuratorDown, model)` (and the classifier's and editor's equivalents). The callers already treat the down errors as a stop (`curateTried`, `retroRetry`, `scheduledEditStopped`), so no kind pause happens.
  - `wait`: when the session row shows `AgentStatus == "blocked"` and `AgentStatusAt` older than `paneBlockedWait`, interrupt, finish with `errors.New("the agent waited on a dialog for " + paneBlockedWait.String())`, return `paneTurn{blocked: true, err: ...}`; the driver then runs `health` as for a timeout.
  - `runCurate` stores `Model: res.Model` (falls back to the configured model when empty); `runEditAt` likewise.

- [ ] **Step 1: Write the failing tests**

In `internal/engine/retro_pane_test.go`, with the file's fake `paneAgents` (extend the fake with `NoteModelLimit` that records calls and writes the two kv keys to the store it was given):
- `TestAPaneJobRestartsOnTheFallbackWhenItsModelIsLimited`: a curator job whose first turn's pane text is "You've hit your Fable limit · resets 3:45pm"; assert the fake saw `NoteModelLimit`, the live store (`paneDeps.Store`) has `KVModelLimited("claude", "fable")`, a second `StartAgent` happened with the fallback model, and `CurateResult.Model == "opus"`.
- `TestAPaneJobSeedsTheLimitsRoundsRecorded`: set the fable limit in the live store first; the first `StartAgent` already asks for `opus`.
- `TestAPaneJobWithNoFallbackLeftStopsAndNeverPausesTheKind`: fable and opus limited, the role's list `["opus"]`: `Curate` returns an error wrapping `ErrCuratorDown`, no `Pause`, and `runCurate` (through the harness) records `curateTried` and leaves `kindPauseReason("claude")` empty.
- `TestATurnBlockedOnAnUnknownDialogEndsAfterTwoMinutes`: the fake session's `AgentStatus` is `blocked` from the start; with the fake clock past `paneBlockedWait`, `wait` returns `blocked == true` and escape was sent (the fake `Keys` recorder).

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/engine -run 'TestAPaneJob|TestATurnBlocked' -count=1`
Expected: compile errors (no `Store` in `paneDeps`, no `Model` in results).

- [ ] **Step 3: Implement**

As in the Interfaces block. Keep `Close` as it is. In `FromApp`, set `Store: a.Store`.

- [ ] **Step 4: Run the engine package under race**

Run: `go test -count=1 -race ./internal/engine`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine
git commit -m "A pane job (the retro's classifier, the curator, the skill editor) whose model hits its own limit records the limit in the live registry and its scratch one, releases its agent and starts again once on the first free fallback with the same prompt, instead of stopping on the same model every time; a job seeds its scratch registry with the limits rounds recorded; no fallback left stops the job as before and never pauses the kind; a turn whose session waits on an unknown dialog for 2 minutes ends and is read for a limit; each result carries the model the answer came from, which the proposal records. No new dependencies."
```

---

### Task 5: The review view and the apply move into the engine

**Files:**
- Create: `internal/engine/notes_review.go`
- Modify: `internal/cli/notes_review.go:86-137` (the types), `:350-452` (`notesReviewOf`, `notesReviewFrom`), `:569-598` (`notesApply`), `:228-316` (the callers), `internal/cli/notes_history.go` (restore preview)
- Test: `internal/engine/notes_review_test.go` (new), `internal/cli/notes_review_test.go` (existing: the JSON shape test keeps passing)

**Interfaces:**
- Consumes: `engine.CheckStale`, `engine.StaleCheck`, `notes.Unified`, `notes.HarnessDelta`, `notes.Changes`, `store.ProposalMisses`, `store.NotesProposal`.
- Produces, in `internal/engine/notes_review.go`:
  - `type NotesReviewView struct` with the exported fields of `notesReviewData` and the same json tags (`Repo`, `Proposal`, `Diff`, `Harness []NotesHarnessChange`, `Sections []notes.Change`, `Misses []NotesReviewMiss`, `Before`, `After`, `Expired`, `Stale`, `Merge *NotesMergeView`) plus exported `Base`, `Proposed`, `Live`, `Merged notes.State` and `Changes notes.Changes`, all tagged `json:"-"`.
  - `type NotesHarnessChange`, `NotesReviewMiss`, `NotesMergeView`: the CLI's three types moved and exported, same tags.
  - `func ReviewProposal(ctx context.Context, st *store.Store, maxLine int, full string, nr notes.Repo, p store.NotesProposal) (NotesReviewView, error)` (`CheckStale` then `ReviewFrom`).
  - `func ReviewFrom(ctx context.Context, st *store.Store, maxLine int, full string, p store.NotesProposal, check StaleCheck) (NotesReviewView, error)`: the body of `notesReviewFrom`. The CLI's `missesPlace` helper moves along as `missPlace` if it is only used there, else the CLI passes it; check `internal/cli/misses.go`.
  - `var ErrNotesChanged = errors.New("the notes changed while the proposal was shown")`.
  - `func ApplyNotesProposal(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal, live string, target store.NotesContent, stale bool, by store.Decider, now time.Time) (store.NotesProposal, error)`: the body of `notesApply` with `by` passed to `DecideNotesProposal`.
  - `func (v NotesReviewView) Target() notes.State`: `Merged` when `Stale`, else `Proposed`.
- The CLI keeps `errNotesChanged` as a wrapped message: `fmt.Errorf("%w; `--review` again shows it merged with their changes", engine.ErrNotesChanged)`, and `notesReviewData` becomes an alias `type notesReviewData = engine.NotesReviewView`.

- [ ] **Step 1: Write the failing test**

`internal/engine/notes_review_test.go`, `TestReviewProposalAndApplyMatchTheCLI`: with `storetest.Open` and a `t.TempDir()` notes repository (see how `notes_stale_test.go` builds one), create a pending proposal whose proposed state differs from the live one, call `ReviewProposal`, assert `Diff` has a `+` line with the new text, `Before.Bytes != After.Bytes`, then `ApplyNotesProposal(... live: view.Live.Fingerprint(), target: ContentOf(view.Target()), stale: false, by: Decider{By: DecidedByOperator} ...)` returns an applied proposal with `DecidedBy == "operator"` and the file on disk holds the new text. A second apply with a wrong fingerprint returns `ErrNotesChanged`.

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/engine -run TestReviewProposalAndApply -count=1`
Expected: compile error.

- [ ] **Step 3: Move the code**

Create the file from the CLI's functions, export, and make the CLI call `engine.ReviewProposal`, `engine.ReviewFrom` and `engine.ApplyNotesProposal` (with `store.Decider{By: store.DecidedByOperator}`). The CLI's JSON output must stay byte-identical: run `go test ./internal/cli -run Notes -count=1` and diff any golden the package keeps.

- [ ] **Step 4: Run the engine and CLI tests**

Run: `go test -count=1 -race ./internal/engine ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine internal/cli
git commit -m "The review of a notes proposal (the diff, the harness changes with the curator's reasons, the sections, the misses, the sizes, the stale merge) and its apply under the notes lock move from the CLI into the engine (ReviewProposal, ReviewFrom, ApplyNotesProposal, NotesReviewView, ErrNotesChanged), so the daemon's critic and `magnum notes --review` share one implementation; the CLI's output is unchanged and its apply records the operator. No new dependencies."
```

---

### Task 6: The notes package: the decision file, the critic's run directory and the superseded reason

**Files:**
- Create: `internal/notes/critic.go`
- Modify: `internal/notes/curate.go:28-52` (scratch names), `:82-105` (`WriteSuperseded`), `internal/notes/notes.go:33` (`CurateDirName`), `:57-70` (`Repo` paths)
- Test: `internal/notes/critic_test.go` (new), `internal/notes/curate_test.go` (existing `WriteSuperseded` test extended)

**Interfaces:**
- Produces:
  - `const CriticDirName = ".critic"` and `func (r Repo) Critic() string` (`<root>/.critic/<owner>/<name>`), beside `Curate()`.
  - `type CriticRun struct{ Dir string }` with `Review()` (`review.json`), `Rejections()` (`rejections.json`), `Current()` (`current.md`), `CurrentHarness()` (`current/`), `Proposed()` (`proposed.md`), `ProposedHarness()` (`proposed/`), `Changes()` (`changes.json`), `Decision()` (`decision.json`).
  - `func PrepareCritic(dir string, live, proposed State, changes, review, rejections []byte) (CriticRun, error)`: makes `dir`, writes the copies with `writeTree` and the three JSON files atomically. `proposed` may be empty (`State{}`), then `proposed.md` is not written.
  - `type Decision struct { Decision string; Reason string; Losses []string; Checked []string }` with json tags `decision`, `reason`, `losses`, `checked`.
  - `const DecisionApply = "apply"`, `DecisionReject = "reject"`, `DecisionRecurate = "recurate"`.
  - `func ReadDecision(run CriticRun) (Decision, bool, error)`: `os.Root`-confined read, `DisallowUnknownFields`; the bool is false when the file is missing.
  - `type DecisionCheck struct { Stale, Conflict bool; Leaks func(text string) []string }`.
  - `func ValidateDecision(d Decision, c DecisionCheck) []string`: the problems: an unknown decision word; `apply` with `Conflict`; `recurate` without `Conflict`; an empty reason for `reject` or `recurate`; a reason over 300 runes or with a line break (`maxReasonRunes` reused); each string `c.Leaks(d.Reason)` returns (the engine passes the scrub of Task 8).
  - `WriteSuperseded(s Scratch, proposed State, changes []byte, reason string) error`: writes `superseded/reason.txt` when `reason != ""`.
  - `func (s Scratch) SupersededReason() string` (`superseded/reason.txt`).

- [ ] **Step 1: Write the failing tests**

`internal/notes/critic_test.go`:
- `TestPrepareCriticWritesCopiesAndInputs`: after `PrepareCritic`, the eight paths exist (`proposed.md` too when `proposed` has notes), `current.md` equals the live notes, `review.json` equals the given bytes.
- `TestReadDecisionRefusesUnknownFieldsAndSymlinks`: a file with an extra field fails; a symlink at `decision.json` fails; a missing file returns `ok == false`.
- `TestValidateDecisionRules`: a table: `{"apply", conflict} → "apply is not valid: the proposal is stale and its merge conflicts; reject or recurate"`, `{"recurate", clean} → "recurate is valid only for a stale proposal whose merge conflicts"`, `{"reject", ""} → "reject needs a reason"`, a 301-rune reason → "the reason is over 300 runes", a two-line reason → "the reason must be one line", a reason the `Leaks` func flags → that string, `{"maybe"} → "decision must be apply, reject or recurate"`, `{"apply", clean}` → no problems.
- Extend the `WriteSuperseded` test: with a reason, `superseded/reason.txt` holds it; without, it is absent and `SupersededReason()` is "".

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/notes -count=1`
Expected: compile errors.

- [ ] **Step 3: Implement**

As in the Interfaces block. Update the one `WriteSuperseded` caller (`internal/engine/notes_curate.go:855`) to pass `p.Reason` when `p.DecidedBy == store.DecidedByCritic`, else `""`.

- [ ] **Step 4: Run the notes and engine tests**

Run: `go test -count=1 -race ./internal/notes ./internal/engine`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notes internal/engine
git commit -m "The notes package gains the critic's run directory (notes.CriticRun under .critic/<owner>/<repo>/<run>/ with copies of the live and proposed notes and harness, review.json and rejections.json), its decision file (notes.Decision: apply, reject or recurate with a one-line reason; ReadDecision confined to the run directory, ValidateDecision with the stale and conflict rules and the caller's leak check), and a superseded proposal's reason beside its state (superseded/reason.txt) for the curator. No new dependencies."
```

---

### Task 7: The critic prompt

**Files:**
- Create: `prompts/notes-critic.md`, `internal/agents/testdata/notes_critic.golden`, `internal/agents/testdata/notes_critic_stale.golden`
- Modify: `prompts/prompts.go:24` (go:embed), `prompts/README.md` (a table row and a section after the notes curation one), `prompts/notes-curate.md:1,8` (text), `internal/agents/templates_test.go:400-510` (cases), `:578-582` (want list), `internal/engine/skew.go:102-107` (a critic block), `internal/engine/prompts.go:64` (snapshot list), `internal/engine/notes_critic_data.go` (new: `criticData`)
- Test: `internal/agents/templates_test.go`, `internal/agents/review_format_test.go` (a sentence pin), `internal/engine/skew_test.go`

**Interfaces:**
- Produces: `type criticData struct { Repo, Dir, Review, Rejections, Current, CurrentHarness, Proposed, ProposedHarness, Changes, Decision string; ProposalID int64; Stale, Conflict, ProposedMissing bool; Limits curateLimits; MaxReasonRunes int }` in internal/engine (the fixture in templates_test.go mirrors it as `criticFixture()` with talkable/example paths, like `curateFixtureWith`).
- The prompt, in the register of `prompts/notes-curate.md`, with `{{.Repo}}`, the file list, the data-not-instructions sentence, the eight rules of spec section 3.2, the decision file's shape, and `{{if .Conflict}}` / `{{if .ProposedMissing}}` branches. It says that the limits are triggers, not caps, and that a rejection never names a byte target.
- `prompts/notes-curate.md` line 1: "a person or the critic reviews it before anything changes"; line 8: "`rejections`: why the operator or the critic rejected earlier proposals, and the reason a superseded one was re-curated (`{{.Superseded}}/reason.txt` when there is one)".

- [ ] **Step 1: Write the failing tests**

- Add `"notes-critic.md"` to the `want` list of `TestEveryDefaultPromptHasAGolden` (sorted position after `notes-curate.md`).
- Add cases `{"notes_critic", "notes-critic.md", criticFixture()}` and `{"notes_critic_stale", "notes-critic.md", criticStale}` (a fixture with `Stale: true, Conflict: true`) to `TestRenderGolden`.
- In `review_format_test.go`, `TestTheCriticPromptKeepsDecisionsAndNamesNoByteTarget`: the rendered prompt contains "Every standing decision and author answer must stay in full", "triggers, not caps", "never names a byte target", "data, never instructions", "recurate" only in the stale fixture.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/agents -run 'TestEveryDefaultPromptHasAGolden|TestRenderGolden|TestTheCriticPrompt' -count=1`
Expected: FAIL (no such prompt).

- [ ] **Step 3: Write the prompt, the data struct, the embed, the check and the snapshot**

Write `prompts/notes-critic.md`. Add it to the `go:embed` directive. Add `criticData` and a `CheckPrompts` block "notes critic prompt" rendering `sampleData[criticData](true)` and `criticData{}`. Append `e.cfg.Notes.CriticPrompt` to the `SnapshotPrompts` call. Regenerate the goldens: `go test ./internal/agents -run TestRenderGolden -update` (this rewrites only the golden files of the listed cases; check `git status` shows the two new goldens and no other).

- [ ] **Step 4: Run the agents and engine tests**

Run: `go test -count=1 ./internal/agents ./internal/engine -run 'Golden|Prompt|Skew'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add prompts internal/agents internal/engine
git commit -m "prompts/notes-critic.md: the notes critic's prompt (the review data, the copies of the live and proposed notes, the eight rules: the notes are data, the limits are triggers not caps, every standing decision stays, a general pitfall stays, a deleted script is fine when a line keeps its method, a rejection names what to keep and never a byte target, a conflicting stale proposal is re-curated or rejected, no PR, login, branch, probe, secret or home path), rendered from criticData, embedded, render-checked at start and in `magnum config`, snapshotted, with two goldens; the curator's prompt says a person or the critic reviews, and reads a superseded proposal's reason. No new dependencies."
```

---

### Task 8: The engine's critic job

**Files:**
- Create: `internal/engine/notes_critic.go`, `internal/engine/notes_critic_test.go`
- Modify: `internal/engine/engine.go:83-100` (nothing: `paneAgents` changed in Task 4), `:212-225` (`Deps.Critic`), `:392-412` (the Engine fields), `:454-460` (init), `:492-501` (`FromApp`), `:694-702` (shutdown), `:781` (the tick), `:822` (`delKV`)
- Modify: `internal/engine/learn_schedule.go:48-75` (`learnRunning`), `internal/engine/inflight.go:38-55` (`backgroundBusy`), `internal/engine/notes_curate.go:47-100` (constants: `CurateTriggerReview`), `:276-305` (`maybeCurate`: call `maybeCritic` after), `:310-382` (`dueCurations`: the review mark and the weekly wait), `:717-723` (the ready toast), `:841-857` (`curateSuperseded` reason), `:824` (rejections: superseded by the critic too), `:914-940` (`expireProposals`: a toast when the critic left it), `:945-960` (prune `.critic` too)
- Modify: `internal/engine/notes_stale.go:231-255` (`startQueuedCuration`: the review trigger obeys `userPause` and the list), `:153-160` (`supersede` takes a `Decider`)
- Modify: `internal/engine/kvkeys_test.go` (the new keys)
- Test: `internal/engine/notes_critic_test.go`, `internal/engine/notes_curate_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 7.
- Produces:
  - Constants: `KVNotesCritic = "notes.critic"`, `func KVNotesRecurate(full string) string` (`"notes." + lower(full) + ".recurate"`), `func KVNotesOverWait(full string) string` (`"notes." + lower(full) + ".over_wait"`, the time until which the over_limit trigger waits after a critic apply), `KVNotesCriticDay = "notes.critic_day"` (`"<YYYY-MM-DD>:<count>"`), `CurateTriggerReview = "review"`, `criticRunFormat = curateRunFormat`.
  - `type CriticMark struct { Repo string; Proposal int64; Model string; Started time.Time }` and `func ReadCritic(ctx, st) *CriticMark`.
  - `type CritiqueJob struct { Run notes.CriticRun; Prompt string; Stale, Conflict bool }`, `type CritiqueResult struct { Decision notes.Decision; Problems []string; Pause *pipeline.Pause; Model string }`, `type Critic interface { Critique(ctx, CritiqueJob) (CritiqueResult, error); Close(ctx) error }`, `var ErrCriticDown`, `Deps.Critic func(ctx context.Context, run CriticRun) (Critic, error)`, `var criticPane = paneSpec{what: "notes critique", workspace: "learn notes critic", owner: learnRepoOwner, name: config.CriticRoleName, role: (*config.Config).CriticRole}`, `paneCritics(pd)`, `paneCritic.Critique` (the driver loop of Task 4 with one nudge `criticNudge(job, problems)`).
  - Engine fields `criticMu sync.Mutex`, `criticCancel context.CancelFunc`, `criticStarted time.Time`, `criticRepo string`, `criticProposal int64`, `criticWG`, `criticTried map[int64]time.Time` (by proposal id).
  - `func (e *Engine) maybeCritic(ctx context.Context)`: the qualification of spec 3.1, in this order: `e.cfg.Notes.Critic`, `e.d.Critic != nil`, `!e.d.DryRun && !e.once`, `e.learnBusy() == ""`, `e.holdReason(ctx) == ""`, `e.criticToolPause(ctx) == ""`, the retro not due-and-unrun today (`e.retroDueToday(ctx)`: `DailyTime` passed and `KVRetroDay != today`), the daily count below `CriticPerDay`; then the oldest pending curation proposal (`NotesProposalFilter{States: pending, Kind: curation, Oldest: true}`) whose repository has no `KVNotesCritic`, `CriticAttempts < store.CriticAttemptLimit`, `criticTried` older than `curateRetry`, declines in a row below `CriticDeclines` (`e.criticDeclines(ctx, repoID)`: walk the repository's proposals newest first counting `decided_by == critic && state in (rejected, superseded)` until a row that is applied or `decided_by == operator`), not `notesJudging(repoID)`, and `userPause` only when `p.Trigger != CurateTriggerRequest`. It then calls `startCritique(ctx, repo, p)`.
  - `startCritique`: `learnMu`, `learnRunning() == ""`, set the fields and `KVNotesCritic`, `criticWG.Go(e.safely(... runCritique ...))` with the panic path recording `criticTried[p.ID]`.
  - `runCritique(ctx, repo, p)`: `ReviewProposal` (the view), `rejections.json` from `NotesProposals{RepoID, States: [rejected, superseded], Limit: curateRejections}` (id, state, decided_by, reason, at), `notes.PrepareCritic(run dir, view.Live, view.Proposed, p.Changes, reviewJSON, rejectionsJSON)` (when `p.Scratch` is gone, `ProposedMissing` and no proposed copies), the prompt from `criticData`, `e.d.Critic(ctx, run)`, `Critique`, then `decideCritique`. A stop (pause, down, both turns timed out) records `criticTried[p.ID] = start` and `SetCriticDir`; an undecided answer (`Problems` after the nudge) or a conflict after a notes change calls `RecordCriticAttempt` and, at the limit, the toast "the critic did not decide proposal N of <repo>: review it with `magnum notes <repo> --review`" with the event `notes.critic_undecided`.
  - `decideCritique(ctx, repo, p, view, d notes.Decision, model string)`:
    - `apply`: `ApplyNotesProposal(... view.Live.Fingerprint(), ContentOf(view.Target()), view.Stale, Decider{critic, model} ...)`. On `ErrNotesChanged`: `CheckStale` again; when `!Stale || Merge.Clean`, apply the merge as reviewed (`ContentOf(check.Merge.State)` with `stale: true`); when it conflicts, count an attempt and return. On `store.ErrConflict`: the event `notes.critic_late` "proposal N of <repo> was decided by the operator meanwhile", nothing else. On success: event `notes.critic_decided` (`decision`, `model`, `reason`, `proposal`), the toast "proposal N of <repo> applied by the critic (<model>): <reason>" (`notify.Item{Key: "notes-critic:<id>"}`), and when `after.Over(limits)` is non-empty, `KVNotesOverWait(full) = now + curateWeeklyEvery` and the toast body adds "the notes are still over <limit> (<KB>): the next automatic curation is in 7 days".
    - `reject`: `DecideNotesProposal(... rejected, reason, Decider{critic, model} ...)`, then when `e.criticDeclines(ctx, repoID) < CriticDeclines` and `len(e.cfg.Notes.Curate) > 0`: `setKV(KVNotesRecurate(full), strconv(p.ID))`; else the toast "the critic declined N proposals of <repo> in a row and stepped back: review the next one with `magnum notes <repo> --review`". Event and toast as for apply with "rejected by the critic (<model>): <reason>. A new curation runs" (or "stepped back").
    - `recurate`: `e.supersede(ctx, p, full, reason, Decider{critic, model})`, then the same mark and cap handling.
  - `dueCurations`: before the trigger switch, when `KVNotesRecurate(full)` is set and the repository has no pending proposal, `out = append(out, curateDue{repo, CurateTriggerReview})` and `continue` (ignoring `since`); the over_limit case also requires `now` past `KVNotesOverWait(full)` when set. `startQueuedCuration` treats `CurateTriggerReview` like the daemon's own (it is not `CurateTriggerRequest`, so the existing gate applies). `runCurate` clears `KVNotesRecurate(full)` beside `clearMissesMark` when it stores a proposal, and `toastRequestedCuration` ignores the review trigger (it already keys on `request`).
  - The ready toast in `runCurate` is skipped when `e.cfg.Notes.Critic` (the event stays).
  - `curateUsage`'s rejections include superseded proposals decided by the critic (filter `States: [rejected, superseded]`, keep a superseded row only when `DecidedBy == critic`), each with `By string` in `curateRejection`.
  - `expireProposals`: when `p.CriticAttempts > 0` or the repository's last critic decision stepped back, a toast "proposal N of <repo> expired unreviewed after <ttl>".
  - `learnRunning` gains `"the notes critique of " + repo` after the curation block; `backgroundBusy` gains it; `engine.go:822` deletes `KVNotesCritic`; `kvkeys_test.go` pins `KVNotesCritic`, `KVNotesRecurate("Talkable/Example")` → `"notes.talkable/example.recurate"`, `KVNotesOverWait`, `KVNotesCriticDay`; `pruneCurations` globs `.critic/*/*/*` too; shutdown cancels and waits `criticWG`; `stopCritique` mirrors `stopCurate`.
  - The leak check passed to `ValidateDecision`: `func (e *Engine) reasonLeaks(ctx) func(string) []string` built as in learn_revert.go:387-398 (people from `PeopleLogins`, own from identities, names from repos), returning the phrases.

- [ ] **Step 1: Write the failing tests**

`internal/engine/notes_critic_test.go`, with `newHarness` and a fake `Critic` (`fakeCritic` returning a scripted `CritiqueResult` and recording the job), the notes repository in a temp dir, `cfg.Notes.Critic = true`:
- `TestTheCriticAppliesAValidProposalAndRecordsTheDecider`: a pending proposal, the fake answers `apply` with model `fable`; after a tick the proposal is `applied`, `DecidedBy == "critic"`, `CriticModel == "fable"`, the file holds the proposed text, an event `notes.critic_decided` exists, the toast batcher holds one item with "applied by the critic (fable)", and no "is ready" toast was sent for that proposal.
- `TestTheCriticAppliesACleanMergeAfterAJudgeRewrite`: the fake apply runs after the harness changed an unrelated line of the live notes; the result is applied with both changes; a conflicting change instead leaves the proposal pending with `CriticAttempts == 1`.
- `TestTheCriticRejectsAndAReviewCurationStartsUnderTheDaemonsGates`: the fake answers `reject` with a reason; the proposal is `rejected`, `KVNotesRecurate` is set, the next tick's `dueCurations` holds the repository with trigger `review`; under `magnum pause` (`userPause`) it does not start; with `cfg.Notes.Curate = nil` no mark is set; the curator's `usage.json` of that curation lists the reason with `by: critic`.
- `TestTheCriticRecuratesAConflictingStaleProposal`: stale with conflict, the fake answers `recurate`; the proposal is `superseded` with `DecidedBy == critic`, and the next curation's scratch has `superseded/reason.txt`.
- `TestTheCriticStepsBackAfterTwoDeclinesInARow`: two rejections, then a third pending proposal is not critiqued and `magnum notes`'s row state (through `notesOverview` in the CLI test, or `ReadCritic` nil and the row's `Proposal` present) stays; an apply resets the count.
- `TestTwoUndecidedAttemptsLeaveAProposalToTheOperatorAcrossARestart`: the fake answers an invalid decision twice (after the nudge) → `CriticAttempts == 1`; a new Engine over the same store (restart) runs again → `CriticAttempts == 2` and the toast; a third tick does nothing.
- `TestACriticRejectionKeepsTheMisses`: a proposal carrying a miss; the critic's rejection leaves it `new`.
- `TestTheOperatorsDecisionDuringACritiqueIsOnlyAnEvent`: the fake decides `apply` after the test decided the proposal as the operator; an event `notes.critic_late`, no toast, `CriticAttempts == 0`.
- `TestTheRetroDueTodayRunsBeforeAWaitingCritique`: with the retro due and unrun, the tick runs the retro first.
- `TestTheDailyCapHoldsCritiques`: `CriticPerDay = 1`, two pending proposals in two repositories: one critique today.
- `TestAnApplyThatLeavesTheNotesOverALimitDelaysTheNextCuration`: after the apply, `KVNotesOverWait` is set and `dueCurations` skips over_limit for the repository until then.
- `TestAStartClearsTheCriticMark`: `KVNotesCritic` set before `startup`, cleared after.
- `TestACriticStopIsRetriedAfterAnHourAndNeverPausesTheKind`: the fake returns a model-limit stop (`ErrCriticDown`); `criticTried` holds the proposal; the next tick within the hour does nothing; `kindPauseReason` empty.
- Extend `TestEveryKVKeyTheEngineWritesKeepsItsString` with the four keys.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/engine -run 'TestTheCritic|TestTwoUndecided|TestACritic|TestTheOperators|TestTheRetroDue|TestTheDailyCap|TestAnApplyThat|TestAStartClears' -count=1`
Expected: compile errors.

- [ ] **Step 3: Implement**

As in the Interfaces block, following `notes_curate.go` for the job's shape (`startCurate`, `runCurate`, the stop closure, `safely`).

- [ ] **Step 4: Run the engine package under race, then the CLI**

Run: `go test -count=1 -race ./internal/engine ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine
git commit -m "The notes critic: with [notes] critic on, the daemon reviews each pending curation proposal minutes after the curator stores it (the oldest first, one learn job at a time, after the retro due today, at most critic_per_day a day, never during a judge stage, never a restore, a request proposal under magnum pause too) by a pane agent on the critic's role that reads the review data and copies of both notes in its run directory and answers decision.json; apply goes through ApplyNotesProposal (a clean merge after a judge rewrite is applied as reviewed, a conflict counts as an attempt), reject records the reason and marks the repository for a review-triggered curation at once under the daemon's gates, recurate supersedes with the reason the next curator reads; rejections and re-curations in a row reach critic_declines and the critic steps back; two undecided attempts leave a proposal to the operator across restarts; a critic rejection never dismisses a miss; the operator's decision meanwhile is only an event; an apply that leaves the notes over a limit delays the next automatic curation a week; the ready toast is skipped with the critic on; a stop retries after an hour and never pauses the kind; KVNotesCritic joins the learn marks a start clears, learnRunning and backgroundBusy. No new dependencies."
```

---

### Task 9: The CLI, the screens and the doctor

**Files:**
- Modify: `internal/cli/notes.go:35-48` (usage, flags: `--decisions`), `:97-165` (dispatch), `:323-355` (`notesSummary`: the last decision)
- Modify: `internal/cli/notes_review.go:228-316` (the critic-in-flight line in the prompt, the decider in the refusal, `Decider` for expiries), `:319-345` (`notesMissesMoved`: "rejected by you")
- Modify: `internal/cli/notes_list.go:32-40` (states), `:123-203` (`notesOverview`: `ReadCritic`, stepped back, expires in), `:309-335` (`notesStateText`), `:342-382` (`notesSummaryLine`: "critic on <repo>")
- Modify: `internal/cli/notes_decisions.go` (new: `--decisions`), `internal/cli/misses.go:61`, `internal/cli/logs.go:50` (`"notes"`), `internal/cli/daemon_restart.go:51-52, 80-81, 89, 121-123`, `internal/cli/daemon_rounds.go:106-115, 203-251` (`KVNotesCritic`), `internal/cli/doctor_agents.go:191-219` (a `doctorNotesCritic` check), `internal/cli/doctor.go:166`
- Modify: `internal/cli/screen_facts.go:56`, `internal/tui/facts.go:52-54, 153-154`, `internal/tui/dashboard.go:34-36`
- Test: `internal/cli/notes_list_test.go`, `notes_review_test.go`, `notes_decisions_test.go` (new), `logs_test.go`, `daemon_rounds_test.go`, `doctor_agents_test.go`, `internal/tui/facts_test.go`

**Interfaces:**
- `notesRow` gains `Critic *engine.CriticMark json:"critic,omitempty"` and `notesRowProposal` gains `SteppedBack bool`, `ExpiresAt *time.Time`, `Attempts int`.
- States: `notesCritic = "critic"` (the critic runs on the repository). `notesStateText`: `critic running (<model>, <ago>)`; a proposal row says `proposal N to review (<why>, <ago>; the critic stepped back)` when `SteppedBack`, and `(...; expires in <d>)` when `Attempts >= store.CriticAttemptLimit`.
- `notesSummaryLine`: `"critic on " + repo` beside `curating`.
- `--decisions [N]`: `fs.Int64Var(&f.decisions, "decisions", 0, ...)` with `NoOptDefVal = "-1"`; `-1` lists the repository's proposals (id, kind, state, decided_by, critic_model, decided_at, reason) as a table or `--json`; `N` prints `decision.json` from `critic_dir` when the file exists, else "the run directory of proposal N is gone".
- `notesReviewProposal`: before the y/N prompt, when `engine.ReadCritic(ctx, d.Store)` names this proposal, print "the critic is on proposal N (<model>, <ago>). y applies anyway". On `store.ErrConflict` from a decision, re-read the proposal and say "proposal N was decided meanwhile by <decided_by>" (the critic with its model).
- `notesSummary` prints, when the repository's newest decided curation proposal has `DecidedBy == critic`: "proposal N <state> by the critic (<model>) at <time>: <reason>".
- `notesMissesMoved` and `misses.go` say "dismissed after 2 proposals you rejected".
- `logsSubjectPrefixes` gains `"notes"`; the help text names `notes:<owner>/<repo>`.
- `backgroundWork` adds `engine.KVNotesCritic` to the IN list and a case: `"notes critique of " + m.Repo`, `m.Started`. The restart flag texts say "review rounds, a notes curation or critique, the retro, a skill edit or gate".
- `doctorNotesCritic(ctx, d)`: skipped when `!d.Config.Notes.Critic` ("the notes critic is off ([notes] critic = false)"), failed when the critic kind is not declared or its CLI is missing (reuse `doctorAgentChecks`'s per-kind helpers), OK otherwise ("the notes critic reviews each curation proposal with <kind> <model>").
- Screens: `screenFacts`/`tui.DaemonFacts` count pending proposals as today; the title fact says "notes ×N" unchanged; the dashboard's `notes:` line comes from `notesSummaryLine` and so shows "critic on <repo>" with no screen change. `tui/facts.go:52-54` comment: "wait for the operator or the critic".

- [ ] **Step 1: Write the failing tests**

- `TestNotesListShowsTheCriticStates` (notes_list_test.go): rows with a `CriticMark`, a stepped-back proposal, and an expiring one render the three texts; `notesSummaryLine` says "critic on talkable/example".
- `TestNotesReviewNamesTheCriticInFlightAndTheDeciderOnConflict` (notes_review_test.go): with `KVNotesCritic` set for the proposal the prompt line appears; after a critic decision, `n` prints "decided meanwhile by the critic (fable)".
- `TestNotesDecisionsListsAndPrintsADecision` (notes_decisions_test.go): two decided proposals print in order with decider and reason; `--decisions N` prints the JSON file from a temp `critic_dir`; a missing dir says so.
- `TestLogsAcceptsANotesSubject` (logs_test.go): `logsMatchers("notes:talkable/example")` returns the below-match.
- `TestBackgroundWorkListsACritique` (daemon_rounds_test.go): a `KVNotesCritic` row yields "notes critique of talkable/example".
- `TestDoctorNotesCritic` (doctor_agents_test.go): off → skipped; on with kind `nope` → failed.
- `TestMissesTextSaysRejectedByYou`: the `notesMissesMoved` string.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/cli -run 'TestNotesList|TestNotesReview|TestNotesDecisions|TestLogsAccepts|TestBackgroundWork|TestDoctorNotesCritic|TestMissesText' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

As in the Interfaces block.

- [ ] **Step 4: Run the CLI and TUI packages**

Run: `go test -count=1 -race ./internal/cli ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli internal/tui
git commit -m "magnum notes shows the critic: the list's critic running, stepped back and expires-in states and the status line's critic on <repo>; magnum notes <repo> prints the last critic decision; --decisions lists a repository's proposals with their deciders and prints one decision.json; --review says when the critic is on the proposal and names the decider when one was decided meanwhile; magnum logs notes:<owner>/<repo>; stop, restart and --drain see a critique; doctor checks the critic's kind; the misses texts say rejected by you. No new dependencies."
```

---

### Task 10: The policy keys reach the code

**Files:**
- Modify: `internal/engine/notes_curate.go:60-71` (`ProposalTTL`, `curateOverLimitEvery` → `e.cfg.Notes.ProposalTTL`, `CurateInterval`), `:809, :868` (`UnusedRounds`), `:925-937` (the expiry text renders the TTL), `internal/engine/notes_stale.go:41` (`staleRecurateAfter` → `CurateInterval`), `internal/engine/learn_schedule.go:131`, `internal/cli/notes_review.go:234-250` (the CLI reads `d.Cfg.Notes.ProposalTTL` and renders it), `internal/cli/notes.go:309, :344`
- Modify: `internal/engine/learn_edit.go:56-70` (`learnBase` → `e.cfg.Learn.Base`, `editInputCap` → `EditorMaxInputs`), `learn_land.go:199-213`, `learn_revert.go:74-162`, `learn_gate.go:78-82, :220, :243, :451` (`GateAttempts`, `GateMaxAge`)
- Modify: `internal/engine/util.go:313`, `engine.go:457` (`MaxCodexStarts`), `round.go:26-42` (`backoff` becomes `(e *Engine) backoff(attempts int)` using `RoundRetryMax`; `maxAttempts` → `MaxRoundAttempts` at `round_settle.go:72-74, :206`, `wait.go:348`), `approval.go:203-211, :289` (`ApprovalCheckWait`)
- Modify: `internal/pipeline/related.go:52-59, :130, :140-142, :162` (`Related.Idle`, `MaxPRs`, `MaxPaths`)
- Test: the existing tests of each path, with one new assertion per key (a config override changes the behaviour): `internal/engine/notes_curate_test.go`, `learn_edit_test.go`, `learn_gate_test.go`, `round_test.go`, `approval_test.go`, `internal/pipeline/related_test.go`

**Interfaces:**
- Consumes: the fields of Task 2.
- Produces: the constants stay as package defaults (`DefaultProposalTTL`, ...) referenced only by `config` defaults where needed; every read goes through `e.cfg` or `rd.r.Config`. `engine.ProposalTTL` becomes `func ProposalTTL(cfg *config.Config) time.Duration` for the CLI and `learn_schedule.go`.

- [ ] **Step 1: Write the failing assertions**

One per key, in the test file of the path, in the style of its neighbours: for example `TestAProposalExpiresAfterTheConfiguredTTL` (`cfg.Notes.ProposalTTL = 1h`, a 2-hour-old proposal expires and the event says "within 1h0m0s"), `TestTheOverLimitCurationWaitsTheConfiguredInterval`, `TestHarnessUnusedRoundsIsConfigurable`, `TestTheEditorReadsTheConfiguredNumberOfInputs`, `TestTheGateHoldsAfterTheConfiguredAttempts`, `TestAGatingProposalIsHeldAfterTheConfiguredAge`, `TestTheEditorBranchesFromTheConfiguredBase` (`cfg.Learn.Base = "main"` and the fake git sees `refs/heads/main`), `TestCodexStartsAreCappedByTheConfig`, `TestRoundRetriesStopAtTheConfiguredAttempts`, `TestRoundBackoffCapsAtTheConfiguredMax`, `TestAnApprovalStandsForTheConfiguredWait`, `TestRelatedCapsAreConfigurable` (idle, max PRs, max paths).

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/engine ./internal/pipeline ./internal/cli -run 'Configured|Configurable|CappedByTheConfig' -count=1`
Expected: FAIL (the constants ignore the config).

- [ ] **Step 3: Implement**

Replace each constant read as listed in Files. Keep the comment of each constant on its config field.

- [ ] **Step 4: Run the three packages under race**

Run: `go test -count=1 -race ./internal/engine ./internal/pipeline ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine internal/pipeline internal/cli
git commit -m "The policy values the configuration now carries are read from it: [notes] unused_rounds, proposal_ttl (the expiry texts render it), curate_interval (the over_limit and misses triggers and the stale supersede); [learn] base (the editor's branch and the landing's), editor_max_inputs, gate_attempts, gate_max_age; [daemon] max_codex_starts, max_round_attempts, round_retry_max, approval_check_wait; [pipeline] related_idle, related_max_prs, related_max_paths. The old constants are the defaults. No new dependencies."
```

---

### Task 11: The simplification cap as `max_posted`

**Files:**
- Modify: `internal/agents/templates.go:146-182` (`Report.MaxPosted int`), `internal/pipeline/pipeline.go:1053, :1056` (fill from `role.MaxPosted`), `prompts/judge-initial.md:6`, `prompts/judge-rereview.md:41`, `prompts/judge-recovery.md:33`, `internal/agents/templates_test.go:37-41` (`judgeFixture` reports get `MaxPosted: 3`), `internal/agents/review_format_test.go:572-605`, the `judge_*` goldens
- Modify: `README.md:196-197, :1259`, `prompts/README.md:92` (and the `.Reports` entries table), `config.defaults.toml:441` (done in Task 2)

**Interfaces:**
- The sentence becomes `Post at most {{.MaxPosted}}, the most substantial, ordered by what they remove, most first.` with the number rendered as a word by a template function if the prompts already register one (`textx.Count`?), else as a digit. Check `agents.RenderPrompt`'s `FuncMap` in templates.go:31; use what exists.
- `judgeReports` sets `MaxPosted: role.MaxPosted` in both `agents.Report` literals. `Report.completed()` leaves 0 alone; the prompt guards with `{{if gt .MaxPosted 0}}` around the sentence so a role without the key posts without a cap sentence, and `validateRole` requires `max_posted > 0` for a role aliased `simplify` (Task 2's check extended here: "role %s: max_posted must be set (the judge posts at most this many of its proposals)").

- [ ] **Step 1: Write the failing test**

In `review_format_test.go`, change the pinned string to `"Post at most 3, the most substantial"` (or the word form chosen) and add `TestTheSimplificationCapComesFromTheRole`: a fixture whose claude-simplify report has `MaxPosted: 5` renders "Post at most 5". In `internal/pipeline`, `TestJudgeReportsCarryTheRolesMaxPosted` through the existing round fixtures: the judge data's report for the simplify role has `MaxPosted == 3`.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/agents ./internal/pipeline -run 'Simplification|MaxPosted' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement and regenerate the goldens**

Edit the three prompts, the struct, the pipeline and the fixture. Run `go test ./internal/agents -run 'TestRenderGolden|TestJudgePromptsNameTheFormerLogins|TestRereviewPromptNamesTheThreadsFile' -update`, then check `git status` lists only `judge_*` goldens.

- [ ] **Step 4: Run the agents, pipeline and engine tests**

Run: `go test -count=1 -race ./internal/agents ./internal/pipeline ./internal/engine`
Expected: PASS (the engine's `goldenTests` list already names the three tests).

- [ ] **Step 5: Commit**

```bash
git add internal/agents internal/pipeline prompts README.md
git commit -m "The judge posts at most max_posted simplification proposals: the three posting judge prompts render the simplify role's [[role]] max_posted (3 by default) from each report's MaxPosted instead of the literal three, a role aliased simplify must set it, the test pins the rendered number, the goldens are regenerated, and the README and prompts/README name the key. No new dependencies."
```

---

### Task 12: Documentation, the design log and the final gate

**Files:**
- Modify: `README.md:157, :196-197, :342, :1067, :1259, :1342-1343, :1351, :1809-1812, :1871-1989` (the notes section: a "Critic" paragraph after "Review", the list states, `--decisions`, `magnum logs notes:`, the misses sentence "rejected by you", "within proposal_ttl", "unused_rounds judge rounds", the toml example with `critic = true`)
- Modify: `config.defaults.toml` (any comment Task 2 left: check `grep -n 'at most 3\|7 days\|30 days\|within 1h' config.defaults.toml`)
- Modify: `prompts/README.md` (the notes critic section, the `rejections` wording, the `.Reports` `MaxPosted` entry, the `(20)` mention)
- Modify: `docs/DECISIONS.md` (three entries at the end of `## Operations`), `AGENTS.md:98` (the internal/notes row mentions the critic's run directory and decision)
- Regenerate: `docs/API.md` (`make api-doc`)
- Test: `make test`, `make lint`, `make api-doc-check`

- [ ] **Step 1: Write the design-log entries**

Three bullets, dated 2026-10-10, in the house style (what, why, what was rejected):
1. **A critic reviews curation proposals** (amends "A curator proposes curated notes"): the agent, its inputs and answer, the decisions, the cap, the attempts, the misses rule, the review trigger, the weekly wait after an over-limit apply, who is recorded, what the operator sees. Rejected: applying without any review (the three earlier rejections superseded: a review still precedes every apply, by a stronger model with the operator's checklist), the critic editing the proposal, re-curating through the request trigger, a byte cap, pausing the kind, the name "reviewer".
2. **Pane jobs switch models on a limit**: the live record, the seeding, the in-job restart, no kind pause, the blocked-dialog wait, the consent wording, the cooldown doubling, per-role fallback lists. Rejected: Claude Code's `--fallback-model`.
3. **Policy values are configuration**: the sixteen keys with their old homes, what stays a constant and why.

- [ ] **Step 2: Update the README, the prompts README, the config comments and AGENTS.md**

As listed in Files. Keep every sentence true to the code of Tasks 1 to 11.

- [ ] **Step 3: Regenerate the API doc and run the gate**

Run: `make api-doc && make test && make lint && make api-doc-check`
Expected: all green. `make test` includes `check-private`.

- [ ] **Step 4: Commit**

```bash
git add README.md config.defaults.toml prompts/README.md docs AGENTS.md
git commit -m "Docs for the notes critic, the pane-job model fallback and the configurable policy values: the README's notes section (the critic, the list states, --decisions, magnum logs notes:, the misses and expiry wording), the daemon row, the learn-job lists and the configuration keys; prompts/README; the design log's three entries; AGENTS.md's notes row; docs/API.md regenerated."
```

---

### After the plan lands (outside the repository)

The operator turns the critic on in `~/.config/magnum/config.toml`:

```toml
[notes]
critic = true
```

The defaults already set fable at xhigh with opus as the fallback. Then `make build && bin/magnum daemon-restart --drain` (migration 0025).
