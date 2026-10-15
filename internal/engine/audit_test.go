package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// auditHarness is evidenceHarness with [pipeline] audit_every = audit and
// full_pass_every = full, and a daily cap the test's rounds stay under.
func auditHarness(t *testing.T, audit, full int, git *evidenceGit, byHead map[string]*pipeline.Evidence) *harness {
	t.Helper()
	return evidenceHarness(t, git, byHead, func(h *harness) {
		h.cfg.Pipeline.AuditEvery, h.cfg.Pipeline.FullPassEvery = audit, full
		h.cfg.Daemon.MaxRoundsPerPRPerDay = 20
	})
}

// unchangedGit is an evidenceGit for which every push from b<i> to b<i+1>
// (i < n) changes none of the files the map holds.
func unchangedGit(n int) *evidenceGit {
	g := &evidenceGit{changed: map[string][]string{}}
	for i := 1; i < n; i++ {
		g.changed[fmt.Sprintf("b%d..b%d", i, i+1)] = nil
	}
	return g
}

// wantAuditPass fails unless in is an audit pass: a recovery whose judge
// started fresh (the reviewers re-review the delta, ColdJudge) with the
// areas to probe.
func wantAuditPass(t *testing.T, label string, in pipeline.RoundInput) {
	t.Helper()
	if in.Kind != pipeline.KindRecovery || len(in.Audit) == 0 || !in.ColdJudge || in.FullPass || in.DeltaCheck != nil {
		t.Fatalf("%s: kind %s, audit %d areas, cold %v, full pass %v: want an audit pass", label, in.Kind, len(in.Audit), in.ColdJudge, in.FullPass)
	}
}

// wantNoAudit fails when in is an audit pass.
func wantNoAudit(t *testing.T, label string, in pipeline.RoundInput) {
	t.Helper()
	if len(in.Audit) > 0 {
		t.Fatalf("%s: an audit pass of %d areas, want none", label, len(in.Audit))
	}
}

// A PR whose rounds never probed some areas, or whose files changed since,
// never gets them read unless a round asks: every fifth automatic re-review
// (audit_every), counted since its first review, its last full pass or its
// last audit pass, runs as an audit pass while the evidence map holds
// unknown or stale entries. Every session starts fresh, the round runs as
// a recovery whose reviewers re-review the delta, and the judge gets at
// most three unknown or stale areas, the riskiest first (an irreversible
// write, authorization, money, an optional branch, a pre-pass lead), plus
// the oldest probe on a file the pushes touched since, to run again. The
// start event and round.audit_pass say so, the posted audit pass is
// recorded, and the count starts over.
func TestTheFifthReReviewIsAnAuditPassWhenAreasAreUnknown(t *testing.T) {
	git := unchangedGit(7)
	git.changed["b1..b2"] = []string{"app/a.rb"}
	git.changed["b1..b6"] = []string{"app/a.rb"}
	h := auditHarness(t, 5, 0, git, map[string]*pipeline.Evidence{
		"b1": {HeadSHA: "b1", Entries: []pipeline.EvidenceEntry{
			probed("app/a.rb", "the restore keeps the tier"),
			probed("app/c.rb", "the export pages"),
			{Path: "app/models/campaign.rb", State: pipeline.EvidenceUnknown},
			{Path: "app/services/payout.rb", Behaviour: "the refund amount rounds down", State: pipeline.EvidenceUnknown},
			{Path: "app/controllers/admin_controller.rb", State: pipeline.EvidenceUnknown},
			{Path: "lib/cleanup.rb", Behaviour: "deletes the expired rows", State: pipeline.EvidenceUnknown},
		}},
		"b2": {HeadSHA: "b2", Entries: []pipeline.EvidenceEntry{probed("app/a.rb", "the restore keeps the tier")}},
	})
	h.reviewedPR(2, "b1")
	for i := 2; i <= 5; i++ {
		wantNoAudit(t, fmt.Sprintf("re-review %d", i-1), pushAndRereview(t, h, fmt.Sprintf("b%d", i)))
	}
	pr := h.pr(2)
	parks := h.ag.count(fmt.Sprintf("park:%d", pr.ID))
	in := pushAndRereview(t, h, "b6")
	wantAuditPass(t, "the fifth re-review", in)
	if h.ag.count(fmt.Sprintf("park:%d", pr.ID)) == parks {
		t.Errorf("calls %v: want the sessions parked before the audit pass", h.ag.all())
	}
	var got []string
	for _, a := range in.Audit {
		got = append(got, a.Path+" "+a.Why)
	}
	want := []string{"lib/cleanup.rb irreversible write", "app/controllers/admin_controller.rb authorization",
		"app/services/payout.rb money", "app/a.rb rotation"}
	if !slices.Equal(got, want) {
		t.Fatalf("audit areas %q, want %q", got, want)
	}

	evs := approvalEvents(t, h, 2, "engine.round_start")
	last := evs[len(evs)-1]
	if !strings.HasPrefix(last.Message, "re-review (audit pass) in ") || eventData(t, last)["audit_pass"] != true {
		t.Fatalf("round start %q %s: want the audit pass named", last.Message, last.Data)
	}
	why := approvalEvents(t, h, 2, "round.audit_pass")
	if len(why) != 1 || !strings.Contains(why[0].Message, "re-review 5 since the first review (audit_every 5)") {
		t.Fatalf("round.audit_pass events: %+v", why)
	}
	if _, ok := kvValue(h, kvPRAuditPass(pr.ID)); !ok {
		t.Fatal("the posted audit pass is not recorded")
	}
	wantNoAudit(t, "the re-review after the audit pass", pushAndRereview(t, h, "b7"))
}

