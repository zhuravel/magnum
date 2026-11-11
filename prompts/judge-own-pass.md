{{template "skill" .}} Start your own pass on {{.URL}} now, while magnum's reviewer roles work: `phase: own_pass` in the skill. Write it to {{.OwnFindings}}, post nothing, then end your turn; their reports follow in the next prompt.
For a missing guard in a set (a list, registry, validator or call sites), sweep the whole set in this pass as the skill's section 2 says: each gap with the same probe, and the `swept N <things>: M gaps` line among your checks.
For a version the PR sets or bumps (an API version in a manifest, a runtime, an SDK), run `date` in this pass: check its age against the platform's support and deploy window, and that its packages and lockfile match its release line, as the skill's section 2 says. When the PR changes which model a call reaches (a model id, a parameter dropped or changed), read that model's page on the provider's own site for the defaults that change the call (thinking on by default and counted in the output limit, parameters it rejects, data-use terms), and check each call's parameters against them.
Also look for these shapes in this pass, and list one only with the input that goes wrong: consent for an irreversible or customer-visible action read from free text (a word list, a regex, a bare "yes" after a question that only mentions the action) needs an explicit approval step, or at least a question that offers the action now; a secret or token sent to a host built from request input needs the host checked against an anchored allowlist of the platform's domains, and no signature or state check that a missing cookie or parameter skips; a find-or-create that adopts a record or remote object by a name, code or few attributes (a slug, an unowned row, a discount) must prove it is the feature's own: every attribute that changes behaviour in the match (a product scope), no other owner, and no other writer that makes rows of that shape (checkout events); a restore, import or bulk overwrite applies the guard and side effects of the editor's path (a live or frozen check), runs a step that can raise before an irreversible delete, keys a match or diff on columns that never change (an A/B test or variant id does change), does not infer a record's origin from state a toggle can leave behind (hidden but enabled rows), remembers that a transaction covers one database (a delete on another shard or a remote survives its rollback), and applies a parent field derived on save again after the child saves that recompute it; a placeholder treated as no value (`null`, `undefined`) must be removed from the data the library stores and sends, not only from a branch's condition, for every source of the field (storage, getters), with a test that feeds invalid values there; a template or default copied into each record at creation reaches only new records, and a value stored from an input (a generated prompt) goes stale when a handler edits the input: ask how existing records get the change; a change that adds cases to a suite or makes a callback fire more often breaks the specs that assert a bound on the total (an exact call count, a worst-case time against a lock or timeout constant): search the specs for that callback's or constant's name; a spec that stubs the call a new guard protects hides that the real call still raises: run the real one; a claim that an error escapes as a server error needs the call chain read for rescues, retries and handlers above the raise; a guard that range-checks a value before a save when the model or schema already validates that range keeps a second copy of the bound: find where the misleading output really comes from and fix it there; a test, sandbox or draft mark copied from a long-lived parent record onto the records that later flows create from it: ask whether real activity after launch inherits the mark and is settled as test, never paid; a rollout step that switches off an existing automation (a setup action, a webhook consumer) also stops what only it serves: list the in-flight and older record types it still handles (an older reward type) and check that the new path credits each; an example for a change to what users receive that asserts only the log or audit row, not the delivered value (the stored message the phone shows), pins nothing: break the delivered text and see which example fails; a value read from an element's position among its siblings (a radio's index for a rating) instead of its own value: a hidden placeholder option or a reversed visual order shifts or inverts it; a module-level cache of the last user-typed value kept as a fallback must be cleared when the action completes, or a later event on the page (single-page navigation, a shared device) is attributed to the previous person; a dedup or idempotency key read from a third-party event payload needs its field checked against the documented payload and a fallback that is not a constant, since an empty key collapses every item onto one entry and drops later events; a change that escapes untrusted text for an HTML sink leaves other values in the same sink or warning type raw (interpolated model fields, data attributes later inserted with an HTML setter): grep every value that reaches it and report one left raw as this PR's incomplete fix, never as pre-existing; an error classifier that treats a rate-limit answer (a 429) as transient, with a retry, a fallback or a "try again in a few minutes" message, must exclude the limits that last hours or weeks (a spend cap, a plan or budget limit), or the generic message hides the cause and the date access returns; a rate limit added to protect a password or other secret check: work out the tries per hour from its count and window, and ask whether sibling endpoints that verify the same secret share one counter, since separate counters and short windows multiply them; a reverse proxy the PR adds or edits that drops client headers (`proxy_pass_request_headers off`, no `X-Forwarded-For`) while the app's rate limiter or auto-ban keys on the client IP: every visitor then shares the proxy's address, and a few requests ban them all; a request gate that rejects by host, origin or caller class for every action of a base controller (a before-action) needs a request spec that sends each mutating verb from every supported class, and a log line or counter on the rejection, since a silent 404 on writes breaks an integration unseen; a test fake of a named lock, queue or channel (`navigator.locks`, a broadcast channel) keeps one queue per name, rejects its waiters on abort and delivers another tab's notice after a slow response, not at once as a microtask: remove the lock or guard and see which example fails, since a name-blind or instant fake passes without it; a write that must arrive (a consent withdrawal, a save) sent from a page that can unload: the `pagehide` handler must not abort it, and the request needs `keepalive` or a beacon, or the old state stays in force. Rank a gap that exists only in one local launcher mode while another documented start mode works P3 at most, and before you report a Secure cookie lost on a plain-HTTP dev or preview host, check that a browser and the third party reach that state (localhost keeps Secure cookies; OAuth providers refuse plain-HTTP redirect URLs on a raw IP).
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
