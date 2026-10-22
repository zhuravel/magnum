package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// critique records a critique of proposal p that ran from start for took,
// on model, and ended as end says (end.Outcome "" leaves it running).
func (h *notesHarness) critique(p store.NotesProposal, start time.Time, took time.Duration, model, runDir string, end store.CritiqueEnd) int64 {
	h.t.Helper()
	id, err := h.st.StartCritique(h.ctx, store.CritiqueStart{ProposalID: p.ID, RepoID: h.repo.ID, RunID: start.Format("20060102-150405"),
		RunDir: runDir, Model: model, Target: "f1e2d3", At: start})
	if err != nil {
		h.t.Fatal(err)
	}
	if end.Outcome != "" {
		end.At = start.Add(took)
		if err := h.st.EndCritique(h.ctx, id, end); err != nil {
			h.t.Fatal(err)
		}
	}
	return id
}

// criticApply applies p as the critic does, through the claim, the write
// and the record (engine.ApplyNotesProposal).
func (h *notesHarness) criticApply(p store.NotesProposal) store.NotesProposal {
	h.t.Helper()
	data, err := engine.ReviewProposal(h.ctx, h.st, 0, h.repo.FullName(), h.nr, p)
	if err != nil {
		h.t.Fatal(err)
	}
	target, _ := data.Target()
	applied, err := engine.ApplyNotesProposal(h.ctx, h.st, h.nr, p, engine.NotesApply{Live: data.Check.Live.Fingerprint(),
		Target: target.Fingerprint(), By: store.DecidedByCritic, Now: h.now, Wait: time.Second})
	if err != nil {
		h.t.Fatal(err)
	}
	return applied
}

