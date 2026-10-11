# The fix loop: magnum fixes its own findings on your PRs

Status: approved in conversation on 2026-10-10 (the fixer runs in the PR's slot, the loop starts by hand
for a first week, the fixer answers only the clear findings in your name, the PR board gets a key, and
magnum ships its own fix skill so the feature works on any machine). Implementation planned.

## 1. Goal

Today a review of your own PR is a relay. Magnum posts findings, you type `/resolve-review-2` in a
herdr pane of your checkout, confirm its verdict table, it fixes and pushes, magnum re-reviews the push
and finds the next things, and the relay goes on for several cycles. You are the only link between two
agents that could talk through GitHub by themselves.

After this change, one key on the PR board (`f`) or one command (`magnum fix <pr>`) starts a loop that
magnum runs to the end: a fixer agent works in the PR's own slot as you, follows magnum's own fix skill,
fixes the findings worth fixing, answers the rest in the review threads, pushes, and magnum re-reviews
the push. The loop stops when a review posts no findings, when the fixer needs your call, or after a
capped number of cycles. You are needed for the contested findings, the design-level ones, a red CI the
fixer could not clear, and the merge. A later config key starts the loop on every own PR without the
key. Nothing in it depends on a skill or a plugin that exists only on this machine: the fix skill ships
in the repository and in the binary, as the judge skill does.

## 2. Context

### 2.1 What exists

- A round runs the review roles and the judge in the PR's slot, posts one review, and `finish`
  (`internal/engine/round_settle.go`) settles it. `runRound` (`internal/engine/round.go`) already acts
  on a posted round after `finish` (auto-approval). The round's result (`pipeline.RoundResult`)
  carries the review's event and the findings count per priority.
- No role may edit the checkout: the pipeline restores the tree after every stage
  (`internal/pipeline/checkout.go`). A fixer cannot be a pipeline role.
- A PR is held by one reservation at a time (`reserve`, `internal/engine/dispatch.go`); `magnum open`
  holds a PR the same way while it restores sessions. Dispatch gates (`snoozeHolds`, `prGate`) and
  `requestReview` decide what may start on a PR.
- A push moves a reviewed PR to `rereview_pending` (`onHeadChange`, `settlePush`); a forced review
  bypasses the throttle and the daily cap. `magnum review` reaches the daemon as a registry request
  plus SIGUSR1 (`store.EnqueueRequest`, `engine.KickDaemon`), and `--wait` follows the PR row.
- A session is a herdr pane in the PR's workspace; its env is fixed when the pane is created and
  carries the posting identity's GitHub env (`Engine.paneEnv`). A role's `identity` key is declared
  (`config.Role.Identity`) and validated, but nothing reads it. The "me" identity (`kind = "gh"`) has
  no `GH_CONFIG_DIR`, so a pane with its env runs `gh` and `git` as you.
- A run is `NewRun` plus `Submit` (`internal/agents/prompt.go`). An agent working with no run in
  flight counts as a human and blocks the PR for `human_cooldown`. A report must start with the run
  marker when the prompt carries one. The observer ends a run after two idle ticks.
- A slot is checked out detached at the PR head and `checked_out_sha` is recorded; the guard holds a
  slot whose HEAD drifted from it (`internal/slots/guard.go`).
- The judge's skill is a file in the repository (`skills/magnum-review/SKILL.md`, `Role.Skill`,
  judges only). The daemon copies it at startup to `<state dir>/skill/<hash>/SKILL.md` and the
  prompts start with `[$magnum-review]({{.SkillPath}}) Read {{.SkillPath}} first … and follow it;
  load no other skill.` A binary without a checkout uses the copy it embeds. The self-improving loop's
  skill editor edits that file and the judge prompts, nothing else.
- The re-review reads the author's replies in magnum's threads: `classifyReply`
  (`internal/pipeline/replies.go`) understands "Fixed in <sha>", "Won't fix: …" and "Not a bug: …",
  and the retro records declines.
- `f` and `F` are free on the PR board, the dashboard and the right-click menu; every key that starts
  work asks y/N.

