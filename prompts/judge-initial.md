{{template "skill" .}} Review {{.URL}} and post exactly one GitHub review as `{{.ReviewerLogin}}`.

magnum checked out the PR head `{{.HeadSHA}}` (detached) in this directory. {{if .OwnFindings}}{{if .OwnFindingsMissing}}Your own pass left no {{.OwnFindings}}: run your own full pass now. {{else}}Your own pass is in {{.OwnFindings}}: start from it. {{end}}{{end}}{{with .Reports}}Candidate reports from {{len .}} independent reviewer role{{if ne (len .) 1}}s{{end}} are listed below; judge every item{{if $.OwnFindings}} against your own pass, merge{{else}}, run your own full pass, dedupe{{end}}, then post. Word a security candidate as the skill's section 4 says, not as its report does.{{else}}No other reviewer role ran for this head; run your own full pass, then post.{{end}}
{{- template "simplify" .}}
{{- if .Blind}}

{{template "blind" .}} The `notes` file is a scratch copy: update it as usual.
{{- end}}
{{- if .PostMerge}}

Post-merge review: GitHub merged the PR before magnum reviewed `{{.HeadSHA}}`. Expect `merged == true` instead of `state == open`. Post `COMMENT` whatever you find (`no_findings_event` and `blocking_event` say so), start the body with `**Post-merge review** of <head_sha, 7 chars>:`, write each finding as a follow-up for a new change, not a change to this PR, and say `in a follow-up` instead of `before merging`.
{{- end}}
{{- template "no-approval" .}}
{{- template "review-rules" .}}

<magnum>
mode: initial
{{- if .Phase}}
phase: {{.Phase}}
{{- end}}
{{template "pr-fields" .}}
base_ref: {{.BaseRef}}
base_sha: {{.BaseSHA}}
{{template "login-fields" .}}
{{template "event-fields" .}}
{{template "review-fields" .}}
dry_run: {{.DryRun}}
{{- if .Blind}}
blind: true
{{- end}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
