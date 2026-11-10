# M0 probes

Results of the probes that settle unverified herdr / Codex / Claude behavior. Each entry: question,
how it was probed, answer, and the config default it sets.

## 2026-10-02 — herdr 0.9.3 / Codex 0.160.0 probes (scratch workspace in talkable.repo12, closed afterwards)

1. **Workspace env reaches the shell.** `herdr workspace create --cwd … --env MAGNUM_CANARY=probe1 --env WT_BRANCH=repo12`
   → `printenv` in the pane printed both after oh-my-zsh + mise init. `mise env` does not contain
   `WT_BRANCH` (it is pane env, not mise env), so magnum also writes it into the slot's `.mise.local.toml`.
   Default: inject slot/identity env with `--env`; no `tmp/.worktree-env.sh` sourcing needed.
2. **`herdr agent start --kind codex` types `codex` into zsh.** With no args after `--`, the foreground argv was
   `codex --dangerously-bypass-approvals-and-sandbox -c model_reasoning_effort=xhigh -c plan_mode_reasoning_effort=xhigh
   -c model_reasoning_summary_format=experimental --search` — i.e. the maintainer's zsh `codex` function supplied the flags.
   Default: `[codex] wrapper_mode = true`, `args = []`; magnum passes only extra args (`resume <uuid>`). Same for
   `claude` (`--dangerously-skip-permissions` comes from the wrapper). `doctor` re-checks with `zsh -ic 'whence -w codex'`.
3. **Session id appears after the first turn.** `pane get` → `agent_session` was `null` right after start and
   `{"kind":"id","source":"herdr:codex","value":"01a0fe66-…"}` after one prompt. Default: record the id after the first
   prompt is acked, re-read on every tick until non-null.
4. **Resume through the wrapper works.** `herdr agent start … -- resume <uuid>` → argv `codex <wrapper flags> resume <uuid>`,
   agent idle + `interactive_ready`, `agent_session.value` equal to the resumed uuid, pane title restored
   ("Reply PROBE OK | talkable.repo12"). A transient `git index-pack` was the foreground process right after start:
   Codex clones its git marketplaces on every launch (stagger starts).
5. **Quit sequence.** Two `ctrl+c` via `herdr agent send-keys` (1 s apart) return the pane to the shell.
6. **`agent prompt --wait` returns** with `result`/`error` both null on success for a one-line reply; multiline
   prompt text (two lines) was pasted intact.
7. **gh honors `GH_CONFIG_DIR`** with a plain `oauth_token` in `hosts.yml` (`gh api user` → zhuravel,
   `gh api repos/talkable/talkable` OK). the maintainer's own gh keeps its token in the keyring; the per-identity dirs use
   hosts.yml.
8. **herdr processes.** `pgrep -x herdr` finds nothing, but `ps -axo pid=,tty=,args=` shows the server
   (`/opt/homebrew/bin/herdr server`, no tty) and the TUI client (`herdr`, `ttys000`). The reveal code must use `ps`
   args filtering, not `pgrep`. iTerm2 listed only `/dev/ttys050`, so the client runs in another terminal app
   (see probe 9 below).
9. **Where the herdr client lives.** The TUI client (pid on `ttys000`) is a child of `Muxy Beta` (`muxy-app → muxy-server →
   zsh → herdr`), not of iTerm2 (iTerm2 had one unrelated session, `ttys050`). `magnum open` therefore cannot focus an
   existing iTerm2 tab for it; it follows the Raycast extension's fallback and opens a NEW client in an iTerm2 tab
   (`create tab with default profile` + `write text "herdr --session default"`), then `herdr agent focus`. Muxy is
   treated as "generic": app activation only (`open -a "Muxy Beta"`).

## 2026-10-03 — first live round on the sandbox PR (a private throwaway repo under the user's own account)

10. **Trust dialogs block fresh directories.** Codex shows "Folder access … Trust this folder? › 1. Trust and continue"
    (trust applies to the repository root, e.g. `~/Projects/<repo>`, persisted as
    `[projects."<root>"] trust_level = "trusted"` in `~/.codex/config.toml`); Claude shows "Accessing workspace … ❯ No, exit /
    Yes, I trust this folder" (persisted as `projects["<dir>"].hasTrustDialogAccepted = true` in `~/.claude.json`).
    Every `agent.prompt` then fails with `agent_blocked`. Fix: `agents.EnsureTrust` writes both entries before
    `agent.start`; a narrow fallback answers exactly these two dialogs.
11. **Clone names differ from repo names.** some clones live at `~/Projects/<owner>-<name>` (or `~/Projects/<owner>` for <owner>/<owner>); `~/Projects/widgets` exists but is not a git repo. Fix: `gitx.FindClone` matches by
    `origin` URL across `<name>`, `<owner>-<name>` and a scan of the clone root; per-PR worktrees go next to the found clone.
