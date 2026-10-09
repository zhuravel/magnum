package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func cleanupFixture() CleanupPlan {
	return CleanupPlan{
		Actions: []CleanupAction{
			{ID: "slot-3", Kind: "release_slot", Subject: "slot:review3", Why: "PR closed 3d ago", Bytes: 1 << 30, DBBytes: 812 << 20, DBNames: []string{"rev3_a", "rev3_b"}},
			{ID: "slot-4", Kind: "remove_slot", Subject: "acme/shop#42", Why: "merged", Bytes: 512 << 20},
		},
		Skipped: []CleanupSkip{{Subject: "slot:review7", Reason: "in use by a running session"}},
		Totals:  CleanupTotals{Disk: 3 << 30, MySQL: 2 << 30},
	}
}

// typedFixture adds two slug drops that need typed confirmation after a
// plain action, in plan order: plain, review9, plain, review8, review9.
func typedFixture() CleanupPlan {
	return CleanupPlan{Actions: []CleanupAction{
		{ID: "p1", Kind: "release_slot", Subject: "slot:review1", Why: "closed", Bytes: 1 << 20},
		{ID: "d9", Kind: "drop_db", Subject: "slug:review9", Why: "orphan", DBBytes: 10 << 20, DBNames: []string{"rev9_a"}, NeedsTypedConfirm: "review9"},
		{ID: "p2", Kind: "remove_slot", Subject: "slot:review2", Why: "closed", Bytes: 2 << 20},
		{ID: "d8", Kind: "drop_db", Subject: "slug:review8", Why: "orphan", DBBytes: 5 << 20, DBNames: []string{"rev8_a"}, NeedsTypedConfirm: "review8"},
		{ID: "d9b", Kind: "drop_db", Subject: "slug:review9", Why: "orphan", DBBytes: 5 << 20, DBNames: []string{"rev9_b"}, NeedsTypedConfirm: "review9"},
	}}
}

func newCleanupTest(t *testing.T, plan CleanupPlan, width, height int) cleanupModel {
	t.Helper()
	m, _ := send(t, newCleanupModel(plan), tea.WindowSizeMsg{Width: width, Height: height})
	return m
}

// cursorLine is the view line that starts with the cursor mark.
func cursorLine(view string) string {
	for l := range strings.SplitSeq(view, "\n") {
		if strings.HasPrefix(l, cursorMark) {
			return l
		}
	}
	return ""
}

func TestCleanupSelectRendersRows(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	v := viewOf(m)
	mustContain(t, v, "Cleanup plan", "KIND", "SUBJECT", "DISK", "MYSQL", "WHY",
		"[x]", "release_slot", "slot:review3", "1.0G", "812M (2 dbs)", "PR closed 3d ago",
		"remove_slot", "acme/shop#42", "512M",
		"selected 2/2 · disk 1.5G · mysql 812M · 2 databases",
		"whole plan: disk 3.0G · mysql 2.0G",
		"space toggle", "enter review")
	mustNotContain(t, v, "[ ]")
	if got := cursorLine(v); !strings.Contains(got, "[x] release_slot") {
		t.Errorf("cursor line = %q, want the first action:\n%s", got, v)
	}
	if !strings.Contains(v, noMark+"[x] remove_slot") {
		t.Errorf("other rows lack the blank mark:\n%s", v)
	}
}

func TestCleanupSpaceTogglesAndUpdatesTotals(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	m, _ = send(t, m, keys(" ")...) // deselect the first action
	v := viewOf(m)
	mustContain(t, v, "[ ]", "[x]", "selected 1/2 · disk 512M · mysql 0 · 0 databases")
	m, _ = send(t, m, keys(" ")...)
	mustContain(t, viewOf(m), "selected 2/2")

	// Moving down then toggling affects the second row instead.
	m, _ = send(t, m, keys("j", " ")...)
	mustContain(t, viewOf(m), "selected 1/2 · disk 1.0G · mysql 812M · 2 databases")
}

