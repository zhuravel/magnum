package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// lossyProposal stores a curation of oldNotes that drops a line without
// archiving it (one stored before the preservation check, say).
func (h *notesHarness) lossyProposal() store.NotesProposal {
	h.t.Helper()
	base := h.write(oldNotes, map[string]string{"run_spec.sh": "x"})
	bv := h.record(base, store.NotesFromImport)
	proposed := engine.ContentOf(notes.State{Exists: true, Notes: []byte(newNotes), Files: []notes.Blob{{Path: "run_spec.sh", Body: []byte("x")}}})
	changes, _ := json.Marshal(notes.Changes{Sections: []notes.Change{{Name: "Tests", Action: notes.ActionKept, Reason: "every review runs specs"}}})
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "misses",
		BaseVersionID: bv.ID, Proposed: &proposed, Changes: changes, State: store.ProposalPending, At: h.now})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// A curation that would lose a line of the notes cannot be applied: the
// review says so and quotes the lines, --json carries them, and y asks the
// daemon for a new curation that supersedes it (saying why) instead of
// writing anything.
func TestReviewOfAProposalThatLosesALineAsksForANewCurationInstead(t *testing.T) {
	h := newNotesHarness(t)
	p := h.lossyProposal()
	before, _ := notes.ReadState(h.nr)

	if code := h.cmd("notes", "talkable/talkable", "--review", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var data struct {
		NotPreserved []string `json:"not_preserved"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil || len(data.NotPreserved) == 0 {
		t.Fatalf("--json = %s, %v", h.out.String(), err)
	}

	h.out.Reset()
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "notes curation of talkable/talkable started")
	}
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "CANNOT BE APPLIED", "campaign_snapshot_probes_spec.rb", "would lose lines of the notes, so it cannot be applied",
		"y asks for a new curation")
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqNotesCurate {
		t.Fatalf("requests = %+v", reqs)
	}
	if pl := actDecode[engine.NotesCuratePayload](t, reqs[0].Payload); pl.Supersede != p.ID || pl.Why == "" {
		t.Errorf("payload = %+v, want a supersede of %d with a reason", pl, p.ID)
	}
	if now, _ := notes.ReadState(h.nr); !now.Same(before) {
		t.Error("the notes were written")
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalPending {
		t.Errorf("proposal = %+v (the daemon supersedes it)", got)
	}
}

// The operator's decision refused because the critic decided first names
// the critic: an apply after its rejection, a rejection after its apply.
func TestTheOperatorsRefusalNamesTheCritic(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	live, _ := notes.ReadState(h.nr)
	if _, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByCritic,
		"keep the probes list", h.now, nil); err != nil {
		t.Fatal(err)
	}
	_, err := notesApply(context.Background(), h.st, h.nr, p, live.Fingerprint(), "", h.now)
	var decided *engine.ProposalDecidedError
	if !errors.As(err, &decided) || !strings.Contains(err.Error(), "already rejected by the critic") {
		t.Fatalf("an apply after the critic's rejection = %v", err)
	}

	p2 := h.proposal()
	if _, err := h.st.ClaimNotesProposal(h.ctx, p2.ID, store.NotesClaim{By: store.DecidedByCritic, Target: "fp"}); err != nil {
		t.Fatal(err)
	}
	h.terminal("n\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "proposal "+itoa(p2.ID)+" for the notes of talkable/talkable is being applied by the critic")
	if got := h.proposalNow(p2.ID); got.State != store.ProposalApplying {
		t.Errorf("the n of a review reached a claimed proposal: %+v", got)
	}
	if err := notesDecided(h.ctx, h.d, p2.ID, store.ErrConflict); !strings.Contains(err.Error(), "being applied by the critic") {
		t.Fatalf("a rejection during the critic's apply = %v", err)
	}
}