12. **Pool provisioning is fast here:** `bin/worktree-setup` for `talkable.review1` took 1m10s (bundle/pnpm caches warm);
    8 databases created; slot `free`.
13. **`codex review` output includes startup noise** (config warnings, an unauthenticated GitHub MCP error from the user's
    Codex plugins, exec traces) before the findings; the judge skill treats the report as data and copes, but trimming to
    the findings section is a possible improvement.

## 2026-10-03 — App REST calls through `gh api` (gh 2.102.0, Little Snitch on the user's Mac)

14. **net/http is blocked, gh is not.** Every direct call magnum made for the App (`POST /app/installations/{id}/access_tokens`,
    `GET /app`, `GET /app/installations/{id}`, `GET /installation/repositories`) failed with `net/http: TLS handshake timeout`
    or `context deadline exceeded while awaiting headers`, while `gh api` and curl answered at once. Little Snitch blocks
    outbound HTTPS from unknown unsigned binaries, and every rebuild changes magnum's binary, so allow-listing it is fragile.
    Fix: `identity.GhTransport`, an `http.RoundTripper` that runs each request as
    `gh api --method M --include --hostname github.com <path?query> -H "Name: value"... [--input -]`. It is the default
    (`[github] transport = "gh"`); `transport = "direct"` restores Go net/http.
15. **An explicit Authorization header wins over gh's own token.** `gh api -H "Authorization: Bearer invalid" /app --include`
    and the same call to `/user` both answered `401 Bad credentials`, while `gh api /user` without `-H` answered 200 as the
    user. gh adds its stored token only when the request has no Authorization header. gh still refuses to start without a
    login of its own: with `GH_TOKEN=` and an empty `GH_CONFIG_DIR` it printed "To get started with GitHub CLI, please run:
    gh auth login" and exited 4 before sending anything. So the transport keeps the user's gh auth in the environment and
    relies on the header override.
16. **`--include` output shape.** The status line ends in LF (`HTTP/2.0 401 Unauthorized\n`), each header line in CRLF
    (`Name: value\r\n`, sorted, repeated values joined with `, `), then a CRLF blank line and the raw body. On a non-2xx
    answer gh still prints all of that to stdout, writes `gh: Bad credentials (HTTP 401)` to stderr and exits 1, so the
    transport parses stdout whatever the exit code and only treats an unparseable stdout as a transport error. The fixture
    in `internal/identity/ghtransport_test.go` is this probe's output. The JWT and installation token travel in argv for
    the duration of the call; execx log lines show `Authorization: <redacted>`.

## 2026-10-03 — agent CLI flags behind the `[kinds.*]` defaults (droid 0.232.0, omp 18.4.12, codex-cli 0.160.0, Claude Code)

17. **droid** (`droid --help`): `-r, --resume [sessionId]` resumes a session; interactive mode has no model, effort or
    session-name flag (`-m` exists only on `droid exec`). On this Mac `droid` is a zsh alias (`droid --auto high`), so
    `wrapper = "auto"` detects a wrapper. Default: `resume = ["--resume", "{session}"]`, nothing else.
18. **omp** (`omp --help`): `-r, --resume=<value>` (id prefix, path or picker), `--model=<value>` (fuzzy match),
    `--thinking=<off|minimal|low|medium|high|xhigh|max|auto>`; no session-name flag. Also a zsh alias here
    (`omp --auto-approve --approval-mode=yolo`). Default: `resume = ["--resume={session}"]`, `model = ["--model={model}"]`,
    `effort = ["--thinking={effort}"]`.
19. **codex resume takes the global flags**: `codex resume --help` lists `-c, --config <key=value>` and `-m, --model`, so
    `resume <id> -c model_reasoning_effort=<effort>` works. The codex-judge role's `effort = "xhigh"` therefore adds
    `-c model_reasoning_effort=xhigh` to the launch args, the same value the zsh `codex` function already passes.
20. **claude** has `--model`, `--effort` and `-n, --name`; the claude kind sets `model` and `name` but no `effort` args,
    because claude-review hands its effort to `/code-review` in the prompt (an `--effort` flag would change the
    session's effort, which magnum never did).

## Probe 21: `/rename` on a fresh idle Codex thread (2026-10-03)

Started `codex` in a trusted directory through `herdr agent start`, waited for idle, typed
`/rename PR #0 probe - magnum` with `herdr pane run` before any prompt. The pane title became
`PR #0 probe - magnum | sandbox` and the composer footer showed the thread name, so a fresh
thread can be named before its first turn. Codex still titles the thread itself a few seconds into the
first turn, so the namer also repeats the rename while magnum's turn works. `codex review` (the
non-interactive subcommand) prints only its verdict on stdout and the whole transcript on stderr
(172 bytes vs 7.5 KB for a one-line change), so shell roles with `capture = "stdout"` tee stdout only.

## 2026-11-04 — the factory's local round with the unmodified judge (a static read; the live run is the operator's)

