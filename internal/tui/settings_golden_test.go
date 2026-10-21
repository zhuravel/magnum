package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var updateSettingsGolden = flag.Bool("update-settings", false, "rewrite the settings screen's golden frames under testdata/settings/")

// checkSettingsFrame compares a frame of the real painter with its golden
// file and checks that no line is wider than the screen.
func checkSettingsFrame(t *testing.T, name, frame string, width int) {
	t.Helper()
	for i, l := range strings.Split(frame, "\n") {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("%s: line %d is %d cells wide, over %d: %q", name, i+1, w, width, ansi.Strip(l))
		}
	}
	path := filepath.Join("testdata", "settings", name)
	if *updateSettingsGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(frame+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update-settings)", err, t.Name())
	}
	if got := frame + "\n"; got != string(want) {
		t.Errorf("%s differs from its golden frame (go test -run %s -update-settings rewrites it)\ngot:\n%s\nwant:\n%s",
			name, t.Name(), got, want)
	}
}

// staged is the fixture screen with the edit of the main mockup staged:
// claude-simplify's model at high from sonnet to opus.
func stagedSettings(t *testing.T, w, h int) settingsModel {
	t.Helper()
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
	m = m.stage("spend.high.claude-simplify.model", SettingsEdit{Text: "opus"})
	m = pump(t, m, settingsCmd(t, m.previewCmd(""))...).m
	return selectRow(t, m, "claude-simplify")
}

func TestTheSettingsScreenMatchesItsGoldenFrames(t *testing.T) {
	light := tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")}
	cases := []struct {
		name  string
		w, h  int
		build func(t *testing.T, w, h int) settingsModel
		plain bool
	}{
		{name: "spend-140.txt", w: 140, h: 30, build: stagedSettings, plain: true},
		{name: "spend-120.txt", w: 120, h: 34, build: stagedSettings, plain: true},
		{name: "spend-90.txt", w: 90, h: 34, build: stagedSettings, plain: true},
		{name: "spend-120-dark.ansi", w: 120, h: 34, build: stagedSettings},
		{name: "spend-120-light.ansi", w: 120, h: 34, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			return pump(t, stagedSettings(t, w, h), light).m
		}},
		{name: "edit-spend-120.txt", w: 120, h: 34, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := selectRow(t, stagedSettings(t, w, h), "spend level")
			return press(t, m, "enter", "j")
		}},
		{name: "edit-spend-140.txt", w: 140, h: 30, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := selectRow(t, stagedSettings(t, w, h), "spend level")
			return press(t, m, "enter", "j")
		}},
		{name: "runtime-120.txt", w: 120, h: 24, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			return selectRow(t, toSection(t, m, sectionRuntime), "codex")
		}},
		{name: "runtime-90.txt", w: 90, h: 24, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			return selectRow(t, toSection(t, m, sectionRuntime), "codex")
		}},
		{name: "watch-120.txt", w: 120, h: 26, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, w, h)
			m = m.stage("watch.1.auto_approve_authors", SettingsEdit{List: []string{"alice", "rev-ann"}})
			return selectRow(t, m, "auto-approve authors")
		}},
		{name: "watch-90.txt", w: 90, h: 26, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, w, h)
			m = m.stage("watch.1.auto_approve_authors", SettingsEdit{List: []string{"alice", "rev-ann"}})
			return selectRow(t, m, "auto-approve authors")
		}},
		{name: "save-120.txt", w: 120, h: 26, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			m = m.stage("pipeline.spend", SettingsEdit{Text: "burn"})
			m = m.stage("spend.burn.claude-simplify.model", SettingsEdit{Text: "opus"})
			m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
			return press(t, press(t, m, "s"), "s")
		}},
		{name: "role-140.txt", w: 140, h: 30, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			return press(t, stagedSettings(t, w, h), "enter", "enter")
		}},
		{name: "list-120.txt", w: 120, h: 26, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, w, h)
			return press(t, selectRow(t, m, "auto-approve authors"), "enter")
		}},
		{name: "pause-120.txt", w: 120, h: 24, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			return press(t, toSection(t, m, sectionRuntime), "p", "j")
		}},
		{name: "search-120.txt", w: 120, h: 24, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			return typeIn(t, press(t, m, "/"), "auth")
		}},
		{name: "help-90.txt", w: 90, h: 26, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			return press(t, m, "?")
		}},
		{name: "saved-120.txt", w: 120, h: 24, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
			return press(t, toSection(t, m, "Usage caps"), "s", "s", "y")
		}},
		{name: "save-90.txt", w: 90, h: 26, plain: true, build: func(t *testing.T, w, h int) settingsModel {
			t.Helper()
			m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, h)
			m = m.stage("pipeline.spend", SettingsEdit{Text: "burn"})
			return press(t, m, "s")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := c.build(t, c.w, c.h)
			frame := m.View().Content
			if c.plain {
				frame = ansi.Strip(frame)
			}
			checkSettingsFrame(t, c.name, frame, c.w)
		})
	}
}

// The help box is at most 90 cells wide however wide the screen is, and a
// line that does not fit it ends in "…": the entries of a / x and / did, at
// every width. F5 is bound beside ctrl+r and the help says so.
func TestSettingsHelpLinesFitTheirBox(t *testing.T) {
	for _, w := range []int{90, 120, 160} {
		m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, w, 30)
		help := ansi.Strip(press(t, m, "?").View().Content)
		if strings.Contains(help, "…") {
			t.Errorf("the help at %d columns cuts a line short:\n%s", w, help)
		}
		for _, want := range []string{"ctrl+r / F5", "add after the selected item", "search every label and key"} {
			if !strings.Contains(help, want) {
				t.Errorf("the help at %d columns lacks %q:\n%s", w, want, help)
			}
		}
	}
}

func TestSettingsFitsANarrowScreen(t *testing.T) {
	for _, w := range []int{40, 60, 80} {
		m := stagedSettings(t, w, 24)
		frames := map[string]string{"spend": m.View().Content}
		frames["popup"] = press(t, selectRow(t, m, "spend level"), "enter").View().Content
		frames["role"] = press(t, m, "enter").View().Content
		frames["runtime"] = toSection(t, m, sectionRuntime).View().Content
		frames["save"] = press(t, m, "s", "s").View().Content
		frames["help"] = press(t, m, "?").View().Content
		for name, f := range frames {
			for i, l := range strings.Split(f, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%s at %d columns: line %d is %d wide: %q", name, w, i+1, lw, ansi.Strip(l))
				}
			}
		}
	}
}

// Without color the frame reads the same: every state is a word or a mark
// (default, set, ● unsaved, ✗ invalid, a changed cell's *, the cursor's ›),
// and only the reverse video of the selected row, which a monochrome
// terminal keeps, goes beyond the text.
func TestTheSettingsScreenReadsTheSameWithoutColor(t *testing.T) {
	m := stagedSettings(t, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "99"})
	m = pump(t, m, settingsCmd(t, m.previewCmd(""))...).m
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{"default", "set", "● unsaved", "opus*", "› claude-simplify", "2 unsaved"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the colorless frame lacks %q:\n%s", want, plain)
		}
	}
	m = toSection(t, m, "Usage caps")
	if plain := ansi.Strip(m.View().Content); !strings.Contains(plain, "✗ invalid") {
		t.Errorf("an invalid value reads without color:\n%s", plain)
	}
}
