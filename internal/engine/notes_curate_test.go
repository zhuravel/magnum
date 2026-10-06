package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// fakeCurator writes a proposal into the scratch directory the way the
// curator agent would, then checks it as the pane curator does: a valid
// proposal at once, an invalid one after a second try (the nudge).
type fakeCurator struct {
	mu      sync.Mutex
	write   func(s notes.Scratch)
	prompts []string
	closed  int
}

func (f *fakeCurator) Curate(_ context.Context, job CurateJob) (CurateResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, job.Prompt)
	f.mu.Unlock()
	f.write(job.Scratch)
	p, problems := job.Check()
	if len(problems) > 0 {
		f.write(job.Scratch) // the nudged turn writes the same again
		p, problems = job.Check()
	}
	return CurateResult{Proposal: p, Problems: problems}, nil
}

func (f *fakeCurator) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

const curatedNotes = "# Notes for talkable/talkable (updated 2026-10-06)\n\n## Tests\nRun `run_spec.sh <spec>`.\n"

// goodProposal is a curator that merges the probe into nothing: it keeps
// run_spec.sh, deletes the probe, and accounts for both.
func goodProposal(s notes.Scratch) {
	_ = os.RemoveAll(filepath.Join(s.Harness(), "campaign_snapshot_probes_spec.rb"))
	_ = os.WriteFile(s.Proposal(), []byte(curatedNotes), 0o600)
	b, _ := json.Marshal(notes.Changes{
		Sections: []notes.Change{{Name: "Tests", Action: notes.ActionKept, Reason: "every review runs specs"}},
		Files: []notes.Change{
			{Name: "run_spec.sh", Action: notes.ActionKept, Reason: "runs any spec with the checkout's Ruby"},
			{Name: "campaign_snapshot_probes_spec.rb", Action: notes.ActionDeleted, Reason: "one pull request's probe"},
		},
	})
	_ = os.WriteFile(s.Changes(), b, 0o600)
}

// curationHarness is a harness whose repository is known, with notes past
// max_bytes holding a probe, and a fake curator.
func curationHarness(t *testing.T, write func(notes.Scratch), mods ...func(*harness)) (*harness, *fakeCurator, notes.Repo) {
	t.Helper()
	cur := &fakeCurator{write: write}
	mods = append([]func(*harness){func(h *harness) {
		h.d.Curator = func(context.Context, CurateRun) (Curator, error) { return cur, nil }
		h.cfg.Notes.MaxBytes = 64
	}}, mods...)
	h := newHarness(t, mods...)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes #11920\n",
		map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	knownRepo(t, h)
	return h, cur, nr
}

