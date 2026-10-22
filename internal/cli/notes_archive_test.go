package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	archiveV1 = "# Archive of the notes of talkable/talkable\n\n## Retired 2026-10-06\n- `campaign_snapshot_probes_spec.rb` probes a fix\n→ obsolete because the probe is gone\n"
	archiveV2 = archiveV1 + "\n## Retired 2026-10-08\nRun `run_spec.sh`.\n→ superseded by: Run `run_spec.sh <spec>`.\n"
)

// Applying a proposal writes its archive with its notes: the review shows
// the archive's size and what it adds, never as a harness file, and the
// version the apply records holds it.
func TestApplyWritesTheArchiveWithTheNotes(t *testing.T) {
	h := newNotesHarness(t)
	base := h.write(oldNotes, map[string]string{"run_spec.sh": "x"})
	bv := h.record(base, store.NotesFromImport)
	proposed := engine.ContentOf(notes.State{Exists: true, Notes: []byte(newNotes), Files: []notes.Blob{{Path: "run_spec.sh", Body: []byte("x")}}}.
		WithArchive([]byte(archiveV2)))
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "misses",
		BaseVersionID: bv.ID, Proposed: &proposed, State: store.ProposalPending, At: h.now})
	if err != nil {
		t.Fatal(err)
	}
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "harness: 1 → 1 files", "archive: 0 → "+itoa(int64(len(archiveV2)))+" bytes",
		"+++ talkable/talkable archive, proposal "+itoa(p.ID), "+Run `run_spec.sh`.", "applied")
	if strings.Contains(out, "+ "+notes.ArchiveName) {
		t.Errorf("the archive is listed as a harness file:\n%s", out)
	}
	live, _ := notes.ReadState(h.nr)
	if string(live.Notes) != newNotes || string(live.Archive()) != archiveV2 {
		t.Fatalf("after the apply: %q, archive %q", live.Notes, live.Archive())
	}
	hist, _ := h.st.NotesHistory(h.ctx, h.repo.ID, 0)
	if c, err := h.st.NotesVersionContent(h.ctx, hist[0].ID); err != nil || string(engine.StateOf(c).Archive()) != archiveV2 || hist[0].Source != store.NotesFromCuration {
		t.Errorf("the applied version = %+v, %v", hist[0], err)
	}
	// `magnum notes` shows the archive's size beside the notes.
	h.errb.Reset()
	if code := h.cmd("notes", "talkable/talkable"); code != 0 {
		t.Fatal(code)
	}
	actContains(t, h.errb.String(), "archive: "+itoa(int64(len(archiveV2)))+" bytes, 9 lines (ARCHIVE.md in the harness", "harness: 1 files")
}

// The history, --diff and --restore cover the archive: --log counts it apart
// from the harness, --diff shows what it gained, and a restore brings it
// back with the notes.
func TestHistoryDiffAndRestoreCoverTheArchive(t *testing.T) {
	h := newNotesHarness(t)
	s1 := h.write(oldNotes, map[string]string{"run_spec.sh": "x", notes.ArchiveName: archiveV1})
	v1 := h.record(s1, store.NotesFromCuration)
	s2 := h.write(newNotes, map[string]string{"run_spec.sh": "x", notes.ArchiveName: archiveV2})
	h.record(s2, store.NotesFromCuration)

	if code := h.cmd("notes", "talkable/talkable", "--log"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "archive "+itoa(int64(len(archiveV2)))+" bytes") || !strings.Contains(lines[1], "1 file, 1 bytes") ||
		strings.Contains(lines[1], "(~1)") {
		t.Fatalf("--log:\n%s", h.out.String())
	}
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--log", "--json"); code != 0 {
		t.Fatal(code)
	}
	var entries []notesLogEntry
	if err := json.Unmarshal(h.out.Bytes(), &entries); err != nil || len(entries) != 2 || len(entries[0].HarnessChanged) != 0 ||
		entries[0].ArchiveBytes != int64(len(archiveV2)) {
		t.Fatalf("--log --json = %+v, %v", entries, err)
	}

	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--diff"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "--- talkable/talkable archive, version "+itoa(v1.ID), "+++ talkable/talkable archive, now",
		"+## Retired 2026-10-08", "harness: unchanged")

	h.out.Reset()
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--restore", itoa(v1.ID)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	live, _ := notes.ReadState(h.nr)
	if string(live.Notes) != oldNotes || string(live.Archive()) != archiveV1 {
		t.Fatalf("after the restore: %q, archive %q", live.Notes, live.Archive())
	}
}

