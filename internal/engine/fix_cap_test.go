package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// recordTitlesOfReReviews wraps h's round script (reviewWith's) so that the
// k-th round also records the provenance a real judge's result carries: the
// posted findings titled by titles(k) on the run reviewWith created for it.
func recordTitlesOfReReviews(h *harness, titles func(k int) []string) {
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	inner := h.rd.script
	var mu sync.Mutex
	n := 0
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := inner(in)
		if err != nil {
			return res, err
		}
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		var fs []store.Finding
		for i, title := range titles(k) {
			fs = append(fs, store.Finding{FindingID: fmt.Sprintf("F%d", i+1), Title: title, Severity: "P2", Sources: []string{"codex-review"},
				Verdict: store.FindingPosted})
		}
		if err := h.st.RecordFindings(h.ctx, fmt.Sprintf("run-loop-%d", k), in.PR.ID, res.Round, fs); err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
}

// earlierFixRuns records n fix runs of the PR from earlier loops, an hour
// apart before now.
func earlierFixRuns(h *harness, pr store.PR, n int) {
	h.t.Helper()
	for i := range n {
		_, err := h.st.CreateRun(h.ctx, store.Run{ID: fmt.Sprintf("run-earlier-fix-%d", i), PRID: pr.ID, Round: 1, Role: "claude-fix", Kind: store.RunFix,
			TargetSHA: fixHead, Identity: pr.Identity, ReviewerLogin: "zhuravel", State: store.RunVerified, Outcome: new("pushed"),
			CreatedAt: h.clock.Now().Add(-time.Duration(n-i) * time.Hour)})
		if err != nil {
			h.t.Fatal(err)
		}
	}
}

// The cap says what is left, not only how much: the last review's findings
// split into new ones, ones the reviews missed earlier and fallout of the
// fixes (the judge ends those titles "(missed earlier)" and "(in the fix for
// ...)"), and the PR's fix cycles across all its loops, so a hand-started
// loop's "cycle 1 of 5" does not hide that the PR is on its fifteenth. The
// card (last_error), the event and the toast carry the same line.
func TestTheCapSaysWhatIsLeft(t *testing.T) {
	h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.FixCycles = 2 })
	n := 0
	pr := loopToReReview(t, h, git, func(pipeline.RoundInput) (map[string]int, string) {
		n++
		if n == 1 {
			return map[string]int{"P2": 1}, "CHANGES_REQUESTED"
		}
		return map[string]int{"P1": 1, "P2": 2}, "CHANGES_REQUESTED"
	}, fixPushed2)
	recordTitlesOfReReviews(h, func(k int) []string {
		if k == 1 {
			return []string{"Offers lose their site (missed earlier)"}
		}
		return []string{"Export skips the site check",
			"A partial refund extends the cutoff (in the fix for Later visits earn rewards)",
			"Case-only tier names block a restore (in the fix for Restoring detached tiers creates overlapping reward…"}
	})
	earlierFixRuns(h, pr, 9)

	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // re-review 1: cycle 2 runs and pushes
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"}) // re-review 2: the cap

	runs := h.fixRuns(pr.ID)
	if len(runs) != 11 {
		t.Fatalf("fix runs = %d, want 11 (9 earlier, 2 in this loop)", len(runs))
	}
	report := deref(runs[10].ReportPath)
	want := "2 fix cycles (11 on this PR), 3 findings left: 1 new, 0 missed earlier, 2 fallout of the fixes: " + report
	cur := h.wantState(2, store.PRNeedsAttention)
	if got := deref(cur.LastError); got != want {
		t.Errorf("last_error = %q, want %q", got, want)
	}
	if ev, ok := h.fixEvent("fix.capped"); !ok || ev.Message != want {
		t.Errorf("fix.capped = %+v, %v; want the message %q", ev, ok, want)
	}
	h.flush()
	if got := toastsWith(h, "2 fix cycles (11 on this PR), 3 findings left: 1 new, 0 missed earlier, 2 fallout of the fixes"); len(got) != 1 {
		t.Errorf("toasts = %v, want one with the cap's line", h.nh.shown())
	}
}

// A loop that is the PR's only one says no more than its own cycles, and a
// review whose findings the registry holds no titles for (a result written
// before the ledger, one that lists fewer posted findings than it counts)
// gets no split rather than a guessed one.
func TestTheCapNamesNoSplitTheRegistryCannotBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		titles []string
		want   string
	}{
		{name: "no titles recorded", want: "2 fix cycles, 2 findings left: "},
		{name: "fewer posted titles than findings", titles: []string{"Export skips the site check"}, want: "2 fix cycles, 2 findings left: "},
		{name: "titles that add up", titles: []string{"Export skips the site check", "Offers lose their site (missed earlier) (optional, no reply needed)"},
			want: "2 fix cycles, 2 findings left: 1 new, 1 missed earlier, 0 fallout of the fixes: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.FixCycles = 2 })
			pr := loopToReReview(t, h, git, findings(map[string]int{"P1": 1, "P2": 1}, "CHANGES_REQUESTED"), fixPushed2)
			recordTitlesOfReReviews(h, func(int) []string { return tc.titles })
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
			h.advance(time.Minute)
			reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"})

			runs := h.fixRuns(pr.ID)
			if len(runs) != 2 {
				t.Fatalf("fix runs = %d, want 2", len(runs))
			}
			cur := h.wantState(2, store.PRNeedsAttention)
			if want := tc.want + deref(runs[1].ReportPath); deref(cur.LastError) != want {
				t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
			}
		})
	}
}

// A loop that waited for its re-review while the daemon was down gets the
// cap's account from the review as the registry holds it: the recovered
// result names the round, so the titles of the posted findings are found.
func TestARecoveredCapSaysWhatIsLeft(t *testing.T) {
	report := "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	if err := h.st.RecordFindings(h.ctx, "run-review-2", pr.ID, 1, []store.Finding{
		{FindingID: "F1", Title: "Export skips the site check", Severity: "P2", Verdict: store.FindingPosted},
		{FindingID: "F2", Title: "Cleanup drops a sibling (in the fix for Undo is blocked)", Severity: "P2", Verdict: store.FindingPosted},
	}); err != nil {
		t.Fatal(err)
	}
	earlierFixRuns(h, pr, 3)
	st := FixState{Cycle: 1, Cap: 1, Phase: FixPhaseReviewing, Since: "f0f0f0f", Head: fixHead, By: "magnum fix",
		Report: report, StartedAt: h.clock.Now().UTC(), Role: "claude-fix"}
	if err := h.e.setFixState(h.ctx, pr.ID, st); err != nil {
		t.Fatal(err)
	}
	h.startup()

	cur := h.wantState(2, store.PRNeedsAttention)
	want := "1 fix cycle (3 on this PR), 2 findings left: 1 new, 0 missed earlier, 1 fallout of the fixes: " + report
	if got := deref(cur.LastError); got != want {
		t.Errorf("last_error = %q, want %q", got, want)
	}
}
