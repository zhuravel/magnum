# Factory stacks: two PRs for a migration that must run first

Status: design, for milestone 2 of the factory (docs/superpowers/specs/2026-11-03-factory-design.md, section 14). The operator settled it in conversation on 2026-11-06.

## Intent

Today one Jira key has one task, and one task has one PR. Some changes need two PRs. The case is a migration or a data backfill that must run in production before the code that depends on it deploys. In talkable, the operator runs every migration by hand. With one PR, the dependent code can deploy before the migration ran.

The operator's rule is the fewest PRs, merges and deploys. Agents tend to split work into many PRs without a need. For example, an agent once split one ticket into two stacked PRs, #4521 and #4522: the first read some records, and the second wrote rows for them. One PR was enough for that. So a stack is rare, and it has one named reason.

## Goals

- One task can open two PRs, stacked: the upper PR targets the lower PR's branch.
- The only reason for a split is a migration or backfill that must run in production before dependent code deploys.
- The operator decides every split before any code is written.
- After the lower PR merges, the upper PR is brought up to date. It counts as ready to merge only after the operator says that the migration ran in production.

## Non-goals and anti-goals

- No split for size, neatness, layers or "a read part and a write part". Even when it works, a split for such a reason is a failure.
- No stack of more than two PRs.
- No stack across repositories. Cross-repository groups are a later design.
- The factory never connects to a production database and never runs a migration. It never merges a PR.

## How it works

1. **The question.** During investigation, the planning stage finds that the change holds a migration or backfill that dependent code needs in production first. The question batch then asks: "Split PR-12345 into 2 PRs: the backfill must run in production before the writer deploys? recommended: yes". The operator answers. A no keeps one PR. No other reason produces this question.
2. **The code.** With a yes, the writer codes the whole change in one branch. The commits of the lower part come first: the migration, and the code that works with both the old and the new state. A marker records the split commit. The upper part's commits follow.
3. **The checks.** The proof, the precheck and the local review run on the head as today. The precheck and the specs also run at the split commit, so that the lower PR is green by itself.
4. **The ready card.** One card shows the whole change and names the split: "2 PRs: the backfill must run in production first", with the split commit and each part's files. One `y` publishes both.
5. **The push.** The factory pushes the lower branch at the split commit and the upper branch at the head. It opens the lower PR against the base and the upper PR against the lower branch. Each body links the other and says the order: merge the lower PR, run the migration in production, then merge the upper PR.
6. **After the lower PR merges.** GitHub retargets the upper PR to the base. The factory merges the base into the upper branch, runs the checks again and pushes.
7. **The migration word.** The task then waits in NEEDS YOU: "migration of PR-12345 ran in production? y/N". Only after the operator's `y` does the upper PR count as ready to merge. Only then do its staging merges and the "ready to merge" alert come.

Stacks made elsewhere, by another agent or a colleague, come in through milestone 2's adoption. The factory keeps the upper PR up to date as in step 6. If the lower PR holds a migration, the factory asks step 7's question too.

## Approaches considered

- **A. One branch, split at the push.** The operator chose this. Coding, proof, local review and the ready card stay one diff. The push makes two branches. It adds the least machinery: the split marker, the checks at the split commit, a two-PR push and the wait for the migration word.
- **B. Two parts built in order inside one task.** Each part gets its own coding and local review, and the card shows two diffs. It is cleaner per part, but it doubles the coding and review stages.
- **C. Two linked tasks.** Task 2 takes task 1's branch as its base. It reuses all single-PR machinery. But it brings two copies, two cards and two question batches: the operator time that the rule wants to save.

## Constraints

- The milestone 1a rules hold. The operator's login authors the PRs until the factory App does (milestone 2). Every push is bound to the card the operator read. The publish never forces.
- The migration rules of section 8.4 hold: every migration is manual, and gh-ost migrations are never run by the factory.

## Left to the builder

- How the writer records the split commit: a trailer in its commit message, or a file beside the report.
- The branch names of the two parts, in the factory's `<login>-<KEY>-<slug>` form.
- The retry and the repair for a lower push that succeeds and an upper push that fails. Both pushes follow the publish's resume rules.
- Where the split question sits in the question batch, and its words beyond the example above.
