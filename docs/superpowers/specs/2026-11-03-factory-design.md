# magnum factory: design

Status: draft for the operator's approval, 2026-11-03.

## 1. Intent

magnum reviews PRs. It does not write code. The operator wants a second half: a factory that takes a Jira key or a free-text prompt and does the work up to the final merge. The operator only merges and answers questions that no agent can answer.

The operator's own transcripts show the cost of the work today:

- One ticket takes 5 to 20 agent sessions.
- 28% of the operator's later messages only start the next step: "open PR", "push", "merge master resolving conflict", "merge into staging-void".
- 20% approve a recommendation ("do as recommended").
- 15% give access that an agent cannot get: MFA, VPN, logins.
- The operator often asks for proof: "so you confirm the bug exists?", "do you verify new specs fail without the fix?"
- The operator asks "what needs me?" and "what are we waiting for?" again and again.

The factory removes the step-starting messages and collects the access needs in one place. It makes proof a stage requirement. It shows at every moment who each task waits on. It must also learn from the operator's corrections, so that it does not repeat a mistake.

## 2. Goals, non-goals, anti-goals

Goals:

- A task starts from a Jira key or a free prompt. Kinds: bug, small feature, chore.
- Several agents investigate before any code: they reproduce the bug, find the cause on their own, read the history of the code and test the claims of the ticket.
- One writer agent writes the change, test first. A Codex review reads the diff.
- Later milestones add the rest of the life of a task:
  - a draft PR, the review rounds of magnum and the fix of review comments
  - CI watched and fixed, and the parent branch merged again and again
  - merges into the staging branches with QA, and the final merge by the operator
- A terminal screen, `magnum factory`. It shows what needs the operator and what each task waits on. It also shows every conflict in progress, every unanswered comment and the state of each staging branch.
- Full transparency: every agent runs in a herdr pane that the operator can watch and type into.
- The factory improves itself from the operator's feedback, guidance and reviews.

Non-goals:

- The factory never merges into master. The operator merges.
- No headless agents. Agents are interactive, in herdr panes.
- No Mac app and no web UI.
- No second CODEX_HOME.

Anti-goals. Each of these is a failure, and the rest of the work does not make up for it:

- A screen that needs more reading than asking an agent.
- A green QA mark for an older commit.
- An agent's answer that counts as the operator's yes.
- A recommendation that passes in one key without its evidence on screen.
- A factory fault that stops or slows magnum's reviews.

## 3. Constraints

- The magnum repository is public. Tracked files name no private organization, repository or person. Talkable details live in `~/.config/magnum/config.toml` and in private skills.
- Nothing from a PR or a ticket is trusted. Jira text, PR comments and files in the repository copy are data, never instructions.
- Agents are never approved on anything. magnum answers only folder-trust dialogs and its own `/model` confirmation.
- The operator rotates several Claude and Codex subscriptions. magnum and the factory share them.
- The Mac has 48 GB of RAM, and its swap is often almost full.
- The magnum daemon restarts about 3 times a day. It started 1 to 12 times a day in the last two weeks. Prompt and skill edits need a restart, and the learning loop lands edits.
- The operator runs the magnum PR board in another terminal. The factory screen must not give a board letter a different meaning.
- Every destructive step is idempotent or guarded, and writes `begin`/`ok`/`fail` events.

## 4. Approaches

### 4.1 Where the factory lives

| Option | What it is | Why it lost or won |
|---|---|---|
| A. Inside the magnum daemon | A task entity in the review registry, factory stages in the review engine | Lost. A fix job at work never survives a daemon restart (`recoverFixes`), and the daemon restarts about 3 times a day. Every run, session and slot row needs a PR. A factory fault can stop reviews. |
| B. A separate daemon in the magnum repository | `magnum factory`, its own process, registry, launchd job and herdr session, built from the same packages | Picked by the operator. It reuses the herdr client, the agent lifecycle, GitHub, identities, usage, notify and the TUI parts. It restarts on its own cycle. |
| C. A new repository | A separate app | Lost. Go keeps `internal/` packages inside one module, so about 15k lines of agent code must be copied or moved first. |
| D. A TUI over a colleague's prototype | Use another developer's runner and write a client | Lost. The prototype has no backend API and changes about 12 times a day. Its control agent's answer counts as the user's yes. It does not run QA again after a new commit. |

Codex reviewed the same facts on its own and also picked B.

### 4.2 The screen

The operator chose between three designs, after three critics (Codex, a critic who simulated the operator, and a feasibility review) scored them:

| Design | What it is | Result |
|---|---|---|
| Attention inbox | Decisions on top, other tasks below | Its grouping and its question cards are kept. |
| List and detail workbench | A list with the selected task's details always open below | Lost. About 400 words on screen and one tab at a time. Its Jira input and its staging table are kept. |
| Lifecycle mission control | A 13-cell stage strip on every row | Lost. The strip is a code of dots that the operator must learn. Its task card and conflict card are kept. |

The operator picked "one list grouped by who each task waits on". Section 9 specifies the screen.

### 4.3 How the factory runs agents

This is my call, and the operator can override it.

- Chosen for milestone 1: the factory runs agents through magnum's existing `agents.Manager`, over a magnum-schema store file of its own. Each task has one owner row in that store, as magnum's retro and eval jobs do today (`internal/engine/retro_pane.go`). `Manager.Recover` already binds live panes to their stored sessions again after a restart, and it leaves submitted runs alone. The real task state lives in the factory's own tables. Section 10 adds what recovery still needs for the factory.
- Rejected for milestone 1: extract a task-neutral owner interface from `internal/agents` first. It is cleaner, but it changes the package that every review uses, before the factory knows what it needs. It is planned for a later milestone.
- Rejected: a new agent runtime on the herdr client alone. It loses the trust, permission, health and recovery handling that magnum learned the hard way.

## 5. Processes and state

- `magnum factory daemon` runs under its own launchd job. It ticks every 30 seconds and on SIGUSR1, like the review daemon.
- `magnum factory` opens the screen. `magnum factory start <key or prompt>`, `magnum factory status --json` and the other commands talk to the daemon through a requests table, as magnum's CLI does.
- The factory keeps its state in its own SQLite file under the magnum data directory, with its own migration list. A factory migration never migrates the review registry, so it never forces a drain of the review daemon.
- Agent sessions live in a separate magnum-schema store file of the factory (section 4.3).
- Factory panes run in a named herdr session, `factory`. Each named session is a separate herdr server with its own socket. So factory panes open in their own terminal window, apart from the operator's own panes. magnum's review panes stay where they are. Milestone 1 also tests whether `[herdr] socket` and `[terminal] session` can move magnum's own panes into a named session without code changes.
- The daemon reads the same usage meter as magnum. Near a soft cap, the factory pauses new stages before magnum pauses reviews. This is my call: reviews are cheaper and other people wait on them.

### 5.1 Configuration

The factory is lean. The code knows a few generic stage types. Everything specific to a repository or a team is configuration in `config.toml`, as magnum's roles and kinds are.

- The stage types in Go:
  - `agents` runs roles with a prompt and an end fact.
  - `gate` waits for the operator.
  - `command` runs a configured command.
  - `wait` waits on CI, a deploy or a person.
- Each kind of task lists its stages. Each stage names its type, its roles, its prompt file, the stage it follows, its end fact and its caps.
- Staging environments are a list. Each entry names its branch, whether it gets QA, the QA skill, and the environment that must pass first.
- Repositories, copies, setup and server commands, the task limit, the memory gate, the CI caps and the poll intervals are configuration.
- `config.defaults.toml` lists no staging environment and no repository, so it stays distributable. The operator's config holds the Talkable values.

An example from the operator's configuration file. The keys are a sketch for the builder:

```toml
[factory]
max_active = 3                     # tasks with live agents or a held copy
memory_gate = true                 # new tasks wait while magnum reads memory pressure
herdr_session = "factory"
poll = "45s"                       # CI checks and staging tips

[[factory.repo]]
repo = "talkable/talkable"
base = "master"
copies = "~/Projects/talkable.factory{n}"   # its own block, never a second [[pool]] of the repo
max_copies = 4
setup = ["bin/worktree-setup"]
server = "bin/worktree-run"
env = { WT_BRANCH = "{copy}", REDIS_DB_OFFSET = "{n*16}" }
repair_budget = 4                  # CI and QA fixes per task, over all heads

  [[factory.repo.staging]]
  name = "void"
  branch = "staging-void"
  qa = "t:qa"

  [[factory.repo.staging]]
  name = "bart"
  branch = "staging-bart"          # merges and deploys, no QA

  [[factory.repo.staging]]
  name = "bastion"
  branch = "staging-bastion"
  qa = "t:qa"
  after = "void"                   # QA on bastion starts after void passes

[factory.kinds.bug]
stages = ["start", "investigate", "questions", "coding", "tests", "ready"]
```

## 6. Tasks and stages

### 6.1 Intake

