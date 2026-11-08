package attention

import (
	"slices"
	"strings"
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

// A finding of magnum's whose Fix ends "This is the author's call." asks
// for your choice: the fixer did not decline it, and `magnum fix` would
// only ask again. Its call says the review leaves the choice to you, with
// the two ways to give it (a reply in the thread, or `magnum fix --decide`
// on that thread); the card reads it back as one decision, and it is a
// decision call, which --decide answers.
func TestAnAuthorsCallLeavesTheChoiceToYou(t *testing.T) {
	const (
		report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
		url    = "https://github.com/talkable/talkable/pull/2#discussion_r4302866759"
		want   = "decide magnum's P2 on app/models/offer.rb:42: the review leaves the choice to you; reply with yours at " + url +
			", or run `magnum fix talkable/talkable#2 --decide 4302866759=\"<your choice>\"`"
	)
	c := AuthorsCall("4302866759", url, "P2", "app/models/offer.rb:42", "talkable/talkable#2", true)
	if c.Action != want || c.Thread != "4302866759" || c.URL != url {
		t.Fatalf("AuthorsCall = %+v\nwant action %q", c, want)
	}
	for _, gone := range []string{"declined", "sends it back"} {
		if strings.Contains(c.Action, gone) {
			t.Errorf("the call says %q", gone)
		}
	}
	msg := "fix: " + FixCallsText([]FixCall{c}, "", report)
	r := Explain(KindFix, msg, "talkable#2")
	if !slices.Equal(r.NeedsYou, []FixCall{c}) || r.Summary != "fix: decide 1 finding" {
		t.Errorf("the card: needs you %+v, summary %q", r.NeedsYou, r.Summary)
	}
	if calls, ok := FixDecisionCalls(msg); !ok || !slices.Equal(calls, []FixCall{c}) {
		t.Errorf("FixDecisionCalls = %+v, %v; want the author's call as a decision", calls, ok)
	}
	// A product choice another reviewer's thread asks for is the author's
	// call too (the fix skill's step 6): nobody declined it either.
	const other = "https://github.com/talkable/talkable/pull/2#discussion_r4322458360"
	o := AuthorsCall("4322458360", other, "", "lib/sandbox.rb:185", "talkable/talkable#2", false)
	if want := "decide another reviewer's finding on lib/sandbox.rb:185: the review leaves the choice to you; reply with yours at " + other +
		", or run `magnum fix talkable/talkable#2 --decide 4322458360=\"<your choice>\"`"; o.Action != want {
		t.Fatalf("AuthorsCall in another reviewer's thread = %q\nwant %q", o.Action, want)
	}
	msg = "fix: " + FixCallsText([]FixCall{c, o}, "", report)
	if calls, ok := FixDecisionCalls(msg); !ok || !slices.Equal(calls, []FixCall{c, o}) {
		t.Errorf("FixDecisionCalls = %+v, %v; want both author's calls as decisions", calls, ok)
	}
}
