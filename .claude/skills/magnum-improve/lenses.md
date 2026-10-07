# Analysis lenses

Start each lens as one read-only subagent. Give it: the repository path, `<run-dir>/evidence.md` and the review
directory list, `since`, its lens below, and the return format at the end. Analysts never edit files, never run
`bin/magnum`, herdr, mysql or launchctl, and use `gh api` only for GET requests (the operator's token also serves the
daemon: keep it to tens of calls). The registry is read with `sqlite3 -readonly`; read `.schema` first.

## 1. Review quality

What magnum posted since `since`, and whether it was right. Sample at least 10 posted reviews: read `review.json`,
`judge-own.md`, the candidate reports and `review-threads.json` in their report directories, and the authors'
replies on GitHub. Look for false positives (declined findings with a sound reason), misses (`misses` table, human
reviewers' comments after magnum's review), findings the judge rejected that a human later raised, weak proofs,
verdict lines that misstate the findings, and noise (re-reviews that only restate open findings).

## 2. Reviewee experience

What a PR author gets and does with it. Time from push to review, threads answered or ignored, arguments
(rebuttal chains), fixes that broke something else (a new finding in code written to fix an earlier one), comments
that were hard to act on, and whether the authors' agents could apply the fixes.

## 3. Reviewer and operator experience

What the operator did by hand since `since` (the `requests` table: forced reviews, mutes, snoozes, pauses, unpins,
approvals) and why each was needed: every manual step is a candidate for a better default, a clearer board state or a
missing command. Read README's command and board sections and `internal/tui` to know what exists. Board layout stays
one line per PR; a layout change needs a mockup.

## 4. Reliability

Every warn and error event kind since `since`, stuck states (`needs_attention`, attempts above 0, rounds that
retried), restarts, infrastructure failures and the daemon log's repeated messages. For each pattern: the root
cause in the code (file and line), how often it happened, and the smallest fix.

## 5. Code health

`git log` since `since` and the diff of each commit. Review for correctness bugs, races, missing tests, dead code,
duplication, names that lie, and complexity that a simpler mechanism would remove (reuse, simplification,
efficiency, altitude). Check that AGENTS.md conventions hold (compare-and-set transitions, begin/ok/fail events,
no PR text in logs or prompts, tests named after behavior). Report each finding with the file, line, failure scenario
and fix.

## 6. Cost and speed

Agent minutes per role and round kind, rounds per PR, re-review share, Codex and Claude spend signals, waits
(quiet periods, throttles, slots). Which role earns its cost on which kind of round (`magnum stats` value tables),
and what could be skipped, shortened or reused without losing findings.

## 7. Security and trust

Inputs a PR controls (files in the checkout, PR text, branch names, configuration files the agent CLIs load) and
where they reach a prompt, a command, a log or an agent's configuration. Secrets in the slots' environment. Any path
from PR content to code execution outside the sandbox or to a write on GitHub as someone else.

## 8. New capabilities

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
