# Decisions

Why Magnum works the way it does. One entry per decision, newest last, each with the
alternative that was rejected. Change a decision by adding a new entry that supersedes it, not by
editing history. Code, config comments and prompts reference these by their heading.

## Runtime and scope (2026-10-02)

- **One Go binary, daemon plus CLI, SQLite registry, no IPC server.** The CLI and the daemon share the
  registry; mutating commands write a request row and `SIGUSR1` the daemon. Rejected: a gRPC/HTTP
  daemon API (one more port and auth surface for no gain on a single machine).
- **Reviews run in interactive herdr panes, never headless.** You can watch, take over or answer an
  agent; sessions persist so re-reviews keep context. Rejected: `codex exec` / `claude -p` batch runs
  (no continuity, nothing to look at when it goes wrong).
- **Three-agent pipeline with a judge.** Candidate reviewers (Claude `/code-review`, Codex `review`)
  propose; one persistent Codex judge proves or rejects every candidate, adds its own pass and posts
  exactly one review. Rejected: posting each reviewer's output (duplicate and contradictory comments).
- **GitHub is the only proof a review was posted.** Each round carries a marker
  `<!-- magnum:run=<id> head=<sha7> -->`; verification looks for it by the expected login on the
  expected commit. Agent status is only a hint for when to check. Rejected: trusting the agent's
  "done" text.
- **Everything lives in the repository**: `config.toml`, prompts, the judge skill, `state/` (gitignored),
  the herdr plugin manifest. Only the launchd plist lives outside. Rejected: `~/.config/magnum` and
  `~/.agents/skills` symlinks (moving parts in three places).
- **All GitHub calls go through `gh api`**, including App token minting. A freshly built unsigned Go
  binary is blocked by Little Snitch on this machine; `gh` is already trusted. Rejected: direct
  `net/http` with a per-build firewall rule.

## Identity and verdicts

- **Polling always uses a `gh` user identity; posting uses the watch's identity** (a user or a GitHub
  App). App installation tokens cannot call `/user`, so App identity is verified through
  `/installation/repositories` and the posted review's `user.login`.
- **A bot never approves by accident.** The default no-findings verdict for an App identity is
  `COMMENT`; `APPROVE` and `REQUEST_CHANGES` must be enabled per repository with a `[[repo]]` block
  (`no_findings_event`, `blocking_event`), which overrides the identity's defaults (2026-10-04).
  Rejected: per-identity only (one App serves many repos with different trust levels).
- **A PR follows its watch's identity until its first review is posted**; afterwards it keeps the
  login that owns the review thread. `magnum review --as` pins one PR explicitly, and a round for a
  changed identity parks the old sessions first because their panes carry the old gh config
  (2026-10-04, after a false "identity leak").
- **A review posted by another login that carries our marker pauses the whole watch** (identity leak).
  Run ids carry 6 random hex characters so a stranger cannot spoof the marker (2026-10-04).
- **A PR follows its watch's identity, also after its first review** (2026-10-04; supersedes "A PR follows
  its watch's identity until its first review is posted"). Moving a watch from one App to another left
  every PR the old App had reviewed posting as the old App. Now a PR nothing reviewed yet still follows
  the watch on every poll, and a reviewed one migrates when its next round is dispatched: `prs.identity`
  becomes the watch's, the old identity joins `pr.<id>.former_identities` (newest last), `pr.identity_migrated`
  is recorded, and the round parks the old sessions through the identity-change path. The round treats the
  former logins as its own history: the previous review (marked `Former`, so the round never dismisses it
  with its own credentials), the threads of the reply contract, the judge's earlier findings
  (`former_logins` in its prompt). Logins compare as accounts: a user `x` and the App `x[bot]` are two. Verification
  still accepts only the new login, so a former login's review with an old marker is neither the round's
  review nor a leak, while one carrying the round's own marker still is a leak. Once the new review is
  posted, each former identity dismisses with its own credentials its own CHANGES_REQUESTED reviews (when it
  dismisses its stale ones) and, for an App without `keep_approvals`, its approvals. A PR pinned with
  `magnum review --as` (`pr.<id>.identity_pinned`) and a paused round never migrate. Rejected: a fresh
  history for the new login (the replies and open findings of the old reviews would be lost) and
  dismissing the old reviews with the new identity (only an identity's own reviews are its to withdraw).

- **Severity decides the verdict** (2026-10-04). 59 of 68 posted findings were P2, and the verdict did
  not follow from them. The skill defines `P1` as blocks the merge (wrong behaviour on a realistic
  path, security, data loss, a broken build or deploy), `P2` as should be fixed before the merge and
  `P3` as optional, with two examples each, and re-ranks candidate priorities by them. The blocking
  event is used if and only if a `P0` or `P1` is posted (an earlier one still open counts), a review
  with only `P2`/`P3` findings is a `COMMENT`, and only a review without findings uses the no-findings
  event. Rejected: the no-findings event (an approval) for `P3`-only reviews, which reads as "nothing
  left to do" while findings are open.
- **The posted review stands on its own and covers exactly its head** (2026-10-04). Every finding
  carries its reproduction inline (the failing input, command or test body, fenced, about 25 lines;
  a test that pins the bug as a `suggestion` on a test file in the diff), because all seven reviews of
  one repository mentioned reproductions the author could not see and four cited `/tmp` or other local
  paths. Local paths are forbidden in posted text and verification warns (`round.local_paths`) when it
  finds one. Checks sit in a collapsed `<details>` block, and failures the review machine caused
  (missing tables, deadlocks, the wrong Ruby) go to the result file's `environment_failures`
  (`round.environment`) and the notes, never into the review. The judge reviews `head_sha` only: the
  skill's "re-read the base and head before posting" predated the restart logic and let a judge call
  four findings fixed by a commit it was not reviewing. Rejected: failing the round on a local path
  (the review is otherwise valid; the warning is for the operator).
- **One run, one review** (2026-10-04). One of 19 reviews was posted twice with identical comments. The
  skill lists the PR's reviews right before its POST and stops when one carries the run's marker;
  verification keeps the first review with the marker, records `round.duplicate_review` and deletes
  pending duplicates (`DELETE .../reviews/{id}` works only for an unsubmitted review). A submitted
  duplicate stays, with a warning, because GitHub cannot delete it. Rejected: dismissing it (dismissal
  applies only to approvals and change requests, and the comments stay either way).

## Checkouts and pools

- **Always detached checkouts of `refs/magnum/pr/<N>`**, never the PR branch, so a PR you hold in a
  manual worktree is still reviewable in a magnum slot and nothing magnum does can push.
- **Big repositories get a pool of warm slots** (`[[pool]]`, min/max, provisioned with the repo's own
  setup script and their own databases); everything else gets a worktree per PR next to its clone,
  with worktrunk hooks or a `[[repo]]` block for setup. Rejected: cloning per PR (minutes per round).
- **A dirty checkout holds a magnum-owned slot only with evidence of a human**: a foreign agent in the
  slot or its workspace, your activity after the checkout, or head drift. Otherwise the changes are
  the round's own residue (test runs rewriting `schema.rb`, copied files), are discarded and recorded
  as a `slot.discarded` event listing the paths. Manual worktrees keep the strict rule (any change
  holds unless `--force`). Rejected: holding on any change (pools starved after one round),
  discarding silently (the pre-review behaviour).
- **Destructive steps re-run their guard when resumed** after a crash; orphan database drops need a
  complete discovery (every worktree list and the MySQL listing succeeded) and a rescan right before
  dropping. Unsuffixed databases are never dropped.
- **Files magnum writes into a checkout go through `os.Root`** so a tracked symlink in the PR cannot
  redirect a write outside the worktree; `.mise.local.toml` is rendered from parsed TOML with token
  keys stripped in every representation.
- **A PR that changes the schema is reviewed on its schema** (2026-10-05, after 4 of 9 rounds of the pool
  repository recorded skipped DB specs: a missing table, missing columns, TEXT instead of MEDIUMTEXT, a
  missing column the PR added). The checkout's schema step only marked the slot `dirty_schema` for its
  release, so the reviewers ran the PR's specs against the base schema. When the round's checkout leaves
  the pool slot `dirty_schema` (the PR changes `schema_paths` since its merge base, the test the schema
  step uses), the pool's `reset_db` commands now run before the reviewers as the first commands of the
  readiness step: `zsh -lc` in the checkout with the slot's env, recorded in `readiness.json`, the
  `round.readiness` event and the judge's `readiness` list as `reset_db` checks. The reset has the
  release's budget for `reset_db` (`slots.ResetDBTimeout`, 30 minutes) of its own, and `ready_timeout`
  starts after it, so `prepare` and `ready` keep theirs whole however long a dev load, a test load and a
  seed take; a reset command that outlives the budget is stopped and the ones after it are skipped. A
  failure does not stop the round; the skill has the judge skip the checks that need the PR's tables and
  record it under `environment_failures`. The slot stays `dirty_schema`, so the release still loads the
  base schema, and a later round of the PR reloads too (also after a push that reverted the change: the
  databases still hold the PR's earlier schema). `[[pool]] reset_db_on_schema_change = false` keeps
  `reset_db` to the release; `magnum doctor` warns about a pool with `schema_paths` and no `reset_db`.
  Not rerun when a push restarts the reviewers mid-round. Rejected: running `prepare` instead (a pool may
  have none, and `db:test:prepare` loads the test database only), counting the reset against
  `ready_timeout` (5 minutes: a slow reset was cut, or left `prepare` and `ready` skipped), and a key of
  its own for the reset's budget (the release's timeout already bounds `reset_db`).

## Scheduling

- **No backfill.** PRs open before the first sync are `baseline` until their next push or a manual
  `magnum review`. Rejected: reviewing every open PR on first start (a usage-limit event).
- **Throttle**: 5 min quiet after a push, 30 min between rounds per PR (2 h for drafts), coalesce to
  the latest head, a daily cap, `magnum review` overrides all of it.
- **Burst-aware quiet period** (2026-10-04): three or more pushes within 30 minutes switch the quiet
  period to 15 minutes, because agent-driven PRs commit in bursts and a round started between two
  commits is stale when it lands.
- **A push during the reviewer stage restarts the round in place** (same sessions re-prompted on the
  new head with the `restart` prompt, reviewers interrupted with Esc, shell panes with ctrl+c, at most
  `max_round_restarts` = 2 per round); **a push during the judge's turn lets the round finish**, the
  posted review gets "_Reviewed <sha>; N commits arrived during the review, re-review follows._"
  appended, and the delta re-review skips the 30-minute spacing (2026-10-04). A head that is an
  ancestor of the target is not a push. Rejected: aborting on every push (frequent pushers would never get a review) and
  waiting for a long quiet period (every PR waits, reviews are still stale when they land).
- **Re-reviews run the judge at `high` effort and confine the Claude reviewer to the delta**;
  the first review stays at `xhigh` (2026-10-04; measured 33–40 min rounds).
- **A paused agent kind blocks only rounds that use it.** Codex capacity counts the Codex roles a
  round actually runs.
- **`skip_paths` on a watch** skips PRs whose changed files all match (docs-only PRs); file lists come
  from one REST call per head, cached. `magnum ignore` is the manual version (mute, kill the round,
  free the slot). Comment-only code changes cannot be detected without reading the diff and stay a
  manual `I`.
- **The Codex budget gates dispatch** (2026-10-04; Codex's weekly limit went from 0% to 99% in 26 hours
  and the daemon had no signal for it). The health tick reads the newest rate-limit snapshot in Codex's
  own session files at most once a minute (`[usage] codex_home`) and records it in kv. At `codex_soft`
  (80%) a first review whose roles run Codex waits with a gate reason; re-reviews, continues and
  `magnum review` still run, because they finish work already promised. At `codex_hard` (95%) the kinds
  backed by Codex pause through the tool-pause path (reason `budget_cap`, one urgent toast), and the
  budget check, not the pause's expiry, lifts it once the budget is below the cap or the window reset.
  Rejected: counting rounds ourselves (Codex knows its account's usage, including the user's own
  sessions) and stopping everything at the soft cap (re-reviews of PRs in flight matter most).
- **Requested reviews run without delay** (2026-10-04). A review request from a person is an ask for a
  review now, yet it waited out the same quiet period, re-review interval and daily cap as a push. A
  ReviewRequestedEvent on the PR's timeline (read with Details, `timelineItems(REVIEW_REQUESTED_EVENT,
  last: 10)`) for the poll login, any posting identity's login (an App's in both its GraphQL and REST
  forms) or a team the watch lists in `request_teams`, and a draft marked ready for review, start a round
  `[daemon] request_debounce` (1m) after the later of the request and the last push, skipping the quiet and
  burst quiet periods, both re-review intervals, the delta threshold and the daily cap. A request is
  edge-triggered: it is recorded once (`pr.<id>.request_at`, `request_by`, `pr.review_requested`), counts
  only when newer than the repository's first sync, the build's first start and the PR's last round start,
  and a review of the head posted after it answers it (a request during a round that covers the head
  starts nothing more). A ready transition on a head already reviewed starts nothing. The wait reads
  `requested by alice → 14:05` (or `→ now`). Rejected: reacting to the pending `reviewRequests` list (GitHub
  keeps a user's request until that user reviews and drops the App's once it reviews, so presence cannot
  tell a new ask from an old one) and marking the PR forced (that also overrides mutes, quiet hours, the
  watch's filters and the budget's soft cap).
- **Small deltas wait for more** (2026-10-04). Every non-trivial push re-ran three agents, also for a
  two-line fix. The compare call the trivial-delta check makes (one per head change, reviewed commit to
  head) also measures the delta: changed lines that are code, judged line by line by the same classifier
  (comments, blank lines, whitespace moves and documentation count nothing; reordered code counts), and
  added, renamed or copied files (`pr.<id>.delta`). An automatic re-review runs after the quiet period once
  the delta reaches `[daemon] rereview_min_lines` (30) or adds a file; a smaller one waits for more pushes, at
  most `rereview_max_wait` (2h) after its first unreviewed push. A file without a complete patch, a failed
  compare and a request or `magnum review` skip the threshold; 0 turns it off and a `[[watch]]` can override
  both keys. The wait reads `small delta 8/30 lines → 16:40`. Rejected: GitHub's additions and deletions
  (comments and docs would count) and summing per-push deltas (one compare of the reviewed commit and the
  head is exact and costs no extra call).
- **The daily cap counts automatic rounds only** (2026-10-04). Requested rounds and `magnum review` record
  their start (the re-review interval and request edge read it) but never count against
  `max_rounds_per_pr_per_day`, whose default rises from 6 to 12 now that it limits only what the daemon
  starts on its own. A refunded requested round restores the previous start, so its request runs again.
  Rejected: a separate cap for requested rounds (a person asked for each of them).
- **Idle agents of reviewed PRs are parked** (2026-10-04; 21 agent processes, about 2.8 GB, idled 10.8
  hours on average). Once every live agent of a `reviewed` PR has been idle (not working or blocked) for
  `[daemon] park_idle_after` (2h) its sessions are parked on the heavy worker under the PR's
  reservation; the next round resumes them as after any park. Pinned or held slots and a keystroke within
  `human_cooldown` keep them. Rejected: `min_warm` as the threshold (it governs slot eviction; 30 minutes
  is too short for agents that keep a conversation for the next push).
- **Idle agents of PRs that wait long are parked too** (2026-10-05, after two `rereview_pending` PRs held 8
  live agents idle for up to 2 hours under `magnum pause`). A `queued` or `rereview_pending` PR is parked
  like a reviewed one, after `park_idle_after` of idling, when the wait the last dispatch recorded for it
  (`pr.<id>.wait`) is `magnum pause` or an infrastructure pause, a drain for a restart, the daily round cap,
  or any wait that ends more than `park_idle_after` from now; the next round resumes the sessions. A wait
  without an end (the next dispatch, a slot, capacity, a mute) is not long. A pinned or held slot still
  keeps its agents whatever the PR waits for, since a person works there, so a wait on a pinned or held
  slot never parks. Rejected: parking every waiting PR after `park_idle_after` (a quiet period or the
  next dispatch ends within minutes, and a resume costs the round its warm agents).

## Agents in panes

- **Agents are launched through the user's own zsh**, so wrapper functions and aliases apply and
  magnum passes only the extra arguments (`resume`, `--name`, model, effort). The `[kinds.*]` tables
  describe each CLI; built-ins: codex, claude, droid, omp.
- **Only first-launch folder-trust dialogs are ever answered**, and only within 90 seconds of the
  start with the full dialog shape on screen. Magnum pre-trusts the directories it creates by writing
  the same entries the CLIs write.
- **Permission prompts that block a magnum run are answered "No"** (`on_permission_prompt = deny`,
  never "Yes"), at most ten per run, followed by one `after_deny_prompt` message so the agent finishes
  without the denied command. Prompts in sessions with no run in flight are yours and are left alone.
  Rejected: waiting for the human (a whole round stalled on Claude Code's "dangerous rm" check, which
  it asks even in bypass-permissions mode).
- **Agent names are unique per owner/repo/number/role** (`mg-<N>-<role>-<6 hex>`), because herdr
  names are global and adoption by name once prompted another PR's agent.
- **Prompts are files under `prompts/`, read at prompt time.** A prompt edit therefore reaches the
  running daemon immediately, and a new template field needs the daemon restarted on the matching
  build: prompt edits and the build restart land together (learned 2026-10-04 when an old daemon
  could not render `.NotesPath`).
- **Repository notes** (`state/notes/<owner>/<repo>.md`, rewritten by the judge, hints to verify) carry
  what a reviewer learned about a repository to the next review. Rejected: writing into the
  repository's `AGENTS.md` (it belongs to the repo and arrives through the untrusted PR channel) and a
  separate memory-extraction agent (extra run for what the judge already knows).
- **The repository's own `AGENTS.md`/`CLAUDE.md` guide conventions only**, never what to post or which
  network or credential commands to run: they come from the PR.
- **Simplify output is a list of optional suggestions, not defect claims** (2026-10-04). The judge keeps
  hunks confined to the PR's own lines that preserve behaviour and are clearly simpler, ranks them by
  substance, posts all of them as `Simplification (optional)` suggestion blocks and reports
  suggested/outside-diff/dropped counts. Rejected: judging them by the defect standard (every hunk was
  rejected for a week), and any cap (six, then 25, both tried and dropped the same day): the diff-only
  scoping of the simplify prompt is what keeps the count sane, and the author decides what to take.
- **A per-model limit switches the session's model; an account limit still pauses the kind**
  (2026-10-04). Claude Code printed "You've reached your Fable limit. /model to switch models." in
  every Claude reviewer pane while the 5-hour and weekly windows were at 20% and 78%; read as a usage
  limit, it paused every Claude role for an hour (doubling) and rounds posted Codex-only reviews. Now
  the health kind `model_limit` (checked before `usage_limit`, which no longer matches a model name)
  makes magnum type the kind's `switch_model` (`/model opus`) into the idle pane and send
  `prompts/model-fallback.md` as a new `continue` run of the same role and round, once per
  `fallback_models` entry. The limit is kept in kv (`kind.<kind>.model_limited.<model>`, until the
  named reset or `daemon.model_limit_cooldown`), so new sessions start on the fallback, live ones
  switch before their next prompt, and both switch back when it ends. Only a kind without
  `switch_model` (codex by default) or a run out of fallbacks pauses as before. Claude Code's `/model`
  also saves the choice as the default for new sessions; the switch back restores it. Rejected:
  relaunching the agent with `--resume <id> --model <fallback>` (session-only, but a quit, a relaunch
  and a trust check for what one typed command does).
- **Magnum answers Yes to Claude Code's "Switch model?" after its own `/model`** (2026-10-04), the one
  dialog besides first-launch folder trust it ever confirms: magnum caused it, Enter is pressed only
  with the cursor on "Yes, switch to …", the observer leaves the session alone meanwhile, and the
  switch counts only when the status line names the new model (`Opus 5.5 | …`); otherwise Esc backs
  out and the kind pauses as on a usage limit. Permission prompts are still always answered No.
- **Every kind can carry a default model** (`[kinds.<k>] default_model`, 2026-10-04): roles without
  their own `model` start with it through the kind's `model` args, and prompts see it as `.Model`.
  The switch-back argument to the CLI's own default, briefly also called `default_model`, is
  `reset_model` (claude: `default`).
- **An observe tick never judges a session lost from a snapshot that may predate it** (2026-10-04).
  A judge was marked lost in the second it started, and its round failed with "no live agent
  session": the tick's herdr snapshot was taken before StartAgent made the session live. The tick
  records when it took the snapshot, and a session that started, changed status or was marked live
  less than 15 s before that (or after it) waits for the next tick. As a second guard, a prompt to a
  role whose newest session is lost while a fresh snapshot still shows its agent makes that session
  live again (`agent.rebound`) instead of refusing.

- **The judge merges the repository notes under a lock** (2026-10-04). Three judges rewrote one notes
  file within four minutes, each from the text it had read half an hour earlier, and the result named
  one of six saved harness scripts (the orphan was the Jest setup another PR needed). The judge now
  takes `state/notes/<owner>/<repo>.lock` with the one-line `mkdir` loop its prompt gives (it waits up
  to three minutes and takes over a lock older than ten), reads the current file again, merges its
  lessons into it keeping other reviews' standing decisions and harness references, writes a temp
  file and `mv`s it, then removes the lock. The prompt lists the harness directory, so a script the
  notes no longer name shows up. Rejected: `flock` (not on macOS by default, and a lock held by one
  shell command cannot span the judge's separate read, merge and write steps) and a magnum command
  that merges (the merge is a judgment the judge already makes).
- **codex review runs against the merge base** (2026-10-04). The default codex-review command is
  `command codex review --base {{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}`: with
  `--base origin/master` a review of a PR whose base branch had moved nine commits ahead flagged files
  the PR does not touch. Rejected: keeping the ref and fetching less often (the base still moves).
- **Plain codex and claude binaries run without approval prompts by default** (2026-10-04).
  `[kinds.codex] args` defaults to `["--dangerously-bypass-approvals-and-sandbox"]` and `[kinds.claude]
  args` to `["--dangerously-skip-permissions"]`; args still apply only without a wrapper, so the
  author's zsh wrappers (which add the same flags) see no change. `magnum doctor` fails, not warns,
  when a kind's session roles start a plain binary without the flag. The trade-off: review agents run
  any command they choose as the user, outside the CLI's sandbox, in a checkout of untrusted PR code;
  what limits them is magnum's own design (detached checkouts that cannot push, PR content treated as
  data, prompts answered No). Without the flags a new user's agents stop at every approval prompt,
  magnum answers No, and a sandboxed Codex judge cannot reach GitHub to post. Rejected: sandboxed
  defaults with per-repository allowlists (they drift with every toolchain and still block the
  network the judge needs).
- **A model switch leaves the user's Claude default model alone** (2026-10-04). Claude Code's
  `/model <x>` also saves x as the default for new interactive sessions (`model` in its
  `settings.json`), so a fallback switch in a review pane changed the user's own next session. Before
  typing the command magnum snapshots that key (`$CLAUDE_CONFIG_DIR/settings.json`, else
  `~/.claude/settings.json`) and restores exactly it once the switch ended, verified or not (absent
  stays absent; an atomic write that keeps every other key and the layout), recording
  `agent.default_model_restored` when it had changed. Claude switches run one at a time, so a second
  switch never snapshots the first one's choice. This replaces "the switch back restores it" in the
  per-model limit entry above. Rejected: restoring only on the switch back (the user's sessions
  started in between got the fallback).
- **A claude agent's turn is not over while its background work runs** (2026-10-06). claude-review ended
  its turn right after starting a spec run in the background (Bash `run_in_background`), herdr showed it
  idle, two idle ticks ended its run, the report was missing, the judge posted "no problems" and the
  checkout was restored under the running specs; the agent went on, resumed by each task notification, and
  wrote the report 2h20m later with the blocking problem a colleague then found. Claude Code ends a turn
  whenever work it started in the background runs (a command run in the background or moved there by its
  timeout, an asynchronous Agent or workflow, a skill forked into the background such as `/code-review`) and
  resumes when that work notifies it. The observer now reads, for an idle claude agent with a run in flight,
  its session's transcript (`<config dir>/projects/<the cwd, every character but A-Z, a-z and 0-9 as
  "-">/<session id>.jsonl`, the config dir being the pane's `CLAUDE_CONFIG_DIR`, else `$CLAUDE_CONFIG_DIR`, else
  `~/.claude`; a project directory named otherwise is found by the session id), from the run's creation on
  (a backward scan finds where, then only appended bytes are read each tick): a background launch (a tool use
  with `run_in_background`, or a result with `backgroundTaskId`, `background: true` or `status:
  async_launched`) is pending until a `<task-notification>` names its tool use or task (any status but
  running) or a TaskStop result names its task, and a notification is pending until an assistant entry
  follows it. While anything is pending the agent counts as working for completion (idle_ticks 0), with one
  `agent.background_wait` event per run ("claude-review: waiting for 1 background task"). A transcript magnum
  cannot find or read, and every other kind, end on herdr's status as before (Codex holds its turn open while
  its subagents run; it never resumes a turn by itself). Rejected: reading the pane for a "background task"
  line (layout-dependent) and holding every idle agent for a while (it would not know when to stop).
- **A reviewer out of time is asked for its report, then interrupted** (2026-10-06). The role's timeout
  still bounds the turn, background work included: when it passes, a session reviewer (not the judge, not a
  shell role) gets one fixed message within the same run, typed only when no dialog is on screen ("Time is
  up ... Stop waiting for background tasks and start nothing new. Write the report to <path> now ..., list the
  checks still running or not run as pending, and end your turn"; `round.time_up`), and from then on its
  background work no longer holds the run; it has `TimeUpGrace` (5 minutes, a constant) to end its turn, else
  it is interrupted (esc) and its report counts as `timeout`, as before. A run that ended without a report
  (`missing`) interrupts the reviewer too, and the `round.reviewer` warning says so and counts the background
  work a claude agent left running, which an interrupt does not stop (an esc ends the turn, not its
  background shells, whose notifications resume the agent). Stopping those shells is left open: magnum does
  not kill processes it did not start. The reviewer prompts (claude-review, claude-rereview, claude-restart)
  name the role's timeout as the time budget (`.Budget`): end the turn with the report written, start
  background work only when it finishes well within the budget and wait for it, prefer the changed code's
  specs to a whole-suite run. Rejected: a config key for the grace (no case for tuning it yet) and a prompt
  file for the message (it carries only the budget and the path, like `after_deny_prompt`).
- **A report is its run's only if its run verified it** (2026-10-06). Report paths are per head
  (`reviews/<owner>/<repo>/<N>/<sha>/<output>`), not per run, so a reviewer that kept working after its run
  ended can write where a later round on the same head looks. The round's start already set aside the files
  there (`<name>.prev`); now each role's file is set aside again right before the role is prompted (a later
  stage, a late write after the round started), and a continued round (`continue`, the judge finishing a
  paused turn) reuses a reviewer's file only when that role's latest run of the paused round on the head is
  verified, warning about a file no run verified. A late write during the next run of the same session on
  the same head is still indistinguishable by path or time; only a run id in the report would tell, which
  the reviewer prompts do not ask for. Rejected for now: per-run report paths (the panes' `MAGNUM_REPORT_DIR`,
  the judge's report directory and eval address a round's reports by head).
- **An interrupted reviewer is told to stop its background tasks** (2026-10-06, amends "A reviewer out of
  time is asked for its report, then interrupted"). An interrupt (esc) ends a claude agent's turn, not the
  shells it started in the background, whose notifications resume the agent with no run in flight. The
  time-up message now asks for that first: "Time is up: this review had <budget>. Stop every background task
  you started with TaskStop and start nothing new. Write the report to <path> now with what you found so far
  (its first line: `<run marker>`), list the checks you stopped or did not run as pending, and end your
  turn." A reviewer interrupted because its run timed out or ended without its report gets, when its
  transcript shows background work still running, one more message within the same run once it is idle
  (`TimeUp` again, never over a dialog): "This review is over. Stop every background task you started with
  TaskStop and do nothing else." Magnum then reads the transcript each poll until none is left or
  `StopGrace` (2 minutes, a constant) passed, and the `round.reviewer` warning says how it ended ("asked it
  to stop the 2 background tasks it started: it did", or ": 1 still runs after 2 minutes"). Nothing is
  killed: magnum does not kill processes it did not start. No message goes to an agent
  whose transcript shows none, cannot be read, or of another kind (Codex has no TaskStop and holds its turn
  open anyway). Rejected: waiting `TimeUpGrace` for the stop (5 more minutes of a round for a step that takes
  seconds); sending it unconditionally (a turn for nothing).
- **A turn a task notification began is nobody typing** (2026-10-06, amends "A claude agent's turn is not
  over while its background work runs"). Background work an earlier run left running resumes the claude agent
  when it finishes; herdr shows it working with no run in flight, which the observer took for someone typing
  into the pane (`human_active`, and Submit waited `human_cooldown`). For a claude agent the observer now reads
  where the turn began: the transcript's newest user entry that starts a turn (text, not a tool result, not
  Claude Code's own `isMeta` text). Claude Code records its origin (`turnOrigin: task_notification`,
  `origin.kind: task-notification`, else `human`; versions without those fields are read from the text, a
  `<task-notification>` block); a notification is no human activity, a typed prompt still is. The last run's
  transcript watch reads on from where it stopped; without one (a daemon restart) a backward scan of the
  transcript's tail finds the last turn start, then only appended bytes are read. Other kinds, and a
  transcript magnum cannot read, count as typed, as before. Rejected: treating any work shortly after a run
  ended as magnum's (it would hide a person who took over the pane).
- **A report names its run** (2026-10-06, amends "A report is its run's only if its run verified it"). Each
  reviewer prompt (claude-review, claude-rereview, claude-restart, claude-simplify) asks for
  `<!-- magnum:run=<run id> -->` (`agents.ReportMarker`, the prompts' `.RunID`) as the report's first line,
  and codex-review's line prints it before the output it tees into the report (`{ printf '<marker>\n';
  command codex review ...; } | tee <report>`; any shell role with `capture = "stdout"` does the same). A
  role whose prompt or line named its run's marker has a report only when the report's first non-blank line
  is that marker: one without it is `missing` with the detail "stale report (no run marker)", one with
  another run's "stale report from run <id>", and a marker with nothing after it is "empty". The run is the
  one the role prompt or line went to, also for its continuations on fallback models (their prompt names
  none, the agent finishes the task as first instructed); the time-up message repeats the marker. A prompt
  or line that names no marker (a role's prompt file of its own, a `capture = "file"` shell role, the
  judge's own pass) needs none, as before. The judge's candidate list is unchanged (a stale report is listed
  as `missing`). Still open: a late write that lands after a report was accepted replaces it, and a
  continued round checks that its paused round verified a report, not its marker. Rejected: the marker
  anywhere in the report (a review that quotes one, as a review of magnum's own code may, would pass).
- **A cold judge starts in a fresh session** (2026-10-06). Codex's prompt cache lasts about 1.5 hours and
  sessions park after 2, so 25 of 39 resumed judge turns started cold, their first turn re-reading the whole
  conversation uncached (4.7M tokens in 2.3 days, avg 187k per turn). `[pipeline] judge_fresh_after` (default
  `90m`, `0` = always resume) starts the judge of a round fresh when its last turn on the PR (the newest
  `ended_at` of its runs) ended longer ago than that and it has a conversation to resume (engine
  `coldJudge`): a live judge is quit first (`agents.Manager.Quit`, now on the engine's `Agents` port), which
  parks its conversation, and one that works or is blocked, or that Quit cannot stop, is resumed as before.
  It takes the path a lost session takes: a re-review becomes a recovery (`judge-recovery.md` has the judge
  read its earlier reviews and their threads from GitHub), a delta check or a same-head re-review runs with a
  fresh judge at its `rereview_effort` (`round.delta_check_fresh` / `round.same_head_fresh`, whose reason
  names the idle time). Only the judge: unlike a lost session's recovery, where every role starts at its full
  effort and reviews the whole PR, the reviewers keep their conversations and their re-review prompts and
  effort (`pipeline.RoundInput.ColdJudge`), since a cold resume costs less than a full review by each of
  them. A continue finishes its paused turn in the old conversation. Each such start is a
  `round.judge_fresh_cold` event ("the judge starts in a fresh session: its last turn ended 1h31m ago
  (judge_fresh_after 1h30m), so its prompt cache is cold", with `idle_seconds`, `last_turn_at`,
  `fresh_after`, `was_live`).
- **Codex roles run at their own effort and with few subagents** (2026-10-06). codex-review set only its
  model, so `codex review` ran at the effort of the operator's global Codex config (`xhigh`), 23% of the
  Codex spend; and the judge started 53 subagent threads in 36 sessions on its own, 27% of the spend (two of
  them 1.4M and 1.3M tokens on one PR). codex-review now has `effort = "high"`, which its command passes as
  `-c model_reasoning_effort={{.Effort}}` (shell roles get `.Model` and `.Effort` as template variables,
  the round's `rereview_effort` in a re-review; `codex-review.sh` does the same) before its `args`, so a
  user's role `effort` or an override in `args` still wins (Codex applies `-c` in order). A role's
  `max_subagents` caps the subagents its agent may have open at once through its kind's new `subagents` args
  (codex: `-c agents.max_concurrent_threads_per_session={subagents}`; Codex's config reference: "Maximum
  number of spawned-agent threads that can be open concurrently, excluding the primary thread", at least 1)
  and `0` through `no_subagents` (codex: `-c agents.enabled=false`, "Enable or disable multi-agent tools");
  a kind without them ignores the key, as it does `effort`. The judge defaults to 2. Codex has no setting
  for how many subagents a session starts in all, only how many are open at once. Not done: a clean Codex
  setup for magnum's sessions. Codex merges `-c` tables into the user's config (`mcp_servers={}` changes
  nothing), so its MCP servers can only be turned off one by one by name
  (`-c mcp_servers.<name>.enabled=false`), and nothing but a separate `CODEX_HOME` keeps the global
  `AGENTS.md` out (Codex reads `$CODEX_HOME/AGENTS.md` unconditionally); a separate `CODEX_HOME` moves the
  login, the session files magnum resumes and reads for usage, the folder trust magnum writes and the hooks
  with it, so it waits for a decision.
- **Magnum's Codex sessions run without the operator's MCP servers, never with a CODEX_HOME of their
  own** (2026-10-06). The operator decided: no separate `CODEX_HOME` for magnum, ever (the login, the
  session files, the folder trust and the hooks stay where they are). So the global `AGENTS.md` keeps
  loading, and only the MCP servers go: the codex kind's new `mcp_off` (default `true`) makes every launch
  and resume of a codex-kind role (`agents` `mcpServers`, `config.Kind.MCPOffArgs` in `Argv`, after the
  subagent args) and codex-review's line (the new `.MCPOff` template variable, which its default command
  and `codex-review.sh` pass after the effort) turn off each server by name with the kind's new
  `mcp_disable` args (codex: `-c mcp_servers.{server}.enabled=false`, once per server), except those in
  `mcp_allow`. The names are read at each launch from the `config.toml` Codex reads: the `CODEX_HOME` of the
  role's pane env (`Config.RoleEnv`, `~` expanded), else `$CODEX_HOME`, else `~/.codex`; the
  `[mcp_servers.<name>]` tables that do not set `enabled = false`. A missing file means no servers; an
  unreadable one and a name that is no TOML bare key are logged (without the path, a pane env value) and
  leave the servers on: a quoted name could change the `-c` key path, and Codex's handling of quoted path
  segments was not verified, so it is skipped rather than quoted. A kind without `mcp_disable` ignores
  `mcp_off`, so `mcp_off = true` on a claude kind (whose `-c` means `--continue`) does nothing. Not
  covered: MCP servers that a plugin or the checkout's own `.codex/config.toml` brings. The judge skill
  says, beside its role, that the operator's instructions for interactive work (status lines, usage-limit
  checks, delegation or orchestration skills) do not apply in a review: check no usage, start a subagent
  only when a review step needs one, end the turn as the skill says (its cap grows by those 278 bytes).
- **A PR that changes `.codex/` runs Codex with its checkout untrusted** (2026-10-06). Codex 0.160 loads a
  project layer from `.codex/` in each directory from the session's cwd up to the project root, for a
  trusted folder only (`config/src/loader/mod.rs`: `config.toml` minus a short denylist of provider, notify
  and profile keys, hooks, rules), and magnum trusts its checkouts. A watched repository tracks
  `.codex/config.toml` with MCP servers whose auth comes from environment variables, and the checkout is the
  PR's head: a PR could add a stdio server (a command Codex starts), point a server's URL elsewhere with
  `bearer_token_env_var` or `env_http_headers` naming a secret of the slot's environment, or set
  `shell_environment_policy`, `zsh_path`, instructions files, agent roles and plugins. W35 read only the
  operator's config. Codex decides a folder's trust from the merged non-project layers plus the session's
  `-c` flags (which merge into the user's tables, `config/src/overrides.rs`, `merge.rs`) before it loads the
  project layers, and a `-c` key path is split on dots, so magnum passes one inline table:
  `-c projects={"<checkout>"={trust_level="untrusted"},"<real path>"={…}}` (`[kinds.codex] project_untrust`,
  `{projects}`), which beats the `trusted` entry it wrote for the checkout (Codex looks up the directory
  itself before the repository root) for that session only and writes nothing. It does so at every launch
  and resume of a codex-kind role and in codex-review's `.MCPOff` when the files on disk under the
  checkout's `.codex/` differ from the merge base of `HEAD` and `origin/<base>` (the round's merge base for
  the shell line): `git diff --name-only <merge base> -- :(top,literal).codex` plus `git ls-files --others`
  (ignored files too: a round's agent may have written one), and also when git cannot tell; a checkout
  without a `.codex` directory runs no git. Declining trust stops more than the project layers, which was
  checked: Codex then leaves the checkout's `AGENTS.md` out of its instructions (`core/src/agents_md.rs`;
  the judge skill reads it itself and treats it as data, which AGENTS.md asks anyway), derives stricter
  sandbox and approval defaults only where nothing sets them (`core/src/config/mod.rs`; magnum's codex args and the operator's
  wrapper pass `--dangerously-bypass-approvals-and-sandbox`; `codex review` sets approvals to never), and
  its TUI opens on a "Folder access" dialog at every start and resume (`tui/src/onboarding`): "Open
  restricted" loads nothing and saves no trust, so magnum answers it at any time (a third trust spec, no
  first-launch window), before a prompt too. The variant "Open existing task", shown for a resumed task on
  Codex's shared daemon, keeps what the task loaded while trusted and is left for the human. Each such
  launch records an `agents.codex_project_declined` event (counts, never a path) and the PR's
  `pr.<id>.codex_project` record with the head, which a launch that finds `.codex/` unchanged clears; the
  board's card says "Codex ran without the PR's .codex/ changes" for the PR's head, and the judge's
  initial, rereview and recovery prompts carry `codex_project: declined` for the round's head, which the
  skill turns into a Checks line (its cap grows by 171 bytes). A PR that leaves `.codex/` alone gets the
  base branch's project config, the team's, as before; the new `project_mcp = "off"` (default `allow`)
  turns its servers off like the operator's, by name, except `mcp_allow`. Rejected: turning off every
  server the PR's file declares and neutralising its other keys one by one (the dangerous keys are many,
  change with each Codex release, and key aliases could hide one), and refusing such PRs (the review still
  works untrusted). Not covered: a Codex TUI attached to a running Codex app-server daemon sends the
  daemon only reasoning overrides when it starts a thread (`tui/src/app_server_session.rs`), so neither
  W35's flags nor this one may apply there (no daemon socket ran on the operator's machine); a session
  herdr restores by itself starts without magnum's flags; a post-merge review compares with
  `origin/<base>`, which then holds the merged change, so it counts as the base's.
- **A PR that changes `.claude/` or `.mcp.json` runs Claude with the user's settings only** (2026-10-06).
  Claude Code 2.1.292 loads from the directory it starts in (the checkout's root) `.claude/settings.json`
  (hooks, which run outside any sandbox, `env`, `enabledPlugins`, `apiKeyHelper`, permissions), the
  servers of `.mcp.json` (connected without asking under `--dangerously-skip-permissions`), the skills
  (a skill folder with `.claude-plugin/plugin.json` is a plugin with hooks and servers of its own),
  commands, agents and rules under `.claude/` and `CLAUDE.md`, through the `project` setting source, and
  `.claude/settings.local.json` and `CLAUDE.local.md` through `local`; magnum trusts its checkouts, so a PR
  could add a hook or a server Claude starts. `--setting-sources user` keeps every one of them out for one
  session and keeps the operator's settings, skills and servers (docs: permissions "What runs before you
  trust a folder" and "Pass `--setting-sources user` ... so Claude Code reads neither the project's settings
  files nor its `.mcp.json`", mcp "Project scope", the Agent SDK's `settingSources` table; in the binary the
  project skills-directory plugins, skills, commands, agents and the `.mcp.json` loader are each gated on
  the `projectSettings` source). It becomes `[kinds.claude] project_untrust`, passed at every launch and
  resume of a claude-kind role when the files under the checkout's `.claude/` or its `.mcp.json` differ
  from the merge base, or git cannot tell, as for Codex (W36's comparison, now one `git diff` and one
  `git ls-files --others` over both paths; a checkout without either runs no git); `{projects}` is now
  optional in `project_untrust`, since Claude's args name no path. `--strict-mcp-config` was not added: it
  also drops the operator's own servers and claude.ai connectors, and `.mcp.json` already follows the
  `project` source (`project_untrust` can carry it). The records are per agent kind:
  `pr.<id>.<kind>_project` (`store.KVPRProject`), `agents.claude_project_declined`, the card's "Claude ran
  without the PR's .claude/ and .mcp.json changes" after Codex's sentence, and `claude_project: declined`
  in the judge's initial, rereview and recovery prompts, which the skill turns into its Checks line (the
  skill's field line lost its explanation, so its cap grows by 22 bytes only). A PR that leaves both alone
  keeps the team's project config. Unlike Codex, Claude Code watches its settings files (hooks included)
  and skills and applies a change to the running session, and loads a `.claude/settings.json` created
  later: a live claude-review that started with the project config loaded (most of them, and every
  adopted one) would take a later push's from disk the moment magnum checks it out, and a quit after that
  would run its `SessionEnd` hooks. So before a round's checkout, and before a restart's switch to a newer
  head, magnum quits such sessions (`agents.Manager.ReloadsProject`; a `session.<id>.project_out` mark tells
  the ones launched with the user's settings only, which stay), parking their conversations, and resumes
  them after the checkout, when their start decides with the new head; a round.project_reload_parked
  event names them. One that works or is blocked holds the round (retried, uncharged). Rejected: always
  `--setting-sources user` (simplest and airtight, but the team's `CLAUDE.md`, skills, hooks and servers
  would never load, which the operator keeps for a PR that leaves them alone), and turning hooks off with
  `--settings '{"disableAllHooks": true}'` (it leaves servers and skills in, and would also drop the
  operator's hooks). Not covered: a PR that adds a nested `<dir>/.claude/skills/` (Claude loads it once it
  works on files there; the comparison looks at the checkout's root only), a team hook that runs a script
  outside `.claude/` the PR changes, an agent that checks another commit out in the checkout while a
  Claude session with the project config loaded runs, and a declined session kept live across a later
  head, whose record names the head it started on.

## Screens and commands

- **Action keys ask y/N and only `y` confirms**; Enter cancels like any other key (a stray `R` followed
  by a habitual Enter must not start a 12-minute round). `ctrl+r`/F5 refresh. Keys mean the same on
  every screen (`r` review, `R` fresh, `i` simplify, `K` kill, `I` ignore, `x` release, `M`/`U` mute).
- **Frames are cached by a fingerprint of everything they show**, because a trackpad scroll in iTerm2
  arrives as hundreds of arrow keys per second and a 5 ms frame per key froze the board.
- **Mouse support is on by default** (`[terminal] mouse`, `m` toggles): wheel scrolls, header click
  sorts, drag resizes with widths kept per screen (`W` resets), right click opens the PR's action menu
  with the same y/N prompts (2026-10-04). Text selection then needs the terminal's override modifier.
  The dashboard's manual-worktrees toggle moved from `m` to `w` for this.
- **`daemon-stop`, `daemon-restart`, `install` and `uninstall` refuse while rounds are in flight**
  unless `--now`, because a restart abandons the rounds (their agents keep working, nobody collects
  the result). The dev loop passes `--now`.
- **`repo#N` resolves against every watched owner** when the default owner has no such repository;
  two matches are an error.
- **The PR board has views, and the filter has qualifiers** (2026-10-04). `v` cycles `all`, `magnum`
  (state not baseline or ineligible), `mine` (assigned to a self login or its review requested) and `ready`
  (open, not a draft, approved, no CHANGES_REQUESTED); the title bar names the view, `magnum prs --view`
  picks the one the board opens in and printed output, and tab keeps it. The filter adds `state:`,
  `assignee:`, `author:` (`@me`) and `review:requested` to the fuzzy words; a comma lists alternatives,
  qualifiers combine with AND, and an unknown `key:` stays free text because titles carry colons.
  Rejected: user-defined saved views (146 of 158 rows were baseline; three fixed views cover the hunts)
  and GitHub's search syntax (the board filters the registry, not GitHub).
- **`magnum init` writes `config.local.toml` from three questions** (2026-10-04): the gh login (default
  from `gh api user`), one repository, and who posts. The result is one gh identity, one watch with
  `include = [name]`, `daemon.default_repo`, no pool; posting as an App adds the App identity and keeps the
  gh login as `poll_identity`, because polling always needs a user. It is validated together with the
  committed `config.toml` before anything is written, refuses an existing file without `--force` (which
  keeps a `.bak`), and never asks for a private key: it prints the `.mise.local.toml` line instead.
  Rejected: a wizard for pools, roles and kinds (the second machine needs one review, not the whole setup)
  and storing the key (secrets stay in `.mise.local.toml`).
- **The PR card and `magnum status <ref>` show the last round's stage timings** (2026-10-04), computed from
  what the registry already holds: the slot's checkout step events before the round's first run, each
  role's runs from submitted to ended, the judge's end to its verification, and the total. A continue
  round has no checkout; a checkout that ended more than an hour before the round is an earlier round's.
  Rejected: a timings table or new events written by the pipeline (one more writer for data that exists).
- **`magnum resume --tool <kind>` also forgets the kind's per-model limits** (2026-10-04), through the
  daemon or directly in the registry when none runs, so a user who knows a limit is over does not wait for
  the cooldown. `magnum roles --kinds` prints each kind's `model switch:` line (switch command, fallbacks,
  default and reset model, or `-` when a model limit pauses the kind), and doctor suggests HTTPS clones
  (`git clone https://…` or `gh repo clone`) with the ssh-agent hint only for an SSH origin.
