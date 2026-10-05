package cli

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The board's round facts name a delta check and its lines.
func TestBoardRoundFactsNameADeltaCheck(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false)
	f.event("engine.round_start", map[string]any{"slot": "review1", "kind": "rereview", "target_sha": "abc1234",
		"roles": []string{"codex-judge"}, "requested": nil, "post_merge": false, "delta_check": true, "delta_lines": 4})
	want := &tui.RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4, Roles: []string{"codex-judge"}}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

// `magnum status` names a delta check: one in flight in the rounds line and
// the PR's next step, one waiting in its queue row (the daemon's wait).
func TestStatusNamesADeltaCheck(t *testing.T) {
	h := newActHarness(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	running := h.seedPR("talkable/talkable", 730, store.PRReviewing)
	other := h.seedPR("talkable/talkable", 731, store.PRReviewing)
	waiting := h.seedPR("talkable/talkable", 732, store.PRRereviewPending)
	b, _ := json.Marshal(engine.DeltaCheckRound{Lines: 4, Files: 4, Target: "abc1234"})
	w, _ := json.Marshal(engine.Wait{Reason: engine.WaitQuiet, Rereview: true, DeltaCheck: true, Until: now.Add(4 * time.Minute)})
	for key, v := range map[string]string{engine.KVPRDeltaCheck(running.ID): string(b), engine.KVPRWait(waiting.ID): string(w),
		engine.KVPRDeltaCheck(waiting.ID): string(b)} { // a leftover of an earlier round says nothing about a waiting PR
		if err := h.st.SetKV(h.ctx, key, v); err != nil {
			t.Fatal(err)
		}
	}

	if got := statusNextWithGate(h.ctx, h.st, running, now); got != "delta check (4 lines) in progress" {
		t.Errorf("next of the delta check in flight = %q", got)
	}
	if got := statusNextWithGate(h.ctx, h.st, other, now); got != "round in progress" {
		t.Errorf("next of another round = %q", got)
	}
	if got := statusNextShort(h.ctx, h.st, waiting, now); got != "delta check · quiet → 12:04" {
		t.Errorf("next of the waiting delta check = %q", got)
	}
	var r statusReport
	if err := statusGatherPRs(h.ctx, statusDeps{Store: h.st}, now, &r); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Rounds.PRs, "talkable#730 (delta check)") || !slices.Contains(r.Rounds.PRs, "talkable#731") {
		t.Errorf("rounds = %q", r.Rounds.PRs)
	}
}
