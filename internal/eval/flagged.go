package eval

// Flagged cases: a case whose replay its agent's provider refused (the
// round's outcome "refused": Codex flagged the content as a possible
// cybersecurity risk) is never replayed, since Codex may block an account
// it takes for a cyber abuser. The run records the case's outcome as any
// other; the cases flagged so far are kept in FlaggedFile under the runs'
// root, which `magnum eval run` reads before it starts a case and refuses
// a flagged one with the reason. Deleting a case's entry there replays it
// again.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
)

// FlaggedFile holds the flagged cases (Flagged by case name) under the
// runs' root.
const FlaggedFile = "flagged.json"

// Flagged is a case whose replay was refused: in which run, at which head,
// why and when.
type Flagged struct {
	Case   string    `json:"case"`
	PR     string    `json:"pr"`
	Head   string    `json:"head"`
	Run    string    `json:"run"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Refusal says why the case is not replayed.
func (f Flagged) Refusal() string {
	s := fmt.Sprintf("flagged in run %s", f.Run)
	if !f.At.IsZero() {
		s += " on " + f.At.Local().Format("Jan 2 15:04")
	}
	if f.Reason != "" {
		s += " (" + f.Reason + ")"
	}
	return s + ": never replayed (delete its entry from " + FlaggedFile + " to replay it)"
}

// LoadFlagged reads the flagged cases under root by case name; a missing
// file is none.
func LoadFlagged(root string) (map[string]Flagged, error) {
	data, err := os.ReadFile(filepath.Join(root, FlaggedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Flagged{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eval: flagged cases: %w", err)
	}
	out := map[string]Flagged{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("eval: flagged cases %s: %w", filepath.Join(root, FlaggedFile), err)
	}
	return out, nil
}

// MarkFlagged adds f to the flagged cases under root (the first record of
// a case stands).
func MarkFlagged(root string, f Flagged) error {
	all, err := LoadFlagged(root)
	if err != nil {
		return err
	}
	if _, ok := all[f.Case]; ok {
		return nil
	}
	all[f.Case] = f
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: flag %s: %w", f.Case, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("eval: flag %s: %w", f.Case, err)
	}
	if err := fsx.WriteFileAtomic(filepath.Join(root, FlaggedFile), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("eval: flag %s: %w", f.Case, err)
	}
	return nil
}
