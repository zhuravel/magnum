# Factory liaison: one agent per task as the operator's front door

Status: design, approved direction on 2026-11-06. It extends docs/superpowers/specs/2026-11-03-factory-design.md (the factory) and builds on milestone 1a.

## Intent

Today the operator meets a factory task on cards in the factory's screen: a question card, a ready card, a task card. A card is one-shot. Questions such as "what happens if I pick 2?" or "why does the critic object?" have no place on it. The only way to talk is to step into an agent's pane with `o`, and that goes around the factory's records.

The operator wants one Claude agent per task, the liaison: the link between the operator and the task's agents. It is the front door of the task. It gathers the task's context, asks the operator the questions, explains decisions and their effects, and takes notes for the factory's improvement. It uses the tools of Claude Code: AskUserQuestion with options, tables, screenshots. So the factory needs less UI of its own.

## Goals

- Every question and decision of a task reaches the operator through its liaison, in chat.
- The operator can ask about anything in the task: a finding, a stage, a file, the effect of an answer.
- The liaison acts on the operator's word in chat: it answers the questions, sends the task back, pushes it, or drops it.
- It writes notes that the improvement loop (`/magnum-improve`) reads.
- The factory keeps its records whole: every answer and decision lands in the registry, and `magnum factory stats` still measures the operator's time.

## Non-goals and anti-goals

- The liaison does not run the task. The engine keeps the order of the stages, the checks, the recovery after a restart and the push itself.
- It never merges, never runs a migration and never touches production or a shared database. These hard lines stay as in the factory's design.
- Anti-goal: the operator types more, or spends more time per task, than with the cards. Then the idea failed, however good the chat feels.
- Anti-goal: the liaison interrupts. It speaks only in two cases: the task needs the operator, or the operator opens it.
- Anti-goal: chat with the liaison freezes the task's other agents.

## Constraints

- The operator accepts more risk for convenience in the factory: the repositories are private and the colleagues are trusted. So the liaison pushes or drops on the operator's word in chat, with no extra step to approve it. The binding of a push to the head, the report and the diff (spec section 6) stays in the engine.
- The machine is a MacBook under memory pressure. A liaison that waits parks, and its conversation resumes.
- A daemon restart, an account switch or a compaction does not lose the task: its state lives in the registry, not in the agent's memory.

## Approaches considered

- A. A voice on top of the engine. The engine runs the stages in order. The liaison is the interface, and it steers through the factory's commands.
- B. The task's lead. The liaison runs the task. It starts the agents, prompts them and calls the factory's helpers as tools. The engine shrinks to a toolbox and bookkeeping.
- Chosen: A with steering, called "front door, engine leads". Two independent reviews, one by Claude and one by Codex, reached it on their own.

The reasons:
- Every stall of the first live run (2026-11-06) was infrastructure: the daemon's PATH, broken copies, herdr's resting focus, macOS privacy prompts, a check command's syntax, the locale, the review queue. A leading agent hits all of them too.
- The order of the stages is already in the configuration file (`kind.Stages` in config.defaults.toml). A new order needs no agent.
- Restart recovery, the prompt ledger and the push binding live in the engine's code and the registry. An agent's memory does not survive restarts and account switches.

B comes back only on evidence. For example, more than one task in three needs a new order or an ad hoc agent. The stage list and the verbs cannot express these.

## How it works

1. The liaison. When a task starts, the factory daemon starts one Claude session for it, the role `fx-liaison`. Its working directory is the task's folder in the data directory, so it lives on after a push or a drop frees the copy. It reads the task's folder: the ticket, the findings, the critic's notes, the questions, the report, the proof, the ready card. It reads the copy too, but it changes nothing there: no git, no database, no server. Its prompt is `prompts/factory-liaison.md`.
2. Its own record. The liaison has its own owner row in the factory's agents store, apart from the task's row. Chat with it never sets the task's `human_active_at`. So the writer, the copy and the checks do not wait while the operator talks. A test protects this.
3. The front door. When the task needs the operator (questions, the ready card, a wait that names "you"), the factory prompts the liaison with what is new. The liaison asks the operator with AskUserQuestion, with its recommendation first. The factory's alert and its home row point to the liaison's pane, and `o` steps in.
4. The steering verbs. The liaison acts through the factory's CLI, never through other panes:
   - `magnum factory answer` records the answers.
   - `magnum factory decide` pushes, sends back or drops, on the operator's word.
   - `magnum factory retry` lets a waiting task go on.
   Each call carries `--surface liaison` and `--shown <time>`, the moment the liaison asked. Today the surface is fixed to "cli" in internal/cli/factory_answer.go and factory_decide.go, and `--shown` is new. A decision records the diff as read only when the operator says that they read it. Otherwise it records an unread diff, as the card's "Push without reading the diff?" does.
5. Dialogs. magnum's agent runtime answers trust dialogs and says No to permission prompts. It leaves the liaison's AskUserQuestion dialogs to the operator, and it tells the operator that the task waits on them.
6. Notes. The liaison keeps `notes.md` in the task's folder: what confused the operator, what the factory asked needlessly, what it got wrong. The improvement loop reads these notes as evidence. A later milestone lets the improvement loop talk to a live liaison.
7. Measurement. The liaison offers each choice as AskUserQuestion options, so a pick is an answer, not a typed message. The stats count an answer or a decision with the surface `liaison` as from a card. The operator's typed chat counts as typed messages, classified as today. The time on a decision counts from `--shown` to the recorded answer.
8. The cards stay. The factory's screen keeps its cards as a fallback, and they show the same questions.

## The first slice

Prove the idea with the least change, then make it the default:

1. `magnum factory talk <task>` sends a request, and the factory daemon starts the liaison for that task, on its own owner row, with the prompt and the folder above. The daemon watches, parks and recovers it as its other agents. The stage machine does not change: the new request kind, the owner row, `--surface liaison` and `--shown` are the only changes in code. The liaison writes `notes.md`. Nothing points to it yet.
2. Run three tasks through it. Compare `magnum factory stats` with the first live task, which used the cards. Two numbers decide: the step starts (the target is zero) and the operator's time per task. The operator's questions in chat are reported but not judged.
3. If the numbers hold or improve, the liaison becomes the front door: it starts with each task, the alerts point to it, and the factory prompts it on each new decision.

## Later, on evidence

- A verb that runs a stage again with a hint, or skips an optional stage. Today only a send back rewinds a task (to coding), and the configuration has no optional stage. It is a change in the engine and the ledger, so it waits until tasks show the need.
- The improvement loop talks to a live liaison.

## Calls made in the design (check these)

- The liaison's working directory is the task's folder, not the copy, and it changes nothing in the copy.
- The factory daemon starts it, through a request: `talk` needs the daemon, as the other factory commands do.
- `--shown` and the read diff come from the liaison, as described in step 4.
- The pass test uses the step starts and the operator's time, not the count of typed messages.
- The verb that runs a stage again waits for evidence.

## Left to the builder

- The prompt's words, within the goals above.
- The pane's place: a tab in the task's herdr workspace, or a workspace of its own in the factory's session.
- How long an idle liaison waits before it parks.
- The name of the new verb and its arguments.
- How the factory hands the liaison what is new: a prompt per decision, or a file. With a file, the liaison reads it at each prompt.
