package attention

import "testing"

// The factory screen said only "magnum: fix" of an own PR whose fixer left
// one item to the author (talkable#12215): the item was in the fixer's
// report, which the fix job's message names. FixReport is that report: the
// last path to a .md file of the message's first line, whatever words come
// before it; none for a message that names none, and never a path of a
// later line (the other reviewers' note withUnread adds).
func TestFixReportIsTheReportAFixMessageNames(t *testing.T) {
	const report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
	thread := "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
	for name, c := range map[string]struct{ msg, want string }{
		"the settle's count":  {"fix: 1 finding needs your call: " + report, report},
		"the loop's names":    {"fix: 2 findings need your call (2101 P1 app/a.rb:42, 5485002407 description): " + report, report},
		"calls":               {"fix: " + FixCallsText([]FixCall{FixedCall("4302616576", thread, "b2b2b2b", false)}, "", report), report},
		"a cap":               {"5 fix cycles (14 on this PR), 3 findings left: 1 new, 0 missed earlier, 2 fallout: " + report, report},
		"the unread note":     {"fix: 1 finding needs your call: " + report + "\nthe other reviewers' threads and comments were cut", report},
		"no report":           {"fix guard failed: slot review1 holds c3c3c3c, not the reviewed head a1a1a1a", ""},
		"a path on line two":  {"fix guard failed: slot review1 holds c3c3c3c\n" + report, ""},
		"another file's path": {"fix run failed: claude-fix wrote no report at /state/reviews/x/claude-fix.txt", ""},
	} {
		if got := FixReport(c.msg); got != c.want {
			t.Errorf("%s: FixReport(%q) = %q, want %q", name, c.msg, got, c.want)
		}
	}
}

// What a fix job's message asks of the author decides what the screen
// draws of the fixer's report: the calls of what the fixer left (named,
// counted or as calls, after a loop's own words too) ask for its Needs you;
// a cap or a failed run asks for something else, and its report's Needs you
// is not what the attention is about.
func TestFixAsksOnlyForWhatTheFixerLeft(t *testing.T) {
	const report = "/state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
	thread := "https://github.com/talkable/talkable/pull/2#discussion_r4302616576"
	calls := []FixCall{FixedCall("4302616576", thread, "b2b2b2b", false)}
	for msg, want := range map[string]bool{
		"fix: 1 finding needs your call: " + report:                                                                                          true,
		"fix: 2 findings need your call (2101 P1 app/a.rb:42, 5485002407 description): " + report:                                            true,
		"fix: 1 conflict needs your call (conflict in app/a.rb): " + report:                                                                  true,
		"fix: 1 finding and 1 conflict need your call (conflict in app/a.rb, 2101 P1 app/b.rb:3): " + report:                                 true,
		"fix: " + FixCallsText(calls, "", report):                                                                                            true,
		"fix: " + FixCallsText(calls, "1 more finding needs your call (2101 P1 app/a.rb:42)", report):                                        true,
		"fix loop ended: the re-review did not post (error); 2 findings need your call (2101 P1 app/a.rb:42, 2102 P2 app/b.rb:7): " + report: true,
		"fix loop ended: the re-review did not post (error); " + FixCallsText(calls, "", report):                                             true,
		"fix: 1 finding needs your call: " + report + "\nthe other reviewers' threads and comments were cut":                                 true,
		"5 fix cycles (14 on this PR), 3 findings left: 1 new, 0 missed earlier, 2 fallout: " + report:                                       false,
		"fix run failed: the fixer's push failed: " + report:                                                                                 false,
		"fix run failed: fix report: a section is missing: no ## Needs you":                                                                  false,
		"fix guard failed: slot review1 holds c3c3c3c, not the reviewed head a1a1a1a":                                                        false,
		"fix loop ended: the re-review did not post (error)":                                                                                 false,
	} {
		if got := FixAsks(msg); got != want {
			t.Errorf("FixAsks(%q) = %v, want %v", msg, got, want)
		}
	}
}
