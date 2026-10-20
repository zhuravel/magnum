package tui

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// The settings screen's fixtures: a fake config source and runtime over
// placeholder data (organizations talkable and example; logins alice,
// rev-ann and bob-rev), and a pump that runs the screen's commands.

var (
	fixtureRoles  = []string{"codex-judge", "claude-review", "codex-review", "claude-simplify", "claude-judge", "claude-fix", "claude-whiteboard"}
	fixtureModels = []string{"gpt-6.1-sol", "opus", "sonnet"}
	fixtureEffort = []string{"low", "medium", "high", "xhigh"}
)

// fixtureBase is each role's base model and its saved efforts at high.
var fixtureBase = map[string][3]string{
	"codex-judge":       {"gpt-6.1-sol", "xhigh", "high"},
	"claude-review":     {"sonnet", "xhigh", "high"},
	"codex-review":      {"gpt-6.1-sol", "xhigh", ""},
	"claude-simplify":   {"sonnet", "high", ""},
	"claude-judge":      {"opus", "xhigh", "high"},
	"claude-fix":        {"opus", "high", ""},
	"claude-whiteboard": {"sonnet", "medium", ""},
}

// fixtureSettings is a config like an operator's: spend high with its
// table, two watches, a repository block and a watched repository without
// one.
func fixtureSettings() *SettingsData {
	var fs []SettingField
	add := func(f SettingField) {
		if f.Value == "" && f.Set {
			f.Value = f.Text
		}
		if f.Value == "" {
			f.Value = f.Default
		}
		if f.Set && f.From == "" {
			f.From = "your file, line " + strconv.Itoa(f.Line)
		}
		if !f.Set && f.From == "" {
			f.From = "built-in default"
		}
		if f.Path == "" { // the loader's name of the key: "[usage] codex_soft" is usage.codex_soft
			f.Path = strings.NewReplacer("[", "", "] ", ".", "]", "").Replace(f.Key)
		}
		fs = append(fs, f)
	}
	spend := "Spend & models"
	add(SettingField{Target: "pipeline.spend", Section: spend, Label: "spend level", Key: "[pipeline] spend", Type: SettingEnum,
		Options: []string{"low", "medium", "high", "burn"}, Default: "medium", Text: "high", Set: true, Line: 12,
		Description: "the level every role's model and effort follows",
		Notes:       map[string]string{"burn": "starts from medium, then its table (7 roles)", "high": "xhigh for every built-in effort"}})
	for _, k := range []string{"codex", "claude"} {
		add(SettingField{Target: "kinds." + k + ".spend", Section: spend, Label: k + " spend", Key: "[kinds." + k + "] spend", Type: SettingEnum,
			Options: []string{"low", "medium", "high", "burn"}, Default: "inherit ([pipeline] spend)", Value: "inherit (high)",
			Inherits: "pipeline.spend", Description: "the spend level of the " + k + " roles in place of [pipeline] spend"})
	}
	line := 80
	for _, level := range []string{"high", "burn"} {
		for _, r := range fixtureRoles {
			b := fixtureBase[r]
			model := SettingField{Target: "spend." + level + "." + r + ".model", Section: spend, Label: r + " model (" + level + ")",
				Key: "[spend." + level + "]." + r + ".model", Type: SettingModel, Options: fixtureModels, Role: r, Level: level, Cell: "model",
				Default: "the role's base model (" + b[0] + ")", Description: "the model " + r + " runs while spend is " + level}
			effort := SettingField{Target: "spend." + level + "." + r + ".effort", Section: spend, Label: r + " effort (" + level + ")",
				Key: "[spend." + level + "]." + r + ".effort", Type: SettingEffort, Options: fixtureEffort, Role: r, Level: level, Cell: "effort",
				Default: "the level's notch", Description: "the effort " + r + " starts with while spend is " + level}
			again := SettingField{Target: "spend." + level + "." + r + ".rereview_effort", Section: spend, Label: r + " re-review effort (" + level + ")",
				Key: "[spend." + level + "]." + r + ".rereview_effort", Type: SettingEffort, Options: fixtureEffort, Role: r, Level: level,
				Cell: "rereview_effort", Default: "the level's notch", Description: "the effort " + r + " re-reviews with while spend is " + level}
			if level == "high" && (r == "claude-review" || r == "claude-judge" || r == "codex-judge" || r == "codex-review") {
				line++
				model.Text, model.Set, model.Line = map[string]string{"claude-review": "opus", "claude-judge": "opus"}[r], true, line
				if model.Text == "" {
					model.Text = "gpt-6.1-sol"
				}
				effort.Text, effort.Set, effort.Line = b[1], true, line
			}
			if level == "high" && r == "claude-simplify" {
				line++
				model.Text, model.Set, model.Line = "sonnet", true, line
			}
			if level == "burn" {
				line++
				effort.Text, effort.Set, effort.Line = "xhigh", true, line
				if r == "claude-simplify" {
					model.Text, model.Set, model.Line = "sonnet", true, line
				}
			}
			add(model)
			add(effort)
			add(again)
		}
	}
	for i, r := range fixtureRoles {
		add(SettingField{Target: "role." + r + ".model", Section: spend, Label: r + " base model", Key: "[[role]] " + r + " · model",
			Type: SettingModel, Options: fixtureModels, Role: r, Cell: "model", Default: fixtureBase[r][0], Line: 200 + i,
			Description: "the model " + r + " runs when its spend level sets none"})
	}
	caps := "Usage caps"
	add(SettingField{Target: "usage.codex_soft", Section: caps, Label: "codex soft cap", Key: "[usage] codex_soft", Type: SettingPercent,
		Default: "90", Text: "85", Set: true, Line: 140, Description: "above it only reviews you ask for start (percent of the Codex week)"})
	add(SettingField{Target: "usage.codex_hard", Section: caps, Label: "codex hard cap", Key: "[usage] codex_hard", Type: SettingPercent,
		Default: "95", Description: "above it Codex roles pause (percent of the Codex week)"})
	watches := "Watches"
	repos := "Repositories"
	scopes := []SettingsScope{
		{ID: "watch:1", Watch: true, Index: 1, Name: "talkable", Repos: []string{"talkable/talkable", "talkable/example", "talkable/web"}},
		{ID: "watch:2", Watch: true, Index: 2, Name: "example", Repos: []string{"example/site"}},
		{ID: "repo:talkable/web", Name: "talkable/web"},
		{ID: "repo:talkable/example", Name: "talkable/example", NoBlock: true},
	}
	for _, w := range scopes[:2] {
		pre := "watch." + strconv.Itoa(w.Index) + "."
		kt := "[[watch]] " + strconv.Itoa(w.Index) + " (" + w.Name + ") · "
		roles := SettingField{Target: pre + "roles", Section: watches, Scope: w.ID, Label: "roles", Key: kt + "roles", Type: SettingRoles,
			Options: []string{"codex-judge", "claude-review", "codex-review", "claude-simplify", "claude-judge"},
			Notes:   map[string]string{"codex-judge": "judge", "claude-judge": "judge"}, Default: "every role",
			Description: "the roles that review this watch's PRs (exactly one judge)",
			ValidateList: func(l []string) error {
				n := 0
				for _, r := range l {
					if strings.HasSuffix(r, "-judge") {
						n++
					}
				}
				if n != 1 {
					return errors.New("exactly one judge role")
				}
				return nil
			}}
		authors := SettingField{Target: pre + "auto_approve_authors", Section: watches, Scope: w.ID, Label: "auto-approve authors",
			Key: kt + "auto_approve_authors", Type: SettingLogins, Default: "every author",
			Description: "only these authors' PRs are auto-approved (allowlist); empty = every author"}
		skip := SettingField{Target: pre + "auto_approve_skip_authors", Section: watches, Scope: w.ID, Label: "skip authors",
			Key: kt + "auto_approve_skip_authors", Type: SettingLogins, Default: "none", Description: "these authors' PRs are never auto-approved"}
		drafts := SettingField{Target: pre + "include_drafts", Section: watches, Scope: w.ID, Label: "include drafts",
			Key: kt + "include_drafts", Type: SettingBool, Default: "false", Description: "review draft PRs too"}
		stakes := SettingField{Target: pre + "stakes", Section: watches, Scope: w.ID, Label: "stakes", Key: kt + "stakes",
			Type: SettingText, Default: "none", Description: "what is at stake in this watch's code, for the reviewers"}
		rereview := SettingField{Target: pre + "own_min_rereview_interval", Section: watches, Scope: w.ID, Label: "own re-review interval",
			Key: kt + "own_min_rereview_interval", Type: SettingDuration, Default: "inherit (2h)", Description: "the least time between re-reviews of your own PRs"}
		if w.Index == 1 {
			roles.List, roles.Set, roles.Line = []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}, true, 25
			authors.List, authors.Set, authors.Line = []string{"alice"}, true, 31
			skip.List, skip.Set, skip.Line = []string{"bob-rev"}, true, 32
			stakes.Text, stakes.Set, stakes.Line = "referral emails and rewards reach real customers", true, 33
		}
		for _, f := range []SettingField{roles, authors, skip, drafts, stakes, rereview} {
			add(f)
		}
	}
	for _, r := range scopes[2:] {
		pre := "repo." + r.Name + "."
		kt := "[[repo]] " + r.Name + " · "
		stakes := SettingField{Target: pre + "stakes", Section: repos, Scope: r.ID, Label: "stakes", Key: kt + "stakes", Type: SettingText,
			Default: "the watch's", Description: "what is at stake in this repository"}
		simpl := SettingField{Target: pre + "max_simplifications", Section: repos, Scope: r.ID, Label: "max simplifications",
			Key: kt + "max_simplifications", Type: SettingInt, Default: "3", Description: "the most simplification findings a review posts"}
		checks := SettingField{Target: pre + "required_checks", Section: repos, Scope: r.ID, Label: "required checks",
			Key: kt + "required_checks", Type: SettingList, Default: "none", Description: "CI checks that must pass before an auto-approval"}
		if !r.NoBlock {
			simpl.Text, simpl.Set, simpl.Line = "1", true, 60
			checks.List, checks.Set, checks.Line = []string{"ci / rspec", "ci / lint"}, true, 61
		}
		for _, f := range []SettingField{stakes, simpl, checks} {
			add(f)
		}
	}
	sched := "Schedule"
	add(SettingField{Target: "daemon.quiet_hours", Section: sched, Label: "quiet hours", Key: "[daemon] quiet_hours", Type: SettingText,
		Default: "none", Text: "22:00-07:00", Set: true, Line: 20, Description: "no automatic round starts in these hours"})
	add(SettingField{Target: "pipeline.min_rereview_interval", Section: sched, Label: "re-review interval", Key: "[pipeline] min_rereview_interval",
		Type: SettingDuration, Default: "30m", Description: "the least time between re-reviews of a PR"})
	rev := "Reviews & agents"
	add(SettingField{Target: "daemon.park_idle_after", Section: rev, Label: "park idle after", Key: "[daemon] park_idle_after",
		Type: SettingDuration, Default: "15m", Description: "an idle agent parks after this long"})
	add(SettingField{Target: "pipeline.status_comment", Section: rev, Label: "status comment", Key: "[pipeline] status_comment",
		Type: SettingBool, Default: "true", Description: "keep a status comment on each reviewed PR"})
	saved := make([]SettingsRole, 0, len(fixtureRoles))
	for _, r := range fixtureRoles {
		saved = append(saved, resolveRole(fs, nil, r))
	}
	return &SettingsData{
		File: "~/.config/magnum/config.toml", HistoryDir: "~/.local/share/magnum/config-history/",
		Sections: []string{spend, caps, watches, repos, sched, rev},
		Fields:   fs, Scopes: scopes, Roles: saved,
	}
}

