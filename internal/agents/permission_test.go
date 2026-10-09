package agents

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Permission prompts as Claude Code and Codex 0.160 render them in a pane's
// visible screen. claudeRmPrompt is the live one that blocked a round: Claude
// Code asks even under --dangerously-skip-permissions (rule over the dialog,
// title, the command card, the question, the options, the key hints).
const (
	claudeRmPrompt = `⏺ Writing the review now.

────────────────────────────────────────────────────────────────────────────
 Bash command · from the code-review agent
 │ Create the review output directory, remove the scratch export, confirm the worktree is clean
 │ mkdir -p out ; S="/private/tmp/x/scratchpad"; rm -rf "$S/run"; git -C /Users/x/Projects/talkable.review1 status --short | head -5
 │ Dangerous rm operation on possibly-empty variable path: ${S}/run (use a literal path: when the expansion is empty this removes /run)

 Do you want to proceed?
 ❯ 1. Yes
   2. No

 Esc to cancel · Tab to amend
`
	// claudeEditPrompt has three options, the second wrapped in a narrow pane.
	claudeEditPrompt = `────────────────────────────────────────────
 Edit file
 │ app/models/user.rb

 Do you want to make this edit to user.rb?
 ❯ 1. Yes
   2. Yes, allow all edits during this session
      (shift+tab)
   3. No, and tell Claude what to do differently
      (esc)

 Esc to cancel · Tab to amend
`
	codexCommandPrompt = `• Running the specs.

  Would you like to run the following command?

  Reason: Need network access to fetch gems

  $ bundle install

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for this command (a)
  3. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel
`
	codexEditsPrompt = `  Would you like to make the following edits?

  app/models/user.rb (+1 -1)

› 1. Yes, proceed (y)
  2. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel
`
	claudeRejected = "⏺ Bash(rm -rf \"$S/run\")\n  ⎿  User rejected the command\n\n  ⎿  Interrupted · What should Claude do instead?\n\n" + "╭──────╮\n│ >    │\n╰──────╯\n"
)

func TestDetectPermissionPrompt(t *testing.T) {
	boxed := "╭──────────────────────────────────────────────╮\n│ Bash command                                 │\n│   rm -rf tmp/cache                           │\n" +
		"│ Do you want to proceed?                      │\n│ ❯ 1. Yes                                     │\n" +
		"│   2. No, and tell Claude what to do differently (esc) │\n╰──────────────────────────────────────────────╯\n"
	cases := []struct {
		name     string
		text     string
		ok       bool
		header   string
		denyNum  string // "" = a (y/n) prompt
		denyText string
	}{
		{"claude rm check", claudeRmPrompt, true, "Bash command · from the code-review agent", "2", "No"},
		{"claude without the rule", strings.SplitN(claudeRmPrompt, "────────────────────────────────────────────────────────────────────────────\n", 2)[1],
			true, "Do you want to proceed?", "2", "No"},
		{"claude boxed", boxed, true, "Bash command", "2", "No, and tell Claude what to do differently (esc)"},
		{"claude three options wrapped", claudeEditPrompt, true, "Edit file", "3", "No, and tell Claude what to do differently (esc)"},
		{"codex command", codexCommandPrompt, true, "Would you like to run the following command?", "3", "No, and tell Codex what to do differently (esc)"},
		{"codex edits", codexEditsPrompt, true, "Would you like to make the following edits?", "2", "No, and tell Codex what to do differently (esc)"},
		{"allow y/n", "Allow? (y/n)\n", true, "Allow? (y/n)", "", ""},
		{"allow command [y/N]", "  Allow command `make deploy`? [y/N]\n\n  esc to cancel\n", true, "Allow command `make deploy`? [y/N]", "", ""},

		{"transcript quoting the question", "⏺ The installer asks \"Do you want to proceed?\" before it deletes anything.\n\n> \n", false, "", "", ""},
		{"question alone in output", "Do you want to proceed?\nThe script continues without asking.\n", false, "", "", ""},
		{"quoted prompt the output continues under", strings.TrimSuffix(claudeRmPrompt, "\n") + "\n⏺ That was the prompt the docs describe; moving on.\n", false, "", "", ""},
		{"no cursor on any option", strings.Replace(claudeRmPrompt, "❯ 1. Yes", "  1. Yes", 1), false, "", "", ""},
		{"no No option", strings.Replace(claudeRmPrompt, "2. No", "2. Yes, always", 1), false, "", "", ""},
		{"no Yes option", strings.Replace(claudeRmPrompt, "1. Yes", "1. Maybe", 1), false, "", "", ""},
		{"numbers out of order", strings.Replace(claudeRmPrompt, "2. No", "3. No", 1), false, "", "", ""},
		{"one option", strings.Replace(claudeRmPrompt, "   2. No\n", "", 1), false, "", "", ""},
		{"y/n with output under it", "Allow? (y/n)\nok, continuing\n", false, "", "", ""},
		{"codex trust", codexTrustScreen, false, "", "", ""},
		{"claude trust", claudeTrustScreen, false, "", "", ""},
		{"claude old trust", "Do you trust the files in this folder?\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit\n", false, "", "", ""},
		{"codex approval-mode trust", "Allow Codex to work in this folder without asking for approval?\n\n› 1. Yes, allow Codex to work in this folder\n  2. No, ask me to approve edits and commands\n", false, "", "", ""},
		{"idle", claudeIdleScreen, false, "", "", ""},
		{"empty", "", false, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := detectPermissionPrompt(tc.text)
			if ok != tc.ok {
				t.Fatalf("detect = %+v, %v; want ok %v", p, ok, tc.ok)
			}
			if !ok {
				return
			}
			if p.header != tc.header || p.key == "" {
				t.Fatalf("header = %q (key %q), want %q", p.header, p.key, tc.header)
			}
			if tc.denyNum == "" {
				if p.options != nil {
					t.Fatalf("options = %+v, want a (y/n) prompt", p.options)
				}
				return
			}
			if d := p.options[p.deny]; d.num != tc.denyNum || d.label != tc.denyText || d.selected {
				t.Fatalf("deny option = %+v, want %s. %q", d, tc.denyNum, tc.denyText)
			}
			if p.cursor() != 0 {
				t.Fatalf("cursor = %d, want the first option", p.cursor())
			}
		})
	}
}

