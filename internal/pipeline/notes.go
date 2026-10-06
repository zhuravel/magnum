package pipeline

import (
	"errors"
	"io/fs"

	"github.com/zhuravel/magnum/internal/notes"
)

// snapshotNotes records the repository notes and their harness as the judge
// finds them (notes.SnapshotFile in the report directory), right before it
// is prompted: after the round the engine records a state that differs from
// it as the judge's version (engine.recordRoundNotes). A continued turn
// keeps the snapshot its round took before the pause, so the change the
// judge made before it is still the round's. A blind replay's notes are a
// scratch copy and get none. A failure is logged, never the round's.
func (rd *round) snapshotNotes() {
	if rd.in.NotesPath == "" || rd.in.Blind {
		return
	}
	repo, ok := notes.RepoOf(rd.in.NotesPath)
	if !ok {
		return
	}
	if rd.in.Kind == KindContinue {
		if s, err := notes.ReadSnapshot(rd.dir); err == nil && s.Round == rd.in.Round {
			return
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			rd.logf("pipeline: read the notes snapshot: %v", err)
		}
	}
	s, err := notes.Take(repo, rd.in.Round, rd.r.now())
	if err == nil {
		err = notes.WriteSnapshot(rd.dir, s)
	}
	if err != nil {
		rd.logf("pipeline: snapshot the repository notes: %v", err)
	}
}