// resolveRole is the fake loader: a role's model and efforts under the
// spend level the draft (or the file) sets.
func resolveRole(fs []SettingField, d SettingsDraft, role string) SettingsRole {
	val := func(f SettingField) (string, bool) {
		if e, ok := d[f.Target]; ok {
			if e.Default {
				return "", false
			}
			return e.Text, true
		}
		return f.Text, f.Set
	}
	find := func(target string) SettingField {
		for _, f := range fs {
			if f.Target == target {
				return f
			}
		}
		return SettingField{}
	}
	level, ok := val(find("pipeline.spend"))
	if !ok {
		level = "medium"
	}
	kind, _, _ := strings.Cut(role, "-")
	if k, ok := val(find("kinds." + kind + ".spend")); ok {
		level = k
	}
	model, ok := val(find("spend." + level + "." + role + ".model"))
	if !ok {
		if model, ok = val(find("role." + role + ".model")); !ok {
			model = fixtureBase[role][0]
		}
	}
	first, ok := val(find("spend." + level + "." + role + ".effort"))
	if !ok {
		first = fixtureBase[role][1]
	}
	again, ok := val(find("spend." + level + "." + role + ".rereview_effort"))
	if !ok {
		again = fixtureBase[role][2]
	}
	return SettingsRole{Role: role, Kind: kind, Model: model, First: first, Again: again, Level: level}
}

