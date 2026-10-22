{{template "skill" .}} The pause is over. Continue the run for {{.URL}} where you stopped. First list the reviews by `{{.ReviewerLogin}}` on this PR. If one already contains `magnum:run={{.RunID}}`, do not post again: write the result file with its id and stop. Otherwise finish the workflow for head `{{.HeadSHA}}`.
{{- template "no-approval" .}}
{{- template "notes-line-limit" .}}

<magnum>
mode: continue
{{template "pr-fields" .}}
{{template "login-fields" .}}
{{template "event-fields" .}}
{{- if .NotesPath}}
{{template "notes-fields" .}}
{{- if .NotesMaxLine}}
notes_max_line: {{.NotesMaxLine}}
{{- end}}
{{- end}}
{{- template "related-field" .}}
{{- if .TicketFile}}
ticket: {{.TicketFile}}
{{- end}}
{{- if .FailingChecks}}
failing_checks: {{.FailingChecks}}
{{- end}}
{{template "result-fields" .}}
{{- if .PostRepliesCommand}}
post_replies: {{.PostRepliesCommand}}
{{- end}}
dry_run: {{.DryRun}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
