---
name: magnum-improve
description: Runs one turn of magnum's improvement loop. It gathers evidence of what magnum did since the last run (posted reviews, findings, misses, author replies, failures, operator interventions), dispatches parallel subagents to analyze it and to review and simplify the code, fixes what is wrong, builds justified improvements in isolated worktrees, lands them through the gate and restarts the daemon. Use when the operator runs /magnum-improve or asks for an improvement pass, a review of recent magnum reviews, or "what should magnum do better".
---

# magnum improvement loop

You orchestrate. Subagents gather evidence, analyze, write code and test. You verify what they claim, decide,
review every diff and land it. One run is one turn of the loop; the run log lets the next run continue.

Read first, every run: `AGENTS.md` (binding), `docs/DECISIONS.md` (do not re-propose a rejected alternative
without new evidence), and the project memory, which holds operator decisions such as "the board stays one line per
PR" and "never a second CODEX_HOME".

## The run

1. **Preflight.** Read `~/.local/share/magnum/improve/runs.md`; the last entry gives `since` (default: 3 days ago) and
   the items it left pending. Make sure that the checkout is clean on `master`, that `magnum status` shows a running
   daemon, and that usage leaves room: Claude (`npx -y ccusage@latest blocks --active --json`) and Codex (the
   `codex:` line). Stop and say so when either window is at or above 90%.
2. **Evidence.** First run `magnum retro --lookback <days since>` and wait for `magnum logs` to show it done, so
   other reviewers' comments on recently closed PRs are classified. PRs closed less than 24 hours ago wait for the
   next run, so that late reviews can arrive. Then `scripts/evidence.sh <since> <run-dir>` writes `evidence.md`, a list of review report
   directories, and the git log since. Read the summary yourself; it is the shared input of every analyst.
3. **Analysis wave.** Start the analysts in [lenses.md](lenses.md) in parallel, each with the evidence path and its
   lens; every analyst prompt names [analyst-rules.md](analyst-rules.md) (read-only work and the return format) by
   absolute path. Lenses 0 and 1 run every time: what other reviewers found and magnum did not, and what magnum
   posted that people rejected or deferred. They are how the loop learns. Up to 8 at once. A later wave of analysts
   may run beside the builders: the session runs at most 20 subagents at once, builders' helpers included.
4. **Verify.** For every item you might act on, open the cited files, rerun the cited query or read the cited
   review. Drop an item you cannot confirm. Merge duplicates across lenses.
5. **Decide.** Sort the confirmed items with the autonomy rules below. Write the plan to `<run-dir>/plan.md`; it
   lists each running builder with the files it owns, kept current as builders start and land. Ask
   the operator once, in one message, about every item that needs approval, with your recommendation and its
   evidence; do not wait for that answer before starting the items that need none. A request the operator makes
   during the run becomes a spec of the same run.
6. **Build.** One spec per item from [spec-template.md](spec-template.md), saved in `<run-dir>/specs/`. One worktree
   agent per spec (Agent tool, `isolation: "worktree"`), at most 4 at a time (up to 6 with disjoint files when the
   operator asks for a larger push), never two on the same files. Every agent prompt names `common-rules.md` and its
   spec by absolute path. A builder starts helpers only in its own worktree, or removes their worktrees before it
   reports. A finding that arrives later (a later analyst, a builder's report) in files a running builder owns goes
   to that builder by message (SendMessage, with the evidence); any other becomes a new spec.
7. **Land.** Follow [landing.md](landing.md) for each finished agent: read the diff, land, gate, push, tell the
   running agents that master moved, restart the daemon the right way.
8. **Close.** Append the run to `runs.md` with [the run log format](landing.md#run-log). Remove finished worktrees
   with `scripts/clean-worktrees.sh`. Save a durable operator decision in the project memory. End with a short report:
   what landed, what is live, what waits for the operator, and what the next run should look at first.

## The evidence standard

New work must be justified. Every item states:

- the problem, with evidence: numbers from the registry or the events, a PR and round, a posted review, a thread
  reply, or a diff hunk;
- the user-visible effect of the change, for the PR author, the reviewer or the operator;
- the cost: effort, and the Codex or Claude usage it adds or saves;
- the risk, and how the next run measures whether it worked.

An item without evidence is not built. If it is plausible, build the measurement first (a stats view, an event)
and let a later run decide. Prefer removing, merging or simplifying over adding.

## Autonomy

Do without asking:

- a bug or regression, fixed test first;
- a simplification with no change of behavior;
- flaky tests, documentation that is wrong, and new measurements;
- an eval case for a real P0-P2 miss, added to `~/.config/magnum/eval.toml` (the corpus is local, never tracked).

A skill or prompt change made for a miss counts as visible to authors: ask, and propose to prove it with
`MAGNUM_IMPROVE_RUN=<run-dir> scripts/eval-at.sh <sha> <label> <the miss's case>...` at master before and at the
builder's commit after, in the background (it runs `magnum eval run` from a throwaway worktree at that sha and logs
to `<run-dir>/tmp/`; each replayed case costs a review round of Codex usage). Name only the cases the change
targets. A case a stored run replayed with the same skill, prompts and role config (run.json's `inputs`) is
reused, not replayed, so the "before" usually costs nothing; one at a commit with other Go code is reused with a
note that says so. The inputs leave magnum's code out: to measure a code change, pass `--fresh`, which replays
every case. It prints the report with the Codex points each replayed case used (`codex: +2 points (…)`): record them in
runs.md. A report line `notes differ from <run>` means the two runs' judges read other repository notes, so the
comparison mixes a notes change with the skill change.

Ask first, in the one batched question:

- anything the PR author sees: review text, format, frequency or noise;
- board layout (show a mockup), and a new default that costs Codex or Claude usage;
- posting identity, approvals, security trade-offs, edits to the operator's config, and removing a feature.

Never:

- post or reply on GitHub, or run `bin/magnum`, herdr, mysql, launchctl or git against real directories from an agent;
- put a private organization, repository or person into a tracked file;
- re-propose something a decision or the operator rejected, without new evidence.