// fakeSettings is a SettingsSource over fixtureSettings that records what
// the screen asked.
type fakeSettings struct {
	mu       sync.Mutex
	data     *SettingsData
	previews []SettingsDraft
	plans    []SettingsDraft
	notes    []bool
	commits  []*SettingsPlan
	restarts int
	restores []int
	planErr  error
	loadErr  error
	// loadCount counts the reads of the file.
	loadCount int
}

func newFakeSettings() *fakeSettings { return &fakeSettings{data: fixtureSettings()} }

func (f *fakeSettings) Load(context.Context) (*SettingsData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCount++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	d := *f.data
	return &d, nil
}

func (f *fakeSettings) Preview(_ context.Context, d SettingsDraft) ([]SettingsRole, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previews = append(f.previews, maps.Clone(d))
	for t, e := range d {
		if t == "usage.codex_soft" && e.Text == "99" {
			return nil, errors.New("[usage] codex_soft (99) must be below codex_hard (95)")
		}
	}
	out := make([]SettingsRole, 0, len(fixtureRoles))
	for _, r := range fixtureRoles {
		out = append(out, resolveRole(f.data.Fields, d, r))
	}
	return out, nil
}

func (f *fakeSettings) Plan(_ context.Context, d SettingsDraft, notes bool) (*SettingsPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, maps.Clone(d))
	f.notes = append(f.notes, notes)
	if f.planErr != nil {
		return nil, f.planErr
	}
	return fixturePlan(f.data.Fields, d, notes), nil
}

