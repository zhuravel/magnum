package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// The daily retro and a plain `magnum retro` take a PR only once it closed
// [learn] settle ago (24h by default), so a review posted right after the
// merge is in; the retro's start event counts the PRs that wait. `magnum
// retro <ref>` takes the PRs it names whenever they closed.
func TestTheRetroWaitsForTheSettleDelay(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "09:00"
	})
	fresh := retroPR(h, 7, 23*time.Hour)
	settled := retroPR(h, 8, 25*time.Hour)
	young := retroPR(h, 9, time.Hour)
	for _, n := range []int{7, 8, 9} {
		seedRetroGitHub(h, n)
	}
	looked := func(pr store.PR) bool {
		_, err := h.st.RetroPRByID(h.ctx, pr.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	h.tick() // past daily_at: the day's retro
	if !looked(settled) || looked(fresh) || looked(young) {
		t.Fatalf("daily retro looked at 25h %v, 23h %v, 1h %v; want only the PR closed 25 hours ago", looked(settled), looked(fresh), looked(young))
	}
	starts, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, "retro.start")
	if err != nil || len(starts) != 1 {
		t.Fatalf("retro.start events = %+v, %v", starts, err)
	}
	if start := starts[0]; !strings.Contains(start.Message, "2 PRs wait for the 24h settle delay") || !strings.Contains(string(start.Data), `"settling":2`) {
		t.Fatalf("retro.start = %q %s", start.Message, start.Data)
	}

	h.requestRetro(RetroPayload{})
	if looked(fresh) || looked(young) {
		t.Fatal("a plain `magnum retro` took a PR closed less than 24 hours ago")
	}
	h.requestRetro(RetroPayload{PRs: []int64{young.ID}})
	if !looked(young) {
		t.Fatal("`magnum retro <ref>` did not take the PR it names, closed an hour ago")
	}
	h.advance(time.Hour)
	h.requestRetro(RetroPayload{})
	if !looked(fresh) {
		t.Fatal("the PR closed 24 hours ago is still waiting")
	}
}

// repoMissClassifier classifies t101 as a miss of scope (the rest as
// style), with a lesson for the repository's notes.
func repoMissClassifier(scope string) *fakeClassifier {
	return &fakeClassifier{answer: func(job ClassifyJob) (ClassifyResult, error) {
		var out learn.Output
		for _, c := range job.Candidates {
			it := learn.Item{ID: c.ID, Class: store.MissStyle}
			if c.ID == "t101" {
				it = learn.Item{ID: c.ID, Class: store.MissMiss, Severity: "P1", Title: "Coupon lookup ignores the site",
					Lesson: "A finder that takes a code from the request must scope it to the current site.",
					Scope:  scope, Lines: []int{40, 42}, Match: []string{"site"}}
			}
			out.Items = append(out.Items, it)
		}
		b, _ := json.Marshal(out)
		return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
	}}
}

func withCurator(cur *fakeCurator) func(*harness) {
	return func(h *harness) {
		h.d.Curator = func(context.Context, CurateRun) (Curator, error) { return cur, nil }
	}
}

// givenMisses reads the misses.json of a curation.
func givenMisses(s notes.Scratch) curateMissesFile {
	var f curateMissesFile
	b, _ := os.ReadFile(s.Misses())
	_ = json.Unmarshal(b, &f)
	return f
}