// criticMark records the critique running now.
func (h *notesHarness) criticMark(p store.NotesProposal, model string, started time.Time) {
	h.t.Helper()
	b, _ := json.Marshal(engine.CriticMark{Repo: h.repo.FullName(), Proposal: p.ID, Model: model, Started: started})
	if err := h.st.SetKV(h.ctx, engine.KVNotesCritic, string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// --decisions reads the critic's record from the registry: the critiques
// of a proposal with their reason, checked and losses lists and detail,
// after the run directories are gone (they are pruned after 30 days, the
// registry keeps every critique). What the critic wrote is cleaned of
// terminal controls, and a run directory is named only while it exists.
func TestDecisionsPrintCheckedLossesAndTheReasonFromTheRegistryAfterTheRunDirectoryIsDeleted(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	gone := filepath.Join(t.TempDir(), "20261003-130000")
	h.critique(p, h.now, 20*time.Minute, "fable", gone, store.CritiqueEnd{Outcome: store.CritiqueUndecided,
		Detail: "decision.json: target names another state"})
	h.now = h.now.Add(time.Hour)
	h.critique(p, h.now, 10*time.Minute, "fable", gone+"-2", store.CritiqueEnd{Outcome: store.CritiqueApplied, Decision: notes.DecisionApply,
		Reason: "keeps every standing decision", Checked: []string{"the \x1b[31mTests\x1b[0m section's commands", "the archive holds the probe list"},
		Losses: []string{"the probe list of one pull request"}})
	h.now = h.now.Add(10 * time.Minute)
	applied := h.criticApply(p)
	if applied.State != store.ProposalApplied {
		t.Fatalf("applied = %+v", applied)
	}

	if code := h.cmd("notes", "talkable/talkable", "--decisions", itoa(p.ID)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "proposal "+itoa(p.ID)+" for the notes of talkable/talkable: a curation (over_limit) by sonnet",
		"applied by the critic (fable) at "+applied.DecidedAt.Local().Format("2006-01-02 15:04"), "critiques: 2 (1 applied, 1 undecided)",
		"undecided", "detail: decision.json: target names another state",
		"applied (decision apply)", "target: f1e2d3", "reason: keeps every standing decision",
		"checked:", "- the  [31mTests [0m section's commands", "- the archive holds the probe list",
		"losses:", "- the probe list of one pull request")
	if strings.Contains(out, "\x1b") || strings.Contains(out, "run directory") {
		t.Errorf("a terminal control or a deleted run directory in the output:\n%s", out)
	}
	if i, j := strings.Index(out, "undecided\n"), strings.Index(out, "applied (decision apply)"); i < 0 || j < i {
		t.Errorf("the critiques are not oldest first:\n%s", out)
	}

	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--decisions"); code != 0 {
		t.Fatalf("list: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "PROPOSAL", itoa(p.ID), "curation (over_limit)", "applied", "the critic (fable)", "1 applied, 1 undecided",
		"keeps every standing decision")

	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--decisions="+itoa(p.ID), "--json"); code != 0 {
		t.Fatalf("--json: exit %d: %s", code, h.errb.String())
	}
	var one notesDecisionsOne
	if err := json.Unmarshal(h.out.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.Proposal.ID != p.ID || one.Proposal.DecidedBy != store.DecidedByCritic || one.Proposal.CriticModel != "fable" || len(one.Critiques) != 2 {
		t.Fatalf("--json = %+v", one)
	}
	if c := one.Critiques[1]; c.Outcome != store.CritiqueApplied || c.Reason != "keeps every standing decision" || len(c.Checked) != 2 ||
		len(c.Losses) != 1 || c.RunDirExists {
		t.Errorf("--json critique = %+v", c)
	}

	// A run directory still on disk is named.
	p2 := h.proposal()
	kept := t.TempDir()
	h.critique(p2, h.now, time.Minute, "opus", kept, store.CritiqueEnd{Outcome: store.CritiqueStopped, Detail: "the daemon is stopping"})
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--decisions", itoa(p2.ID)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "waits for a decision", "stopped", "detail: the daemon is stopping", "run directory: "+kept)

	// A proposal of another repository, or none, is refused.
	if code := h.cmd("notes", "talkable/talkable", "--decisions", "999"); code == 0 {
		t.Error("an unknown proposal was shown")
	}
	actContains(t, h.errb.String(), "proposal 999 is not a proposal of the notes of talkable/talkable")
}

// The list of --decisions shows the newest proposals first, a capped
// number of them, and says how many more --json prints.
func TestDecisionsListsTheNewestProposalsAndSaysHowManyMore(t *testing.T) {
	h := newNotesHarness(t)
	var last store.NotesProposal
	for range notesDecisionsShown + 2 {
		p := h.proposal()
		if _, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByOperator,
			"keep the probes", h.now, nil); err != nil {
			t.Fatal(err)
		}
		last = p
	}
	if code := h.cmd("notes", "talkable/talkable", "--decisions"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != notesDecisionsShown+1 || !strings.HasPrefix(lines[1], itoa(last.ID)+" ") {
		t.Fatalf("list:\n%s", h.out.String())
	}
	actContains(t, h.errb.String(), "2 older proposals not shown (--json prints them all)")
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--decisions", "--json"); code != 0 {
		t.Fatal(code)
	}
	var all notesDecisionsList
	if err := json.Unmarshal(h.out.Bytes(), &all); err != nil || len(all.Proposals) != notesDecisionsShown+2 || all.Proposals[0].ID != last.ID {
		t.Fatalf("--json = %d proposals, %v", len(all.Proposals), err)
	}
	if code := h.cmd("notes", "--decisions"); code != 2 {
		t.Errorf("--decisions without a repository: exit %d", code)
	}
}

// With the critic on, the list says a critique runs (its model and how
// long), a proposal the critic stepped back from and one it left to the
// operator with the time left before it expires; --json carries the same.
func TestTheListShowsTheCriticRunningAndWhatItLeftToYou(t *testing.T) {
	h := newNotesHarness(t)
	prev := inspNow
	inspNow = func() time.Time { return h.now }
	t.Cleanup(func() { inspNow = prev })
	h.c.Config.Notes.Critic = true
	p := h.proposal()
	h.criticMark(p, "fable", h.now.Add(-2*time.Minute))
	row := func() notesRow {
		t.Helper()
		h.out.Reset()
		if code := h.cmd("notes", "--json"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.errb.String())
		}
		var out notesListJSON
		if err := json.Unmarshal(h.out.Bytes(), &out); err != nil || len(out.Repos) != 1 {
			t.Fatalf("--json = %s, %v", h.out.String(), err)
		}
		h.out.Reset()
		if code := h.cmd("notes"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.errb.String())
		}
		return out.Repos[0]
	}
	if r := row(); r.State != notesCritic || r.Critic == nil || r.Critic.Model != "fable" || r.Critic.Proposal != p.ID {
		t.Fatalf("a critique running: %+v", r)
	}
	actContains(t, h.out.String(), "critic running (fable, 2m)")

	// Two undecided critiques leave the proposal to the operator.
	if err := h.st.DeleteKV(h.ctx, engine.KVNotesCritic); err != nil {
		t.Fatal(err)
	}
	for range engine.CriticMaxAttempts {
		h.critique(p, h.now, time.Minute, "fable", "", store.CritiqueEnd{Outcome: store.CritiqueUndecided, Detail: "no decision.json"})
	}
	h.now = p.CreatedAt.Add(5 * 24 * time.Hour)
	if r := row(); r.State != notesProposal || r.Proposal == nil || !r.Proposal.CriticLeft || r.Proposal.CriticSteppedBack ||
		!r.Proposal.ExpiresAt.Equal(p.CreatedAt.Add(engine.ProposalTTL)) {
		t.Fatalf("left to the operator: %+v", r.Proposal)
	}
	actContains(t, h.out.String(), "proposal "+itoa(p.ID)+" to review (over_limit, 5d; the critic did not decide it, expires in 2d)")

	// The critic declined critic_declines proposals in a row: it steps back.
	if _, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByCritic,
		"keep the decisions", h.now, nil); err != nil {
		t.Fatal(err)
	}
	p2 := h.proposal()
	if _, err := h.st.DecideNotesProposal(h.ctx, p2.ID, []string{store.ProposalPending}, store.ProposalSuperseded, store.DecidedByCritic,
		"merge the two test sections", h.now, nil); err != nil {
		t.Fatal(err)
	}
	p3 := h.proposal()
	if r := row(); r.Proposal == nil || r.Proposal.ID != p3.ID || !r.Proposal.CriticSteppedBack || r.Proposal.CriticLeft {
		t.Fatalf("stepped back: %+v", r.Proposal)
	}
	actContains(t, h.out.String(), "proposal "+itoa(p3.ID)+" to review (over_limit, 0s; the critic stepped back)")

	// With the critic off the list says nothing of it.
	h.c.Config.Notes.Critic = false
	if r := row(); r.Proposal.CriticSteppedBack || r.Proposal.CriticLeft || strings.Contains(h.out.String(), "critic") {
		t.Errorf("critic off: %+v\n%s", r.Proposal, h.out.String())
	}
}

