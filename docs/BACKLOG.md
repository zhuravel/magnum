# Backlog

Candidate work, ranked by value for effort. Every entry carries the evidence that justifies it; an
entry without evidence does not belong here. Numbers come from the registry, the daemon log, the
review artifacts and GitHub for the first 34 hours of operation (2026-10-03/04, 42 rounds, 18 posted
reviews across 7 talkable PRs, 75 inline comments). Effort: S under half a day, M one to two days,
L more. When an item ships, move it to [DECISIONS.md](DECISIONS.md) with the decision it settled.

## Shipped

Items struck through below shipped on 2026-10-04; their decisions are in DECISIONS.md. None is open.

## Tier 0: wrong or wasteful today

1. ~~**Classify infrastructure failures as global, pause once, probe until recovery.**~~ (S–M)
   Evidence: on 2026-10-04 every PR fetch failed with `Permission denied (publickey)` from 07:25Z
   while `gh` polling stayed healthy; five PRs went to needs_attention with a per-PR error that read
   like a problem with the PR, and no review posted for ten hours. Fix: SSH, network, toolchain and
   DNS errors pause dispatch with one alert, a probe retries, PRs are not charged attempts.
   Signal: zero needs_attention transitions whose cause is an identical error across PRs.
2. ~~**Validate config and render every prompt with the binary that will run them, before restart.**~~ (S–M)
   Evidence: 138 crash-loop restarts in 23 silent minutes on an unknown config key; 14 runs on 5 PRs
   failed on template fields the running daemon lacked, in two waves. Fix: `daemon-restart`,
   `install` and the daemon's startup run `config` plus a dry render of all prompt files with the
   new binary and refuse on error; the tab bar shows "daemon down" instead of the last line.
   Signal: no `render` errors in round.end events; no crash loops in launchd.log.
3. ~~**Codex budget gauge and soft cap.**~~ (M)
   Evidence: Codex's own session snapshots show the weekly limit going 0% → 99% in 26 hours
   (2026-10-02 23:29Z → 2026-10-04 01:33Z), next reset 2026-10-09 23:29Z; the daemon has no signal
   for it and every round needs Codex twice. Fix: read `rate_limits.primary.used_percent` from the
   newest Codex session files, show it in status and the tab bar, defer first reviews above a soft
   threshold (configurable, default 80%) and pause the kind above a hard one (95%) with a toast.
   Signal: status shows the gauge; no round starts past the hard cap.
4. ~~**Repository-notes write race.**~~ (S)
   Evidence: three judges rewrote the same notes file within four minutes; the final file references
   1 of 6 saved harness scripts, and one orphan was the Jest setup that made 109 tests run on one PR
   while another PR's Jest "failed in setup, zero tests ran". Fix: a per-repo lock, merge on
   conflict, the harness directory listed in the prompt. Signal: no orphan harness files.
5. ~~**Re-review the round's head only.**~~ (S)
   Evidence: at one head the judge "inspected newer fixes through" a later commit and reported four
   fixed while the reviewed diff touched none of the seven threads; a sentence in the skill
   ("re-read the live head") predates the stale-review decision. Fix: delete it; the restart logic
   owns head moves now. Signal: `previous_findings.fixed` only for threads the reviewed diff touches.
6. ~~**Codex review against the merge base, not the moving base branch.**~~ (S)
   Evidence: on talkable-esp#363 the shell review flagged files the PR does not touch because
   `master` was nine commits ahead and `--base origin/master` was used. Fix: pass the merge-base SHA.
   Signal: no findings on paths outside `git diff base...head`.
7. ~~**An App approval must follow the head.**~~ (S–M)
   Evidence: talkable/talkable needs one approval and keeps approvals across pushes; magnum dismisses
   only its own stale CHANGES_REQUESTED; 24 of 50 approved merges there in 30 days landed commits
   newer than the approval. Fix: on a new head, dismiss the bot's own APPROVE with a one-line reason
   until the re-review posts. Signal: no merge carries a bot approval older than its head.
