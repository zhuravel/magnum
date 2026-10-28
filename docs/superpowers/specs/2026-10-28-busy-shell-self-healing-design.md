# Design: a shell reviewer heals its busy pane, and a review that missed a reviewer is completed

Date: 2026-10-28. Status: approved under the operator's rule of 2026-10-28 ("magnum heals itself as much as possible": fix the cause and add the recovery).
Inputs: two independent designs (Claude, Codex), merged by the controller. It builds after docs/superpowers/specs/2026-10-28-auto-approval-explained-design.md lands.

## Why

From 2026-10-16 to 2026-10-28, 12 codex-review runs ended `busy`. In 11 of them a push moved the head during a round, and the round restarted.
The restart pressed ctrl+c three times, 10 seconds apart, and `codex review` still ran ("codex-review still runs 10s after 3 ctrl+c for the restart").
The next codex-review run waited 60 seconds for an idle shell and failed: "shell not idle after 1m0s (foreground: codex,magnum)".
The round went on without that reviewer, and auto-approval refused its clean review, because the review did not hear every reviewer.
That cost 6 approvals from 2026-10-25 to 2026-10-28 (talkable#12092 is one). On talkable#12079 two runs, 5 minutes apart, both found the pane busy.

The shell line is `magnum db-lock --checkout <slot> --role codex-review -- codex review ...`.
`db-lock` starts its child in the job's own process group and passes each signal on (`internal/cli/db_lock.go:155-189`).
So each ctrl+c reaches codex twice, and `db-lock` then blocks in `cmd.Wait()`. The shell is idle only after `db-lock` exits.
Nobody knows yet why codex goes on after SIGINT. This design works without that answer, and its events record the process tree so that a later look can answer it.

## Goals

1. After an interrupt, a command that `db-lock` runs ends. That is also true for a command that ignores SIGINT.
2. The restart stops a cut shell command for sure, and it never switches the checkout while that command still runs.
3. A shell role that finds its pane busy recovers. Magnum's own earlier run is stopped at once. Anything else is never touched: the role moves to a new pane.
4. A clean review that missed a reviewer for a passing cause is completed on the same head, so auto-approval can follow without the operator.

Non-goals: no new herdr API, and no change to what auto-approval requires.
Also no change for a reviewer that refused, is logged out or is at a usage limit.

Anti-goals: magnum signals or closes a process that it cannot prove is its own run. A completion loops without end.

## Part A: db-lock ends a command that outlives its interrupt

In `runLocked`, after db-lock passes on the first SIGINT, SIGTERM or SIGHUP:
- If the child still runs `dbLockKillAfter` (10 seconds) later, db-lock sends SIGTERM to the child and its descendants.
- If the child still runs `dbLockKillGrace` (5 seconds) after that, db-lock sends SIGKILL to them.
- db-lock prints one line on stderr, for example `db-lock: <command> still runs 10s after SIGINT; sent SIGTERM`, and exits with 128 plus the signal number.

The descendants come from the process table at the moment of each signal, so a child in another process group (a sandbox) is included.
The same grace applies to the agent roles' spec runs through db-lock. An interrupted spec run that ignores SIGINT ends too.

Test first: `TestDBLockKillsACommandThatTrapsSIGINT` with a real `sh -c 'trap "" INT; sleep 60'`. db-lock exits within 16 seconds. A second test makes sure that a child that exits on SIGINT gets no SIGTERM.

## Part B: magnum stops its own run, and moves away from anything else

### The run token

The shell line carries the run id: `magnum db-lock --checkout <slot> --role <role> --run <run id> -- ...`.
db-lock accepts `--run` and records it in its holder file. It never acts on it.
A process counts as an own run of magnum in one case only. All of these are true:
- Its argv is magnum's binary with `db-lock`, this role, this checkout and a run id.
- The pane is the shell session pane of this PR's role.
- The run id names a run of this PR and role in the registry.
- The run is the cut run, or it is no longer in flight.

Before each signal, magnum reads the process information of the pane again. If the process group or the process of the token changed, magnum sends nothing.

### Stopping

A new `StopShell` in `internal/agents` stops an owned run:
1. Take the process tree under the token's process.
2. Send SIGTERM to the process group and that tree. Wait up to 5 seconds for an idle shell.
3. Send SIGKILL the same way. Wait up to 5 seconds.
4. If the pane is still busy, split a replacement pane with the recorded working directory and environment.
   Point the session at the replacement, and close the old pane.

The `stopShell` of the pipeline calls `StopShell` after its last ctrl+c. If `StopShell` fails, the restart writes a warning. Otherwise it writes none.
If the cut command still runs after `StopShell`, the restart does not switch the checkout. It ends the round as failed with that cause, and the normal retry path takes the PR again later.

### A busy pane at the next run

In `RunShell`, the 60-second wait for an idle shell changes:
- If the busy command is an owned run that is no longer in flight, magnum calls `StopShell` at once and runs the line.
- If the busy command is anything else, magnum first waits `IdleShellTimeout` as today. A fresh shell or a short command clears in that time.
- If the pane is still busy, magnum splits a new pane with the recorded environment, points the session at it, and runs the line there.
  It never sends a key, a signal or a close to the busy pane. The busy pane goes on a retired row, and `CloseRetiredPanes` closes it only after it is an idle shell.
- Without a recorded environment, the result is `ErrBusy`, as today.

### Events

Every destructive step writes begin, ok and fail events. Their subject is the PR.
- `pr.shell_stop_begin`: the role, pane, run and process group. It also lists the tree as process id, group and name, never argv.
- `pr.shell_stopped`: the step that worked and the seconds.
- `pr.shell_stop_failed`: the cause, `not_own`, `signal` or `close`.
- `pr.shell_move_begin`, `pr.shell_moved` and `pr.shell_move_failed`: a move to a new pane.

### Tests first

- `TestStopShellTermsTheProcessGroupOfItsOwnRun`: one SIGTERM to the group, no close, begin and ok events.
- `TestStopShellKillsWhenTermIsIgnored`: SIGTERM, then SIGKILL 5 seconds later on the fake clock.
- `TestStopShellNeverSignalsACommandWithoutItsRunToken`: no signal, no close, `ErrNotOwn`.
- `TestStopShellNeverSignalsAnotherPRsOrAnInFlightRun`.
- `TestStopShellRechecksTheProcessGroupBeforeEachSignal`.
- `TestStopShellKillsDescendantsThatLeftTheGroup`.
- `TestStopShellReplacesThePaneWhenKillLeavesItBusy`.
- `TestARestartKillsACodexReviewThatOutlivesItsCtrlC` (pipeline): the restarted codex-review report is `ok`, with no "still runs" warning.
- `TestARestartNeverSwitchesTheCheckoutUnderARunningCommand`.
- `TestRunShellReclaimsAPaneItsEarlierRunHolds`: the line runs before 60 seconds pass on the fake clock.
- `TestRunShellMovesToANewPaneWhenSomethingElseRuns`: a split with the environment, zero signals and closes.
- `TestRunShellStaysBusyWithoutAnEnvironmentToMoveWith`.
- `TestAFreshPaneStillGetsItsMinuteToStart`.
- `TestALeftBusyPaneClosesOnlyOnceIdle`.
- `TestTheShellLineCarriesTheRunInItsDBLockLine`.

## Part C: a clean review that missed a reviewer is completed

The mechanism is the same-head re-review (`internal/engine/same_head.go`), widened to run the missing roles too.
The judge stays in its live session, and the round does not count toward the day or the full pass.
Rejected: a continue, because it runs no reviewer. A forced review with roles, because it runs every requested role again in full.
A full pass, because every role starts fresh and it costs most.

Trigger, after the round settles. All of these are true:
- The review posted clean, auto-approval applies to the repository, and the round did not hold its approval.
- The round is not post-merge, a dry run or an eval, and the head did not move.
- The round's missing-report record has entries, and every cause is a passing cause.
- Fewer than 2 completions ran on this head.

Passing causes: busy, timeout, lost, no_session, failed, stalled, overloaded, cancelled, human_active, missing.
Not passing: a refusal, a logged-out CLI, a usage limit. These need the operator or time, and the existing pauses handle them.

The completion round runs the judge and the missing roles only, with no triage and no restarts.
The judge gets the other reports of the completed round and the late reports, and decides the findings of the late reports.
The completion's own missing-report record lists only what it still missed, so the existing gates decide the approval.
A review of another head, or the closing of the PR, drops the completion. After 2 attempts on a head, magnum writes `pr.complete_gave_up` once, and the refusal stays.

Events: `pr.complete_queued` (round, head, roles, causes, attempt), `pr.complete_failed`, `pr.complete_gave_up`, and `completes` in the data of `engine.round_start`.

After Part C, the `unheard` line of the status comment says that magnum runs the missing reviewer again, while a completion is queued or running.

Tests first:
- `TestACleanReviewThatMissedABusyReviewerIsCompleted`: only codex-review and the judge start, and the auto-approval posts.
- `TestCompletionHandsTheJudgeTheOtherReportsOfItsRound`.
- `TestCompletionIsNotForARefusedOrLoggedOutReviewer`.
- `TestCompletionGivesUpAfterTwoAttemptsOnAHead`.
- `TestAPushDropsTheCompletion`.
- `TestAHeldReviewGetsItsFullPassNotACompletion`.
- `TestCompletionCountsNeitherTodayNorTowardTheFullPass`.

## Rulings (the controller's, under the operator's self-healing rule)

- Magnum can SIGKILL its own run. It is magnum's process in magnum's pane, and SIGTERM alone left the PR stuck.
- A pane that something else keeps busy is closed only after it is idle.
- A completion posts a new same-head re-review, the way a same-head re-review already posts. It does not edit the earlier review.
- 2 completions per head. Only clean reviews get one, because a review with findings gets a new round after the author's push.
- `missing` (a role that ended without its report) is a passing cause. A usage limit is not.
- The db-lock grace of 10 seconds also applies to the spec runs of the agent roles.

## Docs

- README: after the restart paragraph, the kill escalation, the run token, the move to a new pane, and the completion with its cap.
- docs/DECISIONS.md, at the end of `## Operations`: "A cut shell command is killed, and a review that missed a reviewer is completed".
  It gives the evidence above and the rejected options: closing the pane first (SIGHUP, which codex outlives, leaves orphans), new herdr ownership APIs (the Codex design), and a full re-review.

## Part D: a restart that launchd cannot spawn heals itself

On 2026-10-28 at 17:29 a drain restart stopped the daemon, and launchd then failed every spawn of the rebuilt binary with exit 78 (EX_CONFIG).
The binary wrote nothing, `magnum config` with the same binary passed, and the job showed "needs LWCR update".
`daemon-restart` printed only a hint ("then `magnum daemon-restart` again (or `magnum install` to reload the job)"), and the daemon stayed down for about 5 minutes (stopped 17:29:57, started 17:35:04).
`magnum install --now`, which unloads and loads the job again, started the daemon at once.

The fix: if no new daemon runs after the restart and launchd reports a spawn failure, `daemon-restart` reloads the job itself, as `install` does, once.
It waits past launchd's throttle (10 seconds) before it decides, so the known false alarm "no new daemon was running after 5s" goes away too.
If the reload also fails, it says so with launchd's exit code, and the daemon is down, as today.

Tests first: a fake launchd that reports `spawn scheduled` with exit 78 gets exactly one reload, and a running daemon after it ends the restart with success. A fake throttle of 10 seconds gets no reload.

## Build order

1. Parts A and B, one builder.
2. Part C, a second builder, after A and B land. Part D goes with Part C. If Part D is ready first, it goes alone.

## Risks

- A process id can be reused between the check and the signal. The re-read before each signal makes that window small.
- A sandbox child that left the session before the tree capture can touch the databases after the lock ends.
- A completion adds one more review on GitHub.
- Runs that started before the run token exist go through the "anything else" path, with a move to a new pane.
