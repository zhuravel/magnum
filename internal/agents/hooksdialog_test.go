package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Codex 0.160's startup hooks review, as it renders.
const codexHooksScreen = `
  Hooks need review

  3 hooks are new or changed.
  Hooks can run outside the sandbox after you trust them.

› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)

  enter confirm · esc cancel
`

func hooksCursorAt(n int) string {
	s := strings.Replace(codexHooksScreen, "› 1. Review hooks", "  1. Review hooks", 1)
	opts := []string{"  1. Review hooks", "  2. Trust all and continue", "  3. Continue without trusting (hooks won't run)"}
	return strings.Replace(s, opts[n], "›"+opts[n][1:], 1)
}

func TestDetectHooksDialog(t *testing.T) {
	if d, ok := detectHooksDialog(codexHooksScreen); !ok || d.cursor != 0 {
		t.Fatalf("hooks review: %+v %v", d, ok)
	}
	if d, ok := detectHooksDialog(hooksCursorAt(hooksDeclineOption)); !ok || d.cursor != hooksDeclineOption {
		t.Fatalf("cursor on decline: %+v %v", d, ok)
	}
	for name, text := range map[string]string{
		"folder trust":   codexTrustScreen,
		"idle":           codexIdleScreen,
		"quoted":         "the agent said: Hooks need review, then Trust all and continue",
		"no cursor":      strings.Replace(codexHooksScreen, "› 1.", "  1.", 1),
		"missing option": strings.Replace(codexHooksScreen, "  2. Trust all and continue\n", "", 1),
	} {
		if _, ok := detectHooksDialog(text); ok {
			t.Errorf("%s: detected a hooks review", name)
		}
	}
}

// A hooks review is live only at the bottom of the screen: its options are
// the last lines but for blanks and key hints. Dialog text an agent's output
// left above a permission prompt or the composer is never taken for it, so
// no Enter lands on "Yes, proceed" and no arrow goes into the composer.
func TestDetectHooksDialogOnlyAtTheBottom(t *testing.T) {
	pressHint := strings.Replace(codexHooksScreen, "enter confirm · esc cancel", "Press enter to confirm or esc to go back", 1)
	if _, ok := detectHooksDialog(pressHint); !ok {
		t.Fatal("a hooks review with a press-enter hint was not detected")
	}
	for name, text := range map[string]string{
		"above a command prompt":                  codexHooksScreen + codexCommandPrompt,
		"cursor on the choice above edits prompt": hooksCursorAt(hooksTrustOption) + codexEditsPrompt,
		"above the composer":                      codexHooksScreen + codexIdleScreen,
		"output below":                            codexHooksScreen + "• Running the specs.\n",
	} {
		if d, ok := detectHooksDialog(text); ok {
			t.Errorf("%s: detected a hooks review %+v", name, d)
		}
	}
}

// A stale hooks review above a live permission prompt or the composer never
// yields a key: no Enter when the cursor already sits on magnum's choice, no
// arrow when it does not.
func TestAnswerHooksIgnoresStaleDialogAboveAnotherScreen(t *testing.T) {
	for name, screen := range map[string]string{
		"cursor on the choice above a prompt": hooksCursorAt(hooksTrustOption) + codexCommandPrompt,
		"cursor elsewhere above a prompt":     codexHooksScreen + codexCommandPrompt,
		"above the composer":                  codexHooksScreen + codexIdleScreen,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			agent := "mg-11920-codex-judge-5d01cf"
			e.h.mu.Lock()
			e.h.reads[agent] = screen
			e.h.mu.Unlock()
			ok, err := e.m.answerHooks(e.ctx, e.pr.ID, RoleJudge, KindCodex, paneRef{name: agent}, t.TempDir())
			if ok || err != nil {
				t.Fatalf("answered %v, err %v", ok, err)
			}
			if keys := e.keysSent(); len(keys) != 0 {
				t.Fatalf("keys sent: %v", keys)
			}
		})
	}
}

