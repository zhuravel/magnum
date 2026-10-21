package tui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSettingsOpensOnSpendAndMovesThroughRowsAndSections(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	if got := m.currentSection(); got != "Spend & models" {
		t.Fatalf("opens on %q, want Spend & models", got)
	}
	m = press(t, m, "j", "j")
	if r, _ := rowAt(m.rows(), m.cursor); r.kind != rowField || m.data.Fields[r.field].Label != "claude spend" {
		t.Errorf("two j should select claude spend, got %+v", r)
	}
	m = press(t, m, "j") // past the list into the roles
	if r, _ := rowAt(m.rows(), m.cursor); r.kind != rowRole || r.role != "codex-judge" {
		t.Errorf("j past the settings should select the first role, got %+v", r)
	}
	m = press(t, m, "G")
	if r, _ := rowAt(m.rows(), m.cursor); r.role != "claude-whiteboard" {
		t.Errorf("G should select the last role, got %+v", r)
	}
	m = press(t, m, "tab")
	if got := m.currentSection(); got != "Usage caps" {
		t.Errorf("tab should go to Usage caps, got %q", got)
	}
	if m.cursor != 0 {
		t.Errorf("a new section starts on its first row, cursor %d", m.cursor)
	}
	m = press(t, m, "shift+tab", "shift+tab")
	if got := m.currentSection(); got != sectionRuntime {
		t.Errorf("shift+tab twice should reach Runtime, got %q", got)
	}
	m = press(t, m, "j")
	if r, _ := rowAt(m.rows(), m.cursor); r.kind != rowRuntime || r.what != rtKind || r.entry.Name != "codex" {
		t.Errorf("j from automation should skip the agent kinds heading to codex, got %+v", r)
	}
}

func TestSettingsLAndHMoveBetweenTheListAndTheRolesGrid(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, m, "l")
	if r, _ := rowAt(m.rows(), m.cursor); r.kind != rowRole {
		t.Fatalf("l should move into the roles grid, got %+v", r)
	}
	if !strings.Contains(viewOf(m), "› claude-review") && !strings.Contains(viewOf(m), "› codex-judge") {
		t.Errorf("the grid should mark the selected role:\n%s", viewOf(m))
	}
	m = press(t, m, "h")
	if r, _ := rowAt(m.rows(), m.cursor); r.kind != rowField {
		t.Errorf("h should go back to the list, got %+v", r)
	}
}

func TestSettingsStagingMarksTheRowTheSectionAndTheTitle(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = toSection(t, m, "Usage caps")
	m = selectRow(t, m, "codex soft cap")
	m = press(t, m, "enter")
	if m.popup == nil {
		t.Fatal("enter should open the field's popup")
	}
	m = press(t, m, "ctrl+u") // clear the input
	m = pump(t, m, append(keys("backspace", "backspace"), append(typed("80"), keyMsg("enter"))...)...).m
	if m.popup != nil {
		t.Fatalf("enter should stage and close the popup:\n%s", viewOf(m))
	}
	if e, ok := m.draft["usage.codex_soft"]; !ok || e.Text != "80" {
		t.Fatalf("draft = %+v, want codex_soft 80", m.draft)
	}
	v := viewOf(m)
	mustContain(t, v, "1 unsaved", "● unsaved", "was 85")
	if !strings.Contains(v, "Usage caps ●") {
		t.Errorf("the rail should mark Usage caps dirty:\n%s", v)
	}
	if strings.Contains(v, "Schedule ●") {
		t.Errorf("only the section with the edit is marked:\n%s", v)
	}
}

func TestSettingsUndoTakesBackTheLastStagedEdit(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "70"})
	m = m.stage("daemon.park_idle_after", SettingsEdit{Text: "30m"})
	m = press(t, m, "u")
	if _, ok := m.draft["daemon.park_idle_after"]; ok {
		t.Error("u should take back park_idle_after")
	}
	if m.draft["usage.codex_soft"].Text != "70" {
		t.Errorf("the earlier edit stays: %+v", m.draft)
	}
	m = press(t, m, "u")
	if m.draft["usage.codex_soft"].Text != "80" {
		t.Errorf("u again restores the edit before: %+v", m.draft)
	}
	m = press(t, m, "u", "u")
	if len(m.draft) != 0 {
		t.Errorf("undoing every edit leaves no draft: %+v", m.draft)
	}
	mustContain(t, viewOf(m), "nothing to undo")
}

