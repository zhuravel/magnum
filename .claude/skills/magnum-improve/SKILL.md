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
2. **Evidence.** `scripts/evidence.sh <since> <run-dir>` writes `evidence.md`, a list of review report directories,
   and the git log since. Read the summary yourself; it is the shared input of every analyst.
3. **Analysis wave.** Start the analysts in [lenses.md](lenses.md) in parallel, read-only, each with the evidence
   path, its lens and the return format there. Up to 8 at once.
4. **Verify.** For every item you might act on, open the cited files, rerun the cited query or read the cited
   review. Drop an item you cannot confirm. Merge duplicates across lenses.
5. **Decide.** Sort the confirmed items with the autonomy rules below. Write the plan to `<run-dir>/plan.md`. Ask
   the operator once, in one message, about every item that needs approval, with your recommendation and its
   evidence; do not wait for that answer before starting the items that need none.
6. **Build.** One spec per item from [spec-template.md](spec-template.md), saved in `<run-dir>/specs/`. One worktree
   agent per spec (Agent tool, `isolation: "worktree"`), at most 4 at a time, never two on the same files.
   Every agent prompt names `common-rules.md` and its spec by absolute path.
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
- flaky tests, documentation that is wrong, and new measurements.

Ask first, in the one batched question:

- anything the PR author sees: review text, format, frequency or noise;
- board layout (show a mockup), and a new default that costs Codex or Claude usage;
- posting identity, approvals, security trade-offs, edits to the operator's config, and removing a feature.

Never:

- post or reply on GitHub, or run `bin/magnum`, herdr, mysql, launchctl or git against real directories from an agent;
- put a private organization, repository or person into a tracked file;
- re-propose something a decision or the operator rejected, without new evidence.
