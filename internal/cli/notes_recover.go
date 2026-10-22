package cli

// `magnum notes <repo> --recover`: the one-time recovery of what earlier
// curations cut (DECISIONS "Curation organizes the notes and never loses a
// line"). It proposes an archive (notes.ArchiveName) that holds every line
// a live version of the notes held and the notes now lack, grouped by the
// day of the version that dropped it (notes.Recover); the notes text stays
// as it is. The proposal is a curation proposal no curator wrote (trigger
// engine.CurateTriggerRecover), reviewed like a restore.

import (
	"context"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// notesRecoverJSON is what --recover --json prints: the recovery and the
// archive it would propose.
type notesRecoverJSON struct {
	Repo     string `json:"repo"`
	Versions int    `json:"versions_read"` // the live versions of the history read
	notes.Recovery
	ArchiveBytes int    `json:"archive_bytes"` // the proposed archive's size (0: nothing to recover)
	Archive      string `json:"archive"`
	// Pending is the proposal that waits for review, when one does: the
	// recovery is proposed only once it is decided.
	Pending int64 `json:"pending_proposal,omitempty"`
}

// notesRecover reads every live version of the repository's notes (the
// history: never a proposal's own state) and proposes the recovery: --json
// prints it and records nothing; on a terminal, with no other proposal
// waiting, the notes now are recorded (when they are not yet), the
// recovery is stored as a proposal and reviewed, y applying it under the
// notes lock as any proposal.
func notesRecover(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, nr notes.Repo) int {
	full := repo.FullName()
	live, err := notes.ReadState(nr)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	hist, err := d.Store.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	slices.Reverse(hist) // oldest first
	history := make([]notes.HistoryNotes, 0, len(hist))
	for _, v := range hist {
		text, err := d.Store.NotesVersionText(ctx, v)
		if err != nil {
			return cmdFail(c, "notes", err)
		}
		history = append(history, notes.HistoryNotes{ID: v.ID, At: v.At, Source: v.Source, Notes: text})
	}
	r := notes.Recover(full, history, live, notesNow(d))
	out := notesRecoverJSON{Repo: full, Versions: len(history), Recovery: r, ArchiveBytes: len(r.Archive), Archive: string(r.Archive)}
	if out.Groups == nil {
		out.Groups = []notes.RecoveryGroup{}
	}
	pending, err := d.Store.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: engine.WaitingStates, Limit: 1})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	if len(pending) > 0 {
		out.Pending = pending[0].ID
	}
	switch {
	case f.json:
		if err := writeJSON(c.Stdout, out); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	case !d.StdinTTY:
		return inspUsage(c, "notes", "--recover asks y/N on a terminal; --json prints the archive it would propose for scripts", notesUsage)
	case r.Lines == 0:
		fmt.Fprintf(c.Stderr, "nothing to recover: every line the %s of the notes of %s held is in the notes or the archive now%s\n",
			textx.Count(len(history), "recorded version", "recorded versions"), full, notesLeftOut(r))
		return 0
	case out.Pending != 0:
		return cmdFail(c, "notes", fmt.Errorf("proposal %d for the notes of %s waits for review: `magnum notes %s --review` applies or rejects it first",
			out.Pending, full, full))
	}
	base, _, err := d.Store.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: repo.ID, Source: store.NotesFromImport,
		Content: engine.ContentOf(live), Dedupe: true})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	proposed := engine.ContentOf(live.WithArchive(r.Archive))
	p, err := d.Store.CreateNotesProposal(ctx, store.NotesProposalInput{RepoID: repo.ID, Kind: store.ProposalCuration,
		Trigger: engine.CurateTriggerRecover, BaseVersionID: base.ID, Proposed: &proposed, State: store.ProposalPending, At: notesNow(d)})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	fmt.Fprintf(c.Stdout, "recovered %s from %s, by the day each left the notes (%s)%s\n", textx.Count(r.Lines, "line", "lines"),
		textx.Count(len(history), "recorded version", "recorded versions"), textx.Count(len(r.Groups), "day", "days"), notesLeftOut(r))
	return notesReviewProposal(ctx, c, d, f, repo, nr, p)
}

// notesLeftOut says how many lines a recovery left out, "" for none.
func notesLeftOut(r notes.Recovery) string {
	if r.Left == 0 {
		return ""
	}
	return fmt.Sprintf("; %s left out: each holds a secret or a home directory path (the registry keeps them)", textx.Count(r.Left, "line", "lines"))
}