func TestSettingsStagingTheSavedValueAgainLeavesNoEdit(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "85"})
	if len(m.draft) != 0 {
		t.Errorf("85 is the saved value: the draft should be empty, got %+v", m.draft)
	}
}

func TestSettingsDPutsTheDefaultBack(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = toSection(t, m, "Usage caps")
	m = selectRow(t, m, "codex soft cap")
	m = press(t, m, "d")
	if e := m.draft["usage.codex_soft"]; !e.Default {
		t.Fatalf("d should stage the default for a key the file sets, got %+v", m.draft)
	}
	mustContain(t, viewOf(m), "● unsaved")
	m = selectRow(t, m, "codex hard cap")
	m = press(t, m, "d")
	if _, ok := m.draft["usage.codex_hard"]; ok {
		t.Error("d on a key the file does not set stages nothing")
	}
	mustContain(t, viewOf(m), "already the default")
}

func TestSettingsSearchFindsLabelsAndKeysAcrossSections(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = pump(t, m, append(keys("/"), append(typed("park"), keyMsg("enter"))...)...).m
	rows := m.rows()
	if len(rows) != 1 || m.data.Fields[rows[0].field].Target != "daemon.park_idle_after" {
		t.Fatalf("search park should find park_idle_after only, got %d rows", len(rows))
	}
	mustContain(t, viewOf(m), "SEARCH", "1 match", "park idle after")
	m = press(t, m, "esc")
	if m.query != "" || m.currentSection() != "Spend & models" {
		t.Errorf("esc clears the search and keeps the section: %q %q", m.query, m.currentSection())
	}
	m = pump(t, m, append(keys("/"), append(typed("auto_approve_authors"), keyMsg("enter"))...)...).m
	if n := len(m.rows()); n != 2 {
		t.Errorf("a key matches in both watches: %d rows", n)
	}
	mustContain(t, viewOf(m), "watch 1 · auto-approve authors", "watch 2 · auto-approve authors")
}

func TestSettingsSearchWithAStarKeepsWhatTheFileSetsOrTheDraftChanges(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("daemon.park_idle_after", SettingsEdit{Text: "30m"})
	m = pump(t, m, append(keys("/"), append(typed("*"), keyMsg("enter"))...)...).m
	for _, r := range m.rows() {
		f := m.data.Fields[r.field]
		if _, staged := m.draft[f.Target]; !f.Set && !staged {
			t.Errorf("* lists %s, which neither the file nor the draft sets", f.Target)
		}
	}
	got := false
	for _, r := range m.rows() {
		got = got || m.data.Fields[r.field].Target == "daemon.park_idle_after"
	}
	if !got {
		t.Error("* should list the staged park_idle_after")
	}
}

func TestSettingsScopePickerOpensAWatchAndBracketsMoveBetweenWatches(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = press(t, m, "c")
	if m.popup == nil || m.popup.kind != popScope {
		t.Fatal("c should open the scope picker")
	}
	mustContain(t, viewOf(m), "Global", "watch 1: talkable · talkable/talkable, talkable/example +1", "watch 2: example",
		"repo talkable/example (no [[repo]] block yet)")
	m = press(t, m, "j", "j", "enter")
	if m.currentSection() != "Watches" || m.scopeLabel() != "watch 2: example" {
		t.Fatalf("picking watch 2 should open Watches on it, got %q %q", m.currentSection(), m.scopeLabel())
	}
	mustContain(t, viewOf(m), "magnum · settings · watch 2: example", "WATCH 2")
	m = press(t, m, "[")
	if m.scopeLabel() != "watch 1: talkable" {
		t.Errorf("[ should go to watch 1, got %q", m.scopeLabel())
	}
	m = press(t, m, "]", "]")
	if m.scopeLabel() != "watch 1: talkable" {
		t.Errorf("] twice from watch 1 wraps back to it, got %q", m.scopeLabel())
	}
	m = press(t, m, "c", "g", "enter") // Global, first in the picker
	if m.scopeLabel() != "Global" {
		t.Errorf("Global should leave the watch, got %q", m.scopeLabel())
	}
}

func TestSettingsOpensOnTheScopeItWasAskedFor(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "repo:talkable/example"}, 120, 34)
	if m.currentSection() != "Repositories" || m.scopeLabel() != "repo talkable/example" {
		t.Fatalf("got %q %q", m.currentSection(), m.scopeLabel())
	}
	mustContain(t, viewOf(m), "no [[repo]] block yet: a save creates one")
}