func TestCleanupAllAndNone(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	m, _ = send(t, m, keys("n")...)
	mustContain(t, viewOf(m), "selected 0/2")
	mustNotContain(t, viewOf(m), "[x]")
	m, _ = send(t, m, keys("a")...)
	mustContain(t, viewOf(m), "selected 2/2")
	mustNotContain(t, viewOf(m), "[ ]")
}

func TestCleanupEnterWithNothingSelectedStays(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	m, _ = send(t, m, keys("n")...)
	m, cmd := send(t, m, keys("enter")...)
	if isQuit(execCmd(t, cmd)) {
		t.Fatal("enter with nothing selected must not quit")
	}
	mustContain(t, viewOf(m), "select at least one action", "Cleanup plan")
	mustNotContain(t, viewOf(m), "Apply 0 action")

	// The note goes away with the next key.
	m, _ = send(t, m, keys("a")...)
	mustNotContain(t, viewOf(m), "select at least one action")
}

func TestCleanupSkippedSection(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	mustContain(t, viewOf(m), "SKIPPED (1)", "slot:review7: in use by a running session")

	plan := cleanupFixture()
	plan.Skipped = nil
	mustNotContain(t, viewOf(newCleanupTest(t, plan, 100, 30)), "SKIPPED")
}

func TestCleanupSkippedListIsCapped(t *testing.T) {
	plan := cleanupFixture()
	plan.Skipped = nil
	for i := range 9 {
		plan.Skipped = append(plan.Skipped, CleanupSkip{Subject: fmt.Sprintf("slot:s%d", i), Reason: "busy"})
	}
	v := viewOf(newCleanupTest(t, plan, 100, 30))
	mustContain(t, v, "SKIPPED (9)", "slot:s0: busy", "… 4 more")
	mustNotContain(t, v, "slot:s8: busy")
}

func TestCleanupConfirmListsSelectedActions(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	m, _ = send(t, m, keys("enter")...)
	v := viewOf(m)
	mustContain(t, v, "Apply 2 action(s)?",
		"release_slot slot:review3 — frees 1.0G, drops 2 databases (812M): rev3_a, rev3_b",
		"remove_slot acme/shop#42 — frees 512M",
		"y apply", "n/esc back")
	mustNotContain(t, v, "Type ", "[ ]")

	// Deselected actions are not listed.
	m, _ = send(t, m, keys("esc", " ", "enter")...)
	v = viewOf(m)
	mustContain(t, v, "Apply 1 action(s)?", "remove_slot acme/shop#42")
	mustNotContain(t, v, "release_slot")
}

func TestCleanupYAppliesInPlanOrder(t *testing.T) {
	plan := cleanupFixture()
	plan.Actions = append(plan.Actions, CleanupAction{ID: "slot-5", Kind: "release_slot", Subject: "slot:review5", Why: "closed"})
	m := newCleanupTest(t, plan, 100, 30)
	m, _ = send(t, m, keys("j", " ")...) // deselect the middle one
	m, _ = send(t, m, keys("enter")...)
	m, cmd := send(t, m, keys("y")...)
	if !isQuit(execCmd(t, cmd)) {
		t.Fatal("y did not quit")
	}
	want := CleanupOutcome{Apply: true, SelectedIDs: []string{"slot-3", "slot-5"}}
	if m.outcome.Apply != want.Apply || !slices.Equal(m.outcome.SelectedIDs, want.SelectedIDs) {
		t.Fatalf("outcome = %+v, want %+v", m.outcome, want)
	}
}

