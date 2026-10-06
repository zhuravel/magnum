package engine

// Stale proposals and the curations waiting for a start (DECISIONS "Stale
// notes proposals, and curations that wait for the judge stage"). A pending
// proposal is stale when the notes are no longer the version it started
// from: a judge round rewrote them since. `magnum notes <repo> --review`
// merges the notes' changes since and the proposal's (notes.MergeStates)
// and applies the merge, or the operator asks for a new curation that starts
// from the notes now and reads the stale proposal, which is kept as
// superseded. The daemon does the same on its own for a stale proposal a day
// old whose changes no longer merge. A curation asked for, or due, while a
// round of the repository is in its judge stage (which may rewrite the
// notes) waits in a queue (KVNotesCurateQueue) and starts once that stage
// ended, one curation at a time; the one running is KVNotesCurating.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// KVNotesCurating holds the curation running now (CurateMark as JSON),
	// deleted when it ends and at every start of the daemon.
	KVNotesCurating = "notes.curating"
	// KVNotesCurateQueue holds the curations waiting for a start
	// ([]CurateQueued as JSON, oldest first, one per repository).
	KVNotesCurateQueue = "notes.curate_queue"

	// CurateTriggerStale is the daemon's follow-up of a stale proposal
	// (notes_proposals.trigger_reason).
	CurateTriggerStale = "stale"
	// staleRecurateAfter is how old a stale proposal whose changes no
	// longer merge must be before the daemon supersedes it on its own.
	staleRecurateAfter = 24 * time.Hour

	// Why a curation waits (CurateQueued.Why).
	QueuedJudge = "judge" // a round of the repository is in its judge stage
	QueuedBusy  = "busy"  // another curation runs
)

// CurateMark is the curation running now (KVNotesCurating).
type CurateMark struct {
	Repo    string    `json:"repo"`
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
}

// CurateQueued is a curation waiting for a start (KVNotesCurateQueue).
type CurateQueued struct {
	Repo    string    `json:"repo"`
	Trigger string    `json:"trigger"`
	Why     string    `json:"why"` // QueuedJudge | QueuedBusy
	At      time.Time `json:"at"`
}

// ReadCurating is the curation running now, nil when none runs (or the
// mark is unreadable).
func ReadCurating(ctx context.Context, st *store.Store) *CurateMark {
	v, ok, err := st.GetKV(ctx, KVNotesCurating)
	if err != nil || !ok || v == "" {
		return nil
	}
	var m CurateMark
	if json.Unmarshal([]byte(v), &m) != nil || m.Repo == "" {
		return nil
	}
	return &m
}

// ReadCurateQueue lists the curations waiting for a start, oldest first.
func ReadCurateQueue(ctx context.Context, st *store.Store) []CurateQueued {
	v, ok, err := st.GetKV(ctx, KVNotesCurateQueue)
	if err != nil || !ok || v == "" {
		return nil
	}
	var q []CurateQueued
	if json.Unmarshal([]byte(v), &q) != nil {
		return nil
	}
	return q
}

// StaleCheck is a pending proposal against the notes now: whether it is
// stale and, when it is, the three-way merge of the notes' changes since
// its base and its own.
type StaleCheck struct {
	Stale          bool
	Live           notes.State // the notes now
	Base, Proposed notes.State
	Merge          notes.StateMerge // set when Stale
}

// ProposalStale reports whether p is stale: the notes on disk are not the
// state of its base version. It reads the version's listing from the
// registry and hashes the files on disk, never a version's content.
func ProposalStale(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal) (bool, error) {
	if p.BaseVersionID == nil {
		return false, nil
	}
	base, err := st.NotesVersionByID(ctx, *p.BaseVersionID)
	if err != nil {
		return false, err
	}
	snap, err := notes.Take(nr, 0, time.Time{})
	if err != nil {
		return false, err
	}
	return snap.Fingerprint() != (notes.Snapshot{NotesSHA: base.SHA256, Harness: listingOf(base)}).Fingerprint(), nil
}

// CheckStale reads p's base and proposed states and the notes now and, when
// the notes are no longer p's base, merges both sides' changes.
func CheckStale(ctx context.Context, st *store.Store, nr notes.Repo, p store.NotesProposal) (StaleCheck, error) {
	var c StaleCheck
	if p.VersionID == nil {
		return c, fmt.Errorf("proposal %d has no proposed state", p.ID)
	}
	if p.BaseVersionID != nil {
		content, err := st.NotesVersionContent(ctx, *p.BaseVersionID)
		if err != nil {
			return c, err
		}
		c.Base = StateOf(content)
	}
	content, err := st.NotesVersionContent(ctx, *p.VersionID)
	if err != nil {
		return c, err
	}
	c.Proposed = StateOf(content)
	if c.Live, err = notes.ReadState(nr); err != nil {
		return c, err
	}
	if c.Live.Fingerprint() != c.Base.Fingerprint() {
		c.Stale, c.Merge = true, notes.MergeStates(c.Base, c.Live, c.Proposed)
	}
	return c, nil
}