### 2.2 Your skill, and why magnum does not call it

`t:resolve-review-2` (your plugin) gives each review comment a verdict with a net score, stops for
your confirmation, then fixes, runs the touched specs, pushes, replies with a `(Claude)` prefix and
resolves bot threads. It is the model for the fix skill's policy. Magnum does not run it: it is
interactive, it lives in a private plugin, and its script paths point at a directory that does not exist
on this machine. The fix skill below carries the same
judgement as a document in magnum's repository, with no scripts, so it works wherever gh works.

## 3. The fix job

### 3.1 When it starts

Two ways, both ending in the same job.

- `magnum fix <pr>` (board key `f`, dashboard too, right-click menu, y/N first, `--wait` follows the
  loop, `--dry-run` prints the request). The daemon refuses, with the sentence as the request's
  answer: no magnum review on the PR's current head ("nothing to fix: no review on <sha7>"); a review
  without findings ("nothing to fix: the review on <sha7> found nothing"); an author who is not you
  (`Config.SelfLogins`; "PR #N is <login>'s, not yours; the fixer pushes and replies as you"); a
  cross-repository PR ("the head lives in a fork magnum cannot push to"); a round in flight, a pinned
  slot or a snooze (the usual refusals); a loop already running ("fixing, cycle 2 of 3; `magnum
  abort` stops it"); and no fix role for the watch. The command refuses inside an agent pane
  (`refuseInAgentPane`), as `approve` does.
- `[pipeline] autofix = "own"` (a `[[watch]]` may set its own; default `"never"`): after a round
  posts a review with findings on a PR whose author is one of your logins, the job starts by itself,
  with the same refusals. Reserved for after the first week; the key ships with the feature so the
  switch is config only.

A PR you want to act on yourself is taken out of autofix the way a muted PR is taken out of automatic
reviews: `magnum fix --off <pr>` (board key `F`, y/N) sets a per-PR flag, `magnum fix --on <pr>` (`F`
again) lifts it, the board shows "fix off" beside "muted", and `magnum status <pr>` says so. Autofix
never starts a loop on such a PR; a `magnum fix` you ask for runs anyway, with the note "fix is off for
this PR: this requested loop runs anyway", as a forced review runs on a muted PR. A running loop ends
with `magnum abort`.

### 3.2 Where it runs

In the PR's slot, pool slot or per-PR worktree, which already holds the reviewed head, the gems and
the databases, so the fixer runs specs through `{{.DBLockCommand}}` like a reviewer. The job holds
the PR for its whole run with a reservation (`roundHandle{fix: true}`), and a per-PR registry row
`KVPRFix` records the loop: the cycle, the cap, the head it started from, who started it, and the
last report path. While the row exists, dispatch skips the PR beside `snoozeHolds` with a wait reason
`WaitFixing`, `prGate` refuses, and `requestReview` refuses a review with the sentence above. No new
PR state: the PR stays `reviewed` while the fixer works, and the board's state cell reads the row. The
only migration (0025) widens the `runs.kind` check with `fix`, the kind of the fixer's runs.

The job's steps are idempotent and write `begin`/`ok`/`fail` events like a round's:

1. `guard`: the slot belongs to the PR, no round is in flight, no human evidence, HEAD equals
   `checked_out_sha`, the head equals the reviewed head. A failed guard ends the job as `needs
   attention` with the reason.
2. `start`: the fixer's session starts, or resumes, in the PR's workspace with the fix role's
   identity env (3.3). `NewRun` with kind `fix` and the role's report path under the head's review
   directory, then `Submit` with the prompt (3.4).
3. `wait`: the run ends when the agent goes idle, the timeout types the "write the report now" line
   as for a reviewer, and `magnum abort` cancels the reservation and interrupts the agent.
4. `settle`: the report is read (3.5) and the slot is put back in order: `git switch
   --discard-changes --detach <pushed sha>` with `checked_out_sha` updated when the fixer pushed,
   the same to the old head when it did not, so the guard sees no drift and the next round's
   checkout starts clean. The fixer's local commits survive only on GitHub.

### 3.3 The fixer role

A built-in role, declared like the reserve judge:

```toml
[[role]]
name = "claude-fix"
kind = "claude"
fix = true                        # the fix job's role: never part of a round
# identity = "me"                 # the pane's GitHub env: the watch's poll identity (you) unless set
model = "opus"
effort = "high"
skill = "skills/magnum-fix/SKILL.md"   # the fix skill (an absolute path points at your own)
prompt = "fix.md"
output = "claude-fix.md"
timeout = "60m"
```

The session persists across cycles and loops like a reviewer's, so the fixer remembers what it declined
and why, what it changed and which specs it ran; a fix role takes no `max_session_rounds` (the operator
parks the session for a fresh conversation). `fix = true` is a role class beside `judge = true`: `Stages`, `RolesToRun`, `RolesFor` and
`magnum review --role` leave it out, `Config.FixRoleFor(w)` resolves `[pipeline] fix_role =
"claude-fix"` (a watch may override) and validation refuses a `fix_role` that names a shell role, a
judge, a role in the watch's roles, or a role whose explicit identity is not a `gh` identity; a fix role
without an identity takes the watch's poll identity (the operator's own gh identity in a `magnum init`
config), and an implicit identity that resolves to an App warns under `autofix = "own"` and is refused
when the job starts. The `skill` key,
judges only today, is allowed for a fix role with the same handling: expanded by `Load`, copied at
startup under the state directory by hash, embedded in the binary, counted by `magnum status` when
edited on disk, and left alone by the skill editor. `Role.Identity` is wired: `paneEnv` lays that
identity's env over the base env for the role's pane, so the fixer's `gh` and `git` act as you (your
default gh login, your git config, your SSH agent through the daemon's `SSH_AUTH_SOCK`). The pane
keeps magnum's `MAGNUM_*` variables and the slot env.

### 3.4 The prompt

`prompts/fix.md`, rendered with the reviewer variables plus four new ones: `{{.HeadRef}}` (the
branch to push to), `{{.ReviewURL}}` and `{{.ReviewFile}}` (the posted review's `review.json`: event,
body, comments with path, line, side and body), and `{{.ReviewerLogin}}` (the login whose threads
are magnum's). `{{.SkillPath}}` is the fix skill's startup copy. The prompt is one paragraph, as the
judge's are: `[$magnum-fix]({{.SkillPath}}) Read {{.SkillPath}} first, unless this session already
read that file, and follow it; load no other skill.` Then the facts: the PR URL, the head and the
branch, the review URL and file, the reviewer's login, the report path with the run marker, the
db-lock line, and the sentence that this checkout is the PR author's and the agent acts as the
author. URLs, SHAs and paths, never PR text.

### 3.5 The fix skill

`skills/magnum-fix/SKILL.md` is a document, with no scripts, written for Claude Code and any kind
that reads a skill file. Its sections:

1. **Who you are.** The PR author's assistant, in the author's checkout, acting as the author. The
   review was posted by `{{.ReviewerLogin}}`, a bot identity of the author's own reviewer. The
   skill never resolves or minimizes a human's thread, never force-pushes or rebases, never touches
   another branch, never writes a `magnum:run=` marker on GitHub, adds no dependency, and changes
   nothing outside what a finding names.
2. **Read.** The review file for the findings, the review's comments with their ids and thread ids
   (`gh api repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments`, then the threads through
   GraphQL for `isResolved` and the thread ids), and the current code each finding names. A finding
   may point at code that has already changed.
3. **Judge each finding.** Skeptical by default: a finding is wrong until the code proves it right.
   One verdict each: `fix` (right, and worth it), `already fixed` (a later commit did it), `not a
   bug` (the analysis is wrong, with the reason), `won't fix` (right, but not worth it here: the
   project does not face what it guards against, or the code is pre-existing and outside the PR's
   purpose). Each `fix` carries a net score: pros minus cons, weighted by what they are worth to this
   project on an open scale with anchors 1 (nobody notices), 3 (the next person to touch this code
   feels it), 10 (real money, real user harm or a day of work) and 30 (not safe to ship, or breaks
   code that works today). Above 3 the item must name the consequence. A defect the diff introduced,
   a broken reference, a failing test or a crash is always `fix`.