// The screen is read once more right before Enter, which goes only to the
// same dialog with the cursor still on magnum's choice: a permission prompt
// (or another hooks review) that replaced it after the first read gets no
// key at all.
func TestAnswerHooksRereadsBeforeEnter(t *testing.T) {
	other := strings.Replace(hooksCursorAt(hooksTrustOption), "3 hooks are new or changed.", "4 hooks are new or changed.", 1)
	for name, next := range map[string]string{
		"permission prompt":    codexCommandPrompt,
		"another hooks review": other,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			agent := "mg-11920-codex-judge-5d01cf"
			reads := 0
			e.h.mu.Lock()
			e.h.reads[agent] = hooksCursorAt(hooksTrustOption)
			e.h.onRead = func(f *fakeHerdr, target string) {
				if reads++; reads == 1 {
					f.reads[target] = next
				}
			}
			e.h.mu.Unlock()
			ok, err := e.m.answerHooks(e.ctx, e.pr.ID, RoleJudge, KindCodex, paneRef{name: agent}, t.TempDir())
			if ok || err == nil || !strings.Contains(err.Error(), "not confirming") {
				t.Fatalf("answered %v, err %v", ok, err)
			}
			if reads < 2 {
				t.Fatalf("reads = %d, want a re-read before Enter", reads)
			}
			if keys := e.keysSent(); len(keys) != 0 {
				t.Fatalf("keys sent: %v", keys)
			}
		})
	}
}

// writeFiles creates files (path relative to root → content) under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A checkout declares hooks of its own through .codex/hooks.json or hooks
// (or plugins) in .codex/config.toml, in any project layer up to the
// repository root; MCP servers alone, a hooks log directory or a .codex above
// the repository are not the checkout's hooks. What magnum cannot read is
// never taken for "no hooks".
func TestCheckoutHooks(t *testing.T) {
	mcpOnly := "[mcp_servers.github]\nurl = \"https://example.com/mcp\"\n"
	for name, tc := range map[string]struct {
		files   map[string]string
		root    string // the repository root, relative to the temp dir
		sub     string // the checkout, relative to the repository root
		want    string // the declaring file, relative to the repository root
		wantErr string
	}{
		"none":                 {files: map[string]string{"README.md": "x"}},
		"mcp servers only":     {files: map[string]string{".codex/config.toml": mcpOnly, ".codex/hooks/log/tool_use.log": "x"}},
		"hooks.json":           {files: map[string]string{".codex/hooks.json": "{}"}, want: ".codex/hooks.json"},
		"hooks table":          {files: map[string]string{".codex/config.toml": mcpOnly + "[[hooks.stop]]\ncommand = \"x\"\n"}, want: ".codex/config.toml"},
		"hooks feature":        {files: map[string]string{".codex/config.toml": "[features]\nhooks = true\n"}, want: ".codex/config.toml"},
		"plugins":              {files: map[string]string{".codex/config.toml": "[plugins.\"x@y\"]\nenabled = true\n"}, want: ".codex/config.toml"},
		"in a parent layer":    {files: map[string]string{".codex/hooks.json": "{}", "app/x.rb": ""}, sub: "app", want: ".codex/hooks.json"},
		"invalid config":       {files: map[string]string{".codex/config.toml": "hooks = [\n"}, wantErr: "is not valid TOML"},
		"missing checkout":     {sub: "gone", wantErr: "is not a directory"},
		"above the repository": {files: map[string]string{"x/.codex/hooks.json": "{}"}, root: "x/repo"},
	} {
		t.Run(name, func(t *testing.T) {
			top := t.TempDir()
			root := filepath.Join(top, tc.root)
			writeFiles(t, top, tc.files)
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, tc.sub)
			got, err := checkoutHooks(dir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %q, %v; want error %q", got, err, tc.wantErr)
				}
				return
			}
			want := ""
			if tc.want != "" {
				want = filepath.Join(root, tc.want)
			}
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
	for _, dir := range []string{"", "relative/path"} {
		if _, err := checkoutHooks(dir); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("%q: %v", dir, err)
		}
	}
}

// hooksEnv starts the judge in a checkout made of files and shows the hooks
// review on its screen; Enter on any option clears it.
func hooksEnv(t *testing.T, files map[string]string) (*env, string) {
	t.Helper()
	e := newEnv(t)
	dir := t.TempDir()
	writeFiles(t, dir, files)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, dir, map[string]string{"WT_BRANCH": "review1"}, "talkable#11920", e.coreRoles())
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	name := "mg-11920-codex-judge-5d01cf"
	e.h.mu.Lock()
	e.h.reads[name] = codexHooksScreen
	e.h.mu.Unlock()
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		for _, k := range keys {
			d, ok := detectHooksDialog(f.reads[target])
			switch {
			case !ok:
			case k == "down":
				f.reads[target] = hooksCursorAt(d.cursor + 1)
			case k == "enter":
				f.reads[target] = codexIdleScreen
			}
		}
	}
	return e, name
}

