# Design: the status comment says why a clean review brought no approval

Date: 2026-10-28. Status: approved by the operator. Inputs: two independent designs (Claude, Codex), merged by the controller.

## Why

For auto-approval, a watch lists a repository in `auto_approve` and sets `auto_approve_as`.
Then magnum posts that person's approval after a review that found nothing blocking.
Sometimes the review is clean and no approval comes. On talkable#12092 one reviewer did not finish ("codex-review (busy)").
On talkable#12100 the PR edits `.github/workflows/ci.yml`, which magnum never approves by design.
In both cases the author and the operator saw a clean review without an approval and asked why.
Today the reason reaches only the operator's board card and a `review.auto_approve_refused` event.

The operator asked for two things:
- Magnum explains on the PR why it did not approve.
- The explanation names the person magnum approves for, and says why that person's own review is needed.

## Goals and non-goals

Goals:
- One line in the PR's status comment names the `auto_approve_as` login and gives the reason in fixed words.
- After the approval posts, the line goes away. After the reason changes, the line changes.
- A withdrawal's dismissal message uses the same fixed words instead of the gate's raw reason.

Non-goals:
- No change to the approval rules. Every gate stays as it is.
- No approval-only comment for watches with `status_comment = false`. The operator's watches all have it on.
- No new GitHub reads, no migration.

Anti-goals, which are failures even with working code:
- The line names a person on a skip list, a watch filter, a label, a file path, a reviewer role or status, or error text.
- The line mentions the person with `@`, which notifies them on every edit.
- The line speaks in the second person ("you").

## Surface

The status comment (`internal/engine/status_comment.go`) is the surface.
It is already one text per PR, edited in place, built from the registry each tick, and limited by `statusEvery`.
The line goes under the posted line, only in the posted stage, and not while an approval hold says that a full pass follows.

Rejected:
- The review body. It posts before the gates decide, and checks and stops change after it.
- A separate comment. Each new comment notifies people and copies the status comment's lifecycle.
- An `@` mention. The operator already gets the card, the event and the toast.
- Moving `noteStatusComments` after `autoApprove` in the tick. The line lags one tick, and that is accepted.
- A basis hash of the facts (the Codex design). The line is rebuilt each tick and checked against the head, the round, the stop and GitHub's latest reviews, so it is not stored.

## Classes and their words

Each refusal gets a class. The class, never the recorded reason, decides the text. `<login>` is `Config.AutoApproveFor(repo).Login`, as plain text.

| Class | Source | Line |
|---|---|---|
| `workflows` | workflow or action files (`autoapprove_gates.go`) | Not approved as `<login>`: the PR changes its own CI workflows or actions, so `<login>` must review and approve it in person. |
| `agent_config` | instruction files, `.claude/`, `.codex/`, `.mcp.json`, a session without the project configuration | Not approved as `<login>`: the PR changes the instructions or configuration of the AI review agents, so `<login>` must review and approve it in person. |
| `too_many_files` | more files than GitHub lists | Not approved as `<login>`: the PR changes more files than Magnum can check, so `<login>` must review and approve it in person. |
| `unheard` | a missing reviewer report | Not approved as `<login>`: one of Magnum's reviewers did not finish on this head, and Magnum approves only a complete review. The next review, or the review of `<login>`, decides. |
| `ci_runs` | checks still run | Approval as `<login>` waits for the checks of this head to pass. |
| `ci_fails` | checks fail (not the way the base fails them) | Approval as `<login>` waits for the checks of this head to pass. Some fail now. |
| `stopped` | any held stop of auto-approval for the PR | Magnum stopped approving this PR as `<login>`. The review of `<login>` decides. |
| `post_failed` | the last failed attempt to post | The approval as `<login>` did not post. Magnum told `<login>`. |

The texts above are the intent. The builder keeps their meaning and can shorten them for plain English.

