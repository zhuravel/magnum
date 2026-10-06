# Prompts and the review pipeline

magnum's review pipeline is configuration, a "Procfile of agents". Three parts of `config.toml` define it:

- `[kinds.<name>]` says how to drive one agent CLI: codex, claude, droid, omp, or any other herdr agent kind.
- `[[role]]` blocks list the agents (or shell commands) of a review round, with their prompts.
- `roles` in a `[[watch]]` picks the roles that watch runs. It defaults to every role.

This directory holds the prompt templates the roles name. With nothing configured, magnum runs the four
built-in roles below, exactly as before roles became configurable.

## How a prompt name resolves

A role names prompts by file name, such as `prompt = "claude-review.md"`. magnum reads
`<prompts_dir>/<name>` when that file exists, else the copy of the same name built into the binary.
`prompts_dir` is set under `[pipeline]` and defaults to this directory (`{{repo}}/prompts`). A file
you delete falls back to the built-in copy. `magnum config` fails when a role names a prompt that exists
in neither place.

The daemon loads every prompt its roles name, `model-fallback.md`, the triage, retro and notes curation prompts and a copy
of each judge's skill (`state/skill/<hash>/SKILL.md`) once, when it starts, right after checking that its build
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
| `judge-initial.md` | judge, first review | judge |
| `judge-rereview.md` | judge, new head after its last review | judge |
| `judge-continue.md` | judge, after a pause (usage limit) ended mid-turn | judge |
| `judge-recovery.md` | judge, fresh session after the old one was lost | judge |
| `judge-nudge.md` | judge, stopped without a result | judge |
| `model-fallback.md` | any session role, the judge included, after magnum switched its model because the model hit its own limit | fallback |
| `claude-review.md` | claude-review, first review | role |
| `claude-rereview.md` | claude-review, new head | role |
| `claude-restart.md` | claude-review, a push cut its review short and the round restarted on the new head | role |
| `claude-simplify.md` | claude-simplify | role |
| `codex-review.sh` | the full codex-review command line, for reference (see Shell roles) | shell |
| `triage.md` | the cheap model that decides which reviewers a small diff needs (see Triage) | triage |
| `retro.md` | the retro's agent, which classifies what other reviewers said about a closed pull request ([learn]) | retro |
| `notes-curate.md` | the notes curator, which proposes curated repository notes and harness ([notes]) | curation |

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
`retro.md` the retro data (`internal/engine/retro.go`) and `notes-curate.md` the curation data
(`internal/engine/notes_curate.go`).

## Variables

### Judge prompts (`judge-*.md`)

