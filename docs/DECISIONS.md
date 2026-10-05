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