// fixturePlan is the save of d as the editor writes it: a scalar replaced
// in place (numbers and booleans bare, strings quoted), a list as an array,
// and a spend cell inside its role's inline table, one line per role and
// level, with the dated note of each changed line when notes is true.
func fixturePlan(fs []SettingField, d SettingsDraft, notes bool) *SettingsPlan {
	byTarget := map[string]SettingField{}
	for _, f := range fs {
		byTarget[f.Target] = f
	}
	p := &SettingsPlan{Count: len(d), Notes: notes}
	done := map[string]bool{}
	for _, t := range slices.Sorted(maps.Keys(d)) {
		field := byTarget[t]
		if field.Role != "" && field.Level != "" {
			if key := field.Level + "/" + field.Role; !done[key] {
				done[key] = true
				p.Hunks = append(p.Hunks, cellHunk(fs, d, field, notes))
			}
			continue
		}
		e := d[t]
		table, key := splitKey(field.Key)
		h := SettingsHunk{Table: table, Line: field.Line}
		if field.Set {
			h.Removed = []string{key + " = " + tomlValue(field.Type, field.Text, field.List)}
		}
		if !e.Default {
			added := key + " = " + tomlValue(field.Type, e.Text, e.List)
			if notes && field.Set {
				added += "  # 2026-10-20: was " + tomlValue(field.Type, field.Text, field.List)
			}
			h.Added = []string{added}
		}
		p.Hunks = append(p.Hunks, h)
	}
	return p
}

// splitKey is a key path's table heading and key: "[usage] codex_soft",
// "[[watch]] 1 (talkable) · auto_approve_authors", "[[role]] r · model".
func splitKey(k string) (table, key string) {
	if i := strings.LastIndex(k, " · "); i > 0 {
		return k[:i], k[i+len(" · "):]
	}
	table, key, _ = strings.Cut(k, " ")
	return table, key
}

// tomlValue is a value as the file writes it.
func tomlValue(t SettingType, text string, list []string) string {
	switch {
	case t.IsList():
		q := make([]string, len(list))
		for i, v := range list {
			q[i] = strconv.Quote(v)
		}
		return "[" + strings.Join(q, ", ") + "]"
	case t == SettingInt || t == SettingPercent || t == SettingBool:
		return text
	}
	return strconv.Quote(text)
}

