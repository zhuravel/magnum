# Design: hold a resolved merge that keeps the PR's own workflow changes, and push it after the operator's y

Date: 2026-11-12. Status: approved by the operator; the engine's owner session agreed, with three notes that this design holds (the
ref is under `refs/magnum/held/`, an empty remerge diff of the workflow files shows one line and then the plain diff, the y/N names the
workflow files, the bot and the Workflows token).

## Why

The operator pressed `P` (merge the parent) on one of their own PRs. The merge conflicted in 5 files. Two of them were workflow files
(`.github/workflows/ci.yml`, `pronto.yml`) that both the base and the PR changed. This is what happened:

1. claude-fix resolved the conflicts in the PR's slot and ran `magnum push`.
2. `magnum push` handed the merge to the daemon with exit 4 (`pushDaemon`, internal/cli/push.go), because the merge carries the base's
   workflow files.
3. The settle called `pushedHead`, then `pushHanded`, then `pushMerge` (internal/engine/fix_settle.go, internal/engine/fix_parent.go).
4. `pushMerge` refused: "the merge of master at … keeps the PR's own workflow changes (…), which … never pushes".
5. The settle's `fixHandRefused` case ended the job. The PR went to Needs you with "merge master into <branch> by hand and resolve
   <paths>". Nothing kept the resolved merge.

The fixer App has the Workflows permission. The rule refuses the push because an agent wrote workflow content, and nobody read it. The
operator wants to read it and say yes, from the screen.

## 1. Hold the merge instead of dropping it

`pushMerge` refuses a handed merge for one reason that a person can lift: it keeps the PR's own workflow changes, and the merged commit is
the tip that GitHub names. That refusal is typed now (`pushRefusal.own`, the workflow files that differ from the tip, and `files`, the
workflow files the push changes), not a string match. In that case alone, and only for a PR that `e.ownPR` accepts (read again from the
registry where the merge is held), the daemon holds the merge:

- A ref keeps the commit from git gc: `refs/magnum/held/<owner>/<repo>/<number>` in the clone that the slot's objects live in
  (`cmp.Or(job.slot.MainClone, job.slot.Path)`), never under refs/heads or refs/remotes.
- A record (`engine.HeldMerge`, the registry's key `pr.<id>.held_merge`, JSON) keeps its SHA, the PR head that it merges into, the base,
  the tip that it merges, the workflow files the push changes, how many conflicts the fixer resolved, the role, the identity and its
  login, the clone, the diff file and the time. The key-value table is an existing persisted structure: no migration.
- `held-merge.diff`, mode 0600, in the PR's report directory (`<reviews>/<owner>/<repo>/<number>/`), holds two parts:
  - how the fixer resolved the conflicts in the workflow files: `git show --remerge-diff --format= <merge> -- .github/workflows`;
    when that is empty, one line instead: "No conflict in a workflow file was resolved: the workflow changes come unchanged from <base>
    and the PR.";
  - then everything the push changes in the workflow files, compared with the PR's head: `git diff <head> <merge> -- .github/workflows`.
- The run ends as needs you. The attention text says that the merge is ready and waits for the operator's y, and names the workflow
  files, the key that shows the diff, and that `P` asks: "merge ready: claude-fix merged master at c0c0c0c and resolved 5 conflicts.
  It changes .github/workflows/ci.yml and pronto.yml. fixer-app[bot] pushes it only after your y: P asks (e shows held-merge.diff)."
  The role's and the identity's names come from the job.
- A fix run's fixer merges the base before the findings and can hand over such a merge too. It is held the same way, and its attention
  adds "The findings wait for the next fix after the push."
- Every other refusal of pushMerge stays as it is: an App without Workflows, a tip that is not on the base as GitHub has it, workflow
  files that differ from a tip that moved, and a refused branch. So does a PR that is not the operator's own.

## 2. Drop a held merge that went stale

The ref, the record and the diff file go when:

- the push of the held merge succeeds;
- the PR's head moves on GitHub (someone pushed);
- the PR's base moves (a retarget) or its base tip moves on GitHub;
- the PR merges or closes.

The poll checks it where it already reads the PR's head and base: a PR with a held merge has its Details read on every poll (as a PR
whose CI run is awaited does), so its base tip is seen. The push checks it again. A drop writes a `fix.held_dropped` event; a PR that
still needs you for the held merge gets words that say it was dropped and why, and that `P` merges the parent again. A stale held merge
never comes back as "merge ready".

