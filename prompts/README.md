# Prompts and the review pipeline

magnum's review pipeline is configuration, a "Procfile of agents". Three parts of `config.toml` define it:

- `[kinds.<name>]` says how to drive one agent CLI: codex, claude, droid, omp, or any other herdr agent kind.
- `[[role]]` blocks list the agents (or shell commands) of a review round, with their prompts.
- `roles` in a `[[watch]]` picks the roles that watch runs. It defaults to every role of a round, with the first judge only.

This directory holds the prompt templates the roles name. With nothing configured, magnum has the seven
built-in roles below. The first four make a round, exactly as before roles became configurable.

## How a prompt name resolves

A role names prompts by file name, such as `prompt = "claude-review.md"`. magnum reads
`<prompts_dir>/<name>` when that file exists, else the copy of the same name built into the binary.
`prompts_dir` is set under `[pipeline]` and defaults to this directory (`{{repo}}/prompts`). A file
you delete falls back to the built-in copy. `magnum config` fails when a role names a prompt that exists
in neither place.

The daemon loads every prompt its roles name, `model-fallback.md`, the triage, retro and notes curation prompts and a copy
of each judge's and fix role's skill (`~/.local/state/magnum/skill/<hash>/SKILL.md`, under the state directory) once, when it starts, right after checking that its build
renders them. An edit here therefore takes effect at the next `magnum daemon-restart`, which checks it again,
without a rebuild; `magnum status` shows when the prompts were loaded and how many files changed on
disk since. The CLI (`magnum config`, `magnum roles`, `magnum doctor`) reads the files as they are now.

`magnum roles` shows which file each role resolves to: its PROMPT column names the initial prompt file
and whether it comes from `prompts_dir` or from the copy built into the binary. `magnum doctor` checks
that every role's prompt resolves and that `prompts_dir` exists when it is set.

```toml
[pipeline]
prompts_dir = "{{repo}}/prompts"   # or "~/magnum-prompts" to keep your edits outside the repository
```

| File | Used by | Data |
|---|---|---|
| `judge-own-pass.md` | judge, its own pass while the reviewers work (`[pipeline] judge_own_pass = "parallel"`) | judge |
| `judge-initial.md` | judge, first review | judge |
| `judge-rereview.md` | judge, new head after its last review | judge |
| `judge-continue.md` | judge, after a pause (usage limit) ended mid-turn | judge |
| `judge-recovery.md` | judge, fresh session after the old one was lost, went cold (`[pipeline] judge_fresh_after`) or served the role's `max_session_rounds` | judge |
| `judge-nudge.md` | judge, stopped without a result | judge |
| `model-fallback.md` | any session role, the judge included, after magnum switched its model because the model hit its own limit | fallback |
| `claude-review.md` | claude-review, first review | role |
| `claude-rereview.md` | claude-review, new head | role |
| `claude-restart.md` | claude-review, a push cut its review short and the round restarted on the new head | role |
| `claude-simplify.md` | claude-simplify | role |
| `fix.md` | the fix role (claude-fix), which fixes the findings of magnum's review of your own PR (the fix job) | role |
| `whiteboard.md` | the whiteboard role (claude-whiteboard), which writes the dev.fast Whiteboard of the head magnum reviewed once the review posts, with the review as its last section (the whiteboard job) | role |
| `codex-review.sh` | the full codex-review command line, for reference (see Shell roles) | shell |
| `triage.md` | the cheap model that decides which reviewers a small diff needs (see Triage) | triage |
| `retro.md` | the retro's agent, which classifies what other reviewers said about a closed pull request ([learn]) | retro |
| `notes-curate.md` | the notes curator, which proposes curated repository notes and harness ([notes]) | curation |
| `skill-edit.md` | the skill editor, which drafts a change to the judge skill and these prompts from what reviews got wrong ([learn] `editor_*`) | skill edit |

## Template syntax