func TestSettingsSpendPopupPreviewsTheRolesAsTheCursorMovesWithoutStaging(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = selectRow(t, m, "spend level")
	m = press(t, m, "enter")
	before := len(src.previews)
	m = press(t, m, "j") // high → burn
	if len(src.previews) == before {
		t.Fatal("a cursor move in the spend popup should ask for a preview")
	}
	last := src.previews[len(src.previews)-1]
	if last["pipeline.spend"].Text != "burn" {
		t.Errorf("the preview should resolve spend burn, got %+v", last)
	}
	if len(m.draft) != 0 {
		t.Errorf("a cursor move stages nothing: %+v", m.draft)
	}
	v := viewOf(m)
	mustContain(t, v, "preview: burn", "xhigh*")
	m = press(t, m, "esc")
	if len(m.draft) != 0 || m.popup != nil {
		t.Errorf("esc cancels: draft %+v", m.draft)
	}
	if strings.Contains(viewOf(m), "preview: burn") {
		t.Errorf("after esc the grid shows the draft again:\n%s", viewOf(m))
	}
	m = press(t, m, "enter", "j", "enter")
	if m.draft["pipeline.spend"].Text != "burn" {
		t.Errorf("enter stages the choice, got %+v", m.draft)
	}
}

func TestSettingsRolePopupEditsTheLevelsTableOrTheBaseModel(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = selectRow(t, m, "claude-simplify")
	m = press(t, m, "enter")
	if m.popup == nil || m.popup.kind != popRole {
		t.Fatal("enter on a role should open its popup")
	}
	mustContain(t, viewOf(m), "[spend.high] table", "[[role]] claude-simplify: base model")
	// The table: model sonnet (saved) → opus, efforts as they are.
	m = press(t, m, "enter") // where: table
	m = press(t, m, "j")     // model: sonnet → other? the options are default, gpt-6.1-sol, opus, sonnet, other
	for m.popup.model != "opus" {
		m = press(t, m, "k")
	}
	m = press(t, m, "enter", "enter", "enter")
	if m.popup != nil {
		t.Fatalf("the last field's enter stages:\n%s", viewOf(m))
	}
	if e := m.draft["spend.high.claude-simplify.model"]; e.Text != "opus" {
		t.Errorf("draft = %+v, want claude-simplify's high model opus", m.draft)
	}
	if len(m.draft) != 1 {
		t.Errorf("untouched cells stage nothing: %+v", m.draft)
	}
	// The base model.
	m = press(t, m, "enter", "j", "enter")
	for m.popup.baseModel != "opus" {
		m = press(t, m, "j")
	}
	m = press(t, m, "enter")
	if e := m.draft["role.claude-simplify.model"]; e.Text != "opus" {
		t.Errorf("draft = %+v, want claude-simplify's base model opus", m.draft)
	}
}

func TestSettingsInputRefusesAnInvalidValue(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = toSection(t, m, "Reviews & agents")
	m = selectRow(t, m, "park idle after")
	m = pump(t, m, append(keys("enter"), append(typed("soon"), keyMsg("enter"))...)...).m
	if m.popup == nil {
		t.Fatal("an invalid duration must keep the popup open")
	}
	if len(m.draft) != 0 {
		t.Errorf("nothing staged: %+v", m.draft)
	}
	mustContain(t, viewOf(m), "a duration, like 15m or 2h")
}

func TestSettingsMarksAValueTheLoaderRefuses(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = toSection(t, m, "Usage caps")
	m = selectRow(t, m, "codex soft cap")
	m = pump(t, m, append(keys("enter", "backspace", "backspace"), append(typed("99"), keyMsg("enter"))...)...).m
	v := viewOf(m)
	mustContain(t, v, "✗ invalid", "must be below codex_hard")
}

func TestSettingsRolesPopupNeedsExactlyOneJudge(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, 120, 34)
	m = selectRow(t, m, "roles")
	m = press(t, m, "enter")
	// Add claude-judge to a selection that holds codex-judge: two judges.
	m = press(t, m, "G", "space", "enter")
	if m.popup == nil {
		t.Fatal("two judges must keep the popup open")
	}
	mustContain(t, viewOf(m), "exactly one judge role")
	m = press(t, m, "space", "enter") // drop claude-judge again: no change
	if m.popup != nil {
		t.Fatalf("one judge should close the popup:\n%s", viewOf(m))
	}
	if len(m.draft) != 0 {
		t.Errorf("the same roles stage nothing: %+v", m.draft)
	}
}