- A Jira key: the daemon reads the issue itself (magnum's `internal/jira`, extended with attachments and comments). The token never enters a pane. The ticket goes to agents as a file that they read as data.
- A free prompt: the text goes to agents as a file in the same way.
- Text after a Jira key ("PR-12345 only the refund path") is the operator's guidance and goes in with the ticket.
- The kind comes from the Jira issue type, or from the words of the prompt. The operator can change it before the start.
- The base branch is master. If the ticket or the prompt names another base, the report asks the operator.
- A preflight tests the access that later stages need: VPN, AWS MFA, the read-only production console, QA logins, the gh login. Each missing access becomes one access card at the start, not a stop in the middle.

### 6.2 Investigation

Agents run in parallel, in the task's copy. Roles are data, as magnum's `[[role]]` blocks are.

- Bug: one Claude agent reproduces the bug (`rails runner`, the copy's local server in a browser, or a failing spec). One Codex agent finds the cause on its own, without the reproducer's guess. One agent reads the history of the code that the fix will change: when and why each guard was added.
- Feature: the agents test each point of the ticket against the code: what exists, what is missing, which files change. They collect the product questions that the ticket leaves open.
- Chore: the agents find every place it touches. If a skill does the chore (for example `t:upgrade-gems`), the writer uses that skill.
- If an investigator needs production data, it writes a read-only script and asks for it on a production card (section 9). The card shows the script, what it reads and why. The operator's `y` binds to the script's hash. Then the agent runs exactly that script through `magnum factory prod-run`, and the output goes back to it. Any other production command is against its prompt rules.
- A judge merges the results into one report, `report.md`. If a question is open, the judge also writes `questions.json`.
- Each question has a context line, a concrete example, options, and a recommended option with its reason. If the evidence gives no clear answer, the question says "no recommendation".

### 6.3 The gate before code

- A bug needs a reproduction and no open question.
- A feature or a chore needs no open question.
- If a question is open, the task waits for the operator. All open questions go in one batch.
- If the bug does not reproduce, or the investigators disagree on the cause, the task waits for the operator with the report.

### 6.4 Implementation

- One writer agent works in the task's copy on a local branch, for example `zhuravel-PR-12345-short-slug` (the naming of `t:create-pr`).
- A bug: first a spec that fails, recorded with its output, then the fix. A feature: specs for the new behavior. A chore: the existing specs stay green.
- Then the specs near the change, rubocop, and a Codex review of the diff. A Codex finding goes back to the writer once.
- If the change is visible, the writer takes screenshots from the copy's local server.
- The end fact of the stage: the branch has commits, the working tree has no tracked changes, the recorded failing spec passes, and the report lists the files. A "done" message alone is not the end fact.

### 6.5 Ready to push

- The task waits for the operator on the ready card (section 9). Nothing is on GitHub yet.
- The operator's `y` binds to the head commit, the report hash and the diff hash. Before anything leaves the Mac, the factory makes sure that the branch still has that head. A change voids the `y`.
- Milestone 1 (my call): on `y`, the factory tells the writer to open a draft PR with `t:create-pr`. The operator does the same by hand today ("open PR, assign me"). The PR uses the operator's login.
- Then the factory makes sure that the pushed head is the approved head. It sends magnum a review request, because magnum does not review drafts by default (`include_drafts = false`).
- The push helper and the branch lock come in milestone 2.

### 6.6 Later stages

- PR: the factory pushes through the push helper (section 7) and opens a draft PR with the body of `t:create-pr`.
- Review: magnum reviews the PR as the operator's own PR. From milestone 2, the fix of review comments runs in the factory: the operator chose to move magnum's fix loop into the factory, with its skill, prompts, `fix-reply` and `fix-sync` helpers and tests. The board's `f` and `F` keys then queue the work in the factory, and magnum's own fix code is deleted. The factory asks magnum for each re-review through magnum's request table. Replies to people post by themselves, as the fix loop does today (the operator's decision). A comment that only the operator can answer goes to NEEDS YOU.
- Author: the factory opens its PRs under the operator's login (the operator's decision), so magnum's own-PR fix loop works unchanged and a colleague approves, as today. From milestone 2, the commits that the factory pushes are authored by the factory App (`zhuravel-factory[bot]`) and pushed with the App's token through the push helper, and the factory's replies and comments come from the App. So the history and the threads show which work an agent did, and factory pushes need no login in the pane.
- CI: section 8.
- Parent branch: the factory merges the parent at three times: on a conflict that GitHub reports, before each staging merge, and once a day. An agent resolves conflicts in the task's copy, and the targeted specs run before the push.
- Stagings and QA: section 8.
- Merge: the operator merges. If QA passed on void and bastion at the latest head, a "ready to merge" alert fires.
- Cleanup: after the merge, the factory frees the copy, removes the local branch, and keeps the logs and the report.

### 6.7 Questions from agents during work

Agents do not use interactive question dialogs. A prompt rule tells them to write `questions.json` and end the turn. The factory sees the file and the idle pane, and the task goes to NEEDS YOU. The answers go back as the next prompt.

If an agent shows a dialog anyway, the factory does not answer it. The task goes to NEEDS YOU as "an agent shows a prompt the factory does not know", and the operator answers in the pane.

### 6.8 The operator's own PRs

The operator can adopt work that started without the factory, with `magnum factory adopt <PR, Jira key or branch>` or one key on the screen. Adoption is explicit, never automatic.

- An open own PR starts at the PR stage. The factory takes over conflicts, parent merges, CI, stagings and QA, and from milestone 2 the fix of review comments.
- A Jira issue without a branch starts at intake, like a new task.
- A Jira issue whose branch lives only in one of the operator's own copies (for example `talkable.repo3`) starts at coding or ready. The factory fetches that branch into a factory copy. It never writes in the operator's copies.
- An adopted task always gets a factory copy, never magnum's review slot, because a review round resets its slot with `git reset --hard` and `git clean -fd`.
- The operator's own open PRs show on the factory screen read-only. One `y` adopts one.

## 7. Copies, servers and the boundary with magnum

### 7.1 Copies

- Each task has its own copy of the repository, for example `talkable.factory1` to `factory4`. The copies have their own configuration block (section 5.1), because magnum refuses a second `[[pool]]` for one repository. They reuse magnum's slot provisioning code and the repository's own setup script (`bin/worktree-setup`), with paths and database names apart from the review slots.
- A task holds its copy from intake until its PR opens. After that, a stage that needs a copy claims a free one: a CI fix, a parent merge, a QA fix or a staging conflict. A task that waits on a reviewer holds no copy and does not count toward the limit.
- That script already gives each copy its own databases and a fixed port derived from the copy's name. The factory also sets a Redis offset for each copy, because the Redis configuration reads `REDIS_DB_OFFSET` and the setup script does not set it.
- The server of a copy starts only for the stages that need it (a browser reproduction, screenshots, local QA) and stops after. If a feature needs background jobs, Sidekiq starts too.
- At most 3 tasks are active at once: a task with live agents or a held copy. A new task waits while the machine shows memory pressure, read by magnum's existing memory check.

### 7.2 The boundary with magnum

| Who | Owns |
|---|---|
| The factory | the work up to the PR: intake, investigation, questions, implementation, the push, the draft PR. The work after it: the fix of review comments (from milestone 2), CI fixes and reruns, parent merges, staging merges, deploy tests, QA, cleanup |
| magnum | review rounds, re-reviews on request, approvals |

- From milestone 2 the factory is the only writer of code, so no lease between the two daemons is needed. A lock inside the factory stops two of its own agents from pushing to one branch at the same time. It survives a restart, and it is not dropped while its pane still runs.
- A push helper (`magnum push`, like `magnum fix-reply` today) is the push path for factory agents from milestone 2. It tests the lock, the target branch and the expected remote head, and refuses master and main.
- The helper and a pre-push hook in each copy stop mistakes. They are not a security boundary (section 12).

### 7.3 GitHub reads: polling, no webhooks

- The factory reads PRs, reviews and comments from magnum's registry, which magnum's poll keeps current.
- A small factory poll reads CI checks and the staging tips every 30 to 60 seconds. One query per repository reads the three staging tips. Job details are read only for failed checks and cached by run id.
- Every new read stays behind magnum's budget floor, and magnum's GitHub cost meter counts it.
- magnum's registry keeps who replied and when, never the text, and its check records drop the raw conclusion and the run id. The factory adds a reader for comment bodies and check records with the run id, the attempt and the conclusion. If a read is truncated (for example more than 100 checks) or unknown, the stage cannot pass on it.
- Rejected: webhooks through a relay, as a colleague's prototype uses. That prototype has no poller of its own. magnum polls as the App identities, and in shadow mode its poll saw 480 of 493 changes that webhooks announced. CI runs take 6 to 20 minutes and a staging deploy 11 to 21, so webhooks save seconds on stages that take minutes.
- If the cost meter shows a budget problem, or a person's comment needs an answer within seconds, `magnum-relay` turns on again (`[webhooks] mode = "shadow"`). Its events then only bring the next poll forward. The poll stays the source of truth.

## 8. CI, stagings and QA

### 8.1 CI

- The factory watches CI on factory PRs and on adopted own PRs.
- For each failed check, it reads the job and sorts the cause:
  - The runner died, lost its connection, did not start, or GitHub cancelled the job: the factory runs `gh run rerun --failed` and changes no code. At most 2 reruns per head.
  - The code failed: a CI fixer agent runs `t:fix-ci` in the task's copy and pushes through the helper. At most 2 fixes per head.
- A repair budget for the whole task stops a loop in which every failed fix makes a new head with fresh caps. It counts CI and QA fixes over all heads, 4 by default. It survives pushes and restarts, and only the operator resets it.
- When a cap or the budget is used up, the task goes to NEEDS YOU with the reason.
- magnum's check reads keep only passed, failed, pending and skipped today. The factory needs the conclusion of each check, which the query already fetches.

### 8.2 Stagings