4. **The policy.** `fix` with net 3 or more: fix it. `fix` below 3: `won't fix` with the score as the
   reason. `not a bug` and `won't fix` on an optional finding (P3, a simplification, a nit): reply.
   `not a bug` or `won't fix` on a blocking finding (P1, P2), and any finding the skill cannot decide
   from the code: no reply; it goes to `Needs you` in the report with the reason.
5. **Fix.** One commit per finding, or one per file set that one fix touches, with a one-line message
   that names the finding. Run the specs the fix touches through the db-lock line and quote the
   command and its result line in the reply. Push with `git push origin HEAD:{{.HeadRef}}`. Confirm
   the commit is at origin (`gh api repos/{owner}/{repo}/commits/<sha>`); if the push failed, stop
   and report it.
6. **Reply.** In each of the reviewer's threads: "Fixed in <sha7>: <what changed>, <spec command>
   passed", "Already fixed in <sha7>", "Not a bug: <reason>" or "Won't fix: <reason>", one reply per
   thread, through `gh api repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies`. Resolve the
   reviewer's threads it fixed through GraphQL `resolveReviewThread`; leave the others open. Reply
   nowhere else: no issue comment, no summary reply.
7. **Report.** The file at the report path, starting with the run marker, then `## Pushed` (one line
   per commit, `<sha7> <message>`, or `nothing`), `## Replied` (`N threads (a fixed, b already
   fixed, c not a bug, d won't fix)`), `## Needs you` (one line per finding with its comment id, its
   priority, its path and line and the reason, or `none`).