func TestSettingsListEditorAddsEditsAndRemovesInOrder(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, 120, 34)
	m = selectRow(t, m, "auto-approve authors")
	m = press(t, m, "enter")
	if m.popup == nil || m.popup.list == nil {
		t.Fatal("enter on a login list opens the list editor")
	}
	m = pump(t, m, append(keys("a"), append(typed("rev-ann"), keyMsg("enter"))...)...).m
	if got := m.draft["watch.1.auto_approve_authors"].List; !slices.Equal(got, []string{"alice", "rev-ann"}) {
		t.Fatalf("add stages the list in order, got %v", got)
	}
	m = pump(t, m, append(keys("a"), append(typed("alice"), keyMsg("enter"))...)...).m
	mustContain(t, viewOf(m), "already in the list")
	m = press(t, m, "esc") // the item's input only
	if m.popup == nil {
		t.Fatal("esc in an item's input keeps the list open")
	}
	m = press(t, m, "k", "enter", "ctrl+u")
	m = pump(t, m, append(typed("bob-rev"), keyMsg("enter"))...).m
	if got := m.draft["watch.1.auto_approve_authors"].List; !slices.Equal(got, []string{"bob-rev", "rev-ann"}) {
		t.Fatalf("edit replaces the item in place, got %v", got)
	}
	m = press(t, m, "x")
	if got := m.draft["watch.1.auto_approve_authors"].List; !slices.Equal(got, []string{"rev-ann"}) {
		t.Fatalf("x removes the selected item, got %v", got)
	}
	m = press(t, m, "esc")
	if m.popup != nil {
		t.Fatal("esc closes the list editor")
	}
	if got := m.draft["watch.1.auto_approve_authors"].List; !slices.Equal(got, []string{"rev-ann"}) {
		t.Errorf("the staged list stays after esc, got %v", got)
	}
	m = press(t, m, "u")
	if got := m.draft["watch.1.auto_approve_authors"].List; !slices.Equal(got, []string{"bob-rev", "rev-ann"}) {
		t.Errorf("u takes back the removal, got %v", got)
	}
}

func TestSettingsSaveShowsTheDiffFirstAndOnlyYWrites(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = press(t, m, "s")
	if m.mode != modeSave || m.plan == nil {
		t.Fatal("s should show the diff")
	}
	if m.confirm != nil {
		t.Fatal("the diff view asks nothing before its own s")
	}
	mustContain(t, viewOf(m), "SAVE · 1 change", "[usage] · line 140", "+ codex_soft = 80  # 2026-10-20: was 85", "n notes off")
	m = press(t, m, "n")
	if src.notes[len(src.notes)-1] {
		t.Fatal("n should rebuild the diff without notes")
	}
	if strings.Contains(viewOf(m), "# 2026-10-20") {
		t.Errorf("no dated note after n:\n%s", viewOf(m))
	}
	mustContain(t, viewOf(m), "dated notes: off", "n notes on")
	m = press(t, m, "n")
	if !src.notes[len(src.notes)-1] {
		t.Fatal("n again turns the notes back on")
	}
	for _, k := range []string{"enter", "n", "space", "esc", "s"} {
		m = press(t, m, "s")
		mustContain(t, viewOf(m), "Save 1 change to ~/.config/magnum/config.toml? y/N")
		m = press(t, m, k)
		if len(src.commits) != 0 {
			t.Fatalf("%q wrote the file", k)
		}
		if m.mode != modeSave || m.confirm != nil {
			t.Fatalf("%q should cancel back to the diff", k)
		}
	}
	m = press(t, m, "s", "y")
	if len(src.commits) != 1 {
		t.Fatalf("y should write once, got %d", len(src.commits))
	}
	if len(m.draft) != 0 || m.mode != modeBrowse {
		t.Errorf("a save empties the draft and goes back: %+v %v", m.draft, m.mode)
	}
}

func TestSettingsSaveRefusalMarksTheSettingAndWritesNothing(t *testing.T) {
	src := newFakeSettings()
	src.planErr = errors.New("[pipeline] spend: unknown level \"turbo\"")
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("pipeline.spend", SettingsEdit{Text: "turbo"})
	m = press(t, m, "s")
	if m.mode == modeSave {
		t.Fatal("a refused plan shows no diff")
	}
	mustContain(t, viewOf(m), "not saved: [pipeline] spend: unknown level", "✗ invalid")
}