// TestCleanupDoubleEnterDoesNotApply: every action starts selected, so a
// double enter (or key autorepeat) must stop at the confirm step; only y
// applies.
func TestCleanupDoubleEnterDoesNotApply(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	m, _ = send(t, m, keys("enter")...)
	m, cmd := send(t, m, keys("enter", "enter")...)
	if isQuit(execCmd(t, cmd)) || m.outcome.Apply {
		t.Fatalf("enter applied the plan: outcome = %+v", m.outcome)
	}
	v := viewOf(m)
	mustContain(t, v, "Apply 2 action(s)?", "y apply", "press y to apply")
	mustNotContain(t, v, "y/enter")

	m, cmd = send(t, m, keys("y")...)
	if !isQuit(execCmd(t, cmd)) || !m.outcome.Apply || !slices.Equal(m.outcome.SelectedIDs, []string{"slot-3", "slot-4"}) {
		t.Fatalf("outcome = %+v", m.outcome)
	}
}

// TestCleanupRendersAfterTheLastTypedText: Bubble Tea renders the model
// once more before it handles tea.Quit, so the view after the last typed
// confirmation must not index past the texts.
func TestCleanupRendersAfterTheLastTypedText(t *testing.T) {
	for _, plan := range []CleanupPlan{{Actions: typedFixture().Actions[1:2]}, typedFixture()} {
		m := newCleanupTest(t, plan, 100, 30)
		m, _ = send(t, m, keys("enter")...)
		var cmd tea.Cmd
		for _, text := range m.texts {
			m, cmd = send(t, m, append(typed(text), keys("enter")...)...)
		}
		if !isQuit(execCmd(t, cmd)) || !m.outcome.Apply {
			t.Fatalf("outcome = %+v", m.outcome)
		}
		if v := viewOf(m); strings.Contains(v, "Type ") {
			t.Errorf("view after the last text still asks to type:\n%s", v)
		}
	}
}

func TestCleanupConfirmBackKeepsSelection(t *testing.T) {
	for _, back := range []string{"esc", "n"} {
		t.Run(back, func(t *testing.T) {
			m := newCleanupTest(t, cleanupFixture(), 100, 30)
			m, _ = send(t, m, keys(" ", "enter")...) // deselect the first, review
			m, cmd := send(t, m, keys(back)...)
			if isQuit(execCmd(t, cmd)) {
				t.Fatalf("%s must not quit", back)
			}
			v := viewOf(m)
			mustContain(t, v, "Cleanup plan", "selected 1/2", "[ ]", "[x]")
			mustNotContain(t, v, "Apply ")
			if m.outcome.Apply {
				t.Fatal("going back must not apply")
			}
		})
	}
}

func TestCleanupCtrlCCancelsFromEveryStep(t *testing.T) {
	m := newCleanupTest(t, cleanupFixture(), 100, 30)
	_, cmd := send(t, m, keys("ctrl+c")...)
	if !isQuit(execCmd(t, cmd)) {
		t.Error("ctrl+c in select did not quit")
	}
	m, _ = send(t, m, keys("enter")...)
	m2, cmd := send(t, m, keys("ctrl+c")...)
	if !isQuit(execCmd(t, cmd)) || m2.outcome.Apply || m2.outcome.SelectedIDs != nil {
		t.Errorf("ctrl+c in confirm: outcome %+v", m2.outcome)
	}

	m = newCleanupTest(t, typedFixture(), 100, 30)
	m, _ = send(t, m, keys("enter")...)
	m, cmd = send(t, m, keys("ctrl+c")...)
	if !isQuit(execCmd(t, cmd)) || m.outcome.Apply {
		t.Errorf("ctrl+c in typed confirm: outcome %+v", m.outcome)
	}
}

