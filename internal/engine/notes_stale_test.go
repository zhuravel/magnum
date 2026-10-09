package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// judging puts PR #1 in its judge stage: reviewing, its round started now
// and its judge prompted (notesJudging). The returned func ends the round.
func judging(t *testing.T, h *harness) func() {
	t.Helper()
	pr := h.pr(1)
	if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRReviewing, func(u *store.PRUpdate) {
		u.Set("last_round_started_at", h.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.judgeRun(h.pr(1), store.RunInitial, "base1", store.RunWorking, "", ""); err != nil {
		t.Fatal(err)
	}
	if !h.e.notesJudging(h.ctx, pr.RepoID) {
		t.Fatal("the round is not in its judge stage")
	}
	return func() {
		t.Helper()
		if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRReviewed, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func queued(t *testing.T, h *harness) []CurateQueued {
	t.Helper()
	return ReadCurateQueue(h.ctx, h.st)
}

// `magnum notes --curate` during a judge stage is queued, not refused: it
// says so, waits while the stage lasts and starts once it ended; the queue
// shows it meanwhile, and the running curation is marked while it runs.
func TestACurateRequestDuringAJudgeStageIsQueuedAndStartsWhenItEnds(t *testing.T) {
	var running *CurateMark
	var h *harness
	h, _, _ = curationHarness(t, func(s notes.Scratch) {
		running = ReadCurating(h.ctx, h.st)
		goodProposal(s)
	}, func(h *harness) { h.cfg.Notes.Curate = config.CurateTriggers{} })
	end := judging(t, h)
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
	if err != nil || !strings.Contains(msg, "queued: starts when the current round's judge stage ends") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	if q := queued(t, h); len(q) != 1 || q[0].Repo != "talkable/talkable" || q[0].Trigger != CurateTriggerRequest || q[0].Why != QueuedJudge {
		t.Fatalf("queue = %+v", q)
	}
	if len(notesEvents(t, h, "notes.curate_queued")) != 1 {
		t.Error("no notes.curate_queued event")
	}
	// A second request while it waits keeps the one entry.
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.e.maybeCurate(h.ctx)
	h.settle()
	if n := len(proposals(t, h)); n != 0 || len(queued(t, h)) != 1 {
		t.Fatalf("started during the judge stage: %d proposals, queue %+v", n, queued(t, h))
	}

	end()
	h.e.maybeCurate(h.ctx) // every tick looks at the queue, whatever curate says
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalPending || ps[0].Trigger != CurateTriggerRequest {
		t.Fatalf("proposals after the judge stage = %+v", ps)
	}
	if q := queued(t, h); len(q) != 0 {
		t.Errorf("queue after the start = %+v", q)
	}
	if running == nil || running.Repo != "talkable/talkable" || running.Trigger != CurateTriggerRequest {
		t.Errorf("the running curation's mark = %+v", running)
	}
	if m := ReadCurating(h.ctx, h.st); m != nil {
		t.Errorf("the mark outlived the curation: %+v", m)
	}
}

// A curation the daemon's own trigger finds due while a round of the
// repository is in its judge stage waits in the queue instead of being
// skipped until a scan lands outside a judge stage.
func TestADueCurationWaitsForTheJudgeStageInTheQueue(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal)
	end := judging(t, h)
	h.advance(curateCheckEvery)
	h.e.maybeCurate(h.ctx)
	h.settle()
	if q := queued(t, h); len(q) != 1 || q[0].Trigger != CurateTriggerOverLimit || q[0].Why != QueuedJudge || len(proposals(t, h)) != 0 {
		t.Fatalf("queue = %+v, proposals %+v", q, proposals(t, h))
	}
	end()
	h.advance(time.Minute) // the next tick, long before the next scan
	h.e.maybeCurate(h.ctx)
	h.settle()
	if ps := proposals(t, h); len(ps) != 1 || ps[0].Trigger != CurateTriggerOverLimit {
		t.Fatalf("proposals = %+v", ps)
	}
}

// A running curation's mark a crashed daemon left behind is gone once the
// next one starts; its queue stays.
func TestADaemonStartClearsTheRunningCurationsMark(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal)
	h.e.setKV(h.ctx, KVNotesCurating, `{"repo":"talkable/talkable","trigger":"request"}`)
	h.e.queueCurate(h.ctx, "talkable/talkable", CurateTriggerRequest, QueuedJudge)
	h.startup()
	if m := ReadCurating(h.ctx, h.st); m != nil {
		t.Errorf("mark after a start = %+v", m)
	}
	if q := queued(t, h); len(q) != 1 {
		t.Errorf("queue after a start = %+v", q)
	}
}

// A request while another curation runs is queued behind it.
func TestACurateRequestWhileAnotherRunsIsQueued(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal, func(h *harness) { h.cfg.Notes.Curate = config.CurateTriggers{} })
	h.e.curateMu.Lock()
	h.e.curateCancel, h.e.curateStarted, h.e.curateRepo = func() {}, h.clock.Now(), "example/tools"
	h.e.curateMu.Unlock()
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
	if err != nil || !strings.Contains(msg, "queued: starts when the running curation (of example/tools") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	h.e.curateMu.Lock()
	h.e.curateCancel = nil
	h.e.curateMu.Unlock()
	h.e.maybeCurate(h.ctx)
	h.settle()
	if ps := proposals(t, h); len(ps) != 1 || len(queued(t, h)) != 0 {
		t.Fatalf("proposals = %+v, queue %+v", ps, queued(t, h))
	}
}

// staleNotes rewrites the first line, which the curated proposal also
// rewrites: the two no longer merge.
const staleNotes = "# Notes for talkable/talkable, as a judge rewrote them\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes #11920\n"

// The operator gives up on a stale proposal: it is superseded (kept, its
// state says so) and a new curation starts from the notes now, reading the
// stale proposal's notes, harness and reasons from its scratch directory.
func TestRequestCurateSupersedesAStaleProposalAndTheCuratorReadsIt(t *testing.T) {
	var sawNotes, sawChanges string
	h, cur, nr := curationHarness(t, func(s notes.Scratch) {
		if b, err := os.ReadFile(filepath.Join(s.Superseded(), "notes.md")); err == nil {
			sawNotes = string(b)
		}
		if b, err := os.ReadFile(filepath.Join(s.Superseded(), "changes.json")); err == nil {
			sawChanges = string(b)
		}
		goodProposal(s)
	}, func(h *harness) { h.cfg.Notes.Curate = config.CurateTriggers{} })
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	old := proposals(t, h)[0]
	if sawNotes != "" {
		t.Fatalf("the first curation read a superseded proposal: %q", sawNotes)
	}
	if strings.Contains(cur.prompts[0], "an earlier curation") {
		t.Fatalf("the first prompt names a superseded proposal:\n%s", cur.prompts[0])
	}
	writeNotes(t, nr, staleNotes, nil)
	h.advance(time.Minute) // the next curation's scratch directory is named by the second
	stale, err := ProposalStale(h.ctx, h.st, nr, old)
	if err != nil || !stale {
		t.Fatalf("ProposalStale = %v, %v", stale, err)
	}
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable", Supersede: old.ID + 1}); err == nil ||
		!strings.Contains(err.Error(), "waits for review") {
		t.Fatalf("superseding another proposal: %v", err)
	}
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable", Supersede: old.ID})
	if err != nil || !strings.Contains(msg, "proposal "+itoa(old.ID)+" superseded; notes curation of talkable/talkable started") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 2 || ps[1].ID != old.ID || ps[1].State != store.ProposalSuperseded || !strings.Contains(ps[1].Reason, "notes changed") ||
		ps[0].State != store.ProposalPending || ps[0].Trigger != CurateTriggerRequest {
		t.Fatalf("proposals = %+v", ps)
	}
	if sawNotes != curatedNotes || !strings.Contains(sawChanges, "one pull request's probe") {
		t.Errorf("the curator read %q and %q", sawNotes, sawChanges)
	}
	if len(cur.prompts) != 2 || !strings.Contains(cur.prompts[1], "superseded/`: proposal "+itoa(old.ID)) {
		t.Errorf("the second prompt does not name the superseded proposal:\n%s", cur.prompts[len(cur.prompts)-1])
	}
	if len(notesEvents(t, h, "notes.proposal_superseded")) != 1 {
		t.Error("no notes.proposal_superseded event")
	}
}

// The daemon supersedes, on its own, a stale proposal a day old whose
// changes no longer merge with the notes'; a stale one that still merges
// waits for the operator.
func TestTheDaemonSupersedesAStaleProposalThatNoLongerMerges(t *testing.T) {
	h, _, nr := curationHarness(t, goodProposal)
	h.advance(curateCheckEvery)
	h.tick()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalPending {
		t.Fatalf("proposals = %+v", ps)
	}
	// A round adds a harness file: stale, but it merges.
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes #11920\n",
		map[string]string{"lint.sh": "bin/rubocop \"$@\"\n"})
	h.advance(25 * time.Hour)
	h.tick()
	if ps := proposals(t, h); len(ps) != 1 || ps[0].State != store.ProposalPending {
		t.Fatalf("a stale proposal that merges was superseded: %+v", ps)
	}
	// The round rewrites the line the proposal rewrites: no merge.
	if err := os.Remove(filepath.Join(nr.Harness(), "lint.sh")); err != nil {
		t.Fatal(err)
	}
	writeNotes(t, nr, staleNotes, nil)
	h.advance(curateCheckEvery)
	h.tick()
	ps = proposals(t, h)
	if len(ps) != 2 || ps[1].State != store.ProposalSuperseded || ps[0].State != store.ProposalPending || ps[0].Trigger != CurateTriggerStale {
		t.Fatalf("proposals = %+v", ps)
	}
}
