package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// fakeCritic answers each critique with decide, which writes decision.json
// (turn 0, then 1 after the nudge); a decision still invalid after the
// second turn is returned with its problems, as the pane critic does.
type fakeCritic struct {
	mu      sync.Mutex
	decide  func(job CritiqueJob, turn int)
	prompts []string
	model   string
	closed  int
}

func (f *fakeCritic) Critique(_ context.Context, job CritiqueJob) (CritiqueResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, job.Prompt)
	f.mu.Unlock()
	var d notes.Decision
	var problems []string
	for turn := range 2 {
		f.decide(job, turn)
		if d, problems = job.Check(); len(problems) == 0 {
			break
		}
	}
	return CritiqueResult{Decision: d, Problems: problems, Model: f.model}, nil
}

func (f *fakeCritic) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeCritic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

var promptTarget = regexp.MustCompile(`"target":"([0-9a-f]+)"`)

// writeDecision writes a decision for job's target (the prompt gives it).
func writeDecision(job CritiqueJob, decision, reason string) {
	m := promptTarget.FindStringSubmatch(job.Prompt)
	target := ""
	if m != nil {
		target = m[1]
	}
	b, _ := json.Marshal(notes.Decision{Decision: decision, Target: target, Reason: reason, Losses: []string{"one pull request's probe"},
		Checked: []string{"every removed line", "the archive"}})
	_ = os.WriteFile(job.Run.Decision(), b, 0o600)
}

// decides is a critic that always answers decision with reason.
func decides(decision, reason string) func(CritiqueJob, int) {
	return func(job CritiqueJob, _ int) { writeDecision(job, decision, reason) }
}

// criticHarness is a curation harness with the critic on and a fake
// critic.
func criticHarness(t *testing.T, decide func(CritiqueJob, int), mods ...func(*harness)) (*harness, *fakeCritic, notes.Repo) {
	t.Helper()
	cr := &fakeCritic{decide: decide, model: "fable"}
	mods = append([]func(*harness){func(h *harness) {
		h.cfg.Notes.Critic = true
		h.d.Critic = func(context.Context, CriticRun) (Critic, error) { return cr, nil }
	}}, mods...)
	h, _, nr := curationHarness(t, goodProposal, mods...)
	return h, cr, nr
}

