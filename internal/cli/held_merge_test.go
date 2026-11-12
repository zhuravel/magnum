package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// heldMergeSHA is the merge the daemon holds for your y in these tests.
	heldMergeSHA = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	// heldPlan is the daemon's plan of its push, and heldQuestion the
	// y/N the command and the screens put.
	heldPlan = "dry run: would merge the parent of talkable/talkable#5: magnum pushes claude-fix's merge of master at c0c0c0c into feature as " +
		"fixer-app[bot] with the Workflows token; it changes .github/workflows/ci.yml and pronto.yml (held-merge.diff)"
	heldQuestion = "Merge the parent of talkable/talkable#5: magnum pushes claude-fix's merge of master at c0c0c0c into feature as " +
		"fixer-app[bot] with the Workflows token; it changes .github/workflows/ci.yml and pronto.yml (held-merge.diff)?"
	heldAnswer = "pushed claude-fix's merge of master at c0c0c0c into feature as fixer-app[bot] with the Workflows token: the head is d3d3d3d"
)

// heldAsked is what the plan of the held merge's push asks about: the
// head, master at c0c0c0c, its five conflicts and the merge.
var heldAsked = engine.MergeAsked{Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Base: "master",
	Tip: "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0", Conflicts: 5, Merge: heldMergeSHA, At: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}

// heldDaemon answers each pending request as the daemon does while it
// holds a merge: a dry run with the plan of its push, the y's push with
// heldAnswer.
func (h *actHarness) heldDaemon() {
	h.onSleep = func(h *actHarness) {
		for _, r := range h.requests() {
			if r.State != store.RequestPending {
				continue
			}
			res := heldAnswer
			if actDecode[engine.FixPayload](h.t, r.Payload).DryRun {
				b, err := json.Marshal(engine.MergePlan{Ref: "talkable/talkable#5", Asked: heldAsked, Text: heldPlan})
				if err != nil {
					h.t.Fatal(err)
				}
				res = string(b) + "\n" + heldPlan
			}
			if err := h.st.CompleteRequest(h.ctx, r.ID, store.RequestDone, res); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// While the daemon holds a merge the fixer resolved, `magnum merge-parent`
// and the screens' P ask the y/N of its push, which names the workflow
// files, the bot and the Workflows token, and the y sends the push bound
// to the merge's SHA besides the head and the tip.
func TestAHeldMergesQuestionNamesTheFilesTheBotAndTheToken(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRNeedsAttention)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.heldDaemon()
	h.stdin("y\n")
	if code := h.cmd("merge-parent", "5"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), heldQuestion+" [y/N]", heldAnswer)
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v, want the plan and the push", reqs)
	}
	if p := actDecode[engine.FixPayload](t, reqs[1].Payload); p.Asked == nil || p.Asked.Merge != heldMergeSHA || !sameAsked(*p.Asked, heldAsked) {
		t.Errorf("the push is bound to %+v, want the held merge %s", p.Asked, heldMergeSHA)
	}

	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()
	res, err := acts.MergeParentPlan(ctx, "talkable#5")
	if err != nil || res.Ask != heldQuestion || res.Parent == nil || res.Parent.Merge != heldMergeSHA {
		t.Fatalf("the screens' plan = %q %+v, %v; want %q bound to %s", res.Ask, res.Parent, err, heldQuestion, heldMergeSHA)
	}
	if _, err := acts.MergeParent(ctx, "talkable#5", *res.Parent); err != nil {
		t.Fatal(err)
	}
	reqs = h.requests()
	if p := actDecode[engine.FixPayload](t, reqs[len(reqs)-1].Payload); p.Asked == nil || p.Asked.Merge != heldMergeSHA {
		t.Errorf("the screens' push is bound to %+v, want the held merge %s", p.Asked, heldMergeSHA)
	}
}

// seedHeld records a merge the daemon holds for your y on PR n, needing
// you with its attention.
func seedHeld(t *testing.T, h *actHarness, n int) store.PR {
	t.Helper()
	pr := h.seedPR("talkable/talkable", n, store.PRNeedsAttention)
	h.setPR(pr.ID, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", attention.MergeReadyText("claude-fix merged master at c0c0c0c and resolved 5 conflicts. It changes "+
			".github/workflows/ci.yml and pronto.yml. fixer-app[bot] pushes it only after your y: P asks (e shows held-merge.diff)."))
	})
	b, err := json.Marshal(engine.HeldMerge{Repo: "talkable/talkable", Number: n, Merge: heldMergeSHA, Head: heldAsked.Head, Base: "master",
		Tip: heldAsked.Tip, Workflows: []string{".github/workflows/ci.yml"}, Role: "claude-fix", Diff: "/elsewhere/held-merge.diff"})
	if err != nil {
		t.Fatal(err)
	}
	for key, v := range map[string]string{engine.KVPRHeldMerge(pr.ID): string(b), engine.KVPRAttention(pr.ID): attention.KindFix} {
		if err := h.st.SetKV(h.ctx, key, v); err != nil {
			t.Fatal(err)
		}
	}
	return pr
}

// The board's row of a PR whose merge magnum holds for your y names its
// held-merge.diff in magnum's report directory of the PR (never the path
// the registry's row names), which e opens, and its state cell says
// "merge ready" on its one line; a PR without one names none.
func TestTheBoardRowOfAHeldMergeSaysMergeReadyAndNamesItsDiff(t *testing.T) {
	h := newActHarness(t)
	seedHeld(t, h, 12510)
	h.seedPR("talkable/talkable", 12511, store.PRReviewed)
	rows := boardRowsOf(t, h)
	want := filepath.Join(h.c.Layout.PRReviews("talkable", "talkable", 12510), engine.HeldMergeFile)
	if got := rows[12510].HeldMerge; got != want {
		t.Errorf("#12510: HeldMerge = %q, want %q", got, want)
	}
	if got := rows[12511].HeldMerge; got != "" {
		t.Errorf("#12511: HeldMerge = %q, want none", got)
	}
	if line := boardLine(t, rows, 12510); !strings.Contains(line, "merge ready") {
		t.Errorf("the row = %q, want it to say merge ready", line)
	}
}

// The dashboard's rows carry the held merge's diff too: the slot row of
// the PR (it holds its slot) and a queue row, so its e opens it.
func TestStatusDashDataCarriesTheHeldMerge(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{11920, 11931} {
		pr, err := st.PRByRepoNumber(ctx, repo.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(engine.HeldMerge{Repo: "talkable/talkable", Number: n, Merge: heldMergeSHA, Head: heldAsked.Head, Base: "master", Tip: heldAsked.Tip})
		if err := st.SetKV(ctx, engine.KVPRHeldMerge(pr.ID), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := statusGather(ctx, d, statusOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	got := statusDashData(r, "talkable/talkable")
	diff := func(n int) string {
		return filepath.Join(d.Layout.PRReviews("talkable", "talkable", n), engine.HeldMergeFile)
	}
	if len(got.Slots) != 1 || got.Slots[0].PRHeldMerge != diff(11920) {
		t.Errorf("the slot row: %+v, want the held merge %s", got.Slots, diff(11920))
	}
	for _, q := range got.Queue {
		if want := map[bool]string{true: diff(11931)}[q.Ref == "talkable#11931"]; q.HeldMerge != want {
			t.Errorf("queue row %s: held merge %q, want %q", q.Ref, q.HeldMerge, want)
		}
	}
}
