{{template "task" .}} Your role: read the history of the code that the fix of this task will change.

{{template "inputs" .}}

Find the code the fix will most likely change, from the ticket and the code. For each guard in it (a condition, a rescue, a validation, a feature flag, an early return), find when and why it was added: `git log -L`, `git blame`, the commit messages and the PR or issue they name. Do not design the fix.

Write {{.Findings}}: one list item per guard, starting with its place as `path:line` (its line in origin/{{.Base}}), then the commit that added it (short sha and date), why it was added (after `why:`), and what would break if the fix removed or changed it (after `breaks:`). The ready card names each guard the change touches with your why.

{{template "rules" .}}