func hooksEvents(t *testing.T, e *env) []store.Event {
	t.Helper()
	all, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range all {
		if ev.Kind == EventHooksTrusted || ev.Kind == EventHooksDeclined {
			out = append(out, ev)
		}
	}
	return out
}

// A Codex resumed after the user's own hooks changed (a tool rewrote
// ~/.codex/hooks.json) shows its hooks review; the checkout declares no hooks,
// so Submit trusts them, confirms the cursor reached "Trust all and
// continue", and only then sends the prompt, once.
func TestSubmitTrustsHooksWhenTheCheckoutHasNone(t *testing.T) {
	e, name := hooksEnv(t, map[string]string{".codex/config.toml": "[mcp_servers.github]\nurl = \"https://example.com/mcp\"\n"})
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "re-review")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := strings.Join(e.keysSent(), " "); got != name+":down "+name+":enter" {
		t.Fatalf("keys = %s", got)
	}
	if n := len(e.h.prompts); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %+v", r)
	}
	if evs := hooksEvents(t, e); len(evs) != 1 || evs[0].Kind != EventHooksTrusted || !strings.Contains(evs[0].Message, "declares no hooks") {
		t.Fatalf("events %+v", evs)
	}
}

// Hooks a PR could have added are never trusted: with .codex/hooks.json in
// the checkout, or on_hooks_review = "decline", Submit picks "Continue
// without trusting" and says why.
func TestSubmitDeclinesHooksTheCheckoutDeclares(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		kind  func(*config.Kind)
		why   string
	}{
		"checkout hooks": {files: map[string]string{".codex/hooks.json": "{}"}, why: "the checkout declares hooks in"},
		"decline":        {kind: func(k *config.Kind) { k.OnHooksReview = config.HooksDecline }, why: "on_hooks_review is not trust_own"},
	} {
		t.Run(name, func(t *testing.T) {
			e, agent := hooksEnv(t, tc.files)
			if tc.kind != nil {
				e.setKind(KindCodex, tc.kind)
			}
			if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "re-review"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if got := strings.Join(e.keysSent(), " "); got != agent+":down "+agent+":down "+agent+":enter" {
				t.Fatalf("keys = %s", got)
			}
			evs := hooksEvents(t, e)
			if len(evs) != 1 || evs[0].Kind != EventHooksDeclined || !strings.Contains(evs[0].Message, tc.why) ||
				!strings.Contains(evs[0].Message, "runs without the untrusted hooks") {
				t.Fatalf("events %+v", evs)
			}
		})
	}
}

// A cursor that does not move is never confirmed: no Enter on "Review hooks"
// or on an option magnum did not pick.
func TestAnswerHooksNeverConfirmsAnotherOption(t *testing.T) {
	e := newEnv(t)
	e.started()
	name := "mg-11920-codex-judge-5d01cf"
	e.h.mu.Lock()
	e.h.reads[name] = codexHooksScreen // keys change nothing on this screen
	e.h.mu.Unlock()
	ok, err := e.m.answerHooks(e.ctx, e.pr.ID, RoleJudge, KindCodex, paneRef{name: name}, t.TempDir())
	if ok || err == nil || !strings.Contains(err.Error(), "not confirming") {
		t.Fatalf("answered %v, err %v", ok, err)
	}
	for _, k := range e.keysSent() {
		if strings.HasSuffix(k, ":enter") {
			t.Fatalf("pressed %v", e.keysSent())
		}
	}
}

// herdr rejects a prompt to an agent it has not registered yet (a resumed
// Claude Code takes a moment to show up as a named agent): agent_not_ready
// means nothing was sent, so Submit waits until herdr lists the agent idle
// and sends once more.
func TestSubmitWaitsForAnAgentHerdrHasNotRegisteredYet(t *testing.T) {
	e := newEnv(t)
	e.started()
	calls := 0
	e.h.onPrompt = func() {
		calls++
		if e.h.errs == nil {
			e.h.errs = map[string]error{}
		}
		if calls == 1 {
			e.h.errs["AgentPrompt"] = &herdr.Error{Method: "agent.prompt", Code: "agent_not_ready", Message: "agent is not an active named agent"}
		} else {
			delete(e.h.errs, "AgentPrompt")
		}
	}
	id, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunRereview, "re-review")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if n := len(e.h.prompts); n != 2 {
		t.Fatalf("prompts = %d, want 2 (the rejected one, then the resend)", n)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %+v", r)
	}
}
