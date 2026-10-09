package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// TestLearnReviewShowsTheGateAsATable: the review prints the gate's
// verdict, mode and rule, then each case with what the baseline and the
// candidate found and why a case was not compared, a target starred.
func TestLearnReviewShowsTheGateAsATable(t *testing.T) {
	h := newLearnHarness(t)
	g := engine.GateResult{State: store.LearnHeld, Mode: learn.GateJudgeOnly, Rule: "no target could be replayed (miss-3: no full replay)",
		Cases: []engine.GateCase{
			{GateRow: learn.GateRow{Case: "miss-3", Target: true, Skip: "no full replay"}},
			{GateRow: learn.GateRow{Case: "coupon-scope", Before: &eval.Score{Found: 1, Total: 2}, After: &eval.Score{Found: 2, Total: 2,
				AbsentHits: []eval.DefectResult{{ID: "decline-4"}}}, Retried: true}},
		}}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, "UPDATE learn_proposals SET gate_json = ? WHERE id = ?", string(b), h.proposal.ID); err != nil {
		t.Fatal(err)
	}
	h.d.StdinTTY = true
	h.stdin("\n")
	if code := h.cmd("learn", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "gate:\n  held (the judge alone): no target could be replayed (miss-3: no full replay)\n", "CASE", "BASELINE", "CANDIDATE",
		"miss-3*", "no full replay", "coupon-scope", "1/2", "2/2, 1 declined reported", "replayed twice", "* a target")
	if strings.Contains(out, `"rule"`) {
		t.Errorf("the gate is printed as JSON:\n%s", out)
	}
}

// TestLearnCountsTheVersionsNotPushed: magnum pushes nothing, so the list
// says how many landed versions origin/master does not hold.
func TestLearnCountsTheVersionsNotPushed(t *testing.T) {
	h := newLearnHarness(t)
	h.gitOut("update-ref", "refs/remotes/origin/master", h.base)
	for _, sha := range []string{h.base, h.cand} {
		if _, err := h.st.CreateLearnVersion(h.ctx, store.LearnVersion{LandedSHA: sha, SkillSHA: "s", PromptsSHA: "p", LandedAt: h.now}); err != nil {
			t.Fatal(err)
		}
	}
	if code := h.cmd("learn"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "1 landed version is not on origin/master: magnum pushes nothing")
	h.out.Reset()
	if code := h.cmd("learn", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var data learnListData
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil || data.Ahead == nil || *data.Ahead != 1 {
		t.Fatalf("--json ahead = %v, %v", data.Ahead, err)
	}
}
