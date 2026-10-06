package notes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
)

// SnapshotFile is the snapshot a round takes of the notes before its judge is
// prompted, in the round's report directory.
const SnapshotFile = "notes-before.json"

// Snapshot is the notes and the harness as a round's judge found them: after
// the round, a state that differs from it is the round's change (or a
// concurrent writer's) and is recorded as a version of the judge.
type Snapshot struct {
	Round    int       `json:"round"`
	At       time.Time `json:"at"`
	Exists   bool      `json:"exists"`
	NotesSHA string    `json:"notes_sha256"`
	Harness  []File    `json:"harness"`
}

// Take snapshots r now for round.
func Take(r Repo, round int, now time.Time) (Snapshot, error) {
	text, exists, err := readNotes(r.Notes())
	if err != nil {
		return Snapshot{}, err
	}
	files, err := List(r.Harness())
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Round: round, At: now, Exists: exists, NotesSHA: TextSHA(text), Harness: files}, nil
}

// Fingerprint identifies the snapshot's state, comparable with
// State.Fingerprint.
func (s Snapshot) Fingerprint() string {
	return fingerprintOf(s.NotesSHA, s.Harness)
}

// WriteSnapshot writes s to dir/SnapshotFile.
func WriteSnapshot(dir string, s Snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(dir, SnapshotFile), b, 0o600)
}

// ReadSnapshot reads dir/SnapshotFile.
func ReadSnapshot(dir string) (Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, SnapshotFile))
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	err = json.Unmarshal(b, &s)
	return s, err
}
