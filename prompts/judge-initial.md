[$magnum-review]({{.SkillPath}}) Read {{.SkillPath}} first, unless this session already read that file, and follow it; load no other skill. Review {{.URL}} and post exactly one GitHub review as `{{.ReviewerLogin}}`.

magnum checked out the PR head `{{.HeadSHA}}` (detached) in this directory. {{if .OwnFindings}}{{if .OwnFindingsMissing}}Your own pass left no {{.OwnFindings}}: run your own full pass now. {{else}}Your own pass is in {{.OwnFindings}}: start from it. {{end}}{{end}}{{with .Reports}}Candidate reports from {{len .}} independent reviewer role{{if ne (len .) 1}}s{{end}} are listed below; judge every item{{if $.OwnFindings}} against your own pass, merge{{else}}, run your own full pass, dedupe{{end}}, then post. Word a security candidate as the skill's section 4 says, not as its report does.{{else}}No other reviewer role ran for this head; run your own full pass, then post.{{end}}
{{- range .Reports}}{{if and (eq .Role "claude-simplify") (not .Missing)}}

{{.Label}}'s report holds optional simplification proposals, each with its current and replacement lines, no defect claims: never judge them by the defect standard or put them in the ledger. Keep a proposal when all hold: its current lines match `{{$.HeadSHA}}` and are lines this PR added or modified (in a re-review, lines changed since the previous review), it removes something a reader must hold (a branch, helper, mode, flag, duplicated block, allocation or control-flow trap), not just moves, renames or rephrases code, and your own equivalence probe proves it preserves behaviour: a focused test, or a command that runs the old and the new code on the same inputs. Give each probe one line in Checks: the command, marked `(equivalence probe)`, and its result. Drop a proposal without one, and any that edits authorization, sandboxing, money or usage recording, or concurrency code, unless it removes a defect-prone construct. One comment per idea: a proposal becomes a ```suggestion at its first site plus "Same change at L…" for the others. Its first line is the title alone, `**Simplification** (optional, no reply needed)`, then a blank line, one sentence on what it removes, and the suggestion. Post at most three, the most substantial, ordered by what they remove, most first. They never affect the verdict. Add `"candidates":{"{{.Role}}":{"suggested":N}}` to the result file, N the ones you post.
{{- end}}{{end}}
{{- if .Blind}}

Blind evaluation: magnum measures what a review of exactly `{{.HeadSHA}}` finds, so nothing written about the PR afterwards may reach you. The PR may be closed or merged and its GitHub head may have moved: skip the `state == open` check, and take `git diff {{.BaseSHA}}..{{.HeadSHA}}` in this checkout as the diff and the review boundary, never GitHub's PR files or diff. Read the PR description and the commits up to `{{.HeadSHA}}` only. Do not read reviews, review comments, issue comments or replies (on this PR or elsewhere), CI results, or any commit, branch or tag newer than `{{.HeadSHA}}` (no `git log --all`, no `refs/magnum/*`, no `origin/{{.BaseRef}}` past `{{.BaseSHA}}`). Otherwise the dry-run rules apply. The `notes` file is a scratch copy: update it as usual.
{{- end}}
{{- if .PostMerge}}

Post-merge review: GitHub merged the PR before magnum reviewed `{{.HeadSHA}}`. Expect `merged == true` instead of `state == open`. Post `COMMENT` whatever you find (`no_findings_event` and `blocking_event` say so), start the body with `**Post-merge review** of <head_sha, 7 chars>:`, write each finding as a follow-up for a new change, not a change to this PR, and say `in a follow-up` instead of `before merging`.
{{- end}}

<magnum>
mode: initial
{{- if .Phase}}
phase: {{.Phase}}
{{- end}}
run_id: {{.RunID}}
pr: {{.Owner}}/{{.Repo}}#{{.Number}}
url: {{.URL}}
owner: {{.Owner}}
repo: {{.Repo}}
number: {{.Number}}
head_sha: {{.HeadSHA}}
base_ref: {{.BaseRef}}
base_sha: {{.BaseSHA}}
checkout: {{.Checkout}}
db_lock: {{.DBLockCommand}}
identity: {{.IdentityKind}}
reviewer_login: {{.ReviewerLogin}}
gh_config_dir: {{.GhConfigDir}}
no_findings_event: {{.NoFindingsEvent}}
blocking_event: {{.BlockingEvent}}
self_authored: {{.SelfAuthored}}
reports:
{{- range .Reports}}
  - {{.Label}}: {{if .Missing}}missing ({{.Detail}}){{else}}{{.Path}}{{end}}
{{- end}}
{{- with .Readiness}}{{if .Checks}}
readiness:{{if .File}} {{.File}}{{end}}
{{- range .Checks}}
  - {{.Kind}} `{{.Command}}`: {{.Status}}{{if .Duration}} in {{.Duration}}{{end}}{{if .Detail}} ({{.Detail}}){{end}}
{{- end}}
{{- end}}{{end}}
{{- if .NotesPath}}
notes: {{.NotesPath}}
notes_dir: {{.NotesDir}}
notes_harness:{{range $i, $f := .NotesHarness}}{{if $i}},{{end}} {{$f}}{{end}}{{if .NotesHarnessMore}} (+{{.NotesHarnessMore}} more){{end}}
notes_lock: {{.NotesLockCommand}}
notes_unlock: {{.NotesUnlockCommand}}
{{- end}}
{{- if .RelatedPRs}}
related_prs: {{.RelatedPRs}}
{{- end}}
{{- if .HistoryFile}}
history: {{.HistoryFile}}
{{- end}}
{{- if .DocsFile}}
docs: {{.DocsFile}}
{{- end}}
{{- if .FailingChecks}}
failing_checks: {{.FailingChecks}}
{{- end}}
{{- if .ProjectChecks}}
project_checks: {{.ProjectChecks}}
{{- end}}
{{- if .OwnFindings}}
own_findings: {{.OwnFindings}}
{{- end}}
result_file: {{.ResultFile}}
post_review: {{.PostReviewCommand}}
dry_run: {{.DryRun}}
{{- if .Blind}}
blind: true
{{- end}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