| Variable | Meaning |
|---|---|
| `.RunID` | the round's run id; reviews carry the marker `magnum:run=<RunID>` |
| `.Owner`, `.Repo`, `.Number`, `.URL` | the pull request |
| `.HeadSHA` | the commit under review (checked out, detached) |
| `.BaseRef`, `.BaseSHA` | the base ref the diff is taken against and the merge base |
| `.Checkout` | absolute path of the checkout the judge works in |
| `.IdentityKind` | `gh` or `app` |
| `.ReviewerLogin` | the login the review is posted as (REST form, e.g. `talkable[bot]`) |
| `.GhConfigDir` | `GH_CONFIG_DIR` of the identity; empty for the `gh` identity |
| `.NoFindingsEvent`, `.BlockingEvent` | review events from the `[[repo]]` or the `[[identity]]`; `.NoFindingsEvent` is `COMMENT` whenever a reviewer of the round left no usable report (anything in `.Reports` that is missing): a review that did not hear every reviewer never approves |
| `.SelfAuthored` | the PR's author is the reviewing identity |
| `.Reports` | one entry per other role of the round, in pipeline order (see below) |
| `.ResultFile` | where the judge writes its JSON result (the role's `output`) |
| `.Magnum`, `.ReviewFile`, `.PostReviewCommand` | the magnum binary the daemon runs (absolute; empty: `magnum` on `PATH`), the file the judge writes its review to (`review.json` next to `.ResultFile`, derived when empty) and the shell line that posts it, `magnum post-review` with the run's facts as flags (repository, PR, head, run id, `.ReviewerLogin`, each of `.FormerLogins`, `.GhConfigDir` when set, `--dry-run` under `.DryRun`, `--local-base .BaseSHA` in a blind replay, `--review .ReviewFile`; every value shell-quoted; always derived). The judge prompts render it as the `<magnum>` field `post_review`; what the command does is in the README (The judge skill) |
| `.DryRun` | plan the review without posting it |
| `.PostMerge` | GitHub merged the PR before magnum reviewed `.HeadSHA` (`magnum review` of a merged PR): post a COMMENT asking for follow-ups; `.NoFindingsEvent` and `.BlockingEvent` are both `COMMENT`. The judge prompts render `post_merge: true` only then |
| `.SkillPath` | the role's `skill`, absolute; in the daemon, the copy it took at startup |
| `.Model`, `.Effort` | the judge role's `model` (else its kind's `default_model`), and its effort for this round: `rereview_effort` in a re-review or a delta check (when set), else `effort` |
| `.EffortInPrompt` | the kind sets the effort only at launch (its `effort` args, as codex) and `.Effort` is not the role's `effort`: a running session cannot switch, so the prompt asks for `.Effort` in words |
| `.PreviousReviewID`, `.PreviousEvent`, `.PreviousHeadSHA` | the last review (rereview, recovery) |
| `.PreviousHeadShort` | `.PreviousHeadSHA` cut to 7 characters (always derived) |
| `.Since` | RFC 3339 time; read every comment since then (rereview, recovery) |
| `.ForcePushed` | `.PreviousHeadSHA` is no longer in the branch (rereview) |
| `.BaseMerged` | the commits since `.PreviousHeadSHA` have a merge commit (the base branch merged in), so `.PreviousHeadSHA..HEAD` carries the base branch's commits: the prompt compares the PR's own diff before and after (`git diff <base>...<previous>` with `git diff <base>...<head>`). `.ForcePushed` wins over it; `judge-rereview.md` renders `base_merged: true` only when set (rereview) |
| `.DeltaCheck`, `.DeltaLines`, `.DeltaFile` | the round is a delta check: the judge alone, in its own session, on a small delta since its last review (`[daemon] delta_check`): `.DeltaLines` changed code lines in the files `.DeltaFile` lists (`delta-check.json` in the report directory, each file's path, status and whether it is a modified binary file; the file names are PR content, so the prompt names the file and never prints them; empty when it could not be written). `judge-rereview.md` renders `delta_check: true` and one instruction (review just those commits against the PR's purpose and the earlier findings, post one short review) only when set (rereview); `judge-recovery.md` does the same for a judge whose session is gone (a lost session, an identity migration): it names the last review by `.PreviousHeadShort` and has the fresh session read that review and its threads first, at the judge's `rereview_effort` (recovery) |
| `.MovedFrom` | the previous checkout when the PR changed slots (rereview) |
| `.PreviousReviews` | earlier reviews by the login: `.ID`, `.Event`, `.SHA`, `.SubmittedAt` (recovery) |
| `.ThreadsFile`, `.ThreadSummary` | the JSON file of the inline threads the login (or a former login) started, every reply classified (`fixed`, `not a bug`, `won't fix`, `other`), and their counts, e.g. `3 threads (1 resolved); replies: 1 fixed, 1 not a bug; 1 thread without a reply` (rereview, and recovery of a reviewed PR; empty when magnum could not read them). `.Threads` holds the same data, but replies are PR content: name the file, never print them |
| `.FormerLogins` | the logins (REST form) the PR's earlier reviews were posted as before its watch moved to another identity: their reviews and threads are the judge's own history; usually empty |
| `.NotesPath` | the repository notes file, `<home>/state/notes/<owner>/<repo>.md` (lower-case); empty when there is none. The judge reads it and rewrites it when a round taught something durable (see Repository notes). The judge prompts pass it and the four rows below as the `<magnum>` fields `notes`, `notes_dir`, `notes_harness`, `notes_lock` and `notes_unlock`, only when it is set; the steps are the skill's |
| `.NotesDir`, `.NotesLock` | the harness directory next to the notes file (its path without `.md`) and the notes lock (that with `.lock`); empty without notes |
| `.NotesHarness`, `.NotesHarnessMore` | the harness directory's entries at prompt time (sorted, a directory ends in `/`, at most 40) and how many more there are |
| `.NotesLockCommand`, `.NotesUnlockCommand` | the shell lines that take the notes lock (printing `notes locked`, or `notes busy` after three minutes) and release it |
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
reviewer, ...), initial, rereview and restart alike.

