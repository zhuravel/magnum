package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pickMerged is a PR GitHub merged, in the form the CLI lists it.
func pickMerged() PickEntry {
	return PickEntry{Ref: "talkable/talkable#5", Title: "Retire the coupon v1 API", Author: "@alice", State: "closed", Age: "2h",
		URL: "https://github.com/talkable/talkable/pull/5", GHState: "MERGED"}
}

// A merged entry asks the post-merge question (comment only) for every review
// key, and y finishes with the entry as for any other.
func TestPickerAsksPostMergeQuestionForMergedEntry(t *testing.T) {
	entries := append([]PickEntry{pickMerged()}, pickEntries()...)
	m := newPickerModel(entries, PickerOptions{})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24})
	for _, c := range []struct {
		keys   []string
		want   string
		action PickAction
	}{
		{[]string{"enter"}, "Post-merge review talkable/talkable#5 (comment only)?", PickActionReview},
		{[]string{"ctrl+r"}, "Post-merge review talkable/talkable#5 (comment only)?", PickActionAgain}, // again has no text of its own
		{[]string{"ctrl+f"}, "Fresh post-merge review of talkable/talkable#5 in new agent sessions (comment only)?", PickActionFresh},
	} {
		got, _ := send(t, m, keys(c.keys...)...)
		if got.confirm == nil || got.confirm.question != c.want {
			t.Errorf("keys %v asked %+v, want %q", c.keys, got.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(got), c.want+" y/N")
		_, out, done := pickOutcome(t, got, "y")
		if !done || out.Action != c.action || out.Entry == nil || out.Entry.Ref != "talkable/talkable#5" {
			t.Errorf("keys %v then y: %+v, want %v on talkable#5", c.keys, out, c.action)
		}
	}
	// an open entry keeps the generic question
	got, _ := send(t, m, keys("down", "enter")...)
	if want := "Review talkable/talkable#7 now (not reviewed yet, queued, last activity 5m ago)?"; got.confirm == nil || got.confirm.question != want {
		t.Errorf("an open entry asked %+v, want %q", got.confirm, want)
	}
}

// A typed reference the list lacks but Lookup finds is asked about as the PR
// it is: post-merge for a merged one, with its facts for an open one. The
// outcome still carries the typed query and no entry, for the caller to act on.
func TestPickerLookupResolvesTypedReference(t *testing.T) {
	const typed = "https://github.com/talkable/talkable/pull/5"
	open := PickEntry{Ref: "talkable/talkable#6", State: "reviewed", Age: "3h", URL: "https://github.com/talkable/talkable/pull/6", GHState: "OPEN"}
	for name, c := range map[string]struct {
		query string
		found PickEntry
		keys  []string
		want  string
	}{
		"merged": {typed, pickMerged(), []string{"enter"}, "Post-merge review talkable/talkable#5 (comment only)?"},
		"merged, fresh": {typed, pickMerged(), []string{"ctrl+f"},
			"Fresh post-merge review of talkable/talkable#5 in new agent sessions (comment only)?"},
		"open": {"talkable/talkable#6", open, []string{"enter"}, "Review talkable/talkable#6 now (no new commits since the last review, last activity 3h ago)?"},
	} {
		var asked []string
		m := newPickerModel(pickEntries(), PickerOptions{Query: c.query, Lookup: func(q string) (PickEntry, bool) {
			asked = append(asked, q)
			return c.found, true
		}})
		m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24})
		if got := visibleRefs(m); len(got) != 0 {
			t.Fatalf("%s: the list matched %v", name, got)
		}
		m, _ = send(t, m, keys(c.keys...)...)
		if m.confirm == nil || m.confirm.question != c.want {
			t.Errorf("%s: asked %+v, want %q", name, m.confirm, c.want)
			continue
		}
		if strings.Contains(m.confirm.question, "not in magnum's list") {
			t.Errorf("%s: the question calls a PR magnum knows unlisted: %q", name, m.confirm.question)
		}
		if len(asked) != 1 || asked[0] != c.query {
			t.Errorf("%s: Lookup got %q, want the typed query once", name, asked)
		}
		_, out, done := pickOutcome(t, m, "y")
		if !done || out.Entry != nil || out.Query != c.query {
			t.Errorf("%s: outcome %+v, want no entry and query %q", name, out, c.query)
		}
	}
}

// A typed reference Lookup does not find (or no Lookup at all) still says it
// is not in magnum's list; Lookup is asked once per filter that is a
// reference the list does not match, never for a listed PR or other text.
func TestPickerLookupMissKeepsNotInListQuestion(t *testing.T) {
	const typed = "talkable/talkable#999"
	const want = "Review " + typed + " now (not in magnum's list)?"
	var asked []string
	lookups := map[string]func(string) (PickEntry, bool){
		"not found": func(q string) (PickEntry, bool) { asked = append(asked, q); return PickEntry{}, false },
		"nil":       nil,
	}
	for name, lookup := range lookups {
		asked = nil
		m := newPickerModel(pickEntries(), PickerOptions{Query: typed, Lookup: lookup})
		m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24}, keyMsg("enter"))
		if m.confirm == nil || m.confirm.question != want {
			t.Errorf("%s: asked %+v, want %q", name, m.confirm, want)
		}
		if name == "not found" && len(asked) != 1 {
			t.Errorf("%s: Lookup asked %v, want one call", name, asked)
		}
	}

	asked = nil
	lookup := func(q string) (PickEntry, bool) { asked = append(asked, q); return pickMerged(), true }
	m := newPickerModel(pickEntries(), PickerOptions{Query: typed, Lookup: lookup})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24})
	if _, out, done := pickOutcome(t, m, "ctrl+g"); !done || out.Action != PickActionOpen || out.Entry != nil || len(asked) != 1 {
		t.Errorf("ctrl+g on a typed reference: %+v, Lookup asked %v; want it to finish without a question, after the one lookup of the filter", out, asked)
	}
	asked = nil
	m = newPickerModel(pickEntries(), PickerOptions{Query: "talkable/talkable#7", Lookup: lookup})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24}, keyMsg("enter"))
	if len(asked) != 0 || m.confirm == nil || m.confirm.entry == nil {
		t.Errorf("a listed PR asked Lookup %v or lost its entry: %+v", asked, m.confirm)
	}
	// text that is no reference never reaches Lookup
	m = newPickerModel(pickEntries(), PickerOptions{Query: "zzzz", Lookup: lookup})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24}, keyMsg("enter"))
	if len(asked) != 0 || m.confirm != nil {
		t.Errorf("a query that is no reference asked Lookup %v or a question %+v", asked, m.confirm)
	}
}

// A typed merged reference magnum knows is described as that PR before any
// key: the preview shows it (merged on GitHub) and the footer offers a
// post-merge review; nothing says it is not in magnum's list.
func TestPickerDescribesATypedMergedReference(t *testing.T) {
	const typed = "https://github.com/talkable/talkable/pull/5"
	m := newPickerModel(pickEntries(), PickerOptions{Query: typed, Lookup: func(string) (PickEntry, bool) { return pickMerged(), true }})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24})
	v := viewOf(m)
	mustContain(t, v, "Retire the coupon v1 API", "merged on GitHub", "merged: enter asks for a post-merge review of talkable/talkable#5")
	mustNotContain(t, v, "not in magnum's list", "not in the list")
}
