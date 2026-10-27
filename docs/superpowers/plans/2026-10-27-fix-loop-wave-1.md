# Fix Loop Wave 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix six defects of the fix loop and `magnum open` that the operator met on 2026-10-27.

**Architecture:** Each task is one builder agent in its own worktree, following
`.claude/skills/magnum-improve/common-rules.md`. Tasks 1, 2 and 3 own disjoint files and run in parallel.
Task 4 starts after task 3 lands. Task 5 starts after tasks 2 and 4 land. The operator's session lands each
branch with `land.sh` and restarts the daemon once, after task 5.

**Tech Stack:** Go (latest), Charm v2, SQLite registry, fakes for git, gh and herdr.

**Spec:** `docs/superpowers/specs/2026-10-27-companion-fix-design.md`, section 5.

## Global Constraints

- AGENTS.md applies in full: hermetic tests, no `bin/magnum`, no git, gh, herdr or mysql against real
  directories or sockets, no private organization, repository or person in tracked files.
- A test that names the behavior comes first and fails first. Run `go test -count=1 -race` on the touched
  packages, then `make test`, then `make lint`. Fix only what lint reports in the lines you touched.
- If exported API changes, regenerate `docs/API.md` with `make api-doc`.
- Append one DECISIONS entry per behavior change at the end of the section it belongs to, never inside
  `## Learned rules`.
- Update README and config comments for every new command, key or behavior.
- No new dependencies.
- One commit per task, in the repository's commit style: a subject with the evidence, then the files, the
  tests that failed first, and "No new dependencies.".

## Review Focus

- A retry of `magnum fix-reply` after a POST whose answer was lost must post once (task 4).
- A thread of magnum's review that the operator resolved by hand drops its call, like one the fixer
  resolved (task 3).
- `magnum fix` on a PR without a slot whose head moved since the review must refuse as today and claim
  nothing (task 2).
- A conflict check whose base fetch fails must not block the fix. The fix runs without the merge step, and
  an event says why (task 5).
- `magnum open` without `--role` on a PR with no parked session at all keeps today's refusal (task 1).

---

### Task 1: `magnum open` restores through any parked session (spec 5.5)

**Files:**
- Modify: `internal/cli/open.go` (`openSessions` ~173, the closed-judge refusal ~233-246, `openWouldRestore`
  ~192, `openRoleOf` ~205)
- Test: `internal/cli/open_test.go`
- Docs: README `magnum open` row (~3131), a DECISIONS entry

**Interfaces:**
- Consumes: nothing new. `requestOpen` already accepts any role in `OpenPayload.Role`.
- Produces: nothing that other tasks use.

- [ ] **Step 1: Write the failing test** `TestOpenWithoutARoleRestoresThroughAParkedSessionWhenTheJudgesIsClosed`
  in `open_test.go`, with the cli test helpers (`h.session`, `h.requests`, `h.completePending`). Its PR has
  a closed codex-judge session and a parked claude-review session.

```go
// magnum open <ref> (no --role) sends one open request whose role is the parked one
if len(reqs) != 1 || reqs[0].Payload.Role != "claude-review" { t.Fatalf(...) }
// and prints no "starts a new round with fresh sessions" refusal
```

  A second case closes every session. It expects today's refusal text and no request.

- [ ] **Step 2: Run it and see it fail** with `go test -count=1 -run TestOpenWithoutARole ./internal/cli/`.
- [ ] **Step 3: Implement the rule.** The judge's session is closed and no `--role` is given: then pick
  the newest parked session of any role, by `started_at`. Apply the same rule in `openWouldRestore` and
  `openRoleOf`, so that the board's `o` asks the same question.
- [ ] **Step 4: Run the package tests with `-race`, then `make test` and `make lint`.** Expected: green.
- [ ] **Step 5: Update the README row, add the DECISIONS entry, and commit.**

### Task 2: `magnum fix` claims a slot when the pool took the PR's back (spec 5.4)

**Files:**
- Modify: `internal/engine/open.go` (extract the reserve, checkout and schema part of `openPR`, ~117-182)
- Modify: `internal/engine/fix.go` (`prepareFix` slot read ~394-400, `fixSlotRefusal` ~467-470, `runFix`
  ~631, `fixGuard` ~717)
- Test: `internal/engine/fix_test.go`
- Docs: README "Fixing your own PRs" (~2606), the `magnum fix --help` text, a DECISIONS entry

**Interfaces:**
- Produces: `func (e *Engine) claimAtReviewedHead(ctx context.Context, pr store.PR, repo store.Repo) (store.Slot, error)`
  in `open.go`, used by `openPR` and `runFix`. It reserves a slot (`Reserve` or `CreatePRWorktree`),
  checks out the PR's reviewed head, and loads the schema. It never pins and never holds.
- Produces: the refusal `no free <repo> slot right now; run magnum fix <ref> again shortly`. Magnum gives
  it when no slot is free, after it starts the eviction, as open does.