// notedProposal is goodProposal with a Pitfalls section that notes the
// first miss given and skips the others.
func notedProposal(s notes.Scratch) {
	goodProposal(s)
	text := curatedNotes + "\n## Pitfalls\n- Scope every finder that takes a code from the request to the current site.\n"
	_ = os.WriteFile(s.Proposal(), []byte(text), 0o600)
	ch := notes.Changes{
		Sections: []notes.Change{{Name: "Tests", Action: notes.ActionKept, Reason: "every review runs specs"},
			{Name: "Pitfalls", Action: notes.ActionAdded, Reason: "a review of a finder checks its scope"}},
		Files: []notes.Change{
			{Name: "run_spec.sh", Action: notes.ActionKept, Reason: "runs any spec with the checkout's Ruby"},
			{Name: "campaign_snapshot_probes_spec.rb", Action: notes.ActionDeleted, Reason: "one pull request's probe"},
		},
	}
	for i, m := range givenMisses(s).Misses {
		if i == 0 {
			ch.Misses = append(ch.Misses, notes.MissChange{ID: m.ID, Action: notes.MissNoted, Section: "Pitfalls"})
		} else {
			ch.Misses = append(ch.Misses, notes.MissChange{ID: m.ID, Action: notes.MissSkipped, Reason: "covered by the same pitfall"})
		}
	}
	b, _ := json.Marshal(ch)
	_ = os.WriteFile(s.Changes(), b, 0o600)
}

