package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

const testArchive = "# Archive of the notes of talkable/talkable\n\n## Retired 2026-10-20\n- an old pitfall\n→ obsolete because its code is gone\n"

// The default triggers start no curation for size alone: a repository past
// max_bytes is marked, nothing more, while `magnum notes --curate` still
// runs one.
func TestARepositoryPastMaxBytesGetsNoAutomaticCuration(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal, func(h *harness) { h.cfg.Notes.Curate = config.Defaults().Notes.Curate })
	if v, _ := h.e.getKV(h.ctx, KVNotesOver("talkable/talkable")); v != notes.LimitBytes {
		t.Fatalf("mark = %q, want max_bytes", v)
	}
	for range 3 {
		h.advance(25 * time.Hour)
		h.tick()
	}
	if ps := proposals(t, h); len(ps) != 0 {
		t.Fatalf("a curation for size alone: %+v", ps)
	}
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
	if err != nil || !strings.Contains(msg, "started") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	h.settle()
	if ps := proposals(t, h); len(ps) != 1 || ps[0].Trigger != CurateTriggerRequest || ps[0].State != store.ProposalPending {
		t.Fatalf("proposals = %+v", ps)
	}
}

// The archive is no QA script: no round counts it and it is never unused,
// a judge naming it uses nothing, and the curator's usage data leaves it
// out of the harness while it reads its size.
func TestTheArchiveCountsForNoRound(t *testing.T) {
	h := newHarness(t)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n", map[string]string{"run_spec.sh": "x", notes.ArchiveName: testArchive})
	judgeRounds(t, h, nil, []string{"run_spec.sh", notes.ArchiveName}, notes.ArchiveName)
	h.reviewedPR(2, "b1")
	uses, err := h.st.NotesFileUses(h.ctx, repoID(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].File != "run_spec.sh" {
		t.Fatalf("file uses = %+v, want run_spec.sh alone", uses)
	}
	repo, _ := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	state, _ := notes.ReadState(nr)
	usage, _, err := h.e.curateUsage(h.ctx, repo, nr, state)
	if err != nil || strings.Contains(string(usage), `"name": "ARCHIVE.md"`) || !strings.Contains(string(usage), fmt.Sprintf(`"archive_bytes": %d`, len(testArchive))) {
		t.Errorf("usage.json = %s, %v", usage, err)
	}
}

// Only a curation or a recovery writes the archive: what a judge changed in
// it is put back as the registry last recorded it (the judge's state kept in
// the history first, so nothing is lost), with a notes.archive_restored
// warning and no harness toast; the rest of the judge's change stays. An
// archive a judge creates is removed.
func TestAJudgesEditOfTheArchiveIsPutBack(t *testing.T) {
	h := newHarness(t, noEveryReview)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n- run bin/rspec\n", map[string]string{"run_spec.sh": "bin/rspec\n", notes.ArchiveName: testArchive})
	judgeRounds(t, h, func(nr notes.Repo) {
		writeNotes(t, nr, "# Notes\n- run bin/rspec\n- lint with bin/rubocop\n", map[string]string{notes.ArchiveName: "# emptied by a judge\n"})
	}, nil, "")
	knownRepo(t, h)
	reviewNext(t, h, 2, "b1")
	h.flush()

	state, err := notes.ReadState(nr)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.Archive()) != testArchive || !strings.Contains(string(state.Notes), "bin/rubocop") {
		t.Fatalf("archive %q, notes %q: want the archive put back and the judge's notes kept", state.Archive(), state.Notes)
	}
	hist, err := h.st.NotesHistory(h.ctx, repoID(t, h), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || hist[1].Source != store.NotesFromJudge || hist[0].Source != store.NotesFromImport {
		t.Fatalf("history = %+v, want the import, the judge's version, the archive put back", hist)
	}
	if got := StateOf(mustContent(t, h, hist[0].ID)); !got.Same(state) {
		t.Errorf("the latest version is not the notes on disk")
	}
	if got := mustContent(t, h, hist[1].ID); !strings.Contains(string(StateOf(got).Archive()), "emptied by a judge") {
		t.Errorf("the judge's archive is not in the history")
	}
	evs := notesEvents(t, h, "notes.archive_restored")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "#2's judge changed the archive") ||
		strings.Contains(evs[0].Message+string(evs[0].Data), "old pitfall") {
		t.Fatalf("notes.archive_restored events = %+v", evs)
	}
	if got := toastsWith(h, "notes:"); len(got) != 0 {
		t.Errorf("toasts = %q: the archive is no script", got)
	}
	if n := len(notesEvents(t, h, "notes.harness_changed")); n != 0 {
		t.Errorf("notes.harness_changed events = %d", n)
	}

	// An archive the judge makes where none was is removed.
	if err := os.Remove(filepath.Join(nr.Harness(), notes.ArchiveName)); err != nil {
		t.Fatal(err)
	}
	h.e.syncNotes(h.ctx, false) // the operator removed it by hand: imported
	judgeRounds(t, h, func(nr notes.Repo) {
		writeNotes(t, nr, "# Notes\n- run bin/rspec\n", map[string]string{notes.ArchiveName: "# a judge's archive\n"})
	}, nil, "")
	reviewNext(t, h, 3, "c1")
	if _, err := os.Stat(filepath.Join(nr.Harness(), notes.ArchiveName)); !os.IsNotExist(err) {
		t.Errorf("the judge's new archive is still there: %v", err)
	}
	if n := len(notesEvents(t, h, "notes.archive_restored")); n != 2 {
		t.Errorf("notes.archive_restored events = %d, want 2", n)
	}
}