| Variable | Meaning |
|---|---|
| `.URL` | the pull request |
| `.Owner`, `.Repo`, `.Number` | the pull request's owner, repository name (e.g. `talkable`) and number |
| `.HeadSHA` | the commit under review (checked out, detached) |
| `.PreviousHeadSHA` | the head of the role's previous run (rereview only, empty otherwise) |
| `.BaseSHA` | the merge base; `git diff {{.BaseSHA}}..HEAD` is the PR's diff |
| `.BaseRef` | the base ref, e.g. `origin/master` or the parent branch of a stacked PR |
| `.ReportPath` | where the role writes its report (its `output`, absolute) |
| `.Model`, `.Effort` | the role's `model` (else its kind's `default_model`), and its effort for this round: `rereview_effort` in a re-review (when set), else `effort` (claude-review passes `.Effort` to `/code-review`) |
| `.EffortInPrompt` | as for the judge: the prompt must ask for `.Effort` in words |
| `.Mode` | `initial`, `rereview` for a new head after the role's earlier run, or `restart` (see below) |
| `.Since` | RFC 3339 time of the role's previous run (rereview only, empty otherwise) |
| `.ForcePushed` | `.PreviousHeadSHA` is no longer in the branch (rereview) |
| `.BaseMerged` | the commits since `.PreviousHeadSHA` merged the base branch in: as for the judge, the prompt compares the PR's own diff before and after them (rereview and its restart) |
| `.RestartedFrom` | the head the role was reviewing when a push cut its turn short (restart only, empty otherwise) |
| `.NotesPath` | the repository notes file (see Repository notes); the role reads it first. Empty when there is none |
| `.PostMerge` | GitHub merged the PR before magnum reviewed `.HeadSHA` (a post-merge review); the prompts that name the PR say "The PR is already merged; review it anyway." only then |

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
| `.Candidates` | the candidates file (`candidates.json`): per comment its `id` (`t<comment id>` or `r<review id>`), `reviewer`, `path`, `start_line` and `line`, `side`, `reviewed_sha`, `diff_hunk`, `body`, `raised` and `reason_code`, `file` or `file_skipped` |
| `.Files` | the directory of the commented files, `<Files>/<first 12 characters of reviewed_sha>/<path>` |
| `.Output` | the answer file the agent writes, `retro.json` |
| `.Count` | how many candidates there are |

The answer is `{"items": [{"id", "class", ...}]}` with one item per candidate and `class` one of `miss`,
`not_issue`, `style` and `outside`; a miss also needs `severity` (`P0` to `P3`), `title` (at most 80
characters), `lesson`, `scope` (`repo` or `general`), `lines` (`[from, to]`, 1 ≤ from ≤ to) and `match`
(one to three case-insensitive Go regular expressions of at most 120 characters). Magnum checks it
(`internal/learn`); a missing or invalid file gets one nudge, then the pull request fails. A lesson that names a
pull request or issue (`#123`), holds a URL, names the author or a reviewer, mentions one of Magnum's logins
or, when its scope is `general`, names the repository is dropped and the miss kept without it.

### Notes curation prompt (`notes-curate.md`)