// supersede marks p, a stale pending curation, superseded: a new curation of
// the notes now follows it up and reads it (runCurate). False when it
// cannot (it was decided meanwhile, or the registry failed).
func (e *Engine) supersede(ctx context.Context, p store.NotesProposal, full, why string) bool {
	if _, err := e.st.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalSuperseded, why, e.now(), nil); err != nil {
		e.log.Warn("notes curation: supersede a stale proposal", "proposal", p.ID, "err", err)
		return false
	}
	e.event(ctx, "info", notesSubjectOf(full), "notes.proposal_superseded",
		fmt.Sprintf("notes proposal %d for %s is superseded: %s", p.ID, full, why), map[string]any{"proposal": p.ID})
	return true
}

// staleConflicts reports whether repo's pending proposal p is stale and its
// changes no longer merge with the notes' (a three-way merge with a
// conflict): nothing an operator could apply.
func (e *Engine) staleConflicts(ctx context.Context, repo store.Repo, p store.NotesProposal) bool {
	nr, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name)
	if !ok || p.Kind != store.ProposalCuration {
		return false
	}
	c, err := CheckStale(ctx, e.st, nr, p)
	if err != nil {
		e.log.Warn("notes curation: check a waiting proposal", "proposal", p.ID, "err", err)
		return false
	}
	return c.Stale && !c.Merge.Clean
}

// queueCurate queues a curation of full (by trigger, waiting for why); a
// repository already queued keeps its place and its first trigger.
func (e *Engine) queueCurate(ctx context.Context, full, trigger, why string) {
	e.curateMu.Lock()
	q := ReadCurateQueue(ctx, e.st)
	if slices.ContainsFunc(q, func(c CurateQueued) bool { return strings.EqualFold(c.Repo, full) }) {
		e.curateMu.Unlock()
		return
	}
	q = append(q, CurateQueued{Repo: strings.ToLower(full), Trigger: trigger, Why: why, At: e.now()})
	e.writeCurateQueue(ctx, q)
	e.curateMu.Unlock()
	e.event(ctx, "info", notesSubjectOf(full), "notes.curate_queued", fmt.Sprintf("notes curation of %s queued (%s): %s", full, trigger, queuedText(why)),
		map[string]any{"trigger": trigger, "why": why})
}

// unqueueCurate drops full from the queue; the caller holds curateMu.
func (e *Engine) unqueueCurate(ctx context.Context, full string) {
	q := ReadCurateQueue(ctx, e.st)
	n := len(q)
	q = slices.DeleteFunc(q, func(c CurateQueued) bool { return strings.EqualFold(c.Repo, full) })
	if len(q) != n {
		e.writeCurateQueue(ctx, q)
	}
}

func (e *Engine) writeCurateQueue(ctx context.Context, q []CurateQueued) {
	if len(q) == 0 {
		e.delKV(ctx, KVNotesCurateQueue)
		return
	}
	b, err := json.Marshal(q)
	if err != nil {
		return
	}
	e.setKV(ctx, KVNotesCurateQueue, string(b))
}

// queuedText says what a queued curation waits for.
func queuedText(why string) string {
	if why == QueuedBusy {
		return "starts when the running curation ends"
	}
	return "starts when the current round's judge stage ends"
}

// startQueuedCuration starts the oldest queued curation that can start: its
// repository has no round in its judge stage. An entry whose repository is
// gone or got a proposal meanwhile is dropped; one of the daemon's own
// triggers waits while `magnum pause` holds automation or [notes] curate
// lists none. The caller checked that no curation runs and nothing holds
// the curator.
func (e *Engine) startQueuedCuration(ctx context.Context) {
	q := ReadCurateQueue(ctx, e.st)
	for _, c := range q {
		repo, err := e.st.RepoByFullName(ctx, c.Repo)
		drop := err != nil
		if err == nil {
			_, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name)
			pending, perr := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: []string{store.ProposalPending}, Limit: 1})
			drop = !ok || (perr == nil && len(pending) > 0)
		}
		if drop {
			e.curateMu.Lock()
			e.unqueueCurate(ctx, c.Repo)
			e.curateMu.Unlock()
			continue
		}
		if c.Trigger != CurateTriggerRequest && (len(e.cfg.Notes.Curate) == 0 || e.userPause(ctx) != "") {
			continue
		}
		if e.notesJudging(ctx, repo.ID) {
			continue
		}
		e.startCurate(ctx, repo, c.Trigger)
		return
	}
}