func TestPermissionPromptKey(t *testing.T) {
	a, _ := detectPermissionPrompt(claudeRmPrompt)
	moved, _ := detectPermissionPrompt(strings.Replace(strings.Replace(claudeRmPrompt, "❯ 1. Yes", "  1. Yes", 1), "  2. No", "❯ 2. No", 1))
	other, _ := detectPermissionPrompt(strings.Replace(claudeRmPrompt, `rm -rf "$S/run"`, `rm -rf "$S/tmp"`, 1))
	codexA, _ := detectPermissionPrompt(codexCommandPrompt)
	codexB, _ := detectPermissionPrompt(strings.Replace(codexCommandPrompt, "$ bundle install", "$ bundle exec rake", 1))
	if a.key != moved.key {
		t.Fatal("moving the cursor must keep the prompt's identity")
	}
	if a.key == other.key || codexA.key == codexB.key {
		t.Fatal("another command is another prompt")
	}
}

// blockedOn prompts claude-review (a run in flight), then shows screen in its
// pane with the agent blocked. It returns the run id.
func (e *env) blockedOn(role Role, screen string) string {
	e.t.Helper()
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, role, store.RunInitial, "review")
	if err != nil {
		e.t.Fatalf("Prompt %s: %v", role, err)
	}
	name := store.Deref(e.session(role).AgentName)
	e.h.setAgentStatus(name, herdr.StatusBlocked)
	e.h.mu.Lock()
	e.h.reads[name] = screen
	e.h.keys = nil
	e.h.mu.Unlock()
	return id
}