func TestSettingsAfterASaveAsksToRestartTheDaemon(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = press(t, m, "s", "s", "y")
	v := viewOf(m)
	mustContain(t, v, "Restart the daemon when it is idle to apply? y/N", "restart pending")
	m = press(t, m, "y")
	if src.restarts != 1 {
		t.Fatalf("y should ask the daemon to restart, got %d", src.restarts)
	}
	mustContain(t, viewOf(m), "the daemon restarts on the saved file once nothing is in flight")

	src2 := newFakeSettings()
	m = testSettings(t, src2, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = press(t, m, "s", "s", "y", "enter")
	if src2.restarts != 0 {
		t.Fatal("enter must not restart")
	}
	mustContain(t, viewOf(m), "restart pending")
}

func TestSettingsRestoreOpensTheSaveViewOfAKeptVersion(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{Restore: 2}, 120, 34)
	if !slices.Equal(src.restores, []int{2}) {
		t.Fatalf("restores = %v, want [2]", src.restores)
	}
	if m.mode != modeSave {
		t.Fatal("a restore opens on its diff")
	}
	mustContain(t, viewOf(m), "puts back kept version 2", `+ spend = "medium"`)
	m = press(t, m, "s", "y")
	if len(src.commits) != 1 || src.commits[0].Restore != 2 {
		t.Errorf("y writes the restore: %+v", src.commits)
	}
}

func TestSettingsEscAsksBeforeDiscardingUnsavedChanges(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	p := pump(t, m, keyMsg("esc"))
	if !isQuit(p.ended) {
		t.Fatal("esc with nothing staged leaves at once")
	}
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	for _, k := range []string{"esc", "ctrl+c", "q"} {
		p = pump(t, m, keyMsg(k))
		if len(p.ended) != 0 {
			t.Fatalf("%s with an unsaved change must ask first", k)
		}
		mustContain(t, viewOf(p.m), "Discard 1 unsaved change? y/N")
		kept := pump(t, p.m, keyMsg("enter"))
		if len(kept.ended) != 0 || len(kept.m.draft) != 1 {
			t.Fatalf("enter keeps the draft and the screen")
		}
		gone := pump(t, p.m, keyMsg("y"))
		if !isQuit(gone.ended) {
			t.Fatalf("y after %s discards and leaves", k)
		}
	}
}

func TestSettingsMouseNeverConfirms(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = press(t, m, "s", "s")
	m = pump(t, m, tea.MouseClickMsg{X: 1, Y: 33, Button: tea.MouseLeft}, tea.MouseReleaseMsg{X: 1, Y: 33}).m
	if len(src.commits) != 0 || m.confirm == nil {
		t.Fatal("a click must neither write nor answer the question")
	}
	if v := m.View(); v.MouseMode != tea.MouseModeNone {
		t.Errorf("the settings screen runs without mouse mode, got %v", v.MouseMode)
	}
}

func TestSettingsFramesAreCachedUntilSomethingShownChanges(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	gen := m.gen
	next, _ := m.Update(prbTickMsg{}) // a message of another screen
	m = next.(settingsModel)
	if m.gen != gen {
		t.Error("a message the screen does not show must keep its frame")
	}
	first := m.View().Content
	calls := 0
	m.cache.get(settingsFrameKey{gen: m.gen, width: m.width, height: m.height, dark: m.st.dark}, func() string { calls++; return "" })
	if calls != 0 || m.View().Content != first {
		t.Error("the same fingerprint must reuse the cached frame")
	}
	m = press(t, m, "j")
	if m.gen == gen {
		t.Error("a key that moves the cursor draws a new frame")
	}
}

