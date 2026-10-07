# Analysis lenses

Start each lens as one read-only subagent; lenses 0 and 1 run in every run, the others as the evidence warrants. Give it: the repository path, `<run-dir>/evidence.md` and the review
directory list, `since`, its lens below, and the return format at the end. Analysts never edit files, never run
`bin/magnum`, herdr, mysql or launchctl, and use `gh api` only for GET requests (the operator's token also serves the
daemon: keep it to tens of calls). The registry is read with `sqlite3 -readonly`; read `.schema` first.

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

Return, besides the format below, one line per miss: PR and head, the person's finding in one sentence, priority,
the why, the proposed change. Each real P0-P2 miss with a clear head and location is also proposed as an eval case
for `~/.config/magnum/eval.toml` (the format is in `eval.toml.example`): a case is how the next run proves that a
skill or prompt change catches it.

## 1. Declined findings: what magnum posted and people rejected or deferred (every run, second)

The other half of the teacher: precision. Inputs: the evidence's "Author replies to magnum's threads" tables (the
newest `review-threads.json` of each PR: every magnum thread with its replies and their class), the judge's later
decisions on those threads (`previous_findings` and the rebuttals in the next round's `codex-judge.json`), the
`findings` rows of the posted finding, and people's reviews that disagree with magnum (GET with `gh api`, bounded as
above). Classes are the author's claim, and `other` hides many real answers: read the replies.

For each finding that was declined (`not a bug`), deferred (`won't fix`, a follow-up, "later", "out of scope") or
argued, decide who was right at that head:

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

## Return format (every lens)

At most 10 items, ranked by impact over effort, under 1,200 words. For each item:

- a one-line title;
- the evidence (numbers, PR and round, file and line, quoted review line);
- the change, as the author, reviewer or operator would see it;
- class: `bug`, `simplify`, `improve` or `idea`;
- effort (S/M/L and the packages), usage impact, risk;
- how the next run measures it.

Then one line per thing you checked and found fine, so the next run does not check it again.