- The staging branches are configuration. For talkable they are staging-void, staging-bart and staging-bastion.
- For each staging, each task shows whether the branch holds the latest head, an older head or nothing. It also shows a merge, a conflict, CI, a deploy or QA in progress.
- The merge uses the method of `t:deploy`: `git merge-tree --write-tree`, then `commit-tree`, then a normal push of the staging branch. A rejected push after a concurrent push is tried again after a fresh fetch, at most 3 times. The factory never force-pushes and never resets a staging branch.
- A conflict: a resolver agent works in a separate worktree of the staging branch, runs the targeted specs on the combined tree and pushes through the helper. At most 2 tries, then NEEDS YOU.
- Staging merges of factory tasks go through one queue per staging. Other developers push too, so the queue coordinates only the factory, and a rejected push is tried again.
- After CI on the staging tip, the factory waits for the deploy. Then it makes sure that the server runs a commit that contains the task head. How to read the deployed commit is an open item (section 13).
- If a staging holds an older head, the factory merges and deploys the latest head without asking.
- What the start of a task allows: merges of the task's own head into the configured staging branches, and their deploys. A conflict resolution that changes other people's changes on a staging branch is pushed after the specs of the touched files pass (the operator's decision). The stage card shows what the agent kept.

### 8.3 QA

- QA uses the `t:qa` skill, first on void, then on bastion. Bastion starts only after void passes. bart gets the merges and no QA.
- `t:qa` asks the operator to approve its list of checks and the use of test data today. The factory needs a mode without those questions and a JSON verdict file: the commit, the environment, each check with PASS, FAIL or BLOCKED, the evidence paths. This is a change to the private skill, or a wrapper in the factory.
- A QA verdict belongs to the task head, the staging commit that the server ran and the version of the check list. A new task head makes every earlier verdict stale, and QA runs again after the new deploy.
- Before QA, after QA and before "ready to merge", the factory reads the commit that the server runs. If another developer's deploy changed it since QA, QA runs once more before "ready to merge".
- A failed check that the lead QA agent confirmed becomes a bug for a QA fix agent. At most 2 QA fixes per environment, then NEEDS YOU.

## 9. The screen

The lead designer wrote this section from the three designs and the three critiques.

Two answers came after the screen spec was drawn. They change two mockups, and the real-painter mockups of milestone 1 will draw them:

- Replies to people post by themselves. So the NEEDS YOU row "post the drafted reply" becomes "answer alice: a product question", and only for a comment that the fixer could not answer. The reply card stays for those.
- A production card joins the decision cards: the read-only script, what it reads and why, then `Run this read-only script on production? y/N`.

### 9.A Principles

1. One list with one line per task. It is grouped by who the task waits on, and no band repeats rows.
2. Every WAITS ON starts with who it waits on. Yellow means "on you" and nothing else.
3. Only NEEDS YOU rows have YOUR MOVE, and it is the recommended step in your own verbs.
4. On home only `enter` acts. Anything that leaves the Mac, speaks as you or starts agents asks y/N in a card.
5. Tasks and decisions are counted apart: "3 tasks need you, 5 decisions".
6. Staging truth has a source and an age. Your head, the branch, the server and the QA commit are separate facts, and `?` is never drawn as nothing.
7. Plain words, no codes. Home has no strips, no kind glyphs and no role ids, and `?` glosses every word.
8. Before you decide you see an example, a reason, the doubts and the diff, then the recommendation. Nothing is accepted in bulk.
9. The chrome is quiet. A machine fact shows only when it blocks work, and only working agents move.
10. Full transparency is one key away: task card, stage card, a live mirror of the selected pane, the real pane.

### 9.B Navigation map

```
home ─ enter on a NEEDS YOU row ─▶ its decision card: questions · ready to push · reply · access · prompt
     │                               └─ the decision (y/N if it leaves the Mac) ─▶ home, next row
     ├ enter on any other row ─────▶ task card ─ enter on a stage ─▶ stage card (live mirror)
     │                                          ─ enter on REPORT ─▶ the report in the pager
     ├ a ─▶ the oldest decision card ─ decided ─▶ the next one ─ … ─▶ "Nothing waits on you"
     ├ n ─▶ new task ─ enter ─ y/N ─▶ home, cursor on the new row (N: back to the text)
     ├ tab ─▶ agents view ─ enter ─▶ that agent's stage card ─ esc ─▶ agents ─ tab/esc ─▶ home
     ├ L ─▶ learning ─ enter ─▶ approve / edit / reject (y/N)
     ├ o ─▶ the real herdr pane (no ask) ─ switch back to this terminal ─▶ same view, same row
     └ ! action log · ? help and words · , settings · / filter · h snoozed rows · q quit
esc goes back one level, keeping the row and the scroll. d diff and l log open the pager (tea.ExecProcess).
```

### 9.C Mockups

#### Home at 140 columns

```
 magnum factory · talkable                                                                            memory full: new tasks wait · ↻ 14:09 
 ● 3 tasks need you, 5 decisions · 3 wait on others · 1 done today                                                    1 rule to approve (L) 

 NEEDS YOU                          WAITS ON                                  YOUR MOVE                 VOID      BART      BASTION   WAITED
› ● #4512     Deploy list on camp…  you: reply to alice · agent: fixing bart  post the drafted reply    QA ✓      conflict  QA 4/9        2h
  ● PR-12345  Credit expiry remin…  you: 3 questions before coding            answer 3 questions                                         41m
  ● PR-12360  Custom activities l…  you: ready to push · 3 files +48 −6       read the diff, then push                                   12m

 WAITS ON OTHERS
  ⣷ PR-12371  Coupon CSV export t…  agent: finding the cause, 2 of 3 done                                                                 9m
  ◷ PR-12330  Advocate offer emai…  rev-ann: review of #4527                                            QA ✓      latest    outdated      3h
  ◷ PR-12388  Weekly gem upgrades   memory: full, starts below 80% swap                                                                  18m

 DONE TODAY
  ✓ PR-12302  Remove old sync job   nothing: merged 13:20 · not rated yet                               QA ✓      latest    QA ✓
── ✓ 14:02 you said MFA done → bart deploy resumed ─────────────────────────────────────────────────────────────────────────────────────────
 alice on #4512: "Do we need the deploy list for archived campaigns too?" · reply drafted · also: agent fixing 2 conflicts in bart, 10m
 enter read and reply · a oldest ask · o pane fixing bart · n new task · ? more
```

- **Colors:**
  - "magnum factory" uses Title. "memory full: new tasks wait" is plain, because it is not on you.
  - In the chips, `●` and "3 tasks need you" are Warn and the rest is Dim. Group names use Section and column names use Header.
  - Warn appears only for `●`, `you:` (bold), WAITED past 30 minutes and the rule chip.
  - Keys use Accent. `agent:`, logins and `magnum:` use Accent. `memory:` and `usage:` are plain. `nothing:` and DONE rows are Dim.
  - In the staging cells, `QA ✓` and `latest` are OK, `QA ✗` is Err, and `–` is Dim.
  - The cursor row is Selected plus `›`. The flash in the status rule is OK.
- **Motion:** one braille spinner, only on rows whose blocker is a working agent (PR-12371). #4512 needs you, so it stays still even though an agent works on it. Nothing in NEEDS YOU ever moves.
- **Behaviour:** #4512 is your own PR, written by hand, so its key is its GitHub number. It shows both of its blockers on one line.

#### Home at 100 columns (same data)

```
 magnum factory · talkable                                    memory full: new tasks wait · ↻ 14:09 
 ● 3 tasks need you, 5 decisions · 3 wait on others · 1 done                  1 rule to approve (L) 

 NEEDS YOU                   WAITS ON                  YOUR MOVE        VOID   BART   BASTION WAITED
› ● #4512    Deploy list o…  you: reply · agent: bart  post reply       QA ✓   confl  QA 4/9      2h
  ● PR-12345 Credit expiry…  you: 3 questions          answer them                               41m
  ● PR-12360 Custom activi…  you: ready to push        diff, then push                           12m

 WAITS ON OTHERS
  ⣷ PR-12371 Coupon CSV ex…  agent: finding the cause                                             9m
  ◷ PR-12330 Advocate offe…  rev-ann: review of #4527                   QA ✓   latest old         3h
  ◷ PR-12388 Weekly gem up…  memory: full                                                        18m

 DONE TODAY
  ✓ PR-12302 Remove old sy…  nothing: merged 13:20                      QA ✓   latest QA ✓
── ✓ 14:02 you said MFA done → bart deploy resumed ─────────────────────────────────────────────────
 alice: "Do we need the deploy list for archived campaigns too?" · also: agent fixing bart
 enter read and reply · a oldest ask · o pane fixing bart · n new task · ? more
```


- **What shrinks, in this order:**
  1. The title, to 14 cells.
  2. YOUR MOVE, to short forms.
  3. WAITS ON, to short forms. Both blockers stay.
  4. The staging cells, to short forms.
  5. The gap after the key, to 1 cell.
- Nothing wraps. The help line keeps the full ask. Colors and motion are as at 140.

**Staging cells (VOID, BART, BASTION).** A row shows them once its task has a PR. The column headers appear once any row has a PR.

| State | 140 | 100 | ASCII 140 | ASCII 100 |
|---|---|---|---|---|
| nothing of this task there | `–` | `–` | `-` | `-` |
| an older head is there (replaced automatically) | `outdated` | `old` | `outdated` | `old` |
| merging the latest head | `merging` | `merge` | `merging` | `merge` |
| merge conflict, an agent fixes it | `conflict` | `confl` | `conflict` | `confl` |
| CI on that staging branch | `CI 12/41` | `CI 29%` | `CI 12/41` | `CI 29%` |
| merged, server not on it yet | `deploying` | `deploy` | `deploying` | `deploy` |
| t:qa running | `QA 4/9` | `QA 4/9` | `QA 4/9` | `QA 4/9` |
| QA passed / failed, latest head only | `QA ✓` / `QA ✗` | `QA ✓` / `QA ✗` | `QA pass` / `QA fail` | `QA +` / `QA x` |
| latest head deployed (bart; or QA not started) | `latest` | `latest` | `latest` | `latest` |
| could not read it | `?` | `?` | `?` | `?` |

- At 140, if a count does not fit in 9 cells (`CI 128/412`), the cell uses the percent form.
- A QA result for an older head never shows in a cell. The cell says `outdated` until the new head is in, and the task card keeps the old verdict with its SHA.
- `conflict` turns Warn only when it is out of tries, because then it is on you.
- Nerd mode keeps these words and ✓ ✗. The row marks take the board's nerd glyphs, each followed by a space.
- The ASCII row marks are `*` needs you, `| / - \` working, `o` waits, `+` done, and `>` for the cursor.

#### Home when nothing needs you (100)

```
 magnum factory · talkable                                    memory full: new tasks wait · ↻ 15:20 
 ✓ nothing waits on you · 6 wait on others · 1 done today

 NEEDS YOU
   ✓ Nothing waits on you.
     Next for you: PR-12371's questions or its diff, once its plan is written.

 WAITS ON OTHERS             WAITS ON                  YOUR MOVE        VOID   BART   BASTION WAITED
  ⣷ PR-12345 Credit expiry…  agent: writing the spec                                             12m
› ⣷ PR-12371 Coupon CSV ex…  agent: writing the plan                                             21m
  ⣷ #4512    Deploy list o…  agents: fixing bart, QA                    QA ✓   confl  QA 6/9     14m
  ◷ PR-12360 Custom activi…  magnum: review of #4540                    –      –      –          40m
  ◷ PR-12330 Advocate offe…  rev-ann: review of #4527                   QA ✓   latest deploy      4h
  ◷ PR-12388 Weekly gem up…  memory: full                                                         1h

 DONE TODAY
  ✓ PR-12302 Remove old sy…  nothing: merged 13:20                      QA ✓   latest QA ✓
── ✓ 15:18 sent 3 answers to PR-12345 → coding started ─────────────────────────────────────────────
 writing the plan: 3 agents agree on the cause; a judge writes the plan, then coding starts
 enter task card · o pane writing the plan · tab agents · n new task · ? more
```

- "Nothing waits on you." is OK and bold. "Next for you" is Dim and never guesses a time.
- The column names move to the first group that has rows.
- The window title reads `factory · nothing waits on you`.
- When `h` hides snoozed rows, the chip line says how many are hidden and why, for example "1 snoozed till 16:10 (h)".

#### Home at 200 columns

```
 magnum factory · talkable                                                                                                                             memory full: new tasks wait (swap 91%) · ↻ 14:09 
 ● 3 tasks need you, 5 decisions · 3 wait on others · 1 done today                                                                                                                1 rule to approve (L) 

 NEEDS YOU                                                    WAITS ON                                                    YOUR MOVE                     VOID          BART          BASTION       WAITED
› ● PR-12310  #4512   Deploy list on campaigns                you: reply to alice · agent: fixing 2 conflicts in bart     post the drafted reply        QA ✓ 1h       conflict 11m  QA 4/9 12m        2h
  ● PR-12345          Credit expiry reminder email            you: 3 questions before coding · first: when is it sent?    answer 3 questions                                                         41m
  ● PR-12360          Custom activities lose points on copy   you: ready to push · 3 files +48 −6, 1 not in the plan      read the diff, then push                                                   12m

 WAITS ON OTHERS
  ⣷ PR-12371          Coupon CSV export times out             agent: finding the cause · reproduced, history read                                                                                     9m
  ◷ PR-12330  #4527   Advocate offer email locale             rev-ann: review of #4527, requested 3h ago                                                QA ✓ 1h       latest 2h     outdated 2m       3h
  ◷ PR-12388          Weekly gem upgrades                     memory: full (swap 91%), starts when swap is under 80%                                                                                 18m

 DONE TODAY
  ✓ PR-12302  #4519   Remove old sync job                     nothing: merged 13:20 · not rated yet                                                     QA ✓ 4h       latest 4h     QA ✓ 3h
── ✓ 14:02 you said MFA done → bart deploy resumed ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 alice on #4512, 2h ago: "Do we need the deploy list for archived campaigns too, or only for live ones?" · reply drafted · also: agent fixing 2 conflicts in bart, try 1 of 2, 10m
 enter read and reply · a oldest ask · o pane fixing bart · n new task · ? more
```


- **What the extra width shows:**
  - KEY splits into JIRA and GITHUB, so you can match a row with the board in the next terminal.
  - Full titles.
  - The long form of both blockers.
  - The age of each staging cell.
  - The full quote in the help line.
- It adds no new facts: no copies, branch, usage or spark. It is still one line per task.

#### New task: from a Jira key (100)

```
 magnum factory · new task                                                              esc cancels 

 Jira key or describe the task                    enter starts · ctrl+j new line · ctrl+g editor 
 ┃ PR-12345 the reminder is for advocates only, not their friends▏

 JIRA      PR-12345 · Story · To Do · High · reporter rev-ann · read by the daemon 2s ago
           Credit expiry reminder email
           "Send advocates a reminder before their referral credit expires."   pgdn reads more
 GUIDANCE  "the reminder is for advocates only, not their friends" goes to every agent
 KIND      feature     from the Jira type Story · tab: bug, feature, chore
 BRANCH    zhuravel-PR-12345-credit-expiry-reminder
 STARTS    now, in talkable.factory2 (free)
 AGENTS    2: test each point of the ticket against the code · collect the open questions
 RULES     3 talkable rules go into its prompts                                    L shows them
           ask before changing an email's words · seed data into the copy's own databases ·
           screenshots come from the copy's own server
────────────────────────────────────────────────────────────────────────────────────────────────────
 Start feature PR-12345 with 2 agents in talkable.factory2? y/N  y confirms, any other key cancels
```

#### New task: from a free prompt (100)

```
 magnum factory · new task                                                              esc cancels 

 Jira key or describe the task                    enter starts · ctrl+j new line · ctrl+g editor 
 ┃ The weekly digest email shows last week's numbers for merchants east of UTC+10.
 ┃ Use the merchant's time zone for the week window. Check a merchant in UTC-11 too.▏

 JIRA      none: the agents work from your words
 KIND      bug         guessed from "shows last week's numbers" · tab: bug, feature, chore
 TITLE     Weekly digest uses the wrong week east of UTC+10
 BRANCH    zhuravel-weekly-digest-time-zone
 STARTS    queued: memory full (swap 91%) · starts when swap is under 80%
 AGENTS    3: reproduce it · find the cause on its own · read the history of the code
 RULES     3 talkable rules go into its prompts                                    L shows them
────────────────────────────────────────────────────────────────────────────────────────────────────
 Queue bug "Weekly digest uses the wrong week…"? It starts when memory allows. y/N
```

- **The input:**
  - It is one bubbles `textarea`, the first one in magnum. `enter` submits, `ctrl+j` inserts a new line, and `ctrl+g` opens `$EDITOR` through `tea.ExecProcess`.
  - A Jira key at the start gets a preview, read by the daemon. Any words after the key are guidance for every agent.
- **Colors:** labels use Header, values are plain, reasons are Dim. An Accent spinner shows while Jira is read.
- **A key Jira does not know:** the screen shows "PR-12399 is not in Jira" in Err before anything is asked. Then `enter` asks "Start from your words only? y/N".
- **The y/N:** `y` is needed because the task starts agents. `N` returns to the text with nothing lost. `esc` keeps a draft.

#### Question card, question 2 of 3 (100)

```
 magnum factory · PR-12345 · question 2 of 3                                               esc back 
 Credit expiry reminder email · feature · asked 41m ago · coding starts after all 3

 ✓ 1  Who gets the reminder?             advocates only                        your answer 13:31
 ● 2  When does the reminder go out?                                           now
   3  Can a merchant turn it off?

 When does the reminder go out?
 The ticket says "before the credit expires" and names no day.
 Example: a credit that expires on Fri 14 Nov.

 › 1  Seven days before it expires             → the email goes out Fri 7 Nov       recommended
      why: the coupon reminder already uses 7 days, so the two emails match
   2  One day before it expires                → Thu 13 Nov
   3  On the day it expires                    → Fri 14 Nov, 09:00 merchant time
   4  Ask rev-ann in Jira (the task waits)     → drafts a comment for you to check
   5  Type your own answer
────────────────────────────────────────────────────────────────────────────────────────────────────
 enter take the marked one · j k move · 1-5 pick · backspace previous · z snooze 2h · esc back
```

When the evidence gives no clear answer, nothing is marked:

```
 Can a merchant turn it off?
 The ticket does not say, and no other campaign email has an off switch.
 Example: a merchant opens Settings, then Emails, and looks for the reminder.

   1  Yes: a switch in the email settings, on by default   → the merchant sees a switch
   2  No: always on                                        → no switch anywhere
   3  Ask rev-ann in Jira (the task waits)                 → drafts a comment for you to check
   4  Type your own answer
 no recommendation: the ticket and the code give no hint · nothing is marked: pick one (j k)
```

- **The order on the card:** the question, one context line, one example, then the options. Each option shows what it means for that example. The recommended option comes first, with its reason directly under it.
- **Colors:** the current question's `●` is Warn, `✓` is OK, the question is bold, context and example are Dim, "recommended" is OK and "why:" is Dim. Nothing moves.
- **Answering:**
  - `enter` or a digit takes an answer and moves on. Neither asks y/N, because nothing is sent until the batch is sent.
  - With no recommendation, `enter` is refused until you pick.
  - Answers are kept on `esc`, and the home row then reads "1 of 3 answered".

#### Recap, one `y` for the batch (100)

```
 magnum factory · PR-12345 · your answers                                                  esc back 
 Credit expiry reminder email · feature · nothing is sent yet

 1  Who gets the reminder?
    advocates only                                                                  your answer
 2  When does the reminder go out?
    seven days before it expires (expires Fri 14 Nov → email Fri 7 Nov)            recommended
 3  Can a merchant turn it off?
    ask rev-ann in Jira                                             there was no recommendation
    ┃ rev-ann, should merchants be able to turn off the credit expiry reminder,
    ┃ or is it always on? The ticket does not say.

 When you send: the agents get answers 1 and 2, the comment goes on PR-12345 as zhuravel,
 and the task waits for rev-ann. These answers are for the questions as of 14:12.
────────────────────────────────────────────────────────────────────────────────────────────────────
 Send 2 answers and post 1 Jira comment as zhuravel? y/N  y confirms, any other key cancels
```

If the evidence changes before you send:

```
── the questions changed at 14:20 · nothing was sent ───────────────────────────────────────────────
 The agent finding the cause saw the coupon reminder uses 3 days, not 7. Question 2 is open again.
 enter see question 2 again · esc back
```

- Any key other than `y` cancels and leaves the recap open. There, `1-3` changes an answer and `e` edits the drafted Jira comment.
- Answers are bound to the question's revision. Changed evidence voids the pending batch for the changed questions.
- There is no "accept all recommended" key.
- In milestone 1 the factory does not write to Jira. The recap then says "copy 1 Jira comment for you to post", and the task waits until you mark the question answered.

#### Ready card, before the diff was opened (100)

```
 magnum factory · PR-12360 · ready to push                                                 esc back 
 Custom activities lose points on copy · bug · waited 12m
 zhuravel-PR-12360-activity-points-on-copy · 1 commit · not on GitHub yet

 CAUSE        copying a campaign copies its activities before their reward rules
 FIX          copy the reward rules first, then point each activity at its copy
 PROOF        spec "copies reward rules with the activities": red before (0 points, want 25),
              green after · 214 specs near the change green · rubocop clean
 FILES        app/services/campaign_copier.rb          +31 −6    in the plan
              spec/services/campaign_copier_spec.rb    +15       in the plan
              spec/factories/reward_rules.rb           +2        not in the plan
 DOUBTS       the writer was unsure if archived campaigns copy too: not tested
              Codex noted one query per rule; fixed in the same commit
 SCREENSHOTS  2, from the copy's own server: the copied campaign shows 25 points

   push + draft PR
   send back with a note
   drop: free the copy, keep the branch
 You have not read the diff yet. The recommendation shows after d.
────────────────────────────────────────────────────────────────────────────────────────────────────
 d read the diff · e screenshots · j k pick · enter do it · esc back
```

#### Ready card, after the diff was opened (lower half; the top is unchanged)

```
 DOUBTS       the writer was unsure if archived campaigns copy too: not tested
              Codex noted one query per rule; fixed in the same commit
 SCREENSHOTS  2, from the copy's own server: the copied campaign shows 25 points

 › push + draft PR                                                                  recommended
   why: the spec proves the fix; the file outside the plan is test data for it
   send back with a note
   drop: free the copy, keep the branch
── you read the diff 14:21 ─────────────────────────────────────────────────────────────────────────
 Push zhuravel-PR-12360-activity-points-on-copy and open a draft PR? y/N
```

- **Colors:** labels use Header and "not in the plan" is Warn, because checking that file is on you. A failed check turns its line `✗` Err, and the recommendation then moves to "send back".
- **Before the diff:** nothing is marked. Choosing push asks "Push without reading the diff? y/N" instead of the normal question.
- **The diff:** `d` runs the pager in the task's copy through `tea.ExecProcess` and returns to the card. `e` opens the screenshots.
- **Asks y/N:** push + draft PR. "Send back" asks after you type a one-line note. "Drop" asks "Drop PR-12360: free talkable.factory3, keep the branch?".
- **Milestone 1:** I keep push as your `y` on this card. The factory pushes and opens the draft PR.

#### Reply card (100)

```
 magnum factory · #4512 · reply to alice                                                   esc back 
 Deploy list on campaigns · your PR, written by hand · also: agent fixing bart, 10m

 alice, 2h ago, on the deploy list view, line 14:
   "Do we need the deploy list for archived campaigns too, or only for live ones?"

 Drafted reply, as you:
   "Only live ones: archived campaigns can't be deployed, so the list would always be
    empty. The campaign deploys spec covers it."
 Checked: the view leaves archived campaigns out (line 9), and the spec has that case.

 › post the drafted reply                                                           recommended
   why: the code already does what the reply says
   edit it, then post
   I'll answer on GitHub myself
────────────────────────────────────────────────────────────────────────────────────────────────────
 Post this reply on #4512 as zhuravel? y/N  y confirms, any other key cancels
```

- The y/N names the account the reply speaks as.
- "Edit it, then post" opens the draft in the textarea (`ctrl+g` opens the editor), then asks the same y/N.
- "I'll answer on GitHub myself" asks nothing. The row stays as "you: answer alice on GitHub", and it leaves NEEDS YOU when your reply appears.
- The second blocker stays in the header line.

#### Access card (100)

```
 magnum factory · PR-12371 · access needed                                       8m left · esc back 
 Coupon CSV export times out · the agent finding the cause needs the bastion logs

 The AWS MFA session ran out. In any terminal, run:
   aws-mfa                    renews the AWS session for 12 hours
 Then press enter here. The factory checks the session and the agent goes on.

 If the 8 minutes run out, the agent goes on without the logs and says so in its report.
 asked 14:05 · alerted 14:05 and 14:10 · one more alert at 2m left

 › done, I logged in                                                 checks it, asks nothing
   not now: snooze 2h (the agent goes on without the logs)
────────────────────────────────────────────────────────────────────────────────────────────────────
 enter do it · j k pick · o the pane that asked · esc back
```

- "8m left" is Warn, and Err from 2 minutes.
- "Done" asks no y/N, because it only starts a check. A failed check says so: "no AWS session yet, checked 14:12".

#### Task card at 140: a task at the staging stage

```
 magnum factory · PR-12330 · bug · Advocate offer email locale                                                                    ↻ 14:09 
 zhuravel-PR-12330-offer-email-locale · #4527 · copy 2 of 3, server off · started 08:40 · your time on it 6m

 WAITING FOR  rev-ann: review of #4527, requested 3h ago · meanwhile the factory moves bastion to your latest head (no agent)

 STAGES                                                                                                                    took   finished
  ✓ start          Jira PR-12330 read · bug, from the Jira type                                                              0s      08:40
  ✓ investigate    reproduced with a failing spec · 2 agents found the same cause                                           24m      09:04
  – questions      skipped: nothing was open
  ✓ coding         spec red before, green after · 3 files +22 −4                                                            31m      09:35
  ✓ tests          216 specs green · rubocop clean · Codex review: 1 note, fixed                                             9m      09:44
  ✓ ready to push  you read the diff 09:52 and pushed                                                                       14m      09:58
  ✓ pushed         draft #4527 · magnum approved round 1 · you marked it ready for review                                 1h11m      11:09
  ◷ review         rev-ann asked 11:09, nothing yet · 0 comments open                                                        3h    waiting
  ✓ CI             41 of 41 green at 9c4e2d1 · 3 tries, see CI                                                            1h26m      11:31
  ✓ merge master   not needed: 0 commits behind master                                                                       0s      11:32
  ✓ void           your latest head deployed · QA ✓ 9 of 9                                                                  52m      12:24
  ✓ bart           your latest head deployed · no QA on bart                                                                23m      11:55
› ⣷ bastion        merging your latest head (bastion has an older one)                                                       2m        now
  · merge          yours: the factory never merges to master
  · cleanup        after your merge: free the copy

 REVIEW COMMENTS  0 open · 3 answered                                                                             GitHub, read 14:08
  ✓ magnum   3h  P2 the email falls back to the site's locale, not the advocate's        fixed in 9c4e2d1 · magnum agreed
  ✓ magnum   3h  P3 the spec says "language" where it means locale                       fixed in 9c4e2d1
  ✓ alice    2h  "Does this cover the SMS text too?"                                     you posted the drafted reply 12:03
  ◷ rev-ann  3h  review requested, nothing yet

 CONFLICTS  none open · 1 earlier
  ✓ bart 11:41  1 file, the offer mailer · the other change: #4519 by alice · agent fixing bart, try 1 of 2 · kept both

 CI  ✓ 41 of 41 at 9c4e2d1                                                                                         GitHub, read 14:08
  try 1  10:05  ✗ code failed: locale_spec got "en", want "de"   → fixed with t:fix-ci, pushed 9c4e2d1 · fix tries 1 of 3
  try 2  10:52  ✗ runner died: rspec 7 lost its runner, no step ran → reran the failed jobs, no code change · reruns 1 of 2
  try 3  11:31  ✓ 41 of 41

 STAGINGS  your head 9c4e2d1
           HAS YOUR HEAD             BRANCH     SERVER     QA                                                    SEEN
  void     yes                       a71f0c3    a71f0c3    ✓ 9 of 9 at a71f0c3, which has your head · 12:24      1m ago
  bart     yes                       e03b9d2    e03b9d2    no QA on bart                                         1m ago
  bastion  no: has 5b1a0f3, older    44c2e81    44c2e81    ✓ at 44c2e81, an older head · runs again              1m ago
           now: merging 9c4e2d1 into staging-bastion, then CI and the deploy · bastion QA waits for void QA: passed

 AGENTS  none working: the factory merges bastion itself · 5 done, logs kept
  ✓ QA on void          t:qa on void: 9 of 9 checks, 4 screenshots          done 12:24 · last output 1h45m ago
  ✓ fixing bart         kept both changes in the offer mailer                done 11:48
  ✓ fixing CI           t:fix-ci: the locale fallback in the mailer          done 10:40
  ✓ writer              wrote the failing spec, then the fix                 done 09:44
  ✓ 3 investigators     reproduced it, found the cause, read the history     done 09:04

 REPORT
  cause   the offer email takes the site's locale, not the advocate's
  proof   spec "sends the offer email in the advocate's locale": red before, green after
  plan    3 steps, all done · no migration · visible: the email text, 2 screenshots

 LEARNED  1 from this task: magnum's P2 "falls back to the site's locale" → a test case in the replay set (L)
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 j k move · enter open the line · d diff · l log · + − rate · esc back · ? more
```

- **Layout:**
  - The title, the facts line and WAITING FOR stay pinned while the rest scrolls, as the board's card does.
  - The cursor moves over stages and section lines. `enter` opens a stage card, the report in the pager, or the reply card of an open comment.
- **Colors:**
  - Marks are as on home. "code failed" is Err and "runner died" is plain, so the two CI causes differ in color and in words.
  - SHAs and "GitHub, read 14:08" are Dim, and every GitHub count names its source.
- **Motion:** the only motion is the `⣷` of a stage with live work.
- **An adopted PR:** its local stages read `– by hand` and the header says "your PR, written by hand".

#### Stage card at 140: the bart conflict, with the mirror

```
 magnum factory · #4512 · bart · Deploy list on campaigns · your PR, written by hand                                              ↻ 14:09 
 merge your head 7e3a9f0 into staging-bart · agent fixing bart (claude) · working 10m · output 3s ago

 WAITING FOR  agent: fixing bart, 1 of 2 files left · then rubocop and the 2 files' specs · then the push to staging-bart

 OTHER CHANGE  #4533 by rev-ann went into staging-bart at 13:40 and changed the same 2 files
 TRIES         1 of 2 · a second failed try stops and asks you
 FILES
  ⣷ app/models/campaign.rb                  both changed deployable?: yours skips archived, #4533 adds a region check     fixing
  ✓ app/views/campaigns/_deploys.html.erb   both added a column to the table: kept both, yours first                        done 14:06

 PANE  agent fixing bart · a live copy, read-only, every 2s                                                             following
  │ ● Both sides change deployable?. Yours skips archived campaigns; #4533 adds the region check.
  │   Keeping both: archived first, then region, as the spec of #4533 expects.
  │ ● Update(app/models/campaign.rb)
  │   ⎿  Updated with 6 additions and 9 removals
  │ ● Bash(bin/rspec spec/models/campaign_spec.rb)
  │   ⎿  38 examples, 0 failures
  │ ⣷ Running rubocop on the 2 files… (14s)

 LOG  this stage, newest last                                                                                          l full log
  13:58  begin  merge 7e3a9f0 into staging-bart (staging-bart at 41c0d2e)
  13:58  fail   conflict in 2 files · the other change: #4533 by rev-ann
  13:59  begin  agent fixing bart: try 1 of 2
  14:06  ok     _deploys.html.erb: both columns kept
  14:08  ok     campaign_spec: 38 examples, 0 failures
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 o step into the pane · c say one line · l full log · K stop this agent · esc back · ? more
```

Saying one line, the refusals, and stepping in (these replace the status rule and footer):

```
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 Say to the agent fixing bart (sent as your message, logged word for word · enter sends · esc drops it)
 ┃ keep the region check first: the spec of #4533 depends on it▏

── ✗ not sent: the agent shows a permission dialog · o step in to see it ───────────────────────────────────────────────────────────────────
── ✗ not sent: you are in that pane since 14:11 · say it there ─────────────────────────────────────────────────────────────────────────────
── you're in this pane since 14:11 · the factory sends it nothing and raises no alert for it ───────────────────────────────────────────────
── back? you typed 1 line in that pane at 14:12 · kept for learning (L) ────────────────────────────────────────────────────────────────────
```

- **The mirror:**
  - It is magnum's watch: one `pane.read` every 2 seconds, only for this pane, with escapes stripped by `paneText`.
  - Scrolling up pauses following, and the PANE line then says "paused · G follows · 3 new". The frame cache keys on the mirror revision.
  - The agent's own output passes through as plain text.
  - Log words use the event colors: `begin` Dim, `ok` OK, `fail` Err.
- **Saying one line:** it uses the `agents.Tell` guards, and the exact text goes into the event log.
- **Coming back from the real pane:** this is only a hint, taken from a focus-in or the first key after `o`, hence "back?".

#### Agents view at 140 (`tab`)

```
 magnum factory · agents · herdr session factory · 6 agents                                                                       ↻ 14:09 
 ⣷ 3 working · ● 1 waits on you · ✓ 2 done, idle · no agents now: PR-12360, PR-12330, PR-12388

    TASK      AGENT                  WHY IT EXISTS                                       STATE                   FOR   OUTPUT
 ›  #4512     fixing bart            merge your head into staging-bart: 2 conflicts      ⣷ working               10m   3s ago
    #4512     QA on bastion          t:qa on bastion, after void passed                  ⣷ working, 4 of 9       12m   20s ago
    PR-12371  finding the cause      find the cause on its own, apart from the others    ⣷ working               21m   no output 9m
    PR-12371  reproducing it         show the bug with a failing spec                    ✓ done, idle             3m   3m ago
    PR-12371  reading the history    how the export code changed, and why                ✓ done, idle             5m   5m ago
    PR-12345  product questions      asked you 3 questions; waits for your answers       ● waits on you          41m   41m ago

────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 fixing bart · last line: "Running rubocop on the 2 files… (14s)" · started 13:59 by the bart merge of #4512
 enter stage card · o step into the pane · c say one line · K stop this agent · esc back
```

- "no output 9m" is plain bold, not Warn, because it is not on you.
- At 10 minutes with a question-like last line or a dialog the factory does not know, the task moves into NEEDS YOU as "agent shows a prompt the factory does not know".
- The states are: working, done and idle, waits on you, you're in it, parked, no output N.
- The view mirrors nothing. The help line shows the last line of the selected pane, taken from the daemon's samples.

#### Learning view at 140 (`L`)

```
 magnum factory · learning · talkable                                                                                             ↻ 14:09 
 ● 1 rule to approve · 6 lessons this week · 9 rules in use · 1 task to rate                                 retro ran 03:00, next 03:00

 TO APPROVE
 › rule for talkable · goes into the writer's and the cause-finder's prompts                               proposed by the retro 03:00
     source   you typed in the writer pane of PR-12360 at 13:02: "the bug is in the copier, not the serializer"
     prompt   + When a copy loses data, check what is copied before what: a copy may point at rows not copied yet.
     replays  3 earlier copy bugs · with the rule: 3 of 3 find the cause · without it: 1 of 3 · none got worse
     enter approve, edit or reject (asks y/N)

 LESSONS, LAST 7 DAYS                                SOURCE                                 BECAME                NOW
  PR-12360  the cause was in the copier              you, in the writer pane                rule                  to approve
  PR-12296  reuse the referral export's streaming    your − on the finished task            rule                  in use since 11-01
  PR-12330  locale comes from the advocate           magnum's P2 on #4527                   test case             in the replay set
  PR-12318  run the failing spec before the fix      Codex: no failing spec came first      skill edit: writer    in use since 10-30
  PR-12311  ask before changing an email's words     your answer to a question              rule                  in use since 10-28
  PR-12305  a red CI run was a dead runner           CI: a rerun passed, no code change     test case             in the replay set

 RULES IN USE  9, most used first                    USED IN      REPEATS SEEN AFTER THE RULE      BEFORE THE RULE
  Run the failing spec before writing the fix        12 tasks     0                                3 in 9 tasks
  Seed data goes into the copy's own databases        9 tasks     1 (PR-12318)                     4 in 8 tasks
  Ask before changing the words of an email           6 tasks     0                                2 in 5 tasks
  … 6 more                                                                                         enter lists them

 RATE FINISHED TASKS  1 without your word
  PR-12302  Remove old sync job                      merged 13:20      + good · − not right, then one key for why
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 a repeat: the same mistake seen again in a later task. It does not prove the rule worked.
 enter open · + − rate · j k move · esc back · ? more
```

The approve card, and the one-key reason after `−`:

```
 › approve: add it to the prompts of later talkable tasks
   edit the words, then approve
   reject: keep it out, and say why in one line
 Add this rule to the writer's and the cause-finder's prompts of later talkable tasks? y/N  y confirms, any other key cancels

 Why not right?  1 wrong cause or fix · 2 changed too much · 3 missed what I told it · 4 too slow · 5 type it · esc cancels
```

- **Colors:** "1 rule to approve" is Warn. In replays, "with the rule" is OK, "without it" is Dim and a worse replay is Err. Repeat counts are plain.
- **Source text:**
  - It is your own words, shown here before any prompt can use them.
  - It is stored only in the factory's registry.
  - Lines typed near an access hand-off are never captured.
- **On home:** learning appears only as the chip "1 rule to approve (L)".

### 9.D Key maps

**When y/N is asked.** It is asked when the effect:
- leaves the Mac: push, post, a Jira comment
- speaks as you to a person
- starts or stops agents
- frees a copy
- changes the prompts of later tasks

It is not asked:
- when the key is the decision itself and nothing is sent yet: an answer in a card, where the batch gets one `y`
- for typed text sent with `enter`
- for ratings and snooze, which are undoable
- for access "done", which only starts a check
- for opening, stepping in or watching

`y` only confirms. Like the board, the factory uses `confirms()`, which also accepts `Y` (`confirm.go:27`).

| View | Key | Does | y/N |
|---|---|---|---|
| Home | `j k ↑ ↓ g G` | move | – |
| | `enter` | the decision card (NEEDS YOU) or the task card | – |
| | `a` | the oldest decision card, then the next | – |
| | `o` | step into the pane the row waits on | no |
| | `n` | new task | in the form |
| | `tab` / `L` | agents view / learning | – |
| | `h` | show or hide snoozed rows | – |
| | `/` | filter (`you`, `agent`, `bug`, words) | – |
| | `b` / `t` | the GitHub PR / the Jira issue in the browser | – |
| | `! , ? q m ctrl+r` | log, settings, help, quit, mouse, refresh | – |
| Any decision card | `j k` / digits / `enter` | move the mark / pick / do the marked option | per option |
| | `z` | snooze 2h, or until it changes | no |
| | `o` | the pane that asked | no |
| | `esc` | back, answers kept | – |
| Questions | `enter`, `1-9` | answer and move on | no |
| | `backspace` | previous question | – |
| | recap `y` | send the batch | yes, once |
| | recap `e` | edit the drafted Jira comment | – |
| Ready to push | `d` / `e` | diff in the pager / screenshots | – |
| | push + draft PR | push the branch, open a draft PR | yes ("without reading the diff?" if `d` was not opened) |
| | send back | send a one-line note to the writer | yes |
| | drop | free the copy, keep the branch | yes |
| Reply | post / edit, then post | post the reply as you | yes |
| | answer myself | you answer on GitHub | no |
| Access | done / snooze | check the access / snooze it | no |
| Task card | `j k` `enter` | move, open the line | – |
| | `d` `l` `o` `b` `t` | diff, log, step in, PR, Jira | – |
| | `+ −` | rate the task | no |
| | `K` | stop the task's agents | yes |
| | `x` | drop: free the copy, keep the branch | yes |
| | `T` | try a stopped stage again | yes |
| Stage card | `c` | say one line (typed, `enter` sends, guarded) | no |
| | `o` `l` `G` | step in, full log, follow | – |
| | `K` | stop this agent | yes |
| | `T` | try again | yes |
| Agents view | `j k` `enter` `o` `c` | move, stage card, step in, say one line | – |
| | `K` | stop this agent | yes |
| | `tab` / `esc` | back to home | – |
| Learning | `enter` | approve / edit / reject | yes |
| | `+ −` then `1-5` | rate, then one key for why | no |
| New task | `enter` | start | yes |
| | `ctrl+j` / `ctrl+g` / `tab` / `pgdn` / `esc` | new line, editor, kind, preview, cancel (keeps a draft) | – |

**Mouse:**
- The wheel scrolls, a click selects, and a double click is `enter`.
- A right click opens a menu built from the same per-state action table, dimmed by its refusals. Picking an item runs its key's path, so the same y/N appears.

**Board-meaning check**

| Key | Board | Factory | Same? |
|---|---|---|---|
| `j k g G` space pgup pgdn | move | move | yes |
| `enter` | open the card (in the card: back) | open the card, or the line under the cursor in a card | the open meaning is kept; `esc` is back in both |
| `esc`, `left`, `backspace` | back | back (in the question flow, `backspace` is the previous question) | yes |
| `/ ? ! , q m ctrl+r` | filter, help, log, settings, quit, mouse, refresh | same | yes |
| `o` | open pane | step into the real pane | yes |
| `a` | go to the most urgent | open the most urgent card | yes |
| `z` | snooze 2h | snooze 2h, or until it changes | yes |
| `h` | show/hide hidden rows | show/hide snoozed rows | yes |
| `b` / `t` | browser / tracker | the PR / the Jira issue | yes |
| `K` | kill the review | stop agents | yes |
| `x` | release (free the slot) | drop (free the copy) | yes |
| `tab` | the other screen (dashboard) | the other view (agents) | yes |
| `G` (watch) | bottom, follow | bottom, follow | yes |
| `y` | confirms only | confirms only | yes |
| `n d e l c L T + −`, digits in cards | unused | new, diff, screenshots, log, say, learning, try again, rate | free on the board |
| `r R i f F I A C D p u * M U s S v O w W` | review, fresh, simplify, fix, fix off, ignore, approve, changes, withdraw, pin, unpin, star, mute, unmute, sort, reverse, view, owner, whiteboard, widths | unused | kept free |

### 9.E Vocabulary (each gloss is shown in `?`)

| Word | Gloss |
|---|---|
| needs you | stopped until you decide or act |
| Nothing waits on you | no task is blocked on you |
| waits on others | blocked by someone or something, not you |
| done today | merged today (milestone 1: pushed today) |
| WAITS ON | who the task waits on, and why |
| YOUR MOVE | the step the factory recommends to you |
| WAITED | how long it has waited on this |
| decisions | single answers you owe, across all tasks |
| `you:` | you are the blocker |
| `agent:` | an AI agent is working on it |
| `CI:` | GitHub's checks are running or failing |
| `alice:`, `rev-ann:` | that person must act, for example review |
| `magnum:` | a magnum review round is running |
| `memory:` | the Mac is short on memory |
| `usage:` | an agent plan's limit is reached |
| `nothing:` | finished; nobody needs to act |
| memory full: new tasks wait | new tasks start when memory frees |
| codex limit: waits till 16:00 | Codex stages pause until the limit resets |
| start | read the Jira issue or your prompt |
| investigate | agents reproduce, find the cause, read history |
| questions | agents stopped for your answers |
| coding | a failing spec first, then the change |
| tests | specs, rubocop, then a Codex review |
| ready to push | committed on this Mac, not on GitHub |
| push + draft PR | push the branch, open a draft PR |
| you pushed 11:02 | you said y; the factory pushed |
| pushed | the branch is on GitHub, draft PR open |
| review | magnum's rounds and people's comments |
| merge master | master merged into the branch, conflicts fixed |
| merge | your final merge into master |
| cleanup | free the copy after your merge |
| send back with a note | the writer tries again with your note |
| drop | stop, free the copy, keep the branch |
| proof | the spec failed before the fix, passes after |
| doubts | what the agents were unsure of or skipped |
| not in the plan | a changed file the plan did not name |
| recommended | the factory's pick; its reason is underneath |
| no recommendation | the evidence favours no answer; you choose |
| ask someone | post a Jira question; the task waits |
| question 2 of 3 | the second of three questions here |
| void, bart, bastion | the three staging servers |
| `–` | nothing of this task is there |
| outdated | an older head is there; updated automatically |
| latest | your latest head is deployed there |
| merging | merging your latest head into that branch |
| conflict | the merge clashes; an agent fixes it |
| CI 12/41 | 12 of 41 checks done on that branch |
| deploying | merged; the server is not on it yet |
| QA 4/9 | t:qa has run 4 of 9 checks |
| QA ✓ / QA ✗ | QA passed or failed on your latest head |
| `?` | unknown: the factory could not read it |
| bart: no QA | bart gets merges and deploys, never QA |
| bastion waits for void QA | bastion QA starts after void passes |
| head | the newest commit of your branch |
| code failed | a check failed because of the code |
| runner died | CI's machine died; rerun, no code change |
| try 1 of 2 | first try; a second failure asks you |
| out of fix tries | the agent gave up; you decide now |
| ready to merge | QA passed on void and bastion, latest head |
| access needed | a login, MFA or VPN step for you |
| snooze | hide it 2h, or until it changes |
| queued | waits for a free copy or memory |
| copy | the task's own checkout, databases and port |
| your PR, written by hand | you wrote it; the factory ships it |
| no output 9m | the pane printed nothing for 9 minutes |
| step in | open the real agent pane in herdr |
| say one line | send one message to that agent |
| you're in it | you're in that pane; the factory sends nothing |
| lesson | one thing a task taught the factory |
| rule | a lesson added to one repo's prompts |
| skill edit | a change to a factory skill or prompt |
| test case | an earlier task kept for replays |
| replay | rerun an earlier task to test a change |
| used in | tasks whose prompts carried the rule |
| repeats seen after the rule | the same mistake, seen in later tasks |
| rule to approve | a proposed rule waits for your y |
| rate + / − | say whether a finished task was right |

### 9.F Notifications and window title

**An alert fires only when a task moves into NEEDS YOU.** It is a magnum toast plus the herdr sidebar token. The triggers are:
- a question batch (one alert per batch)
- ready to push
- a reply to post: a person's comment on a task's PR, once its reply is drafted
- out of fix tries (CI, QA or a conflict), named as such
- access needed: at once, once more after 5 minutes, and at "2m left"
- ready to merge: QA ✓ on void and bastion at the latest head
- an agent showing a prompt the factory does not know

**Rules:**
- Alerts within 2 minutes merge into one toast, for example "factory: 3 need you · PR-12345 questions · #4512 reply · PR-12360 ready to push".
- No toast while the factory terminal has focus (`View.ReportFocus`, focus and blur messages) or while you are in a factory pane (herdr's focused pane, after `o`). A held alert fires 2 minutes after you leave, if the task still needs you.
- magnum's quiet hours hold toasts too.
- There is no alert for stage moves, agent progress, green CI, deploys, QA in progress, a conflict fixed (only a row note and a `!` entry, even inside someone else's change on a staging branch), queued tasks starting, or lessons proposed.
- Nothing vanishes. NEEDS YOU is the persistent list, and `!` keeps every alert with its time.

**Window title (`View.WindowTitle`):**
- `factory · 2 need you`
- `factory · access needed, 8m left`
- `factory · nothing waits on you`

Never use a title that looks like the marker `reveal` puts on herdr's client.

**Sidebar:** the same short text goes through magnum's `notify.Sidebar` (`workspace.report_metadata`), not OSC or `ProgramStatus`.

### 9.G Milestone 1, and what later milestones add

Milestone 1 covers the stages up to ready to push. Then your `y` on the ready card makes the factory push and open the draft PR. I keep this, so it is not an open question. The task then goes to DONE TODAY, and the magnum board takes over the review.

```
 magnum factory · talkable                                    memory full: new tasks wait · ↻ 14:09 
 ● 2 tasks need you, 4 decisions · 2 wait on others · 1 done today

 NEEDS YOU                          WAITS ON                                 YOUR MOVE        WAITED
› ● PR-12345  Credit expiry remin…  you: 3 questions before coding           answer them         41m
  ● PR-12360  Custom activities l…  you: ready to push · 3 files +48 −6      diff, then push     12m

 WAITS ON OTHERS
  ⣷ PR-12371  Coupon CSV export t…  agent: finding the cause, 2 of 3 done                         9m
  ◷ PR-12388  Weekly gem upgrades   memory: full, starts below 80% swap                          18m

 DONE TODAY
  ✓ PR-12302  Remove old sync job   nothing: you pushed 09:40, draft #4519
── ✓ 13:31 you answered 1 of 3 · nothing sent yet ──────────────────────────────────────────────────
 3 questions before coding · first: "When does the reminder go out?" · 1 answered, 2 to go
 enter answer · a oldest ask · o pane that asked · n new task · ? more
```

**Milestone 1 has:**
- NEEDS YOU kinds: questions, ready to push, access needed, out of fix tries, unknown prompt.
- The task card with STAGES (start to pushed), AGENTS, REPORT and LEARNED.
- Stage cards with the mirror, the agents view, and new task.
- Learning from your typed lines, your answers and `+ −`, with rules that wait for your OK. Skill edits come once enough finished tasks exist to replay.
- No staging columns, because no row has staging data.
- No adopted PRs.

**Later milestones add, without a redesign:**
- Staging columns appear when the first row has a PR.
- Reply to post and ready to merge join NEEDS YOU in the same row shape.
- REVIEW COMMENTS, CONFLICTS, CI and STAGINGS appear in the task card, in fixed order, once they have data.
- The stage list is data, so new stages are new lines.
- Your own PRs join as rows keyed `#4512`.
- Stage cards for CI and QA reuse the conflict card's layout.
- At 200 columns, KEY splits into JIRA and GITHUB.

### 9.H Open questions of the screen

The designer's five questions are settled in section 13. Adoption is explicit (13.1). The staging-conflict policy is open (13.3). QA fix tries, the 7-day branch after a drop and the 2-hour snooze are calls (13.2).

## 10. Restarts and durability

- The factory registry keeps a row for each of these: a task, a stage attempt, an input head, a prompt version, a session id, a pane and workspace id, a question, an answer and an external effect.
- The daemon records the intent to launch an agent before it starts the pane. After a restart, it reconciles by session id, copy and attempt before it launches anything.
- A live pane goes on. `Manager.Recover` binds it to its stored session again, and a submitted prompt is observed, never sent again.
- magnum sends a prompt before it saves that the prompt was sent. If the daemon stopped in that window, the factory reads the agent's transcript (the Claude session file or the Codex rollout) for the prompt's hash from the prompt ledger. If the prompt arrived, the run is marked sent. If it did not, the factory sends it.
- magnum creates a herdr workspace before it saves its pane. The factory puts the attempt id into the workspace label, so after a crash it finds a workspace that no row names, and adopts or closes it.
- Crash tests: between the workspace creation and its row, between a prompt and its row, during a push, during a staging merge, and during a question batch.
- If a pane is lost, the agent CLI resumes its session from the stored session id. If the CLI cannot resume it, the stage starts again from its files.
- Each external effect (a push, a PR, a comment, a rerun, a staging merge) has an idempotency key. The daemon looks for an existing result (the PR, the pushed commit, the run) before it tries again.
- Milestone 1 proves this with a restart test: restart the factory daemon during a launch and during work. The panes go on, no prompt is sent twice, and the logs stay.

## 11. Learning

The operator said: "very important for the factory to constantly improve itself - based on my feedback, my guidance, review, etc. - it should become better and better, not repeating previous mistakes".

### 11.1 What the factory captures

- A prompt ledger: every prompt that the factory sends to a pane, with its time and hash. A user turn in the agent's transcript counts as the operator's message only with positive proof: it came through "say one line", or herdr showed the operator in that pane when it arrived, and it matches no ledger prompt and no tool result. A turn of unknown origin is kept for evaluation only, never as a lesson. magnum today records only that the operator typed, not what.
- The operator's answers to questions, and the questions that the operator called needless.
- `+` and `−` on finished tasks, with one key for the reason.
- Tasks that the operator stopped or redid.
- From later stages: magnum's findings on factory PRs that the writer fixed, people's review comments, CI code failures, QA bugs, and conflicts that came back.

Captured text is scrubbed: `execx.Redact` plus AWS keys, other token shapes, `password=` pairs, emails and host names. Long pasted blocks are dropped. Messages within a few minutes of an access card are dropped, because they can hold codes or passwords. The scrub runs before every place that keeps text, the event log of "say one line" too. The text lives only in the factory registry, never in tracked files. A skill edit that a lesson shaped goes through magnum's public-data check (`make check-private`) before it lands.

### 11.2 Two paths

- The fast path: a daily factory retro (a pane agent, as magnum's retro is) turns each correction into a candidate rule for one repository: "When X, make sure that Y, because Z". The operator approves it on the learning screen with `y`. From then on, every later task in that repository gets the rule in its prompts. The rules live in a factory store of their own, with versions, apart from magnum's review notes, which the judge rewrites. They reuse the notes code for versions and the archive. Each task records the ids and versions of the rules that went into its prompts.
- The slow path: an edit to a factory skill or prompt. A skill editor proposes it. A gate replays earlier tasks with and without the edit. If a target improves and nothing gets worse, the edit lands. A case is a past task: its input, its base commit and its acceptance (the spec that proved the fix). An investigation-only replay compares the cause and the plan and costs less.

### 11.3 Rules that the loop keeps from magnum

- Learn only from people, never from the factory's own output.
- A lesson names no PR, login, URL or repository.
- A replay runs against a base that master still holds. One replay of a case at a time.
- At most one landing a day. If a measure gets worse, a revert waits for the operator.
- After a Codex cybersecurity warning, no Codex replay of that case.
- The operator approves every rule and every edit.

### 11.4 magnum-improve

The operator's improvement loop (`/magnum-improve`) covers both halves:

- `evidence.sh` gains factory sections: tasks, stage times, the operator's messages and seconds per task, corrections, stuck or recovered panes, CI fixes and reruns, conflicts, QA results, lessons and repeats.
- Lenses 0 and 1 get factory twins: what reviewers and QA found in factory PRs that the factory missed, and what the operator rejected, redid or corrected.
- A factory-quality lens reads stage failures, caps used up and questions that the operator called needless.

### 11.5 Measures

- The operator's messages per task (baseline: one ticket takes 5 to 20 sessions, a median session has 6 messages from the operator).
- The operator's seconds per task.
- Time from start to ready.
- Repeats seen after a rule.

## 12. Safety

- Branch protection on master stays. No factory identity has a bypass.
- Factory agents push only through the push helper, only to the task's branch or a staging branch, never with force.
- Jira text, PR comments and checkout files reach agents as data files. Prompts carry keys, URLs and paths.
- magnum's rules for a checkout whose `.claude/`, `.codex/`, `CLAUDE.md` or `AGENTS.md` changed apply to factory sessions too.
- On one macOS user, an agent pane can read the operator's `gh` login from the keyring, as magnum's fix agent can today. So the push helper, the pre-push hook and the prompt rules stop mistakes, not a determined agent. The boundary that stops damage is on GitHub: master protection, required reviews and no force-push. The staging branches have no such protection, so a bad push there is visible and can be undone, but it reaches other developers. A stronger boundary needs a second macOS user or a sandbox for the agents, which is not planned.
- Logs and errors never contain tokens or pane environment values.
- Replies to people and to magnum's findings post without asking, as the fix loop does today (the operator's decision).
- Production: an agent runs only a read-only script that the operator approved on a production card, bound to the script's hash, through `magnum factory prod-run`. The operator's AWS and kubectl sessions are readable on the same macOS user, so this rule stops mistakes, not a determined agent.
- From milestone 2, factory pushes and comments use a GitHub App with no bypass of master protection. The PRs stay the operator's own, so magnum never auto-approves them (GitHub refuses an approval by the PR's author).