The factory's design (section 6.5) asks for one hand-seeded local round with the unmodified judge before the
local review is built, to see whether the judge survives the missing PR. No agent task may run a live round
(AGENTS.md), so this is a static read of what the judge prompts, the skill, the reviewer prompts and
`post-review` take from the PR, and the local block is shaped from it. The live run is the operator's step at
the factory's first run: `TestLiveLocalRoundWithTheUnmodifiedJudge` in `internal/evalrun`, with
`MAGNUM_LIVE_ENGINE=1` and `MAGNUM_LIVE_LOCAL=<checkout>,<owner/name>,<base sha>,<head sha>` (the checkout on
the change's branch at its head); it runs the round as a replay runs one, under the live config, and with
`MAGNUM_LIVE_LOCAL_BLOCK=1` a second round as the factory runs it. Record what it shows here.

What a round of a synthetic PR number (no PR on GitHub) meets, by the static read:

1. **The judge prompts.** `judge-initial.md` and `judge-own-pass.md` say "Review <url>" and carry the PR's
   `url`, `number` and `pr` fields; the blind block skips the `state == open` check and takes `git diff
   <base_sha>..<head_sha>` as the boundary, but still says "Read the PR description and the commits up to
   `<head_sha>` only", which the skill reads from GitHub. Nothing else in the prompts calls GitHub.
2. **The skill.** Section 1.3 opens with `gh api repos/{owner}/{repo}/pulls/{number}` (a 404 here, which a
   judge may write as `blocked` or `error`: "PR discovery ... blocks the review"); section 2 reads the PR's
   description, commits, review comments and replies; section 7 reads `base.repo.private` for a nearby
   security finding and checks `gh api user` before its first GitHub write. A dry run makes no GitHub write.
3. **claude-review.** `claude-review.md` starts with `/code-review <url> <effort>`: the code-review skill
   reads the PR from its URL, which does not exist, so its report may be missing or empty.
   `claude-simplify.md` names the URL in its first sentence but reviews the local diff; codex-review is
   `codex review --base <base_sha>` on the local diff.
4. **post-review.** A blind dry run's line carries `--dry-run --local-base <base_sha>` when the round knows
   the base: it checks the anchors against `git diff <base> <head>` in the checkout and reads nothing from
   GitHub. Without a base it would read the PR's files from GitHub.
5. **The round.** A blind dry run reads nothing of the PR from GitHub (no related PRs, failing checks,
   evidence map, earlier reviews or threads), but it reads the Jira issue a key in the title names, and an
   App identity's pane environment mints its token from GitHub.

So the local block: `{{if .Local}}` in judge-shared.md's `blind` block (no `gh` call, the PR checks of
sections 1 and 7 skipped, the local diff as the boundary, the title and description from `description_file`
and the ticket from `ticket`, the skill's "Local round") and `local: true` with `description_file` among the
`<magnum>` fields; a "Local round" section in the skill (`base.repo.private` counts as false, a product
question marked `"question": true` in the provenance); `claude-review-local.md` without `/code-review <url>`,
as the review candidate's `claude-review.md`; and in the round, the factory's base always (`--local-base`),
its `ticket.md` in place of a Jira read, and a gh identity. The live rounds render as before (the judge
goldens are unchanged).

Open for the live run: whether the unmodified judge ends `blocked` or `error` on the 404 or reviews the local
diff anyway; whether `claude-review-local.md`, without the code-review skill, finds what `/code-review` finds
(or whether `/code-review <effort>` with a local target reviews the branch's diff); whether claude-simplify's
URL leads it to a `gh` call; and a round's minutes and tokens.

## 2026-11-10 — the gh transport over a gh config dir of each call's own (gh 2.103.3, a local socket only)

A scratch `GH_CONFIG_DIR` held one config.yml with `http_unix_socket: <a local socket>`, which a Python server
answered with 200; `GH_TOKEN=magnum-no-login`, every other `GH_*` and `GITHUB_*` unset, and `HTTPS_PROXY` at a
closed local port, so nothing could reach GitHub. `gh api --method GET --include --hostname github.com app -H
"Authorization: Bearer fake.jwt.value"` exited 0, and the server saw `Host: api.github.com` and
`Authorization: Bearer fake.jwt.value`, not the placeholder.

1. **An http_unix_socket in the config dir gh reads takes every call.** A pane that writes it into a dir the
   daemon's gh reads (the operator's `~/.config/gh`, or the App's own) receives the request, its Authorization
   header too, and answers it as GitHub. So the App's own REST calls (`identity.GhTransport`) read a gh config
   dir made fresh for each call, and removed after it.
2. **A placeholder GH_TOKEN starts gh, and the explicit header wins over it.** That dir has no login, and gh
   refuses to start without one (probe 15), so a JWT call's `GH_TOKEN` is a placeholder, never sent: probe 15's
   reliance on the user's gh login is gone.