## 3. `P` and `magnum merge-parent` push the held merge after a bound y

The four entry points (`P` on the board, the status dashboard and the factory screen's PR rows, and `magnum merge-parent <pr>`) already
ask the daemon for a dry run and then a y/N bound to the plan (`MergeAsked`, internal/engine/fix_parent_bind.go). They are reused.

- With a held merge, the dry run's plan is its push. Its words keep the first words of a dry run's answer, since the CLI's question and
  its check for an older daemon read them: "dry run: would merge the parent of talkable/talkable#2: magnum pushes claude-fix's merge of
  master at c0c0c0c into feature as fixer-app[bot] with the Workflows token; it changes .github/workflows/ci.yml and pronto.yml
  (held-merge.diff)". The question is "Merge the parent of talkable/talkable#2: magnum pushes …?".
- `MergeAsked` gains `Merge string json:"merge,omitempty"`. The y binds to it, to the PR head and to the base tip, with the existing
  10-minute rule (`mergeAskedWait`).
- On y the daemon checks again: the PR head on GitHub is the held head, the held ref is the SHA that the question named, its first
  parent is that head and its second the held tip, which is on the base as GitHub has it (`baseOnGitHub`) and is still its tip. Then it
  pushes with the App's Workflows token (`DaemonPushEnv(ctx, true)`) and a lease on the head, as `pushMerge` does. It skips only the
  check that the workflow files equal the tip's: the operator read them and said yes.
- After the push the PR row's head moves (`parentOnRow`), `ci_dispatch` starts its CI two minutes later (`notePush`), and the events say
  so: `fix.parent_pushed` (data `held: true`) and the merge's `fix.ended`, which `--wait` reads.
- `--yes` never pushes a held merge, because it binds nothing: it is refused with words that say the held merge needs the operator's y on
  a terminal. A request without a bound plan that names the merge is refused too, and so is one that names a held merge magnum no longer
  holds. An agent's shell can never push it.
- An n leaves the merge held.

## 4. The screens

- `e` opens held-merge.diff in the pager, asking nothing, on a PR row with a held merge: the board's and the dashboard's (which had no
  `e`), and the factory screen's, where `e` opens the fixer's report otherwise. While a merge is held, the factory screen's `e` opens
  held-merge.diff first: the merge is what the PR waits for, and the fixer's report of a merge run says no more than the attention does.
  The hints offer it on such a row.
- `P` on that row asks the held push's question.
- The board's state cell says "merge ready" in the attention's cause, on one line.

## Tests first

- A handed merge that keeps the PR's own workflow changes is held: the ref and the record exist, held-merge.diff holds both parts, the
  attention text names the files and `P`, and nothing is pushed. A fix run's adds that the findings wait.
- A held merge whose workflow files merged without a conflict writes the one line, then the plain workflow diff.
- `P` (the dry run, then a bound y) pushes the held merge with the Workflows token as the bot, deletes the ref, the record and the file,
  and writes the push event.
- The held push is refused, and nothing is pushed, when the PR's head moved, the held merge changed, the tip is not on the base as GitHub
  has it, or the question is 10 minutes old.
- `--yes`, and a request without a bound plan, never push a held merge.
- A moved head, a moved base, a moved base tip, and a merged or closed PR drop the held merge.
- The other refusals of pushMerge still end in Needs you as today.
- A PR that is not the operator's own is never held.
- The screens' `e` opens held-merge.diff in the pager, and `P` asks the held push's question that names the files, the bot and the token.
