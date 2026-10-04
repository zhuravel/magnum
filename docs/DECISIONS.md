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
