# Control commands: pause, drain and resume for both daemons

Status: design, the operator's decisions of 2026-11-12. Phase 1 is described in full and built now. Phase 2 is
described in short and comes later.

## Intent

magnum runs two daemons: the review daemon (`magnum daemon`) and the factory daemon (`magnum factory daemon`).
Before a reboot of the Mac the operator wants one command that stops new work on both, waits until nothing is in
flight, and says that a reboot is safe. After the reboot the work goes on by itself.

Today that takes two commands that do not do it:

- `magnum daemon-restart --drain` and `magnum factory install --drain` hold new work, but only for the restart that
  follows. Their drain is tied to the command's pid and ends with the next daemon start.
- The review drain decides "nothing in flight" from registry rows alone. The daemon never acknowledges it, so a
  tick that started earlier can still start a round.
- The factory drain counts its own off-loop work (work.json) but no ordinary agent turn. A reboot kills herdr too,
  so a turn in flight is lost: the attempt spends one of its resumes, or a prompt of unknown delivery waits for
  `let-go`.
- `magnum pause` holds the review daemon alone. The factory has no pause.

Two independent reviews (Claude's and Codex's, 2026-11-12) hold the inventory with file:line references.

## Decisions (the operator, 2026-11-12)

- One set of verbs for both daemons. With no flag a command acts on both. `--review` or `--factory` narrows it.
  The factory is left out when its launchd job is not installed.
- A drain before a reboot ends by itself at the first daemon start after the Mac rebooted. `--keep` makes it last
  until `magnum resume`.
- Phase 2 adds the old names as hidden aliases. Phase 1 does not.

## Phase 1

### The three levels

- `pause`: no new automatic work. What the operator asks for still runs. It waits for nothing. It lasts until
  `resume`, `--for` or `--until`, across restarts and reboots.
- `drain`: nothing new starts, not even what the operator asks for. Control still works: status, resume, abort,
  stop. The command waits until both daemons are quiet, then each daemon parks its idle agents, and the command
  says "safe to reboot".
- `stop`: no daemon process. That is phase 2.

### `magnum pause [--review] [--factory] [--for D | --until T] [--reason t | words…]`

- Review: as today (kv `daemon.paused*`, through the daemon's request handler while it runs, written directly
  while none runs).
- Factory, new: the record is `pause.json` beside `drain.json` in the factory's state directory:
  `{generation, since, until, reason}`. Each tick reads it into a hold of the machine, `HoldPause`, which
  `holds.json` shows. The tick after its `until` lifts it and says so in an event.
- What the factory's pause holds: launches of a stage's roles (`mayLaunch`), claims of a copy (they precede a
  launch), new local review rounds, checks of a head and QA runs (`startsHeld`, which now includes the pause).
