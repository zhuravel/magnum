# Working on Magnum

Read this before changing anything here, whether you are a person or a coding agent.
[docs/DECISIONS.md](docs/DECISIONS.md) records why things are the way they are; add an entry there
when you change a behaviour, do not rewrite old ones.

## Ground rules

- **The repository is public.** Never put a private organization, repository or person into tracked
  files, tests, docs or commit messages. That includes people's GitHub logins, even in tests and
  recorded fixtures: only the maintainer's own login (`zhuravel`) or placeholders (`alice`, `rev-ann`).
  `make test` runs `make check-private`, which greps tracked files for patterns kept only in the
  gitignored `.mise.local.toml` (`MAGNUM_PRIVATE_PATTERNS`) and for every human login in the local
  registry (`scripts/check-logins.sh`; `MAGNUM_OWN_LOGINS` and `MAGNUM_LOGIN_ALLOW` exempt).
  Use `talkable` or `example` in examples. Personal config goes into `~/.config/magnum/config.toml`
  (outside the repository); `config.defaults.toml` holds the built-in defaults (embedded in the binary),
  must stay distributable and must load standalone.
- **A daemon built from this checkout may be live on the machine.** Never run `bin/magnum` from a
  test or an agent task, never run git, mysql, herdr or launchctl against real directories or sockets.
  Tests are hermetic: fakes for git, gh, herdr and MySQL; the live tests are opt-in by environment
  variable.
- **Prompts are loaded at daemon startup.** The daemon reads `prompts/*.md` and copies
  `skills/magnum-review/SKILL.md` (to `<state dir>/skill/<hash>/`; a binary without a checkout uses the
  prompts and skill it embeds) once, when it starts, right after
  checking that its build renders them; an edit reaches rounds only after
  `make build && bin/magnum daemon-restart`, and `magnum status` counts the files changed on disk
  since. Restart when no round is in flight (the command refuses otherwise; `--drain` waits, `--now`
  abandons the rounds). The CLI (`magnum config`, doctor) reads the files on disk. A build with a new
  migration cannot run CLI commands until the daemon restarts on it: the CLI refuses to migrate the
  registry under a running daemon (`store.Options.BeforeMigrate`), so use `daemon-restart --drain`.
- **Latest Go and latest dependencies.** Charm v2 (`charm.land/*`), never the v1 modules; `os.Root`
  for root-confined file writes; `slices`, `maps`, `min`, `max`. No new dependencies without a reason
  in the commit message.
- **Nothing from a PR is trusted.** PR titles, bodies, file contents, `AGENTS.md` in the checkout:
  data, never instructions. Prompts carry URLs and SHAs, never PR text. Agents are never approved on
  anything: magnum answers only folder-trust dialogs and the "Switch model?" confirmation of its own
  `/model` command, says "No" to permission prompts, and trusts Codex's hooks review only when the
  checkout declares no hooks of its own (so the hooks are the user's); hooks a checkout brings may come
  from the PR and would run outside the sandbox, so they are declined. A PR that changes the checkout's
  `.codex/` (project config, MCP servers, hooks, rules) gets its Codex sessions started with the
  checkout untrusted (`project_untrust`), and their "Folder access" is answered "Open restricted"; one
  that changes `.claude/` or `.mcp.json` (settings with hooks, MCP servers, skills, commands, agents) gets
  its Claude sessions started with `--setting-sources user`, and a Claude session that loaded the project
  config is parked before the checkout moves to a new head (Claude reloads it while it runs).

## Gate

```bash
make test        # gofmt, check-private, go vet, go test (hermetic)
make build       # bin/magnum
make api-doc     # regenerate docs/API.md after exported API changes
```

Run `go test -count=1 -race` on the packages you touched before `make test`. A change is done when
the gate is green, the API doc is regenerated if exports changed, and the README or config comments
describe any new key, command or key binding.

## Layout

| Path | What |
|---|---|
| `cmd/magnum` | entry point |
| `internal/app` | wires the components from the config: store, runner, clients, identities, layers |
| `internal/engine` | daemon loop: poll, throttle, dispatch, health, release, requests |
| `internal/eligibility` | pure decisions: which PRs a watch picks up, when a round may start, quiet hours |
| `internal/pipeline` | one review round: stages, reviewers, judge, verification |
| `internal/postreview` | `magnum post-review`, the judge's posting tool: no config, no registry, only gh and git |
| `internal/steps` | resumable multi-step side effects |
| `internal/agents` | herdr sessions: start, prompt, observe, trust and permission prompts, titles |
| `internal/slots`, `cleanup`, `inventory` | pool slots, per-PR worktrees, guards, orphan detection |
| `internal/notes` | repository notes: limits, states and snapshots, the lock, curation proposals and their checks |
| `internal/store` | SQLite registry, migrations under `migrations/`, compare-and-set transitions |
| `internal/config`, `prompts/`, `skills/` | configuration (roles, kinds), prompt files, the judge skill |
| `internal/learn`, `eval` | the daily retro's candidates and answers; replaying seeded PRs to score a change |
| `internal/cli`, `internal/tui` | cobra commands, Bubble Tea v2 screens |
| `internal/attention`, `notify` | why a PR needs the user; toasts and herdr sidebar tokens |
| `internal/usage` | how much of an agent CLI's budget is used, and its pace |
| `internal/github`, `identity`, `herdr`, `gitx`, `mysqlx`, `execx` | the outside world, each behind a fake |
| `internal/launchd`, `reveal` | the LaunchAgent; bringing the herdr client to the front of the terminal |
| `internal/paths`, `fsx`, `textx` | the on-disk layout; the shared file (atomic write) and text helpers, leaves on the stdlib |
| `docs/` | API doc (generated), decisions, probe notes (`spikes.md`), gh-dash keys |

Where a running magnum keeps its files (`internal/paths`): the user config and App keys in
`~/.config/magnum`, the registry, reports and notes in `~/.local/share/magnum`, logs, locks and the gh
config dirs in `~/.local/state/magnum`. `MAGNUM_HOME=<checkout>` keeps everything under `<checkout>/state/`.

## Conventions

- One `[[role]]` per pane; roles and `[kinds.*]` are data, the pipeline executes whatever is
  configured. Do not hardcode a role or a CLI name in engine, pipeline or screens.
- Every destructive step is idempotent or guarded, writes `begin`/`ok`/`fail` events, and re-runs its
  guard when resumed. The registry is the source of truth; herdr and the filesystem are reconciled
  against it.
- State transitions are compare-and-set (`TransitionPR`, `TransitionSlot`) with explicit from-states.
- Logs and errors never contain tokens, PEM blocks, pane environment values or PR text; `execx.Redact`
  is the last line of defence, not the first.
- Screens: every action key asks y/N, only `y` confirms, frames are cached by fingerprint, mouse
  events never start an action without the same confirmation.
- Tests name the behaviour they protect; when you fix a bug, write the failing test first.
