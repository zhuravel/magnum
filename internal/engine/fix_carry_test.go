package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// cycleFixer makes h's fake fixer do work[i] in its i-th run (the last one
// again past the list): move the slot to the head it returns (pushed when
// its report lists commits) and write its report.
func cycleFixer(h *harness, git *fixGit, work ...func(runID string) (head, report string)) {
	var mu sync.Mutex
	n := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		w := work[min(n, len(work)-1)]
		n++
		mu.Unlock()
		return w(run.ID)
	})
}

// byHead is a round plan whose review of a head posts the findings plan
// names for it (none for any other head), as a comment.
func byHead(plan map[string]map[string]int) func(pipeline.RoundInput) (map[string]int, string) {
	return func(in pipeline.RoundInput) (map[string]int, string) { return plan[in.TargetSHA], "COMMENTED" }
}

// leavesCall is a fixer's run that pushes head and leaves the author the
// call on 2101 at offer.rb's line, beside the other entries of more.
func leavesCall(head string, line int, more ...string) func(runID string) (string, string) {
	return func(id string) (string, string) {
		needs := "- 2101 P1 app/models/offer.rb:" + itoa(int64(line)) + ": not fixed. wants a schema change"
		for _, m := range more {
			needs += "\n" + m
		}
		return head, fixReport(id, head[:7]+" Fix the finding", fixReplied, needs)
	}
}

// carryToCycle2 runs a loop on PR #2 whose first fixer pushes fixPushed and
// leaves the author the call on 2101, and whose re-review of fixPushed
// posts a new P2 (re-reviews of other heads find nothing), so that its
// second cycle runs with work; it returns the PR once that cycle settled.
func carryToCycle2(t *testing.T, h *harness, git *fixGit, second func(runID string) (string, string)) store.PR {
	t.Helper()
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 1})
	cycleFixer(h, git, leavesCall(fixPushed, 42), second)
	h.fix(FixPayload{})
	h.handle()
	reviewWith(h, byHead(map[string]map[string]int{fixPushed: {"P2": 1}}))
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review's new finding: cycle 2
	if n := len(h.fixRuns(pr.ID)); n != 2 {
		t.Fatalf("fix runs = %d, want the second cycle on the re-review's new finding", n)
	}
	return pr
}