Prompts are Go [text/template](https://pkg.go.dev/text/template) files. A variable the data lacks is an
error, not an empty string; a field the data has but the run did not fill renders empty. Trailing
newlines are trimmed before the text is sent. PR titles, bodies and comments are never template
variables: agents read them from GitHub and must treat them as data.

```text
Review {{.URL}} at `{{.HeadSHA}}`.
{{- if .ForcePushed}} History was rewritten.{{end}}
{{range .Reports}}- {{.Label}}: {{if .Missing}}missing ({{.Detail}}){{else}}{{.Path}}{{end}}
{{end}}
```

The data comes in three shapes, all defined in `internal/agents/templates.go`: the judge's prompts get
the judge data, every other session role gets the role data, and a shell role's `command` or full-line
`.sh` template gets the shell data. `model-fallback.md` gets its own small fallback data
(`internal/agents/models.go`, see below), `triage.md` the triage data (`internal/engine/triage.go`),
`retro.md` the retro data (`internal/engine/retro.go`), `notes-curate.md` the curation data
(`internal/engine/notes_curate.go`) and `skill-edit.md` the skill edit data
(`internal/engine/learn_edit.go`).

## Variables

### Judge prompts (`judge-*.md`)

| Variable | Meaning |
|---|---|
| `.RunID` | the round's run id; reviews carry the marker `magnum:run=<RunID>` |
| `.Owner`, `.Repo`, `.Number`, `.URL` | the pull request |
| `.HeadSHA` | the commit under review (checked out, detached) |
| `.BaseRef`, `.BaseSHA` | the base ref the diff is taken against and the merge base. The prompts put `.BaseRef` unquoted into git commands, so a base ref with anything but letters, digits, `.`, `_`, `/` and `-` (or with `..`, or a leading `-`) is refused at render (`agents.ErrUnsafeBaseRef`; the poll makes such a PR ineligible first) |
| `.Checkout` | absolute path of the checkout the judge works in |
| `.IdentityKind` | `gh` or `app` |
| `.ReviewerLogin` | the login the review is posted as (REST form, e.g. `talkable[bot]`) |
| `.GhConfigDir` | `GH_CONFIG_DIR` of the identity; empty for the `gh` identity |
| `.NoFindingsEvent`, `.BlockingEvent` | review events from the `[[repo]]` or the `[[identity]]`; `.NoFindingsEvent` is `COMMENT` whenever a reviewer of the round left no usable report (anything in `.Reports` that is missing): a review that did not hear every reviewer never approves; and in a round that holds its approval for a full pass (`.HoldApproval`) |
| `.SelfAuthored` | the PR's author is the reviewing identity (its login or the human behind it); `.NoFindingsEvent` and `.BlockingEvent` are then both `COMMENT` |
| `.Reports` | one entry per other role of the round, in pipeline order (see below). With a usable `claude-simplify` report, the initial, rereview and recovery prompts carry the rules for its proposals (which to keep, the equivalence probe, at most three, the `**Simplification**` title, `candidates` in the result), which the skill leaves to them |
| `.ResultFile` | where the judge writes its JSON result (the role's `output`) |
| `.Magnum`, `.ReviewFile`, `.PostReviewCommand` | the magnum binary the daemon runs (absolute; empty: `magnum` on `PATH`), the file the judge writes its review to (`review.json` next to `.ResultFile`, derived when empty) and the shell line that posts it, `magnum post-review` with the run's facts as flags (repository, PR, head, run id, `.ReviewerLogin`, each of `.FormerLogins`, `.GhConfigDir` when set, `--dry-run` under `.DryRun`, `--local-base .BaseSHA` in a blind replay, `--review .ReviewFile`; every value shell-quoted; always derived). The judge prompts render it as the `<magnum>` field `post_review`; what the command does is in the README (The judge skill) |
| `.Role`, `.DBLockCommand` | the judge role's name (filled from the role the prompt is rendered for), and the line its commands that touch the checkout's databases run through: `magnum db-lock` (`.Magnum`) with `--checkout .Checkout --role .Role --`, every value shell-quoted (always derived; the role appends its command). The initial, rereview, recovery, continue and own-pass prompts render it as the `<magnum>` field `db_lock`; the roles of a round share the slot's databases and take turns through it (see README, The judge skill) |
| `.DryRun` | plan the review without posting it |
| `.PostMerge` | GitHub merged the PR before magnum reviewed `.HeadSHA` (`magnum review` of a merged PR): post a COMMENT asking for follow-ups; `.NoFindingsEvent` and `.BlockingEvent` are both `COMMENT`. The judge prompts render `post_merge: true` only then, and the initial, rereview, recovery and own-pass prompts the post-merge rules (`merged == true` for `state == open`, the `**Post-merge review**` body start, follow-ups, `in a follow-up`) |
| `.Blind` | a `magnum eval` replay (with `.DryRun`): the initial and own-pass prompts render `blind: true` and the blind rules (the local diff of `.BaseSHA..HeadSHA` as the boundary, nothing written about the PR afterwards, no commit newer than `.HeadSHA`) only then |
| `.SkillPath` | the role's `skill`, absolute; in the daemon, the copy it took at startup. The initial, rereview, recovery, continue and own-pass prompts start with its link and "Read <path> first, unless this session already read that file, and follow it; load no other skill." (with Codex's skills instructions off, nothing else tells the judge to open it) |
| `.Model`, `.Effort` | the judge role's `model` (else its kind's `default_model`), and its effort for this round: `rereview_effort` in a re-review, a delta check or a same-head recovery (when set), else `effort` |
| `.EffortInPrompt` | the kind sets the effort only at launch (its `effort` args, as codex) and `.Effort` is not the role's `effort`: a running session cannot switch, so the prompt asks for `.Effort` in words |
| `.PreviousReviewID`, `.PreviousEvent`, `.PreviousHeadSHA` | the last review (rereview, recovery) |
| `.PreviousHeadShort` | `.PreviousHeadSHA` cut to 7 characters (always derived) |
| `.Since` | RFC 3339 time; read every comment since then (rereview, recovery) |
| `.ForcePushed` | `.PreviousHeadSHA` is no longer in the branch (rereview, recovery): the rereview, recovery and own-pass prompts render `force_pushed` and ask for the full PR diff again |
| `.ColdJudge` | the judge started fresh on purpose, because its conversation's prompt cache had gone cold or its session served the role's `max_session_rounds`, not because its session was lost (recovery): the earlier reviews cover the PR up to `.PreviousHeadSHA`, so the own pass reads the commits since (unless `.ForcePushed` or `.BaseMerged`) |
| `.BaseMerged` | the commits since `.PreviousHeadSHA` have a merge commit (the base branch merged in), so `.PreviousHeadSHA..HEAD` carries the base branch's commits: the prompt compares the PR's own diff before and after (`git diff <base>...<previous>` with `git diff <base>...<head>`). `.ForcePushed` wins over it; `judge-rereview.md` renders `base_merged: true` only when set (rereview), `judge-recovery.md` too when no force push came with it (a recovery, whose fresh judge rebuilds the re-review; for a delta check the delta is the change between the two own diffs) |
| `.FullPass` | the round is a scheduled full pass (`[pipeline] full_pass_every`, `full_pass_lines`, `full_pass_files` or `full_pass_after`, or `magnum review --full`): every session started fresh and the reviewers review the whole PR with their first-review prompts. `judge-recovery.md` renders `full_pass: true` and one instruction in place of the new-commits one (review the full PR diff again as a first review does, then decide every earlier thread under the reply contract, earlier findings still counted, a finding the earlier reviews missed titled `(missed earlier)`, the body headed `Re-review <a> → <b> (full pass):`); `judge-own-pass.md` renders the field and, in place of the earlier reviews, their threads, the former logins and the reply-contract sentence, a blind first-review pass of the full diff that leaves the earlier reviews to the candidates phase (recovery; its `<magnum>` block drops `former_logins`, `previous_review_id`, `previous_head_sha`, `since` and `threads_file`), whose `judge-recovery.md` says the own pass was written before the earlier reviews were read |
| `.AuditFile`, `.AuditAreas` | the round is an audit pass (`[pipeline] audit_every`): every session started fresh, the reviewers re-review the delta as with `.ColdJudge`, and `.AuditFile` is `audit.json` in the report directory, which lists `.AuditAreas` areas of the PR's evidence map (`.EvidenceMap`): at most three `unknown` or `stale` entries, the riskiest first (each `why`: `irreversible write`, `authorization`, `money`, `optional branch` or `pre-pass lead`), and the oldest probe on a file the pushes touched since the last audit or full pass, marked `rotation`. The areas come from the PR, so the prompt names only the file. `judge-recovery.md` renders one instruction after the new-commits one, only then: probe each area through its real entry point with a control, one Checks line per probe, each probe that passes a `probed` entry of `evidence_file`; an area it could not probe is a lead, never a finding; then review the commits and decide the earlier threads under the reply contract. No `<magnum>` field. Empty in every other round |
| `.HoldApproval` | the round cannot approve (`[pipeline] approve_after_full_pass`): the PR has had that many re-reviews since its last full pass (else its first review), so `.NoFindingsEvent` is `COMMENT`. `judge-rereview.md` and `judge-recovery.md` render one instruction beside the events, only then: a clean verdict posts as a comment and a full pass of the whole PR follows before the approval, and a body with the verdict `clean` adds the line `A full pass of the whole PR follows before approval.` right after the verdict line. No `<magnum>` field. False in a full pass, a post-merge review, a reply round, a CI check and a dry run |
| `.DeltaCheck`, `.DeltaLines`, `.DeltaFile` | the round is a delta check: the judge alone, in its own session, on a small delta since its last review (`[daemon] delta_check`): `.DeltaLines` changed code lines in the files `.DeltaFile` lists (`delta-check.json` in the report directory, each file's path, status and whether it is a modified binary file; the file names are PR content, so the prompt names the file and never prints them; empty when it could not be written). `judge-rereview.md` renders `delta_check: true` and one instruction (review just those commits against the PR's purpose and the earlier findings, post one short review) only when set (rereview); `judge-recovery.md` does the same for a judge whose session is gone (a lost session, an identity migration): it names the last review by `.PreviousHeadShort` and has the fresh session read that review and its threads first, at the judge's `rereview_effort` (recovery) |
| `.SameHead` | the round re-reviews the head the judge's last review covered (no new commits), the judge alone at its `rereview_effort`: `magnum review` or a review request on a reviewed head, unless it names roles or asks for fresh sessions. `judge-rereview.md` says the head is unchanged instead of naming new commits and reports, with one instruction (re-read the replies and comments since the last review, re-decide each earlier finding under the reply contract, run no check that review already ran, post one short review); `judge-recovery.md` gives the same instruction to a judge whose session is gone (rereview, recovery) |
| `.CIRed` | with `.SameHead`: the round is a judge check of the head's red CI, which the engine starts when a check of the reviewed head failed after the judge of the review standing on it was prompted (so its `failing_checks` did not list it). `judge-rereview.md` and `judge-recovery.md` add, only then, one instruction after the same-head one: CI finished red after the review; read each failed check's log and decide whether the PR causes it (a P1 broken build anchored on the responsible line, or in the body, with the failing example or step quoted), else give it an unrelated Checks line; post exactly one review with the skill's section 7 verdict and the Checks block. No `<magnum>` field: the checks are `failing_checks`, written for every judge prompt (rereview, recovery) |
| `.MovedFrom` | the previous checkout when the PR changed slots (rereview) |
| `.PreviousReviews` | earlier reviews by the login: `.ID`, `.Event`, `.SHA`, `.SubmittedAt` (recovery) |
| `.Replies`, `.RepliesFile`, `.PostRepliesCommand` | a reply round (with `.SameHead`): `.Replies` replies came on the judge's review since it last read the threads, by the PR author (not a review of theirs whose inline comments start threads of their own) or anyone in one of its threads, with no new commits; 0 otherwise. When its verdict and event stay those of its last review, the judge posts no review: it writes `{"replies":[{"comment_id","kind","body"}]}` (`kind`: `ack`, `rebuttal` or `answer`) to `.RepliesFile` (`replies.json` next to `.ResultFile`, derived) and runs `.PostRepliesCommand`, the `post_review` line with `--replies .RepliesFile` in place of `--review`, then writes `"status":"replied"` and the printed `replies` to `result_file`; a changed verdict posts one short review. The instruction also says, in the sentence SKILL.md section 8 carries, that a thread another reviewer or the author started is theirs, never answered inline (`post-review` skips a reply there), and that a reply round with nothing of the judge's own to answer ends `replied` with no replies, never `blocked`. `judge-rereview.md` and `judge-recovery.md` render the instruction only then. `.RepliesFile` and `.PostRepliesCommand` are derived in every round after an earlier review (`.Replies`, `.PreviousReviewID` or `.PreviousHeadSHA` set) and empty in a first review: the judge posts every thread reply (a rebuttal the reply contract decided, an answer, an ack) through them, after its review in an ordinary round (SKILL.md section 8), so each carries post-review's reply marker, is posted once and stops after two rebuttals in a thread. `judge-rereview.md`, `judge-recovery.md` and `judge-continue.md` render the `<magnum>` field `post_replies` whenever it is set |
| `.StopThreads` | how many threads of `.ThreadsFile` are marked `stop`: magnum rebutted twice there and someone answered again. The re-review and recovery prompts then say to reply there no more (magnum flags the PR for the operator, and `post-review --replies` refuses); nothing is rendered when 0 |
| `.ThreadsFile`, `.ThreadSummary` | the JSON file of the inline threads the login (or a former login) started, every reply classified (`fixed`, `not a bug`, `won't fix`, `other`; the login's own replies carry the `kind` of their reply marker, and a thread its `rebuttals` and `stop`, and its first comment's `review_id`, `created_at` and `commit`: when and where the finding was posted), and their counts, e.g. `3 threads (1 resolved); replies: 1 fixed, 1 not a bug; 1 thread without a reply` (rereview, and recovery of a reviewed PR; empty when magnum could not read them). `.Threads` holds the same data, but replies are PR content: name the file, never print them |
| `.FormerLogins` | the logins (REST form) the PR's earlier reviews were posted as before its watch moved to another identity: their reviews and threads are the judge's own history; usually empty |
| `.Mode` | the round's kind, `initial`, `rereview` or `recovery`: `judge-own-pass.md` serves all three and renders it as `mode` (the other judge prompts serve one kind each) |
| `.Phase` | the judge's phase in a round that prompts it twice (`[pipeline] judge_own_pass = "parallel"` and at least one reviewer runs): `own_pass` for `judge-own-pass.md`, prompted with the reviewers, `candidates` for the usual prompt once every reviewer and the own pass ended; empty in a round with one judge prompt (`after`, a delta check, a continued turn, a judge alone). The prompts render `phase` only when set |
| `.OwnFindings`, `.OwnFindingsMissing` | the file the own pass writes, `judge-own.md` in the report directory, rendered as `own_findings`; in the candidates phase `.OwnFindingsMissing` says the own pass left no file (or an empty one), so the prompt asks for the pass then. Empty outside a two-phase round |
| `.RestartedFrom` | `judge-own-pass.md` after a push cut the own pass short and the round restarted: the head it was on (`.HeadSHA` is the new one), so it reuses what still applies |
| `.RelatedPRs` | `related.json` in the report directory, rendered as the `<magnum>` field `related_prs` by every judge prompt with a block (the own pass, initial, rereview, recovery, continue) only when set: the repository's other open PRs (drafts included; none without activity for 30 days) and those merged within `[pipeline] related_lookback` whose changed paths (the poll's lists, at most 100 per PR) overlap this PR's at `.HeadSHA`, paths matching `related_ignore` aside; at most 10, most shared paths first, each with its number, URL, state, merge time, last activity (`activity_at`), head, up to 20 shared paths, the truncated flags, whether magnum reviewed it (its last review's URL and verdict) and magnum's posted findings on those paths. No titles, bodies or comments. Written for each judge prompt that names it; in any round but an initial one it holds only the related PRs that the set recorded (`related-all.json`) for the head the previous review covered lacked or named another way (state, shared paths). Empty when nothing overlaps or nothing is new, when the PR has no file list for `.HeadSHA`, and always under `.Blind`. What to do with it is the skill's |
| `.HistoryFile` | `history.json` in the report directory, rendered as the `<magnum>` field `history` by the own pass, initial, rereview and recovery prompts only when set (`judge-continue.md` does not: the paused turn had it): each file the PR changes that exists on its base (at most 40, in git's order, paths matching `related_ignore` aside; the rest counted as `more`) with its last 8 commits on `origin/<base>` (under `.Blind` on `.BaseSHA`, never anything newer), each with its short SHA, committer date, subject and the PR number a subject ending in `(#123)` names, and `after_merge_base: true` on a commit the base got after the PR's merge base `.BaseSHA` (a second `git log` at the merge base, for a file with commits, lacks it): code the PR was never tested with (never without `.BaseSHA`, nor under `.Blind`). Written before the reviewers of each head (a restart writes the new head's); empty in a continued turn, for a PR that changes no file its base has, under `.Blind` without `.BaseSHA`, and when `git log` failed or took more than 2 minutes in all (a `round.history` warning). What to do with it is the skill's |
| `.DocsFile` | `docs.json` in the report directory, rendered as the `<magnum>` field `docs` by the own pass, initial, rereview and recovery prompts only when set (`judge-continue.md` does not: the paused turn had it): for each path the PR changes (added files included, paths matching `related_ignore` aside, at most 400), the `.md` files on `origin/<base>` (under `.Blind` on `.BaseSHA`) whose text names it, those naming it most often first (at most 10, the rest counted as `more`); for the paths no page names, the pages that name their directory (`<dir>/`, two levels deep at least), one entry per directory with its paths; at most 40 entries, the paths first, the rest counted as `more`. Page paths only, never their text; `base` is the revision to read them at. One `git grep` on the base's tree finds them, never in the checkout, so a PR that edits a page gets the base's version listed. Written beside `history.json` before the reviewers of each head; empty when no page names a changed path, in a continued turn, under `.Blind` without `.BaseSHA`, and when the grep failed or took more than 30 seconds (a `round.docs` warning); each head's `round.docs` event counts the pages listed. What to do with it is the skill's |
| `.PrepassFile` | `prepass.json` in the report directory, rendered as the `<magnum>` field `prepass` by the own pass, initial, rereview and recovery prompts only when set (`judge-continue.md` does not: the paused turn had it), with one sentence before the block: read `prepass` first, a changed method's body before its hunk, each item a lead to check, never a finding by itself. magnum's reading of the PR's diff (`.BaseSHA...HeadSHA`) and the head's tree with git alone, paths matching `related_ignore` aside: `removed_symbols`, each definition a `-` line holds (Ruby `def`, `class`, `module`; Go `func`, `type`; JavaScript and TypeScript `function`, `class`, `export const`, `const x = (`; Python `def`, `class`) that no `+` line defines again, with the files of the head outside the diff that name it as a whole word (`git grep -w`, each file's first line as `path:line`, at most 20, the rest counted as `more`; names under 4 characters and `new`, `call`, `run`, `get`, `set`, `init`, `main`, `test`, `initialize`, `constructor`, `__init__` never searched, at most 200 names, the rest counted as `unsearched_symbols`), only those still named; `test_gaps`, each added or modified source file whose test by its language's convention the diff leaves alone (`app/x/y.rb` and `lib/x/y.rb`: `spec/x/y_spec.rb`, `test/x/y_test.rb`, lib's also under `spec/lib/`; `x.go`: `x_test.go`; `x.ts`: `x.test.*`, `x.spec.*`, `__tests__/x.*`; `x.py`: `test_x.py`, `x_test.py`), tests, fixtures, migrations, generated files, views, styles, documents and configuration files not counting as source; `siblings`, each added file's closest existing file (the same suffix, the longest common prefix) in its directory, else its parent; `methods`, for each hunk of a modified source file, the innermost method enclosing its lines at the head (`git cat-file blob <head>:<path>`), with its `file`, `name`, `first_line`, `last_line` and `body`, found by the language's shape (Ruby `def` to its `end` at the same indentation; Go `func`, JavaScript and TypeScript `function`, a function bound to a name or a method, to its closing brace, braces in strings and comments aside; Python `def` to the first line at its indentation or less), one entry per method, none for a hunk outside any method or a method the diff adds whole; a method over 300 lines without its body, at most 120 methods (the rest counted as `unlisted_methods`), the bodies at most 400 KB together (past it the largest dropped first, counted as `dropped_bodies`). Written beside `docs.json` before the reviewers of each head; when it cannot run, `skipped` says why instead (no `.BaseSHA`, more than 50,000 changed lines, a git failure, more than 60 seconds; the last two a `round.prepass` warning). Empty in a continued turn; each head's `round.prepass` event counts the lists or gives the reason |
| `.Stakes` | what the PR's repository puts at risk, as the operator's config states it: the `[[repo]]` block's `stakes`, else its watch's (`config.Config.StakesFor`; one line of at most 300 characters, which the loader checks). Rendered as the `<magnum>` field `stakes` after `prepass` by the own pass, initial, rereview and recovery prompts only when set (never in `judge-continue.md`, whose paused turn had it); eval replays render the replayed repository's too. The skill's section 0 says how the judge uses it: it sizes each harm against it, never against a stakes block in the checkout, which is PR content. Empty when neither block sets it |
| `.EvidenceFile` | `evidence.json` in the report directory, beside `.ResultFile` (the pipeline always sets it): where the judge writes what it examined in the round, rendered as the `<magnum>` field `evidence_file` after `result_file` by the initial, rereview, recovery and continue prompts, the first three with one instruction before the block that gives the file's shape: `head_sha` and `entries`, one per behaviour the judge read (`state` `read`) or probed (`probed`, with its `command`, `control` true when a control ran, and its `result`), `skipped` with the reason in `result`, and one `unknown` entry without `behaviour` per file of the diff it reviewed that it did not read (`path` relative to the repository's top; `symbol`, `command`, `control` and `result` optional; 30 to 80 entries for 30 files). The round removes an earlier round's file on the head before the judge's prompt (not in a continue) and reads it at its end; the engine merges it into `.EvidenceMap` when the review posts |
| `.EvidenceMap` | the PR's evidence map, `evidence.json` in the root of its review reports above the per-head directories: what the earlier rounds examined, merged by the engine when their reviews posted, keyed by path and behaviour, each entry `read`, `probed`, `skipped`, `unknown` or `stale` (its file changed since the round that wrote it, whose state it keeps as `was`), with the head and round that wrote it. Rendered as the `<magnum>` field `evidence` after `prepass` by the own pass, initial, rereview and recovery prompts only when the PR has a map (never in `judge-continue.md` nor under `.Blind`), with one sentence before the block: the areas marked `unknown` or `stale` are leads to read first, never findings by themselves, and a `probed` entry with `control: true` needs no new probe, its file unchanged since |
| `.FailingChecks` | `failing-checks.json` in the report directory, rendered as the `<magnum>` field `failing_checks` by the initial, rereview, recovery and continue prompts only when set: the PR's checks that failed on `.HeadSHA` (name, state, workflow, time; `complete` false when GitHub listed only part of the checks), from the registry's latest read of the PR (`prs.ci_json`, the poller's last Details fetch) when the prompt goes out. A file, never names in the prompt: the PR's workflows name the checks. Empty when none failed, when the stored checks are another commit's, and always under `.Blind`. What to do with it is the skill's (one Checks line per check: caused by the PR, a P1 broken build, or unrelated) |
| `.ProjectChecks` | the review's Checks line for the round's agent CLIs whose sessions ran without the PR's changes to the project config they load from the checkout (see `project_untrust` below; the record their launches keep names the round's head), e.g. `Codex ran without the PR's AGENTS.md changes; Claude ran without the PR's CLAUDE.md and .claude/ changes`: magnum's names of the paths, never a path the PR named. Rendered as the `<magnum>` field `project_checks` by the initial, rereview and recovery prompts only when set, naming a CLI only when one of the round's roles, the judge included, is of its kind (the record names the kind's last launch on the head, which may be a session the round did not run); the skill copies it into the review's Checks |
| `.NotesPath` | the repository notes file, `~/.local/share/magnum/notes/<owner>/<repo>.md` (lower-case; under `<checkout>/state/` with `MAGNUM_HOME`); empty when there is none. The judge reads it and rewrites it when a round taught something durable (see Repository notes). The judge prompts pass it and the rows below as the `<magnum>` fields `notes`, `notes_dir`, `notes_harness`, `notes_lock`, `notes_unlock` and `notes_max_line`, only when it is set; the steps are the skill's |
| `.NotesDir`, `.NotesLock` | the harness directory next to the notes file (its path without `.md`) and the notes lock (that with `.lock`); empty without notes |
| `.NotesHarness`, `.NotesHarnessMore` | the harness directory's entries at prompt time (sorted, a directory ends in `/`, at most 40) and how many more there are |
| `.NotesLockCommand`, `.NotesUnlockCommand` | the shell lines that take the notes lock (printing `notes locked`, or `notes busy` after three minutes) and release it |
| `.NotesMaxLine` | `[notes] max_line`, the characters a notes line may have before it marks the repository for a curation (300 by default); the pipeline sets it from the config when the round has notes. The initial, rereview, recovery and continue prompts render it as the `<magnum>` field `notes_max_line` and, before the block, the sentence "Keep each line of the `notes` file at most `notes_max_line` characters long: a longer line triggers a curation."; the own-pass prompt, which writes no notes, does not. Left out when 0 (the startup render check, a test) |
| `.Readiness` | what magnum ran in the checkout before the reviewers (the `[[pool]]`'s `reset_db` when the slot's databases carry another schema than the checkout's `schema_paths`, `prepare` and `ready` of the `[[repo]]` or `[[pool]]`, and the `ruby` check): `.Checks` (each `.Kind`, `.Command`, `.Status` `ok`/`failed`/`timeout`/`skipped`, `.Detail` magnum's reason, `.Duration`), `.Failed` (how many did not pass) and `.File`, the JSON file that also holds each command's last output line. That line is the PR's code talking: name the file, never print it. Empty when nothing ran (initial, rereview and recovery rounds run the step; continue does not). The judge prompts list it as the `<magnum>` field `readiness`; what to do about a check that did not pass is the skill's |

#### `.Reports` entries

Each entry describes one non-judge role's report. Magnum completes the entry before rendering, so a
template can rely on the defaults below.

| Field | Meaning |
|---|---|
| `.Role` | the role's name, e.g. `claude-review` |
| `.Label` | how the prompt names the report; defaults to `.Role` |
| `.Path` | the report file, absolute; empty when the report is missing |
| `.Status` | the role's outcome this round (`ok`, `failed`, `timeout`, ...); informational |
| `.Missing` | there is no usable report this round; true whenever `.Path` is empty |
| `.Detail` | why the report is missing, e.g. `timed out after 40m`; defaults to `.Status`, then `no report` |

`.Reason` is the older name of `.Detail`. Prompt files that still render `missing ({{.Reason}})` keep
working.

### Role prompts (every other session role)

The prompts of claude-review, claude-simplify and any other non-judge session role (a droid or omp
reviewer, ...), initial, rereview and restart alike, and the fix role's `fix.md`. The fix job renders
`fix.md` with only some of these variables: `.URL`, `.Number`, `.Role`, `.HeadSHA` (the reviewed head the
fixer starts from), `.ReportPath`, `.RunID` and `.DBLockCommand`, plus the fix rows at the end of the
table (`.HeadRef`, `.ReviewURL`, `.ReviewFile`, `.ReviewerLogin` and `.SkillPath`). Every other variable,
`.Owner`, `.Repo`, `.BaseSHA`, `.BaseRef`, `.Budget`, `.Effort` and `.NotesPath` among them, renders empty
in `fix.md`. The whiteboard job renders the whiteboard role's `whiteboard.md` with `.URL`, `.Owner`, `.Repo`,
`.Number`, `.HeadSHA` (the reviewed head), `.BaseRef`, `.Checkout` (the slot's checkout, whose code the
whiteboard's views read), `.ReportPath`, `.RunID`, `.Role`, `.ReviewURL`, `.ReviewFile` and
`.WhiteboardSession`; `.BaseSHA`, `.HeadRef`, `.ReviewerLogin`, `.SkillPath`, `.Budget`, `.Effort` and
`.NotesPath` render empty in it. The prompt carries URLs, SHAs and paths, never PR text: the writer reads
the PR's title with `gh` in its pane.

| Variable | Meaning |
|---|---|
| `.URL` | the pull request |
| `.Owner`, `.Repo`, `.Number` | the pull request's owner, repository name (e.g. `talkable`) and number |
| `.HeadSHA` | the commit under review (checked out, detached) |
| `.PreviousHeadSHA` | the head of the role's previous run (rereview only, empty otherwise) |
| `.BaseSHA` | the merge base; `git diff {{.BaseSHA}}..HEAD` is the PR's diff |
| `.BaseRef` | the base ref, e.g. `origin/master` or the parent branch of a stacked PR; refused at render like the judge's when a shell would read it |
| `.ReportPath` | where the role writes its report (its `output`, absolute) |
| `.Model`, `.Effort` | the role's `model` (else its kind's `default_model`), and its effort for this round: `rereview_effort` in a re-review (when set), else `effort` (claude-review passes `.Effort` to `/code-review`) |
| `.EffortInPrompt` | as for the judge: the prompt must ask for `.Effort` in words |
| `.Mode` | `initial`, `rereview` for a new head after the role's earlier run, or `restart` (see below) |
| `.Since` | RFC 3339 time of the role's previous run (rereview only, empty otherwise) |
| `.ForcePushed` | `.PreviousHeadSHA` is no longer in the branch (rereview) |
| `.BaseMerged` | the commits since `.PreviousHeadSHA` merged the base branch in: as for the judge, the prompt compares the PR's own diff before and after them (rereview and its restart); `claude-simplify.md` then hands its subagents the PR's own diff, not `.PreviousHeadSHA..HEAD`, and keeps its proposals to what changed between the two own diffs |
| `.RestartedFrom` | the head the role was reviewing when a push cut its turn short (restart only, empty otherwise) |
| `.NotesPath` | the repository notes file (see Repository notes); the role reads it first. Empty when there is none |
| `.HistoryFile` | the round's `history.json`, the changed files' last commits on the base (as the judge's `.HistoryFile`); the claude-review prompts (initial, rereview, restart) name it in one sentence: read a commit that fixed the code or mechanism the PR touches and check the PR does not undo or re-break that fix. Empty when there is none |
| `.DocsFile` | the round's `docs.json`, the base's `.md` pages that name each changed file or its directory (as the judge's `.DocsFile`); the claude-review prompts (initial, rereview, restart) name it in one sentence: read the pages that bear on the change as the base has them (`git show <base>:<page>`), and report what a page records as deliberate, or as a known gap, only as pre-existing (`nearby` at most). Empty when there is none |
| `.PrepassFile` | the round's `prepass.json`, magnum's reading of the PR's diff (as the judge's `.PrepassFile`): the removed definitions that files outside the diff still name, the changed source files without a test change, the body of each method a hunk changes, each added file's closest existing sibling, or why it was skipped; the claude-review prompts (initial, rereview, restart) name it in one sentence: read it first, a changed method's body before its hunk, the sibling before the new file, each item a lead to check, never a finding by itself. codex-review runs the stock `codex review`, to which magnum passes no prompt, so it does not get it (nor `history` or `docs`). Empty when there is none |
| `.Stakes` | what the PR's repository puts at risk (as the judge's `.Stakes`): the claude-review prompts (initial, rereview, restart) give it in one sentence after the pre-pass's, to size the harm of each finding against, never against a stakes block in the checkout. codex-review gets none (no prompt), nor does claude-simplify. Empty when neither block sets it |
| `.EvidenceMap` | the PR's evidence map (as the judge's `.EvidenceMap`): the claude-review prompts (initial, rereview, restart) name it in one sentence after the pre-pass's: read the areas marked `unknown` or `stale` first, as leads, never findings by themselves, and probe no `probed` entry with `control: true` again. Empty when the PR has none and under `.Blind` |
| `.PostMerge` | GitHub merged the PR before magnum reviewed `.HeadSHA` (a post-merge review); the prompts that name the PR say "The PR is already merged; review it anyway." only then |
| `.Budget` | the role's `timeout` in words (`40 minutes`): the claude prompts give it as the time budget, within which the turn must end with the report written (background work only when it finishes well within it, waited for before the report) |
| `.Magnum`, `.Checkout`, `.Role` | the magnum binary the daemon runs (absolute; empty: `magnum` on `PATH`), the checkout the role works in (absolute; in `whiteboard.md` the slot's checkout, the `repositoryPath` the whiteboard's code views read), and the role's name |
| `.DBLockCommand` | the line the role runs each command that touches the checkout's databases through, as the judge's (`magnum db-lock --checkout .Checkout --role .Role --`, shell-quoted; derived when empty, and the fix job passes its own): the claude prompts (review, rereview, restart, simplify) and the fix prompt name it in one sentence, `` `{{.DBLockCommand}} <command>` ``, with exit 75 meaning the check did not run |
| `.RunID` | the run the prompt starts. The shipped prompts ask for `<!-- magnum:run={{.RunID}} -->` as the report's first line; a prompt that names this marker makes it required: a report without it, or with another run's, is stale (`missing`), since report paths are per head and an earlier run that kept working may write there. A prompt that does not name it needs none |
| `.HeadRef` | the fix prompt only: the PR's branch, which the fixer pushes to (`git push origin HEAD:{{.HeadRef}}`); refused at render, like `.BaseRef`, when a shell would read it |
| `.ReviewURL`, `.ReviewFile` | the fix and whiteboard prompts only: the review the fixer fixes, or the one the whiteboard's last section quotes, and its `review.json` (the review's event, body and comments with path, line, side and body, as the judge posted it) |
| `.ReviewerLogin` | the fix prompt only: the login that posted the review, whose threads the fixer answers |
| `.SkillPath` | the fix prompt only: the fix role's `skill` (`skills/magnum-fix/SKILL.md`; without a checkout, the copy built into the binary), absolute; in the daemon, the copy it took at startup. `fix.md` starts with its link and "Read <path> first, unless this session already read that file, and follow it; load no other skill." |
| `.WhiteboardSession` | the whiteboard prompt only: the Whiteboard session of an earlier head of the PR (the registry's `pr.<id>.whiteboard` record), which the writer moves to `.HeadSHA` with `session_set_target` and updates; `""` for a PR's first whiteboard, which the writer creates with `session_create` |

When a session role's `timeout` passes, magnum types one fixed last call into its agent within the same
run (not a prompt file): time is up, stop every background task with TaskStop, write the report to
`.ReportPath` now (starting with its run marker, when the prompt named one) with the checks stopped or
not run listed as pending. The turn gets 5 more minutes before it is interrupted as timed out. A
reviewer interrupted with background work still running (its transcript shows it) gets one more fixed
message: stop every background task with TaskStop and do nothing else; magnum then waits up to 2
minutes for the transcript to show none and counts what is left in the warning.

A role without its own `<name>-rereview.md` (or `rereview` key) reuses its initial prompt for a new
head. It then sees `.Mode` as `rereview` with `.PreviousHeadSHA` and `.Since` filled, so one file can
branch with `{{if eq .Mode "rereview"}}...{{end}}`.

A push that lands while the reviewers run (before the judge is prompted) restarts them in place on the
new head, at most `[daemon] max_round_restarts` times per round: the turns in flight are interrupted,
the checkout switches, and each session role is prompted again with its `restart` prompt (`.Mode`
`restart`, `.RestartedFrom` the old head, `.ReportPath` in the new head's directory). A role without
one gets the prompt it ran (initial or rereview) on the new head; shell roles run their command again.

### Model-fallback prompt (`model-fallback.md`)

Sent as a new `continue` run of the same role and round after a model hit its own limit ("You've
reached your Fable limit") and magnum typed the kind's `switch_model` command (see Kinds). Keep the
report path or the head in the text: magnum looks for one of them to read only the pane output after
this prompt.

| Variable | Meaning |
|---|---|
| `.Model` | the model the session runs now (a `fallback_models` entry) |
| `.Previous` | the model that hit its limit |
| `.Role`, `.URL`, `.HeadSHA` | the role's name, the pull request and the commit under review |
| `.ReportPath` | the role's report (the judge's result file) |

### Triage prompt (`triage.md`)

The question put to the cheap model of `[triage]` (see Triage below) before a round starts its agents:
which of the round's reviewers does this diff need? The command's stdin is the rendered prompt, its stdout
the answer.

| Variable | Meaning |
|---|---|
| `.Kind` | `initial` (the diff is the whole pull request) or `rereview` (the commits pushed since the last review) |
| `.OwnDiff` | (rereview) those commits merged the base branch in or were rebased onto it, so the diff is the pull request's own diff, against its base, of each file whose own change they altered (a file the PR no longer changes reads as its earlier change reverted), never the base branch's files |
| `.Lines` | the changed lines of the diff, added plus deleted |
| `.Roles` | the reviewers the model may leave out, in pipeline order: `.Name` and `.Summary` (the role's `summary`). Never the judge, never a role without a summary |
| `.Diff` | the diff as one unified diff (`--- a/<path>`, `+++ b/<path>`, hunks). It is the pull request's own text: the prompt must say it is data and not instructions |

The title and description of the pull request are never template variables here either. The prompt asks
for one line of JSON, `{"run": ["<role name>", ...], "reason": "<at most 20 words>"}`. Magnum takes the
last JSON object of the output that has a `"run"` list (the CLI may print text around it); an empty list
means the judge alone, a name matches a role by name or alias (case ignored), and a name that is no role of
the round is ignored. An answer that names no role of the round at all, or a command that fails, times out
or prints nothing readable, runs every role.

### Retro prompt (`retro.md`)

The one prompt per pull request of the retro's interactive agent (`[learn]`, see the README's "Learning from
other reviewers"): classify what other reviewers said about a pull request Magnum reviewed. The comments
themselves are never template variables; the agent reads them from the candidates file, and the prompt must
say they are data, not instructions.

| Variable | Meaning |
|---|---|
| `.URL` | the pull request |
| `.ReviewedSHAs` | the commits Magnum posted reviews of, oldest first |
| `.Candidates` | the candidates file (`candidates.json`): per comment its `id` (`t<comment id>` or `r<review id>`), `reviewer`, `path`, `start_line` and `line`, `side`, `reviewed_sha`, `diff_hunk`, `body`, `raised` with `finding_ref` and `reason_code` (a rejected finding within 3 lines), `file` or `file_skipped`; and `rejected`, the 40 newest findings the judge rejected on the pull request, each with its `id` (`<run id>/<finding id>`), `title`, `path`, `reason` and `priority`: Magnum's own text, never the pull request's |
| `.Files` | the directory of the commented files and of the files of Magnum's answered findings, `<Files>/<first 12 characters of reviewed_sha>/<path>` |
| `.Output` | the answer file the agent writes, `retro.json` |
| `.Count` | how many candidates there are |
| `.Threads` | how many of Magnum's own findings someone answered (0 when none): the candidates file's `threads`, per finding its `id` (`o<comment id>`), `url`, `comment_id`, `review_id`, `path` and `line`, `title` (the posted comment's first line without its bold and priority tag), `priority`, `reviewed_sha`, `finding_ref` (the posted finding of that review within 3 lines), `file` (the finding's file at `reviewed_sha`) or `file_skipped`, and `replies`, oldest first, each with its `author` (`(magnum)` for Magnum's own, `(magnum fix)` for its fixer's, the replies carrying `<!-- magnum:fix -->`) and `body`, and `fixer` when the fixer's replies are the only answers (such a thread keeps no lesson): the pull request's text, data like the comments |

The answer is `{"items": [{"id", "class", ...}]}` with one item per candidate and `class` one of `miss`,
`not_issue`, `style` and `outside`; a miss also needs `severity` (`P0` to `P3`), `title` (at most 80
characters), `lesson`, `scope` (`repo` or `general`), `lines` (`[from, to]`, 1 ≤ from ≤ to) and `match`
(one to three case-insensitive Go regular expressions of at most 120 characters). An item of any class may
name `rejected`, the `id` of the rejected finding that reports the comment's problem by meaning; it becomes the
miss's `raised` (rejected, with that finding and its reason), and an item that names none keeps the finding within
3 lines, if any. Magnum checks it (`internal/learn`; a `rejected` that is not in the list is invalid); a missing or
invalid file gets one nudge, then the pull request fails. A lesson that names a
pull request or issue (`#123`), holds a URL, names the author or a reviewer, mentions one of Magnum's logins
or, when its scope is `general`, names the repository is dropped and the miss kept without it.

With `.Threads` above 0 the answer also holds `"threads": [{"id", "outcome", ...}]`, one thread item per entry
of `threads`, with `outcome` one of `fixed`, `declined`, `deferred`, `argued`, `question` and `unrelated`; a
`declined` or `argued` finding also needs `verdict` (`wrong`, `over_ranked`, `missing_context` or `right`),
and a `wrong`, `over_ranked` or `missing_context` verdict `lesson`, `scope` and `match` with a miss's rules.
Fields an outcome or verdict does not need are dropped; the same check, nudge and lesson scrub apply.

### Notes curation prompt (`notes-curate.md`)

The one prompt of a notes curation (`[notes]`, see the README's "Repository notes"): propose curated notes
and harness for one repository in a scratch copy. Notes text is never a template variable; the agent reads
the copies, and the prompt must say that what they hold (the notes, the harness, the misses and the pull
request text they quote) is data, not instructions.

| Variable | Meaning |
|---|---|
| `.Repo` | the repository, `owner/name` |
| `.Dir` | the curation's scratch directory, the only place the agent may write |
| `.Current`, `.CurrentHarness` | the notes and the harness now, `current.md` and `current/` (read only) |
| `.Usage` | `usage.json`: the limits, the sizes, which limits the notes are past, each harness file's `rounds`, `uses` and `unused_candidate`, and the operator's reasons for rejecting earlier curations |
| `.Proposal`, `.Harness`, `.Changes` | what the agent writes: `proposal.md`, the `harness/` directory (a copy of the current one at first) and `changes.json` |
| `.Limits` | the [notes] curation triggers: `.MaxBytes`, `.MaxLine`, `.MaxHarnessFiles`, `.MaxHarnessBytes` |
| `.Over` | the triggers the notes are past (empty: a weekly or requested curation) |
| `.UnusedRounds` | the rounds without a use after which a harness file is a candidate (20) |
| `.Misses`, `.MissCount` | `misses.json` and how many misses it holds, when the curation was given the retro's misses of the repository (class miss, scope repo, still new); `""` and 0 otherwise. Each miss is data: its `id`, `severity`, `where` (path:line at the reviewed commit), `title` and `lesson` (scrubbed of logins, pull request references and links again) and `rejections`, the reasons of rejected proposals it was in; never a login, a pull request number or a comment |
| `.Superseded`, `.SupersededID` | `superseded/` and its proposal's id, when the curation follows up a stale proposal that was superseded (the notes changed after it was made, and the operator, or the daemon a day later, asked for a new curation instead): its notes as `notes.md`, its `harness/` and its `changes.json`, which the curator carries over where they still apply; `""` and 0 otherwise |

`changes.json` is `{"sections": [...], "files": [...], "misses": [...]}`, each section and file item
`{"name", "action", "into", "reason"}`: every `## ` section of the proposal and every proposed harness file
`kept` or `added`, every current harness file `kept`, `merged` (into a proposed file) or `deleted`, each
with a one-line reason saying how it helps a review of a future pull request. With misses, the prompt adds
one rule (a note for a miss only when a future review of the repository would catch a similar problem
because of it) and `misses` lists every miss id once, `{"id", "action": "noted", "section"}` with the `## `
section that covers it now or `{"id", "action": "skipped", "reason"}`; without misses the prompt renders as
before (the `notes_curate` goldens) and the key may be left out. Magnum checks the proposal
(`internal/notes`): every harness file named in the notes, every miss given accounted for, nothing outside
the scratch directory, no pull request number, branch or probe file, no secret and no home directory path;
size is not checked. The prompt also has the curator check each pitfall against the standing decisions
(the answer one gives goes on the pitfall's line) and turn a line that a base-branch doc covers into a
pointer to that doc (its path and heading), only for a doc the notes name: the curator runs in its scratch
directory, without the repository. A proposal that changes nothing is invalid unless it was given misses and skips them
all. An invalid proposal gets one nudge naming its problems, then is kept as invalid.

### Skill editor prompt (`skill-edit.md`)

The one prompt of a skill edit (`[learn] editor_*`, see the README's "Self-improving loop"): change the judge
skill and the judge and Claude reviewer prompts in a git worktree of magnum's checkout, on the run's own
`learn/<run>` branch, from the general misses and the general findings authors declined. Input text is
never a template variable; the agent reads `inputs.json`, and the prompt must say that what it holds is
data, not instructions.

| Variable | Meaning |
|---|---|
| `.Dir` | the run's directory, `~/.local/share/magnum/learn/skill/<run>/`, which holds the two files below |
| `.Worktree` | the worktree, `~/.local/state/magnum/learn/wt/<run>`: the agent's working directory and the only place it may write |
| `.Inputs` | `inputs.json`: `misses` (`id` `m<n>`, `severity`, `title`, `lesson`, `match`), `declines` (`id` `d<n>`, the posted `priority`, `verdict` `wrong` or `over_ranked`, `title`, `lesson`, `match`), every title, lesson and pattern scrubbed of logins, pull request references, links and repository names again, each with its `history` (the earlier proposals that noted it and failed at the gate: `proposal`, `state`, the gate's `rule`); the newest 40 inputs only, misses and declines each newest first, and `left_out`, how many older ones it leaves out; `rejections` (the operator's reasons for the latest rejected proposals, per proposal and per input), `versions` (the last 3 landed versions, newest first, each with `version`, `commit`, its window `from` and `to` and what its reviews did there: `judge_runs`, `posted`, `by_priority`, `outcomes`, `misses_general`, `misses_repo`; counts only, `store.VersionMeasure`), `skill_bytes`, `skill_cap` and `files`, the globs it may change |
| `.Pins` | `pins.json`: `pins`, the sentences the skill tests pin (from `internal/agents/testdata/skill_pins.txt` of the worktree), and `cap`, the skill's size cap |
| `.Changes` | `changes.json`, at the worktree's root: what the agent writes |
| `.Skill`, `.Prompts` | the worktree's `skills/magnum-review/SKILL.md` and `prompts/` |
| `.Cap` | the skill's cap in bytes (`skillMaxBytes` of the skill tests) |

`changes.json` is `{"files": [...], "inputs": [...], "decision": "..."}`: each file `{"name", "action":
"edited"|"unchanged", "reason"}` (every file the worktree's status lists is an allowed one and listed
`edited`), each input of `inputs.json` once, `{"id", "action": "noted", "where"}` or `{"id", "action":
"skipped", "reason"}` with a one-line reason of at most 300 characters, and `decision`, 1 to 1,200
characters whose first sentence is the change's headline: magnum appends it to the last section of
`docs/DECISIONS.md`, `## Learned rules` (the loop's own), as an entry dated the
run's day and naming the inputs, and commits. Magnum checks the result
(`learn.ValidateEdit`): the worktree still at its base on its branch (the agent commits nothing), only the
existing `skills/magnum-review/SKILL.md`, `prompts/judge-*.md` and `prompts/claude-*.md` changed in place
(none added or deleted), every `{{...}}` action of a changed prompt kept in order, the skill within its cap
and keeping every pin, no pull request reference, branch, secret or home directory path and no login, link
or repository in the added lines or the decision; then the candidate's prompts render (`engine.CheckPrompts` on a config pointed at the
worktree), the decision's entry is appended to the worktree's `docs/DECISIONS.md` (put back as it was
before each check, as are the golden files), and the format gate passes in the worktree: the golden files
regenerated by every test that writes one (`go test ./internal/agents -run
'^(TestRenderGolden|TestJudgePromptsNameTheFormerLogins|TestRereviewPromptNamesTheThreadsFile)$'
-update`, never the whole package, whose -update rewrites the pins), `go test ./internal/agents ./internal/pipeline
./internal/config`, and `make check-private` with the checkout's mise environment, which so checks the
entry too. A result with problems gets one nudge naming them (a failing command with the end of its
output), then is kept as invalid.

### Repository notes

magnum keeps one Markdown file per repository, `~/.local/share/magnum/notes/<owner>/<repo>.md` with owner
and repository lower-cased, and a directory with the same name without `.md` next to it for QA scripts.
The built-in reviewer prompts tell their role to read the file first as hints from earlier reviews to
verify. The judge prompts pass the file, the harness directory, its files (so a script the notes no longer
mention is visible), the lock commands and the longest line the notes may hold before a curation starts
as `<magnum>` fields (`notes`, `notes_dir`, `notes_harness`, `notes_lock`, `notes_unlock`,
`notes_max_line`), with one sentence that asks the judge to keep each line within it, and the skill
(section 2) holds the steps once: rewrite the file (never
append) after posting when the round taught something durable that helps review a future pull request
(what the repository is, how to test, lint and QA a change, failures of the review machine, known
pitfalls with the authors' answer to one on its line, standing decisions the authors made, under a dated header line; a finding they confirmed but left
undecided is open, never declined), never one pull request's findings, code or
probes: a probe for one pull request stays in the round's report directory. The judge's result names the
harness files it ran or read (`harness_used`). Judges of different PRs
of one repository run at the same time, so the judge takes the notes lock (`.NotesLockCommand`: a
directory created with `mkdir`, which outlives the shell command, waited for at most three minutes and
taken over after ten), reads the current file again, merges its lessons into it, writes it through a
temp file and `mv`, and releases the lock. A custom prompt opts in with
`{{if .NotesPath}}...{{.NotesPath}}...{{end}}`. `magnum notes <repo>` prints the file and its sizes,
`magnum notes <repo> --edit` opens it in `$VISUAL` or `$EDITOR`, and the README's "Repository notes"
describes the history and the curation.

### Shell roles (`command`, or a full-line `.sh` prompt)

Paths, refs, URLs, titles and args are shell-quoted when they need it (plain words are left alone)
before they are substituted, because the result is typed into a shell.

| Variable | Meaning |
|---|---|
| `.Title` | the pane title, e.g. `PR #729 codex-review - talkable`; empty = no title prefix |
| `.Command` | the role's `command` template text, not quoted (empty in a full-line template: a role sets `command` or `prompt`, not both) |
| `.Args` | the role's `args`, a list of quoted words (a `command` gets them appended after its text, so use `.Args` in full-line templates) |
| `.Capture` | the role's `capture`: `file` or `stdout` |
| `.ReportPath` | the role's `output`, absolute |
| `.Marker` | the done marker, `MAGNUM_DONE_<run id>`; a `command` line gets `; printf '\n<marker> %d\n' "$?"` appended (the marker on a line of its own with the exit status), a full-line template must print it itself |
| `.BaseRef` | the base ref, e.g. `origin/master` or the parent branch of a stacked PR |
| `.BaseSHA` | the merge base of the head and the base; empty when unknown. It does not move when the base branch gains commits, so codex-review passes it to `--base` (`{{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}`) |
| `.HeadSHA`, `.URL` | the commit under review and the pull request |
| `.Model`, `.Effort` | the role's `model` (its `[[role]]` block's or its spend level's; no kind default), and its effort for this round: `rereview_effort` in a re-review (when set), else `effort`; empty when unset. codex-review's command passes them as `{{if .Model}} -c model={{.Model}}{{end}}` and `-c model_reasoning_effort={{.Effort}}`; a shell role's `model` needs a command (or full-line template) that uses `.Model`, else the role is refused |
| `.MCPOff` | the args that keep configuration out of the session of the kind named by `tool`, quoted words: they turn off your MCP servers (`mcp_off`: Codex's one by one and its apps connector with `mcp_strict`, Claude's with `mcp_strict`), keep the checkout's project config out when the PR changes it (`project_untrust`: Codex's `.codex/`, Claude's `.claude/` and `.mcp.json`), else turn off Codex's project servers under `project_mcp = "off"`; codex-review's command passes them after its effort (`{{range .MCPOff}} {{.}}{{end}}`) |
| `.DBLockCommand` | the line a command that touches the checkout's databases runs through, as for the session roles: `magnum db-lock --checkout <slot> --role <role> --` (the daemon's binary, the round's slot and the role's name, shell-quoted; without a slot db-lock takes the work tree it runs in). It is a ready shell line, quoted once, and never empty. codex-review's command and `codex-review.sh` start with it (`{{.DBLockCommand}} codex review ...`): db-lock waits for the other roles' runs, holds the lock for the whole command, passes its output and exit status through (75 when the wait timed out) and runs `codex` from `PATH`, not your shell function |
| `.RunID` | the run id, i.e. `.Marker` without its `MAGNUM_DONE_` prefix; a line that prints `<!-- magnum:run={{.RunID}} -->` makes it the report's required first line, as for a session role |
| `.ExtraArgs` | the older name of `.Args`, for full-line templates written for `codex-review.sh` |

`.Marker` and `.RunID` are restricted to letters, digits, `.`, `_` and `-`. A template that uses
`.BaseRef`, `.BaseSHA`, `.ReportPath`, `.HeadSHA`, `.URL`, `.Marker` or `.RunID` while that value is empty
is refused, because an empty value would shift the command's arguments (`--base` without a ref); a
template that names both `.BaseSHA` and `.BaseRef` needs only one of them. `.Title`, the args and
`.Capture` may be empty. `capture = "stdout"` needs a report path.

## Kinds

Every key is optional and overrides only itself. codex, claude, droid and omp are built in; declaring
any other name adds a kind that starts from the same health patterns, `wrapper = "auto"`,
`session_source = "herdr"`, `on_permission_prompt = "deny"` and the default `after_deny_prompt`.

| Key | Meaning |
|---|---|
| `wrapper` | `auto` probes `zsh -ic 'whence -w <kind>'`: a function or alias supplies its own flags. `true` or `false` skip the probe. `bypass` removes the function or alias named like the kind (`unset -f`, `unalias`) before every launch and resume, so the binary runs with magnum's args alone (`args` appended). |
| `start` | args always appended |
| `args` | args appended only without a wrapper (the old `[codex]`/`[claude]` `args`). codex defaults to `["--dangerously-bypass-approvals-and-sandbox"]` and claude to `["--dangerously-skip-permissions"]`, so a plain binary runs like the usual zsh wrappers, without approval prompts (magnum answers every prompt No) and, for codex, without the sandbox (a sandboxed judge cannot reach GitHub). `magnum doctor` fails a plain codex or claude binary whose session roles start without that flag |
| `resume` | args that resume `{session}` |
| `model`, `effort` | args that pass a role's `{model}` or `{effort}`; empty means the CLI cannot take it |
| `subagents`, `no_subagents` | args that cap the subagents a session may have open at once to a role's `max_subagents` (`{subagents}`, 1 or more), and args that turn subagents off (`max_subagents = 0`); empty means the CLI cannot cap them and the role key is ignored. codex: `["-c", "agents.max_concurrent_threads_per_session={subagents}"]` (Codex counts the spawned-agent threads open at once, the primary excluded) and `["-c", "agents.enabled=false"]` |
| `default_model` | the model of the kind's roles that set no `model`, passed through `model` args (e.g. `default_model = "gpt-6.1-sol"` under `[kinds.codex]` starts the codex judge with `--model gpt-6.1-sol`); `""` = the CLI's own default. A shell role (codex-review) takes its model through its own `args` |
| `mcp_off`, `mcp_allow`, `mcp_disable`, `mcp_strict` | `mcp_off` (codex and claude: `true`) keeps your MCP servers out of every launch and resume. Codex: each MCP server the Codex `config.toml` declares (`[mcp_servers.<name>]` not set `enabled = false`; the file in the `CODEX_HOME` of the pane's `env`, else `$CODEX_HOME`, else `~/.codex`) goes off, except those named in `mcp_allow`, by passing `mcp_disable` once per server (codex: `["-c", "mcp_servers.{server}.enabled=false"]`); a name that is no TOML bare key (letters, digits, `_`, `-`) stays on, with a log line; `mcp_strict` (codex: `["-c", "features.apps=false", "-c", "skills.include_instructions=false"]`) turns off once Codex's built-in apps connector (`codex_apps`, ChatGPT's connectors), which no `[mcp_servers]` table declares, whatever `mcp_allow` names, and keeps your skills list out of every turn (`[]` keeps both on). Claude: `mcp_strict` (claude: `["--strict-mcp-config"]`) turns them all off at once (user, local, project and plugin servers, claude.ai connectors); `mcp_allow` cannot name servers there (a claude kind setting it is refused), so add `"--mcp-config", "<absolute path>"` to `mcp_strict` to keep the servers of a file you write (`{"mcpServers": {...}}`). A kind without `mcp_strict` and `mcp_disable` ignores `mcp_off`. A machine with a `managed-mcp.json` refuses `--strict-mcp-config` (Claude Code exits at startup): set `mcp_off = false` there. A shell role whose `tool` is the kind gets the same args as `.MCPOff` |
| `plugin_disable`, `plugin_allow` | Codex plugins, under `mcp_off`: `plugin_disable` is the args that turn off plugin `{plugin}` (`name@marketplace`), passed once per plugin installed in the Codex home's `plugins/cache` or declared by a `[plugins."…"]` table, except those `plugin_allow` names (codex: `["-c", "plugins.{plugin}.enabled=false"]`). A plugin's MCP servers are no `[mcp_servers]` table, so `mcp_disable` does not reach them |
| `project_untrust`, `project_docs_off`, `project_mcp` | a PR controls its checkout's project config: Codex's `.codex/`, which Codex loads for a trusted folder (config with MCP servers, hooks, rules), and the `AGENTS.md` or `AGENTS.override.md` at its root, the project's instructions; Claude's `.claude/` and `.mcp.json` (settings with hooks, env and plugins, MCP servers, skills, commands, agents) and the `CLAUDE.md`, `CLAUDE.local.md` and `AGENTS.md` of any directory (Claude Code loads a directory's when it reads files there, and `AGENTS.md` where the project has no `CLAUDE.md`). When the files there (the paths in any case: macOS opens `.Claude/settings.json` for `.claude/settings.json`; what an instruction file at the root links to, too) differ from the merge base of the checkout's `HEAD` and `origin/<base>` (the round's merge base for a shell role; files a round left there count, ignored ones too, but for untracked logs, a file under `log/` or `logs/` right inside `.codex/` or `.claude/` or named `*.log` or `*.log.<digits>` there, and for an ignored instruction file below the root, such as a package's in `node_modules/`; so does git failing), every launch, resume and `.MCPOff` passes `project_untrust`, with `{projects}`, where an arg has it, replaced by a TOML table marking the checkout and its symlink-resolved path untrusted. codex: `["-c", "projects={projects}"]`, so the session loads none of `.codex/` and no `AGENTS.md`, and magnum answers Codex's "Folder access" with "Open restricted"; claude: `["--setting-sources", "user"]`, so it loads the user's settings only. When the changed files are all instruction files, `project_docs_off` goes instead (codex: `["-c", "project_doc_max_bytes=0"]`: no project `AGENTS.md`, the rest of `.codex/` as before; claude: none, as `project_untrust` covers them). An `agents.codex_project_declined` (`agents.claude_project_declined`) event, the card and the judge's `project_checks` field say so, naming the files. Claude Code reloads its project config while it runs, so a Claude session that loaded it is parked before the checkout moves to a new head that changes it and resumed after. `project_untrust` empty = a changed config loads (and `project_docs_off` does not apply). Otherwise the project config is the base branch's and loads, but `project_mcp = "off"` (default `allow`) turns Codex's project MCP servers off through `mcp_disable`, except those in `mcp_allow` (claude: ignored). No git runs for a checkout without any of Codex's paths |
| `name` | args that name the session `{title}` at launch |
| `rename` | a slash command typed while the agent works, e.g. `/rename {title}`; `""` = none |
| `login_check`, `login_ok` | a read-only command (split on spaces) and how its output reads as logged in: `text:<substring>`, `regex:<expr>`, `json:<dotted.path>` or `""` for exit status 0 |
| `login_file` | the CLI's login file, a path under the kind's home (codex: `auth.json` under its `CODEX_HOME`, else `~/.codex`); `""` = none. A login pause a run proved lifts only once `login_check` passes and this file changed too, because `codex login status` reads the saved tokens and says logged in while a revoked refresh token fails every call |
| `env` | extra pane environment (a role's `env` wins) |
| `health_patterns` | `login_required`, `model_limit`, `usage_limit`, `overloaded` and `refused` regexp lists, case-insensitive; a list replaces the built-in one. `refused` (Codex's safety warnings, such as "flagged for possible cybersecurity risk") ends the round at once and flags the PR for good: no round of any kind until `magnum codex-flag clear`. `model_limit` (one model capped, checked before `usage_limit`) may capture the model in a `(?P<model>...)` group; without one the session's current model is the limited one |
| `switch_model` | a slash command typed into the idle agent's pane to switch its session to `{model}`, e.g. `/model {model}`; `""` = the kind cannot switch in-session, so a model limit pauses the kind like a usage limit |
| `fallback_models` | the models a session switches to, in order, when its model hits its own limit; unused without `switch_model` |
| `reset_model` | the `switch_model` argument that returns a session to the CLI's own default once the limit is over, when neither the role's `model` nor the kind's `default_model` is set (claude: `default`); `""` = it stays on the fallback |
| `exit_keys` | the keys a hard park sends an agent that is still its pane's foreground process after two ctrl+c (a fresh start's second try, `magnum abort`'s last), as herdr's `pane send-keys` takes them: one key combo each (a printable key, `enter`, `esc`, `ctrl+d`), never a word. It then waits 10 seconds more and closes that pane alone, never the workspace, and the session (`pr.park_forced`). claude: `["/", "e", "x", "i", "t", "enter"]` (`/exit`); `[]` = the hard park closes the pane straight away |
| `session_source` | `herdr` (herdr reports the session id, so sessions resume) or `none` (always fresh) |
| `on_permission_prompt` | an approval prompt that blocks the agent during a magnum run: `deny` (default) answers its No option, never Yes, at most 10 times per run, each recorded as an `agent.prompt_denied` event; `wait` leaves it to you. The first-launch folder-trust dialog is handled separately. |
| `on_hooks_review` | Codex's startup "Hooks need review" (hooks new or changed since Codex last trusted them): `trust_own` (default) picks "Trust all and continue" when the checkout declares no hooks of its own (no `.codex/hooks.json`, no `hooks` or `plugins` in its `.codex/config.toml`), so every hook listed is yours (Codex home or an installed plugin), recorded as `agents.hooks_trusted`; otherwise, or with `decline`, "Continue without trusting" for that session, recorded as `agents.hooks_declined`. |
| `after_deny_prompt` | the message sent once, in the same run, when an agent stops its turn after a deny (Claude Code does, also while a fork it started keeps it busy: its transcript shows the turn's end), so it finishes without the command; it counts toward the 10 per run and is recorded as `agent.deny_continued`. Default: "magnum denied that command: review roles never run approval-gated or destructive commands. Continue the task without it and finish as instructed."; `""` sends nothing. |

The launch args are built in this order: `resume`, `name`, `model`, `effort`, `subagents` (or
`no_subagents`), `mcp_strict` and `mcp_disable` per server, `project_docs_off` or `project_untrust`, `start`, `args`, then the role's `args`. `magnum roles --kinds` prints the effective kinds, merged with your overrides, so you
can see exactly what magnum will type.

| Kind | resume | model | effort | name / rename | login check |
|---|---|---|---|---|---|
| codex | `resume {session}` | `--model {model}` | `-c model_reasoning_effort={effort}` | `/rename {title}` | `codex login status`, `text:Logged in` |
| claude | `--resume {session}` | `--model {model}` | `--effort {effort}` | `--name {title}` | `claude auth status`, `json:loggedIn` |
| droid | `--resume {session}` | none | none | none | none |
| omp | `--resume={session}` | `--model={model}` | `--thinking={effort}` | none | none |

```toml
[kinds.pi]                            # any herdr agent kind
resume = ["--session", "{session}"]
model = ["--model", "{model}"]
login_check = "pi auth status"
login_ok = "regex:(?i)logged in"

[kinds.codex.health_patterns]         # extend a classifier list (the list replaces the built-in one)
usage_limit = ["you've hit your (?:usage )?limit", "usage limit reached", "credits exhausted"]
```

A model that hits its own limit while the account still has usage (`model_limit`) does not pause the
kind when the kind can switch: magnum records the limit (until the reset the text names, else
`[daemon] model_limit_cooldown`, default 5h), types `switch_model` with the next `fallback_models`
entry into the idle pane and sends `model-fallback.md`, at most once per fallback model per run. While
the limit lasts, new sessions of a role on that model start on the fallback and live ones switch before
their next prompt; afterwards they switch back to the role's `model`, else the kind's `default_model`,
else `reset_model`. claude ships
with `switch_model = "/model {model}"`, `fallback_models = ["opus", "sonnet"]` and
`reset_model = "default"`; the other built-in kinds cannot switch, so a model limit there pauses the
kind as a usage limit does. `magnum status` lists each limit under pauses (`claude: fable limited until
17:30, using opus`). Claude Code's `/model` also saves the choice as the default for your new sessions
until magnum switches back. In a session with history Claude Code asks "Switch model?" first: magnum
answers "Yes, switch to …" with Enter, only while the cursor is on that option, and counts the switch
only once the status line names the new model (the visible screen's last lines, e.g. `Opus 5.5 | …`);
a switch not confirmed within 30 s backs out of the dialog with Esc and pauses the kind as before.

## Roles

| Key | Default | Meaning |
|---|---|---|
| `name` | required | unique, `^[a-z][a-z0-9-]{0,23}$`; pane labels, titles and `magnum review/open/watch --role` use it |
| `kind` | required | a declared kind, or `shell` for a shell command |
| `mode` | from `kind` | `session` for agent kinds, `shell` for `kind = "shell"` |
| `judge` | `false` | the role that posts the review; exactly one per watch |
| `fix` | `false` | the fixer of the fix job (`magnum fix`): a session role that takes part in no round, so a watch's `roles` may not name it and `magnum roles` leaves it out. `[pipeline] fix_role` names the role the job runs, `skill` is the fix skill, and an `identity` it names must be a `gh` one (none: the watch's poll identity). It takes no `runs`, `after`, `own_pass` or `max_session_rounds` |
| `whiteboard` | `false` | the writer of the whiteboard job: a session role that takes part in no round either. `[pipeline] whiteboard_role` names the role the job runs. Its `args` give the session the Whiteboard MCP server. It takes no `runs`, `after`, `own_pass`, `max_session_rounds` or `skill`, and an `identity` it names must be a `gh` one |
| `summary` | none | one line saying what the role checks, shown to the `[triage]` model. Only a role with a summary can be left out of a round by triage; the judge never can. The built-in reviewers have one |
| `runs` | `always` | `always`, `first` (until it completed once for the PR, then on request), `manual` (on request only) or `never` (disabled; requests are refused). Request a role with `magnum review <ref> --role <name>` |
| `rerun_min_lines` | `0`; claude-simplify `150` | with `runs = "first"`: the role runs again once this many code lines changed since its last completed run (comments, blank lines, whitespace moves and documentation do not count); `0` = only the first round, then on request. The other `runs` values ignore it |
| `identity` | the watch's | the `[[identity]]` whose GitHub environment the pane gets |
| `model`, `effort` | none | passed through the kind's `model`/`effort` args; also template variables (a shell role's command gets them too: codex-review's passes its `model` as `-c model=…` when it has one and `effort`, `high` by default, as `-c model_reasoning_effort=…`). A built-in role's `effort` and `rereview_effort` follow `[pipeline] spend` (`low`, `medium`, `high`) unless its block sets them, and the active level's `[spend.<level>]` table sets `model`, `effort`, `rereview_effort` and `max_subagents` over the block too |
| `max_subagents` | none; the judge `2` | the subagents the role's agent may have open at once, through the kind's `subagents` args; `0` = none (`no_subagents`); a kind without those args ignores it |
| `max_session_rounds` | `0`; the judge `6` | session roles: the review rounds one session of the role may serve on a PR, its resumes included (an own pass, a continue or a nudge is part of its round); when a round starts after that many, the role starts in a fresh session (a live one is quit first, history kept), the judge through the recovery prompt at its `rereview_effort` while the reviewers keep theirs, any other role alone with the round's prompt. Never inside a round, for a continue or for a session at work; an `agent.session_rotated` event each time. `0` = no limit |
| `rereview_effort` | `effort` | the effort of re-review rounds (new commits after an earlier review); the judge's built-in default is `high`. A re-review starts or resumes an agent at it; a live session of a kind that sets effort only at launch is asked for it in the prompt (`.EffortInPrompt`) |
| `args`, `env` | none | extra launch args (shell roles: appended to the command, quoted) and pane environment |
| `prompt`, `rereview` | see below | prompt files for the first review and for a new head |
| `restart` | see below | session reviewers: the prompt after a push cut the role's turn short and the round restarted on the new head |
| `continue_prompt`, `recovery`, `nudge` | judge only | the judge's other prompts. A `stop` key is accepted and ignored: magnum never sent a stop prompt |
| `own_pass` | judge only | the judge's own-pass prompt, sent with the reviewers when `[pipeline] judge_own_pass` (or the watch's) is `parallel` |
| `skill` | `{{repo}}/skills/magnum-review/SKILL.md` | the judge's skill, `{{.SkillPath}}` |
| `command`, `tool` | none | shell roles: the command template, and the kind whose login check, pauses and health patterns apply |
| `ok_status` | `[0]` | shell roles: the exit statuses that count as a finished report; any other status fails the role (its output is then checked for login, usage-limit and overload errors) |
| `output` | `<name>.md` | the report file in the round's directory; judges `<name>.json` |
| `capture` | `file` | `file` (the role writes `output`) or `stdout` (shell roles; output is tee'd). No role may edit the checkout: one left modified after its stage is reset to the PR head, with a `round.checkout_dirty` warning |
| `timeout` | `40m` | per turn; judges `90m` (`daemon.reviewer_timeout` / `judge_timeout` are the timeouts of a role that sets none; the built-in roles set theirs) |
| `after` | none | roles that must finish first; roles outside the watch's set are ignored |
| `aliases` | none | other accepted names (the built-in roles keep `judge`, `claude`, `codex`, `codex_review`, `simplify`) |

Prompt defaults: a judge uses the six `judge-*.md` files. Another session role uses `<name>.md`,
`<name>-rereview.md` when that file exists (else `<name>.md` again) and `<name>-restart.md` when that
file exists (else no restart prompt). claude-review's are `claude-review.md`, `claude-rereview.md` and
`claude-restart.md`.

A round runs the watch's non-judge roles in parallel, layered by `after`, then the judge. The judge gets
every other role's report path. With `[pipeline] judge_own_pass = "parallel"` (the default; a `[[watch]]`
may set its own) the judge also starts its own pass with the first stage (`judge-own-pass.md`, a run of
kind `own_pass` that writes `judge-own.md` and posts nothing), and its usual prompt, in the candidates
phase, goes out once every stage and that pass ended; `after` prompts it once, after the reviewers. A
judge alone (a delta check, a continued turn, a round whose reviewers were all dropped) gets one prompt
either way. Declaring any `[[role]]` in the base config (`config.defaults.toml`, or a
`--config` file) replaces the built-in list. In your `~/.config/magnum/config.toml`, a `[[role]]` named
like an existing role overrides only the keys it sets, so `name = "claude-review"` plus `model = "opus"`
is a complete block, and a new name is appended.

### Triage

`[triage]` (off by default) lets a cheap model drop reviewers a small diff does not need, before the round
starts their agents. Magnum, not the model, holds the limits: see DECISIONS "Triage of small diffs".

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | ask the model; everything below applies only when it is on |
| `max_lines` | `120` | a round whose diff changes more lines (added plus deleted) runs every role, unasked |
| `command` | `["claude", "-p", "--model", "haiku", "--tools", "", "--no-session-persistence"]` | the model's CLI as an argument list; the prompt arrives on stdin and stdout is the answer. It runs in a private directory under the state directory (`~/.local/state/magnum/triage`), never in the PR's checkout. A Codex one: `["codex", "exec", "--model", "<a cheap model>", "--sandbox", "read-only", "--ephemeral", "--skip-git-repo-check", "-"]` |
| `timeout` | `2m` | a command that takes longer is killed and the round runs every role |
| `prompt` | `triage.md` | the prompt file, resolved like the roles' |

A first review or a re-review is triaged (a re-review on the commits since the last review, or, when they
merged the base branch in or were rebased, on the PR's own diff of the files whose own change they altered);
a continued round, an eval and a round that names roles (`magnum review --role`) are not. The decision, or the reason
every role ran, is a `round.triage` event on the PR.

### On-demand roles and inspecting the result

`magnum review <ref> --role <name>` runs an on-demand role for one round. The flag repeats. It requests
roles whose `runs` is `first` (after its first completion for the PR, before that it runs anyway) or
`manual`; a role with `runs = "never"` is refused. `--simplify` is a shorthand for `--role` with the
role aliased `simplify` (claude-simplify by default). `magnum open --role` and `magnum watch --role`
take any role name or alias of the PR's watch and default to the watch's judge.

`magnum roles [--repo owner/name] [--kinds] [--json]` prints the effective roles per watch: name, kind,
`runs`, model, effort, the rounds one session serves (`max_session_rounds`, `-` for no limit), capture,
output, the initial prompt file (and whether it comes from `prompts_dir`
or the embedded copy), `after`, the judge and the aliases, under a first line that names `prompts_dir`
and the `[pipeline] spend` level (the model and effort shown are the ones after the level and its `[spend.<level>]` table). `--kinds` prints the effective agent kinds
instead, i.e. what magnum will type to start, resume and name each one. `--json` prints either as JSON.

### The built-in roles

| Role | Kind | What it does |
|---|---|---|
| `codex-judge` | codex | Persistent session; reads the reports, runs `$magnum-review` and posts one review. Effort `xhigh`, `high` for re-reviews, at most 2 subagents open at once, 90 minutes. Starts fresh instead of resuming once its last turn on the PR ended more than `[pipeline] judge_fresh_after` (90m) ago, or once its session served 6 rounds on the PR (`max_session_rounds`). |
| `claude-review` | claude | Persistent session running `/code-review <url> high` (`medium` for re-reviews), leaving out style-only and pre-existing problems (but a pre-existing P1 or P2 in the code the PR touches, marked `nearby` for the judge's nearby block) and naming each finding's trigger; it also checks every caller of changed behaviour (non-production ones too), what a replaced mechanism did implicitly and any test failure or flake the change brings, and searches the base branch's log and docs (never the checkout's) before it drops a candidate (it lists no rejected candidates since 2026-10-14: none of them became a posted finding); counts an untested new or changed rule as a missing test, never too unlikely; lists each request entry point that reached an access hole the PR closes, with its access check (in these prompts only, not the judge's skill: such lists drew Codex cybersecurity flags on judge turns); checks a documentation change's statements about data (who can read it, how long it is kept, when it is deleted) against the code, the infrastructure files and the PR's other pages; for a flow a third party governs with published requirements (an app store listing, a platform app's install, OAuth, billing or webhook flow, an API's terms), reads the whole section of those requirements that covers it on that party's own site and checks each sentence against the PR, reporting a forbidden flow with the rule's number, its sentence and the URL; proves a security finding with a spec, never with attack tooling (its subagents too), and writes it as the input, the wrong read or write, the fix and the spec, never as an attacker's steps; writes `claude-review.md`. |
| `codex-review` | shell | Types `magnum db-lock --checkout <slot> --role codex-review -- codex review -c model_reasoning_effort=high [-c mcp_servers.<name>.enabled=false ...] --base <merge base>` (its `effort`, so never your global Codex effort; your Codex MCP servers off, see `mcp_off`; the base ref when the merge base is unknown) into a plain pane, so it takes its turn on the slot's databases like the other roles (its sandbox still reaches MySQL and Postgres on localhost) and `codex` comes from `PATH`, not from your shell function; its output is tee'd into `codex-review.md`. `codex review` takes custom instructions only as a review target of their own, in place of `--base`, so it gets none of claude-review's extra checks. |
| `claude-simplify` | claude | A read-only `/simplify`, alongside the reviewers on a PR's first review, again after `rerun_min_lines` changed lines, or on request (`magnum review --role claude-simplify`, or `--simplify`): four subagents review the diff in parallel for reuse, simplification, efficiency and altitude (one pass without the Agent tool), and instead of editing it writes every qualifying proposal, ranked, removals rather than renames or moves, with exact current and replacement lines, to `claude-simplify.md`. A re-review proposes only on lines changed since the previous review. |
| `claude-judge` | claude | A second judge, held in reserve: a watch without `roles` runs its first judge (`codex-judge`) only. `[pipeline] judge_fallback = "claude-judge"` hands a PR to it after a Codex refusal, and a watch's `roles` may name it as the watch's judge. Model `opus`, effort `xhigh` (`high` for re-reviews), the same `judge-*.md` prompts and skill as `codex-judge`, 90 minutes, writes `claude-judge.json`. |
| `claude-fix` | claude | The fixer of the fix job, which fixes the findings of magnum's review of your own PR (`magnum fix`). It takes part in no round. Model `opus`, effort `high`, `fix.md` and the fix skill, 60 minutes, writes `claude-fix.md`. It names no identity, so it acts as the watch's poll identity. |
| `claude-whiteboard` | claude | The writer of the whiteboard job, which writes the Whiteboard of a PR once magnum posted a review on it. It takes part in no round. Model `opus`, effort `medium`, `whiteboard.md`, no skill, 30 minutes, writes `claude-whiteboard.md`. Its `args` give the session the Whiteboard MCP server alone, and it names no identity either. |

### Shell roles

A shell role's `command` is a template over the shell variables. magnum types it as one line, like this:

```text
printf '\033]0;%s\007' <title>; DISABLE_AUTO_TITLE=true; set -o pipefail; { printf '<!-- magnum:run=<run> -->\n'; <command> <args...>; } | tee <output>; printf '\nMAGNUM_DONE_<run> %d\n' "$?"
```

The braces, the run marker and the `| tee` are only there with `capture = "stdout"`: the report then
starts with the run's marker, followed by the command's output, and a report without that marker is
stale. `pipefail` makes the printed status the command's rather than `tee`'s; a status outside the
role's `ok_status` fails the role even when it wrote output. To control the whole line, set `prompt` to
a `.sh` template instead of `command`; it must print the done marker itself, on a line of its own and
followed by the status (a bare marker counts as success). `codex-review.sh` shows the line magnum types
for codex-review.

## Examples

A droid role that proposes simplifications once, after both reviewers (`magnum review <ref> --role
droid-simplify` runs it again on request). It needs `prompts/droid-simplify.md`:

```toml
[[role]]
name = "droid-simplify"
kind = "droid"
runs = "first"                        # output defaults to droid-simplify.md
after = ["claude-review", "codex-review"]
```

```text
Propose simplifications of the lines {{.URL}} changes (head `{{.HeadSHA}}` is checked out; `git diff {{.BaseSHA}}..HEAD` shows them) and write them to {{.ReportPath}}: for each, the file and lines, what it removes, the exact current and replacement lines. Edit no file, do not commit and do not post to GitHub.
```

An omp reviewer on a specific model. It needs `prompts/omp-review.md`, and optionally
`prompts/omp-review-rereview.md`:

```toml
[[role]]
name = "omp-review"
kind = "omp"
model = "gpt-5.2"                     # --model=gpt-5.2
effort = "high"                       # --thinking=high
timeout = "30m"
```

```text
Review the pull request {{.URL}} (head `{{.HeadSHA}}`, diff against `{{.BaseSHA}}`). Do not edit files and do not post to GitHub. Write every finding to {{.ReportPath}} as Markdown with severity, file:line, the problem and a fix; write "no findings" if there are none.
```

A watch that runs only the judge and claude-review:

```toml
[[watch]]
owner = "your-org"
include = ["small-*"]
identity = "zhuravel"
roles = ["codex-judge", "claude-review"]   # names or aliases; exactly one judge
```