// missesHarness: talkable/talkable's notes within their limits, a retro
// classifier and a curator.
func missesHarness(t *testing.T, fc *fakeClassifier, cur *fakeCurator, mods ...func(*harness)) (*harness, notes.Repo) {
	t.Helper()
	h := newHarness(t, append([]func(*harness){withClassifier(fc), withCurator(cur)}, mods...)...)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` checks one fix\n",
		map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	// The scan for a curation due counts as just run: a retro runs in its
	// own goroutine, so the tick that starts it would otherwise race it to
	// that scan and, when the retro won, curate before the test looked at
	// the misses. Each test advances curateCheckEvery for the scan it means.
	h.e.curateChecked = h.clock.Now()
	return h, nr
}

func repoMiss(t *testing.T, h *harness, pr store.PR) store.Miss {
	t.Helper()
	m := h.misses(pr.ID)["101"]
	if m.Class != store.MissMiss || m.Scope != store.MissScopeRepo {
		t.Fatalf("t101 = %+v", m)
	}
	return m
}

// A retro that records a miss for the repository's notes (class miss, scope
// repo) gets the repository a curation: the curator reads the misses as
// data (id, severity, path:line, title, lesson; no login, no PR number or
// link) and accounts for each, the proposal keeps what it did with them,
// and the mark is cleared. A proposal nobody reviews expires and its misses
// stay new for the next curation.
func TestARetroThatRecordsARepoMissTriggersACuration(t *testing.T) {
	cur := &fakeCurator{write: notedProposal}
	h, _ := missesHarness(t, repoMissClassifier(store.MissScopeRepo), cur)
	pr := retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	m := repoMiss(t, h, pr)
	if v, _ := h.e.getKV(h.ctx, KVNotesMisses("talkable/talkable")); v == "" {
		t.Fatal("the repository is not marked for a curation")
	}
	if evs := notesEvents(t, h, "notes.misses"); len(evs) != 1 || !strings.Contains(evs[0].Message, "1 miss") {
		t.Fatalf("notes.misses events = %+v", evs)
	}

	h.advance(curateCheckEvery)
	h.tick()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalPending || ps[0].Trigger != CurateTriggerMisses {
		t.Fatalf("proposals = %+v", ps)
	}
	links, err := h.st.ProposalMisses(h.ctx, ps[0].ID)
	if err != nil || len(links) != 1 || links[0].MissID != m.ID || links[0].Outcome != store.MissNoted || links[0].Section != "Pitfalls" {
		t.Fatalf("links = %+v, %v", links, err)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesMisses("talkable/talkable")); v != "" {
		t.Errorf("the misses mark stays after the curation: %q", v)
	}
	scratch := notes.Scratch{Dir: ps[0].Scratch}
	if len(cur.prompts) != 1 || !strings.Contains(cur.prompts[0], scratch.Misses()) || !strings.Contains(cur.prompts[0], "1 miss") {
		t.Fatalf("prompt: %q", cur.prompts)
	}
	raw, err := os.ReadFile(scratch.Misses())
	if err != nil {
		t.Fatal(err)
	}
	given := givenMisses(scratch)
	if len(given.Misses) != 1 {
		t.Fatalf("misses.json = %s", raw)
	}
	if g := given.Misses[0]; g.ID != m.ID || g.Severity != "P1" || g.Where != "app/models/coupon.rb:42" || g.Title != "Coupon lookup ignores the site" ||
		!strings.Contains(g.Lesson, "current site") {
		t.Fatalf("misses.json = %s", raw)
	}
	for _, leak := range []string{"rev-ann", "alice", "pull", "#7", "SECRET-BODY", "reviewer", "number", "url", retroShaA} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("misses.json carries %q: %s", leak, raw)
		}
	}
	for _, leak := range []string{"rev-ann", "pull/7", "Coupon lookup ignores the site"} {
		if strings.Contains(cur.prompts[0], leak) {
			t.Errorf("the prompt carries %q", leak)
		}
	}

	// Nobody reviews it: it expires and the miss stays new.
	h.advance(ProposalTTL)
	h.tick()
	if ps := proposals(t, h); ps[0].State != store.ProposalExpired {
		t.Fatalf("after a week: %+v", ps)
	}
	if got := h.misses(pr.ID)["101"]; got.State != store.MissNew || got.ProposalState != store.ProposalExpired {
		t.Fatalf("after the expiry: %+v", got)
	}
}

// Only a miss of class miss and scope repo is for the notes: a general one
// (for the skill), a not_issue, a style or an outside comment triggers no
// curation.
func TestOnlyRepoMissesTriggerACuration(t *testing.T) {
	cur := &fakeCurator{write: notedProposal}
	h, _ := missesHarness(t, repoMissClassifier(store.MissScopeGeneral), cur)
	retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	if v, _ := h.e.getKV(h.ctx, KVNotesMisses("talkable/talkable")); v != "" {
		t.Fatalf("marked for a general miss: %q", v)
	}
	h.advance(curateCheckEvery)
	h.tick()
	if ps := proposals(t, h); len(ps) != 0 {
		t.Fatalf("proposals = %+v", ps)
	}
}

// [notes] curate without misses turns the trigger off; a curation asked for
// with `magnum notes --curate` still takes the pending misses.
func TestTheCurateSettingTurnsTheMissesTriggerOff(t *testing.T) {
	cur := &fakeCurator{write: notedProposal}
	h, _ := missesHarness(t, repoMissClassifier(store.MissScopeRepo), cur, func(h *harness) {
		h.cfg.Notes.Curate = config.CurateTriggers{config.CurateOverLimit}
	})
	pr := retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	m := repoMiss(t, h, pr)
	h.advance(curateCheckEvery)
	h.tick()
	if ps := proposals(t, h); len(ps) != 0 {
		t.Fatalf("the misses trigger is off, yet: %+v", ps)
	}
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].Trigger != CurateTriggerRequest {
		t.Fatalf("proposals = %+v", ps)
	}
	if links, _ := h.st.ProposalMisses(h.ctx, ps[0].ID); len(links) != 1 || links[0].MissID != m.ID {
		t.Fatalf("the requested curation did not take the pending miss: %+v", links)
	}
}

// A proposal that leaves a miss it was given unaccounted for gets the
// nudge, then is kept as invalid; the miss stays new and unlinked.
func TestAProposalThatSkipsAMissWithoutAccountingIsInvalid(t *testing.T) {
	cur := &fakeCurator{write: goodProposal}
	h, _ := missesHarness(t, repoMissClassifier(store.MissScopeRepo), cur)
	pr := retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	m := repoMiss(t, h, pr)
	h.advance(curateCheckEvery)
	h.tick()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalInvalid ||
		!strings.Contains(ps[0].Reason, "miss "+itoa(m.ID)+" of misses.json is not in changes.json's misses") {
		t.Fatalf("proposals = %+v", ps)
	}
	if links, _ := h.st.ProposalMisses(h.ctx, ps[0].ID); len(links) != 0 {
		t.Errorf("an invalid proposal linked misses: %+v", links)
	}
	if got := h.misses(pr.ID)["101"]; got.State != store.MissNew || got.ProposalID != nil {
		t.Errorf("the miss after an invalid proposal = %+v", got)
	}
}
