package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func pickEntries() []PickEntry {
	return []PickEntry{
		{Ref: "talkable/talkable#7", Title: "Fix the referral widget", Author: "@ann", State: "queued", Age: "5m", URL: "https://github.com/talkable/talkable/pull/7"},
		{Ref: "talkable/talkable#1", Title: "Bump rails", Author: "@bob", State: "reviewed,pinned", Age: "1h", URL: "https://github.com/talkable/talkable/pull/1", Pinned: true},
		{Ref: "talkable/talkable#12", Title: "Add coupon export", Author: "@cid", State: "queued", Age: "2d", URL: "https://github.com/talkable/talkable/pull/12"},
		{Ref: "talkable/magnum#7", Title: "Picker screen", Author: "@dee", State: "new", Age: "3m", URL: "https://github.com/talkable/magnum/pull/7"},
	}
}

func newPicker(t *testing.T, query string, w, h int) pickerModel {
	t.Helper()
	m := newPickerModel(pickEntries(), PickerOptions{Query: query})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// pickOutcome sends keys and returns the outcome when they ended the picker.
func pickOutcome(t *testing.T, m pickerModel, names ...string) (pickerModel, PickOutcome, bool) {
	t.Helper()
	m, cmd := send(t, m, keys(names...)...)
	if !m.done {
		return m, PickOutcome{}, false
	}
	if !isQuit(execCmd(cmd)) {
		t.Fatal("picker finished without quitting")
	}
	return m, m.outcome, true
}

func visibleRefs(m pickerModel) []string {
	var out []string
	for _, it := range m.list.VisibleItems() {
		out = append(out, it.(pickItem).e.Ref)
	}
	return out
}

func TestPickerListsEntriesWithPreview(t *testing.T) {
	m := newPicker(t, "", 120, 24)
	v := viewOf(m)
	mustContain(t, v, "magnum pick", "PR> ", "talkable/talkable#7", "Fix the referral widget", "talkable/magnum#7",
		"› talkable/talkable#7", "author", "@ann", "https://github.com/talkable/talkable/pull/7",
		"enter review", "^p pin/unpin", "esc cancel")
	if w := maxLineWidth(v); w > 120 {
		t.Fatalf("widest line %d > 120:\n%s", w, v)
	}
	if n := len(strings.Split(v, "\n")); n > 24 {
		t.Fatalf("view is %d lines, want <= 24:\n%s", n, v)
	}
	if w := maxLineWidth(v); w != 120 {
		t.Errorf("list plus preview is %d cells wide, want the full 120", w)
	}
}

func TestPickerNarrowHidesPreview(t *testing.T) {
	m := newPicker(t, "", 70, 20)
	v := viewOf(m)
	mustContain(t, v, "talkable/talkable#7")
	mustNotContain(t, v, "https://github.com/talkable/talkable/pull/7")
	if w := maxLineWidth(v); w > 70 {
		t.Fatalf("widest line %d > 70:\n%s", w, v)
	}
}

func TestPickerFuzzyFilterAndEnter(t *testing.T) {
	m := newPicker(t, "", 120, 24)
	m, _ = send(t, m, typed("widget")...)
	if got := visibleRefs(m); len(got) != 1 || got[0] != "talkable/talkable#7" {
		t.Fatalf("visible after typing = %v", got)
	}
	_, out, ok := pickOutcome(t, m, "enter", "y")
	if !ok || out.Action != PickActionReview || out.Entry == nil || out.Entry.Ref != "talkable/talkable#7" || out.Query != "widget" {
		t.Fatalf("outcome = %+v (entry %+v)", out, out.Entry)
	}
}

func TestPickerPrefilledQuery(t *testing.T) {
	m := newPicker(t, "bump", 120, 24)
	if got := visibleRefs(m); len(got) == 0 || got[0] != "talkable/talkable#1" {
		t.Fatalf("visible with query bump = %v, want talkable#1 ranked first", got)
	}
	mustContain(t, viewOf(m), "PR> bump")
	m, _ = send(t, m, keys("backspace", "backspace", "backspace", "backspace")...)
	if got := visibleRefs(m); len(got) != 4 {
		t.Fatalf("clearing the query left %v", got)
	}
}

func TestPickerReferenceQueriesMatchExactly(t *testing.T) {
	for query, want := range map[string]string{
		"https://github.com/talkable/talkable/pull/7":     "talkable/talkable#7",
		"https://github.com/talkable/magnum/pull/7/files": "talkable/magnum#7",
		"talkable/talkable#1":                             "talkable/talkable#1",
		"magnum#7":                                        "talkable/magnum#7",
		"#12":                                             "talkable/talkable#12",
		"https://github.com/talkable/talkable/pull/12?w=1": "talkable/talkable#12",
	} {
		m := newPicker(t, query, 120, 24)
		if got := visibleRefs(m); len(got) != 1 || got[0] != want {
			t.Errorf("query %q shows %v, want [%s]", query, got, want)
		}
	}
	m := newPicker(t, "7", 120, 24)
	if got := visibleRefs(m); len(got) != 2 {
		t.Errorf("query 7 shows %v, want both #7s", got)
	}
}

func TestPickerUnknownReferenceReturnsQuery(t *testing.T) {
	url := "https://github.com/talkable/talkable/pull/999"
	m := newPicker(t, url, 120, 24)
	if got := visibleRefs(m); len(got) != 0 {
		t.Fatalf("unknown URL matched %v", got)
	}
	mustContain(t, viewOf(m), "not in the list: enter reviews "+url, "enter reviews it")
	m, _ = send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(m), "Review "+url+" now (not in magnum's list)? y/N")
	_, out, ok := pickOutcome(t, m, "y")
	if !ok || out.Action != PickActionReview || out.Entry != nil || out.Query != url {
		t.Fatalf("outcome = %+v", out)
	}
	m = newPicker(t, "magnum#99", 120, 24)
	_, out, ok = pickOutcome(t, m, "ctrl+g")
	if !ok || out.Action != PickActionOpen || out.Entry != nil || out.Query != "magnum#99" {
		t.Fatalf("ctrl+g on an unknown ref = %+v", out)
	}
}

