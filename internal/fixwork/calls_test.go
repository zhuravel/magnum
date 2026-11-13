package fixwork

import (
	"slices"
	"strings"
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

// The fixer's ci entries under Needs you name the job and one class word:
// the settle reads only that word, never the reason.
func TestNeedsYouEntryReadsACIEntryAndItsClass(t *testing.T) {
	class, ci, rest := CICannotOf([]string{"129341926468 ci: not fixed. infra: the runner lost its disk",
		"2101 P2 app/a.rb:4: not fixed. contested", "77 ci: not fixed. the log is cut"})
	if class != "infra" || !ci || len(rest) != 1 || !strings.HasPrefix(rest[0], "2101 P2") {
		t.Errorf("class %q, ci %v, rest %q", class, ci, rest)
	}
	for entry, want := range map[string]string{
		"5 ci: not fixed. flaky: passes again": "flaky", "5 ci: not fixed. secret: STRIPE_KEY": "secret", "5 ci: not fixed. config: a step": "config",
		"5 ci: not fixed. base: master fails too": "base", "5 ci: not fixed. whatever": "unfixed",
	} {
		if got, ok, _ := CICannotOf([]string{entry}); got != want || !ok {
			t.Errorf("%q: class %q, %v; want %q", entry, got, ok, want)
		}
	}
	if m := needsYouEntryRe.FindStringSubmatch("2101 P1 app/models/offer.rb:42: not fixed. The author's call: a or b"); m == nil || m[7] == "" || m[8] != "" {
		t.Errorf("the author's call: %q", m)
	}
}
