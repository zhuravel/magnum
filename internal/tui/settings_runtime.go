package tui

// The settings screen's Runtime section: automation's pause, each agent
// kind's pause and each watch's identity-leak pause. They apply at once
// through the same requests and registry writes as `magnum pause` and
// `magnum resume`, each after its own y/N, and are never saved to the file.

import (
	"errors"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

var (
	errPauseFor   = errors.New("a positive duration, like 45m or 2h")
	errPauseUntil = errors.New("a local time like 15:30, or an RFC3339 time")
)

// runtimeRows are the Runtime section's rows.
func (m settingsModel) runtimeRows() []setRow {
	if m.runtime == nil {
		return nil
	}
	s := m.runtime
	auto := s.Automation
	if auto.Name == "" {
		auto.Name = "automation"
	}
	out := []setRow{{kind: rowRuntime, what: rtAutomation, entry: auto}}
	if len(s.Kinds) > 0 {
		out = append(out, setRow{kind: rowHeading, label: "agent kinds"})
		paused := 0
		for _, k := range s.Kinds {
			out = append(out, setRow{kind: rowRuntime, what: rtKind, entry: k})
			if k.Paused {
				paused++
			}
		}
		out = append(out, setRow{kind: rowRuntime, what: rtAllKinds, paused: paused, kinds: len(s.Kinds),
			entry: RuntimeEntry{Name: "all kinds"}})
	}
	if len(s.Watches) > 0 {
		out = append(out, setRow{kind: rowHeading, label: "watches · identity-leak pauses"})
		for _, w := range s.Watches {
			w.Label = m.watchLabel(w)
			out = append(out, setRow{kind: rowRuntime, what: rtWatch, entry: w})
		}
	}
	return out
}

// watchLabel names a watch owner's row as the scopes number the file's
// watches ("watch 1 talkable"), its first one when it has several; a watch
// the file does not hold (a base config's) keeps its source's label.
func (m settingsModel) watchLabel(w RuntimeEntry) string {
	if m.data != nil {
		for _, s := range m.data.Scopes {
			if s.Watch && strings.EqualFold(s.Name, w.Name) {
				return "watch " + strconv.Itoa(s.Index) + " " + w.Name
			}
		}
	}
	return orEmpty(w.Label, "watch "+w.Name)
}

// rtLabel is a Runtime row's WHAT cell.
func rtLabel(r setRow) string {
	if r.entry.Label != "" {
		return r.entry.Label
	}
	return r.entry.Name
}

// rtState is a Runtime row's STATE cell, unstyled.
func rtState(r setRow) string {
	switch {
	case r.what == rtAllKinds:
		return strconv.Itoa(r.paused) + " of " + strconv.Itoa(r.kinds) + " paused"
	case r.entry.State != "":
		return r.entry.State
	case r.entry.Paused:
		return "paused"
	case r.what == rtAutomation:
		return "running"
	}
	return "no pause"
}

// rtAction is a Runtime row's ACTION cell: the key and its short label (the
// full command is in the detail strip); "" when nothing applies.
func rtAction(r setRow) (key, label string) {
	switch r.what {
	case rtAutomation:
		if r.entry.Paused {
			return "r", "resume"
		}
		return "p", "pause"
	case rtKind:
		if r.entry.Paused {
			return "r", "resume " + r.entry.Name
		}
	case rtAllKinds:
		if r.paused > 0 {
			return "R", "resume all"
		}
	case rtWatch:
		if r.entry.Paused {
			return "r", "resume watch"
		}
	}
	return "", ""
}

// rtCommand is the detail strip's last line for a Runtime row: the full
// command its key runs and what it does.
func rtCommand(r setRow) string {
	switch r.what {
	case rtAutomation:
		if r.entry.Paused {
			return "r = magnum resume: lifts the pause (agent-kind and watch pauses stay) · p = magnum pause again"
		}
		return "p = magnum pause [--for 2h | --until 15:30] [--reason text]: running rounds finish, nothing new starts"
	case rtKind:
		return "r = magnum resume --tool " + r.entry.Name + ": lifts its usage-limit, login or budget pause and forgets its model limits"
	case rtAllKinds:
		return "R = magnum resume --tool all: lifts every agent kind's pause and forgets their model limits"
	case rtWatch:
		return "r = magnum resume --watch " + r.entry.Name + ": lifts its identity-leak pause"
	}
	return ""
}

// runtimeKey handles p, r and R: they act only in the Runtime section, on
// the selected row, each after y/N.
func (m settingsModel) runtimeKey(k string, rows []setRow) (settingsModel, tea.Cmd, bool) {
	if m.currentSection() != sectionRuntime || m.query != "" {
		return m.warn(k + " acts in the Runtime section (tab to it)"), nil, true
	}
	switch {
	case m.rt == nil:
		return m.warn("runtime controls need magnum's registry"), nil, true
	case m.busy != "":
		return m.warn("wait: " + m.busy), nil, true
	}
	if k == "p" {
		return m.openPausePopup()
	}
	if k == "R" {
		return m.askResume("Resume every agent kind: lift their pauses and forget their model limits?", ResumeSpec{Tool: "all"}), nil, true
	}
	r, ok := rowAt(rows, m.cursor)
	if !ok || r.kind != rowRuntime {
		return m, nil, true
	}
	switch r.what {
	case rtAutomation:
		if !r.entry.Paused {
			return m.warn("automation is running: p pauses it"), nil, true
		}
		return m.askResume("Resume automation? Agent-kind and watch pauses stay.", ResumeSpec{}), nil, true
	case rtKind:
		if !r.entry.Paused {
			return m.warn(r.entry.Name + " has no pause"), nil, true
		}
		return m.askResume("Resume "+r.entry.Name+": lift its pause and forget its model limits?", ResumeSpec{Tool: r.entry.Name}), nil, true
	case rtAllKinds:
		return m.askResume("Resume every agent kind: lift their pauses and forget their model limits?", ResumeSpec{Tool: "all"}), nil, true
	case rtWatch:
		if !r.entry.Paused {
			return m.warn("watch " + r.entry.Name + " has no pause"), nil, true
		}
		return m.askResume("Resume watch "+r.entry.Name+": lift its identity-leak pause?", ResumeSpec{Watch: r.entry.Name}), nil, true
	}
	return m, nil, true
}

// askResume asks question and, on y, sends the resume; the answer names
// the pauses a plain resume leaves.
func (m settingsModel) askResume(question string, spec ResumeSpec) settingsModel {
	m.confirm = &settingsConfirm{question: question, yes: func(m settingsModel) (settingsModel, tea.Cmd) {
		m.busy = "resuming"
		ctx, rt := m.ctx, m.rt
		return m, m.async(func() tea.Msg {
			text, err := rt.Resume(ctx, spec)
			if err != nil || spec != (ResumeSpec{}) {
				return settingsAnswerMsg{text: text, err: err, reload: true}
			}
			if s, err := rt.Runtime(ctx); err == nil {
				text = joinNonEmpty(text, leftPauses(s, text))
			}
			return settingsAnswerMsg{text: text, reload: true}
		})
	}}
	return m
}

// leftPauses names the pauses a plain resume leaves: each agent kind's and
// each watch's, with the key that lifts it, but a kind the resume's answer
// names with the command that lifts it (the daemon's answer does).
func leftPauses(s RuntimeState, answer string) string {
	var left []string
	for _, k := range s.Kinds {
		if k.Paused && !strings.Contains(answer, "`magnum resume --tool "+k.Name+"`") {
			left = append(left, k.Name)
		}
	}
	for _, w := range s.Watches {
		if w.Paused {
			left = append(left, "watch "+w.Name)
		}
	}
	if len(left) == 0 {
		return ""
	}
	return "still paused: " + strings.Join(left, ", ") + " (r on its row resumes it)"
}

// joinNonEmpty joins the non-empty parts with "; ".
func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// pauseQuestion asks before a pause with spec.
func pauseQuestion(spec PauseSpec) string {
	q := "Pause automation"
	switch {
	case spec.For > 0:
		q += " for " + HumanDuration(spec.For)
	case spec.Until != "":
		q += " until " + spec.Until
	}
	if spec.Reason != "" {
		q += " (" + spec.Reason + ")"
	}
	return q + ": running rounds finish, nothing new starts?"
}

// askPause asks before pausing and, on y, sends it.
func (m settingsModel) askPause(spec PauseSpec) settingsModel {
	m.confirm = &settingsConfirm{question: pauseQuestion(spec), yes: func(m settingsModel) (settingsModel, tea.Cmd) {
		m.busy = "pausing"
		ctx, rt := m.ctx, m.rt
		return m, m.async(func() tea.Msg {
			text, err := rt.Pause(ctx, spec)
			return settingsAnswerMsg{text: text, err: err, reload: true}
		})
	}}
	return m
}

// parsePauseFor reads the pause form's "for": a positive duration.
func parsePauseFor(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	switch {
	case err != nil:
		return 0, errPauseFor
	case d <= 0:
		return 0, errPauseFor
	}
	return d, nil
}

// checkPauseUntil checks the pause form's "until": HH:MM or RFC3339.
func checkPauseUntil(s string) error {
	s = strings.TrimSpace(s)
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return nil
	}
	if _, err := time.Parse("15:04", s); err == nil {
		return nil
	}
	return errPauseUntil
}