- **The screens ask only what `y` will do** (2026-10-05, after questions offered a `y` the daemon then
  refused or that did something else: `x` on a pinned PR, `A` after the head moved, `R` on a PR under review
  flashing a green "follow it with `magnum review --wait`", `I` on a merged PR promising to free a slot).
  The board, the dashboard and the picker share one row-action layer (`internal/tui/rowacts.go`): a table
  of the row actions (key, menu label, hint word, name), what each screen knows of its row (`actRow`; a
  fact a screen does not know stays unknown and is left to the daemon), one predicate `actionRefusal`
  checked before anything is asked, and the question built from the same facts. The right-click menus,
  the card's ACTIONS and the key hints are generated from the table, so the rules exist once instead of in
  six copies. Refused at the key with a red flash: `x` on a pinned PR ("unpin first (u)") or one with no
  slot; `A`/`C` after the head moved ("review again first (r)": a screen cannot pass `--force`); `r`, `R`,
  `i` on a round running (the daemon queues no second one: "round in progress; K kills it"); a review of a
  PR closed without merging or merged with its head reviewed, now on the dashboard too; `I` on a merged PR
  or an ignored one; `K` with no review running, paused or waiting; `U` on a merged or closed PR unless a
  mute dismissed its merged-unreviewed flag (then it asks to restore it), and on a PR that is not muted;
  `M` on a muted one; `p` on a pinned PR and `u` on one that is not. `I` names what it will do (kill or drop
  the review when one runs or waits, mute, free the slot only when the PR holds one; a closed PR is only
  muted, which keeps it ignored if reopened), the dashboard reads a PR `magnum ignore` muted as `ignored`,
  so its `U` asks to stop ignoring it, and the picker's review question says a pinned PR is unpinned, as
  the board's does. A refusal in the picker names its own key ("unpin first (ctrl+p)"). Rejected: asking
  and letting the daemon refuse (its answer came after the flash, or never, for a release), and a
  row-filtered key-hint line (it would change under the cursor and defeat the frame cache).
- **`magnum abort` takes back a review that waits in line** (2026-10-05). Before, only `magnum ignore`
  removed a forced review once queued, and that mutes the PR for good; `mute` does not stop one (a forced
  round runs muted). `abort` (and `K`) on a queued or rereview_pending PR, forced or automatic, now drops
  the review before it starts: the forced mark goes, so do the fresh-sessions, dry-run and on-request-role
  marks of the request, and the PR returns to reviewed, or baseline when never reviewed (a merged PR's
  post-merge review: closed, released after a fresh close grace); no agent is interrupted, no session
  parked, the slot is kept, since nothing started and a person may be working in its panes. A `--as`
  identity switch stays (it is documented as lasting). A round paused mid-way is stopped like a running
  one. The `pr.aborted` event carries `queued: true`. Rejected: restoring the exact state before the
  request (the registry keeps no such history; reviewed or baseline is what an abort of a running round
  gives too).
- **`Again` is gone; `ctrl+r` refreshes the picker** (2026-10-05). The hidden `magnum review --again`,
  `tui.ReviewOpts.Again`, the picker's `ctrl+r` and the numbered prompt's `a` duplicated a plain review (a
  forced round reviews a reviewed head anyway) and asked "Review X again" even of a PR never reviewed.
  `ctrl+r` and F5 now refresh the picker's list, as on every screen (the entry above on keys), keeping the
  filter and the highlighted PR. The daemon still accepts `again` in a review payload and ignores it:
  `decode` refuses unknown fields, so a request an older CLI queued with `--again` would fail otherwise.
- **Fix hints name what works for the verb, and no positional argument is a placebo** (2026-10-05). A PR
  the daemon has not recorded yet got "fix: `magnum review <ref>` adds it now" from mute, ignore, pin,
  abort and approve alike; following it started a forced round. Now mute, ignore and pin say to wait for
  the next poll (`magnum kick` polls now) and run the command again, unmute, unpin, release and abort that
  there is nothing to undo, approve, request-changes, open and watch that magnum has not reviewed it and
  that `magnum review` would start a forced round. `magnum pause 2h` paused for good with the reason
  "2h": a word that parses as a duration is refused ("use --for 2h"). `kick poll|reconcile|schedule`
  echoed its argument back (every tick does all three): it takes none now, and the three names are still
  accepted with a note, since an installed copy of the herdr plugin runs `kick reconcile` at startup.
  `magnum slots pin|unpin <slot>` runs `magnum pin|unpin <slot>`: one path through the daemon, which also
  writes the `pr.pinned` event a review's unpin reply reads; with no daemon running the request waits for
  its start, as `pin` always did (before, `slots pin` pinned in process then). Rejected: pinning in process
  from both (a second writer of pins and their events beside the daemon).
