# Rules for analyst agents

You analyze magnum (a Go daemon that reviews GitHub PRs with AI agents in herdr panes; the repository and the master
sha are in your task). You are read-only:

- Never edit a file in the repository or under `~/.config/magnum`. Write only your report, at the path your task
  names (`<run-dir>/analysis/<lens>.md`), and scratch files under `<run-dir>/tmp/<your lens>/` (delete them at the
  end).
- Never run `bin/magnum`, herdr, mysql, launchctl, or git against real directories, except read-only git commands
  (log, show, diff, grep) in the repository. `go test` and `go vet` in the repository are allowed (they are hermetic).
  The registry may be read with `sqlite3 -readonly ~/.local/share/magnum/magnum.db` (read `.schema` first). `gh api`
  only for GET requests, at most 30 calls (the operator's token also serves the daemon).
- Read first: `AGENTS.md`, `docs/DECISIONS.md` (do not propose a rejected alternative without new evidence), the run's
  plan at `<run-dir>/plan.md` when there is one, and the run log `~/.local/share/magnum/improve/runs.md`, so you do not
  re-propose what is built, queued or rejected. Your task names what the operator declined; do not propose it again.
- Private names (organizations, repositories, people's logins) may appear in your report, which stays on this machine,
  but every proposed rule or test must be general.
- Do not start subagents unless your task says you may; helpers count toward the session's subagent limit.

## Return format

At most 10 items, ranked by impact over effort, under 1,200 words. For each item:

- a one-line title;
- the evidence (numbers, PR and round, file and line, quoted review line);
- the change, as the author, reviewer or operator would see it;
- class: `bug`, `simplify`, `improve` or `idea`;
- effort (S/M/L and the packages), usage impact, risk;
- how the next run measures it;
- whether the PR author would see the change (then the operator must approve it).

Then one line per thing you checked and found fine, so the next run does not check it again. Save the report to your
path and reply with its path and a 5-line summary.
