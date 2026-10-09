package tui

import (
	"context"
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The terminal's background reply switches every screen to the matching
// palette; the default is dark.
func TestScreensFollowTerminalBackground(t *testing.T) {
	light := tea.BackgroundColorMsg{Color: color.White}
	if !defaultStyles.dark || newStyles(false).dark {
		t.Fatal("palette flags wrong")
	}

	d, _, _ := newDash(t, 100, 30)
	d, _ = send(t, d, light)
	p, _ := send(t, newPicker(t, "", 120, 24), light)
	c, _ := send(t, newCleanupTest(t, cleanupFixture(), 100, 30), light)
	w, _ := send(t, watchAt(t, 80, 10, nil), light)
	for name, dark := range map[string]bool{"dashboard": d.st.dark, "picker": p.st.dark, "cleanup": c.st.dark, "watch": w.st.dark} {
		if dark {
			t.Errorf("%s kept the dark palette on a light background", name)
		}
	}
	mustContain(t, viewOf(p), "talkable/talkable#7")
	mustContain(t, viewOf(d), "SLOTS (2)")

	m := testDashboard(context.Background(), &fakeSource{}, nil, DashboardOptions{})
	if _, ok := m.Init()().(tea.BatchMsg); !ok {
		t.Error("dashboard Init must batch the background query with its own commands")
	}
	if !viewOfAlt(d) || !viewOfAlt(p) || !viewOfAlt(c) || !viewOfAlt(w) {
		t.Error("every screen must ask for the alternate screen")
	}
}

func viewOfAlt(m interface{ View() tea.View }) bool { return m.View().AltScreen }

// A bracketed paste (a URL from the clipboard) lands in the picker filter
// and filters synchronously.
func TestPickerAcceptsPaste(t *testing.T) {
	m := newPicker(t, "", 120, 24)
	m, _ = send(t, m, tea.PasteMsg{Content: "https://github.com/talkable/magnum/pull/7"})
	if got := visibleRefs(m); len(got) != 1 || got[0] != "talkable/magnum#7" {
		t.Fatalf("visible after paste = %v", got)
	}
	_, out, ok := pickOutcome(t, m, "enter", "y")
	if !ok || out.Entry == nil || out.Entry.Ref != "talkable/magnum#7" {
		t.Fatalf("outcome after paste = %+v", out)
	}
}

// The typed slug confirmation accepts a paste too.
func TestCleanupTypedConfirmAcceptsPaste(t *testing.T) {
	plan := CleanupPlan{Actions: []CleanupAction{{ID: "d9", Kind: "drop_db", Subject: "slug:review9", NeedsTypedConfirm: "review9"}}}
	m := newCleanupTest(t, plan, 100, 30)
	m, _ = send(t, m, keyMsg("enter"), tea.PasteMsg{Content: "review9"})
	m, cmd := send(t, m, keyMsg("enter"))
	if !m.outcome.Apply || !isQuit(execCmd(t, cmd)) {
		t.Fatalf("pasted slug not accepted: %+v", m.outcome)
	}
}
