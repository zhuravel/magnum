# Companion fixes: the fix loop changes a second repository when a finding needs it

Status: approved in conversation on 2026-10-27, then revised the same day after a review by Codex and two
Claude subagents (section 9). The operator decided four things. Magnum opens the companion PR itself, under
the fix loop's unattended rules. Magnum reviews the companion PR in full. The companion PR can merge once a
declared branch of the main repository contains the main PR's head with green CI. One fix job works in two
worktrees, and the engine publishes the companion branch and PR. Wave 1 (section 5) ships first, with its
own plan. The companion work (section 6) follows in three phases, each with its own plan.

## 1. Goal

A review can find that a PR breaks code in another repository. On talkable/talkable#12112 (2026-10-27), a
password reset no longer logs the user in. The judge found that a Selenium test in
talkable/talkable-tests-automation still expects the old behavior. The repository notes told the judge
where those tests live. The fix loop did not act on that finding. The fix skill knows only the PR's own
checkout, so the fixer listed the finding under "Needs you" and stopped. The operator then typed into the
fixer's pane, and the agent made the change and opened talkable/talkable-tests-automation#8015 in two
minutes.

After this change, the operator declares that one repository is a companion of another. When a finding's
fix is in a declared companion, the same fix run changes both repositories. Magnum pushes the companion
branch and opens a draft companion PR. Magnum reviews that PR in full. The review's findings come back to
the same fix loop. Then magnum waits for the declared branch of the main repository to contain the main
PR's head with green CI. At that point, magnum marks the companion PR ready and tells the operator that it
can merge. The operator only merges.

## 2. Context

### 2.1 What exists

- The fix loop (`internal/engine/fix.go`, `fix_settle.go`, `fix_loop.go`) runs the role claude-fix in the
  PR's own slot as the operator. It follows `skills/magnum-fix/SKILL.md` and `prompts/fix.md`. The fixer
  pushes once with `git push origin HEAD:<branch>`, replies in magnum's threads with a
  `<!-- magnum:fix -->` marker, and writes a report. Settlement reads the report (`FixReport`,
  `fix_report.go`), makes sure that the push reached GitHub, and forces a re-review. `continueFixLoop`
  starts the next cycle, ends the loop clean, or ends it with the "Needs you" calls
  (`internal/attention/fix_calls.go`).
- The skill says "You work in the author's checkout", "Never touch another branch" and "Never stop to ask
  or wait for a human" (`skills/magnum-fix/SKILL.md:10`, `:30-38`). It never names another repository.
- The loop is built around one PR. `FixState` has one head and one `ForcedAt` (`fix_state.go:61-76`).
  `fixAfterRound` reads the fix row of the PR whose round ended (`fix_loop.go:93-100`). Settlement goes on
  to a re-review only after a push to that PR (`fix_settle.go:120-128`).
- The review skill decides that a finding is fixed only from the main PR's own code at its head
  (`skills/magnum-review/SKILL.md:159`).
- A watch covers repositories by `include`. A repository in `manual_repos` is reviewed only on request. A
  repository without a `[[pool]]` uses per-PR worktrees beside its clone (`internal/slots/perpr.go`). Slot
  rows are keyed by PR, with a unique index on `pr_id`. So a worktree for a branch without a PR cannot be
  a slot row.
- The engine has no `git push`, no PR creation and no "mark ready" call today.
- The fixer receives a frozen copy of the main repository's notes only (`fix.go:820-829`).
- Nothing in `[[watch]]` or `[[repo]]` names a second repository (`internal/config/config.go:348`, `:671`).
- The DECISIONS entry "The fix loop: magnum fixes its own findings on your PRs" rejected an author
  worktree, because magnum never touches the operator's checkouts. The entry on the quit before a
  checkout rejected closing a pane to force a quit.

### 2.2 What went wrong on #12112

The transcript of the claude-fix session shows these facts.

