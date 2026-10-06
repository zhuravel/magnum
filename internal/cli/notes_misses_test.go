package cli

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// seedMisses stores a repository miss on the harness's PR per title.
func (h *notesHarness) seedMisses(titles ...string) []store.Miss {
	h.t.Helper()
	prs, err := h.st.ListPRs(h.ctx, store.PRFilter{RepoID: h.repo.ID})
	if err != nil || len(prs) == 0 {
		h.t.Fatalf("PRs = %+v, %v", prs, err)
	}
	var out []store.Miss
	for i, title := range titles {
		m, err := h.st.UpsertMiss(h.ctx, store.Miss{PRID: prs[0].ID, SourceURL: "https://example.com/m/" + strconv.Itoa(i), SourceKind: store.MissSourceThread,
			Reviewer: "rev-ann", ReviewedSHA: "abc", Class: store.MissMiss, Severity: "P2", Path: "app/models/coupon.rb", Line: 40 + i,
			Title: title, Lesson: "Scope every finder to the current site.", Scope: store.MissScopeRepo})
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// missesProposal is h.proposal accounting for the misses links name.
func (h *notesHarness) missesProposal(links ...store.ProposalMiss) store.NotesProposal {
	h.t.Helper()
	p := h.proposal()
	q, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "misses",
		BaseVersionID: store.Deref(p.BaseVersionID), Proposed: h.content(*p.VersionID), Changes: p.Changes, State: store.ProposalPending,
		Model: "sonnet", Misses: links, At: h.now})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.DecideNotesProposal(h.ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired, "replaced", h.now, nil); err != nil {
		h.t.Fatal(err)
	}
	return q
}

func (h *notesHarness) content(version int64) *store.NotesContent {
	h.t.Helper()
	c, err := h.st.NotesVersionContent(h.ctx, version)
	if err != nil {
		h.t.Fatal(err)
	}
	return &c
}

func (h *notesHarness) missState(id int64) string {
	h.t.Helper()
	ms, err := h.st.Misses(h.ctx, store.MissFilter{})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m.State
		}
	}
	h.t.Fatalf("no miss %d", id)
	return ""
}