func TestPickerNoMatchStays(t *testing.T) {
	m := newPicker(t, "zzzz", 120, 24)
	m, _, ok := pickOutcome(t, m, "enter")
	if ok {
		t.Fatal("enter with no match and no reference finished")
	}
	mustContain(t, viewOf(m), `no PR matches "zzzz"`)
	m, _ = send(t, m, keys("backspace")...)
	mustNotContain(t, viewOf(m), "no PR matches")

	empty := newPickerModel(nil, PickerOptions{Title: "pick a PR"})
	empty, _ = send(t, empty, tea.WindowSizeMsg{Width: 100, Height: 20}, keyMsg("enter"))
	mustContain(t, viewOf(empty), "pick a PR", "magnum lists no open PRs yet")
}

func TestPickerActionKeys(t *testing.T) {
	for k, want := range map[string]PickAction{
		"ctrl+r": PickActionAgain, "ctrl+f": PickActionFresh, "ctrl+g": PickActionOpen, "ctrl+o": PickActionBrowser,
		"ctrl+p": PickActionTogglePin, "ctrl+x": PickActionRelease, "enter": PickActionReview,
	} {
		m := newPicker(t, "", 120, 24)
		names := []string{"down", k}
		if want == PickActionReview || want == PickActionAgain || want == PickActionFresh {
			names = append(names, "y") // the review keys ask first
		}
		_, out, ok := pickOutcome(t, m, names...)
		if !ok || out.Action != want || out.Entry == nil || out.Entry.Ref != "talkable/talkable#1" {
			t.Errorf("%s: outcome %+v, want %v on talkable#1", k, out, want)
			continue
		}
		if k == "ctrl+p" && !out.Entry.Pinned {
			t.Error("ctrl+p lost Entry.Pinned")
		}
	}
	if PickActionTogglePin.String() != "toggle-pin" || PickAction(42).String() != "PickAction(42)" {
		t.Errorf("String: %s, %s", PickActionTogglePin, PickAction(42))
	}
}

func TestPickerNavigation(t *testing.T) {
	m := newPicker(t, "", 120, 24)
	m, _ = send(t, m, keys("down", "down", "up", "ctrl+n")...)
	mustContain(t, viewOf(m), "› talkable/talkable#12", "Add coupon export", "@cid")
	m, _ = send(t, m, keys("ctrl+k")...)
	mustContain(t, viewOf(m), "› talkable/talkable#1")
}

func TestPickerCancel(t *testing.T) {
	for _, k := range []string{"esc", "ctrl+c"} {
		m := newPicker(t, "", 120, 24)
		_, out, ok := pickOutcome(t, m, k)
		if !ok || out.Action != PickActionCancel || out.Entry != nil {
			t.Errorf("%s: outcome %+v, want cancel", k, out)
		}
	}
	// q is typed, also as the first letter of a query.
	for _, in := range [][]string{{"q"}, {"b", "q"}} {
		m := newPicker(t, "", 120, 24)
		m, _, ok := pickOutcome(t, m, in...)
		if ok {
			t.Fatalf("%v cancelled instead of typing", in)
		}
		mustContain(t, viewOf(m), "PR> "+strings.Join(in, ""))
	}
}