// `magnum status` and the dashboard's notes line count a repository the
// critic works on, where they name a curation running.
func TestTheStatusLineSaysCriticOnARepository(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	h.criticMark(p, "fable", h.now.Add(-time.Minute))
	_, n := notesLimits(h.c)
	rows, err := notesOverview(h.ctx, h.c.Layout, h.st, n)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := notesSummaryLine(rows), "1 repo · 1 proposal to review (talkable/talkable) · critic on talkable/talkable"; got != want {
		t.Errorf("line = %q\nwant   %q", got, want)
	}
}

// `magnum notes <repo>` names the last decision under the notes: who made
// it (the critic with its model, you, magnum), when and why.
func TestTheSummaryNamesTheLastDecision(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	if _, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByOperator,
		"keep the probes", h.now, nil); err != nil {
		t.Fatal(err)
	}
	summary := func() string {
		t.Helper()
		h.errb.Reset()
		h.out.Reset()
		if code := h.cmd("notes", "talkable/talkable"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.errb.String())
		}
		return h.errb.String()
	}
	actContains(t, summary(), "proposal "+itoa(p.ID)+" rejected by you at "+h.now.Local().Format("2006-01-02 15:04")+": keep the probes")

	p2 := h.proposal()
	h.critique(p2, h.now, time.Minute, "fable", "", store.CritiqueEnd{Outcome: store.CritiqueRecurated, Decision: notes.DecisionRecurate,
		Reason: "merge the two test sections"})
	if _, err := h.st.DecideNotesProposal(h.ctx, p2.ID, []string{store.ProposalPending}, store.ProposalSuperseded, store.DecidedByCritic,
		"merge the two test sections", h.now, nil); err != nil {
		t.Fatal(err)
	}
	actContains(t, summary(), "proposal "+itoa(p2.ID)+" re-curated by the critic (fable) at "+h.now.Local().Format("2006-01-02 15:04")+
		": merge the two test sections")

	h.now = h.now.Add(time.Hour)
	p3 := h.proposal()
	h.critique(p3, h.now, time.Minute, "opus", "", store.CritiqueEnd{Outcome: store.CritiqueApplied, Decision: notes.DecisionApply,
		Reason: "keeps every standing decision"})
	applied := h.criticApply(p3)
	out := summary()
	actContains(t, out, "proposal "+itoa(p3.ID)+" applied by the critic (opus) at "+applied.DecidedAt.Local().Format("2006-01-02 15:04")+
		": keeps every standing decision")
	if strings.Contains(out, "re-curated") {
		t.Errorf("an older decision named:\n%s", out)
	}
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--json"); code != 0 {
		t.Fatal(code)
	}
	var v notesView
	if err := json.Unmarshal(h.out.Bytes(), &v); err != nil || v.LastDecision == nil || v.LastDecision.ID != p3.ID ||
		v.LastDecision.CriticModel != "opus" || v.LastDecision.State != store.ProposalApplied {
		t.Errorf("--json = %+v, %v", v.LastDecision, err)
	}
}

