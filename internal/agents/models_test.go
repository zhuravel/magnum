package agents

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// fableLimitPane is what Claude Code printed in every reviewer pane when the
// Fable model was capped while the account windows (5h 20%, 7d 78%) were not.
const fableLimitPane = "  ⎿  You've reached your Fable limit. /model to switch models.\n" +
	"✻ Crunched for 6m 57s · done 12:29 PM\n" +
	"  Fable 5.1 | pr-23@HEAD | 52k/1m (5%) | effort: xhigh | 5h 20% @16:40 | 7d 78% @Fri Oct 9, 15:00\n"

func TestClassifyModelLimitApartFromUsageLimit(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC)
	at := func(h, m int) *time.Time {
		t := time.Date(2026, 10, 4, h, m, 0, 0, time.UTC)
		return &t
	}
	for _, tc := range []struct {
		text  string
		kind  HealthKind
		model string
		reset *time.Time
	}{
		{fableLimitPane, HealthModelLimit, "fable", nil},
		{"You've hit your Opus limit · resets 3:45pm", HealthModelLimit, "opus", at(15, 45)},
		{"Sonnet 4.5 limit reached", HealthModelLimit, "sonnet", nil},
		{"Something stopped. /model to switch models.", HealthModelLimit, "", nil},
		{"You've hit your limit · resets 3pm", HealthUsageLimit, "", at(15, 0)},
		{"You've hit your usage limit. Try again at 3:45 PM.", HealthUsageLimit, "", at(15, 45)},
		{"5-hour limit reached ∙ resets 4pm", HealthUsageLimit, "", at(16, 0)},
		{"  Fable 5.1 | pr-23@HEAD | 52k/1m (5%) | 5h 20% @16:40 | 7d 78% @Fri Oct 9, 15:00", HealthOK, "", nil},
	} {
		h := ClassifyAt(tc.text, now)
		if h.Kind != tc.kind || h.Model != tc.model {
			t.Errorf("%q: kind %s model %q, want %s %q", tc.text, h.Kind, h.Model, tc.kind, tc.model)
		}
		if (h.ResetAt == nil) != (tc.reset == nil) || (h.ResetAt != nil && !h.ResetAt.Equal(*tc.reset)) {
			t.Errorf("%q: reset %v, want %v", tc.text, h.ResetAt, tc.reset)
		}
	}
}