8. ~~**Priority toasts survive herdr's rate limit; the rest are batched.**~~ (S)
   Evidence: herdr dropped nine toasts as rate_limited, including the identity-leak alert, and the
   notifier treats that as final; 14 "new repo" toasts fired in 34 seconds. Fix: retry priority keys
   with backoff, batch the informational ones per minute. Signal: zero dropped priority toasts.

## Tier 1: the review the author receives

9. ~~**Ship the reproduction inside the finding; never cite local paths.**~~ (S)
   Evidence: all seven talkable reviews mention reproductions the author cannot see; four cite `/tmp`
   or local paths; on one PR each of three rounds (10h40m) found a new P2 in the code written to fix
   the previous one while the pinning test stayed on the review machine. Fix: the skill requires the
   failing input or test body in the comment (fenced, short), forbids local paths, and attaches the
   harness as a `suggestion` when it is a test file. Signal: no `/tmp` or `/Users` in posted bodies.
10. ~~**Collapse Checks and keep the machine's own failures out.**~~ (S)
    Evidence: Checks are 50–80% of every body; three of seven show the review machine's failures
    ("19 failures from the missing campaign_snapshots table", "deadlocks") which read as the author's.
    Fix: Checks in a `<details>` block; environment failures go to the result file and the notes, not
    the review. Signal: median body length halves; no environment failure in a posted body.
11. ~~**A reply contract the re-review honours.**~~ (M)
    Evidence: all 23 author replies start with "(Claude)" and come from the team's resolve-review
    skill, which resolves every thread after replying; magnum never reads replies or resolve state;
    one "moot" answer stayed "still open" for two re-reviews with no reason; zero reactions on 75
    comments, so replies are the only feedback channel. Fix: the re-review reads replies on its own
    threads; `fixed`/`not a bug`/`won't fix` with a reason are honoured, and when the judge disagrees
    it says why in one line; standing decisions go to the repo notes. Signal: `answered` count rises,
    repeated "still open" without a rebuttal falls to zero.
12. ~~**Severity that means something.**~~ (S)
    Evidence: 59 of 68 posted findings are P2. Fix: define P1 as blocks merge (correctness, security,
    data loss), P2 as should fix before merge, P3 as optional, in the skill with examples, and make
    the blocking verdict follow from P1 presence. Signal: P-distribution spreads; verdicts track P1.
13. ~~**Never post a review twice.**~~ (S)
    Evidence: 1 of 19 reviews was posted twice with identical comments; magnum only warned. Fix:
    before posting, the judge checks for a review carrying this run's marker; the engine refuses to
    verify a second one. Signal: zero duplicate markers.
