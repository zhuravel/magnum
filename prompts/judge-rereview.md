{{- if .SameHead -}}
{{template "skill" .}} No new commits were pushed to {{.URL}} since your last review.

HEAD is still `{{.HeadSHA}}`, the commit your last review ({{.PreviousReviewID}}, {{.PreviousEvent}}) covered.
{{- else -}}
{{template "skill" .}} New commits were pushed to {{.URL}}. Read the replies and re-review.

magnum already updated this checkout: HEAD is `{{.HeadSHA}}`. Your last review ({{.PreviousReviewID}}, {{.PreviousEvent}}) covered `{{.PreviousHeadSHA}}`.
{{- end}}
{{- if .FormerLogins}}
This PR's earlier reviews were posted as {{range $i, $l := .FormerLogins}}{{if $i}}, {{end}}`{{$l}}`{{end}} before magnum moved it to `{{.ReviewerLogin}}`. Their reviews and threads are yours: count their findings as your earlier findings and decide their threads under the reply contract. {{template "post-as-reviewer" .}}
{{- end}}
{{- if .EffortInPrompt}}
Work at {{.Effort}} reasoning effort for this re-review.
{{- end}}
{{- if .SameHead}}
The head is unchanged: re-read the replies and the comments since your last review, re-decide each earlier finding under the reply contract, and run no check your last review already ran on this head.{{template "replies" .}}
{{- template "ci-red" .}}
{{- else if .ForcePushed}}
{{template "force-pushed" .}}
{{- else if .BaseMerged}}
{{template "base-merged" .}}
{{- else}}
{{template "new-commits" .}}
{{- end}}
{{- if .DeltaCheck}}
Only the commits since your last review changed ({{.DeltaLines}} {{if eq .DeltaLines 1}}line{{else}}lines{{end}}{{if .DeltaFile}}; files listed in {{.DeltaFile}}{{end}}). Review just those changes against the PR's purpose and your earlier findings; the rest stands as reviewed. Post one short review.
{{- end}}
{{- template "moved-from" .}}
{{- template "threads-and-stops" .}}
{{template "rebuttals" .}}
{{- if .OwnFindings}}
{{if .OwnFindingsMissing}}Your own pass left no {{.OwnFindings}}: do it now.{{else}}Your own pass, with your reply-contract decisions, is in {{.OwnFindings}}: start from it.{{end}}
{{- end}}
{{if not .SameHead}}Fresh candidate reports for this head are listed below{{if .OwnFindings}}: judge every item against your own pass{{end}}. Word a security candidate as the skill's section 4 says, not as its report does. {{end}}Follow the skill's re-review section, the reply contract included, and post exactly one new review on `{{.HeadSHA}}`{{if .Replies}}, or only the replies when your verdict and event stay{{end}}.
{{- template "simplify" .}}
{{- template "post-merge-rereview" .}}
{{- template "hold-approval" .}}
{{- template "no-approval" .}}
{{- template "review-rules" .}}

<magnum>
mode: rereview
{{- if .Phase}}
phase: {{.Phase}}
{{- end}}
{{template "pr-fields" .}}
base_ref: {{.BaseRef}}
base_sha: {{.BaseSHA}}
{{template "login-fields" .}}
former_logins: {{range $i, $l := .FormerLogins}}{{if $i}}, {{end}}{{$l}}{{end}}
{{template "event-fields" .}}
previous_review_id: {{.PreviousReviewID}}
previous_head_sha: {{.PreviousHeadSHA}}
since: {{.Since}}
force_pushed: {{.ForcePushed}}
{{- if .BaseMerged}}
base_merged: true
{{- end}}
{{- if .DeltaCheck}}
delta_check: true
{{- end}}
moved_from: {{.MovedFrom}}
threads_file: {{.ThreadsFile}}
{{template "review-fields" .}}
{{- if .PostRepliesCommand}}
post_replies: {{.PostRepliesCommand}}
{{- end}}
dry_run: {{.DryRun}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
