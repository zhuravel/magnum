package tui

import (
	"context"
	"strings"
	"testing"
)

// Every row action runs its own call under its own name: its what starts
// with the action's, and its call is the DashboardActions method of that
// action (U unmutes; it once ran only through the catch-all). An action
// actionRun has no case for fails instead of running another one's call.
func TestEveryRowActionRunsItsOwnCall(t *testing.T) {
	want := map[rowAct]string{
		actReview:         "review talkable#7 fresh=false simplify=false",
		actFresh:          "review talkable#7 fresh=true simplify=false",
		actSimplify:       "review talkable#7 fresh=false simplify=true",
		actAbort:          "abort talkable#7",
		actIgnore:         "ignore talkable#7",
		actApprove:        "approve talkable#7",
		actRequestChanges: "request-changes talkable#7",
		actOpen:           "open talkable#7",
		actBrowser:        "browser https://github.com/talkable/talkable/pull/7",
		actTracker:        "browser https://tracker.example.com/PS-1",
		actPin:            "pin talkable#7",
		actUnpin:          "unpin talkable#7",
		actRelease:        "release talkable#7",
		actMute:           "mute talkable#7",
		actUnmute:         "unmute talkable#7",
		actUnapprove:      "unapprove talkable#7",
		actSnooze:         "snooze talkable#7 2h0m0s",
	}
	if len(want) != len(rowActDefs) {
		t.Fatalf("the test knows %d row actions, rowActDefs %d", len(want), len(rowActDefs))
	}
	r := actRow{ok: true, ref: "talkable#7", label: "talkable#7", ghState: "OPEN", now: boardNow,
		url: "https://github.com/talkable/talkable/pull/7", issue: "PS-1", issueURL: "https://tracker.example.com/PS-1"}
	for a := range rowAct(len(rowActDefs)) {
		what, fn := actionRun(a, r)
		if !strings.HasPrefix(what, rowActDefs[a].what) {
			t.Errorf("%s runs as %q", rowActDefs[a].what, what)
		}
		act := &fakeActions{}
		if _, err := fn(context.Background(), act); err != nil {
			t.Errorf("%s: %v", rowActDefs[a].what, err)
		}
		if got := act.last(); got != want[a] {
			t.Errorf("%s calls %q, want %q", rowActDefs[a].what, got, want[a])
		}
	}
	unknown := rowAct(len(rowActDefs))
	_, fn := actionRun(unknown, r)
	act := &fakeActions{}
	if _, err := fn(context.Background(), act); err == nil || len(act.calls) != 0 {
		t.Errorf("an unknown row action: err %v, calls %v; want an error and no call", err, act.calls)
	}
}

// Every picker action is its own row action, refused and asked about as
// that action on the board; cancel and an action the picker does not know
// are none (the picker once fell back to release).
func TestEveryPickerActionIsItsOwnRowAction(t *testing.T) {
	pinned := &PickEntry{Ref: "talkable#7", Pinned: true}
	for _, c := range []struct {
		a    PickAction
		e    *PickEntry
		want rowAct
	}{
		{PickActionReview, nil, actReview},
		{PickActionFresh, nil, actFresh},
		{PickActionOpen, nil, actOpen},
		{PickActionBrowser, nil, actBrowser},
		{PickActionTogglePin, nil, actPin},
		{PickActionTogglePin, pinned, actUnpin},
		{PickActionRelease, nil, actRelease},
	} {
		if got, ok := pickRowAct(c.a, c.e); !ok || got != c.want {
			t.Errorf("%s: %s (%v), want %s", c.a, rowActDefs[got].what, ok, rowActDefs[c.want].what)
		}
	}
	for _, a := range []PickAction{PickActionCancel, PickActionRelease + 1} {
		if got, ok := pickRowAct(a, nil); ok {
			t.Errorf("%s is the row action %s, want none", a, rowActDefs[got].what)
		}
	}
	m := newPicker(t, "", 120, 24)
	next, cmd := m.choose(PickActionRelease + 1)
	if pm := next.(pickerModel); cmd != nil || pm.done || pm.note == "" {
		t.Errorf("an unknown picker action: cmd %v, done %v, note %q; want a note and nothing done", cmd, pm.done, pm.note)
	}
}