func TestSameModel(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"opus", "claude-opus-4-5", true}, {"Fable", "fable", true}, {"opus", "opus[1m]", true},
		{"", UnknownModel, true}, {"opus", "sonnet", false}, {"fable", "", false},
	} {
		if got := SameModel(tc.a, tc.b); got != tc.want {
			t.Errorf("SameModel(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// switchDialogScreen is Claude Code 2.1's confirmation of `/model opus` in a
// session with history, verbatim.
const switchDialogScreen = `  Switch model?
  Your next response will be slower and use more
  tokens
  This conversation is cached for the current model.
  Switching to Opus 5.5 means the full history gets
  re-read on your next message.
  ❯ 1. Yes, switch to Opus 5.5
    2. No, go back`

// statusScreen is a claude pane's visible screen whose status line shows
// the model's display name.
func statusScreen(display string) string {
	return "⏺ Review written.\n\n> \n  ? for shortcuts\n  " + display + " | /Users/x/Projects/talkable.review1 | 52k/1m (5%) | effort: high"
}

var displayNames = map[string]string{"opus": "Opus 5.5", "sonnet": "Sonnet 5.5", "fable": "Fable 5.1", "default": "Fable 5.1"}

// setStatusLocked sets an agent's status with f.mu held (inside a hook).
func setStatusLocked(f *fakeHerdr, name string, s herdr.Status) {
	for i := range f.agents {
		if f.agents[i].Name == name {
			f.agents[i].AgentStatus = s
		}
	}
}

// claudeScreen makes the claude-review pane answer /model as Claude Code
// does: with dialog, the "Switch model?" confirmation first (cursor on
// cursorOn: "yes" or "no"), switched on Enter; without, at once. A switch
// shows the new model on the status line.
func (e *env) claudeScreen(dialog bool, cursorOn string) {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	e.h.reads[claudeAgent] = statusScreen("Fable 5.1")
	pending := ""
	e.h.onRun = func(f *fakeHerdr, pane, cmd string) {
		model, ok := strings.CutPrefix(cmd, "/model ")
		if !ok {
			return
		}
		if !dialog {
			f.reads[claudeAgent] = statusScreen(displayNames[model])
			return
		}
		screen := switchDialogScreen
		if cursorOn == "no" {
			screen = strings.Replace(strings.Replace(screen, "❯ 1.", "  1.", 1), "  2. No", "❯ 2. No", 1)
		}
		pending = model
		f.reads[claudeAgent] = screen
		setStatusLocked(f, claudeAgent, herdr.StatusBlocked)
	}
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		if target != claudeAgent || pending == "" {
			return
		}
		switch {
		case slices.Equal(keys, []string{"enter"}) && cursorOn == "yes":
			f.reads[claudeAgent] = statusScreen(displayNames[pending])
		case slices.Equal(keys, []string{"esc"}) || slices.Equal(keys, []string{"enter"}):
			f.reads[claudeAgent] = statusScreen("Fable 5.1")
		default:
			return
		}
		pending = ""
		setStatusLocked(f, claudeAgent, herdr.StatusIdle)
	}
}

func (e *env) keysTo(target string) [][]string {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	var out [][]string
	for _, k := range e.h.keys {
		if k.Target == target {
			out = append(out, k.Keys)
		}
	}
	return out
}

func (e *env) kv(key string) (string, bool) {
	e.t.Helper()
	v, ok, err := e.st.GetKV(e.ctx, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return v, ok
}

// limitFable records that the claude kind's default model, fable, is
// limited until until (as NoteModelLimit leaves it).
func (e *env) limitFable(until time.Time) {
	e.t.Helper()
	for k, v := range map[string]string{
		KVKindCLIModel(KindClaude):          "fable",
		KVModelLimits(KindClaude):           "fable",
		KVModelLimited(KindClaude, "fable"): store.FormatTime(until),
	} {
		if err := e.st.SetKV(e.ctx, k, v); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) eventsOf(kind string) []store.Event {
	e.t.Helper()
	var out []store.Event
	for _, subject := range []string{"pr:talkable/talkable#11920", "tool:claude"} {
		evs, err := e.st.EventsBySubject(e.ctx, subject, 0)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Kind == kind {
				out = append(out, ev)
			}
		}
	}
	return out
}

func TestNoteModelLimitRecordsLimitAndLearnsDefaultModel(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	l, err := e.m.NoteModelLimit(e.ctx, s, ClassifyAt(fableLimitPane, e.clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	want := e.clock.Now().Add(DefaultModelLimitCooldown) // no reset named: daemon.model_limit_cooldown
	if l.Kind != KindClaude || l.Model != "fable" || !l.Until.Equal(want) || l.Using != "opus" {
		t.Fatalf("limit = %+v, want fable until %s using opus", l, want)
	}
	if v, _ := e.kv(KVModelLimited(KindClaude, "fable")); v != store.FormatTime(want) {
		t.Fatalf("limited kv = %q", v)
	}
	if v, _ := e.kv(KVKindCLIModel(KindClaude)); v != "fable" {
		t.Fatalf("a role without model ran the CLI default, so fable is it; got %q", v)
	}
	if evs := e.eventsOf(EventModelLimited); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"model":"fable"`) {
		t.Fatalf("events = %+v", evs)
	}

	// A named reset wins; an earlier one never shortens the recorded end.
	reset := e.clock.Now().Add(time.Hour)
	l, _ = e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "fable", ResetAt: &reset})
	if !l.Until.Equal(want) {
		t.Fatalf("until = %s, want the later %s kept", l.Until, want)
	}
	reset = e.clock.Now().Add(8 * time.Hour)
	l, _ = e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "opus", ResetAt: &reset})
	if !l.Until.Equal(reset) || l.Using != "sonnet" {
		t.Fatalf("opus limit = %+v", l)
	}
	if got := ModelLimits(e.ctx, e.st, e.cfg, KindClaude, e.clock.Now()); len(got) != 2 || got[0].Model != "fable" || got[1].Model != "opus" {
		t.Fatalf("ModelLimits = %+v", got)
	}
	if got := ModelLimits(e.ctx, e.st, e.cfg, KindClaude, want.Add(time.Minute)); len(got) != 1 || got[0].Model != "opus" {
		t.Fatalf("ModelLimits after fable's end = %+v", got)
	}
}

func TestFallbackModelSkipsLimitedTriedAndCurrent(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	e.limitFable(e.clock.Now().Add(time.Hour))
	if m, ok := e.m.FallbackModel(e.ctx, s, nil); !ok || m != "opus" {
		t.Fatalf("fallback = %q %v, want opus", m, ok)
	}
	if m, ok := e.m.FallbackModel(e.ctx, s, []string{"opus"}); !ok || m != "sonnet" {
		t.Fatalf("fallback after opus = %q %v, want sonnet", m, ok)
	}
	if _, ok := e.m.FallbackModel(e.ctx, s, []string{"opus", "sonnet"}); ok {
		t.Fatal("every fallback tried: none left")
	}
	if _, ok := e.m.FallbackModel(e.ctx, e.session(RoleJudge), nil); ok {
		t.Fatal("codex has no switch_model: it cannot switch")
	}
}

func TestSwitchModelTypesCommandWaitsAndRecords(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	s := e.session(RoleClaude)
	e.h.reads["mg-11920-claude-review-5d01cf"] = "> /model opus\n  ⎿  Set model to Opus 5.5\n"
	if err := e.m.SwitchModel(e.ctx, s, "opus", SwitchLimitHit); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.h.runs, paneRunCall{ws.Panes[RoleClaude], "/model opus"}) {
		t.Fatalf("pane runs = %+v", e.h.runs)
	}
	if v, _ := e.kv(KVSessionModel(s.ID)); v != "opus" {
		t.Fatalf("session model = %q", v)
	}
	evs := e.eventsOf(EventModelSwitched)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"to":"opus"`) || !strings.Contains(string(evs[0].Data), `"reason":"model_limit"`) {
		t.Fatalf("events = %+v", evs)
	}

	// Back to the CLI default (the role sets no model): the record goes.
	if err := e.m.SwitchModel(e.ctx, s, "default", SwitchExpired); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.kv(KVSessionModel(s.ID)); ok {
		t.Fatal("a session back on its role's model keeps no record")
	}

	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusWorking)
	n := len(e.h.runs)
	if err := e.m.SwitchModel(e.ctx, s, "opus", SwitchLimitHit); !errors.Is(err, ErrBusy) || len(e.h.runs) != n {
		t.Fatalf("working agent: err %v, %d new pane runs; want ErrBusy and none", err, len(e.h.runs)-n)
	}
	if err := e.m.SwitchModel(e.ctx, e.session(RoleJudge), "o3", SwitchLimitHit); !errors.Is(err, ErrNoModelSwitch) {
		t.Fatalf("codex: err = %v, want ErrNoModelSwitch", err)
	}
}

func TestStartAgentWhileModelLimitedStartsOnFallback(t *testing.T) {
	e := newEnv(t)
	e.limitFable(e.clock.Now().Add(time.Hour))
	e.started()
	var claudeArgs, judgeArgs []string
	for _, st := range e.h.starts {
		switch st.Kind {
		case KindClaude:
			claudeArgs = st.Args
		case KindCodex:
			judgeArgs = st.Args
		}
	}
	if i := slices.Index(claudeArgs, "--model"); i < 0 || i+1 >= len(claudeArgs) || claudeArgs[i+1] != "opus" {
		t.Fatalf("claude args = %q, want --model opus", claudeArgs)
	}
	if slices.Contains(judgeArgs, "--model") {
		t.Fatalf("codex judge args = %q: its model is not limited", judgeArgs)
	}
	if v, _ := e.kv(KVSessionModel(e.session(RoleClaude).ID)); v != "opus" {
		t.Fatalf("session model = %q, want opus", v)
	}
	if evs := e.eventsOf(EventModelSwitched); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"reason":"model_limited"`) {
		t.Fatalf("events = %+v", evs)
	}
}

func TestSubmitSwitchesLimitedSessionFirstAndBackAfterExpiry(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.claudeScreen(false, "")
	e.clock.Add(time.Minute)
	until := e.clock.Now().Add(time.Hour)
	e.limitFable(until) // the session runs the CLI default, fable
	s := e.session(RoleClaude)

	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunContinue, "go on"); err != nil {
		t.Fatal(err)
	}
	switched := slices.Index(e.h.calls, "PaneRun "+ws.Panes[RoleClaude])
	prompted := slices.Index(e.h.calls, "AgentPrompt mg-11920-claude-review-5d01cf")
	if switched < 0 || prompted < switched || !slices.Contains(e.h.runs, paneRunCall{ws.Panes[RoleClaude], "/model opus"}) {
		t.Fatalf("want /model opus before the prompt; calls = %q", e.h.calls)
	}
	if v, _ := e.kv(KVSessionModel(s.ID)); v != "opus" {
		t.Fatalf("session model = %q", v)
	}

	// Still limited: the next prompt needs no switch.
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusIdle)
	n := len(e.h.runs)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunContinue, "again"); err != nil {
		t.Fatal(err)
	}
	if len(e.h.runs) != n {
		t.Fatalf("unexpected pane runs %+v", e.h.runs[n:])
	}

	// The limit is over: back to the CLI default before the prompt.
	e.clock.Add(time.Hour + time.Minute)
	e.h.setAgentStatus("mg-11920-claude-review-5d01cf", herdr.StatusIdle)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunContinue, "later"); err != nil {
		t.Fatal(err)
	}
	if got := e.h.runs[len(e.h.runs)-1]; got != (paneRunCall{ws.Panes[RoleClaude], "/model default"}) {
		t.Fatalf("last pane run = %+v, want /model default", got)
	}
	if _, ok := e.kv(KVSessionModel(s.ID)); ok {
		t.Fatal("back on the role's model: no session model record")
	}
	if evs := e.eventsOf(EventModelSwitched); len(evs) != 2 || !strings.Contains(string(evs[1].Data), `"reason":"limit_expired"`) {
		t.Fatalf("events = %+v", evs)
	}
}

// A role with its own model goes back to that model, not the CLI default.
func TestSubmitSwitchesBackToRoleModel(t *testing.T) {
	e := newEnv(t)
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Name == string(RoleClaude) {
			e.cfg.Roles[i].Model = "opus"
		}
	}
	ws := e.started()
	e.claudeScreen(false, "")
	s := e.session(RoleClaude)
	if err := e.st.SetKV(e.ctx, KVSessionModel(s.ID), "sonnet"); err != nil { // switched during an expired opus limit
		t.Fatal(err)
	}
	e.clock.Add(time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunContinue, "go on"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.h.runs, paneRunCall{ws.Panes[RoleClaude], "/model opus"}) {
		t.Fatalf("pane runs = %+v, want /model opus", e.h.runs)
	}
	if _, ok := e.kv(KVSessionModel(s.ID)); ok {
		t.Fatal("back on the role's model: no session model record")
	}
}

// The race behind a judge marked lost in the second it started: the tick's
// snapshot was taken before StartAgent made the session live.
func TestObserveNeverLosesSessionStartedAfterSnapshot(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	capturedAt := e.clock.Now()
	stale, err := e.h.Snapshot(e.ctx) // no agents yet
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Add(2 * time.Second)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	obs, err := e.m.ObserveSnapshotAt(e.ctx, stale, capturedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Kind == ObsLost {
			t.Fatalf("%s judged lost from a snapshot taken before it started", o.Role)
		}
	}
	if s := e.session(RoleJudge); s.State != store.SessionLive {
		t.Fatalf("judge = %s, want live", s.State)
	}

	// A snapshot taken well after the start that lacks the agent is the
	// real thing.
	e.clock.Add(LostGrace)
	obs, err = e.m.ObserveSnapshotAt(e.ctx, stale, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	lost := false
	for _, o := range obs {
		lost = lost || (o.Role == RoleJudge && o.Kind == ObsLost)
	}
	if !lost {
		t.Fatalf("observations = %+v, want the judge lost", obs)
	}
}

func TestSubmitRebindsLostSessionWhoseAgentIsPresent(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleJudge)
	if err := e.st.TransitionSession(e.ctx, s.ID, liveStates, store.SessionLost, nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "judge"); err != nil {
		t.Fatalf("Prompt = %v, want the lost session rebound", err)
	}
	if got := e.session(RoleJudge); got.ID != s.ID || got.State != store.SessionLive {
		t.Fatalf("judge session = %+v", got)
	}
	if evs := e.eventsOf(EventRebound); len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}

	// An agent that is really gone stays refused.
	s = e.session(RoleJudge)
	if err := e.st.TransitionSession(e.ctx, s.ID, liveStates, store.SessionLost, nil); err != nil {
		t.Fatal(err)
	}
	e.h.removeAgent("mg-11920-codex-judge-5d01cf")
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "judge"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Prompt = %v, want ErrNoSession", err)
	}
}

func TestSwitchModelConfirmsClaudeSwitchDialog(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(true, "yes")
	s := e.session(RoleClaude)
	if err := e.m.SwitchModel(e.ctx, s, "opus", SwitchLimitHit); err != nil {
		t.Fatal(err)
	}
	if got := e.keysTo(claudeAgent); !slices.EqualFunc(got, [][]string{{"enter"}}, slices.Equal) {
		t.Fatalf("keys = %q, want one Enter on the Yes option", got)
	}
	if v, _ := e.kv(KVSessionModel(s.ID)); v != "opus" {
		t.Fatalf("session model = %q", v)
	}
}

func TestSwitchModelNeverConfirmsWithCursorOffYes(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(true, "no")
	s := e.session(RoleClaude)
	err := e.m.SwitchModel(e.ctx, s, "opus", SwitchLimitHit)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if got := e.keysTo(claudeAgent); !slices.EqualFunc(got, [][]string{{"esc"}}, slices.Equal) {
		t.Fatalf("keys = %q, want only Esc out of the dialog (never Enter on No)", got)
	}
	if _, ok := e.kv(KVSessionModel(s.ID)); ok {
		t.Fatal("an unconfirmed switch records no model")
	}
	if evs := e.eventsOf(EventModelSwitched); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

// The status line still names the old model: the switch did not take.
func TestSwitchModelNeedsStatusLineConfirmation(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.reads[claudeAgent] = statusScreen("Fable 5.1")
	if err := e.m.SwitchModel(e.ctx, e.session(RoleClaude), "opus", SwitchLimitHit); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestSwitchDialogIsNoPermissionPrompt(t *testing.T) {
	if _, ok := detectPermissionPrompt(switchDialogScreen); ok {
		t.Fatal("the model-switch confirmation must never be denied as a permission prompt")
	}
	if onYes, ok := detectSwitchDialog(switchDialogScreen); !ok || !onYes {
		t.Fatalf("detectSwitchDialog = %v %v, want the dialog with the cursor on Yes", onYes, ok)
	}
	if _, ok := detectSwitchDialog(statusScreen("Opus 5.5")); ok {
		t.Fatal("a status line is no dialog")
	}
}

// The model-switch confirmation is live only at the bottom of the screen,
// as a hooks review is: its options are the last lines but for blanks and
// key hints, and no permission prompt is on screen. Its text an agent's
// output left above a permission prompt, the composer or more output never
// yields the Enter magnum sends, which could land on "Yes" of the prompt.
func TestDetectSwitchDialogOnlyAtTheBottom(t *testing.T) {
	if onYes, ok := detectSwitchDialog(switchDialogScreen + "\n\n  Enter to confirm · Esc to cancel\n"); !ok || !onYes {
		t.Fatalf("with a key hint below: %v %v, want the dialog with the cursor on Yes", onYes, ok)
	}
	for name, text := range map[string]string{
		"above a permission prompt": switchDialogScreen + "\n" + claudeRmPrompt,
		"above the composer":        switchDialogScreen + "\n" + claudeIdleScreen,
		"above an edit prompt":      switchDialogScreen + "\n" + claudeEditPrompt,
		"output below":              switchDialogScreen + "\n⏺ Review written.\n",
	} {
		if onYes, ok := detectSwitchDialog(text); ok {
			t.Errorf("%s: detected the switch dialog (cursor on yes %v)", name, onYes)
		}
	}
}

func TestModelShownReadsStatusLine(t *testing.T) {
	for _, tc := range []struct {
		screen string
		names  []string
		want   bool
	}{
		{statusScreen("Opus 5.5"), []string{"opus"}, true},
		{statusScreen("Fable 5.1"), []string{"opus"}, false},
		{"> /model opus\n" + statusScreen("Fable 5.1"), []string{"opus"}, false}, // the echoed command is no confirmation
		{"  ⎿  Set model to Opus 5.5\n\n\n\n\n> \n  ? for shortcuts", []string{"opus"}, true},
		{"gpt-6.1-sol | ~/x", []string{"gpt-6.1-sol"}, true},
	} {
		if got := modelShown(tc.screen, "/model opus", tc.names); got != tc.want {
			t.Errorf("modelShown(%q, %v) = %v, want %v", tc.screen, tc.names, got, tc.want)
		}
	}
}

// While magnum switches a session's model, its own confirmation dialog is
// neither reported to the human nor answered by the deny policy.
func TestObserveLeavesSwitchingSessionAlone(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	e.h.setAgentStatus(claudeAgent, herdr.StatusBlocked)
	e.h.reads[claudeAgent] = switchDialogScreen
	e.m.setSwitching(s.ID, true)
	if o := e.observe()[RoleClaude]; o.Kind != "" {
		t.Fatalf("observation while switching = %+v, want none", o)
	}
	e.m.setSwitching(s.ID, false)
	if o := e.observe()[RoleClaude]; o.Kind != ObsBlocked {
		t.Fatalf("observation = %+v, want blocked once magnum no longer switches", o)
	}
	if got := e.keysTo(claudeAgent); len(got) != 0 {
		t.Fatalf("keys = %q, want none", got)
	}
}

// A kind's default_model reaches every role of the kind without its own
// model, through the kind's model args.
func TestStartAgentPassesKindDefaultModel(t *testing.T) {
	e := newEnv(t)
	e.setKind(KindCodex, func(k *config.Kind) { k.DefaultModel = "gpt-6.1-sol" })
	e.started()
	for _, st := range e.h.starts {
		if st.Kind != KindCodex {
			continue
		}
		if i := slices.Index(st.Args, "--model"); i < 0 || i+1 >= len(st.Args) || st.Args[i+1] != "gpt-6.1-sol" {
			t.Fatalf("codex judge args = %q, want --model gpt-6.1-sol", st.Args)
		}
		return
	}
	t.Fatal("no codex start")
}
