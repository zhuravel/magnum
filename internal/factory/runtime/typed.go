package runtime

// The turns typed into the factory's panes (spec section 11.1). Once every
// typedEvery, the observe pass reads the transcript of each conversation of
// a task that is live (or ended within typedSettle) whose transcript changed
// since the last read took it whole, once, through its newest pane's row (a
// conversation resumed in a new pane shares its transcript with its old
// rows: conversation): a live pane's, and one whose pane ended (parked,
// lost, closed: one last read, whether a read saw it live or not, kept for
// the next pass when it fails). It hands the engine every user
// turn the transcript holds that the factory did not send (EventTyped): no
// prompt of the task's ledger (by its hash), no after-deny prompt of the
// session's kind, no record of a slash command magnum types (/compact, the
// kinds' /model, /rename and exit commands: agents.MagnumCommands, which the
// baseline drops as well, so both sides count the same), no
// tool's result and no turn a tool or a notification began (Claude Code's
// own records of a command's output and of a compaction are no user turns
// at all: agents.ClaudeUserTurns). The proof of a turn's origin is herdr's,
// in the turn's own pane (the row whose pane held the conversation when the
// turn arrived): the agent there seen at work on no prompt of the factory's
// (agents.ObsHumanActive) within originWindow of the turn (OriginHerdr). A
// turn without it is OriginUnknown, and one younger than
// originWindow in a live pane waits for a later read, as herdr may not have
// shown the agent at work on it yet. What herdr showed is kept in memory: a
// restart forgets it, and a turn it would have proven is unknown. The pass
// reads a transcript's user turns once: Reconcile's proof of a delivery
// takes the same read (turnReads). The engine records each turn once (the
// registry keys a pane's turn by its role and time), so a restart, which
// forgets what was read, hands them again. Two cases lose a last read: a
// task that ended shortly before a daemon stop that lasts past typedSettle
// after its end is not read again when the daemon is back, and a last read
// that keeps failing past typedSettle after the task's end is dropped.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Turn is a user turn of a factory agent's transcript that the factory did
// not send: At when the CLI stamped it, Text as typed (the engine scrubs
// it), Before what the agent said in its two turns before it (as said,
// oldest first, blank-line apart: the engine scrubs it whole before the
// registry keeps its last 1 KiB), Stage the stage of the attempt the agent
// worked in, AgentState herdr.StatusWorking when a run of the factory's was
// in flight in the pane when the turn arrived (else herdr.StatusIdle), and
// Origin registry.OriginHerdr or registry.OriginUnknown.
type Turn struct {
	At         time.Time
	Text       string
	Before     string
	Stage      string
	AgentState string
	Origin     string
}

// typedEvery is how often the observe pass reads its agents' transcripts
// for the turns typed into their panes: the cadence of reconcileUnknown,
// so a pass that does both reads a transcript once.
const typedEvery = loginEvery

// originWindow is how near a turn herdr must have shown you in its pane for
// the turn to be yours.
const originWindow = 2 * time.Minute

// youKeep is how long the runtime keeps a time herdr showed you in a pane:
// past it, no turn still to read needs it.
const youKeep = 10 * time.Minute

// typedSettle is how long after a task is done, stopped or failed its panes
// are still read: their last reads after the task ended.
const typedSettle = youKeep

// conversation is one agent conversation of a task's role: the rows of the
// sessions that carried it, oldest first. A pane parked and launched again,
// or lost and recovered, goes on in a new row with the same conversation id,
// so the rows share one transcript, which has one reader: the newest row
// says whether the conversation's pane ended, and a turn is proven, and its
// agent's state told, by the row whose pane held the conversation when the
// turn arrived (rowAt), never by another pane's.
type conversation struct {
	key  string // the role and the conversation id
	rows []store.Session
}

// conversations groups ss, a task's sessions in the order of their rows,
// into conversations: the rows of a Claude or Codex agent with a conversation
// id, by role and id, each group in the order of its first row.
func conversations(ss []store.Session) []conversation {
	var out []conversation
	at := map[string]int{}
	for _, s := range ss {
		kind, sid := store.Deref(s.AgentKind), store.Deref(s.SessionID)
		if (kind != agents.KindClaude && kind != agents.KindCodex) || sid == "" {
			continue
		}
		key := s.Role + "\x00" + sid
		if i, ok := at[key]; ok {
			out[i].rows = append(out[i].rows, s)
			continue
		}
		at[key] = len(out)
		out = append(out, conversation{key: key, rows: []store.Session{s}})
	}
	return out
}

// newest is the row of the conversation's pane now, or last.
func (c conversation) newest() store.Session { return c.rows[len(c.rows)-1] }

// rowAt is the row whose pane held the conversation at at: the newest one
// started at or before it, else the oldest.
func (c conversation) rowAt(at time.Time) store.Session {
	row := c.rows[0]
	for _, s := range c.rows[1:] {
		if !s.StartedAt.After(at) {
			row = s
		}
	}
	return row
}

// fileMark tells a transcript changed: its path, size and time.
type fileMark struct {
	path string
	size int64
	mod  time.Time
}