- **The herdr picker and status popups keep a failure on screen** (2026-10-05). `magnum-ctl.sh` ran them
  with `exec`, and `pick` skipped its press-any-key for open and browser, so the popup closed with the
  error. The script now runs them as children and, after a failing exit (ctrl+c's 130 aside), waits for a
  key; `pick` itself waits only after a review, pin or release that worked, so a failure needs one key, not
  two.
- **A pin is a wait, not an error** (2026-10-07). A round behind a slot `magnum open` pinned read "slot"
  with the error mark: dispatch wrote "slot review1 is pinned (magnum unpin)" into `last_error`, and the
  operator asked what "retry, then slot" meant; a forced re-review waited 3h50m on such a pin unseen. The
  pin is the operator's own choice, so it is its own wait (`WaitPinned`: who pinned it and since when, from
  the newest pin event), with no `last_error` (an older daemon's pin error is cleared at the next dispatch):
  the cell reads `re-review · pinned → u`, the card "pinned by magnum open (u unpins)", and once a round has
  waited 30 minutes on the pin one toast per PR and pin (its key names the pin's time) says
  "talkable#N waits for your pin (magnum unpin …)". A pin still never lapses by itself and the round never
  takes another slot. A guard's persisted hold (a person's changes) stays an error. Rejected: unpinning
  after a while (the pin protects a person's work in the checkout).
- **A retry names its attempt and its cause** (2026-10-07). A PR whose setup failed 9 times in 27 minutes
  read `retry` throughout; the attempt was only on the card. The wait now carries the attempt to come and
  the attempts allowed (`retry 2/3`) and a word for the cause read from the failed attempt's error
  (`setup`, `judge lost`, `timeout`, `error`, `stopped`, `human active`, `agent busy`, `infra`), and a
  narrow STATE column drops the cause before anything else (`Wait.Narrow`). Rejected: the error's first
  line in the cell (a chain of contexts; the card and `magnum status <ref>` keep the sentence).
- **A mute has a reason and leaves the queue at once** (2026-10-07). 6 of 8 mutes came before any round
  and none said why; a PR muted while queued stayed `queued` 28 hours, until a restart's reclassification.
  `magnum mute <ref> [reason…]` sends the words after the PR as the request's `reason`, which the answer and
  a new `pr.muted` event keep; a mute of a PR waiting for an automatic round (queued or rereview_pending, not
  forced) moves it to ineligible (`muted`, the filters' reason) in a compare-and-set from those states, and
  `unmute` decides its eligibility again. A forced round still passes the mute. Rejected: a `--reason`
  flag (the words after the ref read as the reason, like a commit message).
- **Board keys promise only what the daemon does** (2026-10-07). `A` and `C` asked "magnum found no
  findings" while the FINDINGS cell said "✗ 3 open"; `x` was offered on running, paused and close-grace rows
  that cleanup's plan skips, `I` promised to free a pinned PR's slot that abort keeps, and `D` was offered on
  merged and closed rows. The verdict question now names the earlier findings still open ("no new findings;
  3 earlier findings still open"), `x` refuses those rows with the reason (`K` kills a round; the grace says
  when the slot goes), `I` leaves the slot out for a pinned PR, and `D` refuses a merged or closed PR. The
  board row carries the close grace's end (`ReleaseAfter`, `prs.release_after`, no migration).
- **`magnum prs` and `magnum status` say what the board says** (2026-10-07). Neither showed a snooze nor
  the "3 open" of a review that posted no new finding. The printed STATE adds `snoozed→18:00` while no
  round runs and FINDINGS `blocking 3 open`; a `magnum status` queue line adds `· snoozed → 18:00 · 3
  open` (`snoozed_until`, `open` in the JSON) and a PR's card its snooze on the state line. The board's
  needs-you pill no longer hides a PR that waits for a round, is paused or needs attention: those keep their
  state's pill (the card shows both). Rejected: a new column (the board stays one line per PR).

## Operations

- **The daemon exits when the schema was migrated under it** (another binary ran a newer migration);
  launchd restarts it on the new build. Rebuild and restart together.
- **Events and requests are pruned** (`keep_events` 30 d, `keep_requests` 7 d), keeping the rows the
  crash-resume logic still reads.
- **In-process slot work takes `state/ops.lock`**; a daemon that finds its own lock held and
  `ops.lock` held exits non-zero so launchd retries.
- **This repository is public.** Nothing private appears in tracked files or history; `make test` runs
  `check-private` with patterns kept only in the gitignored `.mise.local.toml`. Personal identities,
  watches and pools live in `config.local.toml`; `config.toml.example` is a complete worked setup.
- **Linux is a todo** (launchd, AppleScript reveal, `open`, `osascript` are the macOS-only parts); the
  code cross-compiles.
- **Subprocess transcripts log at debug, failures at warn** (2026-10-04). 87% of daemon log lines were
  `exec` lines of successful commands; execx now logs them at debug and a non-zero exit, failed start or
  timeout at warn, always with the redacted command line, when its logger takes levels
  (`execx.LevelLogger`). Rejected: dropping the transcript (it is what explains a failed git or gh call).
- **Doctor checks only what the configuration uses** (2026-10-04). MySQL is checked only when a `[[pool]]`
  declares databases; `mise exec` only with a pool; the login-shell shims only when mise is on PATH, a
  pool exists or the LaunchAgent starts the daemon through mise; otherwise they report SKIP. A new
  `launchd mise` check fails when the installed LaunchAgent's mise does not exist, because launchd then
  cannot start magnum at all. Rejected: unconditional checks (a second machine without DBngin or mise saw
  failures with fixes it did not need).
- **An infrastructure failure pauses dispatch once and is never charged to a PR** (2026-10-04, after every
  fetch failed with `Permission denied (publickey)` for ten hours and five PRs went to needs_attention with
  an error that read like theirs). A checkout whose error names a refused SSH key, DNS, a network timeout or
  refusal or TLS, or a dependency step failing with the same exit status and last stderr line on two PRs
  within ten minutes, sets `daemon.infra_paused_until`: no round starts, one urgent toast (key `infra`), the
  PR goes back in line without an attempt. When the wait ends a probe runs `git ls-remote origin HEAD` in the
  failing clone (else any watched clone) with gitx's environment; success lifts the pause, failure doubles
  the wait (2 to 30 minutes). `magnum resume` lifts it at once. Rejected: per-PR retries (they spent the
  attempts of every PR on the same outage) and a per-repository pause (an SSH agent or DNS failure is
  global).
- **The build that will run checks the configuration and the prompts first** (2026-10-04, after 138
  crash-loop restarts on an unknown config key and 14 runs that failed on template fields the running daemon
  lacked). `magnum config` validates and renders every prompt file the roles name with this binary's
  template data (twice: every field set, optional fields empty, so both sides of an `{{if}}` run);
  `daemon-restart` and `install` run `bin/magnum config` and refuse with its error; the daemon checks the
  same before it starts and writes the refusal to stderr (launchd.log) and daemon.log. Rejected: checking
  with the CLI that runs the command (it need not be the binary launchd starts) and parsing templates only
  (a parse cannot know which fields the data has).
- **Restarts can drain** (2026-10-04; 12 of 42 rounds were killed by restarts). `daemon-restart --drain`
  and `install --drain` set `daemon.draining` straight in the registry (no migration: the running daemon's
  schema may be older), so the daemon starts no round, wait until none is in flight (a line every 15 s, at
  most `--timeout`, 2 h), restart, and lift the flag; the new daemon lifts it too when it starts, and giving
  up or ctrl+c lifts it without restarting. `--now` still restarts at once (the dev loop). Rejected: a
  daemon that waits for its rounds on SIGTERM (launchd and the CLI would wait without a word).
- **Urgent toasts never block the loop; informational ones are batched** (2026-10-04, the engine half of
  the notifier change). Needs-you, attention, tool and budget pauses, identity health, token refresh, the
  identity leak, low disk and the infrastructure pause go through `ToastUrgent` in goroutines that shutdown
  waits for (10 s) and then cancels, releasing their dedupe keys. Posted reviews and new repositories share
  one batcher, so 14 repositories found at once are one toast. The tab-bar file starts with when it was
  written and its max age (three poll intervals); the command `magnum install` prints shows "magnum down"
  when it is older, while a plain `cat` still shows the status line. Rejected: the file's mtime (unchanged
  content is not rewritten, and `stat` flags differ between macOS and Linux).
- **Re-reviews honour a reply contract** (2026-10-04, after magnum left all 23 author replies unread and one
  "moot" answer stayed "still open" through two re-reviews without a reason). Before a re-review the pipeline
  reads the PR's review threads over GraphQL (`github.ReviewThreads`), keeps those the posting login started,
  classifies every other reply by its first words (`fixed`/`done`/`addressed`, `not a bug`/`by design`/
  `intended`, `won't fix`/`out of scope`/`follow-up`, a leading `(Claude)` skipped) and writes them to
  `review-threads.json` next to the reports; the prompt names the file and the counts, never the reply text,
  because replies are PR content. The skill accepts `fixed` only when the code at the head shows it, honours
  `not a bug` and `won't fix` with a reason unless the judge proves the reason wrong (then one sentence in that
  thread, posted after the review is read back), sends standing decisions to the repository notes, and lists
  each old finding as fixed, answered or still open with a one-line reason. A failed read leaves the judge to
  read the replies itself. Rejected: replies inside the prompt (PR text in a prompt), trusting resolved
  threads (the authors' tools resolve every thread they answer) and a model classifier (the judge checks every
  claim anyway; a deterministic parser only sorts them).
- **Finding provenance, then `magnum stats`** (2026-10-04; raw acceptance misled: Claude's 28% hid that it
  was the only source of 9 of 24 posted findings). The result file lists every finding the judge weighed under
  `provenance` with its sources (roles, `judge` for its own pass), its verdict and, for a rejection, one of
  seven reason codes. A posted round stores them in the `findings` table (migration 0005), replacing the
  run's rows; a result without provenance stores none. `magnum stats` reads only the registry: rounds by
  start day and repository with their outcome (the latest `round.end`, else the judge run's), finding counts
  from each posted result's `findings` (so older rounds count too), median and p90 per role over finished
  spans, per-source judged, posted, unique and rejected counts with reasons, and model switches, denies and
  round restarts from events. Rejected: acceptance from the `candidates` counts (they cannot show unique
  contributions) and a separate metrics store (the registry already holds every input).
- **Prompts are loaded once, at startup** (2026-10-04; supersedes "Prompts are files under `prompts/`, read
  at prompt time"). Reading them at prompt time let an edit reach the running daemon mid-round, and an edit
  meant for the next build failed renders until the restart. The daemon now loads every prompt its roles
  name, `model-fallback.md` and each judge's skill once, right after the start check rendered them, and
  renders that snapshot once more before the first tick (an edit landing in between makes it exit; the next
  start refuses with the check's error). Rounds render from the snapshot. The skill is a file the judge reads
  itself, so judges get a copy, `state/skill/<first 12 hex of its SHA-256>/SKILL.md` (copies no judge uses
  are pruned after a week). Every reconcile compares the files with the snapshot (size and mtime, then
  SHA-256) and records the changed ones (`daemon.prompts_changed`, an event when the set changes) for
  `magnum status`: "prompts: loaded <time>, N files changed on disk since". The CLI (`magnum config`,
  `magnum roles`, doctor) still reads the files on disk. A prompt edit therefore takes `daemon-restart`, as a
  new template field did already. Rejected: watching the files and reloading in place (a reload needs the
  same render check, and a half-written file would reach a round).
- **An App approval follows the head** (2026-10-04; in one repository that needs one approval and keeps
  approvals across pushes, 24 of 50 approved merges in 30 days landed commits newer than the approval). When
  a PR's last magnum review by an App identity (the configured App whose login posted it) approved it and
  the poller sees a head with commits that approval did not see (GitHub's compare of the approved commit and
  the head; a failed compare counts as new commits), magnum dismisses the approval as that App with
  "magnum: new commits since this approval; a re-review follows" before the re-review is queued, records
  `review.approval_dismissed` and the review's event as DISMISSED. A head without new commits keeps it
  (`review.approval_kept`); `keep_approvals = true` on the `[[repo]]` (or the `[[watch]]`) keeps them all;
  other logins' reviews are never touched; a 403 is reported once (`review.approval_dismiss_refused`) and not
  retried, other failures every poll. An approval posted on a head that moved during its round goes at the
  next poll. Rejected: relying on the re-review (a merge during the quiet period carries the stale approval,
  and a later COMMENT review does not withdraw an approval on GitHub).
- **Verification readiness runs before the reviewers** (2026-10-04; in 9 of 18 posted rounds the judge
  skipped or failed a check it wanted, and four Ruby-blocked rounds burned about 160 reviewer agent-minutes
  before the judge found the cause). `[[repo]]` and `[[pool]]` take `prepare` (commands such as
  `bin/rails db:test:prepare`), `ready` (probes, exit 0 = ready) and `ready_timeout` (5m, for all of them
  together); a `[[repo]]` key replaces the pool's. Before the reviewers of an initial, re-review or recovery
  round the pipeline runs them in the checkout as `zsh -lc <command>` with the slot's env (the login shell
  the agents' tools use), then checks that `ruby -v` there runs the Ruby the checkout pins (`.mise.toml`
  `[tools] ruby`, else `.ruby-version`, read through `os.Root`). A failure never stops the round. The results
  go to `readiness.json` next to the result file (with each command's last output line), to the judge
  prompt's `readiness` list (status and magnum's own reason only, because output is PR text) and to a
  `round.readiness` event; the skill has the judge read the list before any check and record failures under
  `environment_failures`. A restart on a newer head keeps the round's first result. Rejected: stopping the
  round on a failed prepare (most findings need no test run), and probes taken from the repository notes
  (the judge writes them from PR-influenced experience; magnum runs only configured commands).
- **A push of comments, whitespace or docs is not re-reviewed** (2026-10-04; one PR got a 16-minute re-review
  by three agents for a commit that only reworded YAML comments, and its judge said so). Before a re-review is
  queued, and when a review posts while its head moved during the judge's turn, the poller asks GitHub for the
  delta's patches in one compare call (`github.CompareFiles`) and `engine.TrivialDelta` classifies them: every
  file modified (not added, removed or renamed) with a complete patch, and every added or removed line blank,
  a whitespace-only change of its counterpart in the same run of lines (indentation counts in Python, YAML and
  Makefiles), or a comment of the file's type, or the file is documentation (`.md`, `.txt`, `.rst`, `.adoc`,
  `docs/`). Comments that change behaviour (a shebang, magic and build-directive comments) and code commented
  out or back in are not trivial; any doubt (a truncated patch, a failed call, an unknown file type) means a
  re-review. A trivial delta moves `reviewed_sha` to the new head with the last verdict, queues nothing, keeps
  an App's approval (it runs before the approval check) and records `pr.trivial_delta`; during a review the
  appended note reads "_Reviewed a7b3f8c; 1 commit arrived during the review (comments only), no re-review
  needed._". `[daemon] skip_trivial_deltas` (default comments, whitespace, docs; `[]` turns it off) and its
  `[[watch]]` override choose the classes; a forced `magnum review` always runs. Rejected: asking the judge to
  decide (it costs the round the check saves) and docs that drive behaviour (prompts written as Markdown) as a
  built-in exception (drop "docs" for such a watch instead).
- **Rounds magnum cut short are refunded** (2026-10-04; one PR reached its daily cap of six with a round that
  failed to render a prompt and one stopped by a restart, then sat seven hours through six pushes). The cap
  still counts rounds at their start, but `finish` takes one back (rounds_today down, last_round_started_at back
  to the round before, `round.refunded` with the reason) when it was stopped (shutdown, restart, drain, abort),
  failed before any of its runs was prompted, failed on a prompt that did not render, or ended because the
  judge's model and every fallback model were limited; an infrastructure failure after the count is refunded
  too. Posted, needs-attention, blocked, timed-out and other failed rounds count. Rejected: counting rounds at
  their end (a crash would then never count, and the interval needs the start).
- **Every waiting PR says why and until when** (2026-10-04; the PR above waited seven hours with no word
  anywhere). After each dispatch the engine writes `pr.<id>.wait` for every queued or re-review-pending PR: the
  retry backoff, else the timing rule that clears last (quiet period, burst quiet period, re-review interval,
  draft interval, daily cap with its count), else a mute, quiet hours, what holds every PR (a pause, a drain, an
  infrastructure pause, herdr down), else the reason the dispatcher gave (a paused kind, the budget's soft cap,
  the identity, the slot, capacity), else "next dispatch". The dashboard's queue, the board's state cell and the
  card's "Next review" show the compact form (`re-review · cap 6/6 → 00:00`); the card and `magnum status <ref>`
  show the sentence with what lifts it (`magnum review <ref>` for what a forced review overrides, `magnum
  resume` for pauses). Rejected: computing it in each screen (the screens do not know the throttle's push
  history or the dispatcher's last reason).

- **magnum fetches over HTTPS through gh, whatever the clone's origin** (2026-10-04). A clone whose
  origin is a github.com SSH URL is fetched from the HTTPS URL with gh as the only credential helper;
  refspecs name their destinations, so the same refs move. Reason: the user's SSH agent (a password
  manager) serves no keys while it is locked, and every daemon fetch then failed with "Permission
  denied (publickey)" for hours. gh's token is always available to the daemon. Rejected: depending
  on the SSH agent (and pinning its socket into the plist, which helps only while it is unlocked).
- **Reachability probes go where fetches go** (2026-10-04, after doctor kept failing its origin check
  with "Permission denied (publickey)" while the daemon fetched fine over HTTPS). Doctor's origin check
  and the infrastructure-pause probe run `git ls-remote` against the same remote the fetch uses (gitx
  `NetworkRemote`: a github.com SSH origin becomes its HTTPS URL with gh as the credential helper), so a
  locked SSH agent neither fails doctor nor keeps a network pause alive. The SSH hint (`ssh-add -l`)
  stays for SSH origins on other hosts. Rejected: probing origin as configured (it tested a path the
  daemon no longer uses).
- **Only the daemon migrates the registry while a daemon runs** (2026-10-04, after a `magnum status`
  from a fresh build migrated the registry to schema 5 under the draining older daemon, which exited on
  the schema check; launchd restarted it on the new build mid-way through a judge turn, preempting the
  drain). Every CLI command that opens the registry passes `store.Options.BeforeMigrate`: when the file
  is older than the build and the pidfile names a live magnum daemon (ps confirms it; a recycled pid
  does not count), the command fails before any migration and says to run `magnum daemon-restart
  --drain`, whose new daemon migrates on start. `daemon-restart --drain` itself never migrates (it
  writes one kv row through a plain connection). A registry already at the build's schema opens without
  the check. Rejected: read-only commands against the older schema (every view reads columns the
  migration adds; one clear refusal beats partial screens) and blocking until the daemon exits (the
  user would not know why the command hangs).
- **Evaluation replays are blind dry runs in a scratch tree, scored against a local corpus** (2026-10-04,
  backlog item 17: a human reviewer found two critical issues on a head where magnum's ten findings had
  none, and prompt or model changes were judged by opinion). `magnum eval run` replays each case of
  `eval.local.toml` (gitignored: it describes defects in real code; `eval.toml.example` is the format) at
  its pinned head through the engine's own round preparation and the pipeline, so the roles, kinds,
  identities, readiness probes and prompts on disk are the ones measured. Isolation: a scratch layout
  (`paths.Layout.Scratch`: registry, reports, a copy of the notes) under `state/eval/<run>/`, a detached
  worktree of the existing clone at the pinned commit (fetched into `refs/magnum/eval/*`, never the
  daemon's `refs/magnum/pr/*`), agents tagged `eval` (their names hash the tag, so a replay never adopts
  or collides with the PR's own agents; workspace `eval <repo>#<N>`), no toasts, no restarts, and the
  engine refuses `RunEval` on a layout without Scratch. Blindness: `blind: true` in the judge's block and
  a paragraph in the reviewer prompts forbid reading the PR's reviews, comments, CI and anything newer
  than the head, and make the judge take the local diff (GitHub's shows the latest head) and skip the
  open-state check (corpus PRs are usually merged). Scoring is deterministic: a defect is found when an
  inline comment of the planned review sits on its paths and lines and matches one of its regexps;
  unmatched inline comments are noise, simplification suggestions are counted apart; `eval score`
  re-applies a corrected corpus to saved results. Budget: a case costs a full round, so `run` refuses to
  start one past `[usage] codex_soft` (real reviews first) unless `--force`, and stops at a usage or
  login limit. Rejected: `magnum review --no-post` through the daemon (it reviews the live head, resumes
  the PR's own conversations, moves the original reports aside and lets the judge read the human
  findings), an LLM grader (a second opinion instead of a number), and committing the corpus.
- **An eval replay observes herdr itself** (2026-10-04, after the first baseline run waited out the
  40-minute timeouts of turns that had long ended and then failed on "claude-simplify still works after
  it was interrupted"). Rounds learn that a turn ended, that an interrupted agent went idle, and get
  their trust and permission prompts answered from the daemon tick's herdr observation; `RunEval` runs
  outside that loop, so it observes every 10 seconds from the first agent start to the round's end.
  Rejected: handing replays to the daemon (see the evaluation decision above).
- **Why a PR needs you is explained, not dumped** (2026-10-04, after a PR sat in needs_attention for a day
  showing only "…bundle install --jobs 4: mise -C … exited 1: mise by @jdx – installing 1 tool": the
  cause, a Ruby build failing on a missing jemalloc, was line 200 of the stored error). `attention.Explain`
  reads the stored error (and the kind the engine now records in `pr.<id>.attention`, inferred for older
  rows) into a stage, the attempts and head, the line of the command's output that names the cause (the
  first `error:`/`fatal:` line, else the first line naming a failure, with progress bars, hints and version
  banners dropped; known causes such as a refused SSH key or a tool mise cannot build get a specific
  sentence and fix), a one-line summary, the next step and the cleaned end of the output. The dashboard,
  `magnum status` and its card, `magnum attention`, the PR board's card ("NEEDS YOU"), `review --wait` and
  the toast all use it; the stored error is unchanged (JSON and `magnum logs` keep it whole). Rejected:
  storing a structured error at write time only (rows already parked would stay unreadable) and
  truncating the raw error smarter (the cause is usually at the end, the context at the start).
- **Branch PRs of people who left are not reviewed** (2026-10-04, after an old PR of a former member,
  forced by a stray key, failed three times on a Ruby the repository no longer uses). A watch's
  `skip_departed_authors` (default true) makes a PR from a branch of the repository ineligible when its
  author's GitHub `authorAssociation` is not OWNER, MEMBER or COLLABORATOR: pushing a branch needed
  write access, so an author without it now has left (checked against the organization's member list:
  every open PR's CONTRIBUTOR or FIRST_TIME_CONTRIBUTOR author was a former member, every MEMBER a
  member). The association comes with the poller's Details (migration 0006 stores it; one Details fetch
  backfills open PRs). Fork PRs keep `skip_cross_repository`, so outside contributors are unaffected
  when forks are reviewed. Rejected: an age limit (members' PRs from 2025 are still pushed to, and only
  pushes or requests queue a PR anyway) and the org member list (a paginated call per poll for what the
  association already says).
- **An ignored PR looks ignored, and unmute undoes it** (2026-10-04). On the PR board an ignored row is
  greyed (it is muted) with its title struck through and no separate "muted" tag; on that row `U`, the
  menu and the card say "unmute: stop ignoring" and ask "Unmute …: stop ignoring it and review it on its
  next push?", and `I` is not offered. `magnum unmute` already dropped the ignore mark and re-classified
  the PR. Rejected: an "unignore" command or key (not a word, and a second name for unmute).
- **No real person's GitHub login in tracked files** (2026-10-04, the maintainer: people who never agreed
  to be in a public repository's code). Tests and recorded fixtures use placeholders of the same length;
  `make check-private` also runs `scripts/check-logins.sh`, which fails on any human login from the local
  registry (PR authors, assignees, requested reviewers) except `MAGNUM_OWN_LOGINS`/`MAGNUM_LOGIN_ALLOW`,
  and skips without a registry. Rejected: a hand-kept list in `MAGNUM_PRIVATE_PATTERNS` (it misses the
  next new colleague).
- **A review's verdict is recorded apart from what it posts, and shown** (2026-10-04, the maintainer:
  "I should still see the decision even if it was not posted ... count of P1, P2, P3 and potential
  simplifications"). The judge's result file carries `verdict` (`blocking`, `non_blocking`, `clean`), its
  decision whatever `blocking_event`/`no_findings_event` or self-authorship let it post; results from before
  the field derive it from the counts (P0/P1 block, other or still-open findings comment, none is clean).
  `store.LastReviewSummaries` reads each PR's latest posted round (`runs.result_json`): the board's
  FINDINGS column and card, and `magnum status <ref>`, show the verdict, P0–P3, the simplifications
  suggested and the earlier findings. Rejected: inferring it from the posted event (a comment-only
  repository always says COMMENT).
- **The reviewer posts the verdict magnum could not** (2026-10-04). `magnum approve` / `request-changes`
  (board `A` / `C`, asked y/N with the findings recalled) post a body-only APPROVE or REQUEST_CHANGES on the
  reviewed head as the PR's posting identity, naming magnum's review and findings and carrying
  `<!-- magnum:verdict=… -->`; the daemon does it (one writer, the identities' tokens). It refuses an
  unreviewed PR, a round in flight, and a moved head unless `--force`. The review becomes the PR's
  latest (so an approval follows the head and is withdrawn on real new commits unless `keep_approvals`),
  and is marked manual (`pr.<id>.manual_verdict`): a later round never dismisses it as its own stale
  change request. Rejected: a forced re-run with an overridden event (an agent round for a decision the
  reviewer already made) and editing magnum's posted review (GitHub cannot change a review's event).
- **Open before magnum reads "not reviewed"** (2026-10-04). The `baseline` state keeps its name in the
  registry, filters and JSON; the board shows "not reviewed" and the card says the PR was open before
  magnum began watching and what starts a review; `magnum status` says the same. Rejected: renaming the
  state (every stored row, filter and test names it).
- **Settings live in ~/.config/magnum/config.toml over defaults built into the binary** (2026-10-04, to make
  magnum distributable). The committed defaults are `config.defaults.toml`, embedded in the binary (the root
  package `magnum`); the user's file is `$XDG_CONFIG_HOME/magnum/config.toml`, else
  `~/.config/magnum/config.toml` (`paths.Layout.UserConfig`), layered with the overlay rules
  `config.local.toml` had (blocks appended, keys and same-name roles overridden); `magnum init` writes it.
  `--config`/`$MAGNUM_CONFIG` still name a complete file that replaces the defaults (and tests use it); a
  `config.toml` or `config.local.toml` left in the checkout is still read when the new file is missing, and
  doctor says how to move it. `magnum config` and doctor list the sources read. The examples are
  `config.example.toml` (minimal) and `config.full.example.toml` (a worked setup). Still in the checkout:
  `state/`, the prompts, the judge skill and `.mise.local.toml` (next steps for a binary-only install).
  Rejected: reading the defaults from the checkout at run time (a distributed binary has none) and a
  `--config` that means the user file (it would change every test's meaning of a full config).
- **claude-simplify runs again after significant changes** (2026-10-04, the maintainer: simplification
  "could be enabled for the next review run if there were significant changes since the last
  simplification"). A `runs = "first"` role may set `rerun_min_lines` (claude-simplify: 150): when a round is
  prepared, the code lines changed since the head of the role's last completed run are measured with the
  re-review threshold's rules (comments, blank lines, whitespace moves and documentation do not count; one
  compare call as the poll identity), and at the threshold the role runs as if requested. Significant means
  new code to simplify: 150 lines is about two new functions, and a measure from the last simplification,
  not the last review, lets several small pushes add up. Rejected: rerunning on every re-review (the costly
  role on one-line fixes), a file count (a new 5-line file is not worth a pass) and the PR's total size
  (a large PR already simplified needs nothing new).
- **Icon modes for the screens** (2026-10-04, the maintainer uses a Nerd Font and asked for icons and emoji
  that make the screens easy to scan). `[terminal] icons = "unicode" | "nerd" | "ascii"` (default unicode,
  today's look; ascii what the ASCII mode was) reaches the PR board and the dashboard as their Icons option.
  Nerd mode puts a Nerd Font icon on every state pill, verdict and section heading and colour emoji where
  colour carries the meaning (state marks, verdicts, P0–P3, the daemon's health); every glyph is a `\u`
  escape named in a comment, none uses U+FE0F or a joiner, and a test renders every screen in every mode at
  40 to 200 columns with no line wider or taller than the screen. The mode is fixed per process, so the
  render caches need no key for it. Rejected: guessing a Nerd Font from TERM (no terminal reports its font)
  and emoji-only symbols (their widths vary; icons stay one cell).
- **CI on the board, gated by the checks GitHub requires** (2026-10-04, after surveying the 32 watched
  repositories). The radar reads each PR head's rollup state at no extra cost (through `headRef`, not
  `commits(last: 1)`, which made the radar 100 times dearer); a rollup change, or a head the stored checks do
  not describe, fetches the PR's Details, which list up to 100 checks with their workflow and time,
  deduplicated to the latest run per workflow and name (`store.CIStatus`, migration 0007). A CI change never
  counts as a push. Required checks come from the repository's rulesets (`rules/branches/<default>`, read
  with read access, cached 6 h; classic protection as a fallback) and `[[repo]] required_checks` overrides
  them (private repositories on a free plan answer 403); names are globs matched across workflows with the
  latest run winning (a dispatched `Completion` lands in another workflow's suite), `workflow:<glob>` takes
  a whole workflow. The board's CI column shows the required checks when there are some, else counts; a
  required check that never ran on the head is "not run", a skipped one "skipped", and neither is passed.
  Rejected: "all checks green" (optional pronto and audit checks fail often, legacy statuses stay pending),
  the rollup alone (it calls a draft whose checks all skipped SUCCESS, and in talkable/talkable most heads
  after the first push carry no CI at all) and polling each PR's checks (a call per PR per poll).
- **"Ready" means mergeable now, not once approved** (2026-10-04). The view keeps open, non-draft PRs with
  an approval of the current head (a stale one does not count), no change request (stale or not: GitHub
  keeps them), no blocking verdict from magnum's latest review, and every required check passed; a
  repository without required checks is not held back by its CI. Rejected: counting stale approvals (24 of
  50 approved merges landed commits newer than the approval).
- **Skipped PRs look skipped; labels can be badges; owners rotate** (2026-10-04). A PR the configuration
  skips is "skipped · bot/author/label/draft/fork/left org", dimmed without the strikethrough of `magnum
  ignore`, and a baseline PR the configuration skips becomes skipped at daemon start (back to baseline,
  not into the queue, when the configuration stops skipping it before any push). `[board] badges` maps a
  label to a badge (matched ignoring case and leading emoji: "Flagged" matches "🚩 Flagged"). `O` cycles the
  board's owner scope (all, then each owner with PRs, the default repository's first), kept across a trip
  to the dashboard. Rejected: hiding skipped PRs (they stay findable for a forced review) and sorting
  badged PRs first (the sort is the reviewer's choice; the badge and the count make them visible).
- **`h` hides ignored and skipped PRs; badges may be colored** (2026-10-04). `h` toggles hiding the rows
  `magnum ignore` muted and the configuration skips; the title says "N hidden (h)" so nothing disappears
  silently, and the choice is kept in the registry (`board.hide_skipped`). A `[board] badges` entry is a
  text or `{ text, color }` with the board's ANSI colors (red, green, yellow, blue, magenta, cyan, gray), so
  an icon such as the Nerd Font database for schema migrations reads like the yellow re-review pill.
  Rejected: ⌘⇧. (terminals keep ⌘ for themselves) and arbitrary hex colors (the board sticks to the 16
  ANSI colors so themes stay readable).
- **Codex's hooks review is declined, never trusted** (2026-10-04, after talkable#11483's re-review: the
  resumed judge showed "Hooks need review" because a terminal app had just rewritten the user's
  ~/.codex/hooks.json, the prompt went into the dialog, magnum took the idle agent for a judge that
  stopped, nudged it and parked the PR in needs_attention). The dialog lists hooks Codex has not trusted
  yet, from the user's ~/.codex/hooks.json or from a repository's .codex/ (which a PR controls), and
  does not say which, so magnum treats them all as untrusted. Declining applies to that session only
  and records no distrust; trusting the hooks once in one's own Codex (Codex keeps a trusted hash per
  hook in config.toml) stops the dialog for every later session, magnum's included. Before every prompt to a Codex agent, after a prompt herdr rejected as
  blocked, and when a Codex start stops at a dialog, magnum reads the screen; on the hooks review it moves
  the cursor to "Continue without trusting (hooks won't run)", confirms it got there, presses Enter, records
  `agents.hooks_declined` and waits for the agent to be idle before prompting. Rejected: trusting them
  (they run outside the sandbox and may come from the PR) and leaving the dialog for a human (every
  change to the hooks would stall the next round of every PR).
- **Codex's hooks review trusts the user's own hooks** (2026-10-04, supersedes "declined, never trusted"
  above: declining ran every magnum session without the hooks a terminal app had just installed in
  ~/.codex/hooks.json, and the user's answer was to trust them). The dialog does not name a hook's source,
  but Codex reads hooks from only three places: its home (hooks.json, config.toml), installed plugins
  (hooks/hooks.json) and the project's .codex/ layers (each directory from the checkout up to the
  repository root). magnum checks the checkout on disk, untracked files included (a previous round's agent
  may have written them): with no .codex/hooks.json and no hooks, plugin_hooks, plugins or marketplaces key
  in any .codex/config.toml there, every hook listed is the user's, so it picks "Trust all and continue"
  and records `agents.hooks_trusted`; otherwise, or when the checkout is unknown, missing or unreadable, it
  declines as before. Kind key `on_hooks_review` (`trust_own` default, `decline`). Rejected: opening
  "Review hooks" to read each hook's path (an unrecorded screen that may change between Codex releases;
  the filesystem answers the same question) and trusting unconditionally (a PR adding .codex/hooks.json
  would run its commands outside the sandbox).
- **Logins keep "[bot]": an App named like its owner is another account** (2026-10-04, after the
  user's own App, "zhuravel[bot]", started posting where the user "zhuravel" also reviews). GraphQL
  drops a bot's "[bot]" and magnum stored that form, comparing logins by name (`SameLogin`): the board
  showed the App's review as the user's, reviewer rows of the two would merge, the approval magnum
  follows on new commits could take the user's own approval for the App's (and pick the first of two
  installations of one App), and the "since review" base could start from the user's review. The
  registry now keeps every login as REST names the account (`github.Account`: a GraphQL login of
  __typename Bot gets "[bot]"), comparisons that decide anything use `SameAccount` (exact, case
  aside), and `SameLogin` stays only next to an explicit bot check. Migration 0008 takes
  last_review_login from the posting run's reviewer_login and fetches every open PR's Details once
  more. The board still counts both the user and magnum's identities as "mine" (★); the narrow columns
  mark a bot's login with 🤖 (Nerd Font robot, `[bot]` in ASCII) and the card prints it in full. The
  user's own PRs stay self-authored for the user's App (it comments, never approves its owner's PR).
- **Issue links come from the title, by URL template** (2026-10-04). `[board] trackers` lists URL
  templates with `{num}` after the issue key's prefix ("https://linear.app/example/issue/ENG-{num}"); the
  prefix before `{num}` is the key magnum looks for, so one string says both what to find and where it
  leads, for Jira, Linear or anything else that puts the key in the URL. The leftmost key of any template
  in the PR title wins (a title naming several issues opens the first), matched whole and case-sensitively
  (issue keys are upper-case; `XPS-1`, `PS-1a` and `ps-1` are not PS-1). The board's `t` opens it, the card
  shows it; no tracker API is called. Only the digits come from the PR: the URL is the configured
  template with them in place of `{num}`, so a title cannot steer where `t` leads. Rejected: reading the
  head branch or body (teams put the key in the title) and a regexp per tracker (the template already
  names the prefix).
- **Trackers are scoped like the other per-owner settings** (2026-10-04, amends the entry above: two
  organizations may both have a `PR-` project, one in Jira and one in Linear). `trackers` is also a
  `[[watch]]` and a `[[repo]]` key; a PR's trackers are its repository's, then its watch's, then
  `[board]`'s, and each issue key prefix comes from the most specific level that names it, the others
  falling through (a watch may move `PR-` to Linear and keep `[board]`'s `PS-`). Rejected: an
  owner filter on each `[board]` entry (a second way to say what `[[watch]]` already scopes) and
  replacing whole lists per level (one override would have to repeat every other prefix).
- **The Nerd Font mode draws verdicts and checks with gh-dash's icons** (2026-10-04, the user runs
  gh-dash next to the board). Approved, changes requested, commented and waiting are gh-dash v4.26's
  ApprovedIcon (U+F012C), ChangesRequestedIcon (U+EB43), CommentIcon (U+F27B) and WaitingIcon (U+E641);
  passed and failed checks and the flash marks its SuccessIcon (U+F058) and FailureIcon (U+F0159). They
  replace the ✅ ❌ 💬 ⏳ emoji, which drew the same states in another style than the CI column's icons,
  and take the verdict's color (an emoji brought its own). The colored circles for magnum's states
  and the finding priorities stay emoji: gh-dash has no counterpart, and they are what makes a row
  stand out.
- **Finding priorities are colored icons; a reviewing pill spins** (2026-10-04, amends the entry
  above: the user found the 🔥 🔴 🟠 ⚪ emoji drew too much attention). P0 is nf-oct-flame (U+F490),
  P1 to P3 nf-oct-dot_fill (U+F444), in the priority's color (red, red, yellow, dim), so a row shows its
  worst finding by color without four colour emoji shouting. The reviewing (claiming, verifying) state
  pill shows herdr-radar's eight-dot braille spinner (⣷ ⣯ ⣟ ⡿ ⢿ ⣻ ⣽ ⣾, MIT) instead of a still eye,
  eight frames a second, only while some PR in scope is in such a state: the frame chain ends with the
  last one, and the frame is part of the rows' cache key only then, so a still board keeps its cached
  frames. Badge counts in the summary and the card's badges take the badge's color, as rows did.
- **No colour emoji left in the Nerd Font mode's marks** (2026-10-04, amends the two entries above: the
  board's summary still led each state count with 🔴 🔵 🟢 ⚪). A PR state outside its pill (the board's
  summary, the status dashboard's slots, queue, pauses and attention lines, both legends) shows the
  pill's own icon in the state's color (the summary's reviewing count spins with the pills); a slot
  state and the daemon's status show nf-oct-dot_fill in theirs (prbPalette dots and slotDots). The
  help legend's pills lose their emoji prefix: the pill carries its icon. The dashboard's selected row
  draws its text without the marks' colors, since a color's reset would end the row's highlight.
- **An installed magnum keeps its files where XDG says; the checkout is for development** (2026-10-04,
  preparing a Homebrew formula: a binary in /opt/homebrew/bin has no checkout to keep `state/` in). The
  registry, reports and notes go to `$XDG_DATA_HOME/magnum`, logs, locks, the gh config dirs, the
  tab-bar file and the skill copies to `$XDG_STATE_HOME/magnum`, the user config, App keys
  (`private_key_file`) and the eval corpus to `~/.config/magnum`; the binary embeds the judge skill and
  the herdr plugin as it already embedded the prompts and defaults (`magnum install --plugin` writes the
  plugin out under the data dir, and a daemon start refreshes that copy). `MAGNUM_HOME` keeps the checkout
  layout for development, and a checkout whose `state/` still holds the registry keeps using it until
  `magnum migrate-home` moves it, so rebuilding never starts a live install over with an empty registry.
  migrate-home stops the daemon, checkpoints the registry's WAL (a read-only connection may have
  recreated -wal/-shm, which a list made earlier would miss and the registry would move without),
  renames every item (rolling back on a failure), links nothing back and removes the emptied `state/`:
  prompts carry `gh_config_dir` and the report paths every round, so a pane created before the move only
  keeps a stale `GH_CONFIG_DIR`, under which a bare `gh` fails ("not logged in") instead of posting as the
  user. launchd runs `magnum daemon` directly unless an App key still comes from a checkout's mise
  environment, and pins `MAGNUM_HOME` only for the checkout layout. Rejected: compatibility symlinks at
  the old paths (one-time leftovers nobody finds later) and copying instead of renaming (two registries).
- **Distribution: a source-built Homebrew formula in a personal tap, released from a local script**
  (2026-10-04; the repository stays github.com/zhuravel/magnum, private for now). `brew install
  zhuravel/tap/magnum` builds the tagged commit with Homebrew's Go: nothing to sign or notarize (a
  prebuilt binary in a cask would need an Apple Developer ID), and the formula's git URL works while the
  repository is private because git uses the user's credentials (a release-tarball URL would not).
  `make release VERSION=vX.Y.Z` (scripts/release.sh) runs the gate, tags, publishes the GitHub release and
  pushes the formula, filled from packaging/homebrew/magnum.rb, to the tap with the maintainer's own gh
  credentials. Rejected: a release workflow in Actions (it would need a token with write access to the
  tap stored as a secret, for a one-maintainer project) and `brew services` (it would fight `magnum
  install` over the launchd job; the formula's caveats say to run `magnum daemon-restart --drain` after an
  upgrade). CI runs the gate on macOS runners: magnum is a macOS tool and its tests run where it runs.
- **`magnum pause` holds automatic reviews, not the ones you ask for** (2026-10-05, after a review asked
  for from the board waited behind a pause without saying so). A pause is the user stopping the
  automation; `magnum review`, the board's r/R/i and the picker are the user asking for a round now, so
  a forced PR passes the pause (as it already passed the timing rules, quiet hours and the daily cap) and
  the request's answer no longer says it waits. PRs nobody asked for wait and `magnum status` says the
  pause holds them. A drain for a restart and an infrastructure pause still hold every round: the first
  waits for rounds to end, the second means a round cannot run.
- **The launchd job's PATH carries mise's shims; a prompt herdr rejects as agent_not_ready is sent
  again once herdr lists the agent** (2026-10-05). Running `magnum daemon` directly instead of through
  `mise exec` dropped the mise-installed CLIs from the daemon's PATH (`codex login status` failed with
  "executable file not found"), so launchd's PATH now lists `~/.local/share/mise/shims` (`$MISE_DATA_DIR`
  when set) after `~/.local/bin`. A freshly resumed Claude Code shows up as herdr's named agent a moment
  after the start returns: a prompt sent in between failed the reviewer's run ("is not an active named
  agent", five times in two days); herdr rejects it before sending, so Submit waits until the agent is
  listed idle (TrustReadyTimeout) and sends once more, as it does after a trust dialog.
- **The board shows when a review was last requested, and whether of you** (2026-10-05). An old PR whose
  author asks for a review again looked like any other old PR: the pending-reviewers list says who is
  asked, not when, and UPDATED moves with every comment. The poller now keeps the newest ten review
  requests of the PR's timeline (the events the daemon already reads for review-request rounds) in
  `prs.review_requests_json`: when, by whom, of whom (Account form, a team as `team:<slug>`). The board's
  REQUESTED column shows `★ 2h` for the latest request of you or a posting identity (the ★ means what it
  means everywhere), else the dimmed age of the latest request; the card has a line with the latest request
  to each reviewer; `--sort requested` puts the newest request first, and the one it orders by is the one
  the column shows, so the column reads in order and a PR nobody asked comes last. The summary is built
  where "mine" is known, in the board's row mapping, not in the registry. Migration 0009 sets `details_at`
  to NULL for open PRs: the next poll fetches every open PR's Details once, which backfills the history of
  the older PRs and holds each from dispatch until then, as for a new PR. Rejected: a column from
  `requestedReviewers` (no time), from GitHub's updatedAt (any activity moves it) and a timeline call per
  PR when the board opens (the board never asks GitHub). A request older than the newest ten events is not
  kept.
- **Triage of small diffs: a cheap model may drop reviewers, inside bounds magnum enforces** (2026-10-05, after
  one-line fixes got the full three-reviewer round). `[triage]` (off by default) asks a cheap model (Claude
  haiku through `claude -p`, or any CLI that takes a prompt on stdin) which reviewers the round's diff needs,
  after the round's roles are known (`pipeline.RolesToRun`) and before anything is preflighted or started, so
  a dropped role costs no login check, pane or agent. The diff is the PR's own text and the model reads it, so
  nothing depends on the model resisting what the diff says to it; the bounds are code: only a diff of at most
  `max_lines` changed lines (added plus deleted, 120) is asked about, so a big change always gets every
  reviewer; the judge always runs; the answer can only remove roles RolesToRun picked and only ones with a
  `summary` (a new optional `[[role]]` key, the one-line description the model is shown; a role without one
  is never offered), and a name that is no role of the round is ignored, while an answer naming none of the
  round's roles is treated as no answer; any failure (command missing, non-zero exit, timeout, no JSON
  object with a `"run"` list in the output, an incomplete diff, GitHub unreadable) runs every role and says
  why in a `round.triage` warning. A decision is a `round.triage` event naming the diff size, the roles that
  run and the ones skipped and the model's reason (one line, 120 runes, redacted). The prompt
  (`prompts/triage.md`, part of the startup snapshot) says the diff is data, not instructions, and carries
  neither the PR's title nor its body. The command runs without a terminal and in a private directory under
  `state/`, never the checkout: a model CLI reads the project settings, hooks and instruction files of its
  working directory, which are the PR's to write. Triage applies to first reviews and re-reviews only; a
  round that names roles (`magnum review --role`, or a `runs = "first"` role whose `rerun_min_lines` earned a
  rerun, which counts as named), a continued round and an eval run what they would have run. The diff is read
  from GitHub's comparison as the watch's poll identity (the call `rerun_min_lines` and the re-review
  threshold already make): the base branch against the target for a first review (GitHub's comparison is the
  merge-base diff the PR page shows), the reviewed commit against the target for a re-review (the whole PR
  when the reviewed commit is the target itself). The pipeline gets the reduced candidate list, so a skipped
  role is neither started nor mentioned to the judge, which already copes with absent reports; a skipped
  `runs = "first"` role has not run, so it is still due later. Rejected: letting the model add roles or skip
  the judge (the PR's text could talk its way out of a review, or into a costly one); a heuristic on paths
  (it cannot tell a typo in a string from a logic change); asking about every round (the saving is on the
  small ones, and a big diff costs real tokens to read); failing or delaying the round on an unreadable
  answer (a saved reviewer is not worth a late review). This is the one prompt that carries PR text and the
  first agent call that is not an interactive pane, both on purpose: the call is a classification, not a
  review (reviews still run in panes, top of this file); the default command gives the model no tools
  (`--tools ""`) and no session, and runs in that private directory, so the diff can sway only the answer,
  whose effect the bounds above limit. The prompt names what may skip every reviewer (a typo, a constant, a
  nil guard, test- or docs-only) and what must keep the bug-finding ones (authorization or tenant scoping,
  money, concurrency, data writes, schema, code outside the diff): a prompt that only said "the judge reads
  the diff itself" let haiku drop every reviewer from a change that removed tenant scoping, and one that
  said "when in doubt, keep it" kept them all for a one-line nil guard.
- **Learning loop: daily retro** (2026-10-05, stage 1 of learning from other reviewers: other people's comments
  on PRs magnum reviewed are the most direct evidence of what its review misses, and nothing collected them).
  `[learn]` (schedule off by default) runs a retro once a local day after `daily_at`, never while paused or
  draining; `magnum retro` (`ReqRetro`) forces one past `magnum pause` but not past a drain or an
  infrastructure pause. It looks at PRs merged or closed within `lookback` that have a posted review and no
  `retro_prs` row, or a failed one with fewer than three `attempts` (`--again`, and naming PRs with
  `magnum retro <ref>`, ignore the row), newest closed first, until `max_prs` had candidates. **What
  counts as a miss**: a root comment of a review thread or a review body by another reviewer (not the
  author, not one of magnum's logins, configured or posted-as, not a bot unless `include_bots`; replies never
  count in this stage), long enough once quotes and code blocks are removed and not just an approval, that
  an interactive classifier judges a real defect or risk in the reviewed change a careful reviewer should
  have reported; `not_issue`, `style` and `outside` are stored too, so a later stage can measure noise.
  **Why a comment on a later commit counts only when the file is unchanged**: a comment applies to code
  magnum saw. One made on a reviewed commit does; one made on a later commit applies to the newest commit
  magnum reviewed before the comment only when the comparison's status is `ahead` or `identical` (the
  later commit descends from the reviewed one: the comparison is three-dot, so for an older commit or a
  history a force push replaced it lists nothing that matters) and it lists neither the file nor more than
  its file cap, because then the commented lines are the ones magnum read; otherwise it is stored as
  `outside` without a model call, since a defect introduced after the review is not a miss and a classifier
  cannot be trusted to tell. A review body is kept only on a reviewed commit (it has no file to compare). A
  comment on deleted lines (`diffSide` LEFT) is numbered in the old file, so it keeps its hunk but no line
  and is never near a finding. **Why caught findings are dropped deterministically**: a comment within three lines of a
  finding magnum posted on the same path (the judge's provenance, `findings`) is the same point and is
  counted, not stored or sent to a model, so a model's opinion can never turn a caught defect into a miss
  and the classifier only sees what magnum did not post; one near a finding the judge rejected is kept as
  `raised = rejected` with the reason code, the case a later stage most wants to see. **The classifier is
  an interactive pane agent tagged `learn`**, consistent with "Reviews run in interactive herdr
  panes, never headless" (top of this file): one agent per retro (`[learn] kind`/`model`, Claude sonnet by
  default) in a herdr workspace "learn retro", driven by the eval machinery (a scratch registry under the
  run directory, removed at the end; an agents manager tagged so its agent never meets a PR's own; a scratch
  engine observing herdr for it, which the daemon's own observation never conflicts with, since each only
  reads the sessions of its own registry), working in `learn/retro/`, the directory of every run, because
  the CLIs trust the directory they start in and a path per run would add a trust entry every day; so trust dialogs,
  permission prompts (answered No), hooks review, turn completion and usage-limit pauses behave as in
  rounds; it is prompted once per PR with paths, the URL and the reviewed SHAs only (`prompts/retro.md`)
  and reads the comments from a file it is told is data. Its answer is a file Go validates (`internal/learn`):
  one item per candidate, a known class, and for a miss a severity, a title of at most 80 characters, a
  lesson, a scope, a line range and one to three compiling patterns; a missing or invalid file gets one
  nudge, then the PR is `failed` and the retro goes on; a failure is the PR's own, so the next retros try it
  again, three attempts in all. **A stop that is not the PR's records nothing**: a shutdown, a classifier
  that cannot start or went away, and a limit on its agent end the retro without a `retro_prs` row for the
  PR in flight, which stays due with the rest; a usage limit and a logout also pause the agent's CLI, as
  after a round, but a per-model limit and an overload do not (as in rounds, where an overload is the
  moment's, not the CLI's). An agent the retro could not park (busy at shutdown) is quit and its workspace
  closed, and one an earlier retro left behind (same tagged name) is ended before the next one starts
  rather than adopted; when an agent cannot be ended, the scratch registry that records it stays. A
  classification is never downgraded: a re-run whose classifier fails leaves the earlier one in place.
  **Lessons are scrubbed**: a lesson with a PR or issue reference, a URL, the PR author's or a reviewer's
  login (as a whole word, case-insensitively, with or without "@"), or an @mention of one of magnum's logins
  is dropped and the miss kept without it, and so is a `general` lesson that names the repository's owner or
  name, backticks or not, since general lessons can reach the public judge skill (a `repo` lesson stays in
  that repository's notes and may name it); each drop is an info event with its reason (`issue_ref`, `url`,
  `login`, `names_repo`), never the lesson. What later stages turn into notes and prompt changes teaches a
  principle instead of retelling a PR; titles stay as written, in the local registry only. The registry stores no
  comment text: `misses` keeps the URL, reviewer, place, reviewed commit and the verdict; the comments, the
  commented files (fetched at the reviewed commit, at most 512 KiB, written through `os.Root` because the
  paths come from GitHub) and the answer stay in `learn/retro/<run>/`, pruned after 30 days, and no event
  quotes a comment. Rejected: classifying with `claude -p` like triage (a retro is an investigation of
  files, not a one-line answer, and gets the pane's observability); matching other reviewers' comments
  to findings with a model (the deterministic nearness rule is cheap and auditable); retro-ing open PRs
  (comments keep arriving, and a PR is looked at once).
- **Recently merged and closed PRs stay on the board; one merged before its review is flagged** (2026-10-05,
  after a PR the user had asked magnum to review, forced and rereview_pending, merged before its round ran
  and nothing showed it). **Why a time window, not `--all`**: the board listed open PRs only, and `--all`
  adds every closed PR there ever was, which buries the one that just merged; `[board] recent_closed`
  (24h by default, `"0"` off) keeps the PRs GitHub merged or closed within it (merged_at, else closed_at)
  in their own section after the open ones, newest closed first whatever the sort, dimmed, `merged` or
  `closed` in the state cell. The window is computed at every load, so a PR leaves the section when it
  ages out; `--all` keeps its meaning and only the PRs inside the window form the section. The printed rows
  list them too, after the open ones. **Why the rule uses `prev_state`**: "merged unreviewed" must mean a PR
  magnum meant to review and did not, not every unreviewed merge. `prev_state` is the automation state the
  close confirmation left (`u.Copy("prev_state", "state")` in the engine's `confirmMissing`, kept through
  releasing and released), so the flag needs gh_state MERGED, a `prev_state` where a round was due or
  running (queued, rereview_pending, claiming, reviewing, verifying, paused, needs_attention:
  `store.DueStates`) and a head that is not `reviewed_sha` (or no review at all). Baseline, skipped and
  ignored PRs close in other states, reviewed ones had their head reviewed, and a muted PR waits for no
  round unless it was forced (as the dispatcher reads it), so none of them is flagged.
  `store.IsMergedUnreviewed` is the one rule: the board row, the status dashboard's ATTENTION line and the
  engine use it. **Why a toast**: the board and the dashboard show it only to someone looking; the miss is
  the user's to act on (review the merged change, tell the author), so when the engine confirms the
  close it writes a `pr.merged_unreviewed` warn event and sends one urgent toast through the notifier
  (deduped per PR, silent with `[herdr] notify = false` and in dry runs). Rejected: flagging every
  merge with an unreviewed head (baseline PRs and skipped bots would drown the signal), and a GitHub call
  when the board opens (the board reads only the registry).
- **A merged PR magnum missed gets a post-merge review, posted as a comment** (2026-10-05, for the PR
  of the entry above, merged before its forced re-review ran: GitHub still accepts comment-only reviews
  on a merged PR, and the author can follow up on what it finds). `magnum review` of a PR in gh_state MERGED (and `r`,
  `R`, `i` on its board row) runs the round instead of refusing; a PR closed without merging and a merged
  PR whose head is `reviewed_sha` are still refused, and so is one whose checkout is being released (state
  releasing). A closed PR needs no more: cleanup's apply now moves a PR closed → releasing before its
  first side effect (parking the sessions; a failed park moves it back), and the request moves it closed
  → claimable, both compare-and-set, so one of them loses before doing anything and a round never starts
  in a checkout the cleanup is handing back. **Why COMMENT only**: a verdict after
  the merge can block nothing, and how GitHub treats Approve and Request changes on a merged PR is not
  something to rely on; the round overrides `no_findings_event` and `blocking_event` with COMMENT
  in the judge's `<magnum>` block, which is what the skill reads (a live pane keeps the env it started
  with, so the `MAGNUM_*_EVENT` env is left alone), whatever the identity or `[[repo]]` says; a review
  GitHub verifies as anything else stands as posted, with a `pr.post_merge_event` warning; and it
  dismisses no earlier review (the pipeline's stale change request, a former identity's reviews): they
  are history now. The judge's block says `post_merge: true` (rendered only then, so the prompts of
  every other round are unchanged), and the skill's short section asks for the body prefix
  "**Post-merge review** <prev> → <head>:" and findings framed as follow-ups; the reviewers' prompts that
  name the PR say "The PR is already merged; review it anyway." (a `/code-review` of a closed PR would
  otherwise stop). **Why the commits magnum
  missed**: what was reviewed before the merge was reviewed then; the round is the usual re-review from
  `reviewed_sha` to the merged head (a first review when there is none), with the same sessions, triage
  and checkout of `refs/pull/N/head`, which GitHub keeps after a merge, in a pool slot or a per-PR
  worktree. **Why the base comes from the merge commit**: after a merge-commit merge `origin/<base>`
  holds the head, so its merge base with the head is the head and every reviewer's diff (codex-review's
  `--base`, simplify's `git diff <base>..HEAD`, the judge's `base_sha`) would be empty; the round
  reviews from the merge base of the head and the first parent of GitHub's `mergeCommit` (the merge,
  squash or last rebased commit, fetched by id when the clone lacks it), which is the base before the
  merge for all three merge methods, and falls back to the base tip the last Details fetch recorded
  (`base_sha`) when GitHub names none or names the head itself (a fast-forward). **Why no new state or column**: the round is post-merge whenever GitHub merged the PR, which
  the registry already says (gh_state, read at dispatch); the request moves the PR from closed or
  released to its claimable state, forced, `store.Candidates` takes a MERGED PR only when forced, and
  every end of the round (a verified review, a dry run, a failure that would park an open PR in
  needs_attention, an abort) puts it back in closed with a fresh `close_grace` (the release that
  follows is the normal one, and the panes stay that long for a look). prev_state keeps the state the PR
  closed in, so once the review is verified on the merged head `reviewed_sha` equals `head_sha` and
  the `merged · unreviewed` flag clears by its own rule. needs_attention was rejected for a failed
  post-merge round: closeGrace releases only closed PRs, so a merged PR there would hold its slot forever;
  it goes back to closed with `last_error`, a `pr.post_merge_failed` warning and a toast instead.
  Rejected: an approve or request-changes verdict after the merge, a review of the whole PR every time,
  and a `post_merge` column (gh_state already says it).
- **A push that only merges the base branch is not re-reviewed** (2026-10-05, after an author merged master
  into a reviewed 103-file PR: one merge commit bringing 12 master commits and no change to the PR's own
  code, which `reviewed...head` measured as 13 commits and 300 files, all master's, and queued a full
  re-review). **Why the PR's own diff and not `reviewed...head`**: a re-review is about what the PR
  changes, and the three-dot comparison of two commits of the PR counts whatever reached the branch in
  between, the base branch's work included. The PR's diff against its base, before (`<base>...reviewed`,
  whose merge base is the base commit the reviewed commit was built on) and after the push
  (`<base>...head`), is what the author and the reviewers look at; a file's own change is its sequence of
  added and removed lines, in order, without the context lines and `@@` headers that master's edits around
  it move. Every file unchanged (or in neither diff) is the new trivial class `base` ("base merge only",
  in `skip_trivial_deltas` by default; a user's explicit list replaces the default and so keeps it off);
  otherwise the re-review threshold measures only what changed in the PR's own diff (a multiset difference
  of the changed files' code lines; a file the PR now adds counts as an added file), so a conflict
  resolved inside the PR's code is re-reviewed at its own size, not at master's. **Why only on a merge
  commit or a divergence**: a plain push's `reviewed...head` already is the PR's own change, and two more
  compare calls on every push would triple the delta check's REST calls for nothing; `ComparePush` reads the
  first comparison with up to 100 commits (`per_page=100`, still one call) and looks for a commit with two
  parents, and GitHub's `diverged` status covers a rebase or a force push. A range of more than 100
  commits cannot rule a merge out and gets the check. A push that is already trivial by comments,
  whitespace or docs is settled without it. **Why an incomplete comparison falls back**: a file without a
  patch (binary, too large), or a diff at GitHub's 300-file cap, hides lines that may have changed, and
  a failed call says nothing; the push is then measured `reviewed...head` exactly as before, which can
  only re-review too often, never skip a change. Records measured before this rule carry no
  `DeltaRecord.Version`: once per daemon, after its first poll, every `rereview_pending` PR with such a
  record (not forced, no round running) goes through `settlePush` again, a compare-and-set from
  `rereview_pending`, and either settles or keeps the new measure and is timed again with it. Rejected:
  comparing tree contents at the merge base (needs the checkout, which the poller does not have), and
  judging the own-diff change with the comment and whitespace classes (the difference of two diffs is
  not a patch the classifier can read). Triage still reads `reviewed...target` for a re-review: the
  difference of two diffs is not a diff a model can be shown, a re-review after a base merge now runs only
  when the PR's own code changed, and a merge's comparison is mostly over `max_lines` or at the file cap,
  where triage runs every role, its safe default.
- **The board puts a PR on two lines when its columns do not fit on one** (2026-10-05, after the 13 columns,
  about 260 cells with whole titles, lost columns and cut titles to 18 cells on a narrower pane). **Why
  this split**: the first line says what the PR is (REPO, #, TITLE, AUTHOR, ASSIGNEE, UPDATED, REQUESTED),
  the second, indented four cells and dimmed, where it stands (STATE, LAST REVIEW, FINDINGS, CI, SINCE
  REVIEW, REVIEWERS); each line has its own heading line and computes its own widths with the same caps
  and content widths, so its columns still line up across rows and the title takes the first line's room.
  **Why this rule for auto**: one line while every column fits at its content width (up to its cap) with a
  40-cell title, measured on the content alone so a dragged width never flips the layout mid-drag; that
  is exactly where the one-line layout stopped dropping columns or cutting titles below 40, and above it
  the frame is byte for byte the old one (`testdata/board_*.golden`, drawn by the code before). A line
  that still does not fit drops its own columns (SINCE REVIEW, then LAST REVIEW; ASSIGNEE, then AUTHOR,
  then REQUESTED). **Why dim only the uncolored runs of the second line**: the state pill, the verdict
  glyphs and the check marks are what a glance looks for; the board's dim on everything would grey the
  red attention and merged-unreviewed pills like a muted row. `L` cycles auto, one line and two lines; the
  title bar names a layout other than auto, and the choice is kept in the registry (`board.layout`) like
  `board.hide_skipped`. A row stays one unit: j/k, page up and down and the wheel count rows, scrolling
  never shows half a row, the cursor's background covers both lines, a click on either line selects the
  PR, and each heading line sorts and resizes its own columns. The layout mode is in the rows' cache key
  and the lines per row in the frame's. Rejected: wrapping the title onto a second line (the columns
  still would not fit), a horizontal scroll (what scrolls off is never seen), and per-column visibility
  settings (one key covers the case).
- **Mute on a merged PR dismisses its merged-unreviewed flag, and the stale forced mark is cleared**
  (2026-10-05, after `M` on a recently merged row asked "stop automatic reviews of it?", which means nothing
  for a PR that gets none, and could not even clear the flag of a PR that was forced before it merged).
  **What the keys do**: on the board, the status dashboard and the right-click menu, `M` on a row merged
  before its last push was reviewed asks "Dismiss the merged-unreviewed flag on <ref>? (r still runs a
  post-merge review)" and sends the ordinary mute request on `y`; on a muted merged row that would be
  flagged unmuted (`store.IsFlagDismissed`, `BoardRow.FlagDismissed`) it asks "Restore the merged-unreviewed
  flag on <ref>?" and sends an unmute; on any other merged or closed row it asks nothing and flashes
  "<ref> is merged: nothing to mute" (or closed); an open PR keeps its questions. The card of a dismissed
  row says so in one dim line and offers `M` as the way back. **Why the daemon clears `forced`**:
  `IsMergedUnreviewed` flags a muted PR only when it is forced, which is the review the user asked for while
  the PR was open, and the mark outlives the close (a PR forced while queued closes, and is released, with
  `forced = 1`), so a mute alone left the flag up. The mute handler therefore also clears `forced` when
  GitHub's state of the PR is not OPEN, unless a post-merge round is due or running (queued,
  rereview_pending, claiming, reviewing, verifying, paused): the dispatcher takes a merged PR only while it
  is forced, so that round needs the mark. The clear is its own compare-and-set on the state the decision
  read, so a post-merge review requested in between keeps its mark. An unmute never sets `forced` again, so
  it brings the flag back. **Why not clear `forced` when the PR closes** (`confirmMissing`): for a PR muted
  and forced while open the mark is the only thing that flags it once it merges (a review the user asked for
  that the merge pre-empted), so clearing it there would change the flag's meaning; a reopen returns the PR
  to its previous state with the mark, which keeps the user's pending request; and for an unmuted PR the
  mark does nothing after the close (closed is not a claimable state, a post-merge review request sets it
  itself), so clearing it would change nothing anyone sees. The round-end paths already clear it (posted,
  dry run, failed, aborted). Rejected: a column for the dismissal (muted and not forced says it, and an
  unmute restores the flag for free), clearing `forced` on every mute (on an open PR it is a pending request),
  and keeping the old question on merged rows. Left as is: while a post-merge round is due or running the
  mute keeps the mark and the flag stays until the round ends (`K` aborts it, then `M` dismisses), and `U`
  still asks its old question on a merged row.