// --review shows, under the diff, every miss the proposal was given with
// what it did with it; y marks the noted and the skipped ones used.
func TestNotesReviewShowsTheMissesAndYMarksThemUsed(t *testing.T) {
	h := newNotesHarness(t)
	ms := h.seedMisses("Coupon lookup ignores the site", "Cache key misses the tenant")
	p := h.missesProposal(store.ProposalMiss{MissID: ms[0].ID, Outcome: store.MissNoted, Section: "Tests"},
		store.ProposalMiss{MissID: ms[1].ID, Outcome: store.MissSkipped, Reason: "the cache is gone"})
	if code := h.cmd("notes", "talkable/talkable", "--review", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var data struct {
		Misses []notesReviewMiss `json:"misses"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil || len(data.Misses) != 2 || data.Misses[0].Outcome != store.MissNoted ||
		data.Misses[0].Where != "app/models/coupon.rb:40" || data.Misses[1].Reason != "the cache is gone" || data.Misses[1].State != store.MissNew {
		t.Fatalf("--json misses = %+v, %v\n%s", data.Misses, err, h.out.String())
	}
	h.out.Reset()
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "misses: 1 noted, 1 skipped",
		"  "+itoa(ms[0].ID)+" P2 app/models/coupon.rb:40 Coupon lookup ignores the site → noted in Tests",
		"  "+itoa(ms[1].ID)+" P2 app/models/coupon.rb:41 Cache key misses the tenant → skipped: the cache is gone",
		"; 2 misses used")
	if strings.Index(out, "misses: 1 noted") < strings.Index(out, "+Run `run_spec.sh <spec>`.") {
		t.Errorf("the misses are not under the diff:\n%s", out)
	}
	for _, m := range ms {
		if s := h.missState(m.ID); s != store.MissUsed {
			t.Errorf("miss %d is %s after the apply", m.ID, s)
		}
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalApplied {
		t.Errorf("proposal = %+v", got)
	}
}

// n sends the misses back to new for the next curation, with the reason; a
// miss in a second rejected proposal is dismissed.
func TestNotesReviewNReturnsTheMissesAndASecondRejectionDismisses(t *testing.T) {
	h := newNotesHarness(t)
	ms := h.seedMisses("Coupon lookup ignores the site")
	h.missesProposal(store.ProposalMiss{MissID: ms[0].ID, Outcome: store.MissNoted, Section: "Tests"})
	h.terminal("n\n")
	if code := h.cmd("notes", "talkable/talkable", "--review", "--reason", "too specific"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "the next curation reads why; 1 miss back to new for the next curation")
	if s := h.missState(ms[0].ID); s != store.MissNew {
		t.Fatalf("after one rejection: %s", s)
	}
	h.now = h.now.Add(time.Hour)
	h.missesProposal(store.ProposalMiss{MissID: ms[0].ID, Outcome: store.MissSkipped, Reason: "covered by Tests"})
	h.out.Reset()
	h.terminal("n\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "1 miss dismissed after 2 rejected proposals")
	if s := h.missState(ms[0].ID); s != store.MissDismissed {
		t.Fatalf("after two rejections: %s", s)
	}
}

// A curation that skips every miss may leave the notes as they are; y
// confirms the skips (the misses are used) and records no second copy of
// the same version.
func TestNotesReviewAppliesAProposalThatOnlySkipsMisses(t *testing.T) {
	h := newNotesHarness(t)
	ms := h.seedMisses("Coupon lookup ignores the site")
	base := h.write(newNotes, map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n"})
	bv := h.record(base, store.NotesFromImport)
	same := engine.ContentOf(base)
	changes, _ := json.Marshal(notes.Changes{
		Sections: []notes.Change{{Name: "Tests", Action: notes.ActionKept, Reason: "every review runs specs"}},
		Files:    []notes.Change{{Name: "run_spec.sh", Action: notes.ActionKept, Reason: "runs any spec"}},
		Misses:   []notes.MissChange{{ID: ms[0].ID, Action: notes.MissSkipped, Reason: "Tests already covers it"}}})
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "misses",
		BaseVersionID: bv.ID, Proposed: &same, Changes: changes, State: store.ProposalPending, At: h.now,
		Misses: []store.ProposalMiss{{MissID: ms[0].ID, Outcome: store.MissSkipped, Reason: "Tests already covers it"}}})
	if err != nil {
		t.Fatal(err)
	}
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "misses: 0 noted, 1 skipped", "→ skipped: Tests already covers it", "1 miss used")
	if s := h.missState(ms[0].ID); s != store.MissUsed {
		t.Fatalf("miss = %s", s)
	}
	hist, _ := h.st.NotesHistory(h.ctx, h.repo.ID, 0)
	if len(hist) != 1 || hist[0].ID != bv.ID {
		t.Errorf("history = %+v, want the base version alone", hist)
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalApplied || store.Deref(got.AppliedVersionID) != bv.ID {
		t.Errorf("proposal = %+v", got)
	}
}

// `magnum misses` shows each miss's state and the latest proposal it was
// given to, with that proposal's state: the one that used it once used.
func TestMissesShowTheirStateAndTheProposalThatUsedIt(t *testing.T) {
	f := missesFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.Misses(ctx, store.MissFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]store.Miss{}
	for _, m := range all {
		byTitle[m.Title] = m
	}
	pending, err := st.CreateNotesProposal(ctx, store.NotesProposalInput{RepoID: repo.ID, Kind: store.ProposalCuration, State: store.ProposalPending,
		Misses: []store.ProposalMiss{{MissID: byTitle["Coupon never expires"].ID, Outcome: store.MissNoted, Section: "Pitfalls"}}})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := missesRun(t, f, "--all")
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if got := norm(lines[0]); got != "PR REVIEWER WHERE CLASS SEV STATE RAISED PROPOSAL TITLE LESSON" {
		t.Errorf("header = %q", got)
	}
	row := func(title string) string {
		for _, l := range lines {
			if strings.Contains(l, title) {
				return norm(l)
			}
		}
		t.Fatalf("no row for %q:\n%s", title, out)
		return ""
	}
	if r := row("Coupon never expires"); !strings.Contains(r, " P1 new rejected:low_confidence "+itoa(pending.ID)+" pending Coupon never expires") {
		t.Errorf("row = %q", r)
	}
	if r := row("Old miss"); !strings.Contains(r, " P2 used Old miss") {
		t.Errorf("row = %q", r)
	}
	var ms []store.Miss
	if err := json.Unmarshal([]byte(missesRun(t, f, "--json")), &ms); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Title == "Coupon never expires" && (m.ProposalID == nil || *m.ProposalID != pending.ID || m.ProposalState != store.ProposalPending) {
			t.Errorf("json = %+v", m)
		}
	}
}

// The retro line counts the PRs that wait for [learn] settle, when there
// are any.
func TestStatusRetroLineCountsThePRsThatWaitForTheSettleDelay(t *testing.T) {
	_, st, d, now := statusFixture(t)
	d.Config.Learn.Enabled = true
	_, out := statusRetroOut(t, d)
	if strings.Contains(out, "settle") {
		t.Errorf("no PR waits, yet:\n%s", out)
	}
	ctx := context.Background()
	for i, ago := range []time.Duration{2 * time.Hour, 30 * time.Hour} {
		closed := now.Add(-ago)
		_, pr := inspSeedPR(t, st, "talkable/talkable", 11900+i, store.PRReleased, func(u *store.PRUpdate) {
			u.Set("gh_state", store.GHMerged)
			u.Set("merged_at", closed)
		})
		run, err := st.CreateRun(ctx, store.Run{ID: "r-" + strconv.Itoa(i), PRID: pr.ID, Round: 1, Role: "codex-judge", Kind: store.RunInitial,
			TargetSHA: "abc", State: store.RunPending})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateRun(ctx, run.ID, func(u *store.RunUpdate) { u.Set("review_id", int64(1+i)) }); err != nil {
			t.Fatal(err)
		}
	}
	r, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    never · 0 new misses · 1 PR waits for the 24h settle delay\n")
	if r.Retro == nil || r.Retro.Settling == nil || *r.Retro.Settling != 1 {
		t.Errorf("retro = %+v", r.Retro)
	}
}
