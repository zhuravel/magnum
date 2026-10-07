# Landing and restarting

You land every agent's work yourself, one commit range at a time, on `master` in the main checkout.

## Before landing

1. Read the agent's report and the diff (`git diff master <branch>`). Check the decisions it made on its own and the
   tests it names. A report is a claim: open the files behind anything important.
2. Make sure that the main checkout has no uncommitted changes. Another session may edit files there (README, for
   example): ask it to commit, or stash only its file, fast-forward, and pop, then tell it you are done.

## Land

`scripts/land.sh <branch-or-sha>` fast-forwards master when it can, and otherwise cherry-picks the commits. It
dry-runs every migration the registry lacks on a copy of it, runs the gate, pushes, and builds `bin/magnum` unless
the registry lacks a migration. Both sides append to `docs/DECISIONS.md`, so it conflicts there nearly every time:
when the commit only appends, land.sh keeps both sides by itself (master's file, then the commit's entries; read
them in the diff). Any other conflict stops, with DECISIONS.md already resolved and staged: resolve the rest (the
config comments: keep both sides' keys; SKILL.md: keep both rules and raise its size cap by the minimum; golden
files: resolve the files they render first, then `go test ./internal/agents -update`), `git add` it, run
`git cherry-pick --continue`, then `scripts/land.sh --continue <branch>`: it picks the rest of the range, skipping
the commits whose subject master already has (a resolved commit no longer matches its patch), then gates, pushes
and builds.

`scripts/selftest.sh` tests land.sh, restart.sh and eval-at.sh against throwaway repositories with fakes; run it
after changing a script.

After landing, tell every running agent that master moved: the new sha, the files the landed work touched, and the
migration numbers now taken.

## Restart

The daemon reads prompts and the review skill when it starts, so landed work is live only after a restart.

- No new migration: `scripts/restart.sh` builds and runs `magnum daemon-restart --drain`: no new round starts
  (requested ones included), the rounds in flight end, and the daemon restarts. Waiting for an idle moment without
  holding new rounds once took more than 30 minutes while rounds kept starting.
- A new migration: `scripts/restart.sh --migration` pauses automatic reviews with the running binary and waits for
  the rounds in flight, builds, then restarts with the new binary's drain (which also waits for a round a request
  started meanwhile) and resumes. A binary with a newer schema cannot run CLI commands (the board included) until
  the daemon restarts on it, so the build happens only right before the restart.

Both wait at most `--max-wait` (default `30m`; after it nothing is restarted) and print the rounds they wait for
every 5 minutes. Run either in the background and check its output when it finishes. Then make sure that `magnum status` shows no
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