func proposals(t *testing.T, h *harness) []store.NotesProposal {
	t.Helper()
	ps, err := h.st.NotesProposals(h.ctx, store.NotesProposalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// A repository marked past its limits gets a curation on the next scan: the
// curator works on a scratch copy, its valid proposal is stored for review
// (the proposed state as a curation version, its changes, model and prompt
// hash), a toast says so once, and the live notes do not change.
func TestACurationStoresAValidProposalForReview(t *testing.T) {
	h, cur, nr := curationHarness(t, goodProposal)
	before, _ := os.ReadFile(nr.Notes())
	if v, _ := h.e.getKV(h.ctx, KVNotesOver("talkable/talkable")); v == "" {
		t.Fatal("the notes are not marked past their limits")
	}
	h.advance(curateCheckEvery)
	h.tick() // the next scan finds the mark
	ps := proposals(t, h)
	if len(ps) != 1 {
		t.Fatalf("proposals = %+v", ps)
	}
	p := ps[0]
	if p.State != store.ProposalPending || p.Kind != store.ProposalCuration || p.Trigger != CurateTriggerOverLimit ||
		p.Model != "sonnet" || len(p.PromptSHA256) != 64 || p.BaseVersionID == nil || p.VersionID == nil || !strings.Contains(string(p.Changes), "one pull request's probe") {
		t.Fatalf("proposal = %+v", p)
	}
	content, err := h.st.NotesVersionContent(h.ctx, *p.VersionID)
	if err != nil || string(content.Notes) != curatedNotes || len(content.Files) != 1 || content.Files[0].Path != "run_spec.sh" {
		t.Fatalf("proposed state = %q %+v, %v", content.Notes, content.Files, err)
	}
	if after, _ := os.ReadFile(nr.Notes()); string(after) != string(before) {
		t.Error("the curation changed the live notes")
	}
	if len(cur.prompts) != 1 || !strings.Contains(cur.prompts[0], p.Scratch) || strings.Contains(cur.prompts[0], "#11920") {
		t.Errorf("prompt: %q", cur.prompts)
	}
	if cur.closed != 1 {
		t.Errorf("curator closed %d times", cur.closed)
	}
	if len(notesEvents(t, h, "notes.curate_ready")) != 1 {
		t.Error("no notes.curate_ready event")
	}
	h.advance(2 * time.Minute)
	h.tick()
	if got := strings.Join(h.nh.all(), "\n"); strings.Count(got, "notes curation for talkable/talkable is ready") != 1 {
		t.Errorf("toasts: %s", got)
	}
	// One proposal waits: no other curation of the repository starts.
	h.advance(25 * time.Hour)
	h.tick()
	if n := len(proposals(t, h)); n != 1 {
		t.Errorf("%d proposals while one waits for review", n)
	}
}

// A proposal still invalid after the nudge is kept with its problems, and
// nothing waits for review.
func TestAnInvalidProposalIsKeptWithItsProblems(t *testing.T) {
	h, _, _ := curationHarness(t, func(s notes.Scratch) {
		goodProposal(s)
		_ = os.WriteFile(s.Proposal(), []byte(curatedNotes+"- see #11920\n"), 0o600)
	})
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalInvalid || ps[0].VersionID != nil || !strings.Contains(ps[0].Reason, "names a pull request") ||
		ps[0].Trigger != CurateTriggerRequest {
		t.Fatalf("proposals = %+v", ps)
	}
	if n, _ := h.st.CountNotesProposals(h.ctx, store.ProposalPending); n != 0 {
		t.Errorf("%d pending", n)
	}
	if evs := notesEvents(t, h, "notes.curate_invalid"); len(evs) != 1 || strings.Contains(evs[0].Message, "11920") {
		t.Errorf("curate_invalid events = %+v", evs)
	}
}

// Without a mark nothing is curated under curate = "over_limit"; once a day
// at most for a marked repository, and only after its notes changed since
// the last curation; a proposal nobody reviews expires after a week and
// stays in the registry.
func TestCurationsRunWhenDueAndProposalsExpire(t *testing.T) {
	h, _, nr := curationHarness(t, goodProposal, func(h *harness) { h.cfg.Notes.MaxBytes = 1 << 20 })
	h.advance(curateCheckEvery)
	h.tick()
	if n := len(proposals(t, h)); n != 0 {
		t.Fatalf("%d proposals without a mark", n)
	}
	h.cfg.Notes.MaxBytes = 64
	h.e.syncNotes(h.ctx, false) // a reconcile measures and marks
	h.advance(11 * time.Minute)
	h.tick()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalPending {
		t.Fatalf("proposals = %+v", ps)
	}

	h.advance(ProposalTTL)
	h.tick()
	ps = proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalExpired {
		t.Fatalf("after a week: %+v", ps)
	}
	// The notes did not change since that curation: none runs again.
	h.advance(25 * time.Hour)
	h.tick()
	if n := len(proposals(t, h)); n != 1 {
		t.Fatalf("%d proposals for unchanged notes", n)
	}
	// They changed: the next scan curates them again.
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`, then lint.\n", nil)
	h.e.syncNotes(h.ctx, false)
	h.advance(11 * time.Minute)
	h.tick()
	if n := len(proposals(t, h)); n != 2 {
		t.Fatalf("%d proposals after the notes changed, want 2", n)
	}
}

// `magnum notes --curate` runs whatever curate says, but not while a
// proposal waits or a round of the repository is in its judge stage.
func TestRequestCurateRefusesWhileAProposalWaits(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal, func(h *harness) { h.cfg.Notes.Curate = config.CurateOff })
	h.advance(curateCheckEvery)
	h.tick()
	if n := len(proposals(t, h)); n != 0 {
		t.Fatalf("curate = off curated on its own: %d", n)
	}
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
	if err != nil || !strings.Contains(msg, "started") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	h.settle()
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err == nil || !strings.Contains(err.Error(), "waits for review") {
		t.Fatalf("a second request while the proposal waits: %v", err)
	}
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "example/tools"}); err == nil || !strings.Contains(err.Error(), "not in the registry") {
		t.Fatalf("an unknown repository: %v", err)
	}
}