func critiques(t *testing.T, h *harness, proposal int64) []store.NotesCritique {
	t.Helper()
	cs, err := h.st.NotesCritiques(h.ctx, store.NotesCritiqueFilter{ProposalID: proposal})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// toastText flushes the batched toasts and returns every toast shown.
func toastText(h *harness) string {
	if h.e.batch != nil {
		_, _ = h.e.batch.Flush(h.ctx)
	}
	return strings.Join(h.nh.all(), "\n")
}

// decidedEvent is the message of the newest notes.critic_decided event.
func decidedEvent(t *testing.T, h *harness) string {
	t.Helper()
	evs := notesEvents(t, h, "notes.critic_decided")
	if len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Message
}

func setPause(h *harness, on bool) {
	if on {
		h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
		return
	}
	h.e.delKV(h.ctx, store.KVDaemonPaused)
}

// The critic applies a valid proposal through the claim, the write and the
// record: the proposal is applied, decided by the critic, the notes hold
// its version, the registry keeps the critique with its model, reason,
// checked and losses, the decision is the one toast (no "is ready"), and
// the decision event names the model.
func TestTheCriticAppliesAValidProposalAndRecordsTheDeciderAndTheModel(t *testing.T) {
	h, cr, nr := criticHarness(t, decides(notes.DecisionApply, "keeps every decision"))
	p := curatedProposal(t, h)
	if strings.Contains(toastText(h), "is ready") {
		t.Errorf("a ready toast with the critic on:\n%s", toastText(h))
	}
	h.tick()
	h.settle()
	if cr.count() != 1 {
		t.Fatalf("critiques started = %d", cr.count())
	}
	got := proposalNow(t, h, p.ID)
	if got.DecidedBy != store.DecidedByCritic || got.Reason != "keeps every decision" {
		t.Errorf("decided by %q, reason %q", got.DecidedBy, got.Reason)
	}
	liveMatchesApplied(t, h, nr, got)
	cs := critiques(t, h, p.ID)
	if len(cs) != 1 || cs[0].Outcome != store.CritiqueApplied || cs[0].Decision != notes.DecisionApply || cs[0].Model != "fable" ||
		cs[0].Reason != "keeps every decision" || len(cs[0].Checked) != 2 || len(cs[0].Losses) != 1 || cs[0].RunDir == "" {
		t.Fatalf("critiques = %+v", cs)
	}
	if titles := toastText(h); !strings.Contains(titles, "proposal "+itoa(p.ID)+" of talkable/talkable applied by the critic (fable)") {
		t.Errorf("toasts:\n%s", titles)
	}
	if evs := notesEvents(t, h, "notes.critic_decided"); len(evs) != 1 || !strings.Contains(evs[0].Message, "fable") {
		t.Errorf("decided events = %+v", evs)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesCritic); v != "" {
		t.Errorf("the critique's mark stays: %q", v)
	}
}

// The notes changed between the critique and its apply (a judge rewrote a
// harness script the proposal keeps): the clean merge is not applied on the
// critic's word, the critique ends changed (no attempt), and the next tick
// critiques the proposal again against the new notes, whose decision
// applies the merge.
func TestAStaleProposalWithACleanMergeIsCritiquedAgainNotApplied(t *testing.T) {
	var nr notes.Repo
	turns := 0
	h, cr, nr := criticHarness(t, func(job CritiqueJob, turn int) {
		turns++
		if turns == 1 { // a judge rewrites a script while the first critique runs
			cur, _ := notes.ReadState(nr)
			for i := range cur.Files {
				if cur.Files[i].Path == "run_spec.sh" {
					cur.Files[i].Body = []byte("bin/rspec --fail-fast \"$@\"\n")
					cur.Files[i].SHA256 = notes.TextSHA(cur.Files[i].Body)
				}
			}
			_ = notes.WriteState(nr, cur)
		}
		writeDecision(job, notes.DecisionApply, "")
	})
	p := curatedProposal(t, h)
	before := readLive(t, nr)
	h.tick()
	h.settle()
	cs := critiques(t, h, p.ID)
	if len(cs) != 1 || cs[0].Outcome != store.CritiqueChanged {
		t.Fatalf("the first critique = %+v, want changed", cs)
	}
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalPending {
		t.Fatalf("the proposal after a changed critique = %+v", got)
	}
	if n, _ := CriticAttempts(h.ctx, h.st, p.ID); n != 0 {
		t.Errorf("attempts = %d, want 0", n)
	}
	if live := readLive(t, nr); live.Same(before) {
		t.Fatal("the judge's change is gone")
	}
	h.tick()
	h.settle()
	if cr.count() != 2 {
		t.Fatalf("critiques = %d, want a second one", cr.count())
	}
	got := proposalNow(t, h, p.ID)
	liveMatchesApplied(t, h, nr, got)
	live := readLive(t, nr)
	script := ""
	for _, b := range live.Files {
		if b.Path == "run_spec.sh" {
			script = string(b.Body)
		}
	}
	if !strings.Contains(string(live.Notes), "run_spec.sh <spec>") || !strings.Contains(script, "--fail-fast") {
		t.Fatalf("the merge = %q, run_spec.sh %q", live.Notes, script)
	}
	if cs := critiques(t, h, p.ID); len(cs) != 2 || cs[0].Outcome != store.CritiqueApplied || cs[0].Target == cs[1].Target {
		t.Errorf("critiques = %+v: the second reviewed another target", cs)
	}
}

// A rejection by the critic keeps its reason, marks the repository, and
// the next curation scan starts a curation of trigger review that reads the
// reason; its stored proposal clears the mark. Under `magnum pause` no such
// curation starts.
func TestACriticRejectionStartsAReviewCurationThatReadsTheReason(t *testing.T) {
	h, _, _ := criticHarness(t, decides(notes.DecisionReject, "keep the QA steps for the admin"))
	p := curatedProposal(t, h)
	h.tick()
	h.settle()
	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalRejected || got.DecidedBy != store.DecidedByCritic || got.Reason != "keep the QA steps for the admin" {
		t.Fatalf("the proposal = %+v", got)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesRecurate("talkable/talkable")); v != itoa(p.ID) {
		t.Fatalf("recurate mark = %q", v)
	}
	if titles := toastText(h); !strings.Contains(titles, "rejected by the critic (fable)") {
		t.Errorf("toasts:\n%s", titles)
	}
	setPause(h, true)
	h.advance(11 * time.Minute)
	h.tick()
	h.settle()
	if ps := proposals(t, h); len(ps) != 1 {
		t.Fatalf("a review curation under a pause: %+v", ps)
	}
	setPause(h, false)
	h.advance(11 * time.Minute)
	h.tick()
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 2 || ps[0].Trigger != CurateTriggerReview {
		t.Fatalf("proposals = %+v, want a review curation", ps)
	}
	usage, err := os.ReadFile(filepath.Join(ps[0].Scratch, "usage.json"))
	if err != nil || !strings.Contains(string(usage), "keep the QA steps for the admin") || !strings.Contains(string(usage), `"by": "critic"`) {
		t.Errorf("usage.json = %s, %v", usage, err)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesRecurate("talkable/talkable")); v != "" {
		t.Errorf("the mark stays after the curation stored its proposal: %q", v)
	}
}

// lossyProposal stores a pending curation of the harness's notes that
// drops a line it keeps no archive entry for: the critic may only reject it
// or send it back.
func lossyProposal(t *testing.T, h *harness, nr notes.Repo) store.NotesProposal {
	t.Helper()
	base, err := h.st.LatestNotesVersion(h.ctx, repoID(t, h))
	if err != nil {
		t.Fatal(err)
	}
	cur := readLive(t, nr)
	lossy := ContentOf(notes.State{Exists: true, Notes: []byte("# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n"), Files: cur.Files})
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: repoID(t, h), Kind: store.ProposalCuration, Trigger: "misses",
		BaseVersionID: base.ID, Proposed: &lossy, Changes: []byte(`{"sections":[{"name":"Tests","action":"kept","reason":"r"}],"files":[]}`),
		State: store.ProposalPending})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The critic sends a proposal that would lose lines back to the curator:
