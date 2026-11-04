package runtime

// The turns typed into the factory's panes (spec section 11.1). Once every
// typedEvery, the observe pass reads the transcript of each live session
// that changed since it last read it, and of each that left since (one last
// read: nothing waits for a read that never comes), and hands the engine
// every user turn it holds that the factory did not send (EventTyped): no
// prompt of the task's ledger (by its hash), no after-deny prompt of the
// session's kind, no tool's result and no turn a tool or a notification
// began. The proof of a turn's origin is herdr's: the owner row's
// human_active_at within originWindow of the turn (OriginHerdr); a turn
// without it is OriginUnknown, and one younger than originWindow waits for a
// later read, as herdr may not have shown the agent at work on it yet. The
// pass reads a transcript's user turns once: Reconcile's proof of a delivery
// takes the same read (turnReads). The engine records each turn once (the
// registry keys a pane's turn by its role and time), so a restart, which
// forgets what was read, hands them again and loses none.

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
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

// originWindow is how near a turn herdr must have shown you in the task's
// panes for the turn to be yours.
const originWindow = 2 * time.Minute

// typedSeen is what the runtime keeps of a session it read for typed turns:
// its row, and the transcript as that read saw it ("" path: none was there).
type typedSeen struct {
	session store.Session
	file    fileMark
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

// typed is the observe pass's read of the turns typed into the factory's
// panes, at most once every typedEvery: an EventTyped for each session
// whose transcript holds such turns and changed since its last read (a
// turn that waits for its proof is read again), and one last read of each
// session read before that is no longer live.
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
	live, err := r.st.LiveSessions(ctx)
	if err != nil {
		r.log.Warn("read the turns typed into the panes", "err", err)
		return nil
	}
	var evs []Event
	for _, s := range live {
		if ev, ok := r.typedIn(ctx, s, reads, false); ok {
			evs = append(evs, ev)
		}
	}
	r.mu.Lock()
	var gone []store.Session
	for id, seen := range r.seen {
		if !slices.ContainsFunc(live, func(s store.Session) bool { return s.ID == id }) {
			gone = append(gone, seen.session)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(gone, func(a, b store.Session) int { return cmp.Compare(a.ID, b.ID) })
	for _, s := range gone {
		if cur, err := r.st.SessionByID(ctx, s.ID); err == nil {
			s = cur
		}
		ev, ok := r.typedIn(ctx, s, reads, true)
		r.mu.Lock()
		delete(r.seen, s.ID)
		r.mu.Unlock()
		if ok {
			evs = append(evs, ev)
		}
	}
	return evs
}

// typedIn reads session s's transcript for the turns typed into its pane:
// an EventTyped with them, false for none (no transcript, nothing changed
// since the last read, none typed). last is the read of a session that left:
// what it read is not kept, and no turn waits for its proof.
func (r *Runtime) typedIn(ctx context.Context, s store.Session, reads turnReads, last bool) (Event, bool) {
	kind := store.Deref(s.AgentKind)
	if kind != agents.KindClaude && kind != agents.KindCodex {
		return Event{}, false
	}
	var mark fileMark
	if path, err := r.mgr.TranscriptPath(ctx, s); err == nil {
		if fi, err := os.Stat(path); err == nil {
			mark = fileMark{path: path, size: fi.Size(), mod: fi.ModTime()}
		}
	}
	r.mu.Lock()
	prev, read := r.seen[s.ID]
	if !last {
		r.seen[s.ID] = typedSeen{session: s, file: prev.file}
	}
	r.mu.Unlock()
	if mark.path == "" || (read && prev.file == mark) {
		return Event{}, false
	}
	t, pr, err := r.ownerOf(ctx, s.PRID)
	if err != nil {
		return Event{}, false
	}
	turns, err := r.userTurns(reads, kind, mark.path)
	if err != nil {
		r.log.Warn("read the turns typed into a pane", "task", t.ID, "role", s.Role, "err", err)
		return Event{}, false
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
	now := r.now()
	ev := Event{TaskID: t.ID, Role: s.Role, Kind: EventTyped}
	waits := false
	stages := map[string]string{}
	for _, u := range turns {
		if (u.Origin != "" && u.Origin != "human") || strings.TrimSpace(u.Text) == "" || own[PromptHash(u.Text)] {
			continue // a turn a tool or a notification began, or the factory's own prompt
		}
		origin := originOf(pr.HumanActiveAt, u.At)
		if origin == registry.OriginUnknown && !last && now.Sub(u.At) < originWindow {
			waits = true // herdr may not have shown the agent at work on it yet
			continue
		}
		run, inFlight := runAt(runs, s.ID, u.At)
		state := herdr.StatusIdle
		if inFlight {
			state = herdr.StatusWorking
		}
		ev.Turns = append(ev.Turns, Turn{At: u.At, Text: u.Text, Before: strings.Join(u.Before, "\n\n"),
			Stage: r.stageOf(ctx, t, run, stages), AgentState: string(state), Origin: origin})
	}
	if !last && !waits {
		r.mu.Lock()
		r.seen[s.ID] = typedSeen{session: s, file: mark}
		r.mu.Unlock()
	}
	return ev, len(ev.Turns) > 0
}

// ownPrompts are the hashes of the texts the factory, or the agents layer
// for it, types into a pane of task id of kind: every prompt of the task's
// ledger and the kind's after-deny prompt.
func (r *Runtime) ownPrompts(ctx context.Context, id int64, kind string) (map[string]bool, error) {
	entries, err := r.reg.LedgerOf(ctx, id)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{}
	for _, e := range entries {
		own[e.Hash] = true
	}
	if k, ok := r.cfg.KindSpec(kind); ok && k.AfterDenyPrompt != "" {
		own[PromptHash(k.AfterDenyPrompt)] = true
	}
	return own, nil
}

// originOf is the origin of a turn typed at at: herdr's when it showed you
// in the task's panes (human, the owner row's human_active_at) within
// originWindow of it, before or after (the agent shows at work once it took
// the turn), else unknown.
func originOf(human *time.Time, at time.Time) string {
	if human != nil && human.Sub(at).Abs() <= originWindow {
		return registry.OriginHerdr
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