func TestSettingsRuntimeActionsAskThenSendTheirRequest(t *testing.T) {
	rt := newFakeRuntime()
	m := testSettings(t, newFakeSettings(), rt, SettingsOptions{}, 120, 34)
	m = toSection(t, m, sectionRuntime)
	m = selectRow(t, m, "codex")
	m = press(t, m, "r")
	mustContain(t, viewOf(m), "Resume codex: lift its pause and forget its model limits? y/N")
	m = press(t, m, "enter")
	if len(rt.requests()) != 0 {
		t.Fatal("enter must not resume")
	}
	m = press(t, m, "r", "y")
	if got := rt.requests(); !slices.Equal(got, []string{"resume tool=codex watch="}) {
		t.Fatalf("requests = %v", got)
	}
	mustContain(t, viewOf(m), "resumed codex")
	press(t, m, "R", "y")
	if got := rt.requests(); got[len(got)-1] != "resume tool=all watch=" {
		t.Fatalf("R should resume all kinds, got %v", got)
	}

	// The pause form, then y/N, then the request.
	rt2 := newFakeRuntime()
	m = testSettings(t, newFakeSettings(), rt2, SettingsOptions{}, 120, 34)
	m = toSection(t, m, sectionRuntime)
	m = press(t, m, "p")
	if m.popup == nil || m.popup.kind != popPause {
		t.Fatal("p opens the pause form")
	}
	m = press(t, m, "j", "enter") // for a while
	m = pump(t, m, append(typed("2h"), keyMsg("enter"))...).m
	m = pump(t, m, append(typed("lunch"), keyMsg("enter"))...).m
	mustContain(t, viewOf(m), "Pause automation for 2h (lunch): running rounds finish, nothing new starts? y/N")
	m = press(t, m, "y")
	if got := rt2.requests(); !slices.Equal(got, []string{"pause for=2h0m0s until= reason=lunch"}) {
		t.Fatalf("requests = %v", got)
	}
	mustContain(t, viewOf(m), "paused automation", "paused: lunch")
}

func TestSettingsPlainResumeNamesThePausesItLeaves(t *testing.T) {
	rt := newFakeRuntime()
	rt.state.Automation = RuntimeEntry{Name: "automation", Paused: true, State: "paused: lunch"}
	m := testSettings(t, newFakeSettings(), rt, SettingsOptions{}, 120, 34)
	m = toSection(t, m, sectionRuntime)
	m = selectRow(t, m, "automation")
	m = press(t, m, "r", "y")
	mustContain(t, viewOf(m), "resumed; still paused: codex, claude (r on its row resumes it)")
}

// The daemon's answer to a plain resume names each agent kind still paused
// with the command that lifts it; the screen names only the pauses it left
// out, not the same kind twice.
func TestSettingsPlainResumeDoesNotNameAKindTheAnswerNames(t *testing.T) {
	rt := newFakeRuntime()
	rt.state.Automation = RuntimeEntry{Name: "automation", Paused: true, State: "paused: lunch"}
	rt.resumed = "resumed; claude is still paused (usage_limit) until 07:50: `magnum resume --tool claude` lifts it"
	m := testSettings(t, newFakeSettings(), rt, SettingsOptions{}, 160, 34)
	m = toSection(t, m, sectionRuntime)
	m = selectRow(t, m, "automation")
	m = press(t, m, "r", "y")
	mustContain(t, viewOf(m), "lifts it; still paused: codex (r on its row resumes it)")
}

func TestSettingsRuntimeKeysActOnlyInTheRuntimeSection(t *testing.T) {
	rt := newFakeRuntime()
	m := testSettings(t, newFakeSettings(), rt, SettingsOptions{}, 120, 34)
	m = press(t, m, "R")
	mustContain(t, viewOf(m), "R acts in the Runtime section")
	press(t, m, "y")
	if len(rt.requests()) != 0 {
		t.Fatal("R outside Runtime must send nothing")
	}
}

func TestSettingsSanitizesTheTextsItShows(t *testing.T) {
	src := newFakeSettings()
	src.data.Fields[0].Description = "the level \x1b[31mred\x1b[0m"
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = selectRow(t, m, "spend level")
	if strings.Contains(m.View().Content, "\x1b[31mred") {
		t.Error("a control sequence from the data reached the frame")
	}
}

// boardWithRows is a board over three PRs with its data loaded.
func boardWithRows(t *testing.T, s *SettingsScreen) prBoardModel {
	t.Helper()
	var rows []PRBoardRow
	for i := 1; i <= 40; i++ { // more than a screen: the board scrolls
		title := "fix the widget " + strconv.Itoa(i)
		if i%5 == 0 {
			title = "docs " + strconv.Itoa(i)
		}
		rows = append(rows, PRBoardRow{Ref: "talkable/web#" + strconv.Itoa(i), Owner: "talkable", Repo: "web", Number: i,
			Title: title, Author: []string{"alice", "rev-ann", "bob-rev"}[i%3], State: "queued"})
	}
	src := PRBoardSourceFunc(func(context.Context) ([]PRBoardRow, error) { return rows, nil })
	b := testPRBoard(t.Context(), src, nil, PRBoardOptions{Settings: s, Tick: neverTick})
	b, _ = send(t, b, tea.WindowSizeMsg{Width: 120, Height: 30})
	for _, msg := range execCmd(t, b.loadCmd()) {
		b, _ = send(t, b, msg)
	}
	return b
}