// superseded, decided by the critic, and the curation that follows reads it
// and the reason (superseded/reason.txt).
func TestTheCriticRecuratesALossyProposalAndTheCuratorReadsTheReason(t *testing.T) {
	h, _, nr := criticHarness(t, decides(notes.DecisionRecurate, "archive the probe line instead of dropping it"))
	p := lossyProposal(t, h, nr)
	h.tick()
	h.settle()
	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalSuperseded || got.DecidedBy != store.DecidedByCritic {
		t.Fatalf("the lossy proposal = %+v", got)
	}
	h.advance(11 * time.Minute)
	h.tick()
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 2 || ps[0].Trigger != CurateTriggerReview {
		t.Fatalf("proposals = %+v", ps)
	}
	reason, err := os.ReadFile(filepath.Join(ps[0].Scratch, "superseded", "reason.txt"))
	if err != nil || !strings.Contains(string(reason), "archive the probe line") {
		t.Errorf("superseded/reason.txt = %q, %v", reason, err)
	}
}

// Rejections and re-curations by the critic in a row reach [notes]
// critic_declines: no re-curation is marked, a toast says it stepped back,
// and the repository's next proposal waits for the operator.
func TestTheCriticStepsBackAfterDeclinesInARow(t *testing.T) {
	h, cr, _ := criticHarness(t, decides(notes.DecisionReject, "keep the QA steps for the admin"), func(h *harness) { h.cfg.Notes.CriticDeclines = 2 })
	p1 := curatedProposal(t, h)
	h.tick()
	h.settle()
	h.advance(11 * time.Minute)
	h.tick() // the review curation
	h.settle()
	h.tick() // its critique
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 2 || ps[0].State != store.ProposalRejected || proposalNow(t, h, p1.ID).State != store.ProposalRejected {
		t.Fatalf("proposals = %+v", ps)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesRecurate("talkable/talkable")); v != "" {
		t.Errorf("a re-curation was marked at the cap: %q", v)
	}
	if titles := toastText(h); !strings.Contains(titles, "rejected by the critic") {
		t.Errorf("toasts:\n%s", titles)
	}
	if msg := decidedEvent(t, h); !strings.Contains(msg, "stepped back") {
		t.Errorf("the decision does not say the critic stepped back: %q", msg)
	}
	h.advance(time.Minute) // a curation's directory is named by the second
	p3 := curatedProposal(t, h)
	h.tick()
	h.settle()
	if cr.count() != 2 || proposalNow(t, h, p3.ID).State != store.ProposalPending {
		t.Fatalf("critiques = %d, the third proposal = %+v; want it waiting for the operator", cr.count(), proposalNow(t, h, p3.ID))
	}
}

