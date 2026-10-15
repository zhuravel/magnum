package cli

import (
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/tui"
)

// The board's round facts name the full pass that follows a review that
// held its approval (its start event's before_approval), and `magnum tui
// --json` mirrors it.
func TestBoardRoundFactsNameAFullPassBeforeApproval(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false)
	f.event("engine.round_start", map[string]any{"slot": "review1", "kind": "recovery", "target_sha": "abc1234",
		"roles": []string{"codex-judge", "claude-review"}, "requested": nil, "post_merge": false, "full_pass": true,
		"full_pass_reason": "before approval: the clean re-review of abc1234 in round 4 posted as a comment after 2 re-reviews since the first review (approve_after_full_pass 2)",
		"before_approval":  true})
	want := &tui.RoundWhy{Kind: "recovery", FullPass: true, BeforeApproval: true, Roles: []string{"codex-judge", "claude-review"}}
	got := f.why()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
	if j := prsJSONRoundWhyOf(*got); !j.FullPass || !j.BeforeApproval {
		t.Fatalf("the JSON mirror = %+v, want full_pass and before_approval", j)
	}
}
