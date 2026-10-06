package notes

import (
	"slices"
	"strings"
	"testing"
)

// mergeText joins lines into text, each ending in "\n".
func mergeText(lines ...string) string {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l + "\n")
	}
	return sb.String()
}

func TestMerge3(t *testing.T) {
	base8 := mergeText("l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8")
	tests := []struct {
		name               string
		base, ours, theirs string
		want               string          // the merged text; with conflicts, base's lines
		conflicts          []MergeConflict // in base's lines
	}{
		{
			name:   "non-overlapping edits of both sides merge",
			base:   base8,
			ours:   mergeText("l1", "ours2", "l3", "l4", "l5", "l6", "l7", "l8"),
			theirs: mergeText("l1", "l2", "l3", "l4", "l5", "theirs6", "l7", "l8"),
			want:   mergeText("l1", "ours2", "l3", "l4", "l5", "theirs6", "l7", "l8"),
		},
		{
			name:   "identical edits of both sides merge",
			base:   base8,
			ours:   mergeText("l1", "l2", "same", "l4", "l5", "l6", "l7", "l8"),
			theirs: mergeText("l1", "l2", "same", "l4", "l5", "l6", "l7", "l8"),
			want:   mergeText("l1", "l2", "same", "l4", "l5", "l6", "l7", "l8"),
		},
		{
			name:   "the same edit by both merges to one next to other edits",
			base:   base8,
			ours:   mergeText("l1", "l2", "same", "l4", "l5", "l6", "ours7", "l8"),
			theirs: mergeText("l1", "l2", "same", "l4", "theirs5", "l6", "l7", "l8"),
			want:   mergeText("l1", "l2", "same", "l4", "theirs5", "l6", "ours7", "l8"),
		},
		{
			name:      "edits of the same line conflict",
			base:      base8,
			ours:      mergeText("l1", "l2", "ours3", "l4", "l5", "l6", "l7", "l8"),
			theirs:    mergeText("l1", "l2", "theirs3", "l4", "l5", "l6", "l7", "l8"),
			want:      base8,
			conflicts: []MergeConflict{{2, 3}},
		},
		{
			name:      "an edit of a line next to the other side's edit conflicts",
			base:      base8,
			ours:      mergeText("l1", "ours2", "l3", "l4", "l5", "l6", "l7", "l8"),
			theirs:    mergeText("l1", "l2", "theirs3", "l4", "l5", "l6", "l7", "l8"),
			want:      base8,
			conflicts: []MergeConflict{{1, 3}},
		},
		{
			name:      "an insertion next to the other side's edit conflicts",
			base:      base8,
			ours:      mergeText("l1", "l2", "ours3", "l4", "l5", "l6", "l7", "l8"),
			theirs:    mergeText("l1", "l2", "l3", "theirs-new", "l4", "l5", "l6", "l7", "l8"),
			want:      base8,
			conflicts: []MergeConflict{{2, 3}},
		},
		{
			name:      "ours appending at the end while theirs deletes the last line conflicts",
			base:      base8,
			ours:      base8 + "l9\n",
			theirs:    mergeText("l1", "l2", "l3", "l4", "l5", "l6", "l7"),
			want:      base8,
			conflicts: []MergeConflict{{7, 8}},
		},
		{
			name:   "insertions by both at different places merge",
			base:   base8,
			ours:   mergeText("l1", "l2", "ours-new", "l3", "l4", "l5", "l6", "l7", "l8"),
			theirs: mergeText("l1", "l2", "l3", "l4", "l5", "l6", "theirs-new", "l7", "l8"),
			want:   mergeText("l1", "l2", "ours-new", "l3", "l4", "l5", "l6", "theirs-new", "l7", "l8"),
		},
		{
			name:   "the same insertion by both merges to one next to another edit",
			base:   base8,
			ours:   mergeText("l1", "l2", "new", "l3", "l4", "l5", "l6", "l7", "ours8"),
			theirs: mergeText("l1", "l2", "new", "l3", "l4", "l5", "l6", "l7", "l8"),
			want:   mergeText("l1", "l2", "new", "l3", "l4", "l5", "l6", "l7", "ours8"),
		},
		{
			name:      "different insertions at one place conflict",
			base:      base8,
			ours:      mergeText("l1", "l2", "ours-new", "l3", "l4", "l5", "l6", "l7", "l8"),
			theirs:    mergeText("l1", "l2", "theirs-new", "l3", "l4", "l5", "l6", "l7", "l8"),
			want:      base8,
			conflicts: []MergeConflict{{2, 2}},
		},
		{
			name:   "a deletion by one side and an edit elsewhere by the other merge",
			base:   base8,
			ours:   mergeText("l1", "l3", "l4", "l5", "l6", "l7", "l8"),
			theirs: mergeText("l1", "l2", "l3", "l4", "l5", "theirs6", "l7", "l8"),
			want:   mergeText("l1", "l3", "l4", "l5", "theirs6", "l7", "l8"),
		},
		{
			name:   "both sides deleting the same line merge to one deletion",
			base:   base8,
			ours:   mergeText("l1", "l3", "l4", "l5", "l6", "l7", "l8"),
			theirs: mergeText("l1", "l3", "l4", "l5", "l6", "l7", "theirs8"),
			want:   mergeText("l1", "l3", "l4", "l5", "l6", "l7", "theirs8"),
		},
		{
			name:   "an edit of the last line without its newline merges with an edit elsewhere",
			base:   mergeText("a", "b", "c"),
			ours:   mergeText("A", "b", "c"),
			theirs: "a\nb\nC",
			want:   "A\nb\nC",
		},
		{
			name:   "ours unchanged returns theirs exactly, a missing final newline included",
			base:   mergeText("a", "b"),
			ours:   mergeText("a", "b"),
			theirs: "a\nb\nc",
			want:   "a\nb\nc",
		},
		{
			name:   "theirs unchanged returns ours exactly, a missing final newline included",
			base:   mergeText("a", "b"),
			ours:   "a\nB",
			theirs: mergeText("a", "b"),
			want:   "a\nB",
		},
		{
			name:   "an empty base with the same text on both sides merges",
			base:   "",
			ours:   "a\n",
			theirs: "a\n",
			want:   "a\n",
		},
		{
			name:   "an empty base and a side that adds nothing return the other side",
			base:   "",
			ours:   "",
			theirs: "b\n",
			want:   "b\n",
		},
		{
			name:      "an empty base with different text on both sides conflicts",
			base:      "",
			ours:      "a\n",
			theirs:    "b\n",
			want:      "",
			conflicts: []MergeConflict{{0, 0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, conflicts := Merge3(tt.base, tt.ours, tt.theirs)
			if !slices.Equal(conflicts, tt.conflicts) {
				t.Fatalf("conflicts = %v, want %v\nmerged:\n%s", conflicts, tt.conflicts, got)
			}
			if got != tt.want {
				t.Errorf("merged =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestMerge3KeepsBasesLinesOnlyWhereItConflicts(t *testing.T) {
	base := mergeText("l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8")
	// Ours inserts two lines after l1 and edits l6 and l8, theirs deletes l3
	// and edits l6: only l6 conflicts, and each side's growth before it must
	// not move the lines the other side changed.
	ours := mergeText("l1", "n1", "n2", "l2", "l3", "l4", "l5", "ours6", "l7", "ours8")
	theirs := mergeText("l1", "l2", "l4", "l5", "theirs6", "l7", "l8")
	got, conflicts := Merge3(base, ours, theirs)
	if want := []MergeConflict{{5, 6}}; !slices.Equal(conflicts, want) {
		t.Fatalf("conflicts = %v, want %v", conflicts, want)
	}
	want := mergeText("l1", "n1", "n2", "l2", "l4", "l5", "l6", "l7", "ours8")
	if got != want {
		t.Errorf("merged =\n%q\nwant\n%q", got, want)
	}
}

func TestMerge3CarriesEachSidesGrowthIntoLaterRegions(t *testing.T) {
	base := mergeText("l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8")
	// Ours inserts three lines at the top and edits l5, theirs deletes two
	// lines near the top and edits l8: the offsets of both sides carry into
	// the regions below.
	ours := mergeText("n1", "n2", "n3", "l1", "l2", "l3", "l4", "ours5", "l6", "l7", "l8")
	theirs := mergeText("l1", "l4", "l5", "l6", "l7", "theirs8")
	got, conflicts := Merge3(base, ours, theirs)
	if len(conflicts) > 0 {
		t.Fatalf("conflicts = %v, want none\nmerged:\n%s", conflicts, got)
	}
	want := mergeText("n1", "n2", "n3", "l1", "l4", "ours5", "l6", "l7", "theirs8")
	if got != want {
		t.Errorf("merged =\n%q\nwant\n%q", got, want)
	}
}

// mergeBlob is a harness file with its hash set.
func mergeBlob(path, body string) Blob {
	return Blob{Path: path, SHA256: TextSHA([]byte(body)), Body: []byte(body)}
}

// mergeFiles lists the files as "path=body", for comparing.
func mergeFiles(files []Blob) []string {
	out := make([]string, 0, len(files))
	for _, b := range files {
		out = append(out, b.Path+"="+string(b.Body))
	}
	return out
}

func checkStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestMergeStatesKeepsAFileTheProposalDeletesButLiveChanged(t *testing.T) {
	base := State{Exists: true, Notes: []byte("notes\n"), Files: []Blob{mergeBlob("a.sh", "1\n"), mergeBlob("b.md", "x\n")}}
	live := State{Exists: true, Notes: []byte("notes\n"), Files: []Blob{mergeBlob("a.sh", "1\n2\n"), mergeBlob("b.md", "x\n")}}
	proposed := State{Exists: true, Notes: []byte("notes\n"), Files: []Blob{mergeBlob("b.md", "x\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean {
		t.Fatalf("not clean: %+v", got)
	}
	checkStrings(t, "Kept", got.Kept, []string{"a.sh"})
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"a.sh=1\n2\n", "b.md=x\n"})
	if got.Conflicts != nil || got.Merged != nil || got.NotesConflicts != nil {
		t.Errorf("Conflicts = %v, Merged = %v, NotesConflicts = %v, want none", got.Conflicts, got.Merged, got.NotesConflicts)
	}
}

func TestMergeStatesDeletesAFileTheProposalDeletesAndLiveLeftAlone(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.sh", "1\n"), mergeBlob("b.md", "x\n")}}
	live := base
	proposed := State{Files: []Blob{mergeBlob("b.md", "x\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean || got.Kept != nil {
		t.Fatalf("Clean = %v, Kept = %v, want clean and none kept", got.Clean, got.Kept)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"b.md=x\n"})
}

func TestMergeStatesKeepsAFileLiveAddedThatTheProposalDoesNotKnow(t *testing.T) {
	base := State{Exists: true, Notes: []byte("notes\n"), Files: []Blob{mergeBlob("b.md", "x\n")}}
	live := State{Exists: true, Notes: []byte("notes\n"), Files: []Blob{mergeBlob("b.md", "x\n"), mergeBlob("new.sh", "echo\n")}}
	proposed := State{Exists: true, Notes: []byte("notes\n"), Files: []Blob{mergeBlob("b.md", "y\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean || got.Kept != nil {
		t.Fatalf("Clean = %v, Kept = %v, want clean and none kept", got.Clean, got.Kept)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"b.md=y\n", "new.sh=echo\n"})
}

func TestMergeStatesTakesTheProposalsBodyOfAFileLiveLeftAlone(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", "1\n")}}
	live := State{Files: []Blob{mergeBlob("a.md", "1\n")}}
	proposed := State{Files: []Blob{mergeBlob("a.md", "1\n2\n"), mergeBlob("z.md", "added\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean || got.Merged != nil {
		t.Fatalf("Clean = %v, Merged = %v, want clean and nothing merged", got.Clean, got.Merged)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"a.md=1\n2\n", "z.md=added\n"})
}

func TestMergeStatesKeepsLivesBodyOfAFileTheProposalLeftAlone(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", "1\n")}}
	live := State{Files: []Blob{mergeBlob("a.md", "live\n")}}
	proposed := State{Files: []Blob{mergeBlob("a.md", "1\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean || got.Merged != nil || got.Kept != nil {
		t.Fatalf("Clean = %v, Merged = %v, Kept = %v, want clean and neither", got.Clean, got.Merged, got.Kept)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"a.md=live\n"})
}

func TestMergeStatesAcceptsAFileBothMadeTheSame(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", "1\n")}}
	live := State{Files: []Blob{mergeBlob("a.md", "same\n"), mergeBlob("n.md", "new\n")}}
	proposed := State{Files: []Blob{mergeBlob("a.md", "same\n"), mergeBlob("n.md", "new\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean || got.Merged != nil || got.Conflicts != nil {
		t.Fatalf("Clean = %v, Merged = %v, Conflicts = %v, want clean and neither", got.Clean, got.Merged, got.Conflicts)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"a.md=same\n", "n.md=new\n"})
}

func TestMergeStatesMergesAFileBothChangedOnDifferentLines(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", mergeText("l1", "l2", "l3", "l4", "l5", "l6")), mergeBlob("b.md", "x\n")}}
	live := State{Files: []Blob{mergeBlob("a.md", mergeText("live1", "l2", "l3", "l4", "l5", "l6")), mergeBlob("b.md", "x\n")}}
	proposed := State{Files: []Blob{mergeBlob("a.md", mergeText("l1", "l2", "l3", "l4", "l5", "proposed6")), mergeBlob("b.md", "x\n")}}
	got := MergeStates(base, live, proposed)
	if !got.Clean {
		t.Fatalf("not clean: %+v", got)
	}
	checkStrings(t, "Merged", got.Merged, []string{"a.md"})
	want := mergeText("live1", "l2", "l3", "l4", "l5", "proposed6")
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"a.md=" + want, "b.md=x\n"})
	if sha := got.State.Files[0].SHA256; sha != TextSHA([]byte(want)) {
		t.Errorf("merged file's SHA256 = %s, want the hash of its body", sha)
	}
}

func TestMergeStatesConflictsOnAFileBothChangedOnTheSameLine(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", mergeText("l1", "l2", "l3"))}}
	live := State{Files: []Blob{mergeBlob("a.md", mergeText("l1", "live", "l3"))}}
	proposed := State{Files: []Blob{mergeBlob("a.md", mergeText("l1", "proposed", "l3"))}}
	got := MergeStates(base, live, proposed)
	if got.Clean {
		t.Fatalf("clean, want a conflict: %+v", got)
	}
	checkStrings(t, "Conflicts", got.Conflicts, []string{"a.md"})
	if got.Merged != nil {
		t.Errorf("Merged = %v, want none", got.Merged)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"a.md=" + mergeText("l1", "live", "l3")})
}

func TestMergeStatesConflictsOnAFileLiveDeletedAndTheProposalChanged(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", "1\n"), mergeBlob("b.md", "x\n")}}
	live := State{Files: []Blob{mergeBlob("b.md", "x\n")}}
	proposed := State{Files: []Blob{mergeBlob("a.md", "1\n2\n"), mergeBlob("b.md", "x\n")}}
	got := MergeStates(base, live, proposed)
	if got.Clean {
		t.Fatalf("clean, want a conflict: %+v", got)
	}
	checkStrings(t, "Conflicts", got.Conflicts, []string{"a.md"})
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{"b.md=x\n"})
}

func TestMergeStatesConflictsOnAFileBothAddedWithDifferentBodies(t *testing.T) {
	live := State{Files: []Blob{mergeBlob("a.md", "live\n")}}
	proposed := State{Files: []Blob{mergeBlob("a.md", "proposed\n")}}
	got := MergeStates(State{}, live, proposed)
	if got.Clean {
		t.Fatalf("clean, want a conflict: %+v", got)
	}
	checkStrings(t, "Conflicts", got.Conflicts, []string{"a.md"})
}

func TestMergeStatesIsNotCleanWhenTheNotesTextConflicts(t *testing.T) {
	base := State{Exists: true, Notes: []byte(mergeText("l1", "l2", "l3", "l4"))}
	live := State{Exists: true, Notes: []byte(mergeText("l1", "l2", "live", "l4"))}
	proposed := State{Exists: true, Notes: []byte(mergeText("l1", "l2", "proposed", "l4"))}
	got := MergeStates(base, live, proposed)
	if got.Clean {
		t.Fatalf("clean, want a conflict: %+v", got)
	}
	if want := []MergeConflict{{2, 3}}; !slices.Equal(got.NotesConflicts, want) {
		t.Errorf("NotesConflicts = %v, want %v", got.NotesConflicts, want)
	}
	if got.Conflicts != nil {
		t.Errorf("Conflicts = %v, want none: only the notes conflict", got.Conflicts)
	}
}

func TestMergeStatesMergesTheNotesText(t *testing.T) {
	base := State{Exists: true, Notes: []byte(mergeText("l1", "l2", "l3", "l4", "l5", "l6"))}
	live := State{Exists: true, Notes: []byte(mergeText("live1", "l2", "l3", "l4", "l5", "l6"))}
	proposed := State{Exists: true, Notes: []byte(mergeText("l1", "l2", "l3", "l4", "l5", "proposed6"))}
	got := MergeStates(base, live, proposed)
	if !got.Clean || !got.State.Exists {
		t.Fatalf("Clean = %v, Exists = %v, want clean and existing", got.Clean, got.State.Exists)
	}
	if want := mergeText("live1", "l2", "l3", "l4", "l5", "proposed6"); string(got.State.Notes) != want {
		t.Errorf("notes =\n%q\nwant\n%q", got.State.Notes, want)
	}
}

func TestMergeStatesNotesExistence(t *testing.T) {
	tests := []struct {
		name                 string
		base, live, proposed State
		wantExists           bool
		wantNotes            string
	}{
		{
			name:       "the proposal creates notes live lacks too",
			base:       State{},
			live:       State{},
			proposed:   State{Exists: true, Notes: []byte("new\n")},
			wantExists: true,
			wantNotes:  "new\n",
		},
		{
			name:       "the proposal deletes notes live left alone",
			base:       State{Exists: true, Notes: []byte("old\n")},
			live:       State{Exists: true, Notes: []byte("old\n")},
			proposed:   State{},
			wantExists: false,
			wantNotes:  "",
		},
		{
			name:       "live deleted the notes and the proposal left them alone",
			base:       State{Exists: true, Notes: []byte("old\n")},
			live:       State{},
			proposed:   State{Exists: true, Notes: []byte("old\n")},
			wantExists: false,
			wantNotes:  "",
		},
		{
			name:       "live created the notes and the proposal did not",
			base:       State{},
			live:       State{Exists: true, Notes: []byte("live\n")},
			proposed:   State{},
			wantExists: true,
			wantNotes:  "live\n",
		},
		{
			name:       "both created the notes with the same text",
			base:       State{},
			live:       State{Exists: true, Notes: []byte("same\n")},
			proposed:   State{Exists: true, Notes: []byte("same\n")},
			wantExists: true,
			wantNotes:  "same\n",
		},
		{
			name:       "both deleted the notes",
			base:       State{Exists: true, Notes: []byte("old\n")},
			live:       State{},
			proposed:   State{},
			wantExists: false,
			wantNotes:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeStates(tt.base, tt.live, tt.proposed)
			if !got.Clean {
				t.Fatalf("not clean: %+v", got)
			}
			if got.State.Exists != tt.wantExists || string(got.State.Notes) != tt.wantNotes {
				t.Errorf("Exists = %v, notes = %q, want %v, %q", got.State.Exists, got.State.Notes, tt.wantExists, tt.wantNotes)
			}
		})
	}
}

func TestMergeStatesSortsTheFilesAndSetsTheirHashes(t *testing.T) {
	// The inputs come without hashes: they are computed to compare and to set.
	base := State{Files: []Blob{{Path: "b.md", Body: []byte("b\n")}, {Path: "e.md", Body: []byte("e\n")}}}
	live := State{Files: []Blob{
		{Path: "a.md", Body: []byte("a\n")}, {Path: "b.md", Body: []byte("b\n")},
		{Path: "d.md", Body: []byte("d\n")}, {Path: "e.md", Body: []byte("e live\n")},
	}}
	proposed := State{Files: []Blob{
		{Path: "b.md", Body: []byte("b proposed\n")}, {Path: "c/x.md", Body: []byte("c\n")}, {Path: "e.md", Body: []byte("e\n")},
	}}
	got := MergeStates(base, live, proposed)
	if !got.Clean {
		t.Fatalf("not clean: %+v", got)
	}
	checkStrings(t, "files", mergeFiles(got.State.Files), []string{
		"a.md=a\n", "b.md=b proposed\n", "c/x.md=c\n", "d.md=d\n", "e.md=e live\n",
	})
	for _, f := range got.State.Files {
		if f.SHA256 != TextSHA(f.Body) {
			t.Errorf("%s: SHA256 = %q, want the hash of its body", f.Path, f.SHA256)
		}
	}
}

func TestMergeStatesListsAreSortedAndNilWhenEmpty(t *testing.T) {
	base := State{Files: []Blob{mergeBlob("a.md", "1\n"), mergeBlob("b.md", "1\n"), mergeBlob("c.md", "1\n")}}
	live := State{Files: []Blob{mergeBlob("a.md", "live\n"), mergeBlob("b.md", "live\n"), mergeBlob("c.md", "live\n")}}
	got := MergeStates(base, live, State{})
	if !got.Clean {
		t.Fatalf("not clean: %+v", got)
	}
	checkStrings(t, "Kept", got.Kept, []string{"a.md", "b.md", "c.md"})
	if got.Conflicts != nil || got.Merged != nil || got.NotesConflicts != nil {
		t.Errorf("Conflicts = %v, Merged = %v, NotesConflicts = %v, want nil", got.Conflicts, got.Merged, got.NotesConflicts)
	}
	empty := MergeStates(State{}, State{}, State{})
	if !empty.Clean || empty.State.Files != nil || empty.Kept != nil {
		t.Errorf("merge of empty states = %+v, want clean with nothing", empty)
	}
}

func TestMergeStatesDoesNotChangeItsInputs(t *testing.T) {
	base := State{Exists: true, Notes: []byte(mergeText("a", "b", "c")), Files: []Blob{mergeBlob("f.md", mergeText("1", "2", "3"))}}
	live := State{Exists: true, Notes: []byte(mergeText("A", "b", "c")), Files: []Blob{mergeBlob("f.md", mergeText("one", "2", "3"))}}
	proposed := State{Exists: true, Notes: []byte(mergeText("a", "b", "C")), Files: []Blob{mergeBlob("f.md", mergeText("1", "2", "three"))}}
	clone := func(s State) State {
		out := State{Exists: s.Exists, Notes: slices.Clone(s.Notes)}
		for _, b := range s.Files {
			out.Files = append(out.Files, Blob{Path: b.Path, SHA256: b.SHA256, Body: slices.Clone(b.Body)})
		}
		return out
	}
	b0, l0, p0 := clone(base), clone(live), clone(proposed)
	got := MergeStates(base, live, proposed)
	if !got.Clean {
		t.Fatalf("not clean: %+v", got)
	}
	for name, pair := range map[string][2]State{"base": {base, b0}, "live": {live, l0}, "proposed": {proposed, p0}} {
		if !pair[0].Same(pair[1]) || string(pair[0].Notes) != string(pair[1].Notes) {
			t.Errorf("%s was changed", name)
		}
	}
}