- What it lets through: the operator's own actions. `start` queues a task, `answer`, `retry` and `talk` run, and
  `decide push` runs: effects and talks read the narrower `drainsHeld`, which a pause is not part of. A role the
  runtime starts again outside a launch (a prompt's bring-back, the liaison) passes `MayLaunch`, which a pause is
  not part of either. A QA run whose walk has not begun is not cut by a pause.
- A launch held by the pause records the wait on its task (`pause: paused by you …`); a launch that goes through
  clears it, as for the machine's other holds.
- With no daemon running, the CLI writes both records directly and says that they apply at the daemon's start.

### `magnum drain [--review] [--factory] [--keep] [--timeout D] [--no-wait] [--reason t]`

#### The record

`{generation, since, until: "boot"|"resume", boot: <kern.boottime>, reason}` (`engine.RebootDrain`):

- review: the kv row `daemon.reboot_drain`;
- factory: `reboot-drain.json` beside `drain.json`.

It is never tied to the command's pid. The generation is the command's start in nanoseconds; both records of one
command carry the same one. The boot is `engine.BootID`: `sysctl -n kern.bootsessionuuid` (unique per boot) and `sysctl
-n kern.boottime`, read through a runner (the CLI's, and each daemon's at its start), so tests fake them. Another boot
is another session; without one, a boot time more than two minutes away (macOS moves kern.boottime when it steps the
clock after a wake). A boot nobody could read is unknown, and such a drain lasts until `resume`. A drain run again
while one holds keeps its start, its end and its reason unless a flag changes them.

#### Admission and acknowledgement

- Each daemon closes its admission from the moment the record is visible: every start gate reads it.
- Review: `draining()` includes the reboot drain, so everything a restart drain holds is held: automatic and
  forced rounds (`globalWait`, `stopping`), fix cycles and `magnum fix`, merges of the parent, whiteboards, the
  retro, curations, the skill edit and gate. The staging resolver takes no slot for a queued job (`take` waits,
  "the daemon drains for a reboot"); a job that took its slot runs to its end.
- Factory: `drainsHeld()` includes the reboot drain, so everything a restart drain holds is held (launches,
  effects, rounds, checks, claims, talks, QA runs). New prompts to agents that work already are held too: the
  runtime's `Send` asks `Deps.MayPrompt`, which the engine answers from the reboot drain. A restart drain still
  holds no prompt (DECISIONS 2026-11-07: those turns outlive a restart).
- Review, the start paths that did not read the drain: a staging deploy, an open's restore, a provision, a repair, an
  adoption, a cleanup and a release are refused with why and how to go on (a dry run, a cleanup's plan and an open of
  a session that is live already go on); the reconcile, the start's included, queues no slot work. A whiteboard
  checks the drain again once it holds its PR.
- Each daemon then writes an acknowledgement of that generation: review, the kv row `daemon.reboot_drain_ack`;
  factory, the `reboot_drain` field of work.json. It holds `{generation, pid, quiet, quiet_since, parked, work,
  left}`. The review daemon writes it at the end of each tick, once every start path of the tick ran, so what one
  of them started is in its work. The factory daemon writes it at the start of each tick, after `takeDrain`, and
  reads both files before its startup's recovery. Each writes the record again whenever it does not hold the
  acknowledgement (a write that failed, a row that went).
- A daemon that restarts invalidates its old acknowledgement: the pid no longer matches, and the start drops it.
  The drain applies again from the start, since every gate reads the record.

#### What each daemon waits for

- Review (`rebootDrainWork`): its reservations (rounds, opens, fix jobs, whiteboards, merges of the parent), a
  round's tail before its fix cycle or whiteboard, the notes curation and critique, the retro, the skill edit and
  gate, the heavy worker's jobs and the GitHub writes as the operator, the rounds the registry holds in flight,
  the requests answered off the tick, the staging deploys queued or running, and the staging resolver's jobs that
  took their slot or that its worker runs. A merge of the parent is a fix job in phase fixing, so it is a
  reservation; both reviews named it as uncovered, and a test holds a real one at its push. A session a dialog
  holds (herdr shows it blocked) is named with its remedy.
- Factory (`rebootDrainWork`): work.json's items (local review rounds, checks, claims, talks, QA runs; a backoff
  repair aside, as for a restart) and every agent run that is pending, submitted or working, the liaison's too
  (`runtime.Runtime.TurnsInFlight`, from the agents store). A run whose state the store cannot tell counts as
  working. A runtime that cannot list its turns counts as one that works.

#### Quiet, park, safe

- Once a daemon is quiet, it parks its idle agents, so that the reboot closes no pane in the middle of a turn.
  Review: each quiet check marks the live sessions `magnum restart-sessions --tool all` would restart now (one
  that becomes restartable later, an unpin, is marked then) and parks them through the same early park (pinned
  PRs left alone; a human typing in a pane waits out the cooldown). Factory: `Runtime.Park` of every task that is
  active, waiting, needs you or published with a live agent, once per generation, through `ParkForReboot`, which
  reports a liaison at work as busy. A busy refusal (an agent at work in herdr with no run row: you typed into its
  pane, a shell runs a build) is never left: the task is named as work and tried again at each tick. Any other
  failure, after three tries, leaves the task, said once and named in `left`.
- `parked` is true once the park is done: review, no restartable session is live and none shows a dialog;
  factory, every task's park went through or was left.