func (e *env) promptEvents(kind string) []store.Event {
	e.t.Helper()
	evs, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

const claudeAgent = "mg-11920-claude-review-5d01cf"

// resolveOn makes the fake agent leave its prompt when it receives key: the
// screen becomes claudeRejected and the agent works again.
func resolveOn(key string) func(f *fakeHerdr, target string, keys []string) {
	return func(f *fakeHerdr, target string, keys []string) {
		if !slices.Contains(keys, key) {
			return
		}
		f.reads[target] = claudeRejected
		for i := range f.agents {
			if f.agents[i].Name == target {
				f.agents[i].AgentStatus = herdr.StatusWorking
			}
		}
	}
}

func TestObserveDeniesClaudePermissionPrompt(t *testing.T) {
	cursorOnNo := strings.Replace(strings.Replace(claudeRmPrompt, "❯ 1. Yes", "  1. Yes", 1), "  2. No", "❯ 2. No", 1)
	cases := []struct {
		name  string
		onKey func(f *fakeHerdr, target string, keys []string)
		want  []string
	}{
		// Claude Code selects an option by its number: the prompt is gone.
		{"number selects", resolveOn("2"), []string{claudeAgent + ":2"}},
		// The number only moves the cursor: Enter confirms No.
		{"number moves the cursor", func(f *fakeHerdr, target string, keys []string) {
			if slices.Contains(keys, "2") {
				f.reads[target] = cursorOnNo
			}
			resolveOn("enter")(f, target, keys)
		}, []string{claudeAgent + ":2", claudeAgent + ":enter"}},
		// The number does nothing: the cursor stays on Yes, so no Enter; Escape cancels.
		{"number ignored", resolveOn("esc"), []string{claudeAgent + ":2", claudeAgent + ":esc"}},
		// Enter does not take: Escape once.
		{"enter ignored", func(f *fakeHerdr, target string, keys []string) {
			if slices.Contains(keys, "2") {
				f.reads[target] = cursorOnNo
			}
		}, []string{claudeAgent + ":2", claudeAgent + ":enter", claudeAgent + ":esc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			run := e.blockedOn(RoleClaude, claudeRmPrompt)
			e.h.onKeys = tc.onKey

			o := e.observe()[RoleClaude]
			if o.Kind != ObsPromptDenied || o.Status != herdr.StatusBlocked || o.Run == nil || o.Run.ID != run {
				t.Fatalf("observation = %+v, want prompt_denied for run %s", o, run)
			}
			if got := e.keysSent(); !slices.Equal(got, tc.want) {
				t.Fatalf("keys = %q, want %q", got, tc.want)
			}
			for _, k := range e.keysSent() {
				if strings.Contains(k, ":1") || strings.Contains(k, ":y") {
					t.Fatalf("keys %q approve", k)
				}
			}
			evs := e.promptEvents(EventPromptDenied)
			if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "Bash command · from the code-review agent") {
				t.Fatalf("events = %+v", evs)
			}
			var data map[string]any
			if err := json.Unmarshal(evs[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["role"] != string(RoleClaude) || data["kind"] != KindClaude || data["header"] != "Bash command · from the code-review agent" ||
				data["run"] != run || data["count"] != float64(1) {
				t.Fatalf("event data = %v", data)
			}
			if strings.Contains(string(evs[0].Data), "rm -rf") || strings.Contains(evs[0].Message, "rm -rf") {
				t.Fatal("the event must carry the header, not the command")
			}
		})
	}
}

func TestObserveAfterDenyIsWorkingNotHuman(t *testing.T) {
	e := newEnv(t)
	e.started()
	run := e.blockedOn(RoleClaude, claudeRmPrompt)
	e.h.onKeys = resolveOn("2")
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
		t.Fatalf("tick 1 = %+v", o)
	}
	e.clock.Add(10 * time.Minute) // well past the prompt grace
	o := e.observe()[RoleClaude]
	if o.Kind != "" || o.Status != herdr.StatusWorking || o.Run == nil || o.Run.ID != run {
		t.Fatalf("tick 2 = %+v, want working on the run", o)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		t.Fatalf("human_active_at = %v after a deny", pr.HumanActiveAt)
	}
	if r := e.run1(run); r.State != store.RunWorking {
		t.Fatalf("run = %+v", r)
	}
}

func TestObserveDeniesCodexApproval(t *testing.T) {
	cases := []struct {
		name, screen string
		want         []string
	}{
		{"command", codexCommandPrompt, []string{"3"}},
		{"edits", codexEditsPrompt, []string{"2"}},
		{"y/n", "  Allow command? (y/n)\n", []string{"n,enter"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			e.blockedOn(RoleJudge, tc.screen)
			e.h.onKeys = func(f *fakeHerdr, target string, keys []string) { f.reads[target] = codexIdleScreen }
			if o := e.observe()[RoleJudge]; o.Kind != ObsPromptDenied {
				t.Fatalf("judge = %+v", o)
			}
			var want []string
			for _, k := range tc.want {
				want = append(want, "mg-11920-codex-judge-5d01cf:"+k)
			}
			if got := e.keysSent(); !slices.Equal(got, want) {
				t.Fatalf("keys = %q, want %q", got, want)
			}
			if evs := e.promptEvents(EventPromptDenied); len(evs) != 1 || !strings.Contains(evs[0].Message, "codex permission prompt of codex-judge") {
				t.Fatalf("events = %+v", evs)
			}
		})
	}
}

func TestObservePermissionPromptLeftAlone(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *env)
		screen string
	}{
		{"kind waits", func(e *env) {
			e.setKind(KindClaude, func(k *config.Kind) { k.OnPermissionPrompt = config.PermissionWait })
		}, claudeRmPrompt},
		{"transcript quoting the question", nil, "⏺ Claude Code asks \"Do you want to proceed?\" here.\n  ⎿  1. Yes\n\n> \n"},
		{"trust dialog", nil, claudeTrustScreen},
		{"unreadable", func(e *env) {
			e.h.errs["AgentRead"] = &herdr.Error{Code: "boom"}
			e.h.errs["PaneRead"] = &herdr.Error{Code: "boom"}
		}, claudeRmPrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			e.blockedOn(RoleClaude, tc.screen)
			if tc.setup != nil {
				tc.setup(e)
			}
			if o := e.observe()[RoleClaude]; o.Kind != ObsBlocked {
				t.Fatalf("observation = %+v, want blocked", o)
			}
			if got := e.keysSent(); len(got) != 0 {
				t.Fatalf("keys = %q, want none", got)
			}
			if evs := e.promptEvents(EventPromptDenied); len(evs) != 0 {
				t.Fatalf("events = %+v", evs)
			}
		})
	}
}