// Two critiques without a valid decision are two attempts: the proposal is
// left to the operator for good, across a daemon restart, with one toast.
func TestTwoUndecidedAttemptsLeaveAProposalToTheOperatorAcrossARestart(t *testing.T) {
	h, cr, _ := criticHarness(t, func(job CritiqueJob, _ int) {
		_ = os.WriteFile(job.Run.Decision(), []byte(`{"decision":"maybe"}`), 0o600)
	})
	p := curatedProposal(t, h)
	for range 2 {
		h.tick()
		h.settle()
	}
	cs := critiques(t, h, p.ID)
	if len(cs) != 2 || cs[0].Outcome != store.CritiqueUndecided || cs[1].Outcome != store.CritiqueUndecided || cs[0].Detail == "" {
		t.Fatalf("critiques = %+v", cs)
	}
	if titles := toastText(h); strings.Count(titles, "the critic did not decide proposal "+itoa(p.ID)) != 1 {
		t.Errorf("toasts:\n%s", titles)
	}
	h.startup()
	h.tick()
	h.settle()
	if cr.count() != 2 || proposalNow(t, h, p.ID).State != store.ProposalPending {
		t.Fatalf("critiques = %d after a restart; the proposal = %+v", cr.count(), proposalNow(t, h, p.ID))
	}
}

// The operator decides the proposal while the critic works on it: the
// critic's decision is dropped with an info event, no toast and no attempt.
func TestTheOperatorsDecisionDuringACritiqueIsAnEventAndNothingElse(t *testing.T) {
	var h *harness
	h, _, _ = criticHarness(t, func(job CritiqueJob, _ int) {
		ps, _ := h.st.NotesProposals(h.ctx, store.NotesProposalFilter{States: []string{store.ProposalPending}})
		for _, p := range ps {
			_, _ = h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByOperator, "mine", h.clock.Now(), nil)
		}
		writeDecision(job, notes.DecisionApply, "")
	})
	p := curatedProposal(t, h)
	before := toastText(h)
	h.tick()
	h.settle()
	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalRejected || got.DecidedBy != store.DecidedByOperator {
		t.Fatalf("the proposal = %+v", got)
	}
	if cs := critiques(t, h, p.ID); len(cs) != 1 || cs[0].Outcome != store.CritiqueOvertaken {
		t.Fatalf("critiques = %+v", cs)
	}
	if evs := notesEvents(t, h, "notes.critic_overtaken"); len(evs) != 1 {
		t.Errorf("overtaken events = %+v", evs)
	}
	if after := toastText(h); after != before {
		t.Errorf("toasts after the overtaken critique:\n%s\nwant\n%s", after, before)
	}
}