The skill is magnum's and is copied and embedded like the judge's; an operator who wants another
policy points the role's `skill` at an absolute path. The learning loop does not edit it.

### 3.6 The report and the loop

magnum reads the three sections with the leniency it reads reviewer reports with. `settle` decides:

- **Pushed and `Needs you` empty**: the cycle counts (`cycle + 1`), the slot moves to the pushed
  head, and the PR gets a forced re-review (the same request `magnum review` sends, so the throttle
  and the daily cap do not apply; the loop has its own cap). When that round posts, `runRound`'s
  hook asks the loop: findings and `cycle < fix_cycles` start the next fix run; no findings end the
  loop as done, with an info toast "PR #N: clean after 2 fix cycles" and a `fix.done` event; the cap
  ends it as `needs attention` with "3 fix cycles, 2 findings left: <report path>".
- **Pushed and `Needs you` not empty**: the re-review still runs (the push deserves it), and the
  loop ends after it as `needs attention` with "fix: 2 findings need your call: <report path>".
- **Nothing pushed, `Needs you` empty**: the fixer answered every finding; the loop ends with an
  info toast "PR #N: claude-fix pushed nothing, replied to 3 findings". The replies reach the judge
  through the existing reply round.
- **Nothing pushed, `Needs you` not empty**: `needs attention` with the reasons.
- **No report, a stale marker, a failed push, a timeout, an abort**: `needs attention` with "fix run
  failed: …", the loop ends, the slot is put back to the head it had.

A push by you while the fixer works makes its push fail (not a fast-forward), which is the failed
case above. A fix cycle never counts against the PR's daily review cap, the forced re-review neither,
and `fix_cycles` (default 3, per watch) bounds the loop.

### 3.7 What you see

- The board's state cell: "fixing 1/3 · claude-fix", then "reviewing" for the re-review, and so on.
  The dashboard shows the fixer's pane like any agent's. `magnum status <pr>` prints a `fix:` line
  with the cycle, the cap, who started it and the last report path.
- Events: `fix.start`, `fix.pushed`, `fix.replied`, `fix.needs_you`, `fix.done`, `fix.failed`, in
  `magnum logs pr:<ref>`; the pushes show in the PR's history as yours.
