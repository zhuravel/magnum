package fixwork

import (
	"slices"
	"testing"
)

// The settle's filters decide here; the engine's settle tests run them
// with GitHub's threads and the files beside a report. These pin the rules
// alone.

// A conflict of the base merge is the only entry the push of the merged
// tip drops.
func TestOnlyAConflictOfTheBaseMergeIsAConflictEntry(t *testing.T) {
	for entry, want := range map[string]bool{
		"conflict: app/a.rb, db/schema.rb: keep both indexes?": true,
		"2101 P1 app/a.rb:3: not fixed. wants a schema change": false,
		"not an entry": false,
	} {
		if got := IsConflictEntry(entry); got != want {
			t.Errorf("IsConflictEntry(%q) = %v, want %v", entry, got, want)
		}
	}
}

// A decision the fixer built is built; one it listed itself goes back as
// it wrote it; one it neither built nor listed goes back as magnum's own
// entry, which asks the author to decide again.
func TestADecisionTheFixerNeitherBuiltNorListedGoesBackAsMagnumsEntry(t *testing.T) {
	entries := []string{"300 app/b.rb:9: not fixed. contested"}
	decided := []Decision{{CommentID: 100, Path: "app/a.rb", Line: 3}, {CommentID: 200, Path: "app/c.rb", Line: 5}, {CommentID: 300, Path: "app/b.rb", Line: 9}}
	out, built, back := DecisionsLeft(entries, decided, map[int64]bool{100: true})
	want := []string{entries[0], "200 app/c.rb:5: not fixed. magnum: GitHub shows no reply of the fixer's that builds your decision"}
	if !slices.Equal(out, want) || len(built) != 1 || !built[100] || back != 2 {
		t.Errorf("out %q, built %v, back %d; want %q, 100 built, 2 back", out, built, back, want)
	}
}

// In magnum's own threads, a resolved one drops its entry and one answered
// as its call asks keeps it for the resolution alone; an entry in no
// thread of magnum's, or one the threads do not list, stays as it is.
func TestAResolvedThreadDropsItsEntryAndAnAnsweredOneAsksOnlyToResolve(t *testing.T) {
	entries := []string{"1 P1 a.rb:1: fixed in abc1234.", "2 P2 b.rb:2: fixed in abc1234.", "3 P2 c.rb:3: not fixed. no", "4 P2 d.rb:4: not fixed. no"}
	mine := func(entry string) (NeedsYouEntry, int64, bool) {
		n, _ := ReadNeedsYouEntry(entry)
		id := map[string]int64{"1": 1, "2": 2, "3": 3, "4": 4}[n.ID]
		return n, id, id != 4
	}
	threads := map[int64]ThreadNow{1: {Resolved: true}, 2: {Answer: "fixed"}, 4: {Resolved: true}}
	answers := func(n NeedsYouEntry, answer string) bool { return answer == "fixed" && n.FixedIn != "" }
	kept, answered, resolved := FreshEntries(entries, mine, threads, answers)
	if !slices.Equal(kept, entries[1:]) || len(answered) != 1 || !answered["2"] || resolved != 1 {
		t.Errorf("kept %q, answered %v, resolved %d; want the last three, 2 answered, 1 resolved", kept, answered, resolved)
	}
}

// Only a P3 at a path and line, with its thread's id, can be an optional
// call; any other entry under Optional calls holds as Needs you does.
func TestOnlyAP3AtAPlaceCanBeAnOptionalCall(t *testing.T) {
	for entry, want := range map[string]bool{
		"5200 P3 app/a.rb:4: rename or keep": true,
		"5200 P2 app/a.rb:4: rename or keep": false,
		"5200 P3 description: one or two":    false,
		"conflict: app/a.rb: keep both?":     false,
	} {
		if _, id, ok := OptionalEntry(entry); ok != want || ok && id != 5200 {
			t.Errorf("OptionalEntry(%q) = %d, %v; want %v", entry, id, ok, want)
		}
	}
}