- **Config is refused when it would silently disable a guard or share a slot** (2026-10-05, after a review of
  the config and the registry found each of these loading clean). **Pools**: two `[[pool]]` blocks may not
  render the same `slot_name` or `slot_path` for any slot number up to their `max` (paths compared cleaned,
  `~` expanded). The registry keeps slot names unique, so a second pool copied from the README's `review{n}`
  was handed the first pool's `review1`, never grew, and said nothing. `rev{n}` against `rev1{n}` is caught
  too, at `rev11`, when both pools are big enough to reach it. **`[learn]`**: the role the retro runs as
  (`Config.LearnRole`) goes through the checks of a `[[role]]`, worded `learn: ...` (the prompt and the
  timeout keep their own messages, reported once), so a kind without `model` args fails the load as the same
  role would instead of failing every PR's retro three times. The default model is kind-aware: `sonnet` is
  `DefaultLearnModel("claude")`, other kinds get `""` (their `default_model`, else their CLI's own); a layer
  (the base file or the user config) that sets `learn.kind` and not `learn.model` gets the default of that
  kind, a layer that sets a model keeps it, so `kind = "codex"` no longer runs `codex --model sonnet`.
  **Daemon timings**: `poll_interval` and `reconcile_interval` must be positive, `close_grace`,
  `push_quiet_period`, `min_rereview_interval`, `draft_min_rereview_interval`, `agent_start_stagger`,
  `min_warm`, `human_cooldown`, `reviewer_timeout` and `judge_timeout` not negative (0 stays "none" or "use
  the fallback"), `min_free_disk_gb` not negative and `default_repo`, when set, `owner/name`. A reconcile
  interval of 0 had skipped every reconcile, the retention with it, and a negative `close_grace` released a
  closed PR's slot at once. With the load refusing them, the three `poll_interval <= 0` fallbacks in the
  engine are gone: one place decides what is valid. Rejected: keeping the fallbacks as a second line of
  defence (they hid the bad value, and a config the engine and the validator read differently is a bug
  waiting), and unlimited rendering for the pool check (a bound of 1000 slots per pool is far past any pool).
- **`config.Defaults()` equals `config.defaults.toml`** (2026-10-05). The Go literal said `terminal.app =
  "iTerm2"` and no icon set where the committed file says `"Terminal"` and `"unicode"`, so a `--config` base
  without `[terminal]` and every test that calls `Defaults()` ran on a configuration no installed binary
  has. The literal follows the file, and `TestGoDefaultsEqualTheEmbeddedDefaultsFile` walks every key the
  file sets in every plain-data section and fails on a difference, so the two cannot drift again. A key the
  file only mentions in a comment is the Go value by design (that is how a documented default is spelled)
  and is not compared; kinds and roles had a test already. Rejected: generating the Go literal from the file
  at start-up (the literal is what tests and tools read before any file is, and a parse of the embedded file
  on every `Defaults()` call costs more than the test).
- **`keep_events` prunes step rows too, once their subject is quiet** (2026-10-05). The retention kept the
  step rows of every subject's current generation for ever, because `StepDone` reads them to resume a
  sequence, but a subject carries the head's sha (`slot:review3:pr:11483:2e1a968`), so a finished checkout
  never comes back: 44% of the live registry's events were such rows, about 350 a day, against a default
  that promises 30 days. A step row, and the `step.reset` that opened its generation, is now kept past the
  window only while its subject has had an event of any kind inside it; a subject quiet for the whole
  window loses them with the rest of its history. A sequence that did run again after that would repeat
  its steps, which `package steps` already requires to be idempotent or prechecked (at-least-once), and
  each sequence in `internal/slots` opens with a reset of its generation anyway. Rejected: a separate, longer keep for step
  rows (one more key for rows nobody reads after the sequence ended), and deleting the rows of closed PRs
  only (the retention would need the PR's state, and a checkout's subject does not name its PR row).
- **Two indexes nobody used are dropped: `events_subject` and `prs_gh_updated`** (2026-10-05, migration
  0011). `EXPLAIN QUERY PLAN` on every query in `internal/store` chose `events_subject_id` for every events
  lookup (it also gives the order), and the board's and the dispatcher's orderings start with an expression
  (`gh_updated_at IS NULL`, `COALESCE(gh_updated_at, created_at)`), which no index on the column can serve.
  Each cost a write per row; `events_subject` was 188 KB of a 2.8 MB registry. The retention's new check
  ("an event of this subject inside the window") would have used `events_subject`, and does not need it: it
  reads the few rows of one subject through `events_subject_id`, and a test pins that plan. The migration is
  `DROP INDEX IF EXISTS` twice and rewrites no table. As with any new migration, the CLI cannot open the
  registry until the daemon runs the new build: `magnum daemon-restart --drain`.
- **`magnum prs --json` has its own shape** (2026-10-05). It marshalled `tui.PRBoardRow`, which has no json
  tags: PascalCase keys, unset times as `0001-01-01T00:00:00Z`, durations in nanoseconds, where every other
  `--json` is snake_case. The output is a contract and the screen's row type changes with the screen, so
  `internal/cli/prs_json.go` has its own types, a field for a field. Keys are snake_case at every depth; a
  time is RFC 3339 and left out while unset (`omitzero`); a duration is whole seconds under a key ending
  `_seconds` (`total_seconds`, `duration_seconds`), as `stats --json` writes them; a part a PR has none of
  (`last_review`, `ci`, `last_round`, ...) is `null` and a list is `[]`, so a consumer indexes without testing
  for a missing key; `findings.counts` stays `[p0, p1, p2, p3]` like the review summary's. A test fails when a
  field is added to the board row, or a type it holds, and not to the JSON. Rejected: tagging the TUI types
  (they would carry the contract into every screen change) and `omitempty` on strings (a key that comes and
  goes is the harder shape to consume).
- **`dismiss_own_stale_change_requests` is documented, its default unchanged** (2026-10-05). The key had no
  mention outside code comments: with a `gh` identity (default false) a stale REQUEST_CHANGES from the
  operator's own account survives the approving re-review and keeps blocking the author's merge, and the
  operator could not find the setting. It is now in `config.defaults.toml`, `config.full.example.toml` and
  the README, with what it does (a re-review with nothing blocking dismisses the identity's own earlier
  change request; never one posted by hand, never after the merge; a failed dismissal only warns; the
  old identity's after a watch moves) and that the default follows the kind: true for an app, false for a
  `gh` identity, whose reviews are the operator's own account's.
- **The hooks review is answered only at the bottom of the screen, and only after a fresh look** (2026-10-05,
  after a review of the dialog handling: the detector took the last "Hooks need review" title anywhere in the
  viewport, so dialog text an agent's output left above a real approval prompt, with the cursor on magnum's
  choice, would have had Enter pressed on "Yes, proceed", and with the cursor elsewhere arrows sent into the
  composer). The options must now be the last lines but for blanks and key hints, as for the trust and
  permission dialogs; a screen on which `detectPermissionPrompt` finds a prompt has no hooks review; and
  right before Enter magnum reads the screen again and presses it only on the same dialog (its lines from
  title to last option, the cursor left out) with the cursor on its choice. Keys go to the pane by id only
  when herdr does not know the agent's name (`agent_not_found`): after a timeout or any other error the keys
  may have arrived, and a second send would press them twice. Rejected: also refusing on the health
  classifier's blocked patterns (its "press enter to confirm" is a hint the hooks review may show itself).
- **A cancelled round stops its shell command** (2026-10-05, after both `busy` codex-review runs came 12 to
  19 s after a cancelled one in the same pane: the command kept running, the requeued round waited
  `IdleShellTimeout` against a p50 runtime of 8 minutes, and the judge posted without the most precise
  reviewer). A round cancelled for any reason but a push now sends ctrl+c to a shell role's pane through the
  same interrupt the push restart uses, on an uncancelled context. Rejected: waiting longer for an idle
  shell (the output belongs to a round nobody reads).
- **Replies are classified by their first clause** (2026-10-05, supersedes the "first words" of the reply
  contract above: 7 of 8 real replies came back `other`, among them "(Claude) Good catch, fixed in <sha>",
  "Incorrect — …" and "Noted — …", the style of the very tool the `(Claude)` prefix comes from, so the
  threads file and the prompt summary carried no signal). The first paragraph is split into clauses at
  sentence ends, commas, colons, semicolons and dashes; the first clause decides, after a leading `(Claude)`,
  `[Codex]` or like tag, a "but" and a "this is"/"it was". Fixed: fixed, done, addressed, applied, already
  addressed; not a bug: not a bug, incorrect, moot, by design, intended, intentional, "does not apply"; won't
  fix: won't fix, declined, out of scope, follow-up, deprioritized, kept or left as is. An acknowledgement
  ("Good catch", "Valid", "Analyzed", "Noted", "Low priority", "Thanks") hands the verdict to a later clause
  of the paragraph, so "Low priority — <why>. Kept as is." is won't fix while "Good catch, not fixed yet"
  and a bare "Noted." stay `other`. Rejected: keywords anywhere in the reply (an explanation mentions
  "fixed" in passing) and "low priority" or "noted" alone as won't fix (the verdict is what follows them).
- **A review posted but not verified is adopted, never posted twice** (2026-10-05, after a round's review
  was posted and its verification failed on a network error after three tries: the round ended in error, and
  the next round's judge, looking only for its own new marker, could post a second review on the same
  head). The judge run keeps the failure (state failed, error "pipeline: verify on GitHub: …").
  A round on the same head, by the same identity, with no verified review since, first looks on GitHub for
  that run's marker (or the review id its result file names, posted by the reviewer on the head after its
  prompt): found, the round adopts it before anything is prompted, the old judge runs become verified with
  the review, and the findings, duplicates, local paths and a stale CHANGES_REQUESTED are handled as after
  any post; the result carries the old round's number and marker. Not found, or GitHub still failing, the
  round runs and its judge's review carries the old marker, the way a continued turn quotes the paused run:
  the skill already lists the reviews for its marker right before posting, so a review that was there after
  all is found instead of doubled, and verification looks for the same marker. A restart on a newer head
  drops the old marker. Rejected: retrying verification for minutes inside the failed round (it holds the
  slot and still loses to a longer outage), a marker-less match for the adoption (a review the user posted
  under the same login since would pass for magnum's) and a new outcome for the engine (the run rows already
  say it).
- **claude-review's follow-up reports list no earlier findings** (2026-10-05, after the judge's rejections
  of claude-review on one PR went 13, 12, 13, 21, 34, 46, 55, 63, 75 over nine re-reviews: the re-review and
  restart prompts asked for an "Earlier findings" section with one line per finding of the previous report,
  which already had one, so every round carried every earlier list forward). The section is gone; the judge
  tracks earlier findings through its own threads and the reply contract. The reviewer is still told not to
  re-derive a finding the new commits leave unchanged. Rejected: carrying only the previous report's new
  findings that are still open (the reviewer cannot tell which the judge posted).
- **No stop prompt** (2026-10-05). `judge-stop.md` was never sent: no code path asked for `PromptStop`, and
  prompts/README.md said it served "PR closed or round cancelled", which interrupts handle with keys. The
  file, the prompt kind, `JudgeData.Reason` and its doctor check are gone. A role's `stop` key is still
  accepted and ignored, so a config that names `judge-stop.md` loads (an unknown key once crash-looped the
  daemon 138 times).
- **The radar reads no CI; a read by node id does, and a page GitHub cannot answer in time is asked again
  smaller** (2026-10-05, after the busiest owner's first radar page failed with HTTP 502 or 504 on 9% of
  the polls, 26% in the peak hour, from the day the head's rollup went into the radar). The rollup costs
  no point (`rateLimit(dryRun: true)`: 1 for a page of 100 repositories × 100 pull requests with or without
  it, 101 with `commits(last: 1)`), but GitHub computes it for every open pull request of every repository
  the page lists, watched or not: that page (96 repositories, 193 open pull requests) took 6.8 to 8.1 s
  without it and 8.6 to 10.9 s with it, against GitHub's 10 s, and one of three answered 502. **Why a read
  of its own**: a CI run that ends does not move a PR's `updatedAt`, so the board's CI column needs the
  rollup on every poll. `CIStates` asks `nodes(ids:)` for the head branch's tip and its rollup state, for
  the open same-repository PRs of the watched repositories only, 100 a call (the most `nodes` takes; 1 point
  whatever the size; 99 PRs answered in 1.3 to 1.5 s). A poll of an owner costs its radar pages (1 point
  each, as before the rollup) plus 1 point per 100 such PRs: 3 instead of 1 for the busiest owner, about
  240 points an hour more at a 30 s poll, and nothing else on a quiet tick. A read that fails leaves the
  rollups unknown (the stored CI stays, no Details are fetched for it) and the poll goes on. **Why retry
  at half size**: a 5xx, a GraphQL error naming a timeout or a stream GitHub cancelled is asked again once
  at half the page (the radar's repositories, a repository's further pull requests, a CI call; never below
  10), the pages after it in the same call keep the smaller size, and a second failure gives up for the
  tick; a network failure, a refused or a malformed query is not retried, it would fail the same way. gh's
  bare `gh: HTTP 502` line (over GitHub's HTML error page) now reads as status 502. **Why once an hour**:
  `poll.error` was logged on every change of the error and the daemon's "tick finished with errors" on
  every tick, so poll errors were over half of all warnings; each distinct error (TCP addresses and ports
  aside) of `poll.error`, the new `poll.ci_error` and the tick warning is now logged once an hour, even with
  good polls in between. Rejected: keeping the rollup in the radar with smaller pages (still computed for
  unwatched repositories, and 25 × 25 takes four pages for that owner), reading CI on a slower cadence (a
  re-run's result would show minutes late), and 50-repository radar pages by default (two of them took
  the same 7.7 s in all at a point more every tick; the retry covers the slow tail).
- **A finding sits on the defective line** (2026-10-05, after 25 of 58 findings posted since the "test as a
  `suggestion`" rule sat on spec lines, against 1 of 68 before, and "Apply suggestion" was used on none of
  17 PRs: authors' agents apply changes locally). An author reading the code diff saw no comment at the bug,
  and fixing the code never outdated the thread. The skill anchors every finding on the smallest changed
  line of the code that must change, never a test file; a test that proves it goes in the comment as a
  fenced block naming its file and line (`spec/…_spec.rb:42`), and a `suggestion` is only ever the code fix.
  Rejected: keeping the test-file suggestion for one-click adoption (nobody clicked).
- **One comment per simplification idea, each proven equivalent** (2026-10-05, after one PR got 12
  simplification comments for 7 ideas, all declined with 12 replies, and one posted simplification changed
  behaviour). Hunks that implement one idea, even far apart, are one comment: a `suggestion` at the first
  site and "Same change at L…" for the others, titled "**Simplification** (optional, no reply needed)".
  Each kept one needs an equivalence probe (a focused test, or a command running old and new code on the
  same inputs) listed in Checks; one without a probe is dropped. Rejected: a cap on simplifications (caps
  were rejected before; the probe and the merge cut the noise instead).
- **Three verdict lines** (2026-10-05, after bodies opened with "Approve.", "Blocking:" on a COMMENT review,
  "COMMENT — 8 P2 defects", "Changes needed:" on a P2-only review and process notes such as "This PR is not
  stacked."). A body opens with exactly one of `Blocking: N problem(s) must be fixed before merging.` (a P0
  or P1 among them), `Fix N problem(s) before merging.` (P2) or `No blocking problems.`, N counting the P0
  to P2 findings with the still-open earlier ones, the optional ones counted after it; a re-review puts
  `**Re-review a1b2c3d → d4e5f6a:**` first, a post-merge review says "in a follow-up". No GitHub event
  names and no process notes in the body; the event itself still follows `no_findings_event` and
  `blocking_event`. Rejected: naming the event in the body (GitHub shows it already, and an App that
  never approves posts COMMENT for a clean review and a P2 one alike, so the event says less than the line).
- **Re-reviews list what changed** (2026-10-05, after one PR relisted the same three threads in nine
  consecutive review bodies, rounds 13 and 14 opening with "0 are fixed, 0 answered, 12 remain open" and
  10 to 12 links). A re-review lists the earlier findings now fixed, now answered, or still open despite a
  new reply or commit, and the new findings; the unchanged open ones are one count with one link to the
  previous review, and the verdict still counts them. Rejected: dropping the unchanged ones altogether (the
  author would not see why the verdict still blocks). Not changed: what a forced round on an unchanged head
  posts.
- **A footer tells the author what the review is and how to answer** (2026-10-05, after no review said
  so: every review request went to the operator, never the App, authors' tools replied to every thread,
  and their replies did not use the words the classifier knows). `[[identity]] review_footer` is the
  last line of every review the identity posts, appended verbatim by the judge after the marker line (the
  marker is matched anywhere in the body, and magnum's own "arrived during the review" note is appended
  after everything). Without the key it is magnum's line (config.DefaultReviewFooter, documented word for
  word in config.defaults.toml and checked by a test): automated review, reply `fixed`, `not a bug:
  <why>` or `won't fix: <why>`, simplifications optional, new pushes re-reviewed automatically. `""`
  turns it off. It must be one line under 400 characters: it is a field of the judge's `<magnum>` block
  (`footer:`, rendered only when set). Per identity, so no name goes into the repository. Rejected: a
  fixed line in the skill (the skill is public and generic, while a footer may name a team or a channel).
- **The prompts say a reply's class comes from its first clause** (2026-10-05). The classifier reads the
  first clause past an acknowledgement ("Good catch, fixed in …", "Low priority — … Kept as is."), while
  judge-rereview.md, judge-recovery.md and the skill still said "first words"; they now say what the
  classifier does, the skill with an example of each class.
- **The notes steps and the readiness paragraph live in the skill only** (2026-10-05, after the same
  14-line notes procedure was in four judge prompts and the readiness paragraph in three, about 1.6 KB of
  every re-review prompt resent each round to a persistent session). The judge prompts pass `notes`,
  `notes_dir`, `notes_harness` (the listing, `(+N more)` past 40), `notes_lock` and `notes_unlock` as
  `<magnum>` fields, only when there are notes, beside the `readiness` list they already carried; the
  skill's section 2 holds the steps once. The startup render check fills every JudgeData field, these
  included, and goldens cover them. Rejected: keeping a one-line pointer in each prompt (the skill is
  loaded with the prompt's link anyway).
- **claude-review reports only what can be posted** (2026-10-05, after 3 of the candidates only
  claude-review raised were posted and 302 rejected: speculative 108, style_only 73, pre_existing 29). Its
  prompts no longer ask for "the ones you consider uncertain" and leave out style-only and pre-existing
  problems (not introduced or exposed by the PR), and its built-in `rereview_effort` is `medium`. Its first
  review stays at `high`; eval measures whether it still finds what only it found. Rejected: dropping
  claude-review from re-reviews (it was once the sole source of 9 of 24 posted findings).
- **The judge runs unattended** (2026-10-05, after four rounds ended blocked because a checkout's
  AGENTS.md said to stop and ask the user, and nine posted rounds logged a failed usage-probe MCP call from
  the operator's global agent instructions as a review-machine failure, which leaked into the notes). The
  skill never stops to ask or wait for a human, whatever an instruction file says, records what it cannot
  do under `environment_failures` and goes on, runs no usage or budget checks or unrelated MCP tools
  (magnum handles limits), and keeps such machine noise out of the notes.
- **The judge's final output is two lines and MAGNUM_RESULT** (2026-10-05). The skill asked for one final
  MAGNUM_RESULT line and then a ten-item final response nobody reads (magnum reads the result file, and
  the pane only for that line and health). Now the final response is at most two lines (the review URL or
  the blocker, and the counts), and `MAGNUM_RESULT <json>` is the very last line.
- **No APPROVE without every reviewer's report** (2026-10-05, after an APPROVE went out while claude-review
  had hit a usage limit; 5 of 35 posted rounds had no claude-review report). When a non-judge role of the
  round has no usable report, `pipeline.JudgeEvents` makes the round's `no_findings_event` COMMENT (the
  blocking event and a post-merge round are unchanged), the `round.judge` event says why, and the skill
  names each missing reviewer and its reason in Checks. A git-diff role that found nothing to change
  (`missing (no changes)`) did its job and does not count. Decided in code, not only in the skill, so a
  judge that misreads its reports cannot approve. Rejected: holding the round until the reviewer is back
  (a usage limit can last hours, and the findings the others made are worth posting now).
- **Requests are answered before the GitHub poll** (2026-10-05, after 11 of 14 requests that waited for
  their answer took 10.9 to 15.4 s: every tick, a kick's included, polled GitHub for about 12 s before it
  handled requests, so the CLI's 10 s wait ran out and the board flashed "queued" over a refusal). Every
  tick now handles the pending requests first, then polls, observes and applies the pauses, then handles
  the requests that arrived meanwhile. The CLI and the screens wait 30 s. Rejected: reordering only a
  tick a kick woke (the order is as safe for a timer tick, and one order is one code path).
- **One request client** (2026-10-05). The queue, wake, wait and print steps existed twice
  (`actDeps.submit/await` with `actRequestOutcome`, `inspSubmit/inspWaitRequest/inspPrintRequest`) and were
  copied into review, pin, verdict, pause and retro with different waits and wording. `reqClient`
  (`internal/cli/request_client.go`) is now the only way the CLI and the screens hand work to the daemon;
  it returns the request with its id, its answer or "still pending", the daemon's pid and the build skew,
  so a screen can keep following a pending request.
- **The daemon records its build; a request names what an older daemon lacks** (2026-10-05, after a daemon
  ran a build 70 minutes older than `bin/magnum` and refused a post-merge review the new build allowed, with
  nothing saying so). At startup the daemon writes `daemon.build`: the stamped version, Go's
  `vcs.revision`, the binary launchd starts and its mtime. `magnum status`, every request reply and the
  dashboard's data (`tui.DaemonInfo.Skew`, drawn by a later screen change) report "daemon runs <old>
  since <time>; <new> is built: `magnum daemon-restart`" when the CLI's version differs or that binary
  changed since. The daemon decodes payloads with `DisallowUnknownFields`: a field it does not know fails
  the request ("this daemon predates <field>: restart it") instead of being dropped (a dropped `dry_run`
  would post). A payload field, once added, is therefore never removed; an older daemon learns of it only
  by failing. Rejected: comparing only the revision (a dirty tree builds different code on one revision).
- **Restart when idle, by hand or by the daemon** (2026-10-05, after a restart from the herdr plugin failed
  without a word all day: rounds ran from 13:03 to 15:18, and `daemon-restart` without a flag refuses while
  one is in flight). `magnum daemon-restart --when-idle` waits, without a drain and holding nothing, until
  no round is claiming, reviewing or verifying, then restarts, and waits again when one started in between
  (at most `--timeout`, default 2h). Opt-in `[daemon] restart_on_new_build`: each reconcile looks at the
  binary launchd starts, a new one is checked once (`version`, then `config`, as `daemon-restart` checks a
  build) and, when it passes, the daemon exits non-zero at the first tick with no round in flight, as it
  does after a schema migration, so launchd starts the new build (`daemon.restart_pending`,
  `daemon.restarted_for_build`; `daemon.build_rejected` when the check fails). Dispatch never stops for it.
  It needs launchd: a daemon without `XPC_SERVICE_NAME` set to the job's label only records that a restart
  by hand is due. Rejected: re-executing the binary in place (it keeps the environment `mise exec` built
  for the old one, and a daemon in a terminal would change under its user), and draining for the restart
  (it holds the rounds people asked for).
- **A drain has an owner** (2026-10-05, after a drain stayed up with nothing running it: closing the
  terminal sends SIGHUP, which killed `daemon-restart --drain` before it lifted the drain, and every round
  was held until the next restart). `signalContext` catches SIGHUP like ctrl+c and SIGTERM, so the
  command lifts its drain. `daemon.draining` holds `{"since", "pid"}` (an older CLI's bare time still
  reads); each tick the daemon lifts a drain whose drainer is gone or whose pid now belongs to another
  program (`daemon.drain_lifted`); `magnum status` names the pid and how to lift the drain. The command
  records `daemon.drain_started` and, when it lifts its own drain, `daemon.drain_ended` (the restarted
  daemon writes its own). The fix of a refused restart names `--when-idle` and `--drain`. Rejected: an
  expiry (a drain may rightly last two hours; the pid says exactly whether its owner lives).
- **A request does not outlive the absence of a daemon** (2026-10-05, after a review queued while no daemon
  ran said "applies when the daemon starts" and would have posted whenever that was). At startup the daemon
  fails, as "expired: queued while no daemon ran", the pending requests older than an hour that were queued
  after the previous daemon's last tick. `review`, `approve`, `request-changes` and `open` of a parked PR
  refuse without a daemon and queue nothing (a request no daemon answered is withdrawn); `pin`, `mute`,
  `ignore` and the others still queue for the next start, and `release`, `pause`, `resume` and the slot
  commands keep their in-process paths. Rejected: expiring every pending request older than an hour (a heavy
  request the previous daemon was running when it stopped resumes in the next one).
- **The plugin's actions say how they ended** (2026-10-05). The restart action toasted only a success; it
  now runs `daemon-restart --when-idle` and toasts its last line, success or failure, and every other
  action that fails toasts its last stderr line (else its last output line). The build action says the
  new binary runs after a restart.
- **A review the operator asks for unpins its PR** (2026-10-05, after `magnum open` pinned a PR and a
  `magnum review` of it answered "queued", then waited behind the pin for hours: the reply, the board's
  question and the command named only the drain). `magnum review` (and the board's r, the picker) clears
  the PR's pin and its slot's (`slots.ClearPin`: the pin only), writes `pr.unpinned` and says "unpinned
  review1 (pinned by magnum open at 12:35) to review it", the origin being the PR's newest `pr.opened` or
  `pr.pinned` event (`magnum pin|unpin` and `magnum slots pin|unpin` now write `pr.pinned` and
  `pr.unpinned`). The slot's guard then runs: a persisted hold (dirty_worktree, head_drift,
  unpushed_commits) or a live one (a person's agent or process in the slot) is named in the reply
  ("waiting: slot review1 is held: …") and recorded as the PR's gate, so its wait names it; the round's
  checkout runs the guard again. The board's y/N question and `magnum review` say that the review unpins a
  pinned PR. Rejected: `Unpin` (it also clears the hold and takes a moved HEAD as the checked-out commit, so
  the round's checkout could move a person's commits away) and asking first ("pinned since you opened it:
  unpin and review?"): the request is explicit and the guards protect the person's work.
- **Requested rounds always toast** (2026-10-05, after six rounds the operator asked for posted in one day
  without a word because `toast_every_review` is off, while one sat behind a pin for two hours). A forced
  round (`magnum review`, the board, the picker, a post-merge review) toasts whatever `toast_every_review`
  says, which stays the switch for automatic rounds (a forced one is not toasted twice): when it posts
  ("talkable#2: CHANGES_REQUESTED (1 P1, 2 P2)", "Your requested review took 34m, posted as …"); when it
  fails and will retry (an error, a timeout, an overloaded API, a charged setup failure: why, and when it
  retries; needs_attention, a paused agent kind, the infrastructure pause and a failed post-merge round keep
  their own toasts); and once when it cannot start for a reason only the operator can lift and has waited
  2 minutes for it: a guard keeping its slot (pinned or held), an unhealthy identity, a paused agent kind,
  or a drain that has lasted 15 minutes. They go through the informational batch with a dedupe key per
  review, per head and attempt, or per PR and reason, so recovery and restarts do not repeat them; `[herdr]
  notify = false` silences them. Rejected: urgent toasts (these are news, not alarms, and a burst should be
  one summary) and a toast for every wait (the throttle, capacity and the slot queue lift by themselves).
- **A pause says how long and what it holds** (2026-10-05, after a pause without `--for` ran 19 hours while
  people's review requests waited on it and the tab bar said only "paused"). `magnum pause` records when it
  began (`daemon.paused_at`; a pause renewed keeps it, one an older build started takes its `daemon.paused`
  event's time), and every tick counts the review requests it holds (`daemon.paused_held`: waiting PRs
  nobody forced or muted whose review is requested from the operator on GitHub or whose newest handled
  request is still pending; a draft marked ready is no person's request). The tab bar shows "paused 19h · 6
  requests held"; `magnum status` and the dashboard "lunch since 21:04 (19h ago), for 2h until 23:04 (in
  1h) · 6 requests held" (`since` and `held` in its JSON); each held request toasts once per pause
  ("talkable#2: alice asked for your review", "magnum is paused (since 21:04); …"). The board's title is
  left for later; the registry has the data. Rejected: the kv row's own update time as the start (renewing
  the pause rewrites it).
- **The note on a review that commits outran promises only what will happen** (2026-10-05, after "re-review
  follows" was appended while magnum was paused and the next round ran 17 hours later, when forced). The
  note reads the PR's wait right after the review is recorded (`waitFor`): "re-review follows." when the
  next dispatch starts it, "re-review follows after the quiet period." when the push quiet period (or its
  burst form) holds it, and otherwise "N commits arrived during the review and are not reviewed yet.": the
  daily cap, the small-delta threshold, a pause, a drain or infrastructure pause, quiet hours (now or when
  the quiet period ends), a mute, or a paused agent kind of the watch's roles. The trivial note is unchanged
  and the line still ends the review body. Rejected: naming magnum's reason or a clock time on GitHub (the
  author reads it in another time zone, and the operator's state is not theirs), and promising the
  small-delta or capped re-review (it starts hours later, if no push comes first).
- **Two-line board rows removed** (2026-10-05, the same day they shipped). The operator tried the
  two-line layout and the `L` switcher on the live board and did not want them: the board is one line per
  PR again, and narrow screens drop columns in `prbDropOrder` as before. The registry key `board.layout`
  the switcher saved is ignored. The entry above ("Board: two-line rows") stays as the record of what was
  tried.
- **A push that merges the base branch is reviewed by the PR's own diff** (2026-10-05; the gate above
  measured such a push by the PR's own diff, but everything after it took the raw range: a push that merged
  master and changed 40 of the PR's lines went over triage's `max_lines` on master's files and ran every
  role, reran the simplify reviewer on master's lines, and its reviewers read `git diff previous..head`,
  which carries master's commits). One measure of a range from the reviewed commit (or a role's last run)
  serves the gate, triage and the rerun: the range itself, and, when it has a merge commit or diverged,
  the PR's own diff against its base before and after it. Triage then reads the files whose own change
  differs, each as its own diff after the push (a file the PR no longer changes as its earlier change
  reverted), and says so in its prompt (`.OwnDiff`); the rerun measures that change. The re-review round
  learns that the commits since the review have a merge commit (`RoundInput.BaseMerged`, again after a
  restart on a newer head) and the reviewer, restart and judge prompts say to compare `git diff
  <base>...<previous>` with `git diff <base>...<head>`, not `previous..head`; the judge's `<magnum>` block
  carries `base_merged: true`. Each of these is rendered only when true, and a rewritten history
  (`force_pushed`) still asks for the whole PR. Every comparison goes through a per-tick memo keyed by
  repository, range and kind of call, which a push comparison of the same range also answers (it carries
  Compare's stats): the poll's trivial-delta check, approval check and since-review size, and a review's
  delta check and its "arrived during the review" note, compare a range once; a failed call is never
  kept. Rejected: a local `git` check of the range (the measure would differ from the gate's, and the
  slot's clone may not have fetched the base branch).
- **Simplify's re-review and the recovery prompt read the PR's own diff after a base merge too**
  (2026-10-06). Two prompts still handed out `previous..head` after a push that merged the base branch:
  `claude-simplify.md` in re-review mode ran `git diff <previous>..HEAD` and gave it to its four subagents,
  master's commits included, and `judge-recovery.md` (a fresh judge rebuilding a re-review, the
  fresh-session delta check of a15cbb0 too) neither said the commits merged the base branch nor carried
  `base_merged`, which the engine never set for a recovery round. Simplify now hands out the PR's own diff
  (`git diff <base>..HEAD`) and keeps its proposals to what changed between `git diff <base>...<previous>`
  and `git diff <base>...<head>`; the recovery prompt says what `judge-rereview.md` says, renders
  `base_merged: true`, and calls a delta check's delta the change between the two own diffs, all only
  without a force push (which wins, as in the re-review); the engine measures `RoundInput.BaseMerged` for
  recovery rounds as for re-reviews, also after a restart on a newer head. `judge-continue.md`,
  `judge-nudge.md`, `model-fallback.md` (they continue a turn whose first prompt already said it) and
  `codex-review.sh` (the whole PR against the merge base) name no previous head and need nothing. SKILL.md
  is untouched: its re-review section (6) already covers `mode: recovery` and `base_merged`.
- **A head behind the reviewed commit is not a 0-line delta** (2026-10-05). A force push back to an
  ancestor of the reviewed commit makes GitHub call the range `behind`, with no commit and no file: it was
  measured as a complete delta of 0 lines, so `rereview_min_lines` held the re-review up to
  `rereview_max_wait` while the review still discussed code the push dropped. A range GitHub reports
  `behind`, or with no commit, now measures nothing, like a failed compare: the re-review follows the
  other timing rules. Rejected: comparing the other way round (head...reviewed lists what the push removed,
  but as the base branch would see it, not as the review's reader does).
- **A round whose judge turn continues is not refunded** (2026-10-05; a round a restart stopped during the
  judge's turn was refunded, then recovery paused it and its continue posted, so the posted round never
  counted against the cap and `last_round_started_at` went back to the round before). A round the daemon's
  shutdown stopped (not `magnum abort` or `ignore`) and a usage or login pause, once the judge was
  prompted, continue the judge's turn: the round counted at its start, is not refunded, and its continue
  counts nothing again, also when it becomes a full round (the judge's session was lost, or its checkout
  is gone). A round whose judge was not prompted is still refunded and its restart counts as a new round.
  Recovery's note says where the round was: before it started, before it prompted any agent, or while
  its reviewers ran. Rejected: refunding and counting the continue instead (its start would move the
  re-review interval to a time no round started, and a crash before the continue would never count it).
- **Doctor names the terminal's Automation permission and does not probe it** (2026-10-05; a comment said
  `magnum doctor` used `reveal.Revealer.Probe`, and nothing called it, so a missing permission showed only
  after the fact, when `magnum open` could only activate the terminal app). Probe cannot run from doctor
  without a window: its AppleScript (`tell application "iTerm"`) starts a terminal that is not running,
  and while the operator has not decided the permission macOS asks in a dialog that takes focus, which
  doctor must never do. The permission also belongs to the app that sends the events (the terminal doctor
  runs in, the LaunchAgent's program for the daemon), so a probe from doctor would not answer for
  `reveal_on_attention`. On macOS, a terminal magnum scripts with AppleScript (Terminal, iTerm2,
  Ghostty) gets a SKIP line with the fix (System Settings → Privacy & Security → Automation → allow it);
  WezTerm, a custom launcher and a generic terminal need no permission and get none. Probe stays
  uncalled. Rejected: checking `application "…" is running` first (the dialog remains), and asking
  `AEDeterminePermissionToAutomateTarget` without a prompt through a JavaScript-for-Automation bridge
  (untested here, and it still answers for doctor's terminal, not the daemon).
- **Claude sessions get `--effort` like `--model`** (2026-10-05). The claude kind passed a role's effort only
  inside the prompt (`/code-review <url> high`), so every Claude session ran at the operator's own Claude
  Code `effortLevel` (here `xhigh`, thinking always on), and a role without `model` ran whatever Claude
  Code picked: the default for a new session, the old model for a resumed one (a review parked on 10-03
  came back on Fable two days later, after every live session had moved to Opus). claude-review was the
  slowest role (median 14m, p90 26m). The kind now has `effort = ["--effort", "{effort}"]`; both flags are
  part of every launch and resume (`Kind.Argv`). Which model and effort a deployment uses stays config
  (`[[role]] model`, `effort`, `rereview_effort`), not a built-in default.
- **A screen action shows the daemon's answer, not "queued"** (2026-10-05, after a review of a merged PR
  flashed a green "queued" on the board and the daemon's refusal 11 s later reached nobody, and after a
  release, which never waits, read as done whatever the daemon then did). A screen action's result carries
  the requests it queued (`tui.ActionResult`; the request client reports each send to the screens through
  `actDeps.sent`). One the daemon has not answered when the action returns flashes as pending ("request 47
  (review) is queued; the daemon has not answered yet") and marks its row with ◷ (`?` in ASCII) in the cell
  after the cursor's; every refresh re-reads the pending requests (`DashboardActions.Requests`) and flashes
  the answer when it comes, then drops the mark. A failure, the action's own or the daemon's refusal, stays
  in red until a key is pressed (the key still acts); one wider than the footer is cut with "(! shows all)".
  A request the registry no longer has settles as failed. Rejected: waiting longer in the action (the wait
  is already 30 s and would hold every other action key), and toasts (the operator is looking at the
  screen; the daemon toasts requested rounds already).
- **The action log** (2026-10-05, after errors vanished from the footer after 5 s, cut to the width, with
  only their last line kept). `!` on the board and on the status dashboard (free on both, and on no row
  action) shows the last 20 outcomes, newest first: the time, the action, its requests with their ids and
  states and when the daemon answered, everything the action printed and the daemon's answers whole. An
  outcome with a request still pending is the last to go when the log is full. The two screens share one
  log (`tui.ActionLog`, created by `runInspScreens`), so `tab` keeps the outcomes and the requests followed,
  and a request queued on the dashboard (`talkable#7`) marks the board's row (`talkable/talkable#7`). It
  lives as long as the screens do. Rejected: keeping it in the registry (the requests table and `magnum
  logs request:N` already keep the history; the log is what this sitting did).
- **The titles say what holds the daemon back** (2026-10-05, after a daemon ran an older build for hours, a
  pause held people's review requests for 19 hours and a drain held a forced review for 94 minutes, with
  nothing on the board's title but the refresh time). The title line of the board and of the status
  dashboard carries, in yellow on its right, an older build ("daemon on v1.4.0 since 17:40 · v1.5.0 built:
  daemon-restart"; "new build Oct 5 18:48" when the binary on disk changed under the same version), a drain
  ("draining (pid 4242)"), the `magnum pause` ("paused 19h · 6 requests held") and the Codex pace (below).
  They are read from the registry with every refresh (`screenFacts`: the parts of `magnum status` that need
  no launchctl, herdr or inventory). On a narrow screen the board's title first drops its own words, then
  the facts take their short forms, then the refresh time goes (the spinner stays), then the least pressing
  fact; the dashboard's keeps its title, the age of its data and the scroll position. No line is added:
  the rows, the cursor's highlight and the frame caches are as before (the frame keys carry the facts as
  drawn, the board's row cache the rows marked pending). The dashboard's header cache no longer keys on the
  clock and the spinner, which moved to the title it draws with every frame. Rejected: a status line of
  its own (the operator wants the board one line per PR and the screen's lines for PRs).
- **The Codex budget has a pace** (2026-10-05, after the gauge read 50% used with 18% of the weekly window
  elapsed, 2.8 times the rate that lasts the window, and nothing would have said so before `codex_hard`
  stopped every Codex round for days). `usage.PaceOf` compares the share used with the share of the window
  elapsed and `Pace.Reach` projects, at the average rate since the window began, when a share is reached;
  a time after the reset is no projection. `magnum status`'s codex line adds "pace 2.8x: 80% Oct 7 13:30,
  95% Oct 8 09:10" (the caps still ahead and before the reset; "within the window" when neither is; no
  pace at or past the hard cap), and its JSON `pace`, `soft_at` and `hard_at`; the screens' titles say
  "codex 51% · at this pace 80% Tue 13:30" when a cap comes before the reset. The daemon sends one info
  toast and writes one `usage.pace` event per window when `codex_soft` would come before the reset; the
  window's reset time keys both in the registry, so a restart stays quiet, and nothing is judged in the
  window's first tenth, where one burst is no pace. Rejected: an urgent toast (nothing is paused yet) and
  a projection from the last hours only (it swings with every round).
- **The card says which roles a round ran and why, and what a PR costs** (2026-10-05, after triage's
  decisions existed only as events, a judge-only continue looked like a missing review, and one PR used 15
  rounds and a quarter of three days' agent time with nothing showing it). The board reads, with every
  load, the round events of the last 7 days (`engine.round_start`, and the `round.triage` and
  `round.rerun_role` the round's setup wrote before it) and the agent time of each PR's runs created in
  them (`store.AgentTimeSince`: a run counts from its submission to its end, to now while it goes; one
  query for every row). The card's LAST ROUND opens with the round's kind and roles ("continue (finishing an
  interrupted round): judge only"), what triage skipped and its reason (the model read the PR: its words are cleaned like any PR
  text), a role whose code changed enough to run again ("rerun added claude-simplify (212 lines
  changed)") and the roles asked for; its facts gain "Agent time 7d: 9h02m · 15 rounds". `magnum prs
  --json` carries both (`round_why`, `spend`), and `magnum stats` lists the top 10 PRs by agent time in
  its window with their rounds and share (`top_prs`). The events are read defensively: a field a later
  engine renames is left out, not an error, and a registry that cannot say leaves the card without them
  rather than the board without rows. Rejected: a column on the board (the operator keeps it one line per
  PR, and the spend is a reason to open the card, not to sort by).
- **No score before posting** (2026-10-05). Authors on one organization score every finding with a rubric
  (pros minus cons on an open scale, a negative net dropped) and decline the negative ones; the judge does
  not score its own findings that way before posting. Most declines rested on what only the author holds:
  production telemetry, decisions kept in their own tools' memory, a pending human security review. The
  judge's net would be a guess, and the author's tool scores anyway, so a second score adds nothing.
  Subtracting the fix's cost from the defect also turns "is this real" into "is this worth doing now" and
  parks what a reviewer exists to surface: low-probability real bugs, authorization gaps whose fix lands in
  code the PR does not touch, latent bugs that only today's ordering or data sizes hide, and input an
  attacker controls. The nets were not stable either: on one PR three negative scores were reversed and
  fixed after re-argument. The judge takes the rubric's principles instead (the next entries). Rejected: a
  numeric score per finding, and a threshold below which a proven finding is not posted.
- **Five facts, and reachability decides the priority** (2026-10-05; of 18 posted findings authors scored,
  12 were fixed, 1 kept open as valid, 4 marked low priority and 1 incorrect, and 36 of 45 posted findings
  on that organization were P2). A finding proves five facts instead of four: the exact trigger and who can
  produce it; the wrong result as a concrete consequence, never an adjective; how this PR causes it; how
  likely the trigger is here; a practical fix. Who produces the trigger, its preconditions and how far it
  fails (the triggering request, one account, every tenant) set the priority. A size, count or timing
  trigger states its threshold and why real data reaches it (one finding rested on a 2048-byte input where
  the longest real one was 179 bytes, another on a 4 MiB fixture against a real maximum of 10 KiB). A
  fixture far past realistic sizes, or a test double that allows what the real component forbids (a mock let
  reads finish out of order where the real host runs them in order), proves nothing. A reason the code, the
  PR or an earlier reply gives for deliberate behaviour is answered, or the finding is dropped. P2 now needs
  a path that real use or an attacker reaches with harm beyond the triggering request; a failure only
  crafted input or a stack of unlikely preconditions reaches, harming only that request, is P3.
  claude-review's initial and restart prompts ask for the exact trigger, how the PR causes it and any
  command run with its output, so a candidate without a trigger reads as speculative. Rejected: ranking by
  whether real users send the input (an attacker sends what no user does; how far the harm goes decides).
- **A comment leads with the trigger and ends with the smallest fix** (2026-10-05; 15 of 66 answered
  findings got pushback, often "low priority" or "moot", and authors' agents apply fixes locally: 43 replies
  opened with "fixed in" after an acknowledgement, and no commit came from "Apply suggestion"). The comment
  form names its parts: a title that states the wrong result; the trigger, who produces it and the
  consequence; the reproduction; **Fix** with the code cause and the smallest safe change; no "Why this
  matters" section. The word rules add a first sentence that stands alone and the exact result or change,
  never a category or advice. Before posting, the judge rereads every comment once to cut preamble, repeated
  context and vague words and to check that each finding keeps its trigger, result, reproduction and fix,
  since earlier drift (event names, process notes, mixed verdict openings) was fixed one rule at a time.
  Structure that can hide a defect (a silent fallback or cast over an unclear invariant, a copied helper
  that misses its edge cases, feature checks in a shared path, related writes left half-applied) is reported
  only with the input that goes wrong. Rejected: structure, file size or wrappers as findings of their own
  (taste is never a finding), and comments phrased as questions (the reply contract cannot classify the
  answers).
- **At most three simplifications, each removing something** (2026-10-05; amends "Simplify output is a list
  of optional suggestions" of 2026-10-04 and "One comment per simplification idea" of 2026-10-05, which both
  rejected a cap: 33 of the 33 simplifications that got a scored reply on one organization were declined,
  and one PR got 20 next to 5 findings, in code that records billable usage or enforces trust checks). The
  judge keeps a hunk only when it removes something a reader must hold (a branch, helper, mode, flag,
  duplicated block, allocation or control-flow trap), not when it only moves, renames or rephrases code, and
  the simplify prompt asks for deletions over neater rewrites. It drops a hunk that edits authorization,
  sandboxing, money or usage recording, or concurrency code unless the hunk removes a defect-prone
  construct: there, re-proving equivalence costs the author more than the lines save. A re-review keeps only
  hunks on lines changed since the previous review. It posts at most three, the most substantial, ordered by
  what they remove; the rest count as `dropped`. The diff-only scope and the equivalence probe were expected
  to keep the count down without a cap, and did not. Rejected: removing claude-simplify from the defaults (a
  deployment can leave it out of a watch's `roles`).
- **The review footer is its own paragraph** (2026-10-05). The skill said to append the footer "as the last
  line, after the marker"; the judge put it on the line right after `</details>` and the marker comment,
  and GitHub treats an HTML block as running until a blank line, so the footer's Markdown (the link, the
  italics) was posted as raw text. The skill now asks for a blank line before it.
- **The legacy `[codex]` and `[claude]` sections are gone** (2026-10-05). They predated `[kinds.*]` and
  `[[role]]` and only mapped onto them as fallbacks (`wrapper_mode` and `args` onto the kinds,
  `skill_path` and `review_args` onto codex-judge and codex-review, `effort` and `simplify` onto
  claude-review and claude-simplify); no known config uses them, and the public history starts after
  the kinds and roles replaced them. A config that still has one now fails to load with "unknown keys"
  instead of being mapped silently; the fix is the matching `[kinds.<name>]` key or `[[role]]` block.
  Rejected: keeping the fallbacks with a deprecation warning (nobody to warn, and two ways to set one
  key).
- **`magnum migrate-home` and the `config.local.toml` fallback are gone** (2026-10-05). Every known
  install has moved to the XDG places, and the checkout layout they migrated from predates the public
  history. Without `MAGNUM_HOME` the layout is now always the XDG one: a registry left in a checkout's
  `state/` (the binary's checkout or `~/Projects/magnum`) no longer selects the checkout layout, and the
  user layer is only `~/.config/magnum/config.toml` (`$XDG_CONFIG_HOME`): a `config.local.toml` next to the
  base file is not read, `magnum init` writes the user config even under `--config`, and doctor no longer
  offers to move one. Kept: `MAGNUM_HOME=<checkout>` (development, the checkout layout) and the checkout's
  own `config.toml` as the base when no `--config` names one, which the development and test layouts use.
  Rejected: keeping the command for a straggler (one would run it once from an older release).
- **One atomic file write** (2026-10-05). Ten packages wrote a file through a temporary file and a
  rename, each its own way: some under a fixed `<name>.tmp` that two writers share, some without an
  fsync. `fsx.WriteFileAtomic` (and `WriteFileAtomicIn` for a write confined to an `os.Root`) is the one
  copy: a temporary file with a unique name in the target's directory, its mode set exactly (whatever the
  umask), written and synced, renamed over the target, then the directory synced (best effort: the file
  is already in place). Every caller keeps its mode and its own directory creation. Not yet moved: the
  agents' CLI-config writer (`internal/agents/trust.go`).
- **Text is clipped one way** (2026-10-05). Five clippers disagreed by a rune: some kept n runes and added
  the ellipsis, some kept n-1. `textx.Clip(s, n)` is the one rule: at most n runes, the last an ellipsis
  when the text was cut, spaces before it dropped. Outputs that change: a readiness check's last line, the
  eval report's first lines of errors and findings, herdr's logged arguments (one rune shorter when cut),
  and a CLI cell or completion description cut right after a space (no space before the ellipsis).
  `textx.FirstLine` is the first non-blank line, trimmed, also for attention's explanations (which took
  the first line even when blank) and doctor's error lines (which kept its trailing spaces). The short
  SHA (7), plurals and the login fold (case, "@", "[bot]") have one copy each too; the 10- and 12-character
  SHAs and the folds that keep "@" or "[bot]" are other rules and stay.
- **`magnum open` names the Automation permission when macOS refuses its AppleScript** (2026-10-05).
  osascript's error -1743 ("Not authorized to send Apple events") meant the focus script could not run;
  `magnum open` then brought the terminal forward with `open -a` and reported "activated terminal app",
  which reads as a success while the herdr tab stays hidden. Reveal now returns a `reveal.AutomationError`
  instead, and `magnum open` (and the screens' open, and the daemon's reveal_on_attention log line) says
  "macOS did not let magnum script iTerm (the Automation permission, error -1743): System Settings →
  Privacy & Security → Automation → allow iTerm for the app that runs magnum". Any other focus failure
  still falls back to bringing the terminal forward.
- **A slot reloads its databases only when the checkout needs another schema** (2026-10-06). One
  `reset_db` rewrites every table (746 on Talkable, 0.3-0.5 GB of MySQL writes and minutes of round
  time), and it ran before every round of a slot marked `dirty_schema` (the flag stayed until the
  release), while the release loaded the base schema again for the next PR to replace. A slot now records
  the schema its databases carry (`schema_fp`, `schema_sha`, `schema_version`, migration 0012): the
  fingerprint of the files `schema_paths` match at the commit they were loaded from (`git diff-tree` of the
  empty tree, so the pathspec rules are those of the `dirty_schema` check), set after a reload passed and
  after provisioning (whose setup loads the checkout's schema, as the release trusted before), and
  cleared before a reload starts (one that stops half-way leaves it unknown). The readiness step reloads
  when the checkout's fingerprint differs, when none is recorded (a slot the previous binary released:
  one reload each), or when the development database's `MAX(schema_migrations.version)` is not the
  version the checkout's `db/schema.rb` declares (a migration run by hand or by an agent); otherwise it
  says "schema unchanged since <commit>: no reset" in a `slot.schema_unchanged` event and the readiness
  line and file. The release keeps the databases (`slot.schema_kept`) unless the development database
  drifted from the recorded version, the old check as a safety net, which then loads the base schema as
  before. `magnum open` hands a person databases that fit the checkout: when the free slot it reserves
  carries another schema it reloads first (through mise, the slot's log, 30m), so a released slot's last
  PR never shows through; a failed reload is reported in its answer and leaves the schema unknown. The
  PR's own claimed or held slot is left as it is (a person may have migrated it on purpose), and a
  removal changes nothing (its teardown drops the databases). `reset_db_on_schema_change = false` keeps the old release:
  the base schema at every release, no reload before the reviewers. Rejected: reloading the base schema
  lazily at the next claim (two reloads for a PR that needs another schema), and skipping only the reloads
  of the same PR's later rounds (the next PR with the same schema would still pay one).
- **A pool keeps a surplus free slot for a week** (2026-10-06). `idle_remove_after` was commented out in
  the built-in defaults, so a pool without it removed every free slot above `min` at the next reconcile
  (every 10 minutes), and the next demand provisioned one again: about 1 GB written per cycle. Unset or
  0 now means 168h (`config.DefaultIdleRemoveAfter`); a value in the user's config is kept.
- **Fewer small writes** (2026-10-06). `SetKV` leaves a key that already holds the value alone,
  `updated_at` included (the daemon sets the same gate, wait and pause keys every tick); nothing reads
  `kv.updated_at`, and a heartbeat such as `daemon.last_tick` carries its time in the value, so it is
  still written every tick. The `slot-*`, `provision-*` and `perpr-*` logs rotate like `daemon.log`: the
  write that would take one past 2 MB (`slots.SlotLogMax`) first moves it to `<name>.1`, replacing the
  previous one (a seed that prints its SQL filled several MB on every reset, and they were never cut).
- **Requests are answered between the poll's GitHub calls; pin and mute stay the daemon's** (2026-10-06).
  A pin or mute pressed while the daemon polled GitHub waited for the whole poll, 10 to 12 s with several
  watches (the kick cannot cut a poll short). Writing the flag from the CLI was considered and audited
  first: the store writes only the columns a caller sets, so no daemon path rewrites `pinned` or `muted`
  from a whole stale row, but a review request queued before a pin and answered after it unpins the PR
  and its slot (`unpinForReview`), and an ignore queued before an unmute mutes the PR again: a CLI write
  would invert the order of the person's own actions, and a compare-and-set on the flag cannot tell the
  new pin from the old one. So the daemon stays the only writer, in request order, and answers pending
  requests during the poll instead: before each GitHub call of the radar (its pages included), the CI
  rollups and the Details reads (`github.WithBetweenCalls` in the call's context), before each repository
  and before each PR it applies, the places where the poll holds no PR row it decides on afterwards (its
  snapshot serves GitHub fields only there). Not between a PR's upsert and its transitions, where a
  request could change the row the transitions read. A request now waits for one GitHub call; handlers
  get the poll's context without the hook, and a nested call does nothing.
- **The review footer's paragraph is checked after posting** (2026-10-06). Besides the skill's rule
  (above), a verified review whose body has the identity's footer glued to the line before it (no blank
  line, so GitHub posts its Markdown as raw text after `</details>` or a comment) is edited once to put
  the blank line in (`round.footer_fixed`), through the same author-checked edit `AppendToReview` makes.
  `AppendToReview` puts its note ("_Reviewed 06f72c2; 1 commit arrived …_") before the footer when the
  body ends with it, so the footer stays the last paragraph; a body without one gets the note last, as
  before. Both edits are idempotent.
- **A small re-review delta gets a judge-only check** (2026-10-06; a push during a judge's turn swapped two
  GIFs and changed 4 lines in two mail templates: the review approved the older commit, the approval went 30
  seconds later, and the 4 lines waited under `rereview_min_lines` (30) for `rereview_max_wait` (2h), then
  got a full round of every reviewer, 20 to 30 agent-minutes for 4 lines). A re-review whose delta since the
  reviewed commit has more than 0 and fewer than `rereview_min_lines` code lines and adds no file, measured
  as the gate measures it (the PR's own diff across a base merge, `measureRange`), now runs once the push
  quiet period (or the burst one) is over as a delta check (`eligibility.DeltaCheck`, `[daemon] delta_check`,
  default true, a `[[watch]]` may override it): only the judge runs, in its own session at its
  `rereview_effort`, with no triage call and no rerun, and `judge-rereview.md` gets `delta_check: true` and
  one instruction (review just the commits since the last review against the PR's purpose and the earlier
  findings, post one short review), rendered only then, so the other goldens stay as they were; `SKILL.md`
  is at its size cap and is not touched. The delta's files go to `delta-check.json` in the report directory,
  which the prompt names: file names are PR content. A modified file without a patch whose type is binary
  (an image, a font, an archive: `DeltaSize.Binaries`) counts 0 lines and is listed; it still makes the
  measurement incomplete for the threshold, so `delta_check = false` keeps today's behaviour exactly. Any
  other file without a patch (too large, a removed binary, a listing at GitHub's cap) and an added file mean
  a full round, as before. Pauses, the re-review interval, the daily cap, capacity and drains hold it like any
  round; a forced or requested round, one that names roles and a post-merge round run in full. The round
  measures its commits again after the checkout (a newer head that grew past the threshold runs in full,
  `round.delta_check_dropped`), never restarts on a push (its measure would be stale: the next round
  measures the new commits), and a judge without its session runs a full recovery. The board's state cell,
  the card's last round and `magnum status` call it a delta check (`delta check · quiet → 14:09`, `delta
  check (4 lines)`, also in `engine.round_start`; `pr.<id>.delta_check` holds the one in flight), and the
  note on a review whose head moved says "a short check of those commits follows" when one starts by
  itself. Rejected: waiting for `rereview_max_wait` first (the check is cheap; waiting is what made the
  case cost 2h), a separate run kind (the runs' kind column is a CHECK constraint; a migration for a label
  is not worth it), and counting every file without a patch as 0 lines (a text file too large to diff
  would pass as a short check).
- **An approval stands while a delta check of the commits since is due** (2026-10-06; same case: the author
  saw an approval appear and vanish 30 seconds later, for 4 lines). When the measured delta from an App's
  approved commit to the head qualifies for a delta check (whatever round then runs), `followApproval` does
  not dismiss the approval: it records it (`pr.<id>.approval_pending`, `review.approval_kept_for_check`)
  and asks GitHub nothing more. The review posted next settles it: an approval supersedes it
  (`review.approval_superseded`); a COMMENT or REQUEST_CHANGES dismisses it, and so does a round that fails
  or needs attention (a charged setup failure too), a push that makes the delta too large, and an hour
  after the first push it does not cover with no review posted (a pause, say), so an approval never covers
  unreviewed code for long. The dismissal is the existing one (`review.approval_dismissed`, recorded as
  DISMISSED while it is the PR's last review), its message "magnum: new commits since this approval; "
  plus the reason; one GitHub did not answer is tried again at the next poll. A delta above the threshold,
  an added file and `delta_check = false` keep the immediate dismissal. Rejected: keeping it until any
  later review (an approval of unreviewed code could stand for hours behind a pause), and dismissing it
  only after a failed check (the author would see the approval vanish late instead of early).
- **SINCE REVIEW counts the PR's own changes across a base merge** (2026-10-06; one live PR showed 25
  commits, 269 files, +13,307 −1,062 since its review, almost all of it master merged in). The board's
  SINCE REVIEW column and the card read `since_review_json`, which `refreshSinceReview` took from the raw
  comparison of the reviewed commit with the head, while the re-review gate, triage, the rerun and the
  prompts already took the PR's own diff. It now takes the shared measure (`measureRange`), served by the
  tick's comparisons, so a range the gate measured costs no call: a range with a merge commit or that
  diverged counts the files whose own change differs, the lines that change adds and removes (every
  line, comments too, as a size and not a threshold), and the PR's own commits since the review (the
  commits of base...head that base...reviewed does not have, by SHA from the comparisons, which
  `ComparePush` now lists; with more than 100, the difference of the totals), with `base_merged: true` and
  the base branch's name; the cell gets a merge mark (`⑂`, nerd `nf-oct-git_merge`, ASCII `m`) and the card
  says "(excluding a merge of master)". When the own diff cannot be compared in full, the raw counts stay,
  marked `raw` ("(raw: including a merge of master)"). The own-diff comparisons are push comparisons now
  (the same call, with up to 100 commits listed), and a size measured before this is measured again once
  (`store.SinceReviewVersion`). Rejected: a separate Compare for the size (a third call for a range the gate
  read), and counting only code lines (the column shows what changed, not what the threshold counts).
- **claude-simplify proposes instead of editing, and runs alongside the reviewers** (2026-10-06; one 187-line
  PR spent 15 minutes in the reviewers, then waited for the simplify, then for the judge). claude-simplify ran
  Claude Code's built-in `/simplify`, which edits files, so magnum saved `git diff` as `claude-simplify.patch`,
  reset the checkout, and ran it after both reviewers (`after`): 7 to 13 more minutes on every first review.
  Its prompt now keeps `/simplify`'s review (four subagents started together through the Agent tool, each
  given the diff and one angle: reuse, simplification, efficiency, altitude; one pass without the Agent tool,
  which the report says) and replaces its fix step with proposals: merged, skipped when they change intended
  behaviour, reach well outside the diff or are false positives (after looking for a smaller fix inside it),
  kept only on lines in scope (a re-review: changed since the previous review) when they remove something a
  reader must hold, are small enough for a suggestion block and leave authorization, sandboxing, money or
  usage-recording and concurrency code alone, ranked by what they remove, in `claude-simplify.md`
  (angle, lines, summary, cost removed, exact current and replacement lines, the other sites, why behaviour is
  unchanged with a probe), then a skipped list that accounts for every other finding. It edits nothing, so
  it has no `after` and runs in the reviewers' stage; `runs = "first"`, `rerun_min_lines` and a user's model
  and effort for it are unchanged, and the judge still proves each proposal with its own equivalence probe
  and posts at most three (SKILL.md reads the proposals instead of hunks, 8 bytes shorter). The `git-diff`
  capture had no other user and is gone with its patch collection, its per-role tree restore and its
  `empty`/`no changes` report status (`capture = "git-diff"` is now refused). In its place every stage ends
  with a check that HEAD and `git status` are what the stages found (after the readiness step, so a file it
  left modified is no role's edit); a role that changed them gets `round.checkout_dirty` and the checkout
  reset and switched back to the PR head before anything else runs, and a restart runs the same check once
  the roles a push cut short are settled, before it switches. A/B (the operator's condition: no worse than
  the built-in): three first-review rounds with a non-empty patch and posted simplifications, of two small
  repositories (23 to 127 files, 1.4k to 2.7k added lines), the new prompt run headless on the same head
  (`claude -p --model opus --effort medium`, the Agent tool used in every run, the checkout clean after each)
  and compared idea by idea with the old patch. Of the old patches' 18 ideas the final prompt proposed 7,
  listed 9 among the skipped (below the cut of six, or with a reason: one, a preload, it showed to be
  ineffective) and missed 2; of the 11 the judge had posted, 3 were proposals, 7 listed, 1 missed (a test
  helper in the 127-file diff that none of four runs found). Its other 11 proposals were new, mostly larger
  removals (six hand-copied table rows as one loop), which is why the smaller posted ideas fell below the
  cut. Three prompt revisions came from the misses: every hunk with small findings named and tests and
  styles included, a smaller in-scope fix looked for before a skip, and no finding dropped silently. A run
  took 15 to 16 minutes, about as long as the reviewers (16 to 21), so a first round's reviewer stage now
  ends when the slowest of them does instead of 7 to 13 minutes later. Rejected: running `/simplify` in
  parallel and keeping its patch (it edits the checkout the reviewers and the judge read), a second checkout
  for it (another worktree per round, and a pool slot's databases), and more than six proposals (the judge
  posts three, and a longer list only moved the ranking work to the judge).
- **A running round's stage and time on the board** (2026-10-06; with the simplify alongside the reviewers,
  "reviewing" said nothing about where a 30-minute round was). The state cell of a PR whose round runs names
  the role at work and the whole minutes since `last_round_started_at` (`⣷ reviewing simplify · 17m`,
  `reviewers · 9m` while several non-judge roles work, `judge · 31m`, `delta check · judge · 4m`, the time
  alone during the readiness step and between stages); the card's LAST ROUND lists each role's start, end and
  duration while the round runs, and the status dashboard's rounds line shows the same stage and time. The
  data comes only from the run rows of the current round (the PR's highest round minus runs created before
  the round started, the rule `magnum review` and the crash recovery use): two queries per board refresh
  whatever the number of rows, none when no round runs. Labels come from the configuration, the shortest of a
  role's name and aliases (claude-simplify is `simplify`), and the judge is `judge`, so roles stay data. The
  text changes at most once a minute; the board's rows key already carries the clock and the dashboard's
  header key gets the drawn stage and time, so a frame is redrawn when a round's minute changes and reused
  within it. The board stays one line per PR: on a narrow screen the stage gives way (after UPDATED, before CI
  in the drop order) and the time stays. Rejected: the time since a role started (the round's start is what
  the throttles and the operator count), and the stage from the panes' agent status (herdr is not read when
  the board draws, and the runs are the round's record).
- **The read-only simplify lists every qualifying proposal** (2026-10-06, the operator, after the A/B check). A
  cut at six, ranked by what a proposal removes, pushed the small ideas the old `/simplify` posted below the
  cut whenever a diff had bigger ones (7 of its 11 posted ideas in three replayed rounds). The judge already
  proves each proposal and posts at most three, so the cut only hid candidates from it. With every
  qualifying proposal listed, the new simplify covered 16 of the old 18 ideas (10 of the 11 posted) and
  added 11 of its own, most of them larger, in runs alongside the reviewers.
- **Triage answers with the listed names only** (2026-10-06). The triage prompt spoke of "the bug-finding
  reviewers" and "a reviewer of simplifications" as categories. Once claude-review was made a role triage
  may not drop, the list often held one reviewer (codex-review), and haiku answered with the categories
  (`["bug-finding-reviewers"]`, `["security", "authorization", "bug-finder"]`), which name no role: every
  triage since then logged "its answer names none of the round's roles" and ran every role (safe, but it
  saved nothing). The rules now speak of "a listed reviewer that looks for bugs" and the answer line spells
  out the exact allowed names; replayed against haiku, every answer used them. The fallback (an answer
  naming no role runs every role) stays.
- **A delta check whose judge lost its session runs with a fresh one** (2026-10-06, amends "A small re-review
  delta gets a judge-only check"; a 1-line push to a 533-line PR qualified for a delta check right after the
  PR's posting identity migrated to another App, so its judge session was parked, and the fallback ran
  claude-review, codex-review and the judge at xhigh for that one line). A delta check whose judge has no live
  or resumable session (a lost session, a resume that fails, an identity migration or change, fresh sessions
  requested) now runs with a fresh judge session instead of a full recovery round (`round.delta_check_fresh`,
  "delta check with a fresh judge session: <reason>"): the judge alone, with no triage and no reruns, started
  at its `rereview_effort` (a fresh judge otherwise starts at its full effort to re-read the history), in a
  recovery round whose `judge-recovery.md` gets the same `delta_check: true` and one instruction (only the
  commits since the last review, named by its short SHA, changed; read that review and its threads for
  context, then review just those changes, the rest stands as reviewed; post one short review), rendered only
  then, so the other goldens stay as they were (`SKILL.md` is at its size cap and is not touched). The recovery
  prompt already names the previous review, `threads_file` and `former_logins`, which is the context the old
  session held. The approval kept for the check follows the same rules. A fresh session needs a review to build
  on: when no review by the PR's current or former identities is on record (`previousReview` finds none, as
  after a review posted as another login), the round still runs in full and `round.delta_check_dropped` says
  so, as it does for a delta that grew past the threshold at the checkout; forced and requested rounds were
  never delta checks. Both delta-check prompts now say "1 line" for a one-line delta. Rejected: a dedicated
  prompt file (the recovery prompt carries the check in one conditional line and one field) and a new run kind
  (a recovery is what the round is: a fresh judge session reading its history).
- **The readiness step runs `reset_db` through `mise exec`, as the release does** (2026-10-06, amends
  "Verification readiness runs before the reviewers"; `bin/rails db:seed` exited 1 in every slot: it called
  Shopify with an empty host, though the command's own env had `FAKE_AWS=1`). The step ran every command as
  `zsh -lc <script>`, and a login zsh puts mise's shims first on `PATH`. When `bin/rails` started Ruby through
  a shim, mise applied the slot's `.mise.local.toml` `[env]` again, whose copied main-clone `FAKE_AWS = "0"`
  overrode the command's own variable. The release, the provisioning and `magnum open` always ran the same
  `reset_db` as `mise -C <slot> exec -- env K=V... /bin/sh -c <script>` (`slots.MiseExecArgs`), which puts the
  real Ruby first, so no shim runs and the command's env holds; those seeds passed. A `reset_db` readiness
  command now runs exactly so (`pipeline.Runner.Mise`, the slots manager's mise executable, set from
  `app.Options.Mise`, "" = `mise` on PATH; the env travels in the arguments, as `runHeavy` passes it), with
  the same budget, label, `Mutates` and scrubbed git environment. `prepare`, `ready` and the Ruby check stay
  `zsh -lc`: they must see what the agents' tools see, shims included. Rejected: re-exporting the pool's
  variables inside the login shell (the next variable a seed reads would be the next bug).
- **Magnum owns the review footer: templated, collapsed, and a clean verdict line** (2026-10-06, amends "A footer
  tells the author what the review is and how to answer" and "The review footer's paragraph is checked after
  posting"; the operator, on a clean review: the footer is noise on a review that found nothing, "simplifications
  are optional" is wrong on repositories where simplify never runs, and "No blocking problems." is dry). The judge
  no longer writes the footer: the `footer:` field is gone from every judge prompt and its instruction from the
  skill. Magnum renders it and appends it to the verified review (and to an adopted one) through the author-checked
  edit `AppendToReview` makes (`round.footer`; one more REST read and one edit per posted round). It is its own
  paragraph after a blank line and starts with `<!-- magnum:footer -->`, so editing the review again replaces
  everything from that marker on instead of adding a second footer (verifying it again changes nothing), and the
  notes `AppendToReview` adds go above it. The run marker stays where the judge put it; a dry run gets no footer.
  This replaces the safety net that gave a judge-written footer its blank line (`round.footer_fixed`, gone). A
  review whose judge wrote the old footer itself (a session that started before) is left as it is: no second footer,
  no rewrite; a note goes at its end. `[[identity]] review_footer` is now a Go `text/template` of
  `config.FooterData` (`.SHA`, `.Short` of 10 characters, `.Repo`, `.Number`, `.Login`, `.Simplify`: the PR's watch
  runs a role answering to the alias `simplify`; `.Clean`: the judge's result counts no finding of any priority, no
  earlier one still open and no simplification; `.Event`, `.PostMerge`, `.DeltaCheck`), at most 2,000 characters
  with line breaks allowed, that must render with every field set and `.Simplify` and `.Clean` both ways;
  `CheckPrompts` renders it too, so a broken template fails at start like a broken prompt; `""` still turns it off.
  The default names the reviewed commit and collapses the rest under `<details><summary>ℹ️ About Magnum</summary>`,
  as the Codex GitHub reviewer does, and says simplifications are optional only when the watch runs simplify. The
  skill's verdict line for a review with nothing at all (no finding of any priority, no simplification) is `No
  problems found. LGTM :shipit:`, for such a re-review `No new problems since <previous sha7>. LGTM :shipit:`, for a
  post-merge review `No problems found in the merged commits. :shipit:`; `No blocking problems.` stays for a review
  with only optional ones. SKILL.md went from 30,775 to 30,840 bytes under the unchanged cap (30,844), the footer's
  field and instruction and a stale "(but `no changes`)" making room. Rejected: a footer field the judge copies (a
  multi-line template rendered per review is magnum's data, and the judge already glued the one-line footer to
  `</details>`); `.Clean` from the result's `verdict` (`clean` there ignores simplifications).
- **Repository notes: triggers, history, usage and curation** (2026-10-06, amends "Repository notes" and "The
  judge merges the repository notes under a lock"; approved by the operator). The notes' rules were prompt text
  only, and they piled up: talkable's had grown to 26.5 KB, 81 lines, 36 of them over 300 characters, with 46
  harness scripts (232 KB), mostly one-off probes of single PRs each described in the notes, and machine-specific
  workarounds (a version manager's install path under a home directory). Four parts, below; the notes and the
  harness stay files the judge rewrites under its lock, and nothing here blocks or fails a review.
- **The notes limits are curation triggers, measured in code** (2026-10-06). `[notes]` `max_bytes` (16384),
  `max_line` (300 characters), `max_harness_files` (15) and `max_harness_bytes` (131072) are measured after each
  judge round and at every start; past any of them a `notes.over_limit` event names the limits and the
  repository is marked (`KVNotesOver`), back within all of them `notes.within_limits` clears the mark. Value
  matters, not size: the operator's revision made them triggers, not caps, so a curated proposal may stay above
  them when what it keeps helps future reviews, and the skill no longer says "at most about 80 lines". Rejected:
  caps that refuse a proposal (they would make the curator cut useful knowledge to fit) and a check that blocks
  the judge's write (a review must not fail over notes).
- **Every version of the notes and the harness is kept in the registry, forever** (2026-10-06, the operator:
  nothing lost, so a curation with a changed prompt can start from any earlier version or the whole history).
  Migration 0013 adds `notes_versions` (the source: judge, curation, human or import; the round's PR and run;
  the notes text gzip-compressed, NULL when an earlier version of the repository holds the same text),
  `harness_blobs` (each file body once per content, gzip) and `notes_version_files` (a version's files by path).
  A version is recorded after every judge round that changed the notes or the harness (the round snapshots
  both into its report directory right before the judge is prompted, and a state that differs from the
  snapshot after the round is the judge's; a continued turn keeps its round's snapshot), after an applied
  curation, after `magnum notes --edit` and an applied restore (human), and as an import of a state magnum
  finds on disk without a record of it, at every start (the first start imports every repository) and every
  reconcile (skipping a repository whose round is in its judge stage, which records its own). An unchanged
  state is not recorded again, and a `notes.changed` event counts the lines and harness files a version adds
  and removes without quoting them. `--log` and `--diff [N]` read this history; `--restore <version>`
  proposes a version back through the same review as a curation. No retention applies to these tables, nor
  to `notes_proposals`, `notes_files` and `notes_usage`: the prune step deletes old events and handled
  requests only (`store.Prune`), and the findings, the retro's misses and the judges' stored results
  (`runs.result_json`) are never pruned either (checked: no statement deletes them; `RecordFindings` only
  replaces one run's rows when the same result is recorded again). Rejected: a `.history/` directory of
  files (a second store to prune and back up, and `--diff` would need the files anyway) and keeping only the
  last versions (a later curation could not see what an earlier one removed).
- **The judge reports the harness files it used** (2026-10-06). The result file gains `harness_used` (SKILL.md
  section 8; a list of names relative to `notes_dir`, read leniently), and a reviewer report that names a
  harness file by its path counts as a use too. `notes_files` counts the judge rounds each file existed for and
  `notes_usage` the runs that used it; a file that existed for 20 rounds of its repository without a use is a
  curation candidate, which `magnum notes <repo>` lists and the curator reads. SKILL.md stays within its size
  cap: the sentence and the JSON field replace words in section 2 (30,840 to 30,839 bytes, after the footer change).
- **The notes keep what helps a future review** (2026-10-06, the operator). The pile-up started at the source:
  judges saved one PR's probes in the shared harness and described them in the notes. The skill's notes
  procedure (section 2) now says the content is only what helps review a future PR, never one PR's findings,
  code or probes, and that a probe for one PR stays in the round's report directory (beside `result_file`)
  while only a general script a future PR would run goes in `notes_dir`, named in the notes.
- **A curator proposes curated notes; the operator applies or rejects** (2026-10-06). With `[notes] curate =
  "over_limit"` (the default) a marked repository is curated once its notes changed since its last curation
  began or was applied, at most once a day; `"weekly"` also curates every repository with notes once a week;
  `"off"` only on `magnum notes <repo> --curate` (`ReqNotesCurate`). One curation runs at a time, none while a
  proposal of the repository waits or a round of it is in its judge stage (a PR of it reviewing whose current
  round has a judge run), and an attempt that stored nothing (an agent down, a limit) waits an hour. The
  curation copies the notes and the harness under the notes lock into a scratch directory
  (`notes/.curate/<owner>/<repo>/<run>/`, pruned after 30 days) and drives the retro's pane machinery, now a
  `paneAgent` with a `paneSpec` per kind (a "learn notes" workspace, the `[notes]` role, tagged `learn`): its
  prompt (`prompts/notes-curate.md`, registered and render-checked like the retro's) carries paths, the limits
  and a usage file only. The curator's test is value per future PR: every kept section and harness file in
  `changes.json` carries a one-line reason saying how it helps review another PR of the repository, and one
  PR's content is removed or merged into parameterized scripts. Magnum validates the proposal
  (`notes.Validate`): reasons for every kept item and every current harness file accounted for, every proposed
  harness file named in the notes, plain files only, no pull request reference, branch of the repository's
  PRs or probe file (a name with "probe" or a PR number), no secret (`execx.Redact` would change it) and no
  home directory path; size is not checked. An invalid proposal gets one nudge naming its problems, then is
  stored as invalid with them. A valid one is stored as pending (its state as a version of source curation,
  outside the history until applied), toasted once and counted in the board's and dashboard's titles; `magnum
  notes <repo> --review` shows a colored diff, the harness changes with reasons and the sizes, and asks y/N:
  `y` applies it under the notes lock after reading the live notes again (refused, and the proposal expired,
  when they changed since its base), `n` rejects it with `--reason`, which the next curation reads, anything
  else leaves it waiting; a proposal not reviewed within 7 days expires. Every proposal stays in
  `notes_proposals` with its base, its proposed version, `changes.json`, its state and reason, and the
  curator's model and prompt hash. Rejected: applying a proposal without review (the curator deletes scripts)
  and a new runner for the curator (the retro's pane agent already handles trust, permissions, turns and
  health).
- **An empty or binary file no longer makes a PR's own diff incomplete, and a raw size counts the PR's own
  commits** (2026-10-06; a stacked PR, whose base branch is another feature branch, merged that branch after its
  review, and the board said 44 commits and 300+ files since the review where GitHub's commits tab listed a few).
  GitHub sends no patch for an empty file (two `.keep` files the PR added) nor for a binary one, and the client
  marked every file without a patch `Truncated`, so the comparison of the PR's own diff before and after the push
  gave up and the size fell back to reviewed...head: the base branch's commits, and GitHub's 300-file cap. A file
  GitHub lists without a patch, with no line added or removed and with its blob (`sha`, which GitHub may send as
  null), in a listing below the cap is now complete, its blob in `FileDelta.BlobSHA`; one whose lines GitHub
  counted (a patch too large to send), one without a blob and every file of a listing at the cap stay truncated.
  The own-diff comparison tells such a file by its status, previous path and blob; a changed one counts as
  `MeasureDelta` counts it in a push (an added one as an added file, a modified binary in `Binaries`, any other
  unread, the last two leaving the size incomplete), so the threshold never holds back a change it cannot count;
  triage and the trivial-delta check already read a file with an empty patch as unread. When the own diff is
  still incomplete but both sides were read, the commits are the PR's own (the SHAs of base...head that
  base...reviewed lacks: only the commit lists are needed) and the files and lines stay raw (`raw`). This counts
  for the since-review size and for the note a review gets when commits arrive during it, which now takes the
  shared measure (`measureRange`, the gate's comparisons of the tick) instead of its own compare: a merge of
  master during the review is "2 commits arrived", not master's 13. Where the own count finds none (GitHub listed
  only part of a range's commits and the difference of the totals stands in), the push's count stays, so a range
  with commits never shows none. Sizes stored by the old rule are measured again once (`store.SinceReviewVersion`
  2; the per-repository compare budget spreads the calls); the gate's delta records are not (`deltaRecordVersion`
  unchanged: their raw size errs towards a re-review). Unchanged: the trivial-delta event of a base merge ("only
  merges master (13 commits, ...)") names the commits the merge brought, and an approval's dismissal event still
  counts reviewed...head's commits. Rejected: reading every file without a patch as complete (a patch too large
  to send would read as no change), and comparing such files by path alone (a replaced image would not count).
- **The judge posts through `magnum post-review`** (2026-10-06). Every round, the judge wrote its own Python script
  to build the review JSON, check its inline lines against the diff, look for its marker, POST, read the review back
  and count its comments. On one round that took 2 of the judge's 15 minutes, and a wrong line cost a 422 and a
  retry. Now the `<magnum>` block carries `post_review`: the daemon's own binary (`paths.Layout.Binary`, so the line
  outlives a Homebrew upgrade), running `magnum post-review` with every fact of the run as a flag. The flags are the
  repository, PR, head, run id, reviewer login and former logins, the identity's gh config directory, and
  `--dry-run` in a dry run. The judge writes `review.json` (event, body, comments) into the report directory and runs
  the line. The command checks the file: the event, the bodies and GitHub's 65,536-character limit, the sides,
  `start_line` before `line` on one side, and no footer marker. It appends the run marker when the body lacks it.
  It checks every inline comment against the PR's diff as GitHub shows it: the patches of `pulls/{n}/files`, both
  sides, context lines included, a multi-line comment within one hunk. A file without a patch is checked against
  `git diff` from the merge base of `base.sha` and the head in the current directory. A line nothing can check is
  kept, and GitHub decides. Next it looks for a review by the reviewer login or a former login that already carries
  the marker (all pages). Then it posts once, the JSON on gh's stdin (`github.Client.SubmitReview`). A 422 refusing a
  verdict on the identity's own PR is retried once as `COMMENT`. Another 4xx is `rejected` after a second look for
  the marker. An unclear failure counts as posted only when a review carrying the marker turns up. Last it reads the
  review back: author, commit, state, marker, comment count over all pages (`ReviewComments` now pages). It prints
  one JSON object with a status. Exit 2 means the judge fixes its file: each comment off the diff comes with the
  valid ranges of its file on that side. The command loads no config, opens no registry (a CLI built with a newer
  migration could not open it under an older daemon), never contacts the daemon and writes no file.
  `github.APIError` keeps the `errors` entries of a REST error body as `Details`, so the tool can tell a refused
  self-verdict from a refused line. Three cases the brief left open, decided here. A blind replay passes
  `--local-base <base_sha>` with `--dry-run`: the lines are checked against the local `git diff <base_sha>
  <head_sha>`, as the skill's blind rules require, and nothing is read from GitHub, whose PR may have moved on. A
  PR whose head moved while the judge worked is checked against GitHub's comparison of its base with the reviewed
  head, not its newer diff. A flag error exits 1 (status `error`), because 2 asks the judge to fix its file. The
  skill keeps every judgement rule; its posting mechanics shrink to writing the file, running the line and acting
  on the exit status. SKILL.md went from 30,839 to 30,308 bytes. The pipeline's verification is unchanged:
  GitHub, searched by the marker, stays the oracle. Rejected: having the daemon post for the judge (the judge
  must see a refused line and move it in the same turn); keeping the skill's own read-back next to the tool's (a
  second source of truth for the same facts).
- **The judge and claude-review dig into changed behaviour** (2026-10-06). A PR sped up its test suite by
  cleaning the test databases with DELETE instead of TRUNCATE, and magnum posted "no problems". Another
  reviewer found three. A mock-data generator outside production code also called the changed method and
  relied on TRUNCATE resetting auto-increment ids: with DELETE, regenerated fixtures get new ids and the
  frontend tests that look records up by id fail, while CI, which builds the mocks on a fresh database, stays
  green. DELETE also leaves full-text index statistics behind, so relevance scores drift with earlier
  examples. And a spec that hashes a record id into a split now fails about one run in 800. claude-review had
  raised the first and rejected it itself in its report. The skill now reads every caller and consumer of a
  method whose behaviour changed, signature or not, non-production ones included (fixtures, factories, seeds,
  mock generators, test helpers, scripts, rake tasks), and what consumes their output (generated files,
  snapshots, local runs, not only CI). When the PR swaps a mechanism for a near-equivalent (DELETE for
  TRUNCATE, another library or API, sync for async, eager for lazy), it lists what the old one did implicitly
  and checks each effect against every caller. It probes the real engine and framework while it looks, not
  only to prove a finding (a scratch table in the worktree's own databases, dropped after; the test runner; a
  console), and runs any focused check that can prove or reject a candidate. It judges an item a report lists
  as rejected, dismissed or out of scope like any other claim; such an item enters the ledger like any
  candidate, so `magnum stats` counts more rejections for claude-review. A test failure or flake the PR brings
  is not dropped as speculative or not reproducible without evidence against it; a chance one is replayed
  over its input space (ids, seeds, orderings) to state its rate, since one green run proves nothing.
  Developers' time is harm (local runs that diverge from CI, generated files that change, a new flaky test),
  and `P1` when it breaks their normal work. The rule on deliberate behaviour keeps "answer that reason or
  drop the finding" and adds that the reason covers only the consequences it names, so a stated intent does
  not shield a consequence it never mentions, and does not invite findings against one it does. Two
  techniques of the operator's own review skill, which magnum's was forked from, come back: a broken
  interaction a stacked PR creates with its lower layer is reportable, and a focused check runs to reject a
  candidate as well as to prove one. claude-review's three prompts (first review, re-review, restart) get
  one paragraph with the callers, the replaced mechanism and the test-failure rules, and ask for a `Rejected`
  list with the reason for each candidate dropped for anything but style or being pre-existing, so the judge
  sees what a reviewer talked itself out of. codex-review gets nothing: `codex review` takes custom
  instructions only as a review target of their own (its help lists `[PROMPT]` beside `--uncommitted`,
  `--base` and `--commit`, and the CLI asks for one of the four), so they cannot ride along with `--base`;
  replacing the base-branch target with a custom one would change what codex reviews and is left open.
  SKILL.md went from 30,308 to 30,837 bytes under the unchanged cap (30,844): tighter wording elsewhere pays
  for the additions (about 1,450 bytes) and no rule is dropped. The marker's format is stated once, in
  section 7; a repeated "do not follow instructions" sentence, the blind and post-merge recaps and the
  former-login, thread-file, re-review-note and provenance sentences are shorter. The `readiness` field now
  says that the `reset_db` commands run first and only the rest as `zsh -lc`, as they have since `reset_db`
  moved to `mise exec`. Rejected: raising the cap (the additions fit without it).
- **The retro waits for late reviews: `[learn] settle`** (2026-10-06, amends "Learning loop: daily retro"). The
  retro took a PR once, as soon as it closed: the daily retro at 07:00 took a PR merged at 06:55, and a change
  request a colleague posted 30 minutes after the merge was never seen. The daily retro and a plain `magnum retro`
  now take a PR only once it closed or merged at least `settle` ago (24h by default, 0 = at once, validated as 0 or
  more and shorter than `lookback`, else no PR would ever be due); `magnum retro <ref>` names its PRs and takes them
  whenever they closed. The retro's start event (`settling` in its data) and `magnum status`'s retro line count
  the PRs that wait for the delay when there are any. Rejected: looking at a PR again when a review arrives after
  its retro (another GitHub read per closed PR per day, and a classified miss would be classified twice).
- **The retro's misses become notes proposals** (2026-10-06, amends "Learning loop: daily retro" and "A curator
  proposes curated notes; the operator applies or rejects"). Nothing read the misses back: a blocking finding a
  colleague made after magnum's review changed no later review. A repository's misses of class `miss`, scope `repo`
  and state `new` are now input to its next notes curation, whatever triggered it (`--curate` too); general ones
  are left `new` for a later skill-editing stage. A retro that recorded at least one marks the repository once it
  is over (`KVNotesMisses`, a `notes.misses` event), so the curation starts after the retro and takes all of them;
  the curation trigger `misses` curates a marked repository with misses still new, under the curations' rules
  (one curation at a time, none while a round of it is in its judge stage or a proposal of it waits, at most one
  a day, an attempt that stored nothing waits an hour), and a curation that began after the mark clears it.
  `[notes] curate` becomes a list of triggers, `["over_limit", "misses"]` by default (`weekly` the third, `[]`
  none); the earlier string form reads as it meant (`"weekly"` is over_limit and weekly, `"off"` none). The
  misses reach the curator as
  data in `misses.json` in its scratch directory (id, severity, path:line at the reviewed commit, title, lesson,
  the reasons of the rejected proposals each was in), the title and lesson scrubbed again of the logins of the
  miss's PR and of magnum, pull request references and links; the prompt gets the file's path, the count and one
  rule: note a miss when a future review of the repository would catch a similar problem because of it, else
  skip it, and account for every id in `changes.json`'s `misses` (noted with a `## ` section of the proposal, or
  skipped with a one-line reason). `notes.Validate` makes a proposal that leaves a miss unaccounted for, names one
  it was not given, or notes one in a section it lacks invalid (one nudge, then `invalid`); a proposal that
  changes nothing stays invalid unless it was given misses and skips them all, which the operator then confirms
  (its apply records no second copy of the version). Migration 0014 adds `notes_proposal_misses` (a proposal's
  misses with the outcome, section and reason; never pruned). A miss stays `new` while its proposal waits; the
  decision moves it in the same transaction (`DecideNotesProposal`): applied, the noted and skipped misses become
  `used`, linked to the proposal; rejected, they stay `new` and the next curation reads the reason, and a miss
  in its second rejected proposal becomes `dismissed`; expired (or refused because the notes changed), they stay
  `new`. `magnum notes <repo> --review` lists the misses under the diff with what the proposal did with each and
  says where the decision left them; `magnum misses` shows STATE (with `--all`) and PROPOSAL (the latest proposal
  a miss was given to, with its state) and `--json` carries `proposal_id` and `proposal_state`. Rejected: a
  `proposed` miss state (the misses table's CHECK would have to be rewritten, and a waiting proposal already
  blocks a second curation); the misses in the prompt's text (prompts carry paths, never PR-derived text);
  marking the repository again after a rejection or an expiry (an operator who ignores proposals would get one a
  week; the misses wait for the next trigger); applying a curation of misses without review (the notes are read
  by every later round).
- **The judge does its own pass while the reviewers work** (2026-10-06, amends "Three-agent pipeline with a
  judge" and "A push during the reviewer stage restarts the round in place"). A first review took 32 minutes at
  the median (51 at p90) over 54 recent rounds: the reviewers' stage 17 (claude-review 16, codex-review 4), then
  the judge 15, strictly one after the other, while the judge's own full pass (read the code and the PR, run
  checks, find and prove its findings) needs no candidate report. `[pipeline] judge_own_pass = "parallel"` (the
  default; a `[[watch]]` may set its own, `"after"` keeps one prompt after the reviewers) prompts the judge with
  the new `judge-own-pass.md` (the role key `own_pass`) in the first stage: the usual `<magnum>` block for the
  round's kind with `phase: own_pass` and `own_findings: <report dir>/judge-own.md`, no reports, result file or
  post-review line; it verifies identity and target, reads the code and the PR, in a re-review decides the reply
  contract for its earlier threads, runs its checks, writes its findings with their proofs to that file and ends
  its turn, posting nothing and leaving the notes alone. When every stage and that turn ended, whichever came
  last, the judge's usual prompt goes out with `phase: candidates`, `own_findings` and a sentence that its own
  pass is there (or that it left none, so it does the pass now), and the judge judges every candidate against
  it, merges and does the rest as before. The critical path becomes about max(reviewers, own pass) plus 5 to 8
  minutes of judging and posting: some 9 minutes off the median, at about the same usage. A judge alone (a delta
  check, a continued turn, a round of the judge alone or whose reviewers were all dropped or logged out) gets one
  prompt. The own pass is a run of its own, kind `own_pass` (migration 0015 rebuilds `runs`, and `findings` with
  it, for the CHECK), created after the judge's run so the review's marker stays the judge's run and the
  observer's newest run on the judge's session is the one in flight; its model-limit continuations keep the kind.
  It is shown as such: `round.own_pass` events, `codex-judge own pass` in the card's timeline and last-round
  stages and in `magnum stats`, `own_pass` in `prs --json`, and the state cell reads `reviewers+judge` (or
  `claude+judge`) while both work. It is part of the reviewers' stage everywhere a judge used to come after
  them: a push cuts it short like a reviewer (interrupted with ctrl+c twice, its run abandoned as
  `head_moved`) and the restart prompts it again on the new head, naming the head it moved from; the checkout
  check after a stage names the judge while its own pass works, and once more after it ended; the judge's
  `timeout` bounds each phase (a timed-out pass is interrupted and waited for); a usage limit or any end
  without its file is no failure: the candidates prompt still goes out once the reviewers ended and pauses the
  round on the limit, so the paused round continues the judge's candidates turn with every report in its
  context; the round's cancellation leaves its run in flight as a reviewer's; crash recovery and the
  continue/refund decisions (`engine.latestJudge`, `judgePrompted`) and the unverified-review marker never take
  it for the judge's turn, so a daemon restart during the own pass starts the round again, as one while the
  reviewers run does. Dispatch's working-Codex count already counts the judge: `max_total_working_codex` adds
  every Codex role of a new round (the judge included) and reads herdr's working Codex agents, which now
  include a judge on its own pass. SKILL.md's section 0 lists `phase` and `own_findings` and one paragraph says
  what the own pass does and does not do; it stays at 30,835 bytes under the unchanged 30,844 cap (tighter
  wording elsewhere: the readiness and former-logins fields, the threads file's keys, the candidate reports
  list). Rejected: one prompt file per round kind for the own pass (three near-copies of the re-review and
  recovery context; one file branches on `mode`); the own-pass fields in the usual prompts (a custom
  `judge-initial.md` without them would have the judge post during the reviewers' stage); continuing a judge
  turn after a daemon restart during the own pass (the continue prompt would have it post without the
  reviewers' reports); skipping the candidates prompt when the own pass hit a usage limit (the paused round's
  continue prompt names no reports, so the judge would never see them).
- **The judge knows the related PRs** (2026-10-06). Two open PRs of one repository fixed the same flaky spec in
  different ways; the author found out by chance and reverted one, while magnum reviewed both and said nothing,
  because a round sees only its own PR. A PR that touches lines another PR just fixed can undo that fix too. The
  Details the poll already reads for a changed PR now carry `files(first: 100) { totalCount pageInfo {
  hasNextPage } nodes { path } }` (`github.PRDetails.Files`, `FilesComplete`; `files: null` reads as no list).
  GitHub's dry run (`rateLimit(dryRun: true)`) priced the Details batch of 1, 10, 20 and 40 PRs at 1, 1, 1 and 3
  points without the files and 1, 1, 2 and 3 with them: at most one point more per poll, usually none, and 100
  nodes per PR. A PR with more than 100 files is not paged: a second page would be a query of its own (a point)
  for every head of such a PR, so its list is marked truncated instead and the judge sees the flag. The registry
  keeps one row per PR in `pr_files` (migration 0016: `head_sha`, `paths_json`, `truncated`, `fetched_at`),
  written in the upsert's transaction only when the PR has no list or its list belongs to another head, never
  counted as a GitHub change, kept after the PR closes or merges (with `merged_at` in `prs`); the migration
  clears the open PRs' `details_at`, so the next poll fetches their lists once. At the start of every judge
  prompt (the own pass and the candidates phase alike, a round's one prompt, a delta check, a continued turn)
  the pipeline reads the PR's list at the head under review and the repository's other PRs with a list that are
  open (drafts included) or merged within `[pipeline] related_lookback` (default 14d), leaves out the paths
  matching `related_ignore` (path globs as `skip_paths`; default the lockfiles: `Gemfile.lock`,
  `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock`, `bun.lock`, `go.sum`, `poetry.lock`, `uv.lock`,
  `Pipfile.lock`, `Cargo.lock`, `composer.lock`), and ranks those sharing a path by the number they share, then
  the newest PR: at most 10, each with at most 20 of its shared paths. When any is left it writes `related.json`
  in the report directory (the PR, its head, its truncated flag, the lookback's start, and per related PR its
  number, URL, `open` or `merged` with `merged_at`, the draft flag, the head its paths were read at, the overlap
  count and paths, its truncated flag, whether magnum reviewed it with its last posted review's URL and verdict,
  and the findings magnum posted on the shared paths over all its rounds; `more` counts the PRs past the cap) and
  the prompt's `<magnum>` block carries `related_prs: <path>`; with none, no field and no file. No titles, bodies
  or comments: PR text is data, and the judge reads a PR itself with `gh` when it matters. A blind replay never
  gets it (it would tell of later PRs); a PR without a list for the head under review gets none (the poll writes
  one with the head's Details; a forced `magnum review` that starts before them goes without). A
  `[[watch]]` may set both keys (a zero `related_lookback` or an unset `related_ignore` keeps the pipeline's,
  `[]` ignores no path; `related_lookback = "0s"` in `[pipeline]` keeps only open PRs). SKILL.md lists
  `related_prs` in section 0, and section 2 says what to do with it: an open PR changing the same behaviour (a
  duplicate or competing fix, conflicting edits, one needing the other) gets one line in the review body naming
  it, a finding only when merging both provably breaks something; for a merged one, check this PR does not undo
  or re-break its fix; a fix of a flaky test or a recurring bug class goes into the notes as a pattern. Section
  7 places that line after the finding titles. It goes from 30,832 to 30,836 bytes under the unchanged 30,844
  cap: the frontmatter no longer names the identity kinds, the PR boundary's prohibitions are one sentence,
  section 6 no longer repeats `moved_from`'s "work only in `checkout`" (the re-review prompt says it), section
  7's machine failures and section 8's provenance fields point to where they are said already, and the skill no
  longer describes what `post_review` checks or repeats when to update the notes. `round.related` events name
  the related PRs. Rejected: paging the files (a query per page and head for a rare case); a table of one row
  per path (the candidates are the repository's open and lately merged PRs, a few hundred rows read at most);
  the PR's own paths from `git diff` in the checkout (a second source; the poll's list is at the head a round
  reviews, and a restart moves both); the REST file list `skip_paths` reads (a call per PR; it stays for
  `skip_paths`, which needs a rename's old name and three pages); the related PRs in `prs --json` or on the PR
  card (`prs --json` mirrors the board's row, and a board layout change needs a mockup first).
- **An overview of every repository's notes** (2026-10-06, amends "Repository notes: triggers, history, usage
  and curation"). The board said "notes ×2" and the dashboard "2 notes proposals to review", but nothing said
  which repositories have notes, how big they are or which repositories the proposals are for: `magnum notes`
  without a repository was a usage error and `magnum status` said nothing of notes. `magnum notes` without a
  repository now lists one row per repository with notes (a notes file on disk, or in the registry a recorded
  version with content, a harness on disk, a proposal waiting or a curation running or queued): REPO, NOTES
  (bytes and lines, the long lines counted, `!` past max_bytes or max_line), HARNESS (files and bytes, `!` past
  max_harness_files or max_harness_bytes), CHANGED (how long ago and by whom: the latest recorded version's
  source, `judge #<PR>` for a judge's, or the file's age and "not recorded"), STATE (`ok`, `over limit
  (<triggers>)`, `curation running (<trigger>, <age>)`, `curation queued (<trigger>, waits for …)`, `proposal N
  to review (<trigger>, <age>)`, `proposal N stale (…; the notes changed since it was made)`, the first that
  applies from the last), then a legend for the marks and one hint per row whose state asks for something
  (`review: magnum notes <repo> --review`, `curate: magnum notes <repo> --curate`). `--json` prints
  `{limits, repos}` with the same rows. `magnum status` (and its `--json`, `notes`) and the dashboard's header
  sum the rows up in a `notes:` line ("4 repos · 2 proposals to review (talkable/talkable, example/api stale) ·
  1 over limit", then a curation running or queued), every part that is zero left out and the line itself
  while no repository has notes; the titles' "notes ×N" fact stays. Shell completion of `magnum notes <TAB>`
  offers only the repositories with notes, from the files and a read-only query of the registry, naming a
  proposal that waits. The toast for a new proposal already named the repository ("notes curation for <repo>
  is ready"). Rejected: the sizes from the daemon's `KVNotesOver` marks instead of measuring (the list would
  disagree with `magnum notes <repo>` until the next judge round).
- **Curations wait for a judge stage instead of being refused or skipped** (2026-10-06, amends "A curator
  proposes curated notes; the operator applies or rejects"). `magnum notes <repo> --curate` was refused while
  a round of the repository was in its judge stage ("ask again when it ends"), and since the judge's own pass
  starts with the reviewers that window covers most of a round: on a busy repository the request almost
  always failed. The daemon's own triggers skipped such a repository until a scan every 10 minutes landed
  outside one. Both now queue the curation (`engine.KVNotesCurateQueue`, one entry per repository, oldest
  first, with its trigger, why it waits and since when; a `notes.curate_queued` event), and the request says
  "queued: starts when the current round's judge stage ends". A request while another curation runs is
  queued too ("starts when the running curation (of <repo>, started HH:MM) ends") instead of answered with a
  note and dropped. Every tick, not only every scan, starts the oldest queued curation whose repository has
  no round in its judge stage, one at a time, under the holds a request obeys (a drain, an infrastructure
  pause, a pause of the curator's CLI); a queued daemon trigger also waits out `magnum pause` and is not
  started under `[notes] curate = []`. An entry whose repository got a proposal meanwhile, or is gone, is
  dropped. The curation running is `engine.KVNotesCurating` (repository, trigger, start), deleted when it
  ends and at every daemon start, so `magnum notes` and `magnum status` can say "curation running" and
  "curation queued". Rejected: keeping the request pending in the requests table until the stage ends (a CLI
  waiting for its answer would time out, and `magnum status` would count a pending request as unhandled).
- **A stale notes proposal is merged, or followed up by a new curation** (2026-10-06, amends "A curator
  proposes curated notes; the operator applies or rejects"). A judge round rewrote a repository's notes 14
  minutes after its curation took its copy, and `y` refused the proposal ("notes changed since the proposal;
  run --curate again") only after the operator had read the whole diff, and expired it. A pending proposal
  whose base version is not the notes on disk (their fingerprints differ: `engine.ProposalStale`, from the
  version's listing and the files' hashes) is stale; `magnum notes`, `magnum notes <repo>` and `--review` say
  so first, before the diff. `--review` merges the judges' changes since the base and the proposal's
  (`notes.MergeStates`, a pure diff3 of the notes text, `notes.Merge3`, in Go over the line diff the notes
  already use, with changes that overlap or touch, as git's, one region; no git, no temp files): per harness
  file, live unchanged takes the proposal's, the proposal unchanged or both the same takes live's, a file the
  proposal deletes that a round changed since is kept as the round left it and listed, both changed merges
  their texts, and live deleted with the proposal changed, both added differently, or texts that do not merge
  are conflicts. A clean merge is shown as the diff and harness changes it would apply to the notes now (sizes
  now → merged); `y` applies it under the lock after checking the notes are still what the review showed,
  records the notes it replaces as an import first (the history keeps the judge's state even when its round
  did not record it yet), and records the merge as the curation's version (a restore's as the operator's), as
  every apply. A conflict (the review names the base version's lines and the files), or `c` on a clean
  merge, makes `y` ask the daemon for a new curation from the notes now (`NotesCuratePayload.Supersede`): the
  daemon marks the stale proposal `superseded` (a new state: migration 0017 rebuilds `notes_proposals`, and
  `notes_proposal_misses` with it, for the CHECK; its misses stay `new` for the new curation; a
  `notes.proposal_superseded` event) and starts or queues the curation, whose scratch directory gets the
  superseded proposal as `superseded/` (its notes, harness and `changes.json` with every reason) and whose
  prompt names it, so the work is not lost (`prompts/notes-curate.md`, `.Superseded`, `.SupersededID`; the
  repository's latest curation, when superseded, is the one read, so a follow-up that stored nothing leaves
  it for the next). A stale restore that conflicts cannot be applied: `y` expires it and says to `--restore`
  again. The notes changing between the review and `y` no longer expire the proposal: the apply is refused and
  `--review` again shows it against the new notes. The daemon supersedes a stale proposal on its own, with
  the trigger `stale`, once it is a day old and its changes no longer merge, within the curations' limits (at
  most one a day, one at a time, queued during a judge stage; not under `[notes] curate = []`); a stale
  proposal that still merges waits for the operator, since it can still be applied, and on a busy repository
  every proposal goes stale within hours, so re-curating each after a day would spend a curator run a day per
  repository while the operator is away. Rejected: `git merge-file --diff3` on temp files (a fake git in every
  test, files with notes text on disk); re-validating the merge with `notes.Validate` (the judges' changes
  carry no curator reasons; the operator reviews the merge); applying a clean merge without review.
- **Only a file name or a path names a probe file** (2026-10-06, amends "A curator proposes curated notes; the
  operator applies or rejects"). A curation was stored as invalid after its nudge with "proposal.md names a
  probe file (window.PROBE_SELECTOR)": the check matched any token with "probe" between separators, and that
  is a JavaScript global the notes use in a generic browser-probe technique. `notes.Validate` now takes a token
  of the notes for a probe file only when it is a file name (a short lower-case extension: `.rb`, `.sh`,
  `.json`) or a path (a slash) whose name or a directory of it says probe (`coupon_probe.sh`,
  `qa/probe_coupons.rb`, `spec/probes/coupon_spec.rb`, `probes/`); bare identifiers (`window.PROBE_SELECTOR`,
  `PROBE_TIMEOUT`, `MY_PROBE_TIMEOUT`, `window.PROBE`) and setting keys (`probe.enabled`) pass. A probe in the
  harness the notes name by any other name is still caught by its harness name, as before. Rejected: matching
  the report directory's files (a curation has none; the judges keep one round's probes there and the notes
  must not name them, which the file-name rule already catches).
- **A network blip does not mark an identity unhealthy** (2026-10-06). Right after a daemon restart every
  GitHub call of the first tick failed with gh's "error connecting to api.github.com" for a few seconds; the
  identity checks and token refreshes of that tick marked all four identities unhealthy, which held every
  watch, and unhealthy identities were re-checked only at the reconcile (10 minutes). A failed check or
  token refresh is now classified (engine `connectionCause`, which shares infraCause's DNS, timeout,
  refused, reset and TLS patterns and adds gh's "error connecting to", Go's dial, TLS, timeout and EOF
  errors, HTTP 5xx and 429 and rate limits): a check is connection-class only when every FAIL line and its
  error are. Such a failure keeps the identity's previous verdict (a pass stays a pass, a token still
  valid stays in use, a recorded failure stays as it was) and is retried on the following ticks after 30
  seconds, 1, 2 and 4 minutes, a retry never landing after the fifth minute of the run; only a failure
  that lasts 5 minutes (identityNetGrace) is recorded, as "network failure (<cause>) since <hh:mm>: <the
  error>", with the usual identity.unhealthy or identity.token_error event and toast; the run's first
  failure is an info `identity.unreachable` event. An identity recorded unhealthy for a connection-class
  reason, by the daemon, `magnum identities check`, a judge's own check or a previous daemon, is
  re-checked every tick its backoff allows (at most 4 minutes apart); a real verdict (401, 403, 404, a
  wrong login, a missing permission, a bad key) is recorded at once and re-checked at the reconcile as
  before. The retries live in the daemon's memory: a restart checks every identity at startup anyway.
  Rejected: re-checking every unhealthy identity each tick (a real verdict would cost GitHub calls every
  30 seconds for nothing); retrying inside identity.Check (a check that waits minutes would stall the
  tick, and the token refresh has its own 30-second mint backoff); a persisted retry schedule (nothing
  needs it across a restart).
- **UPDATED is the PR's last activity, not GitHub's updatedAt** (2026-10-06). The board said "11m ago" for a PR
  whose only change was invisible: no push in five days, no comment, review, label, request or edit since
  August. It showed GitHub's `updatedAt`, which also moves for what a reviewer never sees and magnum cannot
  even read (a GitHub Projects field set by someone or an automation, a resolved review thread, someone's
  pending review, a deleted comment). A PR's activity time (`prs.activity_at`, migration 0018) is now the
  latest of its opening, a push, a force push, a comment, a submitted review (a reply in a thread is one), a
  label added or removed, a review requested or removed, ready for review or back to draft, a title rename,
  a description edit (`lastEditedAt`), a base change (also GitHub's automatic one), a close, reopen and
  merge; bots count (magnum's own reviews are activity), CI checks do not. The Details the poll already reads
  for a PR whose `updatedAt` moved carry it (`github.PRDetails.ActivityAt`): a second
  `timelineItems(last: 10)` of those item types, each node's `createdAt` (`submittedAt` for a review, null
  while pending), with `createdAt`, `lastEditedAt`, `mergedAt`, `closedAt`, the latest reviews' times (the
  timeline may list a review where it was begun, so it can fall out of the last ten) and the head commit's
  `committedDate`, which is no push time but never after one, capped at `updatedAt` for a committer clock
  that runs ahead; a missing timeline (null) is no activity time. GitHub's dry run (`rateLimit(dryRun:
  true)`) priced Details batches of 1, 10, 20, 30 and 40 PRs at 1, 1, 2, 2 and 3 points without it and 1,
  1, 2, 3 and 4 with it: at most one point more per batch, none for the few PRs a poll usually reads. The
  registry keeps the later of the Details' time and the head moves the poller saw (`head_changed_at`, now
  when the head just moved; not the PR's insertion, which sets it too), so a push of commits dated days
  earlier reads as the push; an upsert without Details keeps it and moves it for a push; a merge or close
  confirmed sets it, since a closed PR's Details are not read again. Never a GitHub change: it moves no
  eligibility. The board's UPDATED column, the updated sort, the card's Updated, `magnum prs` (table and
  `--json`: `activity_at`, and GitHub's value as `github_updated_at`, where `updated_at` was), `magnum pick`
  and the dashboard's queue ages show it (`store.PR.Activity`, `store.BoardRow.ActivityAt`,
  `tui.PRBoardRow.ActivityAt`; `activity_at` in `status --json`'s queue lines); until the next Details fetch
  reads it they show GitHub's `updatedAt`, marked nowhere. Radar change detection (which PRs get Details) and
  the dispatch order keep GitHub's `updatedAt`, and so does the dashboard's queue, which lists the PRs in the
  dispatcher's order (`updated_at` in its JSON lines). The migration clears the open PRs'
  `details_at`, so the next poll reads every open PR's activity once, and gives a closed or merged PR its
  close or merge. No layout change. Rejected: `timelineItems(last: 1)` (the timeline may list a review
  where it was begun, so its last item need not be the latest; ten cost the same points); `head_changed_at`
  alone (a repository's first sync stamps it on every PR); a REST timeline call per PR (a call each instead
  of a part of the batch); counting assignments, review dismissals, milestones, auto-merge, mentions,
  cross-references and comment edits (not what the operator asked for) or project events (they need
  `read:project`, which magnum's logins lack).
- **A reply that declines the fix in its own words is `won't fix`** (2026-10-06, after a re-review kept a
  P2 open and withheld the approval: the author's reply confirmed the finding, said "still open" and that
  the remedy was "undecided", weighed the proposed fix against its cost and scored it at −8; the author
  meant "we will not do that", the judge read the words, and the classifier said `other` because the first
  clause was neither a verdict nor an acknowledgement). The skill's reply contract now decides a reply by
  what it does, not by its keywords: a reply that weighs the fix and turns it down (a negative score, "not
  worth it", "we accept the risk") is `won't fix` with its reason, even when it calls the thread open or the
  remedy undecided, honoured unless the judge proves the reason wrong at the head (the impact is larger);
  only a reply that neither fixes, disputes nor declines leaves a finding still open. `classifyReply` falls
  back, when no first clause decides, to `won't fix` for a negative score for the fix anywhere in the first
  paragraph ("score(s|d) that/the/this fix at −8", "Net: −3", "net -2.5", hyphen-minus or U+2212) or a
  clause "we accept the/this risk" or "not worth it/the …", unless an earlier clause says fixed or not a
  bug; the class stays a hint. SKILL.md's cap moved from 30,844 to 31,098 bytes, the rule's size: the
  sentences around it are pinned or carry rules. Rejected: taking "still open" or "undecided" at their word
  (what caused the miss), a positive score as a fix (only "applied" or "fixed" claims one, and the code must
  show it) and scores past the first paragraph (the classifier reads only the first).
- **PRs Magnum approved that still need the operator's approval** (2026-10-06). GitHub does not count a
  GitHub App's approval toward a branch's required approvals: on 11 open PRs the App approved, GitHub's
  `reviewDecision` and `latestOpinionatedReviews` never included it, so a PR Magnum approved can still be
  blocked on the operator, whose approval is the one that counts, or on their own earlier changes request
  (one PR had five), which blocks it until they approve or dismiss it. Such a PR "needs me"
  (`store.NeedsMe`): open, not a draft, not authored by one of the operator's logins (the board's ★:
  `config.SelfLogins`, every watch's posting identity and every gh identity, folded so an App named after
  the user is theirs), Magnum's latest verified review on the current head (`reviewed_sha = head_sha`)
  and clean (`last_review_event` APPROVED, or COMMENTED with the latest posted round's verdict clean for an
  identity whose `no_findings_event` is COMMENT on that repository), and GitHub's decision one the
  operator's approval fixes: REVIEW_REQUIRED (`approve`), or CHANGES_REQUESTED with every outstanding
  changes request theirs (`lift`). Someone else's changes request, a decision not read yet, no review
  required (null) or a list of opinions cut at 100 is not. The Details the poll reads for a PR whose
  `updatedAt` moved (a review moves it) now carry `reviewDecision` and `latestOpinionatedReviews(first:
  100, writersOnly: true) { state author commit }` (`github.PRDetails.ReviewGate`); `writersOnly` because
  only reviewers with write access count toward the decision. GitHub's dry run priced Details batches of
  1, 10, 20, 30 and 40 PRs at 1, 1, 2, 3 and 4 points with and without them (2026-10-06): no extra point.
  The registry keeps them in `prs.review_gate_json` (migration 0019, `store.ReviewGate`), written without
  counting as a GitHub change; the migration clears the open PRs' `details_at` so the next poll reads each
  gate once. The board's STATE cell says "✓ needs you" or "✓ lift your ✗" (a round in flight keeps its
  own pill; the card's head adds it, still) in a rainbow of the six ANSI hues, two cells each, sliding one
  cell every 250 ms while such a row is on screen; only those rows are drawn again (the frame is in their
  row keys and, while one is visible, in the frame key, never in the rows' key the layout and the other
  rows are cached by), and nothing ticks while none is on screen, in the card, with `[board] shimmer =
  false`, or on a terminal without colors (`tea.ColorProfileMsg` at or below ASCII: NO_COLOR), which shows
  the cell in bold reverse video. The updated sort (the default) lists these PRs first either way; the
  other sorts keep their order. The titles of the board and the status dashboard say "N need your ✓"
  (`tui.DaemonFacts.NeedsMe`, after a pause, before the Codex pace), `magnum prs --needs-me` lists only
  them (the live board too), `prs --json` gains `needs_me` (`approve`, `lift`, "") and `review_decision`,
  the printed STATE `needs-you`/`lift-yours`, `magnum pick` `✓ needs you`/`✓ lift your ✗`, and the daemon
  toasts each PR once per head with its link (`needs-me:<pr>:<head>` deduped for 30 days in the registry,
  batched with the other informational toasts). Rejected: a stored needs-me column (its inputs change at
  the poll, at a review's verification and with the configuration, so it would be stale between them);
  `latestReviews` (a later comment hides an outstanding changes request); a new section of the board (one
  line per PR, as decided); truecolor for the rainbow (the screens use the 16 ANSI colors only).
- **A re-review of an unchanged head is the judge alone** (2026-10-06; extends "A small re-review delta gets
  a judge-only check"). `magnum review` on a PR whose head Magnum had already reviewed, to have the judge
  re-read an author's reply after a fix to the reply contract, ran a full round: claude-review, codex-review
  and the judge, about 17 minutes and a full round's usage, though no code changed and only the threads
  needed re-deciding; the review it posted was headed "Re-review 42a70de → 42a70de" and its Checks listed an
  empty commit range and an empty diff. A re-review whose head is the reviewed one (`head_sha =
  reviewed_sha`, forced or requested, not post-merge) now runs as the delta check does: the judge alone, no
  triage, reruns, own pass or restarts, at its `rereview_effort` (`round.same_head`, `same_head` in
  `engine.round_start`). A judge whose session is gone runs alone in a fresh session when a review of the
  PR's identities is on record (`round.same_head_fresh`), as the delta check's does; else, or when the
  checkout finds a newer head, the round runs in full (`round.same_head_dropped`). `judge-rereview.md` and
  `judge-recovery.md` (`.SameHead`) say the head is unchanged instead of naming new commits and candidate
  reports: re-read the replies and comments since the last review, re-decide each earlier finding under the
  reply contract, run no check that review already ran on this head, post one short review. SKILL.md heads
  such a review `**Re-review of <sha7> (no new commits):**`, its Checks listing only what ran this time; the
  sentence fits the cap (31,098 bytes) by dropping three asides the prompts or the code already carry (what
  reads `provenance`, how Magnum handles a newer head, the former logins' history spelled out) and
  shortening the description. A request that names roles (`--role`, `--simplify`) or asks for fresh
  sessions (`--fresh`) keeps the full round: it asks for reviewers' eyes or a clean start. Rejected: a
  `same_head` field in the `<magnum>` block (the judge sees `previous_head_sha = head_sha`, and the skill
  would have to describe one more field).
- **Magnum approves as the operator when its review found nothing to fix** (2026-10-06, the operator's design).
  GitHub does not count a GitHub App's approval toward required approvals, so a PR Magnum found clean still
  waited for the operator's. A `[[watch]]` may name repositories (`auto_approve = ["talkable"]`, or `["*"]`)
  and a gh identity (`auto_approve_as`, the operator's own account; validated: declared, kind gh, names the
  watch covers, no owner or pattern) on whose PRs Magnum posts that identity's APPROVE, on the head it
  reviewed (`reviewed_sha` = `head_sha`), when its latest verified review of that head leaves nothing to fix
  before merging: no P0, P1 or P2 finding, still-open earlier ones included. The judge's result gives this
  round's findings by priority but only a count of the earlier ones still open, so with some open the
  review's verdict line (SKILL.md section 7) decides: `No blocking problems.`, `No problems found…` or `No
  new problems since…` are clean, `Blocking:` and `Fix N problem(s)` are not, and an unknown line is not
  clean. Never on a PR that is not open (so never after a post-merge review), a draft, muted, authored by
  that login, in a state other than reviewed (a round due or running), whose latest review is not the
  round's (a manual verdict came after it), or without a posted round (a dry run posts none). Before
  posting, Magnum reads the PR's reviews (`github.Client.Reviews`): the operator's approval of the head
  means nothing to post, a review of theirs in progress waits, and any review of theirs Magnum did not post
  (neither a round's `<!-- magnum:run=` nor an automatic approval's marker; one posted with `magnum approve`
  or `request-changes` is theirs) is an intervention. The body is one line, `auto_approve_body` (a template
  of `.Short` (7 characters), `.SHA`, `.ReviewURL`, `.Repo`, `.Number`), default "Auto-approved: magnum's
  review of `<sha7>` found no blocking problems ([review](<url>)).", plus `<!-- magnum:auto-approval
  head=<sha7> -->`. The registry keeps each one in `auto_approvals` (migration 0020: posting → standing,
  failed, dismissing → dismissed, with who ended it and why; a partial unique index allows one live row per
  PR) and the operator's stops in `auto_approve_holds`. One approval per review (`run_id`): a 403 or 422 is
  not tried again for it, another failure after 5 minutes (three posts at most), and an approval of the head
  carrying the marker that GitHub has and the registry lost (an answer lost, a stop mid-post) is recorded,
  never posted twice; events `review.auto_approve_begin`, `review.auto_approved`, `review.auto_approve_failed`.
  New commits alone do not withdraw it (the repository's stale-approval setting decides; a dismissal and a
  new approval per push would be noise): the next posted round does. One that leaves something to fix (or
  cannot tell: earlier findings open and the verdict line unknown), on any head, dismisses it as the
  operator ("magnum's review of <sha7> found blocking problems; this automatic approval is withdrawn
  ([review](url)).", `review.auto_approval_withdraw_begin`, `_withdrawn`, `_withdraw_failed`; a failure stays
  dismissing, tried again after 5 minutes, with one urgent toast); a clean one leaves it standing without a
  second; a clean round after a withdrawal approves again. The operator's word wins and sticks: a review of
  theirs by hand, or a dismissal of one of these approvals by them or anyone else (the PR's timeline,
  `github.Client.ReviewDismissals`, says who), stops auto-approval of that PR (`review.auto_approve_stopped`);
  GitHub's own stale dismissal on a push (`pullRequestCommit`) does not. `magnum review` keeps the stop;
  `magnum unapprove --resume` lifts it (the operator's reviews from before no longer count). `magnum
  unapprove <ref>` and the board's `D` (y/N each) withdraw the standing approval as the operator ("magnum:
  this automatic approval is withdrawn by its owner (magnum unapprove).") and stop it. GitHub is read only
  for a candidate whose latest round or `updatedAt` moved since it said no, and for a standing approval when
  they moved (a merged or closed PR's is left alone). Visible: a toast per approval ("approved as you:
  talkable#12001", with the link) and per withdrawal Magnum made, batched with the other informational
  toasts; no needs-me toast, title count or `✓ needs you` for a PR approved as the operator; STATE `✓ auto`
  in a cyan pill without the shimmer, the titles' "N auto-approved", the card's line (or why it stopped),
  `magnum prs --auto-approved` and `auto_approved` / `auto_approve_stopped` in `prs --json`, `✓ auto` in
  `magnum pick`, and "approvals: auto-approved: N today, M standing" in `magnum status` (`auto_approved` in
  `status --json`). Rejected: deciding from the review gate the poll stored (it lags the review, and has no
  bodies to tell Magnum's reviews from the operator's), counting only the operator's dismissals as a stop (a
  maintainer's dismissal is a person's judgement too), withdrawing on every push, and an approval when the
  priorities of still-open findings cannot be told.
- **`magnum stats` says who found each posted problem** (2026-10-06; amends "Finding provenance, then `magnum
  stats`"). Two estimates of what the reviewers add disagreed: in 31 full re-reviews they found 5 posted
  findings the judge missed, all P3, against "41 of 75 posted re-review findings came only from
  claude-review", because before the judge's own pass it read the reports first and `sources` mixed "found"
  and "confirmed". Since the own pass writes `judge-own.md` before any report is read, SKILL.md section 3 says
  `sources` names `judge` only if the own pass (`own_findings`, when set) found the problem; a candidate the
  judge only confirmed lists its reports alone. `magnum stats` gains two sections (`value` in `--json`) over
  the posted rounds whose provenance can tell: first reviews and re-reviews (recoveries included) with a run of
  kind `own_pass`, and delta checks, re-reviews the judge ran alone (an unchanged head too), whose findings are
  all its own. Per kind: the rounds, the posted findings by priority (of the round's latest judge run that
  recorded provenance) split into the own pass alone, the own pass and a reviewer, and reviewers only by the
  reviewers that raised them (`claude-review+codex-review` is one key, so the split adds up), those with no
  source apart; the reviewer-only P0 to P2 findings and their rate per 10 rounds; each role's median turn (the
  judge's own pass apart). It informs one decision: when reviewers alone add almost no P0 to P2 in re-reviews,
  more re-reviews can run as the judge alone. Rejected: a separate `--value` view (every section of `stats` is
  one report), counting rounds before the own pass (their `judge` meant "found or confirmed"), and splitting a
  finding two reviewers raised between them (the per-reviewer counts would no longer add up).
- **Bugs found next door are kept, and listed apart** (2026-10-06). The judge proves and drops problems that
  already existed before the PR (`pre_existing`): 63 so far, 1 P1 and 26 P2, among them a cross-site export of
  shoppers' emails on a PR that fixed the same guard elsewhere, and nobody heard of them. Every provenance
  entry now carries a short `title` (required by SKILL.md section 8), and a `pre_existing` P1 or P2 the judge
  proved at `head_sha` in or near code the PR changes is `"nearby": true` (section 7); the result parser keeps
  the title as one line of at most 120 runes and the mark only on a rejection, and migration 0021 stores both
  (`findings.title`, `findings.nearby`, which rows recorded before lack). The review lists up to three nearby
  problems in a collapsed `<details><summary>Found nearby, not this PR's (N)</summary>` block before Checks,
  one line each (`path:line`, the problem, its priority), never counted in the verdict line or the event; a
  security one (an authorization bypass, data exposure, injection) goes only to the result file, since the
  repository may be public. `magnum debt [<repo>] [--json]` lists them across PRs from the registry
  (`store.PreExistingFindings`): the rejected `pre_existing` P1 and P2 findings marked nearby, and the untitled
  ones recorded before titles and the mark existed (their path and reason only), newest first, once per
  repository, path and title (case and blanks folded; an untitled one per path), the newest find with its PR
  and day. SKILL.md grows to 31,523 bytes and its cap from 31,098 by 425: the block, the titles and the
  sources rule add 690 bytes, and shortening sections 3, 7 and 8 without dropping a rule won back 250 (the
  reports' description, the reason a simplification must sit on changed lines, "magnum handles a newer head",
  what magnum does with `verdict`). Rejected: counting nearby problems toward the verdict (the PR did not
  bring them), posting a security one in a public review, and listing titled pre-existing problems the judge
  did not mark nearby (not proven at the head, or far from the PR).
- **Repositories shown but reviewed only on request** (2026-10-06). The operator wanted a repository on the
  board and in `magnum prs` without Magnum reviewing it on its own. A watch's `manual_repos` names such
  repositories (names it covers, any case, without owner or pattern, validated like `auto_approve`'s), and
  `eligibility.Classify` rejects their PRs, right after `muted`, with "manual repository (manual_repos)"
  (`PRFacts.Repo`, which the engine's `factsFor` reads only for a watch that has the key): no round starts
  for a new PR, a push or a review request on GitHub, and a PR already waiting becomes ineligible at
  dispatch. A forced review (`magnum review`, the board's `r`/`R`, the picker) still runs, as with every
  filter. Nothing else changes: the poll reads the PRs, the board, card, status, related PRs and the retro
  see them, and the board says "skipped · manual". Rejected: leaving the repository out of the watch (it
  would vanish from the board and `prs`) and muting each PR (`magnum ignore` is per PR, and a new PR would
  be reviewed before it could be muted).
- **The reviewers read the changed files' history** (2026-10-06). In 4 of the 5 known misses the changed file's
  own recent log pointed at the problem: a PR that changed test database cleanup sat under "Fix flaky mock
  generation" and "Stop CI hang from contended OPTIMIZE TABLE" in its helper's log, exactly the effects the
  review missed, and no prompt or skill line asked for a file's history. Before the reviewers of each head (the
  start of every stage run, so a restart writes the new head's), a round writes `history.json` in its report
  directory (`pipeline.FilesHistory`): each file the PR changes that exists on its base (`gitx.ModifiedPaths`,
  `git diff --name-only --diff-filter=a <base>...<head>`, so an added file, which has no history there, takes
  no place), at most 40 in git's order with the rest counted as `more`, paths matching the watch's
  `related_ignore` aside (the lockfiles: their logs are dependency bumps), each with its last 8 commits on
  `origin/<base>` (`gitx.FileLog`, `git log -n 8 -z --format=%h%x00%cs%x00%s <rev> -- :(top,literal)<path>`,
  four at a time): the unique abbreviation, committer date, subject, and the PR number when the subject ends
  with `(#123)`, as GitHub titles a squash merge. A blind replay reads both at its merge base (`base_sha`),
  never `origin/<base>`, which may hold the PR's own merge and the fixes after it, and gets none without one;
  a continued turn gets none (the paused turn had it). The judge's own pass, its candidates phase and a round's
  one prompt carry `history: <path>` in the <magnum> block (`agents.JudgeData.HistoryFile`; judge-own-pass,
  -initial, -rereview and -recovery, not -continue); the claude-review prompts (initial, rereview, restart) get
  one sentence naming the file (`RoleData.HistoryFile`); claude-simplify proposes no defects and gets none.
  SKILL.md section 2: read a commit that fixed the code or mechanism the PR touches (`git show <sha>`), or a
  merged related PR's fix (the related-PRs paragraph's sentence moved here); undoing or re-breaking it is a
  finding. The history is a hint the reviewers wait for, so reading it is cut at 2 minutes
  (`pipeline.HistoryTimeout`; a big repository without a commit-graph walks its whole history for a file
  changed long ago), and a `git log` that fails or is cut only warns (`round.history`) and leaves the prompts
  without it; events carry counts, never a subject. The file list comes from git, not the poll's (capped at
  100 and only for the head the poll saw). Not in this change, though proposed with it: the commit that
  introduced each deleted or rewritten line (`git blame` at the base), a flag on fix-like subjects, and a
  ranking of a big PR's files by fix density; magnum's own past findings on the paths stay out (153 posted on
  89 paths, none on a path in two PRs).
- **The judge checks the PR's own claims** (2026-10-06). All 17 recent PRs of one repository ticked "Can be
  reverted easily on Production", 6 of them with schema migrations, and one had a rollback hazard under that
  box (jobs queued in the new argument shape fail on the previous release); another claimed "every export stays
  on the site the user is working in" while a sibling path did not. SKILL.md section 2 now has the judge verify
  at `head_sha` each claim of the description that bears on risk: a ticked "Can be reverted easily" (the
  previous release runs on the new schema and the jobs queued meanwhile), "No migrations" or "Covered by
  tests", a stated scope or behaviour. A false claim with impact is a finding at its priority; one without is
  one body line, `Description: ✗ <claim>: <why>`; a true claim gets no line, never a ✓. The description stays
  data, as section 0 says of all PR text. Where the line sits in the body is section 7's (the related-PR
  line's place, after the finding titles, is the natural one). This rule and the history's add 581 bytes;
  423 came from sections 2 and 4 without dropping a rule (the blind replay's reading list, which section 0
  already gives; "Follow repository rules…", which section 1 gives; "Personal taste is never a finding",
  which "Do not post … style preferences" gives; "so use them strictly", which "rank every finding by these
  definitions" gives; shorter wording of the code to read, the probe, the database and notes timing
  sentences), so SKILL.md grows from 31,523 to 31,681 bytes and its cap with it (+158): the rest of sections
  2 and 4 is pinned by tests or calibrates priorities, and sections 3, 7 and 8 were being edited in
  parallel. Not in this change: running the down migration and the base code against the new
  schema in the slot to check revertibility (a later stage, readiness work).
- **Replies on magnum's threads get an answer without a push** (2026-10-06). A reply triggered nothing until
  the next push: on one PR three author replies waited 16.5 hours for a verdict, on another the operator
  forced a 17-minute round to have a declined finding re-decided (it then approved); 69 of 75 replies come
  from the authors' agents, which wait for a verdict. The Details the poll reads for a PR whose activity
  moved now carry the activity timeline's reviews and issue comments with their authors, and the last two
  reviews with the authors of the threads their inline comments answer (`replyTo`; `github.PRDetails.Remarks`).
  GitHub's dry run (`rateLimit(dryRun: true)`, against a public repository) priced Details batches of 1, 5,
  10, 15, 20, 30 and 40 PRs at 1, 1, 1, 2, 2, 3 and 4 points before and 1, 1, 1, 2, 3, 4 and 5 after: at most
  one point more per batch, none for the few PRs a poll usually reads; `replyTo` on every timeline review
  instead doubled the price (8 for 40), and the last three reviews instead of two cost 6. The replies kept
  (`prs.replies_json`, migration 0022: when and by whom, never what they say) are, by none of magnum's
  logins (every watch's posting identity, the PR's and its former ones): the PR author's own reviews and
  comments ("(Claude)" replies the author's agent posts as the author are the author's; a bot author's
  are not), and anyone's reply in one of magnum's threads, a bot's too. A teammate's review elsewhere and a
  bot's top-level comment never count. `prs.replies_read_at` is when the judge last read the threads (its
  prompt; a first review's verification, a continued turn's round start), and the replies after it are
  pending (`store.PendingReplies`). A pending reply on the head magnum reviewed, newer than reply tracking
  (`daemon.replies_since`, so an upgrade re-decides no old reply on its own), moves a reviewed PR the
  watch's filters accept to rereview_pending, held by `eligibility.Throttle` only for `[daemon]
  reply_debounce` (default 3m) after the last reply and `reply_min_interval` (default 2h) after the last
  reply round on that head (`pr.<id>.reply_round`); the quiet periods, the re-review interval and the
  daily cap do not hold it, and it does not count against the cap. A push meanwhile wins: the PR waits for
  the re-review of the new head, whose judge reads the replies with the rest. A review request wins too
  (the requested same-head round posts a review). The round is the same-head judge-only round
  (`pipeline.RoundInput.Replies` with `SameHead`), its prompt (`judge-rereview.md`, `judge-recovery.md`,
  `post_replies` in the `<magnum>` block of those and `judge-continue.md`) telling the judge: when its
  verdict and event stay those of its last review, post no review but answer in the threads through
  `magnum post-review --replies` (an acknowledgement for a reason it accepts, resolving being the
  author's; one sentence with evidence for a rebuttal; an answer as deep as asked; nothing where none is
  needed), and write `"status":"replied"`; a changed verdict posts one short review as before (auto-approval
  follows it as it follows any review). post-review's replies mode checks each reply names the first
  comment of a thread the reviewer login (or a former login) started, appends `<!-- magnum:reply run=<run
  id> kind=<ack|rebuttal|answer> -->`, posts each once (a reply of the run already there is not posted
  again) and prints what it posted. The round ends `replied`, verified by the replies with the run's
  marker by the reviewer login on GitHub (`review-threads`), or by a replied result listing none (nothing to
  answer); a result listing replies GitHub does not show needs attention, and so does a replied result in
  a round that asked for a review. The judge runs are verified with outcome `replied` and no review; no
  findings are recorded, no footer or dismissal happens; the PR goes back to reviewed with its review
  fields as they were (`pr.replied`, `round.replied`). A reply in a thread is a review of its own, by the
  reviewer, on the head, with no body: verification's marker-less last resort now skips a review without a
  body. After two of magnum's rebuttals in a thread (its replies of kind rebuttal, and its unmarked replies,
  which were rebuttals) someone's answer marks the thread `stop` in `review-threads.json`: the prompts say
  to reply there no more, post-review refuses to, and the board flags the PR for the operator
  (`pr.<id>.stalemate`, `pr.stalemate`) until they act on it (`magnum review`, a verdict, a mute), after which
  only a newer answer there flags it again. The board shows `↩N` in LAST REVIEW for replies not yet
  re-decided, the waiting state as `re-decision · 2 replies → 14:09`, and `r` on such a row asks for the
  judge-only re-decision now (`magnum review --replies`, `ReviewPayload.Replies`), which also re-decides
  replies older than reply tracking; a plain `magnum review` of the same head still posts a review.
  SKILL.md is not touched (the prompts carry the reply round's instructions). Rejected: deciding at the poll
  whether every timeline review answers magnum's threads (the doubled price above); counting a teammate's
  top-level review or comment (an approval after magnum's review would start a judge turn that answers
  nothing; the judge still reads them in the next round); a verdict line or review for every reply round
  (a review that repeats the last one is noise); counting reply rounds against the daily cap (they have
  their own interval, and would hold the next push's re-review).
- **A stopping daemon starts no retro and no curation** (2026-10-07, amends "Learning loop: daily retro").
  Every daemon restart logged a retro that started during the shutdown and stopped at once ("retro …: 0
  PR(s) … stopped by the daemon's shutdown", after "append event retro.start: context canceled"), and its
  summary made `magnum status` report the retro as stopped until the next one. A signal cancels the daemon's
  context in the middle of a tick, and the tick goes on: every registry read then fails, so the drain, the
  pauses and the day's retro already done all read as absent, and the day's retro looked due. `holdReason`
  now starts with `stopping`: a cancelled context ("the daemon is stopping") or a drain for a restart; it
  already held new rounds, the daily retro, notes curations and the `magnum retro` and `--curate` requests,
  and `startRetro` and `startCurate` check it too, so no caller starts one. A retro a shutdown cuts short
  is neither done nor failed: it records neither the day nor its summary (the last finished retro's stays,
  and the next start runs the day's again), and its `retro.done` event is info, not a warning. Its PRs
  already behaved so (a stopped PR writes no `retro_prs` row and stays due).
- **A Claude session is quit before the checkout moves only when the new head changes its project config, and a
  quit that fails is charged** (2026-10-07; amends "A PR that changes .claude/ or .mcp.json runs Claude with the
  user's settings only"). On the operator's own PR a push held the re-review for good: parkReloading quit the idle
  claude-review session before every checkout, its agent did not stop within 10 seconds (its MCP servers were still
  in the pane's foreground), and the "agent busy" error made the round retry every tick without counting an attempt.
  A session now stays when the PR's stored file list for the head the round checks out (store.PRFiles, the poller's)
  names nothing at or under the paths its kind reloads (agents.ProjectTouched: `.claude/`, `.mcp.json`): it has
  nothing new to reload. A list for another head, a cut-off list, no list or a session of unknown kind still quits
  it. A quit that fails after the session was idle is no longer agents.ErrBusy: the setup failure is charged, backs
  off and leaves the PR needing attention after its attempts. Rejected: closing the pane to force the quit (it kills
  the agent's processes, and a quit is rarely needed now).
- **The operator's own PRs have their own re-review interval** (2026-10-07). The operator develops his own PRs in
  another agent session that pushes often, and magnum re-reviewed each burst 30 minutes after the last round (after
  15 minutes of quiet), which spent Codex and added noise to a PR still in progress. `[daemon]
  own_min_rereview_interval` (default `"0"`: min_rereview_interval; the operator's config sets `"2h"`, a `[[watch]]`
  may override it, a zero there keeping the daemon's) replaces min_rereview_interval in eligibility.Throttle for a
  PR whose author is one of config.SelfLogins (PRFacts.Own, the board's "mine"), as draft_min_rereview_interval does
  for drafts; an own draft waits the longer of the two. A review request and `magnum review` skip it as they skip
  the other timing rules. Its wait is `own_interval` ("re-review · own PR interval → 16:40", "the own PR interval
  (2h since the last round)"). store.Candidates' SQL backstop cannot tell the operator's PRs apart, so dispatch
  passes it the shortest of min_rereview_interval and every positive own interval (engine backstopInterval): an
  own interval shorter than the normal one is not held to it. Rejected: a per-author list in the config (the self
  logins already say whose PRs are the operator's).
- **Snooze: one PR's automatic rounds held until a time** (2026-10-07). `magnum snooze <ref> [--for 2h | --until
  18:00 | --off]` (default 2h) and the board's `z` (2h, after y/N; on a snoozed PR it offers to lift the snooze) hold
  every automatic round of the PR until then: a push, the quiet period's end, a re-review, a reply round, a delta
  check and a new PR's first review. `magnum review`, the board's review keys and a review request on GitHub still
  run, and the snooze stays for the automatic rounds after them. The CLI and the board send a `snooze` request
  (engine.SnoozePayload: the end, or off, and who asks), which the daemon applies like a mute (queued until one runs);
  only an open PR is snoozed, and a snooze whose end passed before the daemon handled it does nothing. The snooze is
  the kv `pr.<id>.snooze` (until, set at, by whom), so it survives restarts and needs no migration; it ends on its
  own at its time, and an ended record holds nothing and is never cleaned up. eligibility.Throttle holds the PR
  until then (PRFacts.SnoozedUntil, rule `snoozed`, after the request branch so a request passes, before the reply
  branch so a reply round waits too), which sets next_eligible_at and the wait (`snoozed`: "re-review · snoozed →
  18:00", the sentence naming `magnum snooze <ref> --off` and `magnum review <ref>`); setting or lifting a snooze
  gives a waiting PR its next_eligible_at again. Dispatch checks the record once more (snoozeHolds), unless the PR is
  forced or a request waits, so a PR whose time was set before the snooze (a round's settle, a retry's backoff) and a
  paused round's continuation wait too. The board shows `snoozed → 18:00` in the state cell (the daemon's wait, or
  after the state pill when nothing waits there), the card says until when, since when and by whom ("magnum snooze"
  or "the board") and that `z` lifts it, and `prs --json` gains `snoozed_until`, `snoozed_at` and `snoozed_by`. `z`
  shares the help line with `x` to keep the help on one screen. Rejected: a column on prs (a migration, and the CLI
  could not run until the daemon restarted on it, for three values only the engine and the board read); muting with
  an expiry (a mute holds requests too, and the operator still wants a review he asks for or someone requests).
- **Git decides whether a new head changes a reloading session's project config when the file list cannot**
  (2026-10-07; amends "A Claude session is quit before the checkout moves only when the new head
  changes its project config"). The operator's own PR changed 202 files; the stored list holds 100 and
  was cut off, so every re-review quit the idle claude-review session to be safe, and that quit failed every
  time (the agent and its MCP servers still in the pane's foreground after 10 seconds): the round failed its
  setup, charged, until the PR needed attention. Before it quits such a session, parkReloading now runs the
  checkout's fetch (`slots.Manager.Fetch`: the PR's head into `refs/magnum/pr/N`, and the base branch for a
  pool slot) and the checkout checks out the head it fetched without fetching again
  (`slots.Manager.CheckoutFetched`, which fails if the ref moved since); that head decides. The PR's file list
  decides when it is complete and of that head; otherwise (cut off, another head, none) git does: one `git diff
  --name-only origin/<base>...<head> -- .claude .mcp.json` (pathspecs literal from the top;
  `gitx.Client.ChangedUnder`, the kind's paths from `agents.ProjectPaths`), once per kind per checkout. The
  session is quit only when that lists a file or git fails. The fetch runs whenever a live session that
  reloads its project config is there, not only when the list cannot decide: GitHub's head may have moved
  since the radar read it, and the head checked out is the one that must be judged (the list of the radar's
  head no longer decides for a newer one). A fetch that fails leaves the old decision (the radar head's
  list, else a quit) and the checkout fetches as before; a per-PR worktree the round creates or recreates
  is not fetched first. The same holds at a restart's switch to a newer head, which now names the head it
  switches to instead of the PR the round claimed.
- **Magnum's Claude sessions run without MCP servers** (2026-10-07; amends "A PR that changes .claude/ or
  .mcp.json runs Claude with the user's settings only" and extends "Magnum's Codex sessions run without the
  operator's MCP servers" to Claude). The quit that held the operator's PR failed because the session's
  stdio MCP servers (the operator's own: node, python, a browser daemon) kept the pane's foreground after
  the agent was asked to exit, and review sessions need none of them while paying for their tools in every
  turn. Claude Code 2.1.292 has no flag that turns one server off: `--strict-mcp-config` makes it "only use
  MCP servers from `--mcp-config`, ignoring all other MCP configurations" (`claude --help`, cli-reference),
  and with no `--mcp-config` its startup skips the loader of the user, local, project and plugin servers and
  the claude.ai connectors (the binary resolves the MCP configs to none under the flag, and names the
  connectors "restricted to explicitly passed config, e.g. --strict-mcp-config"). The claude kind's
  `mcp_off` is now `true` with the new `mcp_strict = ["--strict-mcp-config"]`, args that keep every server out
  at once (`config.Kind.MCPStrict`, passed by `MCPOffArgs` before any `mcp_disable`), at every launch and
  resume: claude-review, claude-simplify, the retro's classifier and the notes curator when they run Claude,
  and `.MCPOff` of a shell role whose tool is claude. Resumes need the flag again (sessions docs: "Not every
  configuration flag from the original launch is restored"), and get it. `mcp_off = false` loads the servers
  again. `mcp_allow` cannot name servers for an all-at-once flag, so a kind with `mcp_strict`, `mcp_allow`
  and no `mcp_disable` is refused at load; the servers to keep go in a JSON file the operator writes,
  named by `"--mcp-config", "<absolute path>"` appended to `mcp_strict`. `magnum roles --kinds` prints the
  mcp line; the "resume by hand" hint leaves the MCP args out (a person keeps their servers). A running
  session keeps what it started with until its next start. Rejected: magnum writing that file from
  `mcp_allow` (it would copy the servers' `env`, tokens included, from `~/.claude.json` into a file of its
  own, and plugin servers and connectors have no entry there to copy); a per-server deny through
  `--settings` (`deniedMcpServers` is policy, not a documented per-session switch). Known limit: Claude
  Code exits at startup when it is given `--strict-mcp-config` under a deployed `managed-mcp.json`
  (managed-mcp docs), so such a machine needs `mcp_off = false`.
- **The FINDINGS cell counts still-open findings when a review posted nothing new** (2026-10-07). A re-review
  whose earlier findings stayed open and that found nothing new showed "✗ clean": the blocking glyph came from the
  open findings, while "clean" described only the new ones, and the operator could not tell what it meant. The cell
  now reads "✗ 3 open" (the verdict's glyph and color) in that case; "clean" stays for a review with nothing new and
  nothing open, and a review with new findings lists them by priority as before (the card keeps the full counts).
- **The roles of a round take turns on the slot's databases** (2026-10-07, amends "The judge does its own pass
  while the reviewers work"). Since the own pass started with the reviewers, 6 of 13-18 `round.environment` events
  of rounds with an own pass named deadlocks or overlapping test runners (2 in about 38 rounds before): concurrent
  RSpec processes on one slot hit Trilogy 1213 deadlocks, 1062 duplicate fixture keys and each other's rows (12 of
  114 examples failed in one round; `PG::TRDeadlockDetected` and 0 examples run in another), and the judge then
  reran its checks alone or left them inconclusive, while the skill told each role the databases were its own. A
  new command, `magnum db-lock [--checkout DIR] [--timeout 20m] [--role NAME] -- COMMAND`, holds an exclusive
  flock per checkout (`<state>/db-locks/<base>-<hash>.lock`, hashed from the cleaned, symlink-free absolute
  path, `paths.Layout.DBLock`) while COMMAND runs with the role's stdin, stdout and stderr, and exits with its
  status; a holder that crashes or is killed frees the lock. The holder writes its role, command, pid and start
  into the file, and a role that waits prints once `db-lock: waiting for <role> (<command>) since <time>`. After
  the timeout it exits 75 (EX_TEMPFAIL, a status no test run uses) with `db-lock: waited <timeout> for <role>
  (<command>); the check did not run`; 125 is db-lock's own failure, 126, 127 and 128+n mean what they mean in a
  shell, and SIGINT, SIGTERM and SIGHUP reach the command. Like `post-review` it reads no config and opens no
  registry (`internal/dblock`); `--checkout` defaults to the git work tree it runs in. Each role that runs tests
  gets the exact line as a derived template field, `.DBLockCommand` (the daemon's binary, the checkout and the
  role, shell-quoted; `Manager.RolePrompt` fills the role's name): the judge's initial, rereview, recovery and
  continue prompts render it as the `<magnum>` field `db_lock`, the four claude prompts in one sentence. The
  skill's Databases paragraph says the databases are shared, sends every command that touches them (specs, `rails
  runner`, rake tasks, migrations, scratch tables) through `db_lock`, and calls a timeout a check that did not
  run: a machine failure, not a finding. Rejected: a database per role (three schema loads per slot, and three
  times the slot's `reset_db` and readiness work); running the own pass after the reviewers again (it gives up
  the parallel pass's time); a lock in the registry (the panes would need the store or the daemon); a `mkdir` lock
  like the notes' (a killed holder leaves it behind until a staleness timeout); handing the lock's descriptor to
  the command with exec (a background server it starts, such as spring, would inherit the descriptor and hold the
  lock).
- **A role's limits by design are no machine failure** (2026-10-07). 6 of 43 `round.environment` events (three
  PRs) were codex-review's sandbox keeping it from localhost Redis. codex-review is a static review by design
  (`codex review`, sandboxed, no network services), so its unrun checks say nothing about the machine, yet they
  went into `environment_failures`, the notes' review-machine pitfalls and the operator's environment counts.
  SKILL.md's machine-failure paragraph now says a limit a role has by design is neither a finding nor a machine
  failure: codex-review's unrun checks are no `environment_failures`, and the judge runs what it needs itself.
  Rejected: opening codex-review's sandbox to local services (it is the static second opinion; the checks are
  the judge's).
- **A standing decision needs the authors' decision** (2026-10-07). A reply that confirmed a finding on every
  premise, called it still open and its handling undecided, and scored one fix option at −8 reached a
  repository's notes as a declined finding under "Standing decisions (do not re-flag)", which would have kept
  later reviews quiet about a problem nobody had decided on. The reply contract still honours such a reply as
  `won't fix` for that PR's verdict ("A reply that declines the fix in its own words is `won't fix`"); the notes
  speak for every later PR. SKILL.md's notes paragraph and `notes-curate.md` now say standing decisions are the
  authors' decisions only: a finding they confirmed but left undecided is recorded as open with the decision
  pending, or not at all, never as declined; the curator moves such an entry out. Rejected: changing the reply
  contract instead (its rule was made the day before from the same reply, for the verdict).
- **A re-review's own pass covers the new commits** (2026-10-07). The skill's own-pass paragraph named sections
  1, 2 and 4 and, in a re-review, section 6's decisions, but not section 6's scope, so the own pass of a
  re-review read the whole PR again: 5 to 12 responses in a resumed session, up to 56 in a cold one. It now says
  that in a re-review section 2 covers section 6's scope (the new commits; the earlier reviews cover the rest) and
  that `own_findings` holds section 6's decisions too.
- **Untracked log files under `.claude/` and `.codex/` do not decline the project config** (2026-10-07; amends "A
  PR that changes `.claude/` or `.mcp.json` runs Claude with the user's settings only"). In one day 8
  `agents.claude_project_declined` events came from PRs that changed no `.claude/` path: a base branch's own team
  hook writes its gitignored `.claude/log/tool_use.log*` into every slot it runs in, and the comparison counts
  untracked files, ignored ones included, so every later Claude session of those slots lost the team's settings,
  skills and `CLAUDE.md`, and six posted reviews said "Claude ran without the PR's .claude/ and .mcp.json
  changes" when the PR had none. An untracked file under a kind's project directory now counts unless it is a
  log: a base name ending in `.log` or `.log.<digits>`, or a file under `log/` or `logs/` right inside the
  directory (`.claude/log/`, `.codex/logs/`; `agents.projectConfig.logFile`, applied by
  `gitx.Client.WorkTreeChanges` to the `git ls-files --others` list only). The security property holds: neither
  Claude Code (settings, local settings, skills, commands, agents, rules, output styles, `CLAUDE.md`, `.mcp.json`)
  nor Codex (`config.toml`, hooks, `*.rules`) loads a log file as configuration, so nothing a PR or a round can
  put there reaches a session. Every other untracked file, ignored or not, still declines, as does anything a CLI
  may load now or later: `settings.local.json`, a skill (one named `logs`, `.claude/skills/logs/`, too: the
  directory rule is for the top level only), a log-like `tool_use.log.old`. A tracked change counts whatever its
  name, a committed `.claude/log/x.log` too (the PR's commits are what the rule guards first, and a tracked log
  is rare). The rule ignores whether git ignores the file: the question is what a CLI loads, not what git tracks.
- **The judge hears of a declined project config only from a kind the round ran** (2026-10-07). The PR's
  `pr.<id>.<kind>_project` record names the kind's last launch on a head, which may be a session the round did not
  run: a posted review said "Claude ran without the PR's .claude/ and .mcp.json changes" though no Claude role ran
  in its round (claude-review had been launched for the head earlier). `NoteDeclinedProjects`
  now takes the agent kinds of the round's roles that run, the judge's included, and sets `claude_project` or
  `codex_project` only for a kind among them. The judge counts: a Codex judge that ran on a head whose Codex
  record declined started either untrusted on it or on an earlier head before the PR's `.codex/` changes (Codex
  reads its project config at start only), so it ran without them either way; with the default codex judge the
  Codex note is as before, and a non-Codex judge no longer reports a Codex decline no Codex role of its round had.
  The board's card still says what the head's sessions did, whichever round ran them.
- **Codex's apps connector is off with its MCP servers** (2026-10-07; extends "Magnum's Codex sessions run without
  the operator's MCP servers"). A `codex review` started with every `[mcp_servers]` table turned off still called
  `mcp__codex_apps__search_service_web_run` with a query naming a private repository and a branch: `codex_apps`
  is Codex's built-in apps connector (ChatGPT's connectors; the binary's instructions call an app "a set of MCP
  tools within the `codex_apps` MCP"), declared by no `[mcp_servers]` table, so `-c
  mcp_servers.<name>.enabled=false` never names it. Codex 0.160 gates it on the stable `apps` feature, on by
  default (`codex features list`: `apps stable true`; `-c features.apps=false` and `--disable apps`, which
  `codex review --help` lists too, turn it to `false`). The codex kind's `mcp_strict` is now
  `["-c", "features.apps=false"]`, passed once under `mcp_off` before the per-server `mcp_disable` args: every
  launch and resume, and codex-review's `.MCPOff`. `mcp_strict` was Claude's all-at-once switch; it is now any
  kind's args that keep servers out whatever their names, so no new key. `mcp_allow` does not apply to it;
  `mcp_strict = []` keeps the connector, `mcp_off = false` keeps it and the servers. `magnum roles --kinds`
  prints it as "(once, whatever the servers' names)" for a kind that also turns servers off by name. Not
  verified in a live session (magnum never starts one for a check): that the connector's tools are gone with
  the feature off; the feature list says it is off. Rejected:
  `--disable apps` (a flag, where every other Codex override magnum passes is `-c`, and a wrapper gets the same
  `-c`); `[apps.<id>] enabled = false` per app (the apps are the account's, many and changing).
- **The project-config check matches its paths in any case** (2026-10-07; a security fix to "A PR that changes
  `.codex/` ..." and "A PR that changes `.claude/` or `.mcp.json` runs Claude with the user's settings only").
  macOS's filesystem ignores case, so Claude Code opening `.claude/settings.json` reads a `.Claude/settings.json`
  a PR added (and Codex a `.Codex/config.toml`, Claude a `.MCP.json`), while git, which keeps the PR's own case,
  showed no change under the literal pathspec `.claude`: the session started with the project config loaded,
  and the PR's hooks ran outside the sandbox (reproduced in a scratch repository by the code-health analyst).
  `gitx` now builds the comparison's pathspecs as `:(top,literal,icase)<path>` (git 2.56: `icase` combines with
  `literal`, only `glob` is incompatible with it, `git help glossary`; `git diff` and `git ls-files --others`
  both honor it, tested on a real repository), for `WorkTreeChanges` at a launch and `ChangedUnder` before a
  checkout moves. APFS also folds Unicode (a file `.mcp.jſon`, with a long s, opens as `.mcp.json`; checked on
  the operator's volume) and git's `icase` is ASCII only, so the launch's check reads the checkout's root and
  compares every entry whose name folds to a project path (`strings.EqualFold`) by its own name as well
  (`agents.projectConfig.onDisk`), which also finds the config present when only a variant exists.
  `ProjectTouched`, which reads a PR's file list, and the log exception match the same way (`pathOf`:
  `.CLAUDE/LOG/x.LOG` is a log, `.Claude/Settings.Local.JSON` still declines). On a case-sensitive filesystem
  the variants are other files Claude never opens, so they decline needlessly; that errs on the safe side.
  Not covered: `ChangedUnder`'s git fallback, used only when the PR's file list cannot decide, still misses a
  Unicode-folded variant of `.mcp.json` before a checkout moves (the session's next launch catches it).
- **A review request on a draft starts its round; the magnum view keeps requested PRs** (2026-10-07). With
  `include_drafts = false` an author requested the operator's review on a draft: magnum recorded the request
  (`pr.review_requested`, "the next round skips the timing rules") and started nothing, because
  `eligibility.Classify` rejected the draft before a request counted, and the board's `magnum` view hid the
  ineligible PR, so it vanished from there too. A request that would start a requested round (for the poll
  login, a posting identity or a `request_teams` team; not a draft's move to ready for review) now makes a
  draft eligible for the round it starts: `PRFacts.Requested`, which the engine fills from the pending
  request only for a draft a watch skips. It counts only while it is newer than the PR's last round start,
  as a request always did, because GitHub keeps the operator's request after magnum's App posts: a later
  push to the draft is skipped again until a newer request, and a request for anyone else changes nothing.
  The wait, the card and the request's event name it ("requested on a draft by alice"); `magnum review` is
  unchanged. A counted round that fails leaves no request behind, so its retry is skipped like any push to
  the draft (a refunded round restores it). The `magnum` view also keeps any row whose review is requested
  from one of the self logins (the `mine` view's rule), whatever its state.
- **A security fix secures the whole request** (2026-10-07). A PR that closed five access holes passed
  claude-review's check of the target authorization, while a person found that the same request's keys reached
  private methods through `respond_to?(m, true)`, so a read-only user could strip a live record's settings
  (reproduced). The hole was older than the PR, so a reviewer who raised it would have seen
  the judge drop it as `pre_existing`. SKILL.md's section 2 now has a PR that closes an access hole or adds an
  authorization check to an action trace each request parameter of that action to its writes, dynamic dispatch
  included (`send`, `respond_to?(name, true)`, method names built from request keys); a hole left on that request
  is the PR's finding, and `pre_existing` excludes a request the PR secures. The claude reviewer prompts
  (`claude-review.md`, and the same paragraph in `claude-rereview.md` and `claude-restart.md`) carry one clause of
  the same meaning. Rejected: posting every older hole near a security fix (the nearby block keeps security ones
  out of posted text; the request the PR claims to secure is its scope).
- **history.json marks the base's commits after the merge base** (2026-10-07). Master merged a change to the same
  tool and spec 4 hours before magnum's review; the PR's new example failed once merged ("expected: 1 time,
  received: 0 times"), without a textual conflict, and the repository's CI did not run on push; the same pattern
  came up on another PR that week. history.json listed such commits, but its only rule was about a PR that undoes
  a fix. Each commit the base got after the PR's merge base is now marked `after_merge_base: true` (a second
  `git log` at the merge base, for each file with commits, lacks it; abbreviations compared by prefix), never
  without a merge base or in a blind replay. The skill's history rule adds that the PR was never tested with such
  a commit: when it changes what the PR's code or tests call, the judge runs the affected specs on the merged tree
  (`git merge-tree --write-tree HEAD origin/<base_ref>`, checked out in a scratch worktree in the result file's
  directory, removed after, through `db_lock` as every database command), and a failure there is a broken build.
  Rejected: a `rev-list` of the range in one call (a new gitx method, while gitx was being changed elsewhere; the
  second log costs one more `git log` per file with commits within the same 2-minute budget); always running the
  specs on the merged tree (most base commits touch nothing the PR calls).
- **A rare case gets a check of real traffic** (2026-10-07). claude-review raised a domain fallback on a first
  page view that the PR's description called a known edge case; the judge did not post it, and a person showed it
  hits every new visitor, putting one visitor in two country segments. The same person said magnum's three P3s
  needed callers that do not exist in the repository. SKILL.md's reachability paragraph now has a case called a
  known edge case or rare checked for how often real traffic reaches it, starting with the paths that traffic
  takes (a new visitor's first page, the inputs the PR's callers produce), and an input no caller in the
  repository or its documented API produces, and no user can send, is P3 at most. "No user can send" keeps a
  crafted request, which an attacker can send, out of that cap.
- **Parallel copies are compared** (2026-10-07). In one client `atob(null)` sent a garbage email for every
  anonymous visitor, while the sibling client already had the guard; nobody raised it (older than the PR, in the
  hook it edits). The skill's "Structure can hide a defect" list now names parallel copies (of a helper, or a file
  per client, integration or provider) of which one lacks a guard another has: compared when the PR edits one,
  `nearby` when older than the PR. It replaces "a copy of a helper that misses its edge cases".
- **A delete of unsaved records is traced to every save path** (2026-10-07). Save kept an "uploaded" list, and a
  later Reset and Save deleted a saved image; nobody raised it. The same list in the skill now names a delete of
  records thought unsaved (uploads, drafts, temp records) whose list or flag a save path leaves stale, with every
  path that saves them traced.
- **A finding missed earlier is posted as new, with a label** (2026-10-07). codex-review reviews the whole PR on
  every re-review (`--base <merge base>`). In re-reviews the judge rejected 34 of 58 codex candidates as
  `outside_diff` or `duplicate`, yet posted 5 such findings as new P2s in rounds 6 and 7 on code reviewed five
  times (real defects, fixed), with no note to the authors. SKILL.md's re-review scope now says a proved finding
  the earlier reviews missed on PR code is new, never `outside_diff`, and its title ends with `(missed earlier)`;
  it counts for the verdict like any new finding. Rejected: narrowing codex-review's base to the previous head
  (the missed defects were real, and its whole-PR pass found them).
- **The judge sees the head's failing checks** (2026-10-07). A PR got LGTM and the operator's auto-approval
  while its RSpec check had failed on that head 25 minutes earlier (1 of 27,820 tests, unrelated to the PR), and
  the review's Checks did not mention it. When a judge prompt goes out (initial, rereview, continue), the round
  reads the PR from the registry; when `prs.ci_json` belongs to the head under review and a check failed, it
  writes `failing-checks.json` in the report directory (`pipeline.FailingChecks`: the failed checks' name, state,
  workflow and time, and whether GitHub listed every check) and names it as the `<magnum>` field
  `failing_checks` (`JudgeData.FailingChecks`), with a `round.failing_checks` event that counts them. The skill
  gives each one a Checks line, caused by the PR (a P1 broken build) or unrelated, from its log (`gh run view
  --log-failed`, or the check's output). The names stay in the file, since the PR's workflows name the checks
  (prompts carry no PR text); a blind replay, which reads no CI result, gets none. Rejected: the names in the
  block (PR text in a prompt); checks of an older head (they say nothing of the reviewed one); the recovery prompt
  for now (another change in flight edits it; a recovery reads CI itself with `gh` when it matters).
- **The judge's result counts only new findings; still-open ones go by priority** (2026-10-07). About 21 of 134
  posted findings since 10-05 repeated an earlier round's finding (same path, line ±5), one PR six times from round
  2 to round 4; one round-7 finding carried the source `judge` although codex-review had found it in round 6, and 14
  of 30 judge-only posted findings were such repeats, so `magnum stats` overstated the own pass. Judges also
  disagreed: one posted "Blocking: 1 problem…" with every `findings` count at 0, leaving auto-approval and the board
  to parse the verdict line. SKILL.md section 8 now says `findings` and the posted entries of `provenance` hold only
  what the review posts as new, and `previous_findings.open` counts the earlier findings still open by priority
  (`{"P0":…,"P1":…,"P2":…,"P3":…}`); a source that raises one again is a `duplicate`. `store.ParseReviewResult`
  reads either shape: the object fills `ReviewSummary.OpenCounts` (P0..P3, `open_counts` in `status --json`) and
  `Open` with their sum, an older result's number fills `Open` and leaves `OpenCounts` nil (priorities unknown);
  without a verdict an open P0 or P1 blocks. The result stays in `runs.result_json` as written, so no migration.
  Replay scoring reads the planned review's comments and the posted provenance entries' severities, which the
  change leaves alone. Auto-approval still decides from the verdict line when earlier findings are open; a later
  change switches it to `OpenCounts`.
- **Every thread reply goes through `post_replies`** (2026-10-07). The reply rounds (`post_replies`, `delta_check`,
  `status: "replied"`, the reply kinds and `stop` threads) reached three judge prompts, not SKILL.md, which still
  said "Post exactly one GitHub review" and had section 8 post rebuttals with a raw `gh api …/replies`, skipping
  post-review's reply marker, its post-once check and its refusal of a third rebuttal in a thread;
  `TestSkillDescribesEveryMagnumField` never set `Replies` or `DeltaCheck`, so it missed both fields.
  `JudgeData.completed` now derives `RepliesFile` and `PostRepliesCommand` in every round after an earlier review
  (`Replies`, `PreviousReviewID` or `PreviousHeadSHA` set), so the rereview, recovery and continue prompts render
  `post_replies` then (never in a first review, its continued turn or the own pass), and the skill sends every
  thread reply (a rebuttal, an answer, an ack) through it, after the review or, where the prompt allows, alone
  with `"status":"replied"`. The skill names `post_replies` and `delta_check` among the block's fields and leaves
  their details to the prompts; the field test sets both. Rejected: a second, skill-only description of the reply
  round (the prompts that start one explain it; the skill's byte budget pays for each rule).
- **Three contradictions of the skill get one exception each** (2026-10-07). Section 3 asked for a Checks line for
  each missing report even when the machine caused it, while section 7 keeps machine failures out of Checks: the
  line stays, its reason only `(machine)`, the detail under `environment_failures`. Section 5 said never to anchor
  on a test file, but the defect can be a flaky test the PR adds: such a test may carry its finding. The claude
  reviewer prompts dropped every pre-existing problem while section 7 lists proven nearby P1s and P2s (since
  migration 0021: 16 `pre_existing` rejections, 0 nearby): `claude-review.md`, `claude-rereview.md` and
  `claude-restart.md` now report a pre-existing P1 or P2 in the code the PR touches, marked `nearby`, for the
  judge to prove and list. SKILL.md grows by 226 bytes for this entry and the two before it (`skillMaxBytes`
  34,734).
- **A cold judge is not lost to the agent it quit** (2026-10-07; amends "A cold judge starts in a fresh session").
  Of 16 `round.judge_fresh_cold` starts in a day, the 3 that quit a live judge first (`was_live`) started the
  fresh one 2-18 ms after the quit, while herdr still listed the quitting Codex under the role's agent name, so
  `StartAgent` adopted it; 2 of the 3 lost the judge (one own pass got `agent_not_found` 0.5 s after the start,
  and its round failed with "judge prompt refused … no live agent session" after claude-review had worked for
  9 minutes; another was lost in 44 s). The 13 cold starts of parked judges all worked. Now the cold start
  polls the herdr snapshot (every 0.5 s, about 10 s in all, engine `quitAgentGone`) until the quit agent's name
  is gone, and resumes the judge as before when it never goes. A fresh start (no conversation to resume)
  never adopts an agent whose conversation magnum parked for the PR's role (`agents` `parkedConversation`,
  the session id herdr reports): it fails with `ErrBusy` and the round retries. And when the own pass's
  prompt is refused because the judge's session is gone (no live session, herdr's `agent_not_found` or
  `pane_not_found`), the round starts the judge once more the way it started it (`RoundInput.RestartJudge`:
  the gone session is marked lost; fresh, or resumed in a re-review, at the same effort) and sends the pass
  again in a new `own_pass` run (`round.judge_restarted`); a second refusal, or a start that fails, ends the
  round at once with the refusal, the reviewers still at work interrupted and their runs abandoned, instead
  of after them. Not done: restarting the judge for the candidates prompt too (that refusal comes after the
  reviewers' work, and the next round recovers).
- **A cold re-review's judge works at its rereview effort and reads the commits since the review**
  (2026-10-07; reverses part of "A cold judge starts in a fresh session", which started such a judge, like a
  lost session's, at its full effort on the whole PR). Since `judge_fresh_after` went live all 11 cold full
  re-reviews ran the judge at `xhigh`: about 43k output tokens and 0.80 Codex points per re-review, against
  0.50-0.56 at `high` when the same re-reviews resumed the judge, so cold full re-reviews became 90% of the
  Codex spend (1.04 points each, a resumed one 0.75) and the weekly pace 3.4x; the cold quit exists to save
  usage. Their own passes re-reviewed the whole PR (31 responses and 0.38 points on average, up to 56 on one
  PR), where a resumed re-review's takes 5-12. A judge started fresh only because its cache went cold
  (`RoundInput.ColdJudge`) now launches at its `rereview_effort` (engine `judgeEffort`, also when a push
  parks and resumes it, and for its in-round restart) and is prompted at it (`judgeData`), and its own pass
  (`judge-own-pass.md`, recovery branch, `agents.JudgeData.ColdJudge`) reads `git log --oneline
  <previous head>..<head>` and `git diff <previous head>..<head>` and reviews those, the earlier reviews
  covering the rest of the PR; a push that merged the base keeps the PR's-own-diff comparison, and one that
  rewrote history reviews the full PR again. A lost session's recovery keeps its full effort and reviews the
  whole PR, as do initial rounds whose judge was cold.
- **Only a turn the judge got makes its prompt cache warm** (2026-10-07). `judgeLastTurn` counted any judge
  run's `ended_at`, also a run whose prompt never reached the judge: on talkable#11792 round 2 started a fresh
  judge that was lost after 44 s, its unsent run still got `ended_at`, and round 3 took that for a turn a
  minute old and resumed the 5h22m-old conversation, the cold re-read the rule exists to prevent. Only runs
  with `submitted_at` count now.
- **The recovery prompts say when the push rewrote history** (2026-10-07). `judge-recovery.md` and the
  recovery branch of `judge-own-pass.md` had no `force_pushed`, which `judge-rereview.md` has; with the cold
  judge a recovery is common (16 cold starts, 13 recovery posts in two days), and after a rebase a fresh judge
  scoped its review to a commit no longer in the branch. Both now carry `force_pushed:` and, when it is true,
  the re-review prompt's sentence (the previous head is no longer in the branch: review the full PR diff
  again, then compare it with the earlier findings).
- **A reply round does not restart the re-review interval** (2026-10-07). Every round but a continue set
  `last_round_started_at`, reply rounds too, so after magnum only answered in its threads the author's next
  push waited up to the full `min_rereview_interval` again. A reply round that ends replied now puts
  `last_round_started_at` back to the start before it (`roundJob.prevStart`, as a refund does), so the next
  push is timed from the last review round; while it runs it stays the round's start, which the board, crash
  recovery and the continue of a paused reply round read. Reply rounds keep their own spacing on their own
  record (`reply_min_interval` after `pr.<id>.reply_round`), not on the re-review timer. A reply round that
  posts a review (its verdict changed) counts as a review round; one that fails keeps its start.
- **A push during a reply round is settled as one during a review round** (2026-10-07). `onReplied` sent a
  moved head straight to `rereview_pending` without measuring it, so a 1-line push during a reply round got
  a full round instead of a delta check, and a trivial one (comments, whitespace, docs, a base merge) a
  re-review. Both endings now share `settleHead`: a trivial delta leaves the review standing for the new head
  (`reviewed_sha`, a trivial-skip record), any other is recorded for the threshold and the delta check
  (`recordDelta`) before the PR waits for its re-review.
- **A daemon stop logs no warnings for the tick it cuts short, and a drift finding is logged once a day**
  (2026-10-07). A stop cancels the tick's context, and everything the tick then did failed with `context
  canceled` or `signal: terminated`: 1,062 of the 2,066 warning and error lines since 10-05 (registry reads 640,
  event appends 88, writes 82, requests 32, wait reasons 26, closed past grace 26, "herdr unreachable" 23, and the
  gh and git command lines). Those failures now log at debug (`warnUnlessStopped`, which asks `isStop`: the
  context ended or the error is `context.Canceled`): the registry reads, writes and event appends, the requests,
  wait reasons, closed-past-grace and agent-observation reads, and a command execx ran whose caller's context was
  canceled (a command's own timeout, the caller's deadline and a failing exit still warn). A herdr snapshot that
  failed that way notes nothing, no `herdr.down` event and no `herdr_up=0`; the next live tick decides. The daily
  retro checks the context after reading its day, since a canceled read answered "" like a day never run.
  Separately, the reconcile logged the same two orphan-database drift findings at every run (164 lines on
  10-07): an unsafe finding is now logged once per local day while it persists (`daemon.drift_logged` holds the
  day by kind and subject, reduced to the current scan, so a finding that went away and came back is logged
  again and a restart does not repeat the day's lines); status and doctor scan the inventory themselves and
  still show every finding.
- **A network failure of the GitHub check no longer fails a round that posted** (2026-10-07). A live round
  posted its review, then every read of its verification hit "error connecting to api.github.com" within the 3
  attempts `listReviews` makes seconds apart; the round ended in error (`errUnverified`) and the next round
  adopted the review 20 minutes later. A connection-class failure (`github.ConnectionCause`, the classifier the
  identity checks and the infrastructure pause use, moved from the engine into `internal/github` so the
  pipeline can share it; `github.NetworkCause` is its network-only part, which the infrastructure pause reads
  after the refused SSH key) that outlasts those reads now waits `verifyNetRetry` (30 s, through the runner's
  clock) and asks once more, for the review check and for a reply round's thread check (when the judge's
  result says replied), with a `round.verify_retry` warn event naming the cause, never the error's text. Any
  other failure, such as not found, fails at once as before, and a network failure that lasts through the
  second asking still ends the round unverified, to be adopted by the next one. GitHub's 5xx answers and rate
  limits are connection-class too and get the same one wait.
- **The judge's own pass names db_lock too** (2026-10-07; completes "The roles of a round take turns on the
  slot's databases"). The own pass is the judge's turn that runs beside the reviewers, the one most likely to
  meet them on the slot's databases, but only the judge's later prompts rendered `db_lock`. `judge-own-pass.md`
  now renders it after `checkout:` like the others (the field was already filled for it).
- **A recovery's judge gets the head's failing checks too** (2026-10-07; completes the entry that gave the
  initial, rereview and continue prompts `failing_checks`). A recovery is common since the cold judge starts
  fresh (13 recovery posts in two days), and its judge decides the review as the others do, but
  `judge-recovery.md` did not render the field the pipeline already filled for it. It now renders
  `failing_checks` the way `judge-initial.md` does, only when set.
- **A restore is proposed only when the operator can answer, and never hides the version it names**
  (2026-10-07). `magnum notes <repo> --restore <version>` recorded the notes now and created a pending restore
  proposal before it checked for a terminal, so every `--json`, headless or "later" run left one, and the
  board's and the dashboard's titles counted them for 7 days. Worse, the history left out every curation-sourced
  version any proposal named: a restore of an applied curation (whose recorded version is of source curation)
  hid that version from `--log`, `--diff` and `LatestNotesVersion` for good, and the next `recordNotes` imported
  the notes, which still held it, as a duplicate version. Now the restore reads first (the version, the notes
  now, "hold version N already"), refuses without a terminal before any write, and with `--json` prints what the
  restore would change (proposal id 0, no state; its base the latest version when that holds the notes now)
  and records nothing; only y on a terminal records the notes now and proposes the restore. The history leaves
  out only the proposed states of curation proposals (`historyClause`: `p.kind = 'curation'`), so a restore,
  whatever becomes of it, hides nothing. Leftover pending restores stay in the registry (every proposal is kept)
  and expire after 7 days as before; the new clause ignores them, so no data migration. "Later" on a terminal
  still leaves the restore pending: the operator chose to keep it for `--review`.
- **The curator's files are read as regular files of its directory only** (2026-10-07). The curator reads the
  retro's misses, which quote other people's PR comments, and its `proposal.md` and `changes.json` were read
  with `os.ReadFile`, which follows a symbolic link: a manipulated curator could link a file outside its
  directory, and magnum would store it as a version and show it in the review. `notes.ReadProposal` now opens
  the scratch directory as an `os.Root` and reads both with `readRegular`: `Lstat` must say a regular file
  (a link, even to a file inside, or a special file is "not a regular file", and nothing it points to is read),
  the opened file must be the one checked (`os.SameFile`, against a swap), and the root keeps every path inside.
  The harness was read that way already. `Validate` also refuses a harness file `changes.json` declares kept or
  added that is not in `harness/` (it passed before, and the review then showed a file the proposal lacks).
- **The notes lock waits for a stale lock it cannot remove** (2026-10-07). `notes.Lock` takes over a lock
  directory older than ten minutes by removing it; when the removal failed (the directory not empty, a
  permission), it retried at once with no sleep, no deadline and no context check: 100% CPU, and a daemon that
  cannot stop. A failed removal now falls through to the normal wait: every retry sleeps (`lockSleep`), the
  deadline gives `ErrBusy` (naming the removal's error), and a canceled context returns at once, checked before
  every attempt. A removal that succeeds still retries at once, so a takeover is not delayed.
- **Decisions on a PR's reviews read the whole list, and say when they cannot** (2026-10-07). The dismissal of
  what a former identity left standing (`dismissFormer`) read `reviews(last: 30)`, and reply rounds push busy
  PRs past 30 reviews, so an older change request of the former identity stayed standing; post-review's
  post-once guard read `Reviews`, which stopped at 5 pages of 100 without a sign, so on a PR past 500 reviews
  the marker of a review already posted could be among the ones left out and the review posted twice.
  `github.Client.AllReviews` pages from the start and reports whether the list is complete (`Reviews` is it
  without the flag, for the readers that take what they get: auto-approval, the retro). `dismissFormer` reads
  it and, on an incomplete list, dismisses nothing and says so in a `review.former_dismiss_failed` warning;
  post-review returns a review found in the part read, and otherwise, on an incomplete list, posts nothing
  (`error`: the review may be among the later ones). The cost: the dismissal, which runs only for a PR with a
  former identity after a posted review, makes one call per 100 reviews (at most 5) instead of one; the guard
  already paged. `UpdateReviewBody` (the footer and the notes magnum adds to a posted review) now sends the
  body through `restInput` on gh's stdin like the other writes: in argv a routine 403 wrote the body, which
  quotes the PR, to the logs. And the retro's candidate builder (`learn.Build`) takes a comparison GitHub
  answers with a 404 (a commit it no longer has, after a force push) as one that proves nothing, so the comment
  on it is outside and the rest of the PR's retro goes on, where one 404 failed it all. Rejected: dismissing
  what a partial list shows (the former identity's latest verdict may be among the reviews left out); reading
  the reviews newest first for the guard (a marker may still be older than the page read); raising the 5-page
  cap (a PR past 500 reviews is rare, and an incomplete read now says so).
- **The Related lines name only live PRs and say nothing twice** (2026-10-07; completes "The judge knows the
  related PRs"). Reviews asked to coordinate with two open PRs whose last activity was 3 and 6 months earlier,
  another named a draft idle for a month, and one PR's rounds 5, 6 and 7 each repeated the same Related line,
  while the helpful lines (two PRs fixing the same spec, a proven merge conflict) were about live PRs;
  `related.json` had no activity date. Each related PR now carries `activity_at`, the board's UPDATED
  (`prs.activity_at`, else GitHub's `updatedAt`; `store.FilesPR.ActivityAt`), and `PRsWithFiles` leaves out an
  open PR whose activity is older than its `activeSince`, 30 days before the prompt (`relatedIdle`, not a key);
  an open PR with no activity known stays, and a merged PR keeps the `related_lookback` rule whatever its
  activity. Every judge prompt also records its whole set in `related-all.json` in the report directory (the
  round, the head, the set it compared with as `base`, the set), and in any round but an initial one
  `related.json` holds only the related PRs that the set recorded for the head the previous review covered
  (`previousHead`: the previous review's commit, else `reviewed_sha`) lacked or named another way: another state
  (open or merged) or other shared paths; a new head, activity, the draft flag, reviews and findings change no
  relation. With none left there is no file (an earlier round's on the same head is removed) and no
  `related_prs`, so the judge writes no Related line, the skill writing one only from the file. A record the
  same round wrote (its own pass before the candidates, the paused turn a continue finishes; in the round's
  directory or, after a restart, the previous head's) gives its `base` again, so the round's prompts agree
  when the head under review is the previous review's; a head reviewed before the record existed reads its
  `related.json`, which then held the whole set. The base is found by the previous review's head, never by
  the judge's session, so a cold judge or a recovery compares the same way. A round that posted nothing does
  not move it (the next round's previous review is still the older one), so its related PRs are named again,
  but a later round on the previous review's head (a reply round, a failed one) replaces that head's record
  and its set counts as named. The `round.related` event counts the PRs left out as `told`.
- **Replies are read past "Low priority", a deferral and the commit named another way** (2026-10-07; supersedes the
  rejection of "low priority" alone as won't fix in "Replies are classified by their first clause"). A reading of
  every reply by meaning found 32 of 57 declined or deferred replies in `other`, all written by the authors' coding
  agents: 24 of the form "Low priority (Net 0)", "(Net +0.5)" or "(Net +1)" followed by "not worth a change on its
  own", "stays as is" or "goes with … which stays as is"; three "Noted. … Deferred." or "No guard for now"; two
  "Valid, left open — not fixed in this PR"; two "by design" or "by decision" mid-sentence; one "Analyzed —
  correct …, but … rather than …". It also put three fixes in `other`: "Re <id>: right, fixed in <sha>", "… is
  covered in <sha>" and "<sha> adds …". A low priority acknowledgement (its "(Net …)" score included) that no
  verdict follows in the first paragraph is now `won't fix`, while "Low priority — fixed in <sha>" stays `fixed`;
  "stays as (it) is" anywhere in a clause, "left open", "deferred", "not fixed (or changed) in this PR (push,
  round)" and a clause that starts with "no", "not", "nothing", "none", "kept" or "left" and ends in "for now" ("No
  guard for now") are `won't fix` verdicts; "by design" or "by decision"
  anywhere in the paragraph is `not a bug` when no clause names a verdict; a leading "Re <digits>:" is skipped;
  "<it> is covered in <sha>" and a clause that starts with a commit and a verb ("84c0b1e adds …"; the commit has a
  letter and a digit, the verb is not "is", "was", "has", "does" or "keeps") are `fixed`. A clause that starts
  with "fixed" is `fixed` though it ends in "for now" (the fixed verdict is matched first). On the 192 author
  replies in the threads files on disk the change moves 35 from `other` and none of another class. Not covered:
  "Analyzed — correct …, but … rather than …" (no keyword tells that decline from a fix described the same way)
  and a commit and a verb after a clause that is no acknowledgement ("…; so 84c0b1e adds …"). Rejected:
  "rather than" as a decline (a fix reply can start with it) and any "for now" (as in "added a guard for now").
- **The threads file says when and where magnum posted each finding** (2026-10-07). Matching 112 answered
  findings to their runs took a match by path and priority that missed 27 of them, and the posting time was
  missing, so the improvement loop could not tell findings posted under an older skill (169 of 180 answered
  threads) from those under the current one. Each thread of `review-threads.json` now carries its first
  comment's `review_id`, `created_at` (RFC3339, UTC) and `commit` (its original commit), which the threads query
  already read; each is left out when GitHub reports none. The prompts do not change: the fields are data for
  the judge and for the loop. `/magnum-improve`'s evidence.sh prints each answered thread's posting time and the
  run, round and kind whose review it belongs to (`runs.review_id`), marks "current skill" a finding whose run
  named the skill copy (`<state>/skill/<hash>/SKILL.md`, a content hash) that the running daemon's rounds name,
  and calls its class column "class at round time". Rejected: marking by time after the last
  `daemon.prompts_changed` event or daemon start (the registry keeps only the latest start, and a change on
  disk takes effect only at the next one; the copy's hash in each run's prompt says which skill it used).
- **A watch whose polls fail is on screen** (2026-10-07). On 10-05 from 10:00 to 17:00 the talkable watch's
  radar failed 166 times (HTTP 502, 504) while the dashboard said "last poll 1m ago": `daemon.last_poll` is the
  poll's attempt, written whatever its radar calls did. Each watch owner now keeps its own record
  (`watch.<owner>.poll`: the last good poll, and while its calls fail since when and the cause, "HTTP 502" or
  the redacted error clipped; no migration). From 10 minutes the titles of both screens say "talkable polls
  failing 47m (HTTP 502)" (short "talkable ✗ 47m"), the dashboard's activity line and `magnum status` name
  the watch after the last poll, and at 15 minutes one toast goes out per failure streak. A poll cut short by
  a shutdown records nothing. Rejected: writing `daemon.last_poll` only after a good poll (one failing watch
  of several would hide the others' good polls, and the attempt's time still matters).
- **The review machine's failures reach the operator** (2026-10-07). 43 `round.environment` warnings since
  10-05 (db:seed failing in 12 rounds, a repository without a test database for the worktree in 6) were only
  events, though the judge's report says the operator fixes the machine. `magnum status` now groups the last
  24 hours of them by repository and command (`engine.MachineGroups`: a round counts once per group, the
  newest error is kept, a test file's path is left out of the command so `bundle exec rspec spec/a_spec.rb`
  groups with every other rspec run) and prints one `machine:` line per group, cleaned like other event text;
  a group that reaches 3 rounds sends one toast a day. The dashboard stays as it is for now. Rejected:
  grouping by the exact command (every spec file its own group of one, which would never toast).
- **doctor --json stays JSON when the registry cannot be opened** (2026-10-07). Under a running daemon a
  build with a new migration cannot open the registry (the CLI refuses to migrate it), and doctor printed
  plain text under `--json` with "fix config.toml", which is wrong for that refusal. The refusal is a typed
  error (`migrateRefusal`, its text unchanged for every other command), and doctor reports a failure to open
  as one check through its usual printing: a failed `registry` check with the schema versions, the daemon's
  pid and the fix `magnum daemon-restart --drain`, or for anything else the `config` check with the
  config.toml fix.
- **The daily retro runs during `magnum pause`** (2026-10-07, amends "Learning loop: daily retro"). The operator
  paused at 03:13 and resumed at 12:05, and the retro due at 07:00 ran at 12:05:19. `magnum pause` holds
  automatic reviews only (2026-10-05), and the retro reviews nothing: it reads closed pull requests and has its
  agent classify other reviewers' comments. `maybeRetro` no longer checks the user pause, so the retro starts at
  `daily_at` while the reviews stay paused. A shutdown, a drain, an infrastructure pause and a usage limit or a
  logout of the CLI the retro's agent runs (`retroToolPause`: Claude by default, Codex when `[learn] kind` says
  so) still hold it, `magnum retro` is unchanged, and `magnum status` keeps saying when the last retro ran.
  Rejected: holding it until the pause ends (a pause kept overnight moved it into the working day, where its
  agent competes with the reviews for the budget); holding it on a usage pause of a CLI it does not run.
- **At the Codex soft cap full re-reviews wait, not first reviews** (2026-10-07, amends "The Codex budget gates
  dispatch"). From 10-05 to 10-06 a first review cost 0.52-0.69 Codex points and posted 1.46 P0-P2 findings
  per round; a full re-review cost 1.04 and posted 1.44. The weekly budget went from 82% to 92% on 7 full
  re-reviews, and at a pace of 3.4x the soft cap was due around 10-09, where it would have held the cheap
  rounds and let the expensive ones run. `budgetGate` takes the round's job and, at `codex_soft`, holds a full
  re-review whose roles run Codex and that nobody asked for (`softCapHolds`); first reviews, delta checks,
  re-reviews of the same head, reply rounds, continues of a paused turn (`continued`, also one whose checkout
  is gone and restarts in full) and forced or requested rounds start. A paused round whose judge was never
  prompted restarts as a new round and waits like one. The dispatcher decides before the round starts, from
  what it already knows (`deltaCheck`, `sameHead`, `replies`, `requested`); a delta check or a same-head
  re-review that the checkout turns into a full round (`confirmDeltaCheck`, `confirmSameHead`) has started
  and runs as one. The board's cell reads `re-review · Codex soft cap` (`WaitBudget`'s short form, which said
  "codex budget"), the card's sentence says what still runs and that `magnum review` runs it, and the pace
  toast says full re-reviews will wait. An author who wants the re-review asks for it: a review request runs
  it. `codex_hard` is unchanged. Rejected: holding first reviews too (the soft cap would hold every new pull
  request for days); holding delta checks and reply rounds (the judge alone, a fraction of a full round).
- **Magnum auto-approves as the round that posted the review ends** (2026-10-07; completes "Magnum approves
  as the operator when its review found nothing to fix"). On talkable#12006 the round ended at 15:48:25 and
  the operator's approval came at 15:49:02, at the next tick, which runs auto-approval once. The round's
  goroutine now runs the same decision for that PR alone (`autoApproveRound`: the tick's `autoApprove`
  filtered to the PR, the same refusals, GitHub reads and gates) right after `finish` records the posted
  review, unless the round's context was cancelled (an abort or a shutdown: the tick decides). The tick's pass
  stays the safety net (a failed post, a PR that changed). `autoMu` serializes auto-approval between the tick,
  a round's goroutine and `magnum unapprove`, so one review is never approved twice and the remembered GitHub
  reads (`autoSeen`, `autoFollow`) have one writer at a time.
- **The judge's result file ends its turn; what the agent prints after it is the run's** (2026-10-07). On
  talkable#12006 the judge wrote `codex-judge.json` at 15:46:26 and ended its turn at 15:47:13, and magnum
  verified at 15:48:22: a turn ended only after two idle observations 30 seconds apart, and the result file
  counted only after 2 minutes (`ResultSettle`). The wait now ends as soon as the file parses with a final
  status (`posted`, `replied`, `dry_run`, `blocked`, `identity_error`, `closed`, `stopped`, `error`; the
  skill writes it last), and the round goes on to the GitHub check; a file that does not parse or holds
  another status keeps the old rule, and so do the reviewers. The agent may still print its last message, and
  the run is verified (not in flight) by then, which used to make that work look like a human typing
  (`human_active_at`, the `human_cooldown` on the whole PR). The mechanism needs no new state and no new herdr
  call per tick: a judge session whose stored status is `working` since at or before its last run's
  `ended_at` (`agent_status_at` moves only when the status changes, so the agent has not been seen anything
  but working since the run ended; `store.LastEndedRun`) is finishing that run's turn (`turnTail`): Observe
  never counts it as human activity, and the first observation of another status (idle) ends it, so a human
  typing after that is a human again: typing into an idle agent moves `agent_status_at` past the run's end.
  Only a message typed into the turn before it ends counts as the turn's, as it does while a run is in
  flight. Submit holds a prompt to such a session (`awaitTail`): it asks herdr (`pane.get`) every 5 seconds
  until the agent no longer works, and refuses with `ErrBusy` after 2 minutes (the run abandoned, nothing
  typed into the old turn). Rejected: a new run state that Observe ends on the first idle (the runs' state
  check is a schema change, and verified runs are what every reader takes as the round's); an in-memory
  mark (lost on a restart); a grace after the result time (a human typing within it would pass, and a long
  final message outlasts it).
- **The judge runs `db_lock` as given** (2026-10-07). On talkable#12006 the own pass added `--timeout 60s` to
  `db_lock`, gave up while claude-review held the lock, ran the spec later and still reported a machine
  failure. SKILL.md's Databases paragraph now says to run it as given (its own timeout is 20 minutes), and
  that a check that waited, then ran, is no `environment_failures` entry; only exit 75 is a check that did not
  run. SKILL.md grows by 98 bytes (`skillMaxBytes` 34,734 to 34,832).
- **The improvement loop lands DECISIONS-only conflicts by itself, drains before a restart, and has a shared
  analyst rule file** (2026-10-07). In one run every cherry-pick of a builder's branch conflicted in
  `docs/DECISIONS.md`, where both sides append entries, and the resolution was always to keep both; after a
  resolution by hand, `land.sh <branch>` picked the resolved commit again. `land.sh` now resolves a conflicted
  DECISIONS.md whose commit only appends to it (the parent's file is a prefix of the commit's) as master's file
  followed by the appended text, unless master already has that text, and stages it; a conflict in any other
  file stops, and `land.sh --continue <branch>` picks the rest of the range, skipping the commits whose subject
  master has. It builds unless the registry lacks a migration (one above its `user_version`), so a range landed in
  two steps, or after a migration the daemon has not restarted on, does not leave a binary the CLI cannot run.
  `restart.sh` waited more than 30 minutes with `daemon-restart --when-idle` while new rounds kept starting; it now
  restarts with `daemon-restart --drain` (no round starts while it waits), at most `--max-wait` (default 30m),
  printing the rounds every 5 minutes. Analysts get `analyst-rules.md` as builders get `common-rules.md`,
  `eval-at.sh` runs `magnum eval run` from a throwaway worktree at a given commit, and `selftest.sh` tests the
  scripts against throwaway repositories. Rejected: git's union merge (`merge-file --union`, `merge=union`): it
  matches lines across the two sides' entries, so when both end with the same line ("No new dependencies.") the
  line is kept once and the first entry loses it.
- **A on a row that needs the operator approves as the operator** (2026-10-07). Every posting identity is
  a GitHub App, whose approval GitHub never counts, and `A` posted as the PR's identity: on talkable#11980
  round 5 approved as the App at 16:06:56, auto-approval stopped at 16:07:00 ("you requested changes by
  hand"), `A` at 16:07:18 approved as `zhuravel[bot]` to no effect, and the operator approved by hand at
  16:08:00 (as for 8 other PRs, two of them 7 to 9 minutes after the same stop). On a `✔ needs you` or `✔ lift
  your ✗` row `A` now asks "Approve talkable#N at <sha7> as zhuravel? Your approval counts and lifts your ✗;
  magnum found …" and posts as the watch's `auto_approve_as` (`VerdictPayload.As`; `magnum approve <ref> --as
  <identity>`, which takes that identity or the PR's posting identity), through the gh client auto-approval
  already uses. It is refused, naming the precondition, where auto-approval would refuse
  (`engine.OperatorApprovalRefusal` calls `autoApproveRefusal`, which the board row carries as
  `PRBoardRow.ApproveAs.Refusal`), less the two checks that exist only because auto-approval acts without
  asking: the operator's stop (their own changes request is what they lift on purpose) and a manual verdict
  after magnum's round. The approval is recorded like an automatic one, with no migration: an
  `auto_approvals` row (the round's run and review, the identity and login), the body of a manual verdict plus
  the auto-approval marker (so it never counts as a review by hand, and a post whose answer was lost is found
  on GitHub), and a `review.manual_verdict` event with the identity; the PR's latest review stays magnum's
  round's. So a later round that finds P0-P2 withdraws it as it withdraws its own, `D` and `magnum unapprove`
  withdraw it, the row reads `✔ auto`, and the stop stays (magnum still does not approve that PR on its own).
  Without `auto_approve_as` on the watch, `A` there refuses with "GitHub counts only your approval: b opens
  the PR", and the card says it. `auto_approve_as` alone, without `auto_approve`, enables this and nothing
  automatic. Rejected: posting as the App there anyway (GitHub ignores it), refusing `A` after the operator's
  own changes request (lifting it is what the row asks of them), and recording it only as a manual verdict
  (no later round would withdraw it).
- **`reviewer_timeout` and `judge_timeout` stay fallbacks, and say when they change nothing** (2026-10-07).
  Every `[[role]]` block config.defaults.toml writes out sets its own `timeout` (90m for the judge, 40m for
  the others), so an operator who set `[daemon] reviewer_timeout` or `judge_timeout` changed no round, and
  the test of the keys passed only because it loaded no roles. The keys keep their meaning, the timeout of a
  role that sets none (one an operator adds), and the comments in config.defaults.toml, the README and
  prompts/README.md say so; `Config.Warnings` (`magnum config`, doctor) prints one line when a loaded file
  sets one of them while every role it would cover (the judges for `judge_timeout`, the others for
  `reviewer_timeout`) sets its own, naming those roles and their timeouts, read from the roles as merged
  before the fallbacks are filled in. Rejected: removing the keys (removing a setting is the operator's
  call); dropping `timeout` from the shipped roles (the built-in timeouts would then move with a key meant
  for added roles).
- **No duration key may be negative** (2026-10-07). `[[pool]] idle_remove_after = "-1h"` loaded and would
  have removed every surplus free slot at the next reconcile; a disabled `[triage]` with a negative timeout
  loaded too. Both are refused with the key named (`pool <repo>: idle_remove_after must not be negative`,
  `triage.timeout must not be negative`, the latter whether or not triage is enabled), and a test walks every
  duration key of every section and block and fails for one a negative value passes.
- **A watch can turn the related lookback off** (2026-10-07, amends "The judge knows the related PRs"). A
  zero `related_lookback` on a `[[watch]]` meant "keep the pipeline's", so a watch could not say what `"0s"`
  says in `[pipeline]`: open PRs only, no merged one. `Watch.RelatedLookback` is a pointer now: unset keeps
  the pipeline's, any value (`"0s"` included) is the watch's; the TOML is unchanged.
- **`include_own = false` skips the PRs of any of the operator's logins** (2026-10-07). It compared the
  author with the watch's poll login alone, while "own" everywhere else (the board's "mine", `★`,
  `own_min_rereview_interval`) means `config.SelfLogins`: every gh identity and every watch's posting
  identity. A PR opened by a second gh identity of the operator's was reviewed on a watch that skips his own
  PRs. `eligibility.Classify` reads `PRFacts.Own`, which the engine fills from `SelfLogins`, and
  `PRFacts.SelfLogin` is gone.
