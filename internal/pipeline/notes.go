package pipeline

import (
	"errors"
	"io/fs"
	"log/slog"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/notes"
)

// freezeNotes copies the repository notes into the report directory
// (agents.NotesReadFile), right before the round's first prompt, and keeps
// the copy's parts for every role's prompt (rd.notesParts): judges rewrite
// the live file about 20 times a day, and a role reads the notes in parts,
// whose line ranges a rewrite between two reads would shift. A blind
// replay copies its scratch notes the same way. A copy that cannot be
// written leaves the live file's parts; notes that cannot be read leave
// none. A failure is logged, never the round's.
func (rd *round) freezeNotes() {
	if rd.in.NotesPath == "" {
		return
	}
	np, err := agents.FreezeNotes(rd.in.NotesPath, rd.dir)
	if err != nil {
		rd.logAt(slog.LevelWarn, "pipeline: freeze the repository notes: %v", err)
	}
	rd.notesParts = np
}

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
			rd.logAt(slog.LevelWarn, "pipeline: read the notes snapshot: %v", err)
		}
	}
	s, err := notes.Take(repo, rd.in.Round, rd.r.now())
	if err == nil {
		err = notes.WriteSnapshot(rd.dir, s)
	}
	if err != nil {
		rd.logAt(slog.LevelWarn, "pipeline: snapshot the repository notes: %v", err)
	}
}
