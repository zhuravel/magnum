package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// The fix prompt lets the fixer edit the PR's description only when it
// says that the PR is the operator's own (agents.FixPromptInput.OwnPR), and
// the fix job says so from its own check of the PR's author (ownPR), which
// it refuses every other PR by: a PR the factory opened is the operator's
// too, since the factory opens it as the operator.
func TestTheFixJobTellsTheFixerThatThePRIsTheOperatorsOwn(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if !h.fixInput().OwnPR {
		t.Error("the fix prompt of the operator's own PR does not say that it is the operator's own")
	}
}

// untrueDescription is a review body whose only word is about the PR
// description: a claim of it the head made untrue.
const untrueDescription = "No problems found.\n\nDescription: ✗ The Test Plan says 19 checks: the head runs 21"

// writeReviewFile writes the judge's review file of PR #n's review at head
// (agents.PostReviewFile, in the head's review directory) with body.
func writeReviewFile(h *harness, n int, head, body string) {
	h.t.Helper()
	dir := h.e.d.Layout.ReviewDir("talkable", "talkable", n, head)
	b, err := json.Marshal(map[string]any{"event": "COMMENTED", "body": body, "comments": []any{}})
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, agents.PostReviewFile), b, 0o600)
	}
	if err != nil {
		h.t.Fatal(err)
	}
}

// A review whose body says that a claim of the PR description is untrue
// (its `Description: ✗` line, the review skill's) found something the
// fixer of an own PR fixes (2026-11-08): the description line is fix work
// for reviewWork, as the look's items and its rule on magnum's rebuttals
// read it, whatever the review's findings. No file, or a body without the
// line, is none.
func TestAReviewsDescriptionLineIsTheFixersWork(t *testing.T) {
	dir := t.TempDir()
	if reviewDescribes(dir) {
		t.Error("a missing review file has a description line")
	}
	for body, want := range map[string]bool{
		untrueDescription: true,
		"No problems found.\n\n- Description: ✗ the Solution names the old class": true,
		"No problems found.\n\nTicket: ✗ the ticket asks for a CSV export":        false,
		"No problems found.": false,
	} {
		b, err := json.Marshal(map[string]any{"event": "COMMENTED", "body": body})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, agents.PostReviewFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := reviewDescribes(dir); got != want {
			t.Errorf("reviewDescribes(%q) = %v, want %v", body, got, want)
		}
	}
	sum := cleanSum()
	if reviewWork(*sum, p3Calls{}, false) || !reviewWork(*sum, p3Calls{}, true) {
		t.Error("reviewWork does not count the description line")
	}
	read := readFor(nil, nil, nil, nil, nil)
	if got := itemKeys(openItemsOf(itemsInput{read: read, sum: sum, head: fixHead, described: true})); !slices.Equal(got, []string{"review:r1"}) {
		t.Errorf("items = %v, want the review", got)
	}
	f := idleFacts()
	f.described = true
	f.ledger.Reviews = []string{"review:r1"}
	f.items = []openItem{{Key: "mthread:5000@5002", Class: classMagnum, Kind: itemMagnum, URL: prPage + "#discussion_r5000"}}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 reply of magnum's")
	if len(work) != 1 {
		t.Errorf("work = %v", work)
	}
}

// On an own PR the re-review of the fixer's push said that the description
// still claimed the old behaviour (`Description: ✗`) and found nothing else,
// and the loop ended as done: nothing handed that line to a fixer. The
// line is the next cycle's work now, and that cycle's fixer is handed the
// re-review as findings, not as a review that found nothing.
func TestALoopGoesOnWhenItsReReviewFindsTheDescriptionUntrue(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{}, "COMMENTED"), fixPushed2)
	writeReviewFile(h, 2, fixPushed, untrueDescription)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	if n := len(h.fixRuns(pr.ID)); n != 2 {
		t.Fatalf("fix runs = %d, want the second cycle for the description", n)
	}
	if _, ok := h.fixEvent("fix.done"); ok {
		t.Error("the loop ended as done on a re-review that found the description untrue")
	}
	if in := h.fixInput(); in.NoFindings {
		t.Error("the second cycle's prompt says that the review found nothing")
	}
	if ev, ok := h.fixEvent("fix.start"); !ok || !strings.Contains(ev.Message, "fixes the description finding of the review on "+fixPushed[:7]) {
		t.Errorf("fix.start = %q, %v", ev.Message, ok)
	}
}

// Under autofix = "open" a clean review of the head whose body finds the
// description untrue is work: the look starts a loop on it, which hands the
// fixer the review as findings. Without the line the PR is idle.
func TestTheLookStartsALoopOnAReviewsDescriptionLine(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "description: the Test Plan counts 21 checks", "0 threads", "none")
	})
	h.tick() // reviewed
	writeReviewFile(h, 2, fixHead, untrueDescription)
	h.pastSettle()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the loop for the description", n)
	}
	if in := h.fixInput(); in.NoFindings {
		t.Error("the loop's prompt says that the review found nothing")
	}
}