## 13. Decisions

### 13.1 Made by the operator

- Option B: a separate daemon in the magnum repository.
- The screen layout: one list grouped by who each task waits on.
- Milestone 1 writes the fix locally. Tasks are bugs, small features and chores.
- Own copies with their own databases, Redis offset and server port. At most 3 active tasks. A memory gate.
- A separate `magnum factory` screen in its own terminal.
- CI is watched and fixed, and a dead runner only gets a rerun. QA with `t:qa` on void, then bastion. All three stagings get merges, and an older head is replaced without asking.
- The factory learns from the operator's corrections, and magnum-improve covers both halves.
- Explicit adoption of PRs, Jira issues and branches started by hand.
- magnum's fix loop moves into the factory in milestone 2.
- Lean and configurable: staging branches and stages live in `config.toml`.
- Production: an agent runs a read-only script after the operator's `y` on a card that shows it.
- Replies to people's comments post by themselves.
- A staging conflict in other people's changes is pushed after the specs pass.
- PRs open under the operator's login. From milestone 2, factory commits and comments come from a new GitHub App of the factory, not the review App. It is installed on the factory's repositories only, with `contents: write`, `pull_requests: write`, `actions: write`, `checks: read`, `deployments: read` and `metadata: read`. It has no `workflows` permission, so GitHub refuses its pushes that change `.github/workflows`.

