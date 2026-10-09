# The self-improving review loop

Date: 2026-10-09. Status: approved in conversation, implementation planned.

## Goal

The review loop improves itself from two kinds of evidence: what other reviewers caught that magnum
missed (the retro's general misses) and what PR authors declined among magnum's own findings. A daemon
stage drafts a change to the judge skill and the prompts, a gate scores it with the eval, and a passing
change lands as a local commit in the checkout and reaches the next rounds after an idle restart. The
operator sees every held candidate in a review window and the effect of every landed version in numbers.

Decisions the operator made on 2026-10-09: the loop gates on the eval and lands alone when the gate
passes. it learns from general misses and from rejected findings. nothing is pushed by the daemon.

## Non-goals

No automatic push. No automatic revert (a revert is a held candidate). No edits to the retro and curator
prompts (`retro.md`, `notes-curate.md`), to Go code, to tests other than regenerated golden files, or to
the configuration. No herdr popup for the window in this stage. No Claude usage tracking (declined on
2026-10-07). No change to the notes curation beyond a new input (declines that need repository context).

## Today

The daily retro (`internal/learn`, `internal/engine/retro.go`) classifies other reviewers' comments on
closed PRs into the `misses` table. Repo-scoped misses feed the notes curation. General misses have no
consumer and stay `new`. The registry keeps no author reply to a magnum finding: the reply class is a
per-round file (`review-threads.json`, 600-character excerpts, written by re-review and recovery rounds
only). The eval (`internal/eval`, `magnum eval`) scores recall of seeded defects over full blind rounds;
a case cannot say "do not post this", and there is no judge-only replay. `SKILL.md` is at its size cap
(`skillMaxBytes`, 30,001 bytes) and 41 tests pin its sentences. Prompts and the skill are snapshotted at
daemon start, so an edit reaches rounds only after a restart.

## 1. Evidence: declines

The retro already fetches every review thread of a closed PR. It no longer discards the threads whose
root comment is magnum's. For each such thread with at least one reply by someone other than magnum, the
classifier gets the finding (the root's first line, its path and line, the posted priority), a copy of
its file at the reviewed commit and the replies, and answers with:

- `outcome`: `fixed`, `declined`, `deferred`, `argued`, `question` or `unrelated`;
- `verdict` for `declined` and `argued`: `wrong` (magnum's finding was wrong), `over_ranked` (right but
  the priority was too high), `missing_context` (right in general, wrong for this repository, which the
  notes must say) or `right`;
- `lesson` and `scope` (`general` or `repo`) for `wrong`, `over_ranked` and `missing_context`, worded
  like a miss lesson ("When X, check Y because Z") with no PR number, login, URL or repository name
  (`learn.ScrubLesson` applies);
- `match`: 1 to 3 case-insensitive regexps that the text of a finding reporting the same thing would
  contain.

Each answer is one row of a new `declines` table: `pr_id`, `source_url` (the root comment, unique),
`comment_id`, `review_id`, `run_id` (through `runs.review_id`), `finding_row_id` (the `findings` row on
the same review within 3 lines on the same path, when one exists), `path`, `line`, `title`, `priority`,
`reviewed_sha` (the review's commit), `skill_sha` (the 12-hex hash from `runs.prompt_text`), `outcome`,
`verdict`, `lesson`, `scope`, `match_json`, `state` (`new`, `used`, `dismissed`), `created_at`,
`updated_at`. Reply text is never stored. The upsert keeps `state` on a re-run, as `UpsertMiss` does.

Routing: a `missing_context` decline with scope `repo` marks the repository for a notes curation and is
upserted as a repo miss (class `miss`, scope `repo`, the same `source_url`, the posted priority as its
severity), which the curation takes unchanged, noted or skipped like any other repo miss. A later retro
that answers the thread otherwise withdraws that miss while it is still `new` (class `not_issue`), and one
that answers it `missing_context` again makes it a miss again. `wrong` and `over_ranked` declines with
scope `general` are the editor's input. `fixed`, `right`, `question` and `unrelated` are kept for the
measurements only.

`magnum misses` gains a `--declines` listing and `magnum stats` gains a per-source and per-priority table
of outcomes. The retro's `retro.ok` event data counts `declines` and `declined`.

## 2. The eval: absent defects, judge-only replay, candidates, the learned corpus

A defect gets `expect = "found"` (the default) or `expect = "absent"`. An absent defect is matched like a
found one (paths, lines, match, body). a finding that matches it counts as a false positive, listed per
case as `absent_hits`, and never as noise. `Totals` gains `AbsentHits`. The text and Markdown reports show
them.

A judge-only replay (`magnum eval run --judge-only`, `engine.RunEval` with `JudgeOnly`) runs the judge
alone. It takes the reviewer reports of the newest full replay of the same case with the same head
(`<run>/cases/<case>/reviews/...` of a run whose `outcome` was a scored dry run and whose reviewers read
what the replay's would: the same reviewer prompts and role config, `reviewer_inputs` in `run.json`, and
the same notes setting), copies them into the scratch report directory, seeds the reviewer runs in the
scratch registry as verified with those report paths, and runs the judge the way a same-head judge-only
round runs it (its own pass included). A case without a stored full replay is refused with a message
that names the run needed. `run.json` records `judge_only` and the run the reports came from.

A candidate is selected with `--candidate <dir>`: a checkout or worktree whose `prompts/` and
`skills/magnum-review/SKILL.md` are used in place of the binary's, through a config copy with
`prompts_dir` and each judge's `skill` pointed there. The inputs hash covers the candidate's texts, so
baselines match by content. The daemon runs the same code in-process (no build): a `RunEval` option that
takes the candidate directory.

The learned corpus is `<data dir>/learn/corpus.toml`, written by the daemon and read by `eval` after the
operator's default corpus (a case name in both is an error; `--corpus FILE` is read alone, and `eval
run` counts the learned cases it replays on its start line). It holds one case per P0 to P2 general miss
and per `wrong` or `over_ranked` general decline whose PR is in a watch and is not Codex-flagged:
`miss-<id>` with a found defect, `decline-<id>` with an absent defect (an over-ranked one a hit only at
the posted priority or stronger, and no case when that priority is unknown), head = the reviewed commit,
paths = the path, lines, match and severity from the row, `why` = the title. A case is written when the
retro that stored the row ends, and removed when the decision that dismissed the row rewrites the file
(or at the next retro). `eval.WriteCorpus` encodes a corpus. `LoadCorpora` merges the two files.

## 3. The editor

A pane agent (`paneSpec` like the curator: workspace `learn skill`, name `skill`, role from `[learn]
editor_kind`, `editor_model` (default claude, sonnet), `editor_effort`, `editor_args`, `editor_prompt`
(`skill-edit.md`), `editor_timeout` (45m)) runs in a git worktree the daemon creates under
`<state>/learn/wt/<run>` on branch `learn/<run>` from the checkout's `master` head. The run directory
`<data>/learn/skill/<run>/` holds:

- `inputs.json`: the general misses and the general `wrong`/`over_ranked` declines in state `new`
  (ids prefixed `m` and `d`, severity, title, lesson, match, and `history`: the earlier proposals that
  noted the input and failed at the gate, with the gate's rule), the newest 40 of them (misses and
  declines each newest first; `left_out` counts the older ones), the operator's rejection reasons from the
  last 5 rejected proposals and per input, the last 3 landed versions with their measurements, the size
  cap and the current size, and the list of files it may edit;
- `pins.json`: the sentences the skill must keep, produced by a test helper
  (`go test ./internal/agents -run TestSkillPinsDump -args -pins <path>`), so the editor knows them before
  it writes;
- the editor's outputs inside the worktree: the edited files, `changes.json` ({`files`: [{`name`,
  `action` edited|unchanged, `reason`}], `inputs`: [{`id`, `action` noted|skipped, `where`, `reason`}],
  `decision`: the DECISIONS entry text}), and the DECISIONS entry appended by magnum, not the agent, at
  the end of `docs/DECISIONS.md`'s last section, `## Learned rules`, which the file carries from the
  start (with an introduction that says whose it is) and which holds only the loop's entries (the
  editor's and the reverts'):
  people append theirs to the sections before it, so an operator's entry on master and a candidate's
  never touch the same lines and the landing's rebase does not conflict over them.

It may edit `skills/magnum-review/SKILL.md`, `prompts/judge-*.md` and `prompts/claude-*.md`. Nothing
else. Rules in the prompt: keep the skill at or under the cap by shortening. keep every pinned sentence;
general rules only, no PR number, login, URL, repository or person. one change per input or a skip with a
reason. do not weaken a rule a landed version added for a measured gain. keep section numbers.

Magnum checks the result (`learn.Validate`): every input accounted for once. only allowed files changed
(`git status` of the worktree). the skill size. no pinned sentence lost. `notes.Validate`'s leak rules
(PR references, branches, secrets, home paths) on the diff. `learn.ScrubLesson` rules on the DECISIONS
text. the prompts render (`engine.CheckPrompts` on a config pointed at the worktree). Then magnum appends
the DECISIONS entry to the worktree's `docs/DECISIONS.md` (put back to the base before each check, so a
nudge does not append twice, and part of the late-write fingerprint), so the format gate's
`make check-private` sees the entry, whose first sentence is also the commit subject. Then the format gate
runs in the worktree: `go test ./internal/agents ./internal/pipeline -update` is not used. instead
`go test ./internal/agents -run '^(TestRenderGolden|TestJudgePromptsNameTheFormerLogins|TestRereviewPromptNamesTheThreadsFile)$' -update`
regenerates goldens (every test that writes one, `TestTheFormatGateRegeneratesEveryGolden` checks; never
the whole package, whose `-update` rewrites the pins; the goldens go back to the base before each check,
so a golden edited by hand is not committed), then
`go test ./internal/agents ./internal/pipeline ./internal/config` must pass, and
`mise -C <checkout> exec -- make check-private` must pass. One nudge with the problems, then the candidate
is committed on its branch (message: the DECISIONS headline, the inputs it answers by id, the byte
accounting, "No new dependencies.") and stored as a `learn_proposals` row in state `gating`, or stored
`invalid` with the problems. A worktree whose editor stopped (pause, timeout twice, context) is removed and
the run is `stopped`.

## 4. The gate

For a proposal in `gating`, the daemon selects the cases: the learned cases of the inputs it noted
(targets: those no gate compared yet first, then the others, the misses first and the newest first) plus
every other case in both corpora whose head is in a watched, non-flagged PR, capped by
`[learn] gate_cases` (default 6, targets first, then the newest). A target whose baseline already covers
it (a `miss-` case it finds, a `decline-` case it no longer hits: `learn.AtBaseline`) is no target: it
is skipped as "found at the baseline" or "not reported at the baseline", gives its place to the next
target past the cap (`learn.PromoteTarget`), and its input is dismissed by the gate's decision
(`store.GateLearnProposal`, the link row's `gate` = `baseline` and `dismissed` saying why); its learned
case stays in the corpus as a regression check. A proposal whose every target is so fails. Mode: judge-only when only
`SKILL.md` and `prompts/judge-*.md` changed, else full. The baseline is the newest stored run with the
same inputs hash as the proposal's `base_sha` (through `eval.Baselines`), else a baseline replay first.
Both read a detached worktree at `base_sha` under the learn worktrees, never the checkout (its branch and
its uncommitted edits are the operator's), removed after the gate; each run of the gate records the
commit of the tree it read and that tree's clean state, so a later gate reuses it. In judge-only mode
both judges read the reviewer reports of the case's newest full replay whose reviewers read the
candidate's prompts (`eval.FullReplayFor`): the baseline is that replay itself when its judge read the
base's inputs, else the base's judge alone on its reports (stored, `eval.JudgeBaselines`, or replayed
then), and without such a replay the base's full replay is the baseline and gives the candidate's judge
its reports. The candidate
replay runs in-process with `--candidate` = the worktree, under the eval agent tag, one case at a time,
refused above `codex_soft` (then the proposal waits in `gating` and is retried at the next tick after the
window moves) and never on a flagged PR.

Verdict (`learn.Judge`):

- pass: at least one `miss-` target found more often. no case found at the baseline is lost (a lost case
  is replayed once more before it counts as lost). no absent case gets a hit that the baseline did not
  have;
- hold: the targets could not be replayed (no corpus case, a flagged PR, no stored full replay for
  judge-only) but nothing regressed; or the only targets that improved are `decline-` cases no longer
  hit ("only declined findings improved: review the rule it adds"), with nothing regressed: a rule that
  stops a kind of finding lands only on the operator's `y`;
- fail: anything regressed, or no target improved.

A replay without a score is no verdict: on a target or its baseline, or on the second replay of a lost
case, one that ended without a score (anything but a flagged PR) stops the gate like the soft cap does, and
the proposal stays `gating` for a try 30 minutes later; only the third gate of the proposal that stops so
holds it, with the replay named; a corpus or a list of flagged replays that cannot be read counts the
same, before any replay. A drain or a pause is checked before each replay and stops a running gate the
same way, and so does a verdict the registry did not take. A gate that stops short keeps its replays: the
next gate takes the candidate's stored replays of a case and a lost case's second replay (`eval.Baselines`
over clean runs with the candidate's inputs hash; judge-only, `eval.JudgeBaselines` on the same reports)
instead of replaying them. Every stop but a shutdown's counts in `learn.gate_stops` (`attempts`, with
the last stop's reason), and the sixth holds the proposal; one still `gating` 3 days after it was made is
held whether or not a gate could start. `magnum learn --review` shows the oldest `gating` proposal when
none is held and rejects it on `y`, stopping its running gate first.

The results are stored in `gate_json` (baseline run id, candidate run id, per-case before/after, the
rule that decided) and the state becomes `passed`, `held` or `failed`, with what the gate made of each
target on its input's link row (`improved`, `not_improved`, `baseline`). `failed` proposals keep their
inputs `new`, but an input whose case two failed gates compared without improving it
(`store.LearnDismissFailures`) is dismissed ("noted in 2 failed gates"). a `held` one waits for the
operator. a `passed` one lands. `magnum misses --all` (and `--declines --all`) shows why a skill proposal
dismissed a row and which one.

## 5. Landing and restart

Landing (`engine.landProposal`): the checkout must be on `master` with no rebase or merge in progress;
the branch must be at the proposal's `candidate_sha`, or at that commit as an earlier landing rebased it (a
single commit on a commit `master` holds, with the same `git patch-id`), else it is held: a commit made in
the worktree would land ungated. `master` must hold the proposal's `base_sha` (else held: it was reset or
rewritten since the edit); when it moved past it, the candidate's commit alone is rebased once in the
worktree (`git rebase --onto master <base_sha>`, so a commit the operator dropped from `master` does not
come back; a conflict holds the proposal with the reason). the tree that lands must render with the
daemon's build (`CheckPrompts` on the worktree, as the restart on it checks first; else held). the files
the commit touches must be clean in the checkout's working tree (else held, reason named). Then `git -C
<checkout> merge --ff-only --no-autostash learn/<run>` (the rebase and the merge run on a context a shutdown
does not cancel, with a timeout; `gitx.MergeFFOnly` checks that `HEAD` is `refs/heads/master` right before
the merge and that `master` is at the branch's commit right after it, and the landing holds the proposal
when either fails), the
proposal becomes `landed` with `landed_sha` and a `learn_versions` row records the new skill and prompts
hashes in one transaction (`store.LandLearnProposal`), then the branch is deleted and the worktree
removed. A branch `master` already holds (a landing a crash cut short after the merge) is recorded as
landed, at the next landing, at the operator's `n` (instead of the rejection) and at the expiry (instead
of it). A toast says what landed. At most one landing per local day (`[learn] land_per_day`, default 1): a
second passed proposal waits as `passed` until the next day. No proposal lands, at a tick or on the
operator's `y` (which is refused: "ask again after the restart"), before the daemon restarted on the
version landed last.

Restart: the daemon sets `KVLearnRestart` and exits for launchd at the first tick with no round, retro,
curation, editor or gate in flight (the `restartForBuild` path), unless `[learn] restart` is `manual`,
in which case `magnum status` and a toast ask for `daemon-restart --when-idle`. The restart is recorded on
the version (`restarted_at`), which starts its measurement window; a start without `KVLearnRestart`
records it on the newest version when that one was never restarted on (a crash between the landing's
transaction and the key).

Nothing is pushed. `magnum learn` says how many landed versions are ahead of `origin/master`.

## 6. The review window and the surfaces

`magnum learn` (no flags): the landed versions newest first (version, landed, restarted, rounds since,
findings posted, fixed, declined, misses found, from `learn_versions` joined with runs, findings, declines
and misses by time window), then the proposals that wait (`held`, `passed` waiting for the day, `gating`),
the last `failed` and `invalid` ones with their reason, and a line on the next scheduled run. `--json`
prints the same.

`magnum learn --review [--reason <text>]`: the oldest `held` proposal: its evidence (inputs with title and
lesson), the diff of the skill and prompts (`notes.Unified`, `notes.Printable`), the DECISIONS entry, the
gate table, and the question. `y` lands it through the landing path (the daemon does the landing. the CLI
sends `ReqLearnLand` and reports the answer), `n` rejects it with the reason (the branch and worktree are
removed, the inputs go back to `new`, or `dismissed` after a second rejection), anything else leaves it.
`--json` prints the data. A proposal not decided within 7 days of becoming `held` or `passed`
(`waiting_since`, so a gate that waited days for the budget or a pause takes none of the week) expires
like a notes proposal.

`magnum learn --propose`: `ReqLearnPropose`, an editor run now (refused while one runs or a proposal is
`gating`, `passed` or `held`. the refusal names it). `magnum learn --revert <version>`: a proposal that
restores the files of the version before it, through the gate, held for review; its `--reason` goes
into the design log's entry and the commit's subject, so the daemon refuses one that names a pull
request, a link, a login of the registry's pull requests (`store.PeopleLogins`) or a watched
repository's owner or name, and the format gate's `make check-private` checks the entry.

Counters and toasts: `tui.DaemonFacts.LearnProposals` counts `held` proposals ("N skill proposals to
review", short "skill ×N") on the board and dashboard titles. the `magnum status` learn line shows the
last run, held proposals, pending landing and whether a restart is needed. Toasts: candidate held
(`learn-proposal:<id>`), landed (`learn-landed:<id>`), restart pending, gate refused for budget (once per
window). Events on subject `learn`: `learn.edit_start`, `learn.edit_stopped`, `learn.edit_invalid`,
`learn.gate_start`, `learn.gate_done`, `learn.landed`, `learn.held`, `learn.rejected`, `learn.expired`,
`learn.restart`.

## 7. Measurement and revert candidates

A `learn_versions` row: `id`, `proposal_id`, `landed_sha`, `skill_sha`, `prompts_sha` (over the prompt
files), `landed_at`, `restarted_at`. A version's window is from its `restarted_at` to the next version's.
`store.VersionMeasure(id)` counts, in the window: judge runs posted, findings posted by priority, declines
by outcome (joined by `skill_sha`), misses by class and scope (by `reviewed_sha` of runs in the window).
Each count takes only the rounds that ran the version's skill (a run of the review whose `prompt_text`
names the copy `<state>/skill/<skill_sha>/`), so a skill changed outside the loop does not count toward
the version that runs before or after it, and a review counts once however many judge runs a nudge or
a model fallback stamped with its id; the skill before the first version counts every round.
The editor's `inputs.json` carries the last 3 versions' measures. When a version with at least 20 posted
findings has a `declined`+`argued` share higher by 10 points than the previous version's, the daemon
creates one held revert proposal (`trigger = regression`) and toasts it. never twice for the same version.
It creates none when a waiting or landed proposal reverts that version already (the operator's
`--revert`), and the version is remembered only once its revert started (or none is possible), so a
revert another learn job or a drain kept from starting is proposed at a later scan.
A revert's DECISIONS entry ("Reverts skill version N: <why>") goes to the `## Learned rules` section too.

## 8. Scheduling, cost and configuration

The editor runs when the daily retro finished and stored at least one new general miss of severity P0 to
P2 or one new general `wrong`/`over_ranked` decline, at most once per local day (`KVLearnEditDay`), and on
`--propose`. It does not run while a round is in its judge stage on any repository (the judge's skill
copy is about to be re-read), under a hold, a user pause, or a tool pause of its kind, or when the
checkout is missing or `go` is not on PATH (`[learn] editor = false` is implied and `magnum doctor` says
why). Learn-tagged jobs (retro, curation, editor, gate) are serialized by one mutex in the engine, which
also removes the current retro/curation overlap. The gate and the editor count as background work for
`daemon-restart` and `backgroundBusy`, their KV marks (`learn.editing`, `learn.gating`) are cleared at
startup, and shutdown cancels them.

Configuration under `[learn]`: `editor` (bool, default false), `editor_kind`, `editor_model`,
`editor_effort`, `editor_args`, `editor_prompt`, `editor_timeout`, `gate_cases` (6), `land_per_day` (1),
`restart` (`idle` or `manual`, default `idle`), `regression_points` (10), `regression_min_findings` (20).
Everything is validated whether or not enabled. `config.defaults.toml` documents each key.

## 9. Data model

Migration 0023: `declines` (section 1) with indexes on `(pr_id)`, `(state, scope, verdict)` and
`(skill_sha)`. Migration 0024: `learn_proposals` (`id`, `trigger` evidence|request|regression,
`base_sha`, `branch`, `candidate_sha`, `files_json`, `state` editing|invalid|stopped|gating|passed|held|
failed|landed|rejected|expired|superseded, `gate_json`, `reason`, `model`, `prompt_sha256`, `scratch`,
`run_dir`, `created_at`, `waiting_since`, `decided_at`, `landed_sha`, `landed_at`), `learn_proposal_inputs`
(`proposal_id`, `kind` miss|decline, `input_id`, `outcome` noted|skipped, `location`, `reason`, `gate`
improved|not_improved|baseline, `dismissed`, primary key on the three), `learn_versions` (section 7). The `misses` and `declines` state transitions move through
the proposal decision in one transaction, as `decideProposalMisses` does, and a `dismissed` input is
removed from the learned corpus.

KV: `learn.edit_day`, `learn.editing` (run id and start), `learn.gating` (proposal id and start),
`learn.restart` (version id), `learn.land_day`. Every new key is added to `kvkeys_test.go`.

Files: `<data>/learn/skill/<run>/` (inputs, pins, changes.json copy, log), pruned after 30 days with the
retro runs. `<state>/learn/wt/<run>` (the worktree, removed at landing, rejection, expiry or stop);
`<state>/learn/eval/wt/<case>` (the gate's case worktrees, never `magnum eval run`'s `<state>/eval/wt/<case>`;
`eval run` refuses to start while `learn.gating` is set); `<data>/learn/corpus.toml`.

## 10. Testing

Hermetic, as the retro and curation tests: a fake editor (`Deps.Editor`) that writes files into the
worktree. a fake git for the worktree operations (`gitx` fake with `worktree add`, `merge --ff-only`,
`rebase`, `status`). the gate's eval through a fake runner that returns stored scores. the store
transitions through `storetest`. The format gate's `go test` subprocess is behind a seam that the tests
replace. The retro's decline classification is tested with the fake classifier and seeded threads. Every
new behaviour has a named test. `docs/DECISIONS.md` gets one entry per decision above. the README gets a
"Self-improving loop" section after "Learning from other reviewers". `prompts/README.md` documents
`skill-edit.md`'s fields. `docs/API.md` is regenerated.

## 11. Build order

1. Declines: retro classification, table, routing to notes, `magnum misses --declines`, stats.
2. Eval: absent defects, learned corpus, `--candidate`, judge-only replay.
3. Editor: proposals table, worktree, pane agent, validation, format gate, `magnum learn` list and
   `--review`/`--propose`.
4. Gate and landing: case selection, in-process replays, the verdict, landing, restart, toasts and
   counters.
5. Measurement: versions, `VersionMeasure`, the editor's feedback input, revert candidates, scheduling
   and serialization, doctor.

Stages 1 and 2 are independent. Stage 3 needs 1 (inputs). Stage 4 needs 2 and 3. Stage 5 needs 1, 3
and 4. Each stage lands as one local commit with the gate green and its documentation.
