package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// IsAncestor answers from ancestors ("<ancestor>..<rev>"; any other pair
// is no) and records its calls.
func (g *fixGit) IsAncestor(_ context.Context, dir, ancestor, rev string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ancestry = append(g.ancestry, dir+":"+ancestor+".."+rev)
	return g.ancestors[ancestor+".."+rev], nil
}

// On talkable#12114 GitHub's "Update branch" merged master into the PR's
// branch while the fixer worked, its push was rejected and the fixer
// stopped. `magnum fix-sync` now replays the fixer's commits onto such a
// merge, and the fixer pushes again. The poller saw the merge during the
// run, so the PR row's head is the merge, not the head the fixer started
// from: the settle takes the pushed head, which descends from that merge,
// onto the row as it takes any push of the fixer's, so the forced
// re-review reviews the fixer's push, never the merge alone. The prompt
// names the fix-sync line with the run's start head, the branch and the
// base.
func TestAPushAfterAFixSyncSettles(t *testing.T) {
	const (
		merge  = "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5" // GitHub's merge of master into the branch
		synced = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6" // the fixer's commit replayed onto it
	)
	h, git := fixHarness(t, realSleep)
	git.ancestors = map[string]bool{merge + ".." + synced: true}
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()
	if in := h.fixInputsOf(); len(in) != 1 || in[0].FixSyncCommand != agents.FixSyncLine(h.d.Layout.Binary(), fixHead, "feature", "master") {
		t.Fatalf("the fix prompt's fix-sync line = %+v", in)
	}

	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: merge, author: "zhuravel"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	if cur := h.pr(2); cur.HeadSHA != merge {
		t.Fatalf("the poller did not see the merge: head %s", cur.HeadSHA)
	}
	rounds := len(h.rd.all())

	run := h.fixRuns(pr.ID)[0]
	if err := os.MkdirAll(filepath.Dir(deref(run.ReportPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	report := fixReport(run.ID, "f6f6f6f Fix the total (review comment 4100000001)", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	if err := os.WriteFile(deref(run.ReportPath), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	git.setHead(synced)
	h.gh.setHead("talkable/talkable", 2, synced)
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	h.settle()

	if _, ok := h.fixEvent("fix.failed"); ok {
		t.Fatal("the settle failed the synced push")
	}
	if ev, ok := h.fixEvent("fix.pushed"); !ok || !strings.Contains(ev.Message, "the head is "+synced[:7]) {
		t.Errorf("fix.pushed = %q, %v", ev.Message, ok)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Head != synced {
		t.Errorf("loop = %+v, %v; want it waiting for the re-review of %s", st, ok, synced[:7])
	}
	cur := h.pr(2)
	if cur.HeadSHA != synced || !cur.Forced {
		t.Fatalf("PR head %s forced %v: want the synced push, forced", cur.HeadSHA, cur.Forced)
	}
	git.mu.Lock()
	asked := strings.Join(git.ancestry, " ")
	git.mu.Unlock()
	if !strings.Contains(asked, merge+".."+synced) {
		t.Errorf("ancestry asked = %q, want whether the push descends from the merge", asked)
	}

	// The re-review runs on the pushed head; a next cycle's fixer pushes nothing.
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: synced, author: "zhuravel"})
	if got := h.rd.all(); len(got) != rounds+1 || got[len(got)-1].TargetSHA != synced || !got[len(got)-1].PR.Forced {
		t.Fatalf("rounds after the settle: %d (want %d); the re-review must target %s", len(got), rounds+1, synced[:7])
	}
}

// A row whose head the pushed head does not descend from (someone pushed
// after the fixer, and the poller saw it) keeps that head: the fixer's push
// never moves the row back.
func TestAFixPushNeverMovesTheRowBehindANewerHead(t *testing.T) {
	const (
		newer  = "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5" // a push the poller saw, which the fixer's does not contain
		pushed = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"
	)
	h, git := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: newer, author: "zhuravel"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	run := h.fixRuns(pr.ID)[0]
	if err := os.MkdirAll(filepath.Dir(deref(run.ReportPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	report := fixReport(run.ID, "f6f6f6f Fix the total (review comment 4100000001)", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	if err := os.WriteFile(deref(run.ReportPath), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	git.setHead(pushed)
	h.gh.setHead("talkable/talkable", 2, pushed)
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if cur := h.pr(2); cur.HeadSHA != newer {
		t.Errorf("PR head %s, want the newer %s kept", cur.HeadSHA, newer[:7])
	}
}