// TestCleanupKeysQueuedBehindACancelDoNothing: Bubble Tea hands the model
// the keys already read before it handles tea.Quit, so a y (or an enter
// and a y) typed right after ctrl+c, esc or q must not apply the plan the
// user cancelled.
func TestCleanupKeysQueuedBehindACancelDoNothing(t *testing.T) {
	for name, script := range map[string][]string{
		"ctrl+c in confirm": {"enter", "ctrl+c", "y"},
		"ctrl+c in select":  {"ctrl+c", "enter", "y"},
		"esc in select":     {"esc", "enter", "y"},
		"q in select":       {"q", "enter", "y"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newCleanupTest(t, cleanupFixture(), 100, 30)
			m, cmd := send(t, m, keys(script...)...)
			if m.outcome.Apply || m.outcome.SelectedIDs != nil {
				t.Fatalf("outcome = %+v, want zero", m.outcome)
			}
			if cmd != nil {
				t.Errorf("a key after the cancel returned a command: %v", execCmd(t, cmd))
			}
			if v := viewOf(m); v != "" {
				t.Errorf("view after the cancel:\n%s", v)
			}
		})
	}
}

// TestCleanupEnterQueuedBehindTheLastTypedTextDoesNotPanic: an enter (or a
// y) read after the enter that confirmed the last typed text reaches the
// model after it finished; it must neither index past the texts nor change
// the confirmed outcome.
func TestCleanupEnterQueuedBehindTheLastTypedTextDoesNotPanic(t *testing.T) {
	m := newCleanupTest(t, CleanupPlan{Actions: typedFixture().Actions[1:2]}, 100, 30)
	m, _ = send(t, m, keys("enter")...)
	m, cmd := send(t, m, append(typed("review9"), keys("enter", "enter", "y", "esc")...)...)
	if cmd != nil {
		t.Errorf("a key after the confirmation returned a command: %v", execCmd(t, cmd))
	}
	if !m.outcome.Apply || !slices.Equal(m.outcome.SelectedIDs, []string{"d9"}) {
		t.Fatalf("outcome = %+v, want the confirmed d9", m.outcome)
	}
}

func TestCleanupCancelInSelectStep(t *testing.T) {
	for _, k := range []string{"esc", "q"} {
		t.Run(k, func(t *testing.T) {
			m := newCleanupTest(t, cleanupFixture(), 100, 30)
			m, cmd := send(t, m, keys(k)...)
			if !isQuit(execCmd(t, cmd)) {
				t.Fatalf("%s did not quit", k)
			}
			if m.outcome.Apply || m.outcome.SelectedIDs != nil {
				t.Fatalf("outcome = %+v, want zero", m.outcome)
			}
		})
	}
}

func TestCleanupTypedConfirmMismatchThenMatch(t *testing.T) {
	plan := CleanupPlan{Actions: typedFixture().Actions[:2]} // p1 + d9
	m := newCleanupTest(t, plan, 100, 30)
	m, cmd := send(t, m, keys("enter")...)
	if cmd == nil {
		t.Error("entering the typed step should return the input's blink command")
	}
	mustContain(t, viewOf(m), "Apply 2 action(s)?", "(type review9)",
		"Type review9 to confirm dropping its databases (cannot be undone):", "> ")
	mustNotContain(t, viewOf(m), "y apply")

	// y and n are typed, not intercepted; the wrong text is rejected.
	m, cmd = send(t, m, append(typed("ny"), keys("enter")...)...)
	if isQuit(execCmd(t, cmd)) || m.outcome.Apply {
		t.Fatal("a mismatch must not apply")
	}
	mustContain(t, viewOf(m), "does not match review9", "Type review9")

	// Input was cleared; the exact text applies.
	m, cmd = send(t, m, append(typed("review9"), keys("enter")...)...)
	if !isQuit(execCmd(t, cmd)) {
		t.Fatal("exact text did not quit")
	}
	if !m.outcome.Apply || !slices.Equal(m.outcome.SelectedIDs, []string{"p1", "d9"}) {
		t.Fatalf("outcome = %+v", m.outcome)
	}
}

func TestCleanupTypedConfirmShowsTypingAndClearsError(t *testing.T) {
	plan := CleanupPlan{Actions: typedFixture().Actions[1:2]}
	m := newCleanupTest(t, plan, 100, 30)
	m, _ = send(t, m, keys("enter")...)
	m, _ = send(t, m, typed("yes")...)
	mustContain(t, viewOf(m), "> yes")
	m, _ = send(t, m, keys("enter")...)
	mustContain(t, viewOf(m), "does not match review9")
	m, _ = send(t, m, typed("r")...)
	mustNotContain(t, viewOf(m), "does not match")
}

