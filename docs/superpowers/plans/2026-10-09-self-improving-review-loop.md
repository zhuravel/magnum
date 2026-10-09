# Self-improving review loop implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The daemon learns from general misses and from findings authors declined, drafts a skill or prompt change, scores it with the eval, lands a passing change as a local commit and measures each landed version.

**Architecture:** Five stages on the existing retro, notes-curation and eval machinery. The retro gains a declines table. The eval gains absent defects, a learned corpus, a candidate directory and a judge-only replay, with the per-case runner moved into a package the daemon can call. An editor pane agent works in a git worktree, a gate replays the learned cases, and a landing step fast-forwards master and restarts the daemon when idle. `magnum learn` is the window.

**Tech Stack:** Go 1.27, modernc SQLite migrations, cobra, the engine's pane-agent runner (`internal/engine/retro_pane.go`), `internal/eval`, `internal/gitx`, hermetic fakes (`storetest`, `execx.Fake`).

**Spec:** `docs/superpowers/specs/2026-10-09-self-improving-review-loop-design.md`

## Global constraints

- The repository is public: no private organization, repository or person in tracked files, tests, docs or commit messages. Logins in tests: `zhuravel`, `alice`, `rev-ann`, `bob-rev`. Organizations: `talkable`, `example`.
- Never run `bin/magnum`, `make build`, git, mysql, herdr or launchctl against real directories from a task. A new migration blocks the live CLI until the daemon restarts, so `make build` is forbidden in this plan.
- Tests are hermetic: `storetest.Open`, `storetest.Serial` before a `t.Setenv` or a package-variable swap, fakes for git, gh, herdr and agents. Round scripts report with `t.Errorf` and return `failRound`'s result.
- Gate per task: `go test -count=1 -race ./<touched packages>`, then `mise exec -- make test` before a commit. `make api-doc` after an exported API change. `make lint` on the touched lines (CI fails on new issues only).
- One `docs/DECISIONS.md` entry per behaviour, appended at the end of the matching section, 104-column wrap, two-space continuation. README and `config.defaults.toml` describe every new key and command. `prompts/README.md` documents every template field.
- Commit per task: one paragraph in git-log style, the new tests by name, ending "No new dependencies." No push.
- Migrations: 0023 (declines) and 0024 (learn proposals, inputs, versions). No other migration.
- `skillMaxBytes` stays 30,001 in this plan; the editor shortens to add.
- Every new engine KV key goes into `internal/engine/kvkeys_test.go`.
- Logs and errors never contain tokens, PEM blocks, pane environment values, PR text or reply text.

## Review focus

- A reply thread whose root finding has no `findings` row (a review posted before migration 0005, or a dry run): the decline is recorded with `finding_row_id` NULL and nothing panics. Task 1.3 pins it.
- An author reply that only quotes the finding and adds "fixed in abc1234": classified `fixed`, no lesson required, no corpus case. Task 1.2 pins the parse rules.
- A learned corpus case whose PR is in no watch any more or became Codex-flagged after the case was written: the gate skips it with a reason and does not fail the proposal. Task 4.1 pins it.
- The operator has uncommitted edits in `SKILL.md` when a candidate passes: the landing holds the proposal naming the file, never merges over the edit. Task 4.3 pins it.
- The daemon restarts between the editor's commit and the gate: the proposal in `gating` is picked up again at startup (the KV mark is cleared, the row's state is not), and a worktree left behind is reused, not duplicated. Task 4.2 pins it.

---

## Stage 1: declines

### Task 1.1: the declines table and store

**Files:**
- Create: `internal/store/migrations/0023_declines.sql`
- Create: `internal/store/declines.go`
- Test: `internal/store/declines_test.go`

**Interfaces:**
- Produces: `type Decline struct { ID, PRID int64; SourceURL string; CommentID, ReviewID int64; RunID string; FindingRowID *int64; Path string; Line int; Title, Priority, ReviewedSHA, SkillSHA, Outcome, Verdict, Lesson, Scope string; Match []string; State string; CreatedAt, UpdatedAt time.Time; Repo string; Number int }`
- Constants: `DeclineFixed="fixed"`, `DeclineDeclined="declined"`, `DeclineDeferred="deferred"`, `DeclineArgued="argued"`, `DeclineQuestion="question"`, `DeclineUnrelated="unrelated"`; `VerdictWrong="wrong"`, `VerdictOverRanked="over_ranked"`, `VerdictMissingContext="missing_context"`, `VerdictRight="right"`; states reuse `MissNew`, `MissUsed`, `MissDismissed`.
- `func (s *Store) UpsertDecline(ctx context.Context, d Decline) (Decline, error)`: insert or update on `source_url`, keeping `id`, `state` and `created_at`.
- `type DeclineFilter struct { Outcomes, Verdicts, Scopes, States []string; PRID, RepoID int64; SkillSHA string }`
- `func (s *Store) Declines(ctx context.Context, f DeclineFilter) ([]Decline, error)` newest first, joined with repo and number.
- `func (s *Store) CountDeclines(ctx context.Context, f DeclineFilter) (int, error)`
- `func (s *Store) SetDeclineState(ctx context.Context, ids []int64, from []string, to string) (int, error)` (compare-and-set per row; Task 3.1 uses it inside the proposal decision).

- [ ] **Step 1: Write the failing tests** in `declines_test.go`: `TestUpsertDeclineKeepsIDStateAndCreatedAt` (upsert twice, the second with a new outcome: same id, state unchanged, outcome updated), `TestDeclinesFilterByVerdictScopeAndState`, `TestSetDeclineStateIsCompareAndSet` (from `[new]` moves one row and leaves a `used` row alone, returns the count).
- [ ] **Step 2: Run** `go test ./internal/store -run Decline -count=1` and see them fail to compile.
- [ ] **Step 3: Write migration 0023** with the columns of the spec (section 1), `source_url TEXT NOT NULL UNIQUE`, CHECKs on outcome, verdict, scope and state, indexes `declines_pr(pr_id)`, `declines_state(state, scope, verdict)`, `declines_skill(skill_sha)`. Implement the store functions following `internal/store/learn.go`'s `UpsertMiss` and `Misses` (the `keepClass`-style CASE is not needed: every write carries the full classification).
- [ ] **Step 4: Run the store tests** (`go test ./internal/store -count=1 -race`) and the migration test suite; expect PASS.
- [ ] **Step 5: Commit.**

