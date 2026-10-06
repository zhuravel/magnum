[$magnum-review]({{.SkillPath}}) Review {{.URL}} and post exactly one GitHub review as `{{.ReviewerLogin}}`.

magnum checked out the PR head `{{.HeadSHA}}` (detached) in this directory. {{with .Reports}}Candidate reports from {{len .}} independent reviewer role{{if ne (len .) 1}}s{{end}} are listed below; judge every item, run your own full pass, dedupe, then post.{{else}}No other reviewer role ran for this head; run your own full pass, then post.{{end}}

<magnum>
mode: initial
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
result_file: {{.ResultFile}}
dry_run: {{.DryRun}}
{{- if .Blind}}
blind: true
{{- end}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