// hostPump feeds msgs to a host and the messages of its commands back.
func hostPump(t *testing.T, h tea.Model, msgs ...tea.Msg) tea.Model {
	t.Helper()
	queue := slices.Clone(msgs)
	for n := 0; len(queue) > 0; n++ {
		if n > 2000 {
			t.Fatal("the host's commands never settle")
		}
		msg := queue[0]
		queue = queue[1:]
		if _, ok := msg.(tea.QuitMsg); ok {
			continue
		}
		var cmd tea.Cmd
		h, cmd = h.Update(msg)
		queue = append(queue, settingsCmd(t, cmd)...)
	}
	return h
}

func TestCommaOnTheBoardOpensSettingsAndEscComesBackToTheSameCursor(t *testing.T) {
	s := &SettingsScreen{Source: newFakeSettings(), Runtime: newFakeRuntime()}
	b := boardWithRows(t, s)
	// A filter kept, then the cursor deep enough to scroll the table.
	b, _ = send(t, b, keys("/")...)
	b, _ = send(t, b, typed("fix")...)
	b, _ = send(t, b, keys("enter", "G", "k", "k")...)
	before := viewOf(b)
	sel, _ := b.selected()
	if b.filter.Value() != "fix" || !strings.Contains(before, "fix the widget") || strings.Contains(before, "docs 5") {
		t.Fatalf("the filter is not in effect:\n%s", before)
	}
	h := withSettings(t.Context(), b, s)
	h = hostPump(t, h, keyMsg(","))
	if hs := h.(settingsHost); hs.settings == nil {
		t.Fatal(", should open settings")
	}
	mustContain(t, ansi.Strip(h.View().Content), "magnum · settings", "SECTIONS")
	h = hostPump(t, h, keyMsg("j"), keyMsg("esc"))
	if hs := h.(settingsHost); hs.settings != nil {
		t.Fatal("esc should close settings")
	}
	back := unwrapSettings(h).(prBoardModel)
	if got, _ := back.selected(); got.Number != sel.Number {
		t.Errorf("the board's cursor moved: PR %d, want %d", got.Number, sel.Number)
	}
	if back.filter.Value() != "fix" {
		t.Errorf("the board's filter is %q, want fix", back.filter.Value())
	}
	if got := ansi.Strip(h.View().Content); got != before {
		t.Errorf("the board should come back as it was left:\n%s\nwant:\n%s", got, before)
	}
}

func TestCommaOnTheDashboardOpensSettingsAndEscComesBackToTheSameCursor(t *testing.T) {
	s := &SettingsScreen{Source: newFakeSettings(), Runtime: newFakeRuntime()}
	var data StatusData
	for i := 1; i <= 40; i++ { // more than a screen: the queue scrolls
		data.Queue = append(data.Queue, PRRow{Ref: "talkable/web#" + strconv.Itoa(i), Title: "change " + strconv.Itoa(i), State: "queued"})
	}
	src := SourceFunc(func(context.Context) (StatusData, error) { return data, nil })
	d := testDashboard(t.Context(), src, nil, DashboardOptions{Settings: s, Tick: neverTick})
	d, _ = send(t, d, tea.WindowSizeMsg{Width: 120, Height: 30})
	for _, msg := range execCmd(t, d.gatherCmd()) {
		d, _ = send(t, d, msg)
	}
	d, _ = send(t, d, keys("end", "k", "k")...)
	before := viewOf(d)
	if d.scroll == 0 || strings.Contains(before, "talkable/web#1 ") {
		t.Fatalf("the dashboard should be scrolled down (scroll %d):\n%s", d.scroll, before)
	}
	h := hostPump(t, withSettings(t.Context(), d, s), keyMsg(","))
	mustContain(t, ansi.Strip(h.View().Content), "magnum · settings")
	h = hostPump(t, h, keyMsg("esc"))
	if got := ansi.Strip(h.View().Content); got != before {
		t.Errorf("the dashboard should come back as it was left:\n%s\nwant:\n%s", got, before)
	}
}

