{{template "skill" .}} Start your own pass on {{.URL}} now, while magnum's reviewer roles work: `phase: own_pass` in the skill. Write it to {{.OwnFindings}}, post nothing, then end your turn; their reports follow in the next prompt.
For a missing guard in a set (a list, registry, validator or call sites), sweep the whole set in this pass as the skill's section 2 says: each gap with the same probe, and the `swept N <things>: M gaps` line among your checks.
For a version the PR sets or bumps (an API version in a manifest, a runtime, an SDK), run `date` in this pass: check its age against the platform's support and deploy window, and that its packages and lockfile match its release line, as the skill's section 2 says.
Also look for these shapes in this pass, and list one only with the input that goes wrong: consent for an irreversible or customer-visible action read from free text (a word list, a regex, a bare "yes" after a question that only mentions the action) needs an explicit approval step, or at least a question that offers the action now; a secret or token sent to a host built from request input needs the host checked against an anchored allowlist of the platform's domains, and no signature or state check that a missing cookie or parameter skips; a find-or-create that adopts a record or remote object by a name, code or few attributes (a slug, an unowned row, a discount) must prove it is the feature's own: every attribute that changes behaviour in the match (a product scope), no other owner, and no other writer that makes rows of that shape (checkout events); a restore, import or bulk overwrite applies the guard and side effects of the editor's path (a live or frozen check), runs a step that can raise before an irreversible delete, keys a match or diff on columns that never change (an A/B test or variant id does change), does not infer a record's origin from state a toggle can leave behind (hidden but enabled rows), remembers that a transaction covers one database (a delete on another shard or a remote survives its rollback), and applies a parent field derived on save again after the child saves that recompute it; a placeholder treated as no value (`null`, `undefined`) must be removed from the data the library stores and sends, not only from a branch's condition, for every source of the field (storage, getters), with a test that feeds invalid values there; a template or default copied into each record at creation reaches only new records, and a value stored from an input (a generated prompt) goes stale when a handler edits the input: ask how existing records get the change; a change that adds cases to a suite or makes a callback fire more often breaks the specs that assert a bound on the total (an exact call count, a worst-case time against a lock or timeout constant): search the specs for that callback's or constant's name; a spec that stubs the call a new guard protects hides that the real call still raises: run the real one; a claim that an error escapes as a server error needs the call chain read for rescues, retries and handlers above the raise.
{{- if .RestartedFrom}}
New commits arrived while you worked: magnum moved this checkout from `{{.RestartedFrom}}` to `{{.HeadSHA}}` and restarted the round. Do the pass for `{{.HeadSHA}}`, reusing what still applies (`git diff {{.RestartedFrom}}..{{.HeadSHA}}` shows what changed).
{{- end}}
{{- if eq .Mode "rereview"}}
New commits were pushed: HEAD is `{{.HeadSHA}}`. Your last review ({{.PreviousReviewID}}, {{.PreviousEvent}}) covered `{{.PreviousHeadSHA}}`.
{{- if .ForcePushed}}
{{template "force-pushed" .}}
{{- else if .BaseMerged}}
{{template "base-merged" .}}
{{- else}}
{{template "new-commits" .}}
{{- end}}
{{- else if and (eq .Mode "recovery") .FullPass}}
This is a scheduled full pass: earlier sessions reviewed this PR, but review it now as a first review does (sections 1, 2 and 4 of the skill), on the full PR diff (`git diff {{if .BaseSHA}}{{.BaseSHA}}{{else}}origin/{{.BaseRef}}{{end}}...{{.HeadSHA}}`). Read no earlier review, review thread or reply in this pass: the next prompt lists them, and you decide every earlier thread under the reply contract then.
{{- else if eq .Mode "recovery"}}
An earlier session reviewed this PR, but its history is gone. {{template "earlier-reviews" .}}
Read them and their threads first (`gh api repos/{{.Owner}}/{{.Repo}}/pulls/{{.Number}}/reviews/<id>/comments`).
{{- if .ForcePushed}}
{{template "force-pushed" .}}
{{- else if .BaseMerged}}
{{template "base-merged" .}}
{{- else if and .ColdJudge .PreviousHeadSHA (ne .PreviousHeadSHA .HeadSHA)}}
Read the new commits with `git log --oneline {{.PreviousHeadSHA}}..{{.HeadSHA}}` and `git diff {{.PreviousHeadSHA}}..{{.HeadSHA}}` and review those: the earlier reviews cover the rest of the PR.
{{- end}}
{{- else}}
magnum checked out the PR head `{{.HeadSHA}}` (detached) in this directory.
{{- end}}
{{- if and .FormerLogins (ne .Mode "initial") (not .FullPass)}}
{{template "former-logins" .}}
{{- end}}
{{- if and .ThreadsFile (not .FullPass)}}
{{template "threads" .}}
{{- end}}
{{- if and (ne .Mode "initial") (not .FullPass)}}
Decide the reply contract for your earlier findings now and note each decision in {{.OwnFindings}}; post the rebuttals only in the next phase.
{{- end}}
{{- if .EffortInPrompt}}
Work at {{.Effort}} reasoning effort for this {{if eq .Mode "rereview"}}re-review{{else}}review{{end}}.
{{- end}}
{{- template "moved-from" .}}
{{- if .Blind}}
{{template "blind" .}}
{{- end}}
{{- if .PostMerge}}
GitHub merged the PR before magnum reviewed `{{.HeadSHA}}`: expect `merged == true` instead of `state == open`, and find follow-ups for a new change, not changes to this PR.
{{- end}}
{{- if .PrepassFile}}
{{template "prepass" .}}
{{- end}}
{{- if .EvidenceMap}}
{{template "evidence-map" .}}
{{- end}}
{{- if .StackedBelow}}
{{template "stacked" .}}
{{- end}}

<magnum>
mode: {{.Mode}}
phase: own_pass
{{template "pr-fields" .}}
base_ref: {{.BaseRef}}
base_sha: {{.BaseSHA}}
{{template "login-fields" .}}
{{- if and (ne .Mode "initial") (not .FullPass)}}
former_logins: {{range $i, $l := .FormerLogins}}{{if $i}}, {{end}}{{$l}}{{end}}
{{- end}}
gh_config_dir: {{.GhConfigDir}}
self_authored: {{.SelfAuthored}}
{{- if and (ne .Mode "initial") (not .FullPass)}}
previous_review_id: {{.PreviousReviewID}}
previous_head_sha: {{.PreviousHeadSHA}}
since: {{.Since}}
{{- end}}
{{- if ne .Mode "initial"}}
force_pushed: {{.ForcePushed}}
{{- end}}
{{- if and .BaseMerged (or (eq .Mode "rereview") (not .ForcePushed)) (ne .Mode "initial")}}
base_merged: true
{{- end}}
{{- if .FullPass}}
full_pass: true
{{- end}}
{{- if eq .Mode "rereview"}}
moved_from: {{.MovedFrom}}
{{- end}}
{{- if and (ne .Mode "initial") (not .FullPass)}}
threads_file: {{.ThreadsFile}}
{{- end}}
{{- template "readiness-field" .}}
{{- if .NotesPath}}
{{template "notes-fields" .}}
{{- end}}
{{- template "context-fields" .}}
own_findings: {{.OwnFindings}}
dry_run: {{.DryRun}}
{{- if .Blind}}
blind: true
{{- end}}
{{- if .PostMerge}}
post_merge: true
{{- end}}
</magnum>
