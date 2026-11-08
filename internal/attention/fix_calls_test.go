package attention

import (
	"slices"
	"testing"
)

// FixDecisionCalls are the calls of an attention that asks for the fixer's
// decision calls and nothing else, in other reviewers' threads and in
// magnum's own (`magnum fix --decide` takes both): the engine lets a loop
// start over it once you decided each one, and lifts it once the fixer
// built them. A reply call, a conflict, a finding in the older words, the
// loop's own words before the calls and a line under them each make it ask
// for something else.
func TestFixDecisionCallsAreTheFixersDecisionsAlone(t *testing.T) {
	const (
		report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
		page   = "https://github.com/talkable/talkable/pull/2"
		other  = page + "#discussion_r4302616576"
		second = page + "#discussion_r4302616999"
	)
	decide := DeclinedCall("4302616576", other, "", "lib/sandbox.rb:185", "talkable/talkable#2", false)
	decide2 := DeclinedCall("4302616999", second, "P2", "app/a.rb:3", "talkable/talkable#2", false)
	magnums := DeclinedCall("2101", page+"#discussion_r2101", "P2", "app/b.rb:1", "talkable/talkable#2", true)
	msg := "fix: " + FixCallsText([]FixCall{decide, decide2, magnums}, "", report)
	if calls, ok := FixDecisionCalls(msg); !ok || !slices.Equal(calls, []FixCall{decide, decide2, magnums}) {
		t.Errorf("FixDecisionCalls = %+v, %v; want the three decisions", calls, ok)
	}
	for name, msg := range map[string]string{
		"a reply call": "fix: " + FixCallsText([]FixCall{decide, FixedCall("4302616999", second, "b2b2b2b", false)}, "", report),
		"a conflict":   "fix: " + FixCallsText([]FixCall{ConflictCall([]string{"db/schema.rb"}, page), decide}, "", report),
		"older words":  "fix: " + FixCallsText([]FixCall{decide}, "1 more finding needs your call (5808905525 description)", report),
		"loop's words": "fix loop ended: the re-review did not post (error); " + FixCallsText([]FixCall{decide}, "", report),
		"a line under": "fix: " + FixCallsText([]FixCall{decide}, "", report) + "\nthe other reviewers' threads and comments were not read (HTTP 502)",
		"no call":      "fix: 1 finding needs your call: " + report,
	} {
		if calls, ok := FixDecisionCalls(msg); ok {
			t.Errorf("%s: FixDecisionCalls = %+v, want none", name, calls)
		}
	}
}