- [ ] **Step 1: Write the failing tests** in `fix_test.go`, with `fixHarness` and `fixablePR`.
  - `TestAFixOnAPRWhoseSlotWasHandedBackClaimsOneAtTheReviewedHead`: a reviewed PR with no slot row. Assert
    that `h.fix` starts the fixer, that `h.sl.all()` shows one claim and one checkout at the reviewed head,
    and that the slot is not pinned.
  - `TestAFixWithoutAFreeSlotRefusesAndNamesTheRetry`: assert the refusal text above and no fixer start.
  - `TestAFixOnASlotlessPRWhoseHeadMovedRefusesAndClaimsNothing`: assert today's refusal and no claim.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** `claimAtReviewedHead` and call it from `openPR`, whose behavior stays the same.
  Add the claim step to `runFix` before `fixGuard`, for a job whose slot ID is 0. Let `fixGuard` accept
  that job. If the review stood on the head, the "handed back" case of `fixSlotRefusal` becomes the claim.
  `whiteboard.go:383` keeps its refusal.
- [ ] **Step 4: Run `go test -count=1 -race ./internal/engine/`, then `make test` and `make lint`.**
- [ ] **Step 5: Update the README, the help text and DECISIONS, and commit.**

### Task 3: No stale "Needs you" call, and settlement says when the operator typed (spec 5.2 and 5.3)

**Files:**
- Modify: `internal/engine/fix_calls.go` (`call` ~148, `needsYouCalls` ~171), `internal/engine/fix_settle.go`
  (the calls ~105-118, the settle event's data ~83), `internal/engine/fix_loop.go` (the calls at the loop's
  end ~206-214)
- Modify: `internal/attention/fix_calls.go` (`fixCallRe` ~82-90, `fixCallsCause` ~136, a new call sentence)
- Modify: `internal/agents/background.go`, only to export what the engine needs from `humanTurn` (~192)
- Test: `internal/engine/fix_calls_test.go`, `internal/engine/fix_loop_test.go`,
  `internal/attention/attention_test.go`, `internal/engine/fix_round_test.go`
- Docs: README "Fixing your own PRs", one DECISIONS entry for each of the two behaviors

**Interfaces:**
- Produces: `func (e *Engine) freshNeedsYou(ctx context.Context, ...) []NeedsYouEntry`. The builder takes
  the parameter types from the existing code. Both call sites use it.
  - It drops an entry whose thread of magnum's own review is resolved.
  - It turns an entry whose open thread has a matching answer into a "resolve it" call.
  - If the re-review's `review-threads.json` is newer than the call, it reads that file. Otherwise it makes
    one GitHub read.