// The daily retro due and unrun today goes first: no critique starts until
// it ran.
func TestTheRetroDueTodayRunsBeforeAWaitingCritique(t *testing.T) {
	h, cr, _ := criticHarness(t, decides(notes.DecisionApply, ""), func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "00:00"
	})
	p := curatedProposal(t, h)
	h.e.delKV(h.ctx, KVRetroDay) // due and not run today
	if !h.e.retroWaiting(h.ctx) {
		t.Fatal("the retro is not due")
	}
	h.e.maybeCritic(h.ctx)
	h.settle()
	if cr.count() != 0 {
		t.Fatalf("a critique started before the retro due today")
	}
	h.e.setKV(h.ctx, KVRetroDay, store.DayKey(h.clock.Now()))
	h.e.maybeCritic(h.ctx)
	h.settle()
	if cr.count() != 1 || proposalNow(t, h, p.ID).State != store.ProposalApplied {
		t.Fatalf("critiques = %d once the retro ran", cr.count())
	}
}

// [notes] critic_per_day caps the critiques started per local day.
func TestTheCriticsDailyCapHolds(t *testing.T) {
	h, cr, _ := criticHarness(t, func(job CritiqueJob, _ int) {
		_ = os.WriteFile(job.Run.Decision(), []byte(`{"decision":"maybe"}`), 0o600)
	},
		func(h *harness) { h.cfg.Notes.CriticPerDay = 1 })
	curatedProposal(t, h)
	for range 3 {
		h.tick()
		h.settle()
	}
	if cr.count() != 1 {
		t.Fatalf("critiques = %d, want 1 a day", cr.count())
	}
	h.advance(24 * time.Hour)
	h.tick()
	h.settle()
	if cr.count() != 2 {
		t.Fatalf("critiques = %d the next day, want 2", cr.count())
	}
}

// A critic apply that leaves the notes past a limit says so and delays the
// next automatic curation for size to a week.
func TestACriticApplyLeftOverALimitDelaysTheNextSizeCurationToAWeek(t *testing.T) {
	h, _, nr := criticHarness(t, decides(notes.DecisionApply, ""), func(h *harness) { h.cfg.Notes.MaxBytes = 32 })
	curatedProposal(t, h)
	h.tick()
	h.settle()
	if _, ok := h.e.kvTime(h.ctx, KVNotesCriticOver("talkable/talkable")); !ok {
		t.Fatal("no mark of notes left past a limit")
	}
	if msg := decidedEvent(t, h); !strings.Contains(msg, "next automatic curation for size is in 7 days") {
		t.Errorf("the decision does not say the next curation is in a week: %q", msg)
	}
	cur := readLive(t, nr)
	cur.Notes = append(cur.Notes, []byte("- a judge's new lesson\n")...)
	if err := notes.WriteState(nr, cur); err != nil {
		t.Fatal(err)
	}
	h.startup() // the notes are recorded and measured past max_bytes
	n := len(proposals(t, h))
	h.advance(25 * time.Hour)
	h.tick()
	h.settle()
	if got := len(proposals(t, h)); got != n {
		t.Fatalf("a curation for size a day after the critic's apply: %d proposals, want %d", got, n)
	}
	h.advance(6 * 24 * time.Hour)
	h.tick()
	h.settle()
	if ps := proposals(t, h); len(ps) != n+1 || ps[0].Trigger != CurateTriggerOverLimit {
		t.Fatalf("proposals = %+v, want an over_limit curation a week later", ps)
	}
}

// The critic's toasts and keys need the critic: with it off, a curation's
// proposal toasts that it is ready and nothing critiques it.
func TestWithTheCriticOffNothingCritiques(t *testing.T) {
	h, cr, _ := criticHarness(t, decides(notes.DecisionApply, ""), func(h *harness) { h.cfg.Notes.Critic = false })
	p := curatedProposal(t, h)
	h.tick()
	h.settle()
	if cr.count() != 0 || proposalNow(t, h, p.ID).State != store.ProposalPending {
		t.Fatalf("critiques = %d with the critic off", cr.count())
	}
	if !strings.Contains(toastText(h), "is ready") {
		t.Errorf("no ready toast with the critic off:\n%s", toastText(h))
	}
}