// On an own PR the fixer of cycle 1 left the author an unrelated call and
// pushed; the re-review of its push posted 4 new optional findings, and the
// loop ended at once as needs attention for the call, so no fixer was ever
// handed them. A re-review that posts new findings now runs the next cycle
// while cycles are left, and the calls of the cycles before go on with the
// loop (FixState.Carried) to its end, where they are asked with the last
// cycle's, each once: a call the last fixer left again is its own, with the
// place it names now (the push may have moved its line).
func TestAReReviewWithNewFindingsRunsTheNextCycleOverTheCalls(t *testing.T) {
	h, git := fixHarness(t)
	pr := carryToCycle2(t, h, git, leavesCall(fixPushed2, 44, "- 2301 P2 app/lib/total.rb:9: not fixed. contested"))
	st, ok := h.fixStateOf(2)
	if !ok || st.Cycle != 2 || st.Phase != FixPhaseReviewing || len(st.Carried) != 0 || len(st.NeedsYou) != 2 {
		t.Fatalf("fix state after cycle 2 = %+v, %v; want the call 2101 asked again by cycle 2 alone", st, ok)
	}
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"}) // a clean re-review: the loop's end
	report := deref(h.fixRuns(pr.ID)[1].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	want := "fix: 2 findings need your call (2101 P1 app/models/offer.rb:44, 2301 P2 app/lib/total.rb:9): " + report
	if deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
}

// A call of cycle 1 that cycle 2's fixer did not leave again (it worked on
// the re-review's new finding alone) is still the author's at the loop's
// end: the clean re-review of cycle 2's push ends the loop as needs
// attention for it, never as done.
func TestTheCallsOfAnEarlierCycleReachTheLoopsEnd(t *testing.T) {
	h, git := fixHarness(t)
	carryToCycle2(t, h, git, func(id string) (string, string) {
		return fixPushed2, fixReport(id, fixPushed2[:7]+" Fix the new total", fixReplied, "none")
	})
	st, ok := h.fixStateOf(2)
	if !ok || len(st.Carried) != 1 || len(st.Carried[0].NeedsYou) != 1 || !strings.HasPrefix(st.Carried[0].NeedsYou[0], "2101 P1") {
		t.Fatalf("fix state after cycle 2 = %+v, %v; want cycle 1's call carried", st, ok)
	}
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"})
	cur := h.wantState(2, store.PRNeedsAttention)
	if want := "fix: 1 finding needs your call (2101 P1 app/models/offer.rb:42): "; !strings.HasPrefix(deref(cur.LastError), want) {
		t.Errorf("last_error = %q, want %q…", deref(cur.LastError), want)
	}
	if _, ok := h.fixEvent("fix.done"); ok {
		t.Error("the loop ended as done over a call of its first cycle")
	}
}

// A next cycle whose fixer pushes nothing ends the loop at its settle: with
// a call carried from cycle 1 that end needs attention for it, not the info
// toast of a fixer that only replied; a cycle that fails keeps the call
// after its own words.
func TestACycleThatEndsTheLoopAsksTheCallsItCarries(t *testing.T) {
	for name, tc := range map[string]struct {
		report func(id string) string
		lead   string
	}{
		"replies alone": {func(id string) string { return fixReport(id, "nothing", fixReplied, "none") },
			"fix: 1 finding needs your call (2101 P1 app/models/offer.rb:42): "},
		"a failed push": {func(id string) string { return fixReport(id, "push failed: remote rejected", "0 threads", "none") },
			"fix run failed: the fixer's push failed: "},
	} {
		t.Run(name, func(t *testing.T) {
			h, git := fixHarness(t)
			carryToCycle2(t, h, git, func(id string) (string, string) { return "", tc.report(id) })
			if _, ok := h.fixStateOf(2); ok {
				t.Fatal("the loop goes on after a cycle that pushed nothing")
			}
			cur := h.wantState(2, store.PRNeedsAttention)
			msg := deref(cur.LastError)
			if !strings.HasPrefix(msg, tc.lead) || !strings.Contains(msg, "2101 P1 app/models/offer.rb:42") {
				t.Errorf("last_error = %q, want %q… naming the call 2101", msg, tc.lead)
			}
			h.flush()
			if got := toastsWith(h, "pushed nothing"); len(got) != 0 {
				t.Errorf("toasts = %v, want no info toast of a fixer that only replied", got)
			}
		})
	}
}

// What a later cycle asked again or took on is no carried call any more: an
// entry with a comment id the later report lists (its entry is the call
// now), a conflict with the base (the later job checks the base again) and
// a description entry (its fixer judged the re-review's own line), and,
// once the later cycle's fixer got the external file, another reviewer's
// thread, review body or comment (it had every open one). A call in
// magnum's own thread that the later fixer left alone is carried on.
func TestACarriedCallGoesOnceALaterCycleTookItOn(t *testing.T) {
	root := t.TempDir()
	first, later := filepath.Join(root, "a1a1a1a"), filepath.Join(root, "b2b2b2b")
	for _, dir := range []string{first, later} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal([]agents.ReviewThread{{CommentID: 2101}})
	if err == nil {
		err = os.WriteFile(filepath.Join(first, pipeline.ThreadsFile), b, 0o600)
	}
	if err == nil {
		err = writeExternalFile(filepath.Join(first, externalFile), externalItems{Threads: []externalThread{{CommentID: 3101}}})
	}
	if err != nil {
		t.Fatal(err)
	}
	carried := []FixCarry{{Report: filepath.Join(first, "claude-fix.md"), NeedsYou: []string{
		"2101 P1 app/a.rb:1: not fixed. wants a schema change",
		"3101 app/b.rb:2: not fixed. contested",
		"conflict: db/schema.rb: both sides change the column",
		"4100 review: not fixed. asks for the rollout plan",
		"5100 description: the why is the author's",
	}, Answered: []string{"2101"}}}
	report := filepath.Join(later, "claude-fix.md")
	entries := func(c []FixCarry) []string {
		var out []string
		for _, g := range c {
			out = append(out, g.NeedsYou...)
		}
		return out
	}
	got := carriedLeft(carried, report, nil)
	if want := []string{"2101 P1 app/a.rb:1: not fixed. wants a schema change", "3101 app/b.rb:2: not fixed. contested",
		"4100 review: not fixed. asks for the rollout plan"}; !slices.Equal(entries(got), want) {
		t.Errorf("without the later file: carried %q, want %q", entries(got), want)
	}
	if len(got) != 1 || !slices.Equal(got[0].Answered, []string{"2101"}) {
		t.Errorf("carried %+v, want 2101 still answered", got)
	}
	if err := writeExternalFile(filepath.Join(later, externalFile), externalItems{}); err != nil {
		t.Fatal(err)
	}
	if got := entries(carriedLeft(carried, report, nil)); !slices.Equal(got, []string{"2101 P1 app/a.rb:1: not fixed. wants a schema change"}) {
		t.Errorf("with the later file: carried %q, want magnum's call alone", got)
	}
	if got := carriedLeft(carried, report, []string{"2101 P1 app/a.rb:3: not fixed. wants a schema change"}); len(got) != 0 {
		t.Errorf("asked again: carried %+v, want none", got)
	}
}