- Produces: the settle event's data key `operator_typed` (bool), and the message text
  `the operator typed into the fixer's pane during the run`.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheLoopsEndAsksNoReplyForAFixedFindingTheReReviewShowsAnswered`: a stored entry "reply fixed in
    e05fbde", and the re-review's threads show the thread resolved. Assert that no call is raised.
  - `TestAnAnsweredOpenThreadOfMagnumsReviewAsksOnlyToResolveIt`: the answer `Fixed in e05fbde`, the thread
    open. Assert that the call asks only to resolve, and that `fixCalls` still returns it for the card.
  - `TestAThreadTheOperatorResolvedByHandDropsItsCall`: resolved, with no fixer reply. Assert no call.
  - `TestAnOtherReviewersThreadStillAsksOnlyForAReply`: assert the existing reply-only call.
  - `TestSettlementSaysTheOperatorTypedIntoTheFixersPaneDuringTheRun`: a Claude transcript with a typed turn
    after magnum's prompt. Assert the event text and `operator_typed: true`. A transcript without such a
    turn gives no flag.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** `freshNeedsYou`, and call it at both sites. Add the new call sentence to
  `fixCallRe` and `fixCallsCause`. At settlement, read the fixer's transcript with `humanTurn`, for Claude
  kinds only. Add the flag and the sentence to the settle event.
- [ ] **Step 4: Run `go test -count=1 -race ./internal/engine/ ./internal/attention/ ./internal/agents/`,
  then `make test` and `make lint`.**
- [ ] **Step 5: Update the README and DECISIONS, and commit.**

### Task 4: Every fixer reply goes through `magnum fix-reply` (spec 5.1)

This task starts after task 3 lands.

**Files:**
- Create: `internal/cli/fix_reply.go`, modelled on `internal/cli/post_review.go` (no config, no registry)
- Modify: `internal/postreview/replies.go` (a fix-reply runner beside `RunReplies`, with `FixReplyMarker`
  ~51), `internal/cli/cli.go` (~107, register the command)
- Modify: `internal/agents/templates.go` (`FixPromptInput` ~787 gets `FixReplyCommand string`, like
  `DBLockCommand`), `prompts/fix.md` (still one paragraph), `skills/magnum-fix/SKILL.md` section 6
- Modify: `internal/engine/fix_external.go` (an event for each author reply without the marker since the
  job started, with the `byFixer` filter ~618 and without `IsFixReply`)
- Test: `internal/postreview/replies_test.go`, `internal/cli/fix_reply_test.go`,
  `internal/agents/prompt_test.go` (~289, ~362 and ~395 expect the old `gh api` heredoc and change),
  the goldens `fix.golden` and `fix_no_findings.golden`, `internal/engine/fix_external_test.go`
- Docs: README (the command table and "Fixing your own PRs"), prompts/README.md (the new variable), a
  DECISIONS entry

**Interfaces:**
- Produces: `magnum fix-reply (--comment <id> | --issue) < body`. It reads the PR from `MAGNUM_PR_URL`.
  - It refuses a body that contains `<!-- magnum:`, and it appends `<!-- magnum:fix -->`.
  - It makes sure that a review comment belongs to the PR.
  - If a reply with the same body and the marker exists, it posts nothing.
  - It does not copy the pane guard of `cli/verdict.go`.

- [ ] **Step 1: Write the failing tests.**
  - `TestFixReplyEndsTheBodyWithTheFixMarker`: an inline reply and a PR comment on a fake gh. Assert that
    each posted body ends with the marker.
  - `TestFixReplyRefusesABodyWithAMagnumMarker`.
  - `TestFixReplyRefusesACommentOfAnotherPR`.
  - `TestFixReplyPostsOnceWhenTheSameReplyExists`.
  - `TestTheFixPromptNamesTheFixReplyCommand`, through the goldens.
  - `TestSettlementNamesAnAuthorReplyWithoutTheMarker`.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** the runner, the command, the prompt field and its value (the engine fills it as
  it fills the db-lock line), the skill's section 6, and the settlement event. After the code is right,
  update the goldens with `go test ./internal/agents -update`.
- [ ] **Step 4: Run `-race` on cli, postreview, agents and engine, then `make test`, `make lint` and
  `make api-doc`.**
- [ ] **Step 5: Update the README, prompts/README.md and DECISIONS, and commit.**

### Task 5: The fixer merges the base when the PR conflicts with it (spec 5.6)

This task starts after tasks 2 and 4 land.

**Files:**
- Modify: `internal/gitx/branches.go` (a merge-tree helper beside `ReplayConflicts` ~219)
- Modify: `internal/engine/engine.go` (the `Git` interface ~210), `internal/engine/fix.go` (`prepareFix`
  ~411-427, `fixGuard`), `internal/engine/fix_external.go` (`othersWaiting` ~406-433, so that a conflict
  counts as work)
- Modify: `internal/agents/templates.go` (`FixPromptInput` gets `ConflictBase`, `ConflictSHA` and
  `ConflictPaths []string`), `prompts/fix.md`, `skills/magnum-fix/SKILL.md` (a merge section before the
  findings, and exemptions in the rules at ~33-37 and ~134)
- Modify: `internal/engine/fix_loop.go` (`needsYouEntryRe` ~52 accepts `conflict: <paths>` without a comment
  id), `internal/engine/fix_settle.go` (`pushedHead` ~286 accepts the merge commit in the Pushed lines)
- Test: `internal/gitx` (a real-git test of the helper), `internal/engine/fix_test.go` (`TestFixRefusals`,
  the `othersOnly` case), `internal/engine/fix_loop_test.go`, the goldens
- Docs: README "Fixing your own PRs", prompts/README.md, a DECISIONS entry that amends "A textual merge
  conflict is no finding"

**Interfaces:**
- Consumes: `claimAtReviewedHead` from task 2 (the guard runs after the claim), and the prompt field layout
  of task 4.
- Produces: `func (c *Client) MergeConflicts(ctx context.Context, dir, head, base string) ([]string, error)`
  in gitx. It returns the conflicting paths of `git merge-tree --write-tree <head> <base>`, sorted. For a
  clean merge, it returns nil.

- [ ] **Step 1: Write the failing tests.**
  - `TestAHandStartedFixOnAConflictingPRWhoseReviewFoundNothingStartsAndNamesTheBase`: assert that the fix
    starts, and that the prompt carries the base branch, its SHA and the paths.
  - `TestAutofixDoesNotStartForAConflictAlone`.
  - `TestAConflictEntryUnderNeedsYouBecomesACall`.
  - `TestAPushWhoseFirstCommitMergesTheBaseSettles`.
  - `TestAFailedBaseFetchRunsTheFixWithoutTheMergeStep`: assert the event and no conflict fields.
  - A real-git test of `MergeConflicts`: a clean merge gives nil, and a conflict gives its path.
- [ ] **Step 2: Run them and see them fail.**
- [ ] **Step 3: Implement** the helper, the guard's fetch and check, the prompt fields and the settlement
  changes. The skill section says these things:
  - Run `git merge --no-edit <sha>`. Never rebase and never force-push.
  - Run the specs of the resolved files through the db-lock line.
  - If the environment blocks them, write "not run: environment".
  - If a conflict needs a product decision, run `git merge --abort` and list `conflict: <paths>` under
    "Needs you".
- [ ] **Step 4: Run `-race` on gitx, engine and agents, then `make test`, `make lint` and `make api-doc`.**
- [ ] **Step 5: Update the README, prompts/README.md and DECISIONS, and commit.**