### Task 1.2: own threads as retro candidates and the classifier's answer

**Files:**
- Modify: `internal/learn/candidates.go` (`Build`, `Input`), `internal/learn/output.go` (`Output`, `ParseOutput`, `Candidates`)
- Create: `internal/learn/threads.go`
- Modify: `prompts/retro.md`, `prompts/README.md`
- Test: `internal/learn/threads_test.go`, `internal/learn/output_test.go`, `internal/agents/testdata/retro_*.golden` (regenerate with `go test ./internal/agents -update`)

**Interfaces:**
- Consumes: `learn.Input` (its `Own` logins, `Threads`, `Findings`, `Reviewed`).
- Produces: `type OwnThread struct { ID string ("o"+comment id); URL string; CommentID, ReviewID int64; Path string; Line int; Title, Priority, ReviewedSHA string; FindingRef string; Replies []ThreadReply }`, `type ThreadReply struct { Author string; Body string }`; `Candidates.Threads []OwnThread` (written to candidates.json; reply bodies are data, pruned with the run); `type ThreadItem struct { ID, Outcome, Verdict, Lesson, Scope string; Match []string }`; `Output.Threads []ThreadItem`.
- `func OwnThreads(in Input) []OwnThread`: threads whose root is by one of `in.Own` with at least one reply by someone else; `Title` = the root's first line, `Priority` = the first `P[0-3]` in it, `ReviewedSHA` = the root's commit when magnum reviewed it, else the newest reviewed commit before the root (`reviewedBefore`); `FindingRef` = the posted finding on the same path within 3 lines on the same review (through `near` with verdict posted), empty when none.
- `ParseOutput` returns `(map[string]Item, map[string]ThreadItem, error)`: exactly one thread item per own thread; `outcome` in the six values; `verdict` required for `declined` and `argued`, absent otherwise; `lesson`, `scope` and `match` required for `wrong`, `over_ranked` and `missing_context`, with the same rules as a miss (lesson non-empty, scope repo|general, 1 to 3 regexps under 120 bytes); error texts name ids and fields, never text.

- [ ] **Step 1: Write the failing tests**: `TestOwnThreadsKeepOnlyMagnumsRootsWithOthersReplies`, `TestOwnThreadsBindTheReviewedCommitAndTheFinding`, `TestParseOutputRequiresOneItemPerOwnThread`, `TestParseOutputRequiresAVerdictOnlyForDeclinedAndArgued`, `TestParseOutputRequiresALessonForWrongOverRankedAndMissingContext`, `TestAFixedReplyNeedsNoLesson`.
- [ ] **Step 2: Run** `go test ./internal/learn -count=1` and see them fail.
- [ ] **Step 3: Implement** `threads.go` and the `ParseOutput` extension. In `Build`, the own-root threads no longer count as `other` skips; `Result.Threads []OwnThread` carries them.
- [ ] **Step 4: Extend `prompts/retro.md`** with a "Magnum's own findings" section: the `threads` list in the candidates file, the outcomes, the verdicts, the lesson rules (same wording as misses), and the output shape `{"items":[...],"threads":[{"id":"o123","outcome":"declined","verdict":"wrong","lesson":"When ...","scope":"general","match":["..."]}]}`. Add `{{.Threads}}` (count) to the template; document it in `prompts/README.md`.
- [ ] **Step 5: Regenerate the retro goldens**, run `go test ./internal/learn ./internal/agents -count=1 -race`; expect PASS.
- [ ] **Step 6: Commit.**

### Task 1.3: the retro stores declines and routes them

**Files:**
- Modify: `internal/engine/retro.go` (`retroData.Threads`, the classify step, `storeMiss` neighbour `storeDecline`, `retro.ok` data), `internal/engine/retro_pane.go` (`answered`, `retroNudge` mention threads)
- Create: `internal/engine/retro_declines.go`
- Modify: `internal/engine/notes_curate.go` (`markRepoMisses` counts declines routed to notes)
- Test: `internal/engine/retro_declines_test.go`

