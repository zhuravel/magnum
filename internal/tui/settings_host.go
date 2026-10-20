package tui

// `,` on the dashboard and the PR board opens the settings screen inside
// the same program: the host keeps the screen that opened it, which goes on
// receiving its own timers and data while settings shows, and takes the
// keys back when the user leaves settings. The board comes back exactly as
// it was left (its cursor, filter, scroll, view and an action in flight),
// with nothing to rebuild.

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// settingsKey opens the settings screen from the dashboard and the board.
const settingsKey = ","

// openSettingsMsg asks the host to show the settings screen.
type openSettingsMsg struct{}

// openSettings is the command `,` returns: the host opens settings, or the
// screen says it cannot.
func openSettings() tea.Msg { return openSettingsMsg{} }

// settingsHost runs a screen with the settings screen over it on demand.
type settingsHost struct {
	inner    tea.Model
	screen   *SettingsScreen
	ctx      context.Context
	settings *settingsModel // nil: the inner screen shows
	width    int
	height   int
	bg       tea.Msg // the terminal's last background answer, for settings opened later
	profile  tea.Msg // the last color profile message
}

// withSettings wraps m so `,` can open the settings screen; with no screen
// it returns m as it is.
func withSettings(ctx context.Context, m tea.Model, s *SettingsScreen) tea.Model {
	if s == nil || s.Source == nil {
		return m
	}
	return settingsHost{inner: m, screen: s, ctx: ctx}
}

// unwrapSettings is the screen a host ran (m itself when it is no host).
func unwrapSettings(m tea.Model) tea.Model {
	if h, ok := m.(settingsHost); ok {
		return h.inner
	}
	return m
}

func (h settingsHost) Init() tea.Cmd { return h.inner.Init() }

func (h settingsHost) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h.width, h.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		h.bg = msg
	case tea.ColorProfileMsg:
		h.profile = msg
	case openSettingsMsg:
		if h.settings != nil {
			return h, nil
		}
		s := newSettingsModel(h.ctx, h.screen.Source, h.screen.Runtime, h.screen.Options, true)
		s.width, s.height = h.width, h.height
		for _, m := range []tea.Msg{h.bg, h.profile} {
			if m != nil {
				s, _ = s.step(m)
			}
		}
		h.settings = &s
		return h, s.Init()
	case settingsClosedMsg:
		h.settings = nil
		return h, nil
	}
	if h.settings == nil {
		next, cmd := h.inner.Update(msg)
		h.inner = next
		return h, cmd
	}
	switch msg.(type) {
	case tea.KeyPressMsg, tea.KeyReleaseMsg, tea.MouseMsg, tea.PasteMsg:
		s, cmd := h.settings.step(msg)
		h.settings = &s
		return h, cmd
	}
	// Everything else reaches both: the inner screen's timers keep it
	// fresh, settings' own answers reach settings.
	s, cmdS := h.settings.step(msg)
	h.settings = &s
	inner, cmdI := h.inner.Update(msg)
	h.inner = inner
	return h, tea.Batch(cmdI, cmdS)
}

func (h settingsHost) View() tea.View {
	if h.settings != nil {
		return h.settings.View()
	}
	return h.inner.View()
}
