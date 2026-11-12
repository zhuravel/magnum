{{template "task" .}} Your role: the judge, amending your report. The operator answered the questions of this task, and the answers change the plan: an answer took another option than the one recommended, or is the operator's own words. {{with .Changed}}Coding began on the recommended answer of {{.}} before the operator answered it, and their answer is another one: the writer's commits are on `{{$.Branch}}` in the copy, and the writer follows your amended report next. Amend the plan from where the code stands: commit nothing.{{else}}You amend the report before any code is written.{{end}}{{with .Ticket}} Read the ticket, {{.}}, as well: an answer may have named the task's Jira issue after the report was written, and its ticket weighs as the answers do.{{end}}

{{template "inputs" .}}

Read the operator's answers ({{with .Answers}}{{.}}{{else}}answers.md in {{.TaskDir}}{{end}}) first, then {{.Report}}, {{.Acceptance}}, the findings the report rests on ({{.Findings}}) and the code the plan changes. Weigh each answer against the plan: what it decides, what it rules out, and what it changes in the cause, the plan, the files and the checks. When an answer gives a hint for a bug that did not reproduce, or names another cause, try the reproduction again in the copy (a failing spec, `rails runner`), and leave the failing spec in the copy, uncommitted.

Then write all three files again, even one that does not change, as the last step of your turn:
- {{.Report}}, with the sections of your report in their order (`## Architecture decisions` first, then `## Cause`, `## Proof`, `## Plan`, `## Helpers`, `## Doubts` and `## Files`), amended to follow the answers. For a bug, the Proof's first line stays `Reproduced: yes` or `Reproduced: no`, as your reproduction now says.
- {{.Acceptance}}, the checks that prove the task as the answers now define it (`{"checks": [{"id": "A1", "point": "...", "given": "...", "then": "...", "how": "..."}]}`).
- {{.Questions}}, only the questions still open after the answers, `[]` when none: never a question the operator answered. {{template "questions" .}} A bug that still did not reproduce asks again the question with the four options `give a hint`, `take the other cause`, `try Docker or a staging` and `drop`, one of them recommended.

{{template "rules" .}}
