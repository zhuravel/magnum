The pause is over. Continue the [$magnum-review]({{.SkillPath}}) run for {{.URL}} where you stopped. First list the reviews by `{{.ReviewerLogin}}` on this PR. If one already contains `magnum:run={{.RunID}}`, do not post again: write the result file with its id and stop. Otherwise finish the workflow for head `{{.HeadSHA}}`.

<magnum>
mode: continue
run_id: {{.RunID}}
pr: {{.Owner}}/{{.Repo}}#{{.Number}}
url: {{.URL}}
owner: {{.Owner}}
repo: {{.Repo}}
number: {{.Number}}
head_sha: {{.HeadSHA}}
checkout: {{.Checkout}}
db_lock: {{.DBLockCommand}}
identity: {{.IdentityKind}}
reviewer_login: {{.ReviewerLogin}}
gh_config_dir: {{.GhConfigDir}}
no_findings_event: {{.NoFindingsEvent}}
blocking_event: {{.BlockingEvent}}
self_authored: {{.SelfAuthored}}
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
{{- if .FailingChecks}}
failing_checks: {{.FailingChecks}}
{{- end}}
result_file: {{.ResultFile}}
post_review: {{.PostReviewCommand}}
{{- if .PostRepliesCommand}}
post_replies: {{.PostRepliesCommand}}
{{- end}}
dry_run: {{.DryRun}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
