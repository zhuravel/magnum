package engine

import (
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/attention"
)

// The factory screen draws the fixer's own Needs you entries in place of
// magnum's words, but the settle and the loop's end write the attention
// only after they left out the entries whose magnum thread is resolved, or
// that the fixer answered, and name the rest. FixNeedsAsked keeps of the
// report's entries those the message still asks for: each one it names (by
// its call's link, its comment id in the older words, a conflict by its
// paths, an entry magnum cannot read by their count, after a loop's own
// words too), never an id that only starts the same, and of a message that
// only counts them, all when it counts them all and none otherwise (it
// cannot say which went). A message that asks for no call keeps none.
func TestFixNeedsAskedKeepsWhatTheAttentionStillAsks(t *testing.T) {
	const report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
	const page = "https://github.com/talkable/talkable/pull/2"
	e1 := "2101 P1 app/models/offer.rb:42: not fixed. which rounding does the cart want?"
	e2 := "2102 P2 app/b.rb:7: not fixed. keep or drop the old rate?"
	e3 := `5808905525 description: change "fixes the cart total" to "rounds the cart total"`
	e4 := "conflict: app/a.rb, db/schema.rb: which side of the rate wins?"
	e5 := "the deploy needs a feature flag nobody named"
	short := "210 P3 app/c.rb:1: not fixed. a nit the author decides"
	calls := attention.FixCallsText([]attention.FixCall{
		attention.DeclinedCall("2102", page+"#discussion_r2102", "P2", "app/b.rb:7", "talkable#2", true),
		attention.ConflictCall([]string{"app/a.rb", "db/schema.rb"}, page),
	}, "1 more finding needs your call (5808905525 description)", report)
	resolve := attention.FixCallsText([]attention.FixCall{attention.ResolveCall("2101", page+"#discussion_r2101")}, "", report)
	for name, c := range map[string]struct {
		msg           string
		entries, want []string
	}{
		"named, one resolved since": {"fix: 1 finding needs your call (2102 P2 app/b.rb:7): " + report, []string{e1, e2}, []string{e2}},
		"counted, all":              {"fix: 2 findings need your call: " + report, []string{e1, e2}, []string{e1, e2}},
		"counted, fewer":            {"fix: 1 finding needs your call: " + report, []string{e1, e2}, nil},
		"counted with a conflict":   {"fix: 1 finding and 1 conflict need your call: " + report, []string{e1, e4}, []string{e1, e4}},
		"calls and the older words": {"fix: " + calls, []string{e1, e2, e3, e4}, []string{e2, e3, e4}},
		"a call to resolve":         {"fix: " + resolve, []string{e1, e2}, []string{e1}},
		"an entry magnum cannot read": {"fix: 2 findings need your call (2101 P1 app/models/offer.rb:42, 1 entry magnum could not read): " + report,
			[]string{e1, e2, e5}, []string{e1, e5}},
		"after the loop's own words": {"fix loop ended: the re-review did not post (timeout); 1 finding needs your call (2102 P2 app/b.rb:7): " + report,
			[]string{e1, e2}, []string{e2}},
		"an id that starts the same": {"fix: 1 finding needs your call (2101 P1 app/models/offer.rb:42): " + report, []string{short, e1}, []string{e1}},
		"the unread line under it": {"fix: 1 finding needs your call (2102 P2 app/b.rb:7): " + report +
			"\nthe other reviewers' threads and comments were not read (HTTP 502): `magnum fix talkable/talkable#2` reads them again", []string{e1, e2}, []string{e2}},
		"the cap": {"3 fix cycles, 1 finding left: 1 new: " + report, []string{e1}, nil},
	} {
		if got := FixNeedsAsked(c.msg, c.entries); !slices.Equal(got, c.want) {
			t.Errorf("%s: FixNeedsAsked = %q, want %q", name, got, c.want)
		}
	}
}
