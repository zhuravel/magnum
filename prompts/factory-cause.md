{{template "task" .}} Your role: find the cause of the bug on your own, apart from the other investigators.

{{template "inputs" .}}

Do not read what the other investigators write (the other findings-*.md files of {{.TaskDir}}), and do not run a spec another agent left uncommitted in the copy (`git status` shows it): the factory compares your cause with theirs, so it must be your own. Read the code, its callers and the data it reads, and run what you need to confirm the cause.

Write {{.Findings}}: the cause in one sentence, then the chain from the trigger to the failure with each step's file:line, the evidence (the commands you ran and their output), the other causes you ruled out and why, and how sure you are.

{{template "rules" .}}