func TestCleanupTwoTypedTextsAskedInSequence(t *testing.T) {
	m := newCleanupTest(t, typedFixture(), 100, 30)
	m, _ = send(t, m, keys("enter")...)
	mustContain(t, viewOf(m), "Type review9 to confirm", "(1 of 2)")

	// The second text cannot be typed first.
	m, _ = send(t, m, append(typed("review8"), keys("enter")...)...)
	mustContain(t, viewOf(m), "does not match review9", "(1 of 2)")

	m, cmd := send(t, m, append(typed("review9"), keys("enter")...)...)
	if isQuit(execCmd(t, cmd)) || m.outcome.Apply {
		t.Fatal("the second text is still outstanding")
	}
	mustContain(t, viewOf(m), "Type review8 to confirm", "(2 of 2)")
	mustNotContain(t, viewOf(m), "does not match")

	m, cmd = send(t, m, append(typed("review8"), keys("enter")...)...)
	if !isQuit(execCmd(t, cmd)) {
		t.Fatal("did not quit after the last text")
	}
	want := []string{"p1", "d9", "p2", "d8", "d9b"}
	if !m.outcome.Apply || !slices.Equal(m.outcome.SelectedIDs, want) {
		t.Fatalf("outcome = %+v, want ids %v", m.outcome, want)
	}
}

func TestCleanupDeselectingTypedActionSkipsTyping(t *testing.T) {
	m := newCleanupTest(t, typedFixture(), 100, 30)
	// Deselect both review9 actions (rows 1 and 4) and review8 (row 3): only
	// plain actions remain.
	m, _ = send(t, m, keys("j", " ", "j", "j", " ", "j", " ")...)
	m, _ = send(t, m, keys("enter")...)
	mustContain(t, viewOf(m), "Apply 2 action(s)?", "y apply")
	mustNotContain(t, viewOf(m), "Type ")
	m, cmd := send(t, m, keys("y")...)
	if !isQuit(execCmd(t, cmd)) || !slices.Equal(m.outcome.SelectedIDs, []string{"p1", "p2"}) {
		t.Fatalf("outcome = %+v", m.outcome)
	}

	// Keeping only review8 asks for review8 alone.
	m = newCleanupTest(t, typedFixture(), 100, 30)
	m, _ = send(t, m, keys("n", "j", "j", "j", " ", "enter")...)
	mustContain(t, viewOf(m), "Apply 1 action(s)?", "Type review8 to confirm")
	mustNotContain(t, viewOf(m), "(1 of")
}

func TestCleanupEscInTypedStepForgetsProgress(t *testing.T) {
	m := newCleanupTest(t, typedFixture(), 100, 30)
	m, _ = send(t, m, keys("enter")...)
	m, _ = send(t, m, append(typed("review9"), keys("enter")...)...)
	mustContain(t, viewOf(m), "(2 of 2)")
	m, cmd := send(t, m, keys("esc")...)
	if isQuit(execCmd(t, cmd)) {
		t.Fatal("esc must go back, not quit")
	}
	mustContain(t, viewOf(m), "Cleanup plan", "selected 5/5")
	m, _ = send(t, m, keys("enter")...)
	mustContain(t, viewOf(m), "Type review9", "(1 of 2)")
	mustNotContain(t, viewOf(m), "> review")
}

func TestCleanupTypedStepForwardsNonKeyMessages(t *testing.T) {
	m := newCleanupTest(t, typedFixture(), 100, 30)
	m, _ = send(t, m, keys("enter")...)
	// A cursor blink message must reach the input without disturbing state.
	m, _ = send(t, m, struct{ other bool }{true})
	mustContain(t, viewOf(m), "Type review9")
}