// cellHunk is the change of a role's inline table at a spend level: its
// line before and after every staged cell of that role and level.
func cellHunk(fs []SettingField, d SettingsDraft, at SettingField, notes bool) SettingsHunk {
	cells := func(staged bool) (string, int, []string) {
		var parts, was []string
		line := 0
		for _, f := range fs {
			if f.Role != at.Role || f.Level != at.Level {
				continue
			}
			val, set := f.Text, f.Set
			if e, ok := d[f.Target]; ok && staged {
				val, set = e.Text, !e.Default
				if f.Set && val != f.Text {
					was = append(was, f.Cell+" "+strconv.Quote(f.Text))
				}
			}
			if f.Set && line == 0 {
				line = f.Line
			}
			if set {
				parts = append(parts, f.Cell+" = "+strconv.Quote(val))
			}
		}
		if len(parts) == 0 {
			return "", line, was
		}
		return at.Role + " = { " + strings.Join(parts, ", ") + " }", line, was
	}
	before, line, _ := cells(false)
	after, _, was := cells(true)
	h := SettingsHunk{Table: "[spend." + at.Level + "]", Line: line}
	if before != "" {
		h.Removed = []string{before}
	}
	if after != "" {
		if notes && len(was) > 0 {
			after += "  # 2026-10-20: was " + strings.Join(was, ", ")
		}
		h.Added = []string{after}
	}
	return h
}

func (f *fakeSettings) Commit(_ context.Context, p *SettingsPlan) (SettingsSaved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, p)
	f.data.RestartPending = true
	return SettingsSaved{History: "~/.local/share/magnum/config-history/20261020T101405Z.toml", AfterRestart: true}, nil
}

func (f *fakeSettings) RequestRestart(context.Context, SettingsSaved) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return "the daemon restarts on the saved file once nothing is in flight", nil
}

func (f *fakeSettings) Restore(_ context.Context, n int) (*SettingsPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, n)
	return &SettingsPlan{Count: 1, Restore: n, Hunks: []SettingsHunk{{Table: "[pipeline]", Line: 12,
		Removed: []string{`spend = "high"`}, Added: []string{`spend = "medium"`}}}}, nil
}

// fakeRuntime is a SettingsRuntime that records the requests it got.
type fakeRuntime struct {
	mu    sync.Mutex
	state RuntimeState
	sent  []string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{state: RuntimeState{
		Daemon:     true,
		Automation: RuntimeEntry{Name: "automation", State: "running"},
		Kinds: []RuntimeEntry{
			{Name: "codex", Paused: true, State: "paused: budget cap, 95% of the week used · since 14:20",
				Detail: "paused by the Codex budget cap: 95% of this week used ([usage] codex_hard = 95)"},
			{Name: "claude", Paused: true, State: "paused: usage limit until 07:50 · since 02:10", Detail: "Claude's usage limit; it lifts at 07:50"},
		},
		Watches: []RuntimeEntry{
			{Name: "talkable", Label: "watch 1 talkable", State: "no pause"},
			{Name: "example", Label: "watch 2 example", State: "no pause"},
		},
	}}
}

func (f *fakeRuntime) Runtime(context.Context) (RuntimeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state
	s.Kinds, s.Watches = slices.Clone(s.Kinds), slices.Clone(s.Watches)
	return s, nil
}

func (f *fakeRuntime) Pause(_ context.Context, p PauseSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, "pause for="+p.For.String()+" until="+p.Until+" reason="+p.Reason)
	f.state.Automation = RuntimeEntry{Name: "automation", Paused: true, State: "paused: " + p.Reason}
	return "paused automation: running rounds finish, nothing new starts; `magnum resume` lifts it", nil
}

func (f *fakeRuntime) Resume(_ context.Context, r ResumeSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, "resume tool="+r.Tool+" watch="+r.Watch)
	switch {
	case r.Tool == "all":
		for i := range f.state.Kinds {
			f.state.Kinds[i].Paused, f.state.Kinds[i].State = false, "no pause"
		}
		return "resumed codex and claude", nil
	case r.Tool != "":
		for i := range f.state.Kinds {
			if f.state.Kinds[i].Name == r.Tool {
				f.state.Kinds[i].Paused, f.state.Kinds[i].State = false, "no pause"
			}
		}
		return "resumed " + r.Tool, nil
	case r.Watch != "":
		return "resumed watch " + r.Watch, nil
	}
	f.state.Automation = RuntimeEntry{Name: "automation", State: "running"}
	return "resumed", nil
}