func mustContent(t *testing.T, h *harness, id int64) store.NotesContent {
	t.Helper()
	c, err := h.st.NotesVersionContent(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The curator's prompt asks for organized notes that lose nothing: no
// sentence that pushes toward the limits, the no-loss rules, and the
// archive's path in the proposed harness.
func TestTheCuratorPromptKeepsEveryLine(t *testing.T) {
	h := newHarness(t)
	nr := notesOf(t, h)
	scratch := notes.Scratch{Dir: filepath.Join(nr.Curate(), "20261022-120000")}
	text, _, err := h.e.curatePrompt(scratch, nr, []string{notes.LimitBytes}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "Stay within them") || strings.Contains(strings.ToLower(text), "stay within") {
		t.Errorf("the prompt pushes toward the limits:\n%s", text)
	}
	for _, want := range []string{
		"never loses knowledge",
		"Standing decisions and the authors' answers stay in full",
		"Age, rarity, low use and size are no reasons",
		"word for word",
		"merged into", "superseded by", "obsolete because",
		filepath.Join(scratch.Harness(), notes.ArchiveName),
		notes.ArchiveLine,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, text)
		}
	}
}

// The archive a judge changed goes back to what the round found (its
// snapshot), never to a newer row of the registry: a curation proposal
// waiting for review, whose own version holds another archive and is the
// newest row, leaves the live archive the snapshot's.
func TestAJudgesEditOfTheArchiveGoesBackToWhatTheRoundFound(t *testing.T) {
	h := newHarness(t, noEveryReview)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n- run bin/rspec\n", map[string]string{notes.ArchiveName: testArchive})
	judgeRounds(t, h, func(nr notes.Repo) {
		writeNotes(t, nr, "# Notes\n- run bin/rspec\n- lint\n", map[string]string{notes.ArchiveName: "# emptied by a judge\n"})
	}, nil, "")
	knownRepo(t, h)
	base, err := h.st.LatestNotesVersion(h.ctx, repoID(t, h))
	if err != nil {
		t.Fatal(err)
	}
	proposed := ContentOf(notes.State{Exists: true, Notes: []byte("# Notes\n- run bin/rspec\n")}.
		WithArchive([]byte(testArchive + "- a line only the waiting proposal archives\n")))
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: repoID(t, h), Kind: store.ProposalCuration,
		Trigger: CurateTriggerMisses, BaseVersionID: base.ID, Proposed: &proposed, State: store.ProposalPending})
	if err != nil || p.VersionID == nil || *p.VersionID <= base.ID {
		t.Fatalf("proposal = %+v, %v: its version is not the newest row", p, err)
	}
	reviewNext(t, h, 2, "b1")

	state, err := notes.ReadState(nr)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.Archive()) != testArchive {
		t.Fatalf("archive = %q, want the one the round found", state.Archive())
	}
	if evs := notesEvents(t, h, "notes.archive_restored"); len(evs) != 1 || !strings.Contains(evs[0].Message, "put back as the round found it") {
		t.Fatalf("notes.archive_restored events = %+v", evs)
	}
}

// When no recorded version holds the archive the round found (a hand edit
// right before the judge's snapshot, not imported yet), the judge's archive
// stays: the warning says it could not be put back, and nothing else is
// written or recorded.
func TestAnArchiveTheRegistryNeverHeldIsNotPutBack(t *testing.T) {
	h := newHarness(t, noEveryReview)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes\n- run bin/rspec\n", map[string]string{notes.ArchiveName: testArchive})
	const judged = "# rewritten by a judge\n"
	judgeRounds(t, h, func(nr notes.Repo) {
		writeNotes(t, nr, "# Notes\n- run bin/rspec\n", map[string]string{notes.ArchiveName: judged})
	}, nil, "")
	inner := h.rd.script
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		nr, _ := notes.RepoOf(in.NotesPath)
		if err := notes.WriteArchive(nr, []byte(testArchive+"- added by hand\n")); err != nil { // before the snapshot
			t.Error(err)
		}
		return inner(in)
	}
	knownRepo(t, h)
	reviewNext(t, h, 2, "b1")

	state, err := notes.ReadState(nr)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.Archive()) != judged {
		t.Errorf("archive = %q, want the judge's left as it is", state.Archive())
	}
	evs := notesEvents(t, h, "notes.archive_restored")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "could not be put back") {
		t.Fatalf("notes.archive_restored events = %+v", evs)
	}
	hist, _ := h.st.NotesHistory(h.ctx, repoID(t, h), 0)
	if len(hist) != 2 || hist[0].Source != store.NotesFromJudge {
		t.Errorf("history = %+v, want the import and the judge's version only", hist)
	}
}
