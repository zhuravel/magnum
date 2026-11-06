# Factory liaison, first slice: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `magnum factory talk <task>` starts one Claude liaison for a task, in the task's folder, on its own owner row. The liaison records the operator's answers and decisions through `answer` and `decide`, with the surface `liaison`.

**Architecture:** The stage machine does not change. A new request kind `talk` makes the factory daemon start the role `fx-liaison` through its runtime. The liaison has its own owner row in the factory's agents store. So chat with it never marks the task's row as "you at work". `answer` and `decide` gain `--surface` and `--shown`. A role can override its kind's `on_permission_prompt`, and the liaison waits, so magnum never answers the liaison's Yes/No questions for the operator.

**Tech Stack:** Go 1.27.3, the factory's registry (SQLite), magnum's agents store and agents manager, herdr, cobra.

**Spec:** docs/superpowers/specs/2026-11-06-factory-liaison-design.md (and docs/superpowers/specs/2026-11-03-factory-design.md for the factory).

**Base:** master at or after c8fcd4c6, where the branch `review-herdr-session` landed. It changes the copies' guards, the factory's herdr session (`cfg.FactoryHerdr()`) and reveal (`reveal.Options.Session`). Use those names. Do not use the APIs they replace.
- `herdr.GuardView` has three fields: `Own`, `Yours` and `Review`. The copies' guard reads magnum's review session too.
- `YourHerdr()` is `[terminal] session` (else `default`) on `[herdr] socket`. `ReviewHerdr()` is `[herdr] review_session` on its own socket, else yours. `FactoryHerdr()` is `[factory] herdr_session`, else `default`.
- A stopped herdr session counts the agents that herdr saved (its session.json beside the socket) as busy.

## Global Constraints

- The repository is public. Tests and docs use the placeholder keys PR-12301..PR-12399, the numbers #4501..#4599 and the logins zhuravel, alice and rev-ann only. `make check-private` runs in `make test`.
- Tests are hermetic: fakes for git, gh, herdr and MySQL. Never run bin/magnum, herdr, launchctl or mysql from a test. Tests open registries with `storetest.Open` and `registrytest`.
- The surface values are exactly `cli`, `screen`, `pane` and the new `liaison`. `answer` and `decide` accept only `cli` and `liaison`.
- The role name is exactly `fx-liaison` and its prompt file is exactly `prompts/factory-liaison.md`.
- The liaison's working directory is exactly `Layout.FactoryTaskDir(task id)`. It never works in the copy.
- The liaison's owner row: repository node id `factory-liaison` with owner `factory-liaison` and name `liaison` (ruling 10: `repos` is unique on node id and on owner and name, and task repositories are `factory/<name>`), PR node id `factory:liaison:<task id>`.
- Every behavior change gets a DECISIONS entry at the end of the section it belongs to (AGENTS.md): a factory change under `## The factory (2026-11-03)`, a change to the agents' handling of any role under `## Agents in panes`. Never inside `## Learned rules`. The README describes every new command, flag and key.
- Gate per task: `go test -count=1 -race` on the touched packages, `make test`, and `golangci-lint run --new-from-rev=<base>` on them. When exports change, also `make api-doc` and `make api-doc-check`.

## Review Focus

1. The liaison asks a Yes/No question (`Would you like to push PR-12345?`) while the factory's first prompt is still its run. magnum must leave it to the operator, never answer No. Test in Task 1.
2. The operator chats with the liaison while fx-writer works. The writer's prompts and the copy's checks are not held. Test in Task 3.
3. `talk` on a task whose copy was freed (after a push or a drop). The liaison still starts, because its folder is the task's. Test in Task 4.
4. `talk` twice. One liaison, one first prompt, the same pane. Test in Task 4.
5. A daemon restart while the liaison is live. The restart keeps its session, and no second first prompt goes out. Test in Task 4.

---

### Task 1: A role overrides its kind's `on_permission_prompt`