**Interfaces:**
- Consumes: Task 1.1 store, Task 1.2 output.
- Produces: `func skillSHAOf(promptText string) string` (the 12 hex after `/skill/`, "" when absent); `func (e *Engine) storeDeclines(ctx, pr store.PR, threads []learn.OwnThread, items map[string]learn.ThreadItem, runs []store.Run) (stored, declined int, err error)`.
- Routing: a `missing_context` thread item with scope `repo` also upserts a `misses` row (`SourceURL` = the thread URL, `SourceKind` thread, `Class` miss, `Scope` repo, `Severity` = the priority, `Title`, `Lesson`, `Path`, `Lines` `[line,line]`, `Match`, `Reviewer` = the first replier's login) so the notes curation consumes it unchanged.
- `finding_row_id` is the `findings` row whose `run_id` is the run of the thread's review and whose path matches within 3 lines; NULL when none.

- [ ] **Step 1: Write the failing tests** with the engine's fake classifier (`newHarness`, seeded threads): `TestTheRetroStoresADeclineForEachAnsweredFinding`, `TestAMissingContextDeclineBecomesARepoMissForTheCuration`, `TestADeclineWithoutAFindingsRowIsStoredWithoutOne`, `TestTheRetroOKEventCountsDeclines` (data has `declines` and `declined`), `TestSkillSHAOfReadsTheSkillCopyPath`.
- [ ] **Step 2: Run** `go test ./internal/engine -run 'Decline|SkillSHA' -count=1` and see them fail.
- [ ] **Step 3: Implement** the storing, the finding link, the skill hash from the judge run's `prompt_text`, `ScrubLesson` on the lesson with the PR author and repliers as people, the routing to misses and the event data.
- [ ] **Step 4: Run** `go test ./internal/engine -count=1 -race -run 'Retro|Decline'`; expect PASS.
- [ ] **Step 5: Commit.**

### Task 1.4: `magnum misses --declines`, stats outcomes, docs

**Files:**
- Modify: `internal/cli/misses.go`, `internal/cli/stats.go`, `internal/cli/status_gather.go` (status `retro:` line adds `N declined`)
- Modify: `README.md` ("Learning from other reviewers": a paragraph "What authors answered"), `docs/DECISIONS.md`
- Test: `internal/cli/misses_test.go`, `internal/cli/stats_test.go`

**Interfaces:**
- `--declines` lists `store.Declines` (filter default: outcomes declined and argued, state new; `--all` lifts it): PR, WHO (the finding's priority), WHERE, OUTCOME, VERDICT, SCOPE, STATE, TITLE, LESSON. `--json` prints `[]store.Decline`.
- `magnum stats` gains "findings answered" rows: per source and per priority, counts by outcome.

- [ ] **Step 1: Write the failing tests**: `TestMissesDeclinesListsAnsweredFindings`, `TestStatsCountsHowAuthorsAnsweredBySourceAndPriority`.
- [ ] **Step 2: Run them**, see them fail.
- [ ] **Step 3: Implement**, following `missesFlags` and the stats tables.
- [ ] **Step 4: Docs**: README paragraph, DECISIONS entry "The retro records how authors answered magnum's findings" (section "Learning"), `prompts/README.md` already done in 1.2.
- [ ] **Step 5: Run** `mise exec -- make test`; expect green. **Commit.**

## Stage 2: the eval

### Task 2.1: absent defects

**Files:**
- Modify: `internal/eval/corpus.go` (`Defect.Expect string`), `internal/eval/score.go` (`Score.AbsentHits []DefectResult`), `internal/eval/report.go` (`Totals.AbsentHits`, text and Markdown rows), `eval.toml.example`, `README.md` (eval section)
- Test: `internal/eval/score_test.go`, `internal/eval/corpus_test.go`, `internal/eval/report_test.go`

**Interfaces:**
- `Defect.Expect`: toml `expect`, "" or `found` or `absent`; validation rejects other values. `const ExpectFound = "found"`, `ExpectAbsent = "absent"`.
- `ScoreCase`: an absent defect matched by an inline finding adds a `DefectResult{Found: true}` to `Score.AbsentHits` and is left out of `Found`/`Total`; such a finding is not noise. `Totals.AbsentHits` sums them. `Score.Absent` is the count of absent defects in the case.

- [ ] **Step 1: Write the failing tests**: `TestAnAbsentDefectHitByAFindingCountsAsAFalsePositive`, `TestAnAbsentDefectIsNeverNoiseNorRecall`, `TestCorpusRejectsAnUnknownExpect`, `TestTheReportShowsAbsentHits`.
- [ ] **Step 2: Run** `go test ./internal/eval -count=1`, see them fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/eval -count=1 -race`; PASS. **Commit.**

### Task 2.2: corpus writing and the learned corpus

**Files:**
- Modify: `internal/eval/corpus.go` (`WriteCorpus`, `LoadCorpora`), `internal/paths/paths.go` (`Layout.LearnCorpus()`), `internal/cli/eval.go` (`evalCorpusPath` becomes the pair)
- Create: `internal/learn/corpus.go`
- Modify: `internal/engine/retro.go` (call `syncLearnedCorpus` at the end of a retro)
- Create: `internal/engine/learn_corpus.go`
- Test: `internal/eval/corpus_test.go`, `internal/learn/corpus_test.go`, `internal/engine/learn_corpus_test.go`

**Interfaces:**
- `func WriteCorpus(path string, c Corpus) error` (TOML through `fsx` atomic write; a round trip through `LoadCorpus` is exact).
- `func LoadCorpora(user, learned string) (*Corpus, error)`: `learned` missing is fine; a name in both is an error naming it.
- `paths.Layout.LearnCorpus() string` = `<data>/learn/corpus.toml`.
- `func CaseFromMiss(m store.Miss) (eval.Case, bool)`: ok when class miss, scope general, severity P0..P2, state new or used, `ReviewedSHA` 40 hex; name `miss-<id>`, pr `<repo>#<number>`, head `ReviewedSHA`, why = title, one defect `miss-<id>` with paths `[path]`, lines, match, severity.
- `func CaseFromDecline(d store.Decline) (eval.Case, bool)`: ok when verdict wrong or over_ranked, scope general, state new or used; name `decline-<id>`, one defect with `Expect: absent`, paths `[path]`, lines `[line,line]`, match.
- `func (e *Engine) syncLearnedCorpus(ctx context.Context) error`: rewrites the file from the registry; PRs not in a watch or Codex-flagged (`KVPRCodexFlag`) are left out; an unchanged file is not rewritten.

- [ ] **Step 1: Write the failing tests**: `TestWriteCorpusRoundTrips`, `TestLoadCorporaRefusesACaseNameInBothFiles`, `TestCaseFromMissTakesOnlyGeneralP0ToP2Misses`, `TestCaseFromDeclineMakesAnAbsentDefect`, `TestSyncLearnedCorpusLeavesOutUnwatchedAndFlaggedPRs`.
- [ ] **Step 2: Run them**, see them fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/eval ./internal/learn ./internal/engine ./internal/cli -count=1 -race`; PASS. **Commit.**

### Task 2.3: the per-case runner moves to `internal/evalrun` with a candidate directory

**Files:**
- Create: `internal/evalrun/run.go`, `internal/evalrun/candidate.go`
- Modify: `internal/cli/eval.go` (uses `evalrun`; `--candidate <dir>` and `--judge-only` flags), `internal/cli/eval_inputs.go` (`evalInputs` takes the config the candidate produced)
- Modify: `docs/API.md` (regenerate)
- Test: `internal/evalrun/candidate_test.go`, `internal/cli/eval_test.go`

**Interfaces:**
- `type Deps struct { Layout paths.Layout; Config *config.Config; Store *store.Store (the live registry, read-only use); Logger; Now; Runner execx.Runner; Herdr; GitHub func(identity) ...; Progress func(format string, args ...any) }` built from what `evalRunner` holds today.
- `type Options struct { Candidate string; JudgeOnly bool; NoNotes bool; Keep bool; Force bool; Label string; AgentTag string; RunDir string }`
- `func RunCase(ctx context.Context, d Deps, ec eval.Case, o Options) eval.CaseRun`: the body of today's `evalRunner.runCase`, unchanged in behaviour.
- `func WithCandidate(cfg *config.Config, dir string) (*config.Config, error)`: a copy with `Pipeline.PromptsDir = dir/prompts` and every judge role's `Skill = dir/skills/magnum-review/SKILL.md`; an error when either path is missing. `evalInputs` of that config hashes the candidate's texts.
- `eval.Run` gains `Candidate string` and `CaseRun` gains `JudgeOnly bool`, `ReportsRun string` (filled in Task 2.4).

- [ ] **Step 1: Write the failing tests**: `TestWithCandidatePointsPromptsAndEveryJudgeSkillAtTheDirectory`, `TestWithCandidateRefusesADirectoryWithoutASkill`, `TestEvalRunCandidateFlagChangesTheInputsHash` (two configs, two hashes).
- [ ] **Step 2: Run them**, see them fail.
- [ ] **Step 3: Move `runCase`** and its helpers (`evalClone`, `evalJudgeReport`, `evalCopyNotes`, the budget check, the flag check) into `evalrun`, keeping the CLI's output identical (the existing CLI eval tests stay green). Add the flags.
- [ ] **Step 4: Run** `go test ./internal/evalrun ./internal/cli -count=1 -race`, `make api-doc`; PASS. **Commit.**

### Task 2.4: judge-only replay

**Files:**
- Modify: `internal/engine/eval.go` (`EvalCase.JudgeOnly`, `EvalCase.Reports []agents.Report`, `seedEval` seeds verified reviewer runs, `RunEval` runs the judge-only shape), `internal/engine/round.go` (reuse the same-head judge-only round spec for eval)
- Modify: `internal/evalrun/run.go` (`FullReplayFor`, copying the reports into the scratch report dir), `internal/eval/report.go` (`FullReplayFor(runs []Run, caseName, head string) (Run, CaseRun, bool)`)
- Test: `internal/engine/eval_judge_only_test.go`, `internal/eval/report_test.go`, `internal/evalrun/run_test.go`

**Interfaces:**
- `eval.FullReplayFor`: the newest run whose case has the name, the head, a score, `JudgeOnly == false` and outcome `dry_run`.
- `RunEval` with `JudgeOnly`: the reviewer roles' runs are created `verified` with `ReportPath` set to the copied reports and the judge runs alone (own pass as configured); `RoundResult.Reports` carries the copied reports' statuses. A case without a stored full replay fails before any agent starts with `ErrNoFullReplay` naming the case and the head.

- [ ] **Step 1: Write the failing tests**: `TestAJudgeOnlyReplayRunsOnlyTheJudgeAgainstTheStoredReports` (engine, in-memory herdr as `eval_herdr_test.go` does), `TestAJudgeOnlyReplayWithoutAFullReplayIsRefusedBeforeAnyAgent`, `TestFullReplayForPicksTheNewestScoredFullRun`.
- [ ] **Step 2: Run them**, see them fail.
- [ ] **Step 3: Implement.** `run.json` records `judge_only` and `reports_run`; the text report marks a judge-only run in its header.
- [ ] **Step 4: Docs**: README eval section (`--judge-only`, `--candidate`, the learned corpus, absent defects), DECISIONS entries (one for absent defects, one for the learned corpus, one for judge-only replay and the candidate directory). `make api-doc`.
- [ ] **Step 5: Run** `mise exec -- make test`; green. **Commit.**

## Stage 3: the editor and the window

### Task 3.1: proposals, inputs and versions in the store

**Files:**
- Create: `internal/store/migrations/0024_learn_proposals.sql`, `internal/store/learn_proposals.go`
- Test: `internal/store/learn_proposals_test.go`

**Interfaces:**
- `type LearnProposal struct { ID int64; Trigger, BaseSHA, Branch, CandidateSHA string; Files []string; State string; Gate json.RawMessage; Reason, Model, PromptSHA256, Scratch, RunDir string; CreatedAt time.Time; DecidedAt *time.Time; LandedSHA string; LandedAt *time.Time }`
- Triggers: `LearnTriggerEvidence="evidence"`, `LearnTriggerRequest="request"`, `LearnTriggerRegression="regression"`. States: `LearnEditing="editing"`, `LearnInvalid`, `LearnStopped`, `LearnGating`, `LearnPassed`, `LearnHeld`, `LearnFailed`, `LearnLanded`, `LearnRejected`, `LearnExpired`, `LearnSuperseded`.
- `type LearnInput struct { Kind string (miss|decline); InputID int64; Outcome string (noted|skipped); Where, Reason string }`
- `func (s *Store) CreateLearnProposal(ctx, in LearnProposalInput) (LearnProposal, error)` (one tx: row plus inputs).
- `func (s *Store) DecideLearnProposal(ctx, id int64, from []string, to, reason string, at time.Time, gate []byte) (LearnProposal, error)`: compare-and-set; `landed` sets nothing else (Task 4.3 sets `landed_sha` with `MarkLearnLanded`); `landed` moves the noted inputs to `used`, `rejected` moves an input to `dismissed` once it is in 2 rejected proposals, other states move nothing.
- `func (s *Store) LearnProposals(ctx, states []string) ([]LearnProposal, error)`, `func (s *Store) LearnProposalInputs(ctx, id int64) ([]LearnInput, error)`, `func (s *Store) CountLearnProposals(ctx, state string) (int, error)`, `func (s *Store) LearnInputRejections(ctx, kind string, id int64) ([]string, error)`.
- `type LearnVersion struct { ID, ProposalID int64; LandedSHA, SkillSHA, PromptsSHA string; LandedAt time.Time; RestartedAt *time.Time }`; `CreateLearnVersion`, `LearnVersions(ctx) ([]LearnVersion, error)` newest first, `MarkLearnRestarted(ctx, id, at)`.

- [ ] **Step 1: Write the failing tests**: `TestCreateLearnProposalLinksItsInputs`, `TestDecideLearnProposalIsCompareAndSet`, `TestALandedProposalMarksItsNotedInputsUsed`, `TestAnInputRejectedTwiceIsDismissed`, `TestLearnVersionsNewestFirst`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Write migration 0024** (three tables, CHECKs, indexes `learn_proposals_state(state)`, `learn_proposal_inputs_input(kind, input_id)`) and the functions, following `notes.go`'s proposal code. **Step 4: Run** `go test ./internal/store -count=1 -race`; PASS. **Commit.**

### Task 3.2: the skill pins file and the edit validator

**Files:**
- Create: `internal/agents/testdata/skill_pins.txt` (one pinned sentence per line, generated), `internal/agents/skill_pins_test.go` (`TestSkillPinsFileIsCurrent`, `-update` rewrites it from the `skillSays` want lists)
- Modify: `internal/agents/review_format_test.go` (`skillSays` also records its wants into a package-level list the pins test reads)
- Create: `internal/learn/edit.go`, `internal/learn/edit_test.go`
- Modify: `internal/notes/curate.go` (export `CheckLeaks(text string, check Check) []string` over the existing leak rules)

**Interfaces:**
- `type EditInputs struct { Misses []EditMiss; Declines []EditDecline; Rejections []EditRejection; Versions []VersionMeasure (Task 5.1; empty until then); SkillBytes, SkillCap int; Files []string }` (inputs.json), `EditMiss{ID string ("m<id>"), Severity, Title, Lesson string; Match []string}`, `EditDecline{ID string ("d<id>"), Priority, Verdict, Title, Lesson string; Match []string}`, `EditRejection{Input, Reason string}`.
- `type Changes struct { Files []FileChange; Inputs []InputChange; Decision string }`, `FileChange{Name, Action string (edited|unchanged); Reason string}`, `InputChange{ID, Action string (noted|skipped); Where, Reason string}`.
- `type EditCheck struct { Inputs EditInputs; Changed []string (from git status); Allowed []string; Skill []byte; Pins []string; Diff string; Decision string }`
- `func ValidateEdit(c EditCheck, changes Changes) []string`: every input exactly once; noted needs `Where`; skipped needs a one-line reason of at most 300 runes; every changed file is allowed and listed `edited`; the skill is at most `SkillCap` bytes; every pin is present; `notes.CheckLeaks` on the diff and the decision; `ScrubLesson`'s rules on the decision; a `Decision` of 1 to 1200 runes without a PR number.
- `const EditAllowedGlobs = []string{"skills/magnum-review/SKILL.md", "prompts/judge-*.md", "prompts/claude-*.md"}` and `func AllowedEditFile(path string) bool`.

- [ ] **Step 1: Write the failing tests**: `TestSkillPinsFileIsCurrent`, `TestValidateEditRequiresEveryInputOnce`, `TestValidateEditRefusesAFileOutsideTheAllowedSet`, `TestValidateEditRefusesASkillOverTheCap`, `TestValidateEditRefusesALostPin`, `TestValidateEditRefusesAPRNumberInTheDecision`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: Run** `go test ./internal/agents ./internal/learn ./internal/notes -count=1 -race`; PASS. **Commit.**

### Task 3.3: worktree operations in gitx

**Files:**
- Create: `internal/gitx/worktree.go`
- Test: `internal/gitx/worktree_test.go` (real git in a temp repository, through `realEnv`)

**Interfaces:**
- `func (c *Client) WorktreeAdd(ctx, repo, dir, branch, base string) error` (`git worktree add -b <branch> <dir> <base>`), `WorktreeRemove(ctx, repo, dir string) error` (`--force`, then `worktree prune`), `CommitAll(ctx, dir, message string) (sha string, err error)` (`add -A` of the allowed paths, `commit -q -F -`, no signing: `-c commit.gpgsign=false`), `MergeFFOnly(ctx, repo, branch string) error` (`ErrNotFastForward` when refused), `RebaseOnto(ctx, dir, onto string) error` (`ErrRebaseConflict`, after `rebase --abort`), `StatusPaths(ctx, dir string, paths ...string) ([]string, error)` (the dirty ones among `paths`), `CurrentBranch(ctx, repo string) (string, error)`, `InProgress(ctx, repo string) (string, error)` ("rebase", "merge", "cherry-pick" or "").
- Every command runs with `core.hooksPath=/dev/null` like the rest of gitx.

- [ ] **Step 1: Write the failing tests**: `TestWorktreeAddAndCommitAllOnABranch`, `TestMergeFFOnlyRefusesADivergedMaster`, `TestRebaseOntoReportsAConflictAndAborts`, `TestStatusPathsNamesOnlyTheDirtyOnes`, `TestInProgressSeesARebase`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: Run** `go test ./internal/gitx -count=1 -race`; PASS. `make api-doc`. **Commit.**

### Task 3.4: the editor run in the engine

**Files:**
- Create: `internal/engine/learn_edit.go` (run, job, result, pane editor), `internal/engine/learn_edit_test.go`
- Create: `prompts/skill-edit.md`; Modify: `prompts/prompts.go` (embed), `prompts/prompts_test.go` (`TestNames`), `prompts/README.md`
- Modify: `internal/config/learn.go` (editor keys and `EditorRole()`), `config.defaults.toml`, `internal/config/learn_test.go`
- Modify: `internal/engine/engine.go` (`Deps.Editor`, `Deps.GoTest`, `Deps.Checkout string`, fields `editMu`, `editCancel`, `editWG`), `internal/engine/inflight.go` (`KVLearnEditing`), `internal/engine/kvkeys_test.go`, `internal/cli/daemon_rounds.go` (background work)
- Modify: `internal/engine/skew.go` (`CheckPrompts` renders `skill-edit.md`)

**Interfaces:**
- Config: `Learn.Editor bool`, `EditorKind`, `EditorModel`, `EditorEffort string`, `EditorArgs []string`, `EditorPrompt string` (default `skill-edit.md`), `EditorTimeout Duration` (45m), `GateCases int` (6), `LandPerDay int` (1), `Restart string` (`idle`|`manual`), `RegressionPoints int` (10), `RegressionMinFindings int` (20). `func (c *Config) EditorRole() Role` built with `sessionRole` (name `skill`, output `changes.json`).
- `type EditRun struct { ID, Dir, Worktree string }`, `type EditJob struct { Run EditRun; Prompt string; Check func() (learn.Changes, []string) }`, `type EditResult struct { Changes learn.Changes; Problems []string; Pause *pipeline.Pause }`, `type Editor interface { Edit(ctx, EditJob) (EditResult, error); Close(ctx) error }`, `Deps.Editor func(ctx, EditRun) (Editor, error)`, `Deps.GoTest func(ctx context.Context, dir string, args ...string) (string, error)` (the format gate's subprocess seam: real = `go test` with the checkout's mise environment), `Deps.Checkout string` (`Layout.Home`).
- `func (e *Engine) runEdit(ctx, trigger string) (LearnProposal, error)`: writes `inputs.json` (store queries: general misses P0..P2 new, general wrong/over_ranked declines new, rejections, versions), copies `skill_pins.txt` to `pins.json`, creates the worktree on `learn/<run>` from `master`'s sha, renders the prompt (`editData{Dir, Worktree, Inputs, Pins, Changes, Skill, Prompts, Cap}`), runs the editor with one nudge, validates (`learn.ValidateEdit`), renders the prompts (`CheckPrompts` on `WithCandidate`), runs the format gate (`GoTest(worktree, "./internal/agents", "-run", "TestRenderGolden", "-update")` then `GoTest(worktree, "./internal/agents", "./internal/pipeline", "./internal/config")` and `GoTest(checkout mise, "make", "check-private")` through the same seam), appends the DECISIONS entry at the end of the "Learning" section of the worktree's `docs/DECISIONS.md`, commits, stores the proposal `gating` (or `invalid` with the problems, worktree removed).
- The paneSpec: `editPane = paneSpec{what: "skill edit", workspace: "learn skill", owner: "magnum", name: "skill", role: (*config.Config).EditorRole}`; the agent's cwd is the worktree.
- Events `learn.edit_start`, `learn.edit_invalid`, `learn.edit_stopped`; toast on invalid or stopped only for trigger `request`.

- [ ] **Step 1: Write the failing tests** with a fake editor (`Deps.Editor` writing files into the worktree, a fake `GoTest` recording its calls, a `gitx` real temp repository as the checkout): `TestAnEditRunProposesACandidateOnItsOwnBranch`, `TestAnEditThatLosesAPinGetsOneNudgeThenIsInvalid`, `TestAnEditThatTouchesCodeIsInvalid`, `TestTheFormatGateRunsGoldensThenTheTestsThenCheckPrivate`, `TestTheEditRunAppendsTheDecisionsEntryAndCommits`, `TestAnEditRunIsBackgroundWorkForRestarts`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement**, including `prompts/skill-edit.md`: the inputs file, the pins, the allowed files, the rules of spec section 3, the `changes.json` shape with the `decision` text, "write `<file>.tmp` then move". **Step 4: Run** `go test ./internal/engine ./internal/config ./prompts ./internal/agents -count=1 -race`; PASS. **Commit.**

### Task 3.5: `magnum learn`, the review window and `--propose`

**Files:**
- Create: `internal/cli/learn.go`, `internal/cli/learn_review.go`, `internal/cli/learn_test.go`
- Modify: `internal/engine/requests.go` (`ReqLearnPropose="learn_propose"`, `ReqLearnLand="learn_land"` (handled in Task 4.3), `ReqLearnReject="learn_reject"`), `internal/engine/learn_edit.go` (`requestEdit`), `internal/cli/cli.go` (the command), `internal/cli/complete.go`
- Modify: `herdr-plugin.toml` (no popup; nothing), `README.md` (command table row), `docs/DECISIONS.md`

**Interfaces:**
- `magnum learn [--review [--reason <text>] | --propose | --revert <version>] [--json]` (revert in Task 5.3).
- The list (section 6 of the spec): versions newest first with their measures (empty measures until Task 5.1), waiting proposals, the last failed and invalid, the next scheduled run.
- `--review`: the oldest `held` proposal; prints inputs, `notes.Unified` diffs per file from `git show <base>:<file>` and the worktree file (through the store's `Files` and the run dir), the decision text, the gate table; TTY-only y/N; `y` sends `ReqLearnLand{ID}`; `n` sends `ReqLearnReject{ID, Reason}` (the daemon decides and removes the worktree and branch). `--json` prints `learnReviewData`.
- `--propose` sends `ReqLearnPropose` with `d.quick()`; the daemon refuses while an edit runs or a proposal is `gating`, `passed` or `held`, naming it.

- [ ] **Step 1: Write the failing tests**: `TestLearnListsVersionsAndWaitingProposals`, `TestLearnReviewShowsTheDiffAndTheGateAndAsksYN`, `TestLearnReviewNSendsARejectWithTheReason`, `TestLearnProposeIsRefusedWhileAProposalWaits`, `TestLearnUsageLineNamesItsFlags` (the usage-line walker passes).
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: Docs**: README row and section stub, DECISIONS entries ("The editor proposes skill and prompt changes on a branch", "magnum learn is the review window"). **Step 5: Run** `mise exec -- make test`; green. **Commit.**

## Stage 4: the gate and the landing

### Task 4.1: case selection and the verdict

**Files:**
- Create: `internal/learn/gate.go`, `internal/learn/gate_test.go`

**Interfaces:**
- `type GateCase struct { Case eval.Case; Target bool; Skip string }`; `func SelectCases(corpus *eval.Corpus, targets []string, watched func(pr string) bool, flagged func(pr string) bool, cap int) []GateCase`: targets first (a target not in the corpus or flagged gets `Skip`), then the newest others up to `cap`; a case in no watch is skipped with "not watched".
- `type GateRow struct { Case string; Target bool; Before, After *eval.Score; Skip string }`, `type Verdict struct { State string (passed|held|failed); Rule string; Rows []GateRow }`.
- `func Judge(rows []GateRow) Verdict`: the rules of spec section 4; a lost case needs `Retried bool` on the row set by the caller before it counts; a target `miss-` improves when `After.Found > Before.Found`; a target `decline-` improves when `len(After.AbsentHits) < len(Before.AbsentHits)`; held when every target row is skipped and nothing regressed.
- `func Mode(files []string) string` (`judge_only` when every file is `SKILL.md` or `prompts/judge-*.md`, else `full`).

- [ ] **Step 1: Write the failing tests**: `TestSelectCasesPutsTargetsFirstAndCapsTheRest`, `TestSelectCasesSkipsAFlaggedOrUnwatchedTarget`, `TestJudgePassesWhenATargetImprovesAndNothingIsLost`, `TestJudgeFailsALostCaseOnlyAfterARetry`, `TestJudgeFailsANewAbsentHit`, `TestJudgeHoldsWhenNoTargetCouldRun`, `TestModeIsJudgeOnlyForSkillAndJudgePromptEdits`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: PASS; Commit.**

### Task 4.2: the gate run in the engine

**Files:**
- Create: `internal/engine/learn_gate.go`, `internal/engine/learn_gate_test.go`
- Modify: `internal/engine/engine.go` (`Deps.EvalCase func(ctx, evalrun.Deps, eval.Case, evalrun.Options) eval.CaseRun` seam, default `evalrun.RunCase`; `Deps.EvalRuns func() ([]eval.Run, error)` default `eval.ListRuns(<state>/eval)`), `internal/engine/inflight.go` (`KVLearnGating`), `kvkeys_test.go`, `internal/cli/daemon_rounds.go`

**Interfaces:**
- `func (e *Engine) runGate(ctx, p store.LearnProposal) error`: selects cases (`learn.SelectCases` over `eval.LoadCorpora`), finds the baseline per case (`eval.Baselines` with the inputs hash of `master`'s config; a missing baseline is replayed first with `Candidate` = the checkout), replays each case with `Candidate` = the worktree in the mode of `learn.Mode(p.Files)` (judge-only needs `eval.FullReplayFor`; without one the row is skipped "no full replay"), retries a lost case once, stores `gate_json` (`Verdict` plus run ids) and moves the proposal to `passed`, `held` or `failed` (a `failed` proposal removes its worktree and branch). Budget: `e.budgetLevel()` at or above soft leaves the proposal `gating` with `learn.gate_waiting` event once per window. A `gating` proposal found at startup (state in the row, `KVLearnGating` cleared) is resumed at the first idle tick.
- Events `learn.gate_start`, `learn.gate_done` (data: state, rule, cases), `learn.gate_waiting`.

- [ ] **Step 1: Write the failing tests** with a fake `EvalCase` returning scripted scores: `TestTheGatePassesACandidateThatFindsItsTarget`, `TestTheGateReplaysALostCaseOnceBeforeFailing`, `TestTheGateHoldsWhenTheTargetHasNoFullReplayForJudgeOnly`, `TestTheGateWaitsAboveTheSoftCap`, `TestAGatingProposalIsResumedAfterARestart`, `TestAFailedProposalLosesItsWorktree`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: PASS; Commit.**

### Task 4.3: landing, restart, toasts and counters

**Files:**
- Create: `internal/engine/learn_land.go`, `internal/engine/learn_land_test.go`
- Modify: `internal/engine/build.go` (`restartForBuild` also honours `KVLearnRestart`), `internal/engine/requests.go` (`ReqLearnLand`, `ReqLearnReject` handlers), `internal/store/learn_proposals.go` (`MarkLearnLanded(ctx, id int64, sha string, at time.Time)`), `internal/engine/surface.go` (toasts), `internal/cli/screen_facts.go`, `internal/tui/facts.go` (`DaemonFacts.LearnProposals`), `internal/cli/status_gather.go`, `internal/cli/status_render.go` (the `learn:` line), `internal/cli/status.go`
- Test: `internal/tui/facts_test.go`, `internal/cli/status_test.go`

**Interfaces:**
- `func (e *Engine) landProposal(ctx, p store.LearnProposal) error`: the checks of spec section 5 in order (branch is `master`, nothing in progress, `base_sha == master` else `RebaseOnto` once, `StatusPaths(p.Files)` clean), `MergeFFOnly`, `BranchDelete`, `WorktreeRemove`, `MarkLearnLanded`, `CreateLearnVersion` (skill sha of the landed `SKILL.md`, prompts sha over `prompts/*.md` sorted), `KVLearnLandDay`, `KVLearnRestart` = version id when `Restart == idle`, toast `learn-landed:<id>`. A hold writes `learn.held` with the reason and the toast `learn-proposal:<id>`.
- Scheduling: `maybeLearnLand` on each tick lands the oldest `passed` proposal when `KVLearnLandDay != today`.
- Restart: `restartForBuild` exits when `KVLearnRestart` is set and nothing is in flight; the new daemon marks the version `restarted_at` and clears the key; `Restart == manual` sets the status line and toast `learn-restart` instead.
- `DaemonFacts.LearnProposals` ("N skill proposals to review", short "skill ×N") from `CountLearnProposals(held)`.

- [ ] **Step 1: Write the failing tests**: `TestLandingFastForwardsMasterAndRecordsAVersion`, `TestLandingHoldsOnADirtySkillFile`, `TestLandingRebasesOnceAndHoldsOnAConflict`, `TestOneLandingPerDay`, `TestALandedVersionAsksForAnIdleRestart`, `TestTheNewDaemonMarksTheVersionRestarted`, `TestHeldProposalsCountOnTheTitles`, `TestLearnLandRequestLandsAHeldProposal`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: Docs**: README "Self-improving loop" section (the full flow, the window, the restart, the keys), DECISIONS entries ("A passing candidate lands as a local commit and the daemon restarts when idle", "The gate replays the learned cases"). **Step 5: `mise exec -- make test`** green; `make api-doc`. **Commit.**

## Stage 5: measurement, scheduling, revert

### Task 5.1: version measures

**Files:**
- Create: `internal/store/learn_measure.go`, `internal/store/learn_measure_test.go`
- Modify: `internal/learn/edit.go` (`VersionMeasure` in `EditInputs`), `internal/engine/learn_edit.go` (fills the last 3), `internal/cli/learn.go` (the table)

**Interfaces:**
- `type VersionMeasure struct { VersionID int64; From time.Time; To *time.Time; JudgeRuns, Posted int; ByPriority map[string]int; Outcomes map[string]int; MissesGeneral, MissesRepo int }`
- `func (s *Store) VersionMeasure(ctx, v LearnVersion, next *LearnVersion) (VersionMeasure, error)`: window from `v.RestartedAt` (else `LandedAt`) to `next.RestartedAt`; judge runs posted in the window (`runs.created_at`), findings posted by severity, declines by outcome with `skill_sha == v.SkillSHA`, misses whose `reviewed_sha` is a `target_sha` of a judge run in the window.

- [ ] **Step 1: Write the failing tests**: `TestVersionMeasureCountsOnlyItsWindow`, `TestVersionMeasureJoinsDeclinesBySkillSHA`, `TestLearnShowsAVersionsNumbers`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: PASS; Commit.**

### Task 5.2: scheduling, serialization and doctor

**Files:**
- Modify: `internal/engine/learn_edit.go` (`maybeEdit`), `internal/engine/retro.go` (after a daily retro: `KVLearnEvidence` set when new general evidence was stored), `internal/engine/engine.go` (`learnMu` serializing retro, curation, edit and gate: `startRetro`, `startCurate`, `startEdit`, `startGate` take it), `internal/engine/notes_curate.go`, `internal/engine/kvkeys_test.go`
- Modify: `internal/cli/doctor_agents.go` (a check "learn editor: checkout and go toolchain" that says why the editor is off)
- Test: `internal/engine/learn_schedule_test.go`, `internal/cli/doctor_test.go`

**Interfaces:**
- `maybeEdit` (every tick, scan every 10 minutes): runs when `Learn.Editor`, `KVLearnEvidence` set, `KVLearnEditDay != today`, no proposal in `editing|gating|passed|held`, no round in its judge stage, `holdReason == ""`, `userPause == ""`, no tool pause of the editor's kind, `Deps.Checkout != ""` and `Deps.GoTest != nil`. It clears `KVLearnEvidence` when it starts.
- `learnMu`: a `sync.Mutex` plus `learnBusy() string` naming the job that holds it; `maybeCurate` and `maybeRetro` skip when another learn job runs (fixing the overlap).

- [ ] **Step 1: Write the failing tests**: `TestTheEditorRunsOnceADayAfterNewGeneralEvidence`, `TestTheEditorWaitsForAJudgeStageAndAUserPause`, `TestLearnJobsRunOneAtATime` (a running curation holds a due retro), `TestDoctorSaysWhyTheEditorIsOff`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: Docs**: DECISIONS ("Learn jobs run one at a time", "The editor runs after a retro with new general evidence"), `config.defaults.toml` comments. **Step 5: PASS; Commit.**

### Task 5.3: revert candidates and regression

**Files:**
- Modify: `internal/engine/learn_edit.go` (`runRevert`), `internal/engine/learn_land.go` (`maybeRegression`), `internal/cli/learn.go` (`--revert <version>`), `internal/engine/requests.go` (`ReqLearnRevert`)
- Test: `internal/engine/learn_revert_test.go`, `internal/cli/learn_test.go`

**Interfaces:**
- `func (e *Engine) runRevert(ctx, v store.LearnVersion, trigger string) (store.LearnProposal, error)`: a proposal whose branch restores the allowed files from the version before `v` (`git checkout <prev landed sha> -- <files>` in the worktree), with a decision text "Reverts version N: <reason>", through the format gate and the eval gate, always `held` (never auto-landed).
- `maybeRegression` (on the 10-minute scan): for the newest restarted version with at least `RegressionMinFindings` posted findings, when its `declined+argued` share exceeds the previous version's by `RegressionPoints` points, one revert proposal (`trigger regression`, `KVLearnRegression` = version id so it never repeats) and a toast.

- [ ] **Step 1: Write the failing tests**: `TestARegressionMakesOneHeldRevertProposal`, `TestARevertProposalIsNeverLandedAlone`, `TestLearnRevertProposesThePreviousVersion`.
- [ ] **Step 2: Run them**, see them fail. **Step 3: Implement.** **Step 4: Docs**: README, DECISIONS ("A regression proposes a revert, held for review"). `make api-doc`. **Step 5: `mise exec -- make test`** green. **Commit.**

---

## Self-review notes

Spec coverage: section 1 → Tasks 1.1–1.4; section 2 → 2.1–2.4; section 3 → 3.2–3.5; section 4 → 4.1–4.2; section 5 → 4.3; section 6 → 3.5 and 4.3; section 7 → 5.1 and 5.3; section 8 → 3.4 (config) and 5.2; section 9 → 1.1, 3.1, 3.4; section 10 → every task's tests; section 11 → the stage order. Types named across tasks: `store.Decline`, `learn.OwnThread`, `learn.ThreadItem`, `learn.EditInputs`, `learn.Changes`, `learn.ValidateEdit`, `store.LearnProposal`, `store.LearnVersion`, `store.VersionMeasure`, `evalrun.RunCase`, `evalrun.WithCandidate`, `eval.FullReplayFor`, `learn.SelectCases`, `learn.Judge`, `learn.Mode`, `gitx` worktree methods, `Deps.Editor`, `Deps.GoTest`, `Deps.EvalCase`, `Deps.EvalRuns`, `Deps.Checkout`.