### 13.2 Calls made in this document (the operator can override them)

- Milestone 1: `y` on the ready card makes the writer open the draft PR with `t:create-pr` as the operator, and the factory asks magnum for a review.
- Agents run through `agents.Manager` over a store file of the factory (section 4.3).
- A task holds its copy until its PR opens. Later stages claim a copy when they need one.
- Near a usage cap, the factory pauses before magnum's reviews.
- In milestone 1, "ask someone" drafts the Jira comment for the operator to post. The factory writes to Jira in a later milestone.
- Milestone 1 configures one repository. More repositories are configuration later.
- The milestone 1 exit test counts the operator's typed messages, answer batches and card decisions per task, apart.
- From the screen spec: QA fixes run up to 2 tries before NEEDS YOU, a dropped task keeps its branch for 7 days, and snooze matches the board's 2 hours.

### 13.3 Open, for the operator

1. For milestone 4: how to read the commit that a staging server runs (the GitHub deployment, the image tag in the cluster, or a version page).

## 14. Milestones

1. Investigation and the local fix, in two parts.
   - 1a: one task end to end: intake, investigation, the gate, the batch of questions, implementation with proof, the ready card and the draft PR through the writer. The screen: home, the task card, the question card, the ready card and new task. The prompt ledger records. The restart and crash tests pass. Before any screen code, real-painter mockups at 100, 140 and 200 columns go to the operator.
   - 1b: the agents view, the stage card with the mirror, "say one line", access cards and the preflight, alerts, the learning screen and the fast learning path.
2. The PR. The push helper, the branch lock, the fix loop moved from magnum into the factory, the review requests to magnum, adoption of PRs, issues and branches.
3. CI and the parent branch. CI watched, fixed or rerun, the parent merged on a conflict, before stagings and daily.
4. Stagings and QA. void comes first: the merge, conflicts, the deploy test and QA with a JSON verdict. Then bart and bastion, the redeploy of the latest head and the "ready to merge" alert.
5. The slow learning path and scale. Gated skill edits, the factory lenses of magnum-improve, parallel implementation tries, Jira comments on request.

Exit test of milestone 1: one bug, one feature and one chore reach "ready to push". The operator sends fewer messages per task than today's median of 6 per session.

## 15. Left to the builder

- The exact tables of the factory registry and the request kinds.
- The Redis offset for each copy and the Sidekiq switch.
- The format of `report.md` and `questions.json`.
- The prompt files and the factory skills (`prompts/factory-*.md`, `skills/magnum-factory-*/SKILL.md`), loaded at daemon start like magnum's.
- The CI cause detection (job steps and annotations), cached per run id.
- The scrubber patterns and the size limit for captured text.
- The probe of how Codex takes a prompt in the middle of a turn, before "say one line" ships for Codex panes.