// An audit pass counts toward the full pass's count and never replaces a
// full pass: when both are due, the full pass runs.
func TestAFullPassWinsOverAnAuditPass(t *testing.T) {
	h := auditHarness(t, 2, 4, unchangedGit(6), map[string]*pipeline.Evidence{
		"b1": {HeadSHA: "b1", Entries: []pipeline.EvidenceEntry{{Path: "app/models/campaign.rb", State: pipeline.EvidenceUnknown}}},
	})
	h.reviewedPR(2, "b1")
	wantNoAudit(t, "the first re-review", pushAndRereview(t, h, "b2"))
	wantAuditPass(t, "the second re-review", pushAndRereview(t, h, "b3"))
	wantNoAudit(t, "the third re-review", pushAndRereview(t, h, "b4"))
	in := pushAndRereview(t, h, "b5")
	wantFullPass(t, "the fourth re-review", in)
	wantNoAudit(t, "the fourth re-review", in)
	if evs := approvalEvents(t, h, 2, "round.audit_pass"); len(evs) != 1 {
		t.Fatalf("round.audit_pass events %d, want the second re-review's alone", len(evs))
	}
}

// audit_every = 0 turns audit passes off, and a map whose every area was
// read or probed on files unchanged since needs none.
func TestAuditOffByZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		every int
		state string
	}{
		{"audit_every 0", 0, pipeline.EvidenceUnknown},
		{"nothing unknown or stale", 2, pipeline.EvidenceRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := auditHarness(t, tc.every, 0, unchangedGit(5), map[string]*pipeline.Evidence{
				"b1": {HeadSHA: "b1", Entries: []pipeline.EvidenceEntry{{Path: "app/models/campaign.rb", Behaviour: "x", State: tc.state}}},
			})
			h.reviewedPR(2, "b1")
			for i := 2; i <= 5; i++ {
				in := pushAndRereview(t, h, fmt.Sprintf("b%d", i))
				wantNoAudit(t, fmt.Sprintf("re-review %d", i-1), in)
				if in.Kind != pipeline.KindRereview {
					t.Fatalf("re-review %d: kind %s, want an ordinary re-review", i-1, in.Kind)
				}
			}
			if evs := approvalEvents(t, h, 2, "round.audit_pass"); len(evs) != 0 {
				t.Fatalf("round.audit_pass events: %+v", evs)
			}
			if pr := h.pr(2); deref(pr.ReviewedSHA) != "b5" || pr.State != store.PRReviewed {
				t.Fatalf("PR after the re-reviews: %s at %s", pr.State, deref(pr.ReviewedSHA))
			}
		})
	}
}
