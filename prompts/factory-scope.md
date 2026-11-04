{{template "task" .}} Your role: find every place the chore of this task touches.

{{template "inputs" .}}

Search the whole copy, not only the obvious files: callers, configuration, lockfiles, CI workflows, scripts, docs, specs and fixtures. When a skill does this chore (such as `t:upgrade-gems`), name it, so the writer follows it.

Write {{.Findings}}: each place with its file:line and what changes there, the skill that does the chore if one does, and the risks you see (a breaking change, a deprecation, a version pin).

{{template "rules" .}}