The one prompt of a notes curation (`[notes]`, see the README's "Repository notes"): propose curated notes
and harness for one repository in a scratch copy. Notes text is never a template variable; the agent reads
the copies.

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
size is not checked. A proposal that changes nothing is invalid unless it was given misses and skips them
all. An invalid proposal gets one nudge naming its problems, then is kept as invalid.

### Repository notes

magnum keeps one Markdown file per repository, `~/.local/share/magnum/notes/<owner>/<repo>.md` with owner
and repository lower-cased, and a directory with the same name without `.md` next to it for QA scripts.
The built-in reviewer prompts tell their role to read the file first as hints from earlier reviews to
verify. The judge prompts pass the file, the harness directory, its files (so a script the notes no longer
mention is visible) and the lock commands as `<magnum>` fields (`notes`, `notes_dir`, `notes_harness`,
`notes_lock`, `notes_unlock`), and the skill (section 2) holds the steps once: rewrite the file (never
append) after posting when the round taught something durable that helps review a future pull request
(what the repository is, how to test, lint and QA a change, failures of the review machine, known
pitfalls, standing decisions, under a dated header line), never one pull request's findings, code or
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
| `.RunID` | the run id, i.e. `.Marker` without its `MAGNUM_DONE_` prefix |
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
| `wrapper` | `auto` probes `zsh -ic 'whence -w <kind>'`: a function or alias supplies its own flags. `true` or `false` skip the probe. |
| `start` | args always appended |
| `args` | args appended only without a wrapper (the old `[codex]`/`[claude]` `args`). codex defaults to `["--dangerously-bypass-approvals-and-sandbox"]` and claude to `["--dangerously-skip-permissions"]`, so a plain binary runs like the usual zsh wrappers, without approval prompts (magnum answers every prompt No) and, for codex, without the sandbox (a sandboxed judge cannot reach GitHub). `magnum doctor` fails a plain codex or claude binary whose session roles start without that flag |
| `resume` | args that resume `{session}` |
| `model`, `effort` | args that pass a role's `{model}` or `{effort}`; empty means the CLI cannot take it |
| `default_model` | the model of the kind's roles that set no `model`, passed through `model` args (e.g. `default_model = "gpt-6.1-sol"` under `[kinds.codex]` starts the codex judge with `--model gpt-6.1-sol`); `""` = the CLI's own default. A shell role (codex-review) takes its model through its own `args` |
| `name` | args that name the session `{title}` at launch |
| `rename` | a slash command typed while the agent works, e.g. `/rename {title}`; `""` = none |
| `login_check`, `login_ok` | a read-only command (split on spaces) and how its output reads as logged in: `text:<substring>`, `regex:<expr>`, `json:<dotted.path>` or `""` for exit status 0 |
| `env` | extra pane environment (a role's `env` wins) |
| `health_patterns` | `login_required`, `model_limit`, `usage_limit` and `overloaded` regexp lists, case-insensitive; a list replaces the built-in one. `model_limit` (one model capped, checked before `usage_limit`) may capture the model in a `(?P<model>...)` group; without one the session's current model is the limited one |
| `switch_model` | a slash command typed into the idle agent's pane to switch its session to `{model}`, e.g. `/model {model}`; `""` = the kind cannot switch in-session, so a model limit pauses the kind like a usage limit |
| `fallback_models` | the models a session switches to, in order, when its model hits its own limit; unused without `switch_model` |
| `reset_model` | the `switch_model` argument that returns a session to the CLI's own default once the limit is over, when neither the role's `model` nor the kind's `default_model` is set (claude: `default`); `""` = it stays on the fallback |
| `session_source` | `herdr` (herdr reports the session id, so sessions resume) or `none` (always fresh) |
| `on_permission_prompt` | an approval prompt that blocks the agent during a magnum run: `deny` (default) answers its No option, never Yes, at most 10 times per run, each recorded as an `agent.prompt_denied` event; `wait` leaves it to you. The first-launch folder-trust dialog is handled separately. |
| `on_hooks_review` | Codex's startup "Hooks need review" (hooks new or changed since Codex last trusted them): `trust_own` (default) picks "Trust all and continue" when the checkout declares no hooks of its own (no `.codex/hooks.json`, no `hooks` or `plugins` in its `.codex/config.toml`), so every hook listed is yours (Codex home or an installed plugin), recorded as `agents.hooks_trusted`; otherwise, or with `decline`, "Continue without trusting" for that session, recorded as `agents.hooks_declined`. |
| `after_deny_prompt` | the message sent once, in the same run, when an agent stops its turn after a deny (Claude Code does), so it finishes without the command; it counts toward the 10 per run and is recorded as `agent.deny_continued`. Default: "magnum denied that command: review roles never run approval-gated or destructive commands. Continue the task without it and finish as instructed."; `""` sends nothing. |

The launch args are built in this order: `resume`, `name`, `model`, `effort`, `start`, `args`, then
the role's `args`. `magnum roles --kinds` prints the effective kinds, merged with your overrides, so you
can see exactly what magnum will type.

| Kind | resume | model | effort | name / rename | login check |
|---|---|---|---|---|---|
| codex | `resume {session}` | `--model {model}` | `-c model_reasoning_effort={effort}` | `/rename {title}` | `codex login status`, `text:Logged in` |
| claude | `--resume {session}` | `--model {model}` | (prompt only) | `--name {title}` | `claude auth status`, `json:loggedIn` |
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
| `summary` | none | one line saying what the role checks, shown to the `[triage]` model. Only a role with a summary can be left out of a round by triage; the judge never can. The built-in reviewers have one |
| `runs` | `always` | `always`, `first` (until it completed once for the PR, then on request), `manual` (on request only) or `never` (disabled; requests are refused). Request a role with `magnum review <ref> --role <name>` |
| `identity` | the watch's | the `[[identity]]` whose GitHub environment the pane gets |
| `model`, `effort` | none | passed through the kind's `model`/`effort` args; also template variables |
| `rereview_effort` | `effort` | the effort of re-review rounds (new commits after an earlier review); the judge's built-in default is `high`. A re-review starts or resumes an agent at it; a live session of a kind that sets effort only at launch is asked for it in the prompt (`.EffortInPrompt`) |
| `args`, `env` | none | extra launch args (shell roles: appended to the command, quoted) and pane environment |
| `prompt`, `rereview` | see below | prompt files for the first review and for a new head |
| `restart` | see below | session reviewers: the prompt after a push cut the role's turn short and the round restarted on the new head |
| `continue_prompt`, `recovery`, `nudge` | judge only | the judge's other prompts. A `stop` key is accepted and ignored: magnum never sent a stop prompt |
| `skill` | `{{repo}}/skills/magnum-review/SKILL.md` | the judge's skill, `{{.SkillPath}}` |
| `command`, `tool` | none | shell roles: the command template, and the kind whose login check, pauses and health patterns apply |
| `ok_status` | `[0]` | shell roles: the exit statuses that count as a finished report; any other status fails the role (its output is then checked for login, usage-limit and overload errors) |
| `output` | `<name>.md` | the report file in the round's directory; judges `<name>.json` |
| `capture` | `file` | `file` (the role writes `output`) or `stdout` (shell roles; output is tee'd). No role may edit the checkout: one left modified after its stage is reset to the PR head, with a `round.checkout_dirty` warning |
| `timeout` | `40m` | per turn; judges `90m` (`daemon.reviewer_timeout` / `judge_timeout` are the fallbacks) |
| `after` | none | roles that must finish first; roles outside the watch's set are ignored |
| `aliases` | none | other accepted names (the built-in roles keep `judge`, `claude`, `codex`, `codex_review`, `simplify`) |

Prompt defaults: a judge uses the six `judge-*.md` files. Another session role uses `<name>.md`,
`<name>-rereview.md` when that file exists (else `<name>.md` again) and `<name>-restart.md` when that
file exists (else no restart prompt). claude-review's are `claude-review.md`, `claude-rereview.md` and
`claude-restart.md`.

A round runs the watch's non-judge roles in parallel, layered by `after`, then the judge. The judge gets
every other role's report path. Declaring any `[[role]]` in the base config (`config.defaults.toml`, or a
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
| `command` | `["claude", "-p", "--model", "haiku", "--tools", "", "--no-session-persistence"]` | the model's CLI as an argument list; the prompt arrives on stdin and stdout is the answer. It runs in a private directory under `state/`, never in the PR's checkout. A Codex one: `["codex", "exec", "--model", "<a cheap model>", "--sandbox", "read-only", "--ephemeral", "--skip-git-repo-check", "-"]` |
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
`runs`, model, effort, capture, output, the initial prompt file (and whether it comes from `prompts_dir`
or the embedded copy), `after`, the judge and the aliases. `--kinds` prints the effective agent kinds
instead, i.e. what magnum will type to start, resume and name each one. `--json` prints either as JSON.

### The built-in roles

| Role | Kind | What it does |
|---|---|---|
| `codex-judge` | codex | Persistent session; reads the reports, runs `$magnum-review` and posts one review. Effort `xhigh`, `high` for re-reviews, 90 minutes. |
| `claude-review` | claude | Persistent session running `/code-review <url> high` (`medium` for re-reviews), leaving out style-only and pre-existing problems and naming each finding's trigger; it also checks every caller of changed behaviour (non-production ones too), what a replaced mechanism did implicitly and any test failure or flake the change brings, and lists the candidates it rejected with the reason; writes `claude-review.md`. |
| `codex-review` | shell | Types `command codex review --base <merge base>` (the base ref when the merge base is unknown) into a plain pane; its output is tee'd into `codex-review.md`. `codex review` takes custom instructions only as a review target of their own, in place of `--base`, so it gets none of claude-review's extra checks. |
| `claude-simplify` | claude | A read-only `/simplify`, alongside the reviewers on a PR's first review, again after `rerun_min_lines` changed lines, or on request (`magnum review --role claude-simplify`, or `--simplify`): four subagents review the diff in parallel for reuse, simplification, efficiency and altitude (one pass without the Agent tool), and instead of editing it writes every qualifying proposal, ranked, removals rather than renames or moves, with exact current and replacement lines, to `claude-simplify.md`. A re-review proposes only on lines changed since the previous review. |

### Shell roles

A shell role's `command` is a template over the shell variables. magnum types it as one line, like this:

```text
printf '\033]0;%s\007' <title>; DISABLE_AUTO_TITLE=true; set -o pipefail; <command> <args...> | tee <output>; printf '\nMAGNUM_DONE_<run> %d\n' "$?"
```

The `| tee` part is only there with `capture = "stdout"`. `pipefail` makes the printed status the
command's rather than `tee`'s; a status outside the role's `ok_status` fails the role even when it wrote
output. To control the whole line, set `prompt` to a `.sh` template instead of `command`; it must print
the done marker itself, on a line of its own and followed by the status (a bare marker counts as
success). `codex-review.sh` shows the line magnum types for codex-review.

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