1. The first run fixed P2 and pushed. It listed P1 (the Selenium test) and P3 (the author's call) under
   "Needs you" and ended at 12:09 UTC.
2. The operator typed "do the fix in ~/Projects/talkable-tests-automation - open pr". The agent ran
   `git worktree add` from the operator's own checkout, from `origin/bastion`, and opened #8015.
3. Nobody compiled #8015, because the machine has no JDK. The PR body copied the merge-order wording of an
   earlier operator PR.
4. The P1 and P3 replies have no `<!-- magnum:fix -->` marker, so magnum reads them as the operator's own.
5. At the loop's end, magnum raised "reply fixed in e05fbde in magnum's thread and resolve it", although
   the agent had already replied and resolved the thread.
6. The worktree `talkable-tests-automation.pr27587` stays on disk, and no record names it.

## 3. Goals, non-goals, anti-goals

Goals:

- A finding whose fix is in a declared companion repository does not end the loop. The fixer fixes it in
  the same run.
- Magnum creates, publishes, reviews, tracks and cleans up the companion work. Each step is guarded,
  recorded and recoverable after a restart.
- The operator sees both PRs on the main PR's card. When the companion PR can merge, the operator gets one
  toast.
- Wave 1 fixes the defects of section 2.2 and three problems found the same day.

Non-goals:

- Magnum does not merge the companion PR.
- Magnum does not prove that a deploy finished. It reads a branch and its CI (section 6.5).
- Magnum does not compile the companion code unless the companion declares a `verify` command.
- Magnum does not discover companions from notes, findings or PR text.

Anti-goals. If any of these happens because of magnum's own steps, the change fails:

- Magnum writes to a repository that the operator did not declare as a companion.
- Magnum touches the operator's own working tree of the companion repository.
- Magnum marks a companion PR ready while the main PR's head is not on the declared branch with green CI.
- A companion fix opens a companion PR of its own (no recursion).

The fixer agent runs with permission prompts bypassed, as the fix loop does today. For the agent, these
rules are best-effort: the skill states them, and the checks after the run name what broke them (section
6.7). This is the exposure the fix loop already accepts, because the fixer pane can push to any
repository that the operator can.

## 4. Approaches

The operator chose approach A.

A. One fix job, two worktrees. The fix job also prepares a companion worktree. The same claude-fix session
changes both repositories. The engine pushes the companion branch and opens the draft PR. One loop waits
for both reviews. The agent that found the gap keeps its context, and one session matches one loop. The
cost is that one run acts in two repositories, and settlement and the loop track two PRs.

B. A separate companion job. The main fixer writes a structured request into its report. Magnum starts a
fresh claude-fix session in the companion worktree, under the parent loop's cap. This isolates failures,
and each session reads one repository. It needs parent and child records, two sessions per cycle, and a
second agent that knows the problem only from the finding text. Codex recommended B. The operator chose A,
because the engine still controls the risky steps (publication and the worktree).

C. Open the draft, then treat it as an ordinary own PR with its own fix loop. This gives two loops and two
caps. It was dropped, because the operator expects one loop and one result. After the main PR closes, the
companion PR becomes an ordinary own PR anyway (section 6.4).

## 5. Wave 1: fixes that do not need companions

Wave 1 ships first, as its own plan. Each item hurts today. Each item starts with a failing test.

### 5.1 Every fixer reply carries the marker

The P1 and P3 replies of #12112 lacked `<!-- magnum:fix -->`.

- A new command, `magnum fix-reply`, posts one reply. It needs no config and no registry, like
  `magnum post-review`. It reads the PR from the pane's `MAGNUM_PR_URL`. With `--comment <id>` it answers
  a review comment, and with `--issue` it writes a PR comment. The body comes from stdin.
- It refuses a body that already carries a `<!-- magnum:` marker. It appends `<!-- magnum:fix -->`.
- Before it answers a review comment, it makes sure that the comment belongs to the PR.
- Before it posts, it reads the existing replies. If a reply with the same body and the marker exists, it
  posts nothing and says so. A retry after an unclear failure therefore posts once.
- The fix prompt gives the command line, as it gives the db-lock line (`FixPromptInput`). Skill section 6
  says that every reply goes through it.
- The command does not copy the guard of `cli/verdict.go`, which refuses panes with `MAGNUM_PR_URL`.
- Nothing forces a reply typed by hand through the command. Settlement names each reply of the author
  without the marker since the job started, in an event. Magnum never edits a posted comment.

First failing test: `TestFixReplyEndsTheBodyWithTheFixMarker` (a review-comment reply and a PR comment,
and the refusal of a body with a marker).

### 5.2 No stale "Needs you" call

On #12112 the call came at the loop's end, from the report's stored entries (`fix_loop.go:206-214`). The
agent had replied and resolved the thread hours before.

- One helper runs at both places that raise calls (`fix_settle.go:105-118` and `fix_loop.go:206-214`).
- For a thread of magnum's own review, it reads the thread again. If the re-review's `review-threads.json`
  is newer than the call, it uses that file. Otherwise it makes one read.
- A resolved thread drops the call.
- An open thread with an answer that matches the call changes the call to "resolve it". A `Fixed in`
  answer matches "reply fixed". A `Won't fix` or `Not a bug` answer matches "decide".
- Any other thread keeps its call.
- Threads of other reviewers keep today's rule: magnum asks only for a reply there, never a resolution
  (DECISIONS, near line 10490). `dropFixerAnswered` stays as it is.
- The new sentence also goes into `fixCallRe` and `fixCallsCause` (`attention/fix_calls.go`). Otherwise
  the call disappears from the card.

First failing test: `TestTheLoopsEndAsksNoReplyForAFixedFindingTheReReviewShowsAnswered`.

### 5.3 Settlement says when the operator typed in the fixer's pane

`HumanActiveAt` cannot show this. The observer sets it only while no run is in flight
(`agents/observe.go:304`), and it is per PR.

- At settlement, the engine reads the fixer's transcript with `humanTurn` (`agents/background.go:192`). A
  typed turn after magnum's prompt means that the operator drove the run.
- The settlement event then says "the operator typed into the fixer's pane during the run" and carries
  `operator_typed: true`. Nothing else changes.
- This works for Claude kinds only. Other kinds get no flag.
- Typing after the run is already the `agent.human_active` event.

First failing test: `TestSettlementSaysTheOperatorTypedIntoTheFixersPaneDuringTheRun`.

### 5.4 `magnum fix` claims a slot when the PR has none

On talkable/talkable#11907 the review stood on the head, but the pool had taken the slot back. The fix
refused with "has no checkout of its reviewed head: its slot was handed back".

- The part of `openPR` that reserves a slot, checks out the reviewed head and loads the schema
  (`engine/open.go:117-182`) moves into a helper. `openPR` keeps its pin, its hold and its sessions.
- `runFix` gets a claim step before `fixGuard`, for a job without a slot. `fixGuard` accepts that job.
- The claim works as the open's does. If no slot is free, the eviction starts, and the fix refuses with
  `no free slot right now; run magnum fix <ref> again shortly`.
- If the review did not stand on the PR's head, the fix refuses as today, and no claim happens.
- `whiteboard.go:383` keeps its refusal.

First failing tests: `TestAFixOnAPRWhoseSlotWasHandedBackClaimsOneAtTheReviewedHead`, and a second test
for the refusal without a free slot.

### 5.5 `magnum open` restores through any parked session

A plain `magnum open` of #11907 refused because the judge's session was closed. The parked claude-review
session restored the PR with `--role claude-review`.

- If no `--role` is given and the judge's session is closed, open uses the newest parked session of any
  role (`cli/open.go:233-246`).
- `openWouldRestore` and `openRoleOf`, which the board uses, follow the same rule.

First failing test: `TestOpenWithoutARoleRestoresThroughAParkedSessionWhenTheJudgesIsClosed`.

### 5.6 The fixer merges the base when the PR conflicts with it

On talkable/talkable#12079 (2026-10-27) GitHub reported `CONFLICTING`, and the fixer did not resolve it.
The skill forbids pull, merge and rebase, and it changes only what a finding names. The DECISIONS entry
"A textual merge conflict is no finding" keeps conflicts out of findings. The operator expects the fix
session to resolve the conflict.

- In the fix guard, the engine fetches the base branch, as the slot's checkout does. Then it runs
  `git merge-tree --write-tree` on the head and the base tip, as `gitx.ReplayConflicts` does. GitHub's
  `mergeable` is not used, because GitHub answers `UNKNOWN` until its computation ends.
- If the merge conflicts, the job carries the base branch, the base SHA and the conflicting paths. The
  prompt names them, in its one paragraph.
- A conflict counts as work in `prepareFix`. A hand-started `magnum fix` on a conflicting PR runs, also
  after a review that found nothing. Autofix still starts only for findings.
- A new skill section comes before the findings. The fixer runs `git merge --no-edit <base sha>`, never a
  rebase or a force-push. It resolves each conflict so that both sides keep their intent.
- The rule "change only what a finding names" exempts the merge's own changes. The rule "add no lockfile
  entry" exempts the entries that the base brings.
- The fixer runs the specs of the resolved files through the db-lock line. If the environment blocks them
  (new migrations or gems from the base), the report says "not run: environment". The re-review's
  readiness step reloads the schema.
- If a conflict needs a product decision, the fixer runs `git merge --abort`. The report lists
  `conflict: <paths>` under "Needs you". `needsYouEntryRe` accepts that entry without a comment id.
- The report's Pushed lines name the merge commit, so `pushedHead` accepts the push. The re-review is
  forced as usual.
- The merge can bring the base's `AGENTS.md` or `.claude/` into a running Claude fixer. These are the
  base's files, which the team owns, so this is accepted.
- The DECISIONS entry "A textual merge conflict is no finding" gets an amendment: still no finding, but
  the fixer resolves it.

First failing test: `TestAHandStartedFixOnAConflictingPRWhoseReviewFoundNothingStartsAndNamesTheBase`.

## 6. The companion design

### 6.1 The declaration

The operator declares a companion on the main repository's `[[repo]]` entry:

```toml
[[repo]]
repo = "talkable/talkable"

[[repo.companion]]
repo = "talkable/talkable-tests-automation"   # owner/name, covered by a [[watch]]
base = "bastion"                              # the companion PR's base branch
merge_after = "staging-bastion"               # a branch of the main repository; see 6.5
verify = ""                                   # a command the fixer runs in the companion worktree; "" = none
```

The companion repository must be covered by a `[[watch]]`, so that magnum can review its PRs. The operator
adds it to the watch's `include` and to `manual_repos`. Then magnum reviews only the companion PRs that it
opens and the PRs that the operator asks for. Validation refuses these declarations:

- a companion that no watch covers
- an empty `base` or `merge_after`
- a companion that is the main repository
- a companion with companions of its own

Notes, findings and PR text never add a companion. Magnum writes the notes partly from PR text, and
nothing from a PR can authorize a write (AGENTS.md, "Nothing from a PR is trusted").

### 6.2 The registry

A new table records each companion of a main PR. A row holds the main PR, the companion repository and
the branch. It also holds the companion PR's number, the provisional worktree, the last published tip,
the link state and the step state. A migration adds it, so the landing needs `daemon-restart --drain`. The record outlives the fix
loop, because the companion PR can stay open after the loop ends.

### 6.3 Phase 1: the worktree, the publication and the reply

1. When the fix job starts, the engine reads which companions the review's findings name. If the body of
   a finding links a file of a companion repository on GitHub, the finding names that companion. The
   judge prompt carries the declared companions (repository and base) as data from the config. A finding
   about a companion then links the file at the base.
2. For the first cycle, the engine creates a provisional worktree beside the companion's clone, detached
   at `origin/<base>`. It records the path before it creates it. Inventory and cleanup know this kind.
3. The fix prompt names each companion worktree, its repository, its base, and the `verify` command if
   there is one. The skill gets a companion section. The fixer changes a companion worktree only for a
   finding that names that companion. It commits there detached, and it never pushes there.
4. The report gets a `## Companion` section: the repository, the commits, the finding ids, and "verified
   with <command>" or "not compiled". `fixSectionBodies` learns this section.
5. Settlement makes sure that the commits are in the registered worktree and descend from the recorded
   base. Then publication runs as guarded steps, each with begin, ok and fail events:
   1. Record the intent: the branch, the commits and the expected remote tip (none for a new branch).
   2. Push with `--force-with-lease=refs/heads/<branch>:<expected tip>`. For a new branch, the expected
      tip is empty. If someone created the branch meanwhile, the push fails.
   3. Record the pushed tip.
   4. Look for an open PR whose head is the branch. If none exists, create the draft PR. Its body is a
      fixed template: the main PR, the findings it fixes and the merge rule. If no `verify` command ran,
      the body also says "not compiled". It carries no PR text.
   5. Write the companion PR's registry row and the link.
   6. Post the reply in the main PR's thread through the code of `magnum fix-reply`: "Fixed in
      <companion repo>#<M>".
6. After a restart, each record in a middle state is reconciled from GitHub before anything else: the
   branch tip, the PR found by its head branch, and the reply found by its marker in the thread.
7. If the companion push or the PR creation fails, the finding goes to "Needs you" with the cause. The main
   PR's push settles as today, apart from the companion.
8. The branch name is the main PR's head branch name. If the remote branch exists and the record links it
   to this loop, magnum adopts it. Otherwise magnum never pushes to it. It adds the suffix `-companion`,
   then `-companion-2` and so on, until the name is free.
9. After the PR exists, later cycles use the companion PR's own per-PR slot. Review and fix take turns
   there, as they do on the main PR. Magnum removes the provisional worktree.
10. Settlement lists the worktrees of the main clone and of each companion clone, and names each worktree
    that appeared since the job started and that no record owns. It never removes them.

### 6.4 Phase 2: the loop

- `FixState` records the expected review of every PR that a cycle touched. `continueFixLoop` waits for all
  of them and advances once, with compare-and-set.
- A cycle forces a review of every PR that it pushed to. If the engine posted a companion reply, the cycle
  also forces the main PR's re-review, because that reply changed the main thread's input.
- The review skill gets one sentence: a reply "Fixed in <declared companion>#<M>" answers a finding about
  that companion's code, unless the judge shows that the companion PR does not fix it. The companion PR
  gets its own full review, so the main judge reads no companion diff.
- The prompt of a later cycle carries the companion PR's review as it carries the main one: its URL, its
  review file, its threads and the other reviewers' items. Skill sections 2, 6 and 7 take the owner, the
  repository and the number per PR. The same policy applies to both PRs, including optional P3 findings.
- The companion's "Needs you" entries go into the same calls.
- `fixAfterRound` routes the end of a companion round to the main PR's loop through the link. Autofix and
  `magnum fix` refuse a linked companion PR and name the main PR's loop.
- If one review pauses or fails, the loop waits or fails as it does today for the re-review.
- The link holds while the main PR is open. When the main PR merges or closes, the link ends. The companion
  PR then becomes an ordinary own PR, and `magnum fix` works on it.

### 6.5 Phase 3: the merge gate

The companion PR is advisory-gated: magnum marks it ready, but nothing on GitHub enforces the order. All
of these must hold:

- The tip of `origin/<merge_after>` in the main repository contains the main PR's current head. Magnum
  reads this with the compare API, so it fetches nothing into the operator's clone.
- The checks of that tip passed, after `ignore_checks`. The deploy skill merges the PR's branch into
  `staging-<env>`, and Jenkins deploys the environment only after that CI passes.
- The companion PR's last review is on its current head and left nothing open.
- No fix cycle runs.

Then magnum marks the companion PR ready and sends one toast. Magnum checks again on each tick, with a time
limit and the GitHub budget rules of `baseTipChecks`, while the companion PR is open. If a condition stops
holding, magnum turns the PR back into a draft and sends a toast with the reason. That covers a staging
reset, a new main head, and a push by someone else to the companion branch. For an unreviewed companion
head, the toast asks for `magnum review`. If the main PR closes without a merge, magnum never marks the
companion PR ready. It sends a toast that the companion PR waits for the operator's decision.

### 6.6 Cleanup

The companion PR's slot follows the per-PR rules. After the companion PR closes or merges, a guarded step
removes what remains. If the companion PR never opened, the provisional worktree goes after the main PR
closes.

### 6.7 Isolation

Magnum's own steps write only to the declared companion repository, its registered worktrees and the
refs that magnum fetches. For the agent, the rules are best-effort: the skill states them, settlement
checks the commits and the new worktrees (6.3, step 10), and the events name what broke them. A sandboxed
kind needs an `add-dir` key for the companion worktree. That is out of scope. The fixer's
session stays live across cycles. A companion first named in cycle 2 reaches it through the prompt.

## 7. Constraints

- The repository is public. Config examples use talkable or example names. Tracked files name no private
  organization or person.
- Repositories, roles and kinds stay data. The engine hardcodes no repository, role or CLI name.
- Every destructive or outward step is idempotent or guarded and writes begin, ok and fail events. The
  registry is the source of truth.
- Tests are hermetic, with fakes for git, gh and herdr. A bug fix starts with a failing test.
- The fix role is a gh identity, whose environment is the operator's gh login (`identity/gh.go`). The
  engine's push and PR calls for the companion use the same login.

## 8. Left to the builder

- The columns of the companion table, and its migration number.
- How the engine reads the finding links that name a companion.
- The gitx and GitHub client methods for the push, the PR creation, and the change between draft and ready.
- The events, the board's card line and the toast wording, in the style of the existing fix events.
- The exact skill wording, within the size limit of the review skill.
- The size of the time limit for the merge gate's checks.

## 9. Review on 2026-10-27

Codex and a Claude subagent reviewed the first version. A third subagent mapped it to the code. This
version answers their findings as follows.

- Publication had no recovery protocol: 6.2, and 6.3 steps 5 to 7.
- The ready mark went stale in some cases: 6.5 checks again and turns the PR back into a draft.
- Someone else can push to the companion branch: the lease push in 6.3, and the review rule in 6.5.
- The main PR's merge stranded the companion: the link ends with the main PR (6.4).
- A companion-only cycle never re-reviewed the main PR, and the review skill never accepted "Fixed in
  <repo>#<M>": 6.4.
- The isolation anti-goals were stated as guarantees: section 3 now calls them best-effort for the agent.
- Wave 1 item 2 asked for resolutions in other reviewers' threads: 5.2 keeps today's rule.
- Wave 1 item 4 relied on `HumanActiveAt`, which does not see typing during a run: 5.3 reads the
  transcript.
- Wave 1 item 3 (unknown worktrees) needs the companion clones to be useful: it moved to 6.3, step 10.
- The conflict merge contradicted the skill and read a stale base: 5.6 fetches in the guard, uses
  `git merge-tree`, and exempts the merge in the skill's rules.
- The config example used `name` instead of `repo`: fixed in 6.1.
- The design needed three worktrees: the provisional one lives for one cycle only (6.3, step 9).