**Files:**
- Modify: `internal/config/roles.go` (the `Role` struct), `internal/config/validate.go` (role checks), `internal/config/factory.go` (the factory role checks near lines 587-627)
- Modify: `internal/agents/observe.go` (the blocked branch near lines 314-318, where `answerPermission` is called)
- Test: `internal/agents/permission_test.go`, `internal/config/factory_test.go`

**Interfaces:**
- Produces: `config.Role.OnPermissionPrompt string` with toml key `on_permission_prompt`. The values are "" (the kind's), `config.PermissionDeny` and `config.PermissionWait`.
- Produces: `func (r Role) PermissionPrompt(k Kind) string` in `internal/config/roles.go`. It returns the role's value. A role without one gets the kind's.

- [ ] **Step 1: Write the failing tests**

`TestARoleThatWaitsLeavesAPermissionShapedDialogToYou` (agents). Its setup:
- a Claude session of a role with `OnPermissionPrompt: config.PermissionWait`, and a run in flight
- herdr status blocked
- a screen that `detectPermissionPrompt` matches: `Would you like to push PR-12345?`, the options `1. Yes` and `2. No`, the cursor on 1

After one observe pass, no key went to the pane, and the observation is `ObsBlocked`. For a role without the override, the same screen still gets the No key, as today.

`TestAFactoryRoleTakesOnPermissionPrompt` (config): a `[[factory.role]]` with `on_permission_prompt = "wait"` loads. `"maybe"` is refused, with an error that names the key and the two values.

- [ ] **Step 2: Run them and see them fail**

Run: `go test -count=1 -run 'TestARoleThatWaitsLeavesAPermissionShapedDialogToYou|TestAFactoryRoleTakesOnPermissionPrompt' ./internal/agents/ ./internal/config/`
Expected: FAIL. The field does not exist yet.

- [ ] **Step 3: Implement**

Add the field and `PermissionPrompt`. The observe pass reads `role.PermissionPrompt(kind)`. With `config.PermissionDeny` it calls `answerPermission`. With `config.PermissionWait` it sends no key. The validation accepts "", "deny" and "wait" for review roles and factory roles. Comment the key in `config.defaults.toml` next to the factory roles.

- [ ] **Step 4: Run them and see them pass**

Run the command of Step 2. Expected: PASS. Then the gate.

- [ ] **Step 5: Commit**

`A role can override its kind's on_permission_prompt` with a DECISIONS entry and the README line beside the kind's key.

### Task 2: The role `fx-liaison` and its prompt

**Files:**
- Modify: `config.defaults.toml` (a `[[factory.role]]` after `fx-writer`, near line 914) and its Go mirror `DefaultFactory` in `internal/config/factory.go`
- Create: `prompts/factory-liaison.md`
- Modify, for a new prompt field: `internal/factory/stage_agents.go` (`PromptData` near line 93, `promptData` near line 1169, `checkData` near line 1308)
- Test: the file that tests `CheckPrompts` (`internal/factory/stage_agents_test.go`). A golden test in `prompts/` that renders every prompt covers the new one too.

**Interfaces:**
- Consumes: `config.Role.OnPermissionPrompt` (Task 1).
- Produces: the role `fx-liaison`: `kind = "claude"`, `effort = "high"`, `prompt = "factory-liaison.md"`, `timeout = "24h"`, `on_permission_prompt = "wait"`, `why = "talk with you about the task: ask its questions, explain, act on your word"`, `doing = "talking with you"`. No stage names it, so no stage launches it.
- Produces: `PromptData.TaskDir string`, the task's folder. If `PromptData` already has such a field, use that one.

- [ ] **Step 1: Write the failing test**

`TestTheLiaisonsPromptNamesItsFolderAndItsCommands`: render `factory-liaison.md` with `checkData` for the task PR-12345. The text holds the task's folder, `magnum factory answer`, `magnum factory decide`, `magnum factory retry`, `--surface liaison`, `--shown`, `notes.md` and `AskUserQuestion`. It holds no PR text.

- [ ] **Step 2: Run it and see it fail**

Run: `go test -count=1 -run TestTheLiaisonsPromptNamesItsFolderAndItsCommands ./internal/factory/`
Expected: FAIL. The prompt file does not exist.

- [ ] **Step 3: Write the role and the prompt**

The prompt tells the liaison:
- who it is and which task it serves
- that it reads the task's folder: the ticket, `findings-*.md`, `report.md`, `critic.md`, `questions.json`, `proof.json`, `ready-card.json`
- that it reads the copy but changes nothing there: no git, no database, no server
- that it asks with AskUserQuestion, its recommendation first, one decision at a time
- that it records every answer and decision with the factory's commands, `--surface liaison` and `--shown <the time it asked>`, and the ready card's revision for `decide`
- that `decide --diff-read` is only for a diff that the operator said they read
- that it acts on the operator's word in chat, push and drop included
- the hard lines: never merge, never run a migration, never touch production or a shared database
- that it writes `notes.md` in the folder: what confused the operator, what the factory asked needlessly, what it got wrong

- [ ] **Step 4: Run it and see it pass**

Run the command of Step 2, then `CheckPrompts` through `go test ./internal/factory/ ./prompts/`. Expected: PASS. Then the gate.

- [ ] **Step 5: Commit**

`The factory's fx-liaison role and its prompt` with the README lines in the factory's roles section.

### Task 3: The liaison's own owner row

**Files:**
- Modify: `internal/factory/runtime/owner.go` (beside `owner` at line 81, `OwnerTask` at line 56, `ownerOf` at line 164)
- Modify: `internal/factory/runtime/health.go` (`blocked` near line 372, `completed` near line 249) only as far as they must drop a liaison row's events
- Test: `internal/factory/runtime/owner_test.go`, `internal/factory/runtime/runtime_test.go`

**Interfaces:**
- Produces: `func (r *Runtime) liaisonRow(ctx context.Context, t registry.Task) (store.PR, error)`: the row is created on first use. Its repo is `store.Repo{NodeID: "factory:liaison", Owner: "factory", Name: "liaison", WatchOwner: "factory", Mode: store.RepoModePerPR}`. Its PR is `store.GitHubPR{NodeID: "factory:liaison:<id>", Number: int(t.ID), URL: "file://" + FactoryTaskDir(id), HeadSHA: "t<id>", Title: t.Title, GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: "factory"}`.
- Produces: `func LiaisonTask(nodeID string) (int64, bool)`.
- Produces: `ownerOf` returns `errLiaisonRow` for a liaison row, and its callers drop such an event as they drop one with no task.

- [ ] **Step 1: Write the failing tests**

`TestChatWithTheLiaisonLeavesTheTasksRowAlone`:
1. Launch fx-writer on the task's row and the liaison on its row.
2. Show the liaison's agent working with no run (the operator types).
3. After an observe pass, the liaison row has `human_active_at` set, and the task's row has none.
4. `Send` to fx-writer is not refused, and `AtWork` names nobody for "you".

`TestALiaisonsEventsNeverReachTheTasksRoles`: a blocked status and a finished turn of the liaison's session produce no `EventBlocked` and no completion for the task.

- [ ] **Step 2: Run them and see them fail**

Run: `go test -count=1 -run 'TestChatWithTheLiaisonLeavesTheTasksRowAlone|TestALiaisonsEventsNeverReachTheTasksRoles' ./internal/factory/runtime/`
Expected: FAIL. `liaisonRow` does not exist.

- [ ] **Step 3: Implement** `liaisonRow`, `LiaisonTask` and the `ownerOf` case.

- [ ] **Step 4: Run them and see them pass**, then the gate.

- [ ] **Step 5: Commit** `The factory's liaison gets its own owner row` with a DECISIONS entry.

### Task 4: `Runtime.Talk`, the liaison's typed chat, and its end

**Files:**
- Modify: `internal/factory/runtime/runtime.go` (beside `Launch` at line 514 and `Park` at line 947)
- Modify: `internal/factory/runtime/typed.go` (`typed` near line 199)
- Modify: `internal/factory/runtime/recover*.go` if a restart does not yet reconcile the liaison's row
- Test: `internal/factory/runtime/talk_test.go` (new)

**Interfaces:**
- Consumes: `liaisonRow` (Task 3), the role `fx-liaison` (Task 2).
- Produces: `func (r *Runtime) Talk(ctx context.Context, t registry.Task, prompt string) (pane string, err error)`:
  - With a live liaison session, it returns its pane and sends nothing.
  - Otherwise it opens or reuses a workspace labeled `<Tag> liaison <task key>` in the factory's herdr session, with the working directory `FactoryTaskDir(t.ID)`.
  - It starts `fx-liaison`. If `ResumeID` names a parked conversation, it resumes that one.
  - Only a fresh conversation gets `prompt`, through the agents manager's `Prompt` with `store.RunInitial`.
- Produces: `typed` reads the liaison's sessions too. Their typed turns become typed inputs of the task with the surface `liaison`.
- Produces: `Runtime.Park(ctx, t)` parks the liaison's row as well as the task's row.

- [ ] **Step 1: Write the failing tests**

- `TestTalkStartsOneLiaisonInTheTasksFolder`: `Talk` starts `fx-liaison` with the working directory `FactoryTaskDir(id)` and sends the prompt once. A second `Talk` returns the same pane and sends nothing.
- `TestTalkWorksAfterTheCopyIsFreed`: a task with no copy (its copy released) still gets its liaison, in its folder.
- `TestTalkResumesAParkedLiaison`: after `Park`, `Talk` resumes the same conversation id and sends no prompt.
- `TestARestartKeepsTheLiaison`: a new runtime over the same stores sees the live liaison, and `Talk` sends no second prompt.
- `TestTypedChatWithTheLiaisonCountsForItsTask`: a turn that herdr shows the operator typed into the liaison's pane becomes an `EventTyped` of the task with the surface `liaison`.
- `TestDroppingATaskParksItsLiaison`: `Park` of the task parks the liaison's session.

- [ ] **Step 2: Run them and see them fail**

Run: `go test -count=1 -run 'TestTalk|TestARestartKeepsTheLiaison|TestTypedChatWithTheLiaison|TestDroppingATaskParksItsLiaison' ./internal/factory/runtime/`
Expected: FAIL. `Talk` does not exist.

- [ ] **Step 3: Implement** `Talk`, the `typed` change, the `Park` change and the restart case.

- [ ] **Step 4: Run them and see them pass**, then the gate.

- [ ] **Step 5: Commit** `Runtime.Talk starts or finds a task's liaison` with a DECISIONS entry.

### Task 5: `--surface` and `--shown` on `answer` and `decide`

**Files:**
- Modify: `internal/cli/factory_answer.go` (flags at lines 53-56, the payload at line 98), `internal/cli/factory_decide.go` (flags at lines 48-50, `read := time.Now()` at line 94, the payloads at lines 97 and 112)
- Modify: `internal/factory/answers.go` (`AnswerPayload` at line 47, `recordAnswer` near line 484)
- Test: `internal/cli/factory_answer_test.go`, `internal/cli/factory_decide_test.go`, `internal/factory/answers_test.go`

**Interfaces:**
- Produces: `AnswerPayload.Shown time.Time`. When it is set, `recordAnswer` records it as the batch's shown time with `registry.ShowQuestions`, so the operator's time counts.
- Produces: the flags `--surface` (`cli` by default, else `liaison`) and `--shown` (RFC 3339) on both commands, and `--diff-read` on `decide`. `--diff-read` sets `DecidePayload.DiffOpened` to the shown time. Without it, `DiffOpened` stays zero: an unread diff.

- [ ] **Step 1: Write the failing tests**

- `TestAnswerAndDecideRecordTheLiaisonsSurfaceAndTime`:
  - `answer --surface liaison --shown 2026-11-06T10:00:00Z` records an input with the surface `liaison`, and the batch shows at 10:00.
  - `decide push --card 3 --surface liaison --shown <t> --diff-read` records the decision with `shown_at = t`, its surface, and a read diff.
- `TestAnUnknownSurfaceIsRefused`: `--surface screen` and `--surface x` exit 2 with a line that names the two values.
- `TestStatsCountALiaisonsAnswerAsACards`: an answer and a decision with the surface `liaison` count in `Answers`, `Decisions` and `OperatorSecs` as the same from a card.

- [ ] **Step 2: Run them and see them fail**

Run: `go test -count=1 -run 'TestAnswerAndDecideRecordTheLiaisonsSurfaceAndTime|TestAnUnknownSurfaceIsRefused|TestStatsCountALiaisonsAnswerAsACards' ./internal/cli/ ./internal/factory/...`
Expected: FAIL. The flags do not exist.

- [ ] **Step 3: Implement** the flags, the payload field and the shown time.

- [ ] **Step 4: Run them and see them pass**, then the gate with `make api-doc`.

- [ ] **Step 5: Commit** `answer and decide take --surface liaison and --shown` with the README lines in the factory's commands table.

### Task 6: The request `talk` and `magnum factory talk <task>`

**Files:**
- Modify: `internal/factory/requests.go` (the kinds at lines 34-43, the switch at line 146)
- Create: `internal/cli/factory_talk.go`
- Modify: `internal/cli/factory.go` (the `AddCommand` list at line 52, the usage strings at lines 44 and 48)
- Test: `internal/factory/requests_test.go`, `internal/cli/factory_talk_test.go` (new)

**Interfaces:**
- Consumes: `Runtime.Talk` (Task 4), the role and the prompt (Task 2).
- Produces: `ReqTalk = "talk"`, `type TalkPayload struct { TaskID int64; Surface string }`, and `func (e *Engine) talkTask(ctx context.Context, p TalkPayload) (string, error)`.
  - It renders `factory-liaison.md` with the task's prompt data.
  - It calls `Runtime.Talk`.
  - It answers `the liaison of <key> talks in pane <pane> of the factory's herdr session`.
  - A task that is done, stopped or failed is refused with a line that names its state.
- Produces: the command `magnum factory talk <task>` (a task id or a key). It sends the request with `sendFactoryRequest`, prints the answer, and then brings the pane to the front as the screen's `o` does, with `reveal.Options.Session`.

- [ ] **Step 1: Write the failing tests**

- `TestTalkRequestStartsTheLiaison` (engine, fake runtime): the request calls `Talk` once with the rendered prompt and completes with the pane in its result.
- `TestTalkRefusesAnEndedTask`: a stopped task fails the request, and `Talk` is not called.
- `TestFactoryTalkQueuesATalkRequest` (cli): `magnum factory talk PR-12345` enqueues `talk` with the task's id and the surface `cli`, and an unknown key exits 1 and names it.

- [ ] **Step 2: Run them and see them fail**

Run: `go test -count=1 -run 'TestTalkRequestStartsTheLiaison|TestTalkRefusesAnEndedTask|TestFactoryTalkQueuesATalkRequest' ./internal/factory/ ./internal/cli/`
Expected: FAIL. `ReqTalk` does not exist.

- [ ] **Step 3: Implement** the kind, the handler and the command.

- [ ] **Step 4: Run them and see them pass**, then the whole gate (`make test`, lint, `make api-doc-check`).

- [ ] **Step 5: Commit** `magnum factory talk <task> starts the task's liaison` with the README entry in the factory's commands table and a DECISIONS entry. The entry names the trial: three tasks through the liaison, judged on step starts and the operator's time against the first live task.

## After the plan

Run three tasks with `magnum factory talk` and compare `magnum factory stats` with the first live task. If the numbers hold, the next plan makes the liaison the front door: it starts with each task, the alerts point to it, and the factory prompts it on each new decision.