func TestObserveDenyKeysFailing(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.blockedOn(RoleClaude, claudeRmPrompt)
	e.h.errs["AgentSendKeys"] = &herdr.Error{Code: "boom"}
	e.h.errs["PaneSendKeys"] = &herdr.Error{Code: "boom"}
	for tick := 1; tick <= 2; tick++ {
		if o := e.observe()[RoleClaude]; o.Kind != ObsBlocked {
			t.Fatalf("tick %d = %+v, want blocked: no key reached the agent", tick, o)
		}
	}
	if evs := e.promptEvents(EventPromptDenied); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
	delete(e.h.errs, "AgentSendKeys")
	e.h.onKeys = resolveOn("2")
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
		t.Fatalf("after recovery = %+v", o)
	}
	if evs := e.promptEvents(EventPromptDenied); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"count":1`) {
		t.Fatalf("events = %+v, want the first deny of the run", evs)
	}
}

func TestObservePermissionPromptWithoutRunIsLeftToTheHuman(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.h.setAgentStatus(claudeAgent, herdr.StatusBlocked)
	e.h.reads[claudeAgent] = claudeRmPrompt
	if o := e.observe()[RoleClaude]; o.Kind != ObsBlocked || o.Run != nil {
		t.Fatalf("observation = %+v, want blocked", o)
	}
	if got := e.keysSent(); len(got) != 0 {
		t.Fatalf("keys = %q, want none: no magnum run is in flight", got)
	}
}

func TestObserveSamePromptWaitsOneTick(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.blockedOn(RoleClaude, claudeRmPrompt)
	e.h.onKeys = func(*fakeHerdr, string, []string) {} // the screen does not change
	want := []string{claudeAgent + ":2", claudeAgent + ":esc"}
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied || !slices.Equal(e.keysSent(), want) {
		t.Fatalf("tick 1 = %+v, keys %q", o, e.keysSent())
	}
	// Still on screen right after the deny: wait a tick.
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied || len(e.keysSent()) != 2 {
		t.Fatalf("tick 2 = %+v, keys %q", o, e.keysSent())
	}
	// Still there a tick later: deny again.
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied || len(e.keysSent()) != 4 {
		t.Fatalf("tick 3 = %+v, keys %q", o, e.keysSent())
	}
	if n := len(e.promptEvents(EventPromptDenied)); n != 2 {
		t.Fatalf("denied events = %d, want 2", n)
	}
}

func TestObserveDenyCapPerRun(t *testing.T) {
	e := newEnv(t)
	e.started()
	run := e.blockedOn(RoleClaude, claudeRmPrompt)
	// The agent asks again at once, about another command each time.
	n := 0
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		n++
		f.reads[target] = strings.Replace(claudeRmPrompt, `"$S/run"`, `"$S/run`+strings.Repeat("x", n)+`"`, 1)
	}
	for tick := 1; tick <= MaxPromptDenies; tick++ {
		if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
			t.Fatalf("tick %d = %+v, want prompt_denied", tick, o)
		}
	}
	sent := len(e.keysSent())
	for tick := 1; tick <= 2; tick++ {
		if o := e.observe()[RoleClaude]; o.Kind != ObsBlocked {
			t.Fatalf("past the cap, tick %d = %+v, want blocked", tick, o)
		}
	}
	if got := len(e.keysSent()); got != sent {
		t.Fatalf("keys sent past the cap: %q", e.keysSent()[sent:])
	}
	if got := len(e.promptEvents(EventPromptDenied)); got != MaxPromptDenies {
		t.Fatalf("denied events = %d, want %d", got, MaxPromptDenies)
	}
	limit := e.promptEvents(EventPromptDeniedLimit)
	if len(limit) != 1 || limit[0].Level != "warn" || !strings.Contains(string(limit[0].Data), run) {
		t.Fatalf("limit events = %+v", limit)
	}
	// The human answers; the budget is spent, so no continuation either.
	e.h.onKeys = nil
	e.idleAfterDeny(claudeRejected)
	if o := e.observe()[RoleClaude]; o.Kind != "" || len(e.continuations()) != 0 {
		t.Fatalf("past the cap = %+v, continuations %d", o, len(e.continuations()))
	}

	// A new run gets a new budget.
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.observe()
	e.observe() // two idle ticks end the run
	e.blockedOn(RoleClaude, claudeRmPrompt)
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
		t.Fatalf("new run = %+v, want prompt_denied", o)
	}
}

// idleAfterDeny leaves claude-review idle on screen, as Claude Code is after
// a No ends its turn.
func (e *env) idleAfterDeny(screen string) {
	e.t.Helper()
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = screen
	e.h.mu.Unlock()
}

// continuations are the after-deny messages sent so far.
func (e *env) continuations() []promptCall {
	e.t.Helper()
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	var out []promptCall
	for _, p := range e.h.prompts {
		if p.Text == config.DefaultAfterDenyPrompt {
			out = append(out, p)
		}
	}
	return out
}

// denyThenIdle denies the rm prompt on claude-review's run; the agent ends
// its turn (idle, Claude's rejected screen). It returns the run id.
func (e *env) denyThenIdle() string {
	e.t.Helper()
	run := e.blockedOn(RoleClaude, claudeRmPrompt)
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		f.reads[target] = claudeRejected
		for i := range f.agents {
			if f.agents[i].Name == target {
				f.agents[i].AgentStatus = herdr.StatusIdle
			}
		}
	}
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
		e.t.Fatalf("deny tick = %+v", o)
	}
	e.h.onKeys = nil
	return run
}

func TestObserveContinuesOnceAfterDeny(t *testing.T) {
	e := newEnv(t)
	e.started()
	run := e.denyThenIdle()

	e.clock.Add(30 * time.Second)
	o := e.observe()[RoleClaude]
	if o.Kind != ObsDenyContinued || o.Run == nil || o.Run.ID != run || o.Session.IdleTicks != 0 || o.Session.LastPromptAt == nil {
		t.Fatalf("idle tick = %+v, want deny_continued", o)
	}
	c := e.continuations()
	if len(c) != 1 || c[0].Target != claudeAgent || c[0].Wait != nil {
		t.Fatalf("continuations = %+v", c)
	}
	if r := e.run1(run); r.State == store.RunEnded || r.EndedAt != nil {
		t.Fatalf("run = %+v, want still in flight", r)
	}
	evs := e.promptEvents(EventDenyContinued)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"count":2`) || !strings.Contains(string(evs[0].Data), run) {
		t.Fatalf("events = %+v, want one continuation counted after the deny", evs)
	}

	// The agent works on, then finishes: the run ends normally, no second message.
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != "" || o.Status != herdr.StatusWorking {
		t.Fatalf("working tick = %+v", o)
	}
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	e.observe()
	if o := e.observe()[RoleClaude]; o.Kind != ObsCompleted {
		t.Fatalf("finish = %+v, want completed", o)
	}
	if n := len(e.continuations()); n != 1 {
		t.Fatalf("continuations = %d, want 1", n)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		t.Fatalf("human_active_at = %v", pr.HumanActiveAt)
	}
}

