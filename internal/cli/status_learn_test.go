package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// TestStatusLearnLineShowsTheLoop: `magnum status` says what the
// self-improving loop waits for: the newest proposal and what became of it,
// the held ones (for the operator), the passed ones (for the landing), the
// gating ones, and a landed version the daemon has to restart on; nothing
// when no proposal was ever made.
func TestStatusLearnLineShowsTheLoop(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	r, out := statusRetroOut(t, d)
	if r.Learn != nil || strings.Contains(out, "learn:") {
		t.Fatalf("learn = %+v\n%s", r.Learn, out)
	}
	var last store.LearnProposal
	for i, state := range []string{store.LearnLanded, store.LearnHeld, store.LearnPassed, store.LearnGating} {
		p, err := st.CreateLearnProposal(ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRequest, BaseSHA: "b", Branch: "learn/x",
			State: state, At: now.Add(time.Duration(i-5) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		last = p
	}
	mark, _ := json.Marshal(engine.LearnRestartMark{Version: 2, Proposal: 1, Landed: now.Add(-time.Hour)})
	if err := st.SetKV(ctx, engine.KVLearnRestart, string(mark)); err != nil {
		t.Fatal(err)
	}
	r, out = statusRetroOut(t, d)
	actContains(t, out, "learn:    last proposal "+itoa(last.ID)+" gating, made 2h ago · 1 held for review (`magnum learn --review`) · "+
		"1 passed, waiting to land · 1 at the gate · restarts on skill version 2 once nothing is in flight\n")
	if l := r.Learn; l == nil || l.Held != 1 || l.Passed != 1 || l.Gating != 1 || l.Restart == nil || l.Restart.Version != 2 {
		t.Fatalf("learn = %+v", r.Learn)
	}
	// A restart the operator has to do says how.
	mark, _ = json.Marshal(engine.LearnRestartMark{Version: 2, Manual: true})
	if err := st.SetKV(ctx, engine.KVLearnRestart, string(mark)); err != nil {
		t.Fatal(err)
	}
	_, out = statusRetroOut(t, d)
	actContains(t, out, "restart needed for skill version 2: `magnum daemon-restart --when-idle`\n")
	if i, j := strings.Index(out, "learn:"), strings.Index(out, "rounds:"); i < 0 || j < i {
		t.Errorf("learn line out of place:\n%s", out)
	}
}
