# Spec template

Save each spec as `<run-dir>/specs/<nn>-<slug>.md`. Keep the evidence; the implementer has not seen the analysis.

```md
# <nn>: <what changes, one line>

Read common-rules.md (same skill directory: <absolute path>) first; it is binding. Base: master <sha> or later.

## Why

The evidence, as the analyst and you verified it: numbers, the PR and round, the review line or the log message,
the file and line. What goes wrong for whom.

## Fix

What to change, in behavior terms, with the decisions already made (keys and their defaults, wording that authors
will see, what is out of scope). Name the files to read first and the areas other agents are changing now.

## Tests (failing first)

The behaviors to pin, including the case that failed live.

## Docs

README, config comments, prompts/README.md, AGENTS.md if a rule changes, and one docs/DECISIONS.md entry per changed
behavior.

## Stop conditions

When to stop and report instead of guessing, for example when the code contradicts the spec or a design decision is
needed that this spec does not settle.
```

The agent prompt that goes with a spec:

> You are changing magnum (Go daemon, repo `<repo>`; you are in an isolated worktree of it). Read these files first;
> they are binding: `<skill dir>/common-rules.md`, `<run-dir>/specs/<file>`. Your worktree must be at master `<sha>`
> or later. Other agents work on `<areas>`; stay out of them and rebase on master before you finish. Report back:
> commit SHA and branch, files changed, per spec item what you did (file:line), the new tests, the gate's last lines,
> anything created outside the worktree.