Silent (no line, the comment reads as on a watch without auto-approval):
- Both author lists (`auto_approve_skip_authors`, `auto_approve_authors`). On 2026-10-28 the operator kept the rule of 2026-10-16.
- Every watch filter of a forced round: a manual repository, a bot, a label, skipped paths, a Codex flag, a draft, a muted PR.
- The operator's own PR, the operator's pending review, and a head that the login already approved.
- Reasons that carry error text or that heal at the next poll: an unreadable run or round record, a missing or failed file list, an incomplete review list, a post that is retried.
- States that the stage of the comment already shows. These are a round due or running, an unreviewed head and a closed PR.
  They are also the approval hold, the GitHub budget hold, a manual verdict after the review, something to fix and an unknown verdict line.
- A gate added later without a class. The default is silence, and a test enforces it.

## Code

- `internal/engine/autoapprove_gates.go`: a `publicRefusal` string type with the class constants, and an `autoRefusal` struct (`why`, `public`, `waits`).
  `autoGateRefusal`, `authorRefusal`, `missingReports`, `changedFilesRefusal`, `standingGateRefusal` and `noteAutoRefused` carry the class.
  `authorRefusal` always has an empty class.
- `AutoApproveRefused` gains `Public string` with the JSON key `public` and `omitempty`.
  `noteAutoRefused` compares the class too. A record that differs only in its class is rewritten without a second event.
- The last failure to post writes the record with the `post_failed` class and no new event, because `review.auto_approve_failed` already records it.
- `internal/engine/status_comment.go`: `publicText(p publicRefusal, login string) string` and an `approvalLine` function.
  `approvalLine` returns text only in this case. The login is set, and the head of the record is the head of the PR.
  The run of the record is the last posted round, and the review of that round is the last review of the PR.
  The latest reviews on GitHub show no approval of the head by the login.
  A stop wins over the record.
- `noteStatusComments` reads the refusal records and the holds once per tick, only for repositories where `AutoApproveFor` is set.
  The line goes into `statusView`, `body` and `state()`.
- The header comment of `status_comment.go` gets one clause: of auto-approval, the comment says only `approvalLine`'s fixed words.
- `standingWithdrawal` uses the class's words without the "Not approved as" prefix.
  A gate without a class reads "found nothing to fix, but Magnum no longer approves it automatically".

## Tests first

Engine tests with `newHarness` and the login `alice`:
- `TestACleanReviewOfAWorkflowChangeSaysWhoMustApprove`: the line names `alice`. The body has no `@alice` and no `.github/workflows`.
- `TestAReviewThatMissedAReviewerSaysSoWithoutTheRole`: the body has no role name and no `busy`.
- `TestTheApprovalLineGoesWhenTheChecksPassAndTheApprovalPosts`: first the `ci_runs` line, then the approval, then one edit that removes the line.
- `TestALaterRoundsWithdrawalBringsTheLineBackOnTheNewHead`.
- `TestAnUnchangedClassEditsNothing`: a reason that changes inside one class writes nothing to GitHub.
- `TestAStoppedAutoApprovalSpeaksInTheThirdPerson`: no "you" and no URL.
- `TestNoApprovalLineOnceTheLoginApprovedTheHead`.
- `TestTheAuthorListsAndFiltersLeaveTheCommentAsWithoutAutoApproval`: the body equals the body on a watch without `auto_approve`.
- `TestNoApprovalLineWithoutAutoApproveOrALogin`.
- `TestAnOldRefusalRecordGainsItsClassWithoutASecondEvent`.
- `TestAFinalPostFailureSaysTheLoginWasTold`.
- `TestTheWithdrawalMessageUsesThePublicWords`: a gate's raw reason is not in the dismissal message.
- `TestApprovalLineNeverCarriesTheRecordsWords`: for every class, the body has no recorded reason text, no `@` and no " you ".

## Docs

- README: a bullet after the posted stage of the status comment, and one sentence in the auto-approval section that points to it.
- docs/DECISIONS.md: a new entry at the end of `## Operations`, "The status comment says why a clean review brought no approval".
  It amends "Nothing on GitHub names the author or the list: the status comment never spoke of auto-approval".
  The author lists stay silent, and the entry lists what was rejected.

## Left to the builder

The exact words within the meaning above, the helper names inside the files, and the test fixtures.

## Follow-up

The self-healing fix for the busy codex-review pane comes next.
It is a separate change: a stuck `codex review` after a restart, and a run that gives up as busy after one minute.
After it, the `unheard` line can say that magnum runs the missing reviewer again.