func TestCleanupViewFitsNarrowAndWide(t *testing.T) {
	plan := typedFixture()
	plan.Actions[0].Why = "closed a long while ago and nobody has touched the slot since then"
	plan.Actions[1].DBNames = []string{"review9_development", "review9_test", "review9_extra_database_name"}
	plan.Skipped = []CleanupSkip{{Subject: "slot:review7", Reason: "in use by a running session that was started yesterday"}}
	for _, width := range []int{60, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newCleanupTest(t, plan, width, 30)
			check := func(step string) {
				t.Helper()
				if got := maxLineWidth(viewOf(m)); got > width {
					t.Errorf("%s: widest line is %d cells, width %d:\n%s", step, got, width, viewOf(m))
				}
			}
			check("select")
			m, _ = send(t, m, keys("enter")...)
			check("typed")
			m, _ = send(t, m, append(typed("nope"), keys("enter")...)...)
			check("mismatch")
			m, _ = send(t, m, keys("esc", "j", " ", "j", " ", "j", " ", "j", " ", "enter")...)
			check("plain confirm")
		})
	}
}

func TestCleanupDefaultSizeBeforeResize(t *testing.T) {
	m := newCleanupModel(cleanupFixture())
	v := viewOf(m)
	mustContain(t, v, "Cleanup plan", "selected 2/2")
	if got := maxLineWidth(v); got > 80 {
		t.Errorf("default width exceeded: %d", got)
	}
}

func TestCleanupConfirmColumnOnlyWhenNeeded(t *testing.T) {
	mustNotContain(t, viewOf(newCleanupTest(t, cleanupFixture(), 100, 30)), "CONFIRM")
	v := viewOf(newCleanupTest(t, typedFixture(), 120, 30))
	mustContain(t, v, "CONFIRM", "[confirm: review9]", "[confirm: review8]")
}

func TestCleanupConfirmMarkerShortensWhenNarrow(t *testing.T) {
	v := viewOf(newCleanupTest(t, typedFixture(), 60, 30))
	mustContain(t, v, "CONFIRM", "[confirm]")
	mustNotContain(t, v, "[confirm: ")
	if got := maxLineWidth(v); got > 60 {
		t.Errorf("widest line %d > 60:\n%s", got, v)
	}
}

func TestCleanupScrollsToKeepCursorVisible(t *testing.T) {
	var plan CleanupPlan
	for i := 1; i <= 20; i++ {
		plan.Actions = append(plan.Actions, CleanupAction{ID: fmt.Sprint(i), Kind: "release_slot", Subject: fmt.Sprintf("slot:s%02d", i)})
	}
	m := newCleanupTest(t, plan, 100, 12) // room for 5 rows
	v := viewOf(m)
	mustContain(t, v, "slot:s01", "slot:s05", "rows 1-5 of 20", "selected 20/20")
	mustNotContain(t, v, "slot:s06")

	m, _ = send(t, m, keys("j", "j", "j", "j", "j", "j", "j")...) // cursor on row 8
	v = viewOf(m)
	mustContain(t, v, "slot:s08", "rows 4-8 of 20")
	mustNotContain(t, v, "slot:s01", "slot:s09")
	if got := cursorLine(v); !strings.Contains(got, "slot:s08") {
		t.Errorf("cursor line = %q, want slot:s08:\n%s", got, v)
	}
	if n := len(strings.Split(v, "\n")); n > 12 {
		t.Errorf("view is %d lines, height 12", n)
	}

	m, _ = send(t, m, keys("G")...)
	mustContain(t, viewOf(m), "slot:s20", "rows 16-20 of 20")
	m, _ = send(t, m, keys("g")...)
	mustContain(t, viewOf(m), "slot:s01", "rows 1-5 of 20")
	m, _ = send(t, m, keys("k")...) // clamped at the top
	if got := cursorLine(viewOf(m)); !strings.Contains(got, "slot:s01") {
		t.Errorf("cursor line = %q, want slot:s01", got)
	}
}

