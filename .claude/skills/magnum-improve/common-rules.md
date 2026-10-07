# Rules for implementer agents

- You work only in your isolated git worktree of magnum (Go). Read AGENTS.md first; it is binding.
- Tests first. For a bug, write the failing test before the fix and name it after the behavior it protects.
  Fix the root cause.
- Gate: `go test -count=1 -race` on the packages you touched, then `mise exec -- make test`, and `make api-doc` when
  exported names change. The worktree has no `.mise.local.toml`. To run `make check-private` with the operator's
  patterns, load them through `mise -C <main checkout> exec`; never print them or copy the file.
- Commit on your worktree branch in the style of `git log`: one paragraph that says what changed and why, ending
  with "No new dependencies." when true. No attribution lines, no private names. Do not push.
- Docs: one `docs/DECISIONS.md` entry per changed behavior (append; never rewrite an old entry), and README or config
  comments for every new key, command or key binding.
- A new migration takes the next free number on master when you commit.
- Never run `bin/magnum`, herdr, launchctl, mysql, or git against any directory but your worktree. Never start a real
  agent session (codex, claude); `--help` is fine.
- Temporary files go only in `<run-dir>/tmp/<your-slug>/`. Delete them before you finish. No `.bak` files.
- No real GitHub logins other than the maintainer's own (`zhuravel`) in tests or docs: use `alice`, `rev-ann` and
  `bob-rev`, and `talkable` or `example` for organizations. Never a private organization or repository name.
- Stop and report when the code contradicts the spec, or when a fix needs a design decision the spec does not
  settle.
- Report back: the commit SHA and branch, the files changed, per item what you did (file:line), the new tests, the
  gate's last lines, and anything you created outside the worktree.
