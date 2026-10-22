{{template "skill" .}} Re-review {{.URL}}. An earlier session reviewed this PR, but its history is gone.
{{template "earlier-reviews" .}}
Read them and their threads first (`gh api repos/{{.Owner}}/{{.Repo}}/pulls/{{.Number}}/reviews/<id>/comments`), then follow the skill's re-review section for head `{{.HeadSHA}}`.{{if not .SameHead}} Candidate reports for this head are listed below; word a security candidate as the skill's section 4 says, not as its report does.{{end}}
{{- $merged := and .BaseMerged (not .ForcePushed)}}
{{- if .FullPass}}
This is a scheduled full pass: review the full PR diff again (`git diff {{if .BaseSHA}}{{.BaseSHA}}{{else}}origin/{{.BaseRef}}{{end}}...{{.HeadSHA}}`), the way a first review does (section 2 of the skill), not only the commits since `{{.PreviousHeadSHA}}`. Then decide every earlier thread under the reply contract (section 6). Earlier findings stay counted. A proved finding the earlier reviews missed ends its title with `(missed earlier)`. Head the body `**Re-review <previous_head_sha, 7 chars> → <head_sha, 7 chars> (full pass):**`.
{{- else if .ForcePushed}}
{{template "force-pushed" .}}
{{- else if $merged}}
{{template "base-merged" .}}
{{- else if and .PreviousHeadSHA (not .SameHead)}}
{{template "new-commits" .}}
{{- end}}
{{- if .AuditFile}}
This is an audit pass: {{.AuditFile}} lists {{.AuditAreas}} {{if eq .AuditAreas 1}}area{{else}}areas{{end}} of this PR that no round probed or whose files changed since (`why` says what makes each risky), and, marked `rotation`, an earlier probe to run again. First probe each through its real entry point, the way its callers reach it, with a control, and give each probe one line in Checks; write each probe that passes to `evidence_file` as `probed`. An area you could not probe is a lead, never a finding: post no finding that says an area is unknown or unprobed. Then review the commits as this prompt says above and decide the earlier threads under the reply contract (section 6).
{{- end}}
{{- if .OwnFindings}}
{{if .OwnFindingsMissing}}Your own pass left no {{.OwnFindings}}: do it now.{{else if .FullPass}}Your own pass, written before you read the earlier reviews, is in {{.OwnFindings}}: judge every candidate against it.{{else}}Your own pass, with your reply-contract decisions, is in {{.OwnFindings}}: judge every candidate against it.{{end}}
{{- end}}
{{- if .DeltaCheck}}
Only the commits since your last review (`{{.PreviousHeadShort}}`) changed ({{.DeltaLines}} {{if eq .DeltaLines 1}}line{{else}}lines{{end}}{{if .DeltaFile}}; files listed in {{.DeltaFile}}{{end}}){{if $merged}}, counted as the change between the PR's own diff before and after them{{end}}. Read your previous review and its threads for context, then review just those changes; the rest stands as reviewed. Post one short review.
{{- end}}
{{- if .SameHead}}
The head is unchanged (`{{.PreviousHeadShort}}`): re-read the replies and the comments since your last review, re-decide each earlier finding under the reply contract, and run no check your last review already ran on this head.{{template "replies" .}}
{{- template "ci-red" .}}
{{- end}}
{{- if .FormerLogins}}
{{template "former-logins" .}} {{template "post-as-reviewer" .}}
{{- end}}
{{- template "threads-and-stops" .}}
{{template "rebuttals" .}}
{{- template "simplify" .}}
{{- template "post-merge-rereview" .}}
{{- template "hold-approval" .}}
{{- template "review-rules" .}}

<magnum>
mode: recovery
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
{{- if $merged}}
base_merged: true
{{- end}}
{{- if .DeltaCheck}}
delta_check: true
{{- end}}
{{- if .FullPass}}
full_pass: true
{{- end}}
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