func TestCommaWithoutSettingsSaysSo(t *testing.T) {
	b := boardWithRows(t, nil)
	b, _ = send(t, b, keyMsg(","))
	mustContain(t, viewOf(b), "settings: not available on this screen")
}

func TestSettingsKeepsTheRuntimeSectionWhenTheConfigCannotBeRead(t *testing.T) {
	src := newFakeSettings()
	src.loadErr = errors.New("config.toml: line 3: expected '='")
	rt := newFakeRuntime()
	m := testSettings(t, src, rt, SettingsOptions{}, 120, 30)
	if m.currentSection() != sectionRuntime || len(m.sections) != 1 {
		t.Fatalf("a broken file leaves Runtime alone, got %v", m.sections)
	}
	mustContain(t, viewOf(m), "cannot read the config: config.toml: line 3", "codex", "r resume codex")
	m = press(t, selectRow(t, m, "codex"), "r", "y")
	if got := rt.requests(); !slices.Equal(got, []string{"resume tool=codex watch="}) {
		t.Fatalf("the pauses still work: %v", got)
	}
	src.mu.Lock()
	src.loadErr = nil
	src.mu.Unlock()
	m = press(t, m, "ctrl+r")
	if m.loadErr != nil || m.currentSection() != "Spend & models" {
		t.Errorf("ctrl+r reads the fixed file and opens on its first section, got %v %q", m.loadErr, m.currentSection())
	}
}

func TestSettingsAnUnsetSettingsPopupStartsOnItsDefault(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, 120, 34)
	m = press(t, selectRow(t, m, "include drafts"), "enter")
	mustContain(t, viewOf(m), "› default (false) (saved)")
	m = press(t, m, "enter")
	if len(m.draft) != 0 {
		t.Fatalf("enter on an unset bool keeps its default, got %+v", m.draft)
	}
	m = testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "claude-simplify"), "enter", "j", "enter", "enter")
	if len(m.draft) != 0 {
		t.Fatalf("enter on a role's unset base model keeps it, got %+v", m.draft)
	}
}

func TestSettingsTheDraftWaitsWhileItIsChecked(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	next, plan := m.Update(keyMsg("s")) // the check is out; its answer not in yet
	m = next.(settingsModel)
	m = toSection(t, m, "Schedule")
	m = press(t, selectRow(t, m, "quiet hours"), "d")
	mustContain(t, viewOf(m), "checking the draft with magnum's loader…")
	m = press(t, m, "enter")
	if m.popup != nil || len(m.draft) != 1 {
		t.Fatalf("no popup and no edit while the draft is checked: popup %v, draft %+v", m.popup != nil, m.draft)
	}
	m = pump(t, m, settingsCmd(t, plan)...).m
	if m.mode != modeSave {
		t.Fatal("the check's answer opens the diff")
	}
	stale := settingsAsync{from: m.id, msg: settingsPlanMsg{plan: &SettingsPlan{Count: 9}, gen: m.draftGen - 1}}
	m = pump(t, m, stale).m
	if m.plan.Count == 9 {
		t.Error("a plan of an older draft must be dropped")
	}
	other := settingsAsync{from: m.id + 1, msg: settingsPlanMsg{plan: &SettingsPlan{Count: 7}, gen: m.draftGen}}
	m = pump(t, m, other).m
	if m.plan.Count == 7 {
		t.Error("an answer to another settings screen must be dropped")
	}
}

func TestSettingsShowsARuntimeAnswerOnOneLine(t *testing.T) {
	rt := &lineRuntime{fakeRuntime: newFakeRuntime()}
	m := testSettings(t, newFakeSettings(), rt, SettingsOptions{}, 120, 34)
	m = press(t, toSection(t, m, sectionRuntime), "p", "enter", "enter", "y")
	v := viewOf(m)
	if n := strings.Count(v, "\n") + 1; n > 34 {
		t.Fatalf("the frame is %d lines on a 34-line screen:\n%s", n, v)
	}
	mustContain(t, v, "paused automation: running rounds finish no daemon is running")
}

// lineRuntime answers a pause in two lines, as the cli's captured output
// does when no daemon runs.
type lineRuntime struct{ *fakeRuntime }

func (r *lineRuntime) Pause(ctx context.Context, p PauseSpec) (string, error) {
	_, err := r.fakeRuntime.Pause(ctx, p)
	return "paused automation: running rounds finish\nno daemon is running", err
}