func TestCleanupConfirmListFitsHeight(t *testing.T) {
	var plan CleanupPlan
	for i := 1; i <= 20; i++ {
		plan.Actions = append(plan.Actions, CleanupAction{ID: fmt.Sprint(i), Kind: "release_slot", Subject: fmt.Sprintf("slot:s%02d", i), Bytes: 1 << 20})
	}
	m := newCleanupTest(t, plan, 100, 12)
	m, _ = send(t, m, keys("enter")...)
	v := viewOf(m)
	mustContain(t, v, "Apply 20 action(s)?", "slot:s01", "… and ")
	if n := len(strings.Split(v, "\n")); n > 12 {
		t.Errorf("confirm view is %d lines, height 12:\n%s", n, v)
	}
}

func TestCleanupEmptyPlan(t *testing.T) {
	m := newCleanupTest(t, CleanupPlan{}, 100, 30)
	mustContain(t, viewOf(m), "no actions in this plan", "selected 0/0")
	m, _ = send(t, m, keys("j", " ", "a", "enter")...)
	mustContain(t, viewOf(m), "select at least one action")
	_, cmd := send(t, m, keys("q")...)
	if !isQuit(execCmd(t, cmd)) {
		t.Error("q did not quit")
	}
}

func TestCleanupSelectionIsNotSharedAcrossModels(t *testing.T) {
	before := newCleanupTest(t, cleanupFixture(), 100, 30)
	after, _ := send(t, before, keys(" ")...)
	mustContain(t, viewOf(before), "selected 2/2")
	mustContain(t, viewOf(after), "selected 1/2")
}

// TestCleanupRunPlanScriptedInput drives the real program: enter to review,
// y to apply.
func TestCleanupRunPlanScriptedInput(t *testing.T) {
	w := scriptedInput(t)

	go func() {
		for _, k := range []string{"\r", "y"} {
			if _, err := w.Write([]byte(k)); err != nil { // returns once the program read it
				return
			}
		}
	}()

	type result struct {
		out CleanupOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := RunCleanupPlan(context.Background(), cleanupFixture())
		done <- result{out, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal(res.err)
		}
		if !res.out.Apply || !slices.Equal(res.out.SelectedIDs, []string{"slot-3", "slot-4"}) {
			t.Fatalf("outcome = %+v", res.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCleanupPlan did not return")
	}
}

func TestCleanupRunPlanContextEndedMeansNoApply(t *testing.T) {
	w := scriptedInput(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// j only moves the cursor; the write returns once the running
		// program read it, and the context ends then.
		if _, err := w.Write([]byte("j")); err == nil {
			cancel()
		}
	}()
	done := make(chan CleanupOutcome, 1)
	go func() {
		out, err := RunCleanupPlan(ctx, cleanupFixture())
		if err != nil {
			t.Errorf("err = %v", err)
		}
		done <- out
	}()
	select {
	case out := <-done:
		if out.Apply || out.SelectedIDs != nil {
			t.Fatalf("outcome = %+v, want zero", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCleanupPlan did not return after ctx ended")
	}
}

// TestCleanupConfirmKeepsTheInputOnShortScreens: the typed confirmation's
// prompt and input are at the bottom; a short terminal must cut the list
// above them, not them.
func TestCleanupConfirmKeepsTheInputOnShortScreens(t *testing.T) {
	for _, h := range []int{4, 6, 8} {
		m := newCleanupTest(t, typedFixture(), 60, h)
		m, _ = send(t, m, keys("enter")...)
		m, _ = send(t, m, typed("rev")...)
		v := viewOf(m)
		if n := len(strings.Split(v, "\n")); n > h {
			t.Errorf("height %d: %d lines:\n%s", h, n, v)
		}
		mustContain(t, v, "> rev", "enter confirm")
	}
}