// turnReads are the user turns one observe pass read, by transcript: a pass
// reads a transcript once, for whichever asks first (Reconcile's proof of a
// delivery, the typed turns), and the other takes the same read. A nil one
// reads afresh each time.
type turnReads map[string]turnRead

type turnRead struct {
	turns []agents.UserTurn
	err   error
}

// userTurns are the user turns of the transcript at path of a session of
// kind: reads' when this pass read it already.
func (r *Runtime) userTurns(reads turnReads, kind, path string) ([]agents.UserTurn, error) {
	if got, ok := reads[path]; ok {
		return got.turns, got.err
	}
	turns, err := r.s.userTurns(kind, path)
	if reads != nil {
		reads[path] = turnRead{turns, err}
	}
	return turns, err
}

// readUserTurns reads every user turn of the transcript at path of a
// session of kind.
func readUserTurns(kind, path string) ([]agents.UserTurn, error) {
	switch kind {
	case agents.KindClaude:
		return agents.ClaudeUserTurns(path, time.Time{})
	case agents.KindCodex:
		return agents.CodexUserTurns(path, time.Time{})
	}
	return nil, fmt.Errorf("a %q session keeps no transcript the factory reads", kind)
}

// sawYou notes that herdr showed you in session id's pane now: its agent at
// work on no prompt of the factory's (agents.ObsHumanActive).
func (r *Runtime) sawYou(id int64) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.youAt[id][:0]
	for _, at := range r.youAt[id] {
		if now.Sub(at) < youKeep {
			kept = append(kept, at)
		}
	}
	r.youAt[id] = append(kept, now)
}

// youIn is when herdr showed you in session id's pane, oldest first.
func (r *Runtime) youIn(id int64) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.youAt[id]...)
}

// typed is the observe pass's read of the turns typed into the factory's
// panes, at most once every typedEvery: an EventTyped for each conversation
// whose transcript holds such turns and changed since a read took it whole.
func (r *Runtime) typed(ctx context.Context, reads turnReads) []Event {
	now := r.now()
	r.mu.Lock()
	due := r.typedRead.IsZero() || now.Sub(r.typedRead) >= typedEvery
	if due {
		r.typedRead = now
	}
	r.mu.Unlock()
	if !due {
		return nil
	}
	tasks, err := r.reg.Tasks(ctx)
	if err != nil {
		r.log.Warn("read the turns typed into the panes", "err", err)
		return nil
	}
	listed, whole := map[string]bool{}, true
	var evs []Event
	for _, t := range tasks {
		switch t.State {
		case registry.TaskDone, registry.TaskStopped, registry.TaskFailed:
			if now.Sub(t.Updated) >= typedSettle {
				continue
			}
		}
		pr, err := r.ownerRow(ctx, t.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue // a task that never launched an agent
		}
		var ss []store.Session
		if err == nil {
			ss, err = r.st.SessionsByPR(ctx, pr.ID)
		}
		if err != nil {
			r.log.Warn("read the turns typed into the panes", "task", t.ID, "err", err)
			whole = false // what was read of its panes is kept for the next pass
			continue
		}
		for _, c := range conversations(ss) {
			listed[c.key] = true
			if ev, ok := r.typedIn(ctx, t, pr, c, reads); ok {
				evs = append(evs, ev)
			}
		}
	}
	r.mu.Lock()
	for key := range r.seen {
		if whole && !listed[key] {
			delete(r.seen, key)
		}
	}
	for id, at := range r.youAt {
		if len(at) == 0 || now.Sub(at[len(at)-1]) >= youKeep {
			delete(r.youAt, id)
		}
	}
	r.mu.Unlock()
	return evs
}

// typedIn reads the transcript of conversation c of task t (owner row pr)
// for the turns typed into its panes: an EventTyped of its role with them,
// false for none (no transcript, nothing changed since a read took it whole,
// none typed, a read that failed). A conversation whose pane ended gets a
// last read: no turn of it waits for its proof.
func (r *Runtime) typedIn(ctx context.Context, t registry.Task, pr store.PR, c conversation, reads turnReads) (Event, bool) {
	s := c.newest()
	kind := store.Deref(s.AgentKind)
	r.mu.Lock()
	prev, read := r.seen[c.key]
	r.mu.Unlock()
	mark := r.transcriptMark(ctx, s, prev.path)
	if mark.path == "" || (read && prev == mark) {
		return Event{}, false
	}
	turns, err := r.userTurns(reads, kind, mark.path)
	if err != nil {
		r.log.Warn("read the turns typed into a pane", "task", t.ID, "role", s.Role, "err", err)
		return Event{}, false // read again at the next pass
	}
	own, err := r.ownPrompts(ctx, t.ID, kind)
	if err != nil {
		r.log.Warn("read the turns typed into a pane", "task", t.ID, "role", s.Role, "err", err)
		return Event{}, false
	}
	runs, err := r.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		r.log.Warn("read the turns typed into a pane", "task", t.ID, "role", s.Role, "err", err)
		return Event{}, false
	}
	ended := s.State != store.SessionStarting && s.State != store.SessionLive
	you := map[int64][]time.Time{} // by row: when herdr showed you in its pane
	now := r.now()
	ev := Event{TaskID: t.ID, Role: s.Role, Kind: EventTyped}
	waits := false
	stages := map[string]string{}
	for _, u := range turns {
		if (u.Origin != "" && u.Origin != "human") || strings.TrimSpace(u.Text) == "" || own.holds(u.Text) {
			continue // a turn a tool or a notification began, or the factory's own
		}
		row := c.rowAt(u.At)
		if _, ok := you[row.ID]; !ok {
			you[row.ID] = r.youIn(row.ID)
		}
		origin := originOf(you[row.ID], u.At)
		if origin == registry.OriginUnknown && !ended && now.Sub(u.At) < originWindow {
			waits = true // herdr may not have shown the agent at work on it yet
			continue
		}
		run, inFlight := runAt(runs, row.ID, u.At)
		state := herdr.StatusIdle
		if inFlight {
			state = herdr.StatusWorking
		}
		ev.Turns = append(ev.Turns, Turn{At: u.At, Text: u.Text, Before: strings.Join(u.Before, "\n\n"),
			Stage: r.stageOf(ctx, t, run, stages), AgentState: string(state), Origin: origin})
	}
	if !waits {
		r.mu.Lock()
		r.seen[c.key] = mark
		r.mu.Unlock()
	}
	return ev, len(ev.Turns) > 0
}

