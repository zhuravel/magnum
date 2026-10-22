package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// The fixer gets the repository notes too, which it read nowhere before
// (fix.md rendered no notes): a copy beside its report, frozen when its
// prompt is written, in the parts every role reads, so a judge's rewrite
// of the live file while it works changes nothing it was given. A
// repository without notes gives it none.
func TestTheFixerReadsAFrozenCopyOfTheNotesInParts(t *testing.T) {
	for _, withNotes := range []bool{true, false} {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		text := strings.Repeat("- a standing decision of the repository, with the author's reason and the PR that settled it --------------------\n", 400)
		live := NotesPath(h.layout, "talkable", "talkable")
		if withNotes {
			if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(live, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		fixer(h, git, "", func(run store.Run) string {
			if withNotes {
				if err := os.WriteFile(live, []byte("# Notes for talkable/talkable (updated 2026-10-22)\n"), 0o600); err != nil {
					t.Errorf("rewrite the live notes: %v", err)
				}
			}
			return fixReport(run.ID, "nothing", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
		})
		h.fix(FixPayload{})
		h.handle()
		runs := h.fixRuns(pr.ID)
		if len(runs) != 1 {
			t.Fatalf("notes %v: fix runs = %+v", withNotes, runs)
		}
		h.ag.mu.Lock()
		inputs := h.ag.fixInputs
		h.ag.mu.Unlock()
		if len(inputs) != 1 {
			t.Fatalf("notes %v: fix prompts = %d", withNotes, len(inputs))
		}
		np := inputs[0].NotesParts
		if !withNotes {
			if np.Path != "" || len(np.Parts) != 0 {
				t.Errorf("no notes: the fix prompt names %+v", np)
			}
			continue
		}
		frozen := filepath.Join(filepath.Dir(deref(runs[0].ReportPath)), agents.NotesReadFile)
		if want := agents.SplitNotes(frozen, []byte(text)); np.Path != frozen || np.Bytes != len(text) || len(np.Parts) != len(want.Parts) || len(np.Parts) < 2 {
			t.Errorf("the fix prompt's notes = %s, %d bytes, %d parts; want %s, %d bytes, %d parts", np.Path, np.Bytes, len(np.Parts), frozen, len(text), len(want.Parts))
		}
		if got, err := os.ReadFile(frozen); err != nil || string(got) != text {
			t.Errorf("the fixer's copy holds %d bytes, %v; want the notes as its prompt found them", len(got), err)
		}
	}
}