14. ~~**Verification readiness before the reviewers start.**~~ (M)
    Evidence: in 9 of 18 posted rounds the judge skipped or failed a check it wanted (no worktree test
    DB in 7 of 7 per-PR-worktree rounds, a missing table in a slot's test DB, DB contention); four
    Ruby-blocked rounds burned about 160 reviewer agent-minutes before the judge found the problem in
    three. Fix: per-repo `prepare` and `ready` probes (from `[[repo]]`/`[[pool]]` or the notes) run
    before the reviewers; the result is in the magnum block so the judge knows what it can run.
    Signal: judge checks that could not run drop toward zero.

## Tier 2: the reviewer's day

15. ~~**Board views: Magnum, Mine, Ready.**~~ (S)
    Evidence: 146 of 158 rows are baseline; the filter cannot match state, assignee or review request;
    6 of 11 manual forces were hunts for baseline rows. Fix: `v` cycles views (reviewed by magnum /
    assigned to me or requesting my review / ready to merge), filter grammar `state:`, `assignee:`.
16. ~~**Finding provenance and reason codes, then `magnum stats`.**~~ (M)
    Evidence: raw acceptance misleads: Claude scores 28% (70/246) yet was the only source of 9 of 24
    posted findings where both reviewers ran (Codex 4, judge 3); 13 of 14 "accepted" at one head were
    re-confirmations. Fix: the result file records per finding its sources and a reason code for
    rejections; `magnum stats` reports per day/repo/role: rounds, durations, outcomes, unique
    contributions, denies, restarts. Signal: a role is demoted only on unique-contribution data.
17. ~~**Evaluation harness.**~~ (M–L)
    Evidence: on talkable/talkable#11932 a human reviewer found two CRITICAL issues on the same head
    (OAuth HMAC skip, discount reuse) with zero overlap with magnum's ten findings. Fix: a corpus of
    PRs with known defects (the sandbox PR, #11932's head) replayed with `--no-post` after prompt or
    model changes; report recall of the seeded defects and the noise count. Signal: a number per
    prompt change instead of an opinion.
18. ~~**Park idle live agents of reviewed PRs.**~~ (S–M)
    Evidence: 21 agent processes, about 2.8 GB, idle 10.8 hours on average. Fix: park sessions idle
    longer than `min_warm` once the PR is reviewed; resume on the next push (sessions survive parking).
19. ~~**Log hygiene and timings on the card.**~~ (S)
    Evidence: 87% of log lines are INFO exec lines; provision logs total 113 MB. Fix: exec lines at
    debug, per-stage timings (fetch, checkout, each role, judge, verify) on the PR card and in events.
20. ~~**Restarts that drain.**~~ (M)
    Evidence: 12 of 42 rounds were killed by restarts (most of them mine during development). Fix:
    `daemon-restart --drain` stops dispatch, waits for rounds, then restarts; the dev loop uses it.

## Tier 3: before other people run it

21. ~~**Quick start that works on a second machine, plus a 12-line `config.local.toml.example`.**~~ (S)
    Evidence: for anyone but the author the documented steps fail at doctor (unconditional MySQL
    check), slot provisioning (needs a pool) and the first review ("not watched"); no step writes a
    config. Fix: `magnum init` writes `config.local.toml` from three questions (gh login, one
    repository, posting identity), the quick start reviews one PR without a pool or a database.
22. ~~**Agent autonomy explicit in config.**~~ (S)
    Evidence: the author's zsh wrappers add `--dangerously-bypass-approvals-and-sandbox` and
    `--dangerously-skip-permissions`; a new user's plain binaries start without them, magnum answers No
    to every prompt and a sandboxed Codex judge cannot reach the network to post. Fix: `[kinds.*] args`
    defaults carry those flags when `wrapper = false`, doctor fails (not warns) when neither a wrapper
    nor the flags provide autonomy.
23. ~~**Doctor checks only what the config uses.**~~ (S)
    Evidence: the MySQL check always runs and fails without DBngin; the login-shell check fails for
    anyone without mise; nothing checks that mise exists although launchd needs it.
24. ~~**HTTPS clones through gh's credential helper, SSH only when the existing clone uses it.**~~ (S)
    Evidence: new clones are hard-coded to `git@github.com:`; see item 1 for what an agent hiccup does.
25. ~~**A test that `.mise.toml`'s placeholder is never read as a key path.**~~ (S)
    Evidence: the App identity failed to load its key five times in two days, and the placeholder
    value in the tracked `.mise.toml` is treated as a file path, so the error names the wrong cause.

26. ~~**A newer CLI must not migrate the registry under an older running daemon.**~~ (S)
    Evidence: on 2026-10-04 a `magnum status` from a freshly built binary migrated the store to schema 5
    while the old daemon was draining; the daemon exited on the schema check and launchd restarted it
    on the new build, cutting a judge turn mid-way and preempting the drain. Fix: commands other than
    the daemon refuse to migrate when the daemon lock is held by another process, and say "run
    `magnum daemon-restart --drain` first"; read-only commands work against the older schema or
    explain why they cannot.

## Not worth it (considered, dropped)

- One-key merge or approve from the board: the reviewer merged 1 of 39 merged PRs; authors merge
  their own, and 59 of the reviewer's 66 PRs are in the repository where the App already approves.
- Deeper gh-dash integration: the reviewer's gh-dash config has no magnum keys and has not changed
  since August 2025.
- Dismissing stale CHANGES_REQUESTED: already done.
- Pool-exhaustion alerts: no round has ever waited for a slot.
- Homebrew tap or `go install`: the binary is not self-contained (prompts, skill, config live in the
  checkout) and the checkout-is-the-install decision stands while it is a one-person tool.
- Reactions as a feedback channel: zero reactions on 75 comments; the authors are agents.