func (f *fakeRuntime) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// settingsCmd runs cmd as execCmd does, dropping cursor blinks too, and
// returns its messages (batches and sequences expanded).
func settingsCmd(tb testing.TB, cmd tea.Cmd) []tea.Msg {
	tb.Helper()
	if cmd == nil || isInstantTick(cmd) || reflect.ValueOf(cmd).Pointer() == blinkCode {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(execCmdWait):
		tb.Fatalf("a command still runs after %v", execCmdWait)
	}
	if reflect.TypeOf(msg) == reflect.TypeOf(tea.RequestBackgroundColor()) {
		return nil
	}
	switch m := msg.(type) {
	case nil, spinner.TickMsg, cursor.BlinkMsg:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range m {
			out = append(out, settingsCmd(tb, c)...)
		}
		return out
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		var out []tea.Msg
		for i := range v.Len() {
			out = append(out, settingsCmd(tb, v.Index(i).Interface().(tea.Cmd))...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// pumped is what a pump left: the model and the messages that end or leave
// the screen (tea.QuitMsg, settingsClosedMsg), which it does not feed.
type pumped struct {
	m     settingsModel
	ended []tea.Msg
}

// pump feeds msgs to m and the messages of every command they return back
// to it, until none is left.
func pump(t *testing.T, m settingsModel, msgs ...tea.Msg) pumped {
	t.Helper()
	queue := slices.Clone(msgs)
	var ended []tea.Msg
	for n := 0; len(queue) > 0; n++ {
		if n > 2000 {
			t.Fatal("the screen's commands never settle")
		}
		msg := queue[0]
		queue = queue[1:]
		switch msg.(type) {
		case tea.QuitMsg, settingsClosedMsg:
			ended = append(ended, msg)
			continue
		}
		next, cmd := m.Update(msg)
		m = next.(settingsModel)
		queue = append(queue, settingsCmd(t, cmd)...)
	}
	return pumped{m: m, ended: ended}
}

// press pumps key presses one at a time, each after the screen settled
// from the one before, as a person types.
func press(t *testing.T, m settingsModel, names ...string) settingsModel {
	t.Helper()
	for _, n := range names {
		m = pump(t, m, keyMsg(n)).m
	}
	return m
}

// typeIn types s one key at a time, then the keys after.
func typeIn(t *testing.T, m settingsModel, s string, after ...string) settingsModel {
	t.Helper()
	for _, r := range s {
		m = pump(t, m, keyMsg(string(r))).m
	}
	return press(t, m, after...)
}

// testSettings builds the screen over src and rt as RunSettings does, at
// w x h, loaded.
func testSettings(t *testing.T, src SettingsSource, rt SettingsRuntime, opts SettingsOptions, w, h int) settingsModel {
	t.Helper()
	m := newSettingsModel(t.Context(), src, rt, opts, false)
	return pump(t, m, append([]tea.Msg{tea.WindowSizeMsg{Width: w, Height: h}}, settingsCmd(t, m.Init())...)...).m
}

// selectRow moves the cursor to the row whose plain text starts with
// label after the mark.
func selectRow(t *testing.T, m settingsModel, label string) settingsModel {
	t.Helper()
	rows := m.rows()
	for i, r := range rows {
		name := ""
		switch r.kind {
		case rowField:
			name = r.prefix + m.data.Fields[r.field].Label
		case rowRole:
			name = r.role
		case rowRuntime:
			name = rtLabel(r)
		}
		if name == label {
			m.cursor = i
			m.fixScroll()
			m.gen = nextGen()
			return m
		}
	}
	t.Fatalf("no row %q in %s", label, m.currentSection())
	return m
}

// toSection tabs to the section named sec.
func toSection(t *testing.T, m settingsModel, sec string) settingsModel {
	t.Helper()
	for range len(m.sections) {
		if m.currentSection() == sec {
			return m
		}
		m = press(t, m, "tab")
	}
	t.Fatalf("no section %q in %v", sec, m.sections)
	return m
}