func TestObserveNoContinuationAfterRunEnded(t *testing.T) {
	e := newEnv(t)
	e.started()
	run := e.denyThenIdle()
	if err := e.st.TransitionRun(e.ctx, run, []string{store.RunSubmitted, store.RunWorking}, store.RunEnded, func(u *store.RunUpdate) {
		u.Set("ended_at", e.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if o := e.observe()[RoleClaude]; o.Kind != "" {
			t.Fatalf("observation = %+v, want nothing", o)
		}
	}
	if c := e.continuations(); len(c) != 0 {
		t.Fatalf("continuations = %+v, want none for an ended run", c)
	}
	if evs := e.promptEvents(EventDenyContinued); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

func TestObserveNoContinuationInWaitMode(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.setKind(KindClaude, func(k *config.Kind) { k.OnPermissionPrompt = config.PermissionWait })
	e.blockedOn(RoleClaude, claudeRmPrompt)
	if o := e.observe()[RoleClaude]; o.Kind != ObsBlocked {
		t.Fatalf("blocked tick = %+v", o)
	}
	e.idleAfterDeny(claudeRejected) // the human answered No
	for range 2 {
		e.observe()
	}
	if c := e.continuations(); len(c) != 0 || len(e.keysSent()) != 0 {
		t.Fatalf("continuations = %+v, keys %q; want none in wait mode", c, e.keysSent())
	}
}

func TestObserveNoContinuationWhenNotNeeded(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
	}{
		{"agent carried on by itself", func(e *env) {
			e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
			e.observe()
			e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
		}},
		{"text in the composer", func(e *env) { e.idleAfterDeny(strings.Replace(claudeRejected, "│ >    │", "│ > fix t│", 1)) }},
		{"after_deny_prompt off", func(e *env) { e.setKind(KindClaude, func(k *config.Kind) { k.AfterDenyPrompt = "" }) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			run := e.denyThenIdle()
			tc.setup(e)
			e.observe()
			if o := e.observe()[RoleClaude]; o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != run {
				t.Fatalf("second idle tick = %+v, want the run completed", o)
			}
			if c := e.continuations(); len(c) != 0 {
				t.Fatalf("continuations = %+v, want none", c)
			}
		})
	}
}

func TestComposerEmpty(t *testing.T) {
	cases := map[string]bool{
		claudeRejected:   true,
		claudeIdleScreen: true,
		codexIdleScreen:  true,
		"──────\n❯ \n──────\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n": true,
		"> Try \"refactor app/models/user.rb\"\n":                               true,
		"╭──────╮\n│ > fix the│\n╰──────╯\n":                                    false,
		"› run the migrations\n\n  100% context left\n":                         false,
		"⏺ Done.\n":    false,
		claudeRmPrompt: false,
		"":             false,
	}
	for text, want := range cases {
		if got := composerEmpty(text); got != want {
			t.Errorf("composerEmpty(%q) = %v, want %v", text, got, want)
		}
	}
}

// rejected is what Claude Code writes when a tool use is denied: the error
// result, the interruption line and the end of the turn, all stamped at
// once.
func rejected(at time.Time, toolUseID string) []tline {
	r := toolResult(at, toolUseID, "The user doesn't want to proceed with this tool use. The tool use was rejected "+
		"(eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed.", nil, true)
	r["toolUseResult"], r["toolDenialKind"] = "User rejected tool use", "user-rejected"
	return []tline{r, interruption(at, "p1"), turnEnded(at)}
}

// forkedThenDenied starts the claude agent on a run whose turn forked
// /code-review into the background, then asks for an rm that magnum denies;
// herdr keeps showing the agent working (its fork runs). It returns the run
// id and when the deny was sent.
func (e *env) forkedThenDenied(tr *transcript) (run string, denied time.Time) {
	e.t.Helper()
	run = e.promptClaude()
	now := e.clock.Now()
	tr.add(humanPrompt(now, "/code-review https://github.com/talkable/talkable/pull/11920 high"))
	tr.addAll(skillForked(now.Add(5*time.Second), "toolu_skill1", "a2f00d"))
	tr.add(toolUse(now.Add(time.Minute), "toolu_rm", "Bash", map[string]any{"command": `rm -rf "$S/run"`}))
	e.clock.Add(time.Minute)
	e.h.setAgentStatus(claudeAgent, herdr.StatusBlocked)
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = claudeRmPrompt
	e.h.keys = nil
	e.h.mu.Unlock()
	e.h.onKeys = resolveOn("2")
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
		e.t.Fatalf("deny tick = %+v", o)
	}
	e.h.onKeys = nil
	return run, e.clock.Now()
}

// The live case (two rounds on 2026-10-08): the deny ends Claude
// Code's turn, but the /code-review fork it started keeps herdr showing the
// agent working, so the observer took the agent for one that carried on by
// itself and dropped the continuation; when the fork reported, the agent
// answered that it had stopped, and the run ended without a report. The
// transcript shows the turn cut by the rejection: the continuation is typed
// then, into the free composer, and the run goes on.
func TestObserveContinuesAfterADenyWhileTheAgentWaitsForItsBackgroundWork(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	run, denied := e.forkedThenDenied(tr)
	tr.addAll(rejected(denied, "toolu_rm"))

	e.clock.Add(30 * time.Second)
	o := e.observe()[RoleClaude]
	if o.Kind != ObsDenyContinued || o.Status != herdr.StatusWorking || o.Run == nil || o.Run.ID != run ||
		o.Session.IdleTicks != 0 || o.Session.LastPromptAt == nil {
		t.Fatalf("working tick after the deny = %+v, want deny_continued", o)
	}
	if c := e.continuations(); len(c) != 1 || c[0].Target != claudeAgent {
		t.Fatalf("continuations = %+v, want one to the agent", c)
	}
	if evs := e.promptEvents(EventDenyContinued); len(evs) != 1 || !strings.Contains(string(evs[0].Data), run) {
		t.Fatalf("events = %+v, want one continuation", evs)
	}
	if r := e.run1(run); r.State != store.RunWorking {
		t.Fatalf("run = %+v, want still working", r)
	}
	if pr := e.reloadPR(); pr.HumanActiveAt != nil {
		t.Fatalf("human_active_at = %v after a deny", pr.HumanActiveAt)
	}

	// The agent takes the message and works on; its fork reports; the run
	// ends as before, with no second message.
	tr.add(humanPrompt(e.clock.Now(), config.DefaultAfterDenyPrompt))
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != "" || o.Status != herdr.StatusWorking {
		t.Fatalf("working tick = %+v", o)
	}
	done := e.clock.Now()
	tr.addAll(notification(done, "toolu_skill1", "a2f00d", "completed"))
	tr.add(assistantSays(done.Add(2*time.Second), "Report written."), turnEnded(done.Add(3*time.Second)))
	if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != run {
		t.Fatalf("finish = %+v, want completed", o)
	}
	if n := len(e.continuations()); n != 1 {
		t.Fatalf("continuations = %d, want 1", n)
	}
}

// A deny stays pending while a claude agent works after it: before its
// transcript shows the turn cut (the file lags the screen), while text sits
// in its composer, and while it answers its fork's notification. The
// continuation goes when the composer is free, here once the agent is idle.
func TestADenyStaysPendingWhileAClaudeAgentWorksAfterIt(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	run, denied := e.forkedThenDenied(tr)

	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != "" || o.Status != herdr.StatusWorking || len(e.continuations()) != 0 {
		t.Fatalf("working tick before the transcript caught up = %+v, continuations %d", o, len(e.continuations()))
	}

	tr.addAll(rejected(denied, "toolu_rm"))
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = strings.Replace(claudeRejected, "│ >    │", "│ > finish the review without it │", 1)
	e.h.mu.Unlock()
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != "" || len(e.continuations()) != 0 {
		t.Fatalf("working tick with text in the composer = %+v, continuations %d", o, len(e.continuations()))
	}

	done := e.clock.Now()
	tr.addAll(notification(done, "toolu_skill1", "a2f00d", "completed"))
	tr.add(assistantSays(done.Add(2*time.Second), "I stopped when you rejected the command, and I have run nothing since."), turnEnded(done.Add(3*time.Second)))
	e.clock.Add(30 * time.Second)
	if o := e.observe()[RoleClaude]; o.Kind != "" || len(e.continuations()) != 0 {
		t.Fatalf("working tick while the agent answers its notification = %+v, continuations %d", o, len(e.continuations()))
	}
	e.idleAfterDeny(claudeRejected)
	e.clock.Add(30 * time.Second)
	o := e.observe()[RoleClaude]
	if o.Kind != ObsDenyContinued || o.Run == nil || o.Run.ID != run || len(e.continuations()) != 1 {
		t.Fatalf("idle tick = %+v, continuations %d, want deny_continued", o, len(e.continuations()))
	}
	if r := e.run1(run); r.State == store.RunEnded || r.EndedAt != nil {
		t.Fatalf("run = %+v, want still in flight", r)
	}
}