func TestRunPickerEndToEnd(t *testing.T) {
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close(); r.Close() })
	old := extraProgramOptions
	extraProgramOptions = []tea.ProgramOption{tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	t.Cleanup(func() { extraProgramOptions = old })

	type result struct {
		out PickOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := RunPicker(context.Background(), pickEntries(), PickerOptions{Query: "coupon"})
		done <- result{out, err}
	}()
	time.Sleep(100 * time.Millisecond)                 // let the program start reading
	if _, err := w.Write([]byte("\x0f")); err != nil { // ctrl+o
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.err != nil || res.out.Action != PickActionBrowser || res.out.Entry == nil || res.out.Entry.Ref != "talkable/talkable#12" {
			t.Fatalf("RunPicker = %+v, %v", res.out, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPicker did not return")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := RunPicker(ctx, pickEntries(), PickerOptions{Query: "x"})
	if err != nil || out.Action != PickActionCancel {
		t.Fatalf("RunPicker with an ended ctx = %+v, %v", out, err)
	}
}

// enter, ^r and ^f ask before the picker hands back a review; any other
// key cancels the question and leaves the picker up.
func TestPickerReviewKeysAsk(t *testing.T) {
	for k, want := range map[string]PickAction{"enter": PickActionReview, "ctrl+r": PickActionAgain, "ctrl+f": PickActionFresh} {
		m := newPicker(t, "", 120, 24)
		m, _, done := pickOutcome(t, m, "down", k)
		if done || m.confirm == nil {
			t.Fatalf("%s alone finished the picker or did not ask", k)
		}
		mustContain(t, viewOf(m), "y/N")
		for _, no := range []string{"enter", "n", "esc", "q"} {
			c, _, done := pickOutcome(t, m, no)
			if done || c.confirm != nil {
				t.Errorf("%s then %s finished the picker or kept asking", k, no)
			}
			mustNotContain(t, viewOf(c), "y/N")
			mustContain(t, viewOf(c), "enter review") // the footer is back
		}
		if c, _ := send(t, m, tea.PasteMsg{Content: "x"}); c.confirm != nil || c.query() != "" {
			t.Errorf("%s then a paste kept asking or typed", k)
		}
		for _, yes := range []string{"y", "Y"} {
			_, out, done := pickOutcome(t, m, yes)
			if !done || out.Action != want || out.Entry == nil || out.Entry.Ref != "talkable/talkable#1" {
				t.Errorf("%s then %s: %+v, want %v on talkable#1", k, yes, out, want)
			}
		}
		if _, out, done := pickOutcome(t, m, "ctrl+c"); !done || out.Action != PickActionCancel {
			t.Errorf("%s then ctrl+c: %+v, want cancel", k, out)
		}
	}

	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"down", "enter"}, "Review talkable/talkable#1 now (no new commits since the last review, last activity 1h ago)?"},
		{[]string{"down", "ctrl+f"}, "Fresh review of talkable/talkable#1 in new agent sessions (no new commits since the last review, last activity 1h ago)?"},
		{[]string{"ctrl+r"}, "Review talkable/talkable#7 again (not reviewed yet, queued, last activity 5m ago)?"},
	} {
		m := newPicker(t, "", 200, 24)
		m, _ = send(t, m, keys(c.keys...)...)
		if m.confirm == nil || m.confirm.question != c.want {
			t.Errorf("keys %v asked %+v, want %q", c.keys, m.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(m), c.want+" y/N")
	}
}

// With the heads known the picker's question counts the commits since the
// last review.
func TestPickerQuestionUsesReviewFacts(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	entries := pickEntries()
	entries[0].State = "rereview_pending"
	entries[0].Review = &ReviewFacts{HeadSHA: "1234567aaa", ReviewedSHA: "ffa3270aaa", ReviewedAt: now.Add(-20 * time.Minute),
		ReviewedBy: "zhuravel[bot]", SinceReview: &ReviewDelta{Base: "reviewed", Commits: 3}}
	entries[1].Review = &ReviewFacts{HeadSHA: "ffa3270aaa", ReviewedSHA: "ffa3270", ReviewedAt: now.Add(-2 * time.Hour), ReviewedBy: "zhuravel"}
	m := newPickerModel(entries, PickerOptions{Now: func() time.Time { return now }})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 24})
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"ctrl+f"}, "Fresh review of talkable/talkable#7 in new agent sessions " +
			"(3 commits since the last review of ffa3270 20m ago by zhuravel[bot], head 1234567, last activity 5m ago)?"},
		{[]string{"down", "enter"}, "Review talkable/talkable#1 now (no new commits since head ffa3270 was reviewed 2h ago by zhuravel, last activity 1h ago)?"},
	} {
		got, _ := send(t, m, keys(c.keys...)...)
		if got.confirm == nil || got.confirm.question != c.want {
			t.Errorf("keys %v asked %+v, want %q", c.keys, got.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(got), c.want+" y/N")
	}
}