- The command says quiet only once both acknowledgements are of its generation, from the running daemon's pid,
  quiet and parked. A daemon that does not run is done only once its own store (the review registry's runs, the
  factory's agents store, read-only) shows no agent's turn in flight, since those turns outlive it in herdr, and,
  for the factory, once no record a crash left (work.json) names work, whose agents (a local review round's) no
  read reaches; a store it cannot stat or read, and a daemon whose pidfile it cannot read, are never certified. A
  daemon that starts while the command waits has its build checked again, at each poll while it says no.
- While it waits it prints one line per daemon every 15 seconds naming what still runs, and wakes each daemon at
  most once a minute (the review daemon's tick polls GitHub).
- At the end it prints `quiet: safe to reboot. After the reboot, the daemons' first start ends the drain. magnum
  resume ends it now.` and exits 0. With `--keep`: `quiet: safe to reboot. The drain lasts until magnum resume.`

#### How it ends

- At each daemon's first start whose boot time differs from the recorded one: the daemon deletes the record and
  its acknowledgement and says so in an event. A crash restart, an idle restart for a new build, a restart by
  command on the same boot keeps it. `--keep` (`until: "resume"`) makes it last until `resume`.
- Ctrl-C (any of the signals the command takes) or the timeout (default 3h30m) keeps the drain. The command prints
  what still runs, says that `magnum drain` waits again and that `magnum resume` allows new work, and exits 1.
  `--no-wait` sets the drain and returns at once.
- After one quiet hour with no reboot, each daemon toasts once that the drain still holds (review: a notify item
  keyed by the generation; factory: an alert of the factory).

#### Loud while it holds

`magnum status`, `magnum factory status`, the board's and the dashboard's title and the factory screen's blocker
line show one line per daemon with a pause or a drain, for example:

```
review   running · pid 4242 · paused by you until 15:30 (lunch): magnum resume
factory  running · pid 4300 · draining for a reboot since 14:02: fx-writer turn of PR-12360
```

A drain that holds says `draining since <time>: magnum resume ends it` on every one of those views. The JSON of
both status commands gains `daemons: [{name, running, pid, build, pause, drain}]`.

#### Restart drains

`daemon-restart --drain` and `factory install --drain` work as before, on their own records (`daemon.draining`,
`drain.json`). A restart drain and a reboot drain can both hold; neither lifts the other, and a daemon start ends
only the restart drain.

### `magnum resume [--review] [--factory]`, and `--tool <kind>|all`, `--watch <owner>` as today

- With neither `--tool` nor `--watch` it lifts the pause and the reboot drain on the daemons it names. On the review
  side it also lifts the infrastructure pause, as today. The review daemon unmarks the sessions the drain's park
  marked once the record is gone.
- It never lifts a tool's or a watch's hold without `--tool` or `--watch`, and never touches mutes, snoozes or pins.
  When another hold still stops work it says so with that hold's own remedy (a kind's pause, a watch's pause, a
  restart drain).
- A waiting `drain` command sees its record gone, says that `magnum resume` lifted the drain, and exits 0; a resume of
  one daemon alone ends it with exit 1, saying which still drains.

### Safety

- A daemon that does not know these records: each daemon of this build records that it knows them (review: the
  kv row `daemon.controls`, `{version, pid}`; factory: work.json's `controls`). The CLI checks the running daemon's
  record first. For a daemon that lacks it, it says "restart it on this build first" and claims no hold for it:
  `drain` needs it on both daemons, the factory's `pause` on the factory.
- A command that fails on one daemon reports each daemon, exits non-zero, and undoes only what it set, while the
  record is still its own: it lifts a record it wrote where none was, and puts back one it overwrote.
- Concurrent commands use the generation: a waiting `drain` whose record now carries another generation stops
  ("a newer `magnum drain` replaced this one") and never certifies it; a lift compares the generation first.

### What phase 1 does not do

- No hidden aliases and no `restart`, `stop`, `start`, `install --factory`, `uninstall --factory` or `kick
  --factory`: phase 2.
- A staging deploy in progress goes on with its steps; a resolver job queued during a drain waits for its slot and
  fails past its own bound (one hour), as when its slot is taken.

## Phase 2 (in short)

Follow Claude's proposal, sections 3 and 4:

- `magnum restart|stop|start [--review] [--factory] [--drain | --when-idle | --now] [--timeout D]`, `magnum install
  [--review] [--factory]`, `magnum uninstall [--review] [--factory] [--now]` and `magnum kick [--review]
  [--factory]`. `stop` boots the job out and disables it, so a login does not start it; `start` enables and boots it.
  A restart skips a daemon already on this build and config unless `--force`.
- The old names become hidden, deprecated aliases for one minor release (`daemon-restart`, `daemon-stop`,
  `factory install`, `factory uninstall`, `unapprove --resume`, `fix --off/--on`), each saying its replacement.
- One help group, "Daemons (review and factory)". Every doc, prompt, script and message that names an old command
  moves to the new one in the same landing; the drift test then rejects the old names.
- The per-PR words get one meaning each: abort, drop (`magnum factory drop`), stop and start for daemons only,
  pause and resume for daemons, kinds and owners.

## Tests

Hermetic: fakes for the daemons' pids and kicks, the factory's launchd job, the boot time (through the runner or a
function), `storetest` and `registrytest`.

- CLI: pause with no flag holds both daemons, `--review` leaves the factory alone, with no daemon both records are
  written; drain waits for both acknowledgements then prints "safe to reboot"; Ctrl-C and the timeout keep the
  drain and exit 1; resume lifts the pause and the drain and keeps a tool's hold, a watch's hold, mutes and pins;
  a waiting drain exits 0 after a resume; an older build gets "restart it on this build first"; a failure on one
  daemon is reported per daemon and lifts only what this command set; status shows one line per daemon in text and
  JSON, and the "draining since" line shows on the screens.
- Review engine: a round about to start as the drain begins does not start after the acknowledgement; the drain
  waits for a staging resolver job and a merge of the parent; once quiet the idle sessions are parked; the drain
  outlives its command, a start on a new boot ends it, a crash restart on the same boot keeps it, `--keep` keeps it
  until resume; a restart drain and a reboot drain do not lift each other.
- Factory: the pause holds launches, rounds, checks and QA, lets the operator's decisions act, survives a restart
  and ends at its `until`; the drain stays pending while a writer works and work.json is empty; it holds a new
  prompt; once quiet the idle agents are parked; a start on a new boot ends it.