// transcriptMark is session s's transcript as it is now: at known (the path
// a read found before, when it is there still), else where the agents layer
// says (the zero mark for none).
func (r *Runtime) transcriptMark(ctx context.Context, s store.Session, known string) fileMark {
	if known != "" {
		if fi, err := os.Stat(known); err == nil {
			return fileMark{path: known, size: fi.Size(), mod: fi.ModTime()}
		}
	}
	path, err := r.mgr.TranscriptPath(ctx, s)
	if err != nil {
		return fileMark{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fileMark{}
	}
	return fileMark{path: path, size: fi.Size(), mod: fi.ModTime()}
}

// ownTexts are what the factory, or the agents layer for it, types into a
// pane: the hashes of its prompts, and the slash commands magnum types
// (agents.MagnumCommands, the list the baseline drops as well).
type ownTexts struct {
	hashes   map[string]bool
	commands map[string]bool
}

// holds reports whether text, a user turn's, is the factory's own: one of
// its prompts, or the record of one of magnum's slash commands.
func (o ownTexts) holds(text string) bool {
	return o.hashes[PromptHash(text)] || agents.MagnumCommand(text, o.commands)
}

// ownPrompts are the texts the factory, or the agents layer for it, types
// into a pane of task id of kind: every prompt of the task's ledger, the
// kind's after-deny prompt, and the slash commands magnum types
// (r.commands: agents.MagnumCommands). A command of yours of one of those
// names is dropped with them, as the baseline drops it: a turn counts as
// yours only with positive proof.
func (r *Runtime) ownPrompts(ctx context.Context, id int64, kind string) (ownTexts, error) {
	entries, err := r.reg.LedgerOf(ctx, id)
	if err != nil {
		return ownTexts{}, err
	}
	own := ownTexts{hashes: map[string]bool{}, commands: r.commands}
	for _, e := range entries {
		own.hashes[e.Hash] = true
	}
	if k, ok := r.cfg.KindSpec(kind); ok && k.AfterDenyPrompt != "" {
		own.hashes[PromptHash(k.AfterDenyPrompt)] = true
	}
	return own, nil
}

// originOf is the origin of a turn typed at at: herdr's when it showed you
// in the turn's pane (you, the times it did) within originWindow of it,
// before or after (the agent shows at work once it took the turn), else
// unknown.
func originOf(you []time.Time, at time.Time) string {
	for _, t := range you {
		if t.Sub(at).Abs() <= originWindow {
			return registry.OriginHerdr
		}
	}
	return registry.OriginUnknown
}

// runAt is the newest run of session sid made at or before at (the zero
// run for none), and whether it was in flight then: submitted at or before
// at and not ended before it.
func runAt(runs []store.Run, sid int64, at time.Time) (store.Run, bool) {
	var found store.Run
	for _, run := range runs {
		if run.SessionID != nil && *run.SessionID == sid && !run.CreatedAt.After(at) &&
			(found.ID == "" || !run.CreatedAt.Before(found.CreatedAt)) {
			found = run
		}
	}
	inFlight := found.SubmittedAt != nil && !found.SubmittedAt.After(at) && (found.EndedAt == nil || found.EndedAt.After(at))
	return found, inFlight
}

// stageOf is the stage of run's attempt (stages caches them by attempt),
// else the task's.
func (r *Runtime) stageOf(ctx context.Context, t registry.Task, run store.Run, stages map[string]string) string {
	if run.TargetSHA == "" {
		return t.Stage
	}
	if st, ok := stages[run.TargetSHA]; ok {
		return st
	}
	st := t.Stage
	if a, err := r.reg.AttemptByID(ctx, run.TargetSHA); err == nil && a.Stage != "" {
		st = a.Stage
	}
	stages[run.TargetSHA] = st
	return st
}
