package cli

import (
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/tui"
)

// The board's round facts name a scheduled audit pass (a recovery in fresh
// sessions, its start event's audit_pass), and `magnum tui --json` mirrors
// it.
func TestBoardRoundFactsNameAnAuditPass(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false)
	f.event("engine.round_start", map[string]any{"slot": "review1", "kind": "recovery", "target_sha": "abc1234",
		"roles": []string{"codex-judge", "claude-review"}, "requested": nil, "post_merge": false, "audit_pass": true, "audit_areas": 4})
	want := &tui.RoundWhy{Kind: "recovery", AuditPass: true, Roles: []string{"codex-judge", "claude-review"}}
	got := f.why()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
	if j := prsJSONRoundWhyOf(*got); !j.AuditPass || j.FullPass {
		t.Fatalf("the JSON mirror = %+v, want audit_pass", j)
	}
}