- Toasts: an info toast when a loop ends clean or with nothing pushed; the urgent attention toast
  otherwise, with the report path, and the attention key opens the pane.
- `magnum abort <pr>` stops the fixer and ends the loop; `magnum review <pr>` refuses while fixing.
- "fix off" in the board's flags and on the status card for a PR you took out of autofix.
- `magnum stats` counts fix cycles per day and repository, beside rounds. `magnum doctor` checks the
  fix skill renders and that the fix role's identity is a `gh` identity whose login works.

### 3.8 Configuration

```toml
[pipeline]
fix_role = "claude-fix"           # the fix job's role (a [[watch]] may set its own); "" = no fix job
fix_cycles = 3                    # fix runs per loop before the PR needs you
autofix = "never"                 # never | own: start the loop by itself after a review with findings
                                  # on a PR of yours (a [[watch]] may set its own)
```

Your config needs nothing for the first week; then `autofix = "own"`.

## 4. Rejected alternatives

- **Running the operator's own skill.** `t:resolve-review-2` is interactive, private to one plugin
  and broken on this machine's paths; a tool meant for other machines carries its own skill, as the
  judge does, and an operator who prefers another policy points `skill` at it.
- **A pipeline role.** The pipeline restores the checkout after every stage and treats a report as a
  review; the fixer edits the checkout and pushes.
- **A separate author worktree per PR.** A full database setup per PR (eight databases for talkable)
  and a second checkout of the branch next to yours; the slot has both already and never touches your
  checkouts.
- **A new PR state `fixing`.** A migration and a state every screen and gate must learn; a registry
  row plus a reservation holds the PR as well, as a snooze does.
- **Scripts in the skill.** The judge skill is a document and the fixer's needs (reading comments,
  replying, resolving threads) are a few `gh api` calls the skill spells out.
- **Any PR by default.** The fixer pushes and replies in your name; on another author's PR that is
  their call.
- **Codex as the fixer.** The judge's kind; a second model catches the judge's errors.
- **Fixing CI in the fixer.** The judge already reads failing checks and never reads LGTM on a red
  head; the next cycle's findings carry them, and the cap bounds the rest.
- **A loop without a cap.** Two agents can disagree forever; three cycles cover every real case seen
  so far (two on #738, two on #11932).

## 5. Documentation and tests

README: a `magnum fix` row in the command table, `f` in the board keys, the configuration keys, a
"Fixing your own PRs" section that names the skill file. `config.defaults.toml`: the keys and the
role block. `prompts/README.md`: `fix.md` and its variables, and the fix skill beside the judge's.
`docs/DECISIONS.md`: one entry at the end of "Agents in panes". `docs/API.md` regenerated
(`DashboardActions` gains `Fix`). AGENTS.md's startup note names both skills.

Tests name the behaviour: config (`fix = true` roles leave rounds, `fix_role` validation, `autofix`
values, `FixRoleFor` per watch, `Role.Identity` reaches the pane env, a fix role's `skill` is loaded,
copied and embedded like a judge's), engine (a fix request on a reviewed PR with findings starts the
job and holds the PR; refusals for no review, no findings, not own, cross-repo, fixing already; `fix
--off` keeps autofix away and a requested loop runs with the note; a
pushed report settles the slot and forces the re-review; a clean re-review ends the loop with the
toast; the cap ends it as needs attention; needs-you ends it as needs attention; nothing pushed ends
it with the info toast; a missing report fails it; abort ends it; `autofix = "own"` starts it only
for self authors), agents (the fix run's kind and report path, the marker), slots (settle switches to
the pushed head without drift), cli (`magnum fix`: y/N, `--wait`, `--dry-run`, `--off`/`--on`, the refusal sentences,
the agent-pane refusal), tui (keys `f` and `F` on the board and the dashboard ask y/N and dispatch; the
key tables; the "fix off" flag), prompts (`fix.md` renders with the new variables and the skill link), and a render test that
the fix skill is embedded and its copy is taken at startup.
