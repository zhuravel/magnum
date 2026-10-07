# Landing and restarting

You land every agent's work yourself, one commit range at a time, on `master` in the main checkout.

## Before landing

1. Read the agent's report and the diff (`git diff master <branch>`). Check the decisions it made on its own and the
   tests it names. A report is a claim: open the files behind anything important.
2. Make sure that the main checkout has no uncommitted changes. Another session may edit files there (README, for
   example): ask it to commit, or stash only its file, fast-forward, and pop, then tell it you are done.

## Land

`scripts/land.sh <branch-or-sha>` fast-forwards master when it can, and otherwise cherry-picks the commits. It
dry-runs every new migration on a copy of the registry, runs the gate, pushes, and builds `bin/magnum` unless the
range adds a migration. It stops at a conflict: resolve it (DECISIONS.md: keep both entries; the config comments:
keep both sides' keys; SKILL.md: keep both rules and raise its size cap by the minimum), run
`git cherry-pick --continue`, and run the script again.

After landing, tell every running agent that master moved: the new sha, the files the landed work touched, and the
migration numbers now taken.

## Restart

The daemon reads prompts and the review skill when it starts, so landed work is live only after a restart.

- No new migration: `scripts/restart.sh` builds and restarts the daemon when no round is in flight.
- A new migration: `scripts/restart.sh --migration` pauses automation, waits for the rounds in flight, builds,
  restarts and resumes. A binary with a newer schema cannot run CLI commands (the board included) until the daemon
  restarts on it, so the build happens only right before the restart.

Run either in the background and check its output when it finishes. Then make sure that `magnum status` shows no
pauses and no build mismatch, and read the warn and error lines of the daemon log since the restart.

## Run log

Append one entry to `~/.local/share/magnum/improve/runs.md` per run:

```md
## <run id, e.g. 2026-10-07T08:30>

- since: <the since this run used>
- evidence: <rounds, posted reviews, misses, warn/error kinds: the headline numbers>
- landed: <sha: one line each>
- live since: <restart time>
- waiting for the operator: <item: the question>
- parked: <item: why, and what would justify it>
- next run: <what to look at first, and the measurements to read>
```

Keep the run directory (`~/.local/share/magnum/improve/<run id>/`: evidence, plan, specs). It stays on this machine;
never copy it into the repository.
