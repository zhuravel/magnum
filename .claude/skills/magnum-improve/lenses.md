# Analysis lenses

Start each lens as one read-only subagent; lenses 0 and 1 run in every run, the others as the evidence warrants. Give
it: the repository path and master's sha, `<run-dir>/evidence.md` and the review directory list, `since`, its lens
below, the report path `<run-dir>/analysis/<nn>-<lens>.md`, what the operator declined, and
[analyst-rules.md](analyst-rules.md) by absolute path: it is binding (read-only work, the limits on `gh api` and the
registry, the return format), as `common-rules.md` is for builders.

## 0. Misses: what others found and magnum did not (every run, first)

This lens is the loop's main teacher; start it in every run. Inputs: `magnum misses --all` (the retro's
classification of other reviewers' comments on closed PRs), the evidence's misses section, and, for open PRs, the
comments and reviews by people (not magnum's logins, not the author) posted after magnum's latest review on the same
head: read them with `gh api` (GET) for the PRs magnum reviewed since `since`, at most 30 PRs.

For each real miss (a problem a person raised that magnum's review on that head did not post), find out why:

- **nobody raised it**: no reviewer report and not the judge's own pass (`judge-own.md`) mention it;
- **raised and rejected**: a reviewer or the own pass raised it and the judge dropped it (`findings` with that path
  and its reason code, the ledger in `codex-judge.json`): was the reason wrong?
- **raised and under-ranked**: posted, but at a priority the person disagreed with;
- **out of reach**: outside the diff, pre-existing, or it needed something magnum cannot see (production data, a
  ticket, another repository): say which input would have shown it;
- **environment**: a check the review needed could not run (readiness, databases, tools).

Then name the smallest change that would have caught it and where it belongs: a general rule in
`skills/magnum-review/SKILL.md` or a reviewer prompt, a repository note (repository-specific lessons go to
the notes curation, not the skill), an input magnum could pass (history, related PRs, a ticket), or nothing (say why).
Never propose a rule that names the PR, the repository or a person.

Return, besides the format in analyst-rules.md, one line per miss: PR and head, the person's finding in one
sentence, priority, the why, the proposed change. Each real P0-P2 miss with a clear head and location is also
proposed as an eval case for `~/.config/magnum/eval.toml` (the format is in `eval.toml.example`): a case is how the
next run proves that a skill or prompt change catches it (`scripts/eval-at.sh`).

## 1. Declined findings: what magnum posted and people rejected or deferred (every run, second)

The other half of the teacher: precision. Inputs: the evidence's "Replies to magnum's threads" tables (every
answered magnum thread of the newest `review-threads.json` of each PR, with the start of its last reply), the full
replies (`gh api repos/{owner}/{repo}/pulls/comments/{id}` when a reply is cut), the judge's later decisions on those
threads (`previous_findings` and the rebuttals in the next round's `codex-judge.json`), the `findings` rows of the
posted finding, and people's reviews that disagree with magnum (GET with `gh api`, bounded as above).

People answer in free form. A reply may score the finding ("Net: -3", "I'd rate this -8"), weigh it against its cost,
accept it and defer the fix to a ticket, fix only part of it, argue with its premise, ask a question, or say "fixed"
while changing something else. The class in the table is magnum's keyword guess when the round ran (an older round
had an older classifier) and misses much of this: read every reply and classify it yourself as fixed, accepted but
not fixed yet, declined with a reason, deferred, argued, question, or unrelated. Report where your reading and the
keyword guess differ: that is evidence for the reply classifier and for the judge's reply contract. `posted` says when
magnum posted the finding and marks one posted under the current skill; judge the skill by those.

For each finding that was declined, deferred or argued (by your reading), decide who was right at that head:

- **magnum was wrong**: the reason holds (intended behavior, a misread, an impossible trigger, a test double that
  hid the real component). Say what led the judge astray and which rule would have stopped it.
- **right but over-ranked**: the problem is real but minor or rare; a P2 that people defer is a P3 or a nearby item.
- **right, missing context**: the repository decided it on purpose (a standing decision): it belongs in the notes so
  no later review raises it again.
- **magnum was right**: the decline is wrong; say how the finding could have been more convincing (the trigger, the
  reproduction, the impact).

Count by reason, by priority, by reviewer source and by repository, so the next run can see whether a change helped.
Propose the smallest fix for each pattern (a skill or prompt rule for precision or priority, a notes entry, a
reply-classifier pattern, or nothing). Never propose a rule that names the PR, the repository or a person.

## 2. Review quality

What magnum posted since `since`, and whether it was right (misses and declined findings are lenses 0 and 1). Sample at least 10 posted reviews:
read `review.json`, `judge-own.md`, the candidate reports and `review-threads.json` in their report directories, and
the authors' replies on GitHub. Look for false positives (declined findings with a sound reason), weak proofs,
verdict lines that misstate the findings, and noise (re-reviews that only restate open findings).

## 3. Reviewee experience

What a PR author gets and does with it. Time from push to review, threads answered or ignored, arguments
(rebuttal chains), fixes that broke something else (a new finding in code written to fix an earlier one), comments
that were hard to act on, and whether the authors' agents could apply the fixes.

## 4. Reviewer and operator experience

What the operator did by hand since `since` (the `requests` table: forced reviews, mutes, snoozes, pauses, unpins,
approvals) and why each was needed: every manual step is a candidate for a better default, a clearer board state or a
missing command. Read README's command and board sections and `internal/tui` to know what exists. Board layout stays
one line per PR; a layout change needs a mockup.

## 5. Reliability

Every warn and error event kind since `since`, stuck states (`needs_attention`, attempts above 0, rounds that
retried), restarts, infrastructure failures and the daemon log's repeated messages. For each pattern: the root
cause in the code (file and line), how often it happened, and the smallest fix.

## 6. Code health

`git log` since `since` and the diff of each commit. Review for correctness bugs, races, missing tests, dead code,
duplication, names that lie, and complexity that a simpler mechanism would remove (reuse, simplification,
efficiency, altitude). Check that AGENTS.md conventions hold (compare-and-set transitions, begin/ok/fail events,
no PR text in logs or prompts, tests named after behavior). Report each finding with the file, line, failure scenario
and fix.

## 7. Cost and speed

Agent minutes per role and round kind, rounds per PR, re-review share, Codex and Claude spend signals, waits
(quiet periods, throttles, slots). Which role earns its cost on which kind of round (`magnum stats` value tables),
and what could be skipped, shortened or reused without losing findings.

## 8. Security and trust

Inputs a PR controls (files in the checkout, PR text, branch names, configuration files the agent CLIs load) and
where they reach a prompt, a command, a log or an agent's configuration. Secrets in the slots' environment. Any path
from PR content to code execution outside the sandbox or to a write on GitHub as someone else.

## 9. New capabilities

What magnum could do that it does not, given what only it has: a persistent judge session per PR, a checkout with
databases, every past finding and miss, the whole team's PR stream, herdr panes and the operator's attention.
Read the earlier idea reports in `~/.local/share/magnum/improve/ideas/` and `runs.md` first, so nothing is proposed
twice. At most 5 ideas, each with evidence from the data.

## 10. Test-suite health

How fast and how trustworthy the gate is. Measure per-package durations with and without `-race` (`go test -count=1
-json ./...`, parsed), the 30 slowest tests and why each is slow (real sleeps, timeouts waited out, polling with a real
clock, large fixtures, a SQLite migration per test, serial tests that could call `t.Parallel`), flaky tests (the engine
and pipeline packages three times with `-count=1 -shuffle=on`), tests that touch real directories, sockets or the
network, and test helpers duplicated across packages. Propose speed-ups ranked by seconds saved, each with the file and
line and the mechanism (an injected clock, a shared migrated template database, `t.Parallel`, shorter timeouts in
tests), and say which change no behavior. It may start 2 read-only helpers.

## 11. Skill and prompt economy

What the review instructions cost, and whether they agree. `skills/magnum-review/SKILL.md` has a size cap
(`skillMaxBytes` in `internal/agents/review_format_test.go`) and the prompts carry more rules. From the Codex rollouts
of magnum's judges in `~/.codex/sessions/<recent days>/` (their working directory is a review slot or a PR worktree),
measure how often a judge reads SKILL.md (each fresh session, each turn) and the input tokens of a session's first
request. Then audit the text: rules said twice (in SKILL.md and a prompt, or twice in SKILL.md), rules that contradict
each other or the prompts, rules no evidence supports any more, wording that could be shorter without losing a rule,
and sections a role never needs. Propose a shorter structure with the bytes saved per change and list every rule that
must survive (the format tests pin many); do not write the new skill.

## 12. Docs accuracy

Compare README.md, AGENTS.md, prompts/README.md, the comments of config.defaults.toml, the cobra help in
`internal/cli` and the board's help line (`internal/tui`) with the code: every command and flag (`go run ./cmd/magnum
<cmd> --help` only prints help; never run a command that acts), every config key and its default, every board key, and
every event kind and file path the docs name. Report each statement that is wrong or missing (a key the code reads that
no doc names, a doc that names a removed flag) with the doc's line and the code's file and line, and the docs that
contradict each other. Fix nothing.