// --review of a proposal the critic works on says so before it asks: y
// applies anyway, and the claim settles who wins.
func TestReviewSaysTheCriticIsOnTheProposal(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	h.criticMark(p, "fable", h.now.Add(-2*time.Minute))
	h.terminal("\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	note := "the critic is on proposal " + itoa(p.ID) + " (fable, 2m). y applies anyway"
	if i, j := strings.Index(out, note), strings.Index(out, "Apply proposal"); i < 0 || j < i {
		t.Fatalf("the note does not come before the question:\n%s", out)
	}

	b, _ := json.Marshal(engine.CriticMark{Repo: "talkable/talkable", Proposal: p.ID + 100, Model: "fable", Started: h.now})
	if err := h.st.SetKV(h.ctx, engine.KVNotesCritic, string(b)); err != nil {
		t.Fatal(err)
	}
	h.out.Reset()
	h.terminal("\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(h.out.String(), "the critic is on") {
		t.Errorf("a critique of another proposal was named:\n%s", h.out.String())
	}
}

// `magnum logs notes:<owner>/<repo>` follows a repository's curations and
// critiques, whatever the case of the name.
func TestLogsFollowsTheNotesOfARepository(t *testing.T) {
	_, st, _ := logsFixture(t)
	inspAppendEvent(t, st, "notes:talkable/talkable", "notes.critic_decided", "proposal 3 applied by the critic (fable)")
	inspAppendEvent(t, st, "notes:talkable/talkable-api", "notes.critic_decided", "another repository")
	for _, arg := range []string{"notes:talkable/talkable", "notes:Talkable/Talkable"} {
		m, _, err := logsMatchers(context.Background(), st, app.RefParser{DefaultRepo: "talkable/talkable"}, arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		evs, err := st.EventsMatching(context.Background(), m, 0, 0)
		if err != nil || len(evs) != 1 || evs[0].Message != "proposal 3 applied by the critic (fable)" {
			t.Errorf("%s: events = %+v, %v", arg, evs, err)
		}
	}
}

// `magnum config` renders the critic's prompt with the others and fails on
// one this build cannot render.
func TestConfigFailsOnACriticPromptThisBuildCannotRender(t *testing.T) {
	f := newInspFixture(t)
	cfg := config.Defaults()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes-critic.md"), []byte("{{if .Repo}}{{.Missing}}{{end}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Pipeline.PromptsDir = dir
	c := &Context{Version: "test", Layout: f.Ctx.Layout, Config: cfg, Stdout: f.Out, Stderr: f.Err}
	if code := execute(c, []string{"config"}); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, f.Out.String())
	}
	actContains(t, f.Err.String(), "prompts do not render with this build", "notes critic prompt", "notes-critic.md")
}

// `magnum doctor` checks the critic's kind (its CLI on PATH, its login)
// when the critic is on, as it checks the kinds the roles use; with the
// critic off a kind only the critic would use is not probed.
func TestDoctorChecksTheCriticsKindWhenTheCriticIsOn(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	cfg := d.Config
	cfg.Roles = []config.Role{{Name: "claude-judge", Kind: config.KindClaude, Judge: true, Prompt: "judge-initial.md"}}
	cfg.Notes.CriticKind, cfg.Notes.CriticModel = config.KindCodex, ""
	cfg.Normalize()
	f := d.Run.(*execx.Fake)
	if m := doctorByName(doctorAgentChecks(context.Background(), d)); m["codex"].Name != "" || len(f.CallsWithPrefix("zsh", "-ic", "whence -w codex")) != 0 {
		t.Fatalf("the critic's kind was checked with the critic off: %+v", m["codex"])
	}
	cfg.Notes.Critic = true
	m := doctorByName(doctorAgentChecks(context.Background(), d))
	if c := m["codex"]; c.Status != doctorPass || !strings.Contains(c.Detail, "used by notes-critic") {
		t.Errorf("codex = %+v", c)
	}
	if c := m["codex login"]; c.Status != doctorPass {
		t.Errorf("codex login = %+v", c)
	}
	if c := m["claude"]; !strings.Contains(c.Detail, "used by claude-judge") || strings.Contains(c.Detail, "notes-critic") {
		t.Errorf("claude = %+v", c)
	}
}