// --recover proposes an archive of every line a live version held and the
// notes lack: a line dropped twice is archived once, and a line only a
// never-applied proposal held is not. --json prints it and records nothing;
// on a terminal it is proposed and reviewed like a restore, and y applies
// it with the notes as they are.
func TestRecoverArchivesWhatTheHistoryDropped(t *testing.T) {
	h := newNotesHarness(t)
	const (
		n1 = "# Notes for talkable/talkable\n\n## Tests\n- run the specs\n- the importer skips blank rows\n"
		n2 = "# Notes for talkable/talkable\n\n## Tests\n- run the specs\n"
		n4 = "# Notes for talkable/talkable\n\n## Tests\n- run the specs and lint\n"
	)
	bv := h.record(h.write(n1, nil), store.NotesFromImport)
	only := engine.ContentOf(notes.State{Exists: true, Notes: []byte(n1 + "- a line only the proposal held\n")})
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "request",
		BaseVersionID: bv.ID, Proposed: &only, State: store.ProposalPending, At: h.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, "no", h.now, nil); err != nil {
		t.Fatal(err)
	}
	h.record(h.write(n2, nil), store.NotesFromJudge)
	h.record(h.write(n1, nil), store.NotesFromJudge) // the dropped line came back
	h.record(h.write(n4, nil), store.NotesFromCuration)

	if code := h.cmd("notes", "talkable/talkable", "--recover", "--json"); code != 0 {
		t.Fatalf("--json: exit %d: %s", code, h.errb.String())
	}
	var got notesRecoverJSON
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if got.Lines != 2 || got.Versions != 4 || len(got.Groups) != 1 || strings.Count(got.Archive, "- the importer skips blank rows\n") != 1 ||
		!strings.Contains(got.Archive, "- run the specs\n") || strings.Contains(got.Archive, "only the proposal") || got.ArchiveBytes != len(got.Archive) {
		t.Fatalf("--recover --json = %+v", got)
	}
	if ps, _ := h.st.NotesProposals(h.ctx, store.NotesProposalFilter{RepoID: h.repo.ID}); len(ps) != 1 {
		t.Fatalf("--json proposed something: %+v", ps)
	}
	if code := h.cmd("notes", "talkable/talkable", "--recover"); code != 2 || !strings.Contains(h.errb.String(), "--recover asks y/N on a terminal") {
		t.Fatalf("not a terminal: exit %d: %s", code, h.errb.String())
	}

	h.out.Reset()
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--recover"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "a recovery of the lines the notes history held", "+- the importer skips blank rows", "applied")
	live, _ := notes.ReadState(h.nr)
	if string(live.Notes) != n4 || string(live.Archive()) != got.Archive {
		t.Fatalf("after the recovery: %q, archive %q", live.Notes, live.Archive())
	}
	ps, _ := h.st.NotesProposals(h.ctx, store.NotesProposalFilter{RepoID: h.repo.ID})
	if len(ps) != 2 || ps[0].Trigger != engine.CurateTriggerRecover || ps[0].State != store.ProposalApplied {
		t.Fatalf("proposals = %+v", ps)
	}
	// Recovered once: nothing is left to recover.
	h.errb.Reset()
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--recover"); code != 0 || !strings.Contains(h.errb.String(), "nothing to recover") {
		t.Errorf("a second recovery: exit %d: %s", code, h.errb.String())
	}
}
