package agents

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

func (e *env) trustUnsavedEvents() []store.Event {
	e.t.Helper()
	all, err := e.st.EventsBySubject(e.ctx, "tool:"+KindCodex, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(ev store.Event) bool { return ev.Kind != EventHooksTrustUnsaved })
}

// codexHomeFiles writes the Codex home's config.toml (an hour old) and
// hooks.json beside e.codexConfig.
func (e *env) codexHomeFiles(hooks string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.codexConfig), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.codexConfig, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(e.codexConfig, old, old); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(e.codexConfig), "hooks.json"), []byte(hooks), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// Codex saves a hook trust in its config.toml. A "Trust all and continue"
// that leaves the file as it was gives one warning and one toast, and a
// later session (after a daemon restart, here a new manager on the same
// registry) trusting the same hooks.json gives no second one; a changed
// hooks.json is told about again.
func TestAHookTrustCodexDidNotSaveIsToldOncePerHooksJSON(t *testing.T) {
	e, name := hooksEnv(t, nil)
	e.codexHomeFiles(`{"hooks":{}}`)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := e.trustUnsavedEvents()
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "Codex did not save the hook trust") ||
		!strings.Contains(evs[0].Message, "Run `codex` once in a terminal and choose Trust all.") {
		t.Fatalf("events %+v, want one warning", evs)
	}
	toasts := e.toasts.all()
	if len(toasts) != 1 || !strings.Contains(toasts[0], "|magnum: Codex did not save the hook trust|Every new Codex session will ask again.") {
		t.Fatalf("toasts %q, want one", toasts)
	}

	again := func(m *Manager) {
		t.Helper()
		e.h.mu.Lock()
		e.h.reads[name] = codexHooksScreen
		e.h.mu.Unlock()
		ok, err := m.answerHooks(e.ctx, e.pr.ID, RoleJudge, KindCodex, paneRef{name: name}, store.Deref(e.session(RoleJudge).Cwd))
		if !ok || err != nil {
			t.Fatalf("answerHooks = %v, %v", ok, err)
		}
	}
	restarted := New(e.m.d)
	restarted.sleep = e.m.sleep
	again(restarted)
	if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 1 || len(toasts) != 1 {
		t.Fatalf("the same hooks.json again: %d events, %d toasts, want no second warning", len(evs), len(toasts))
	}

	if err := os.WriteFile(filepath.Join(filepath.Dir(e.codexConfig), "hooks.json"), []byte(`{"hooks":{"Stop":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	again(restarted)
	if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 2 || len(toasts) != 2 {
		t.Fatalf("a changed hooks.json: %d events, %d toasts, want a second warning", len(evs), len(toasts))
	}
}

// A trust Codex saved (its config.toml written as the answer went in) and
// a declined review give no warning.
func TestASavedOrDeclinedHookTrustGivesNoWarning(t *testing.T) {
	t.Run("saved", func(t *testing.T) {
		e, _ := hooksEnv(t, nil)
		e.codexHomeFiles(`{"hooks":{}}`)
		answer := e.h.onKeys
		e.h.mu.Lock()
		e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
			answer(f, target, keys)
			if slices.Contains(keys, "enter") {
				if err := os.WriteFile(e.codexConfig, []byte("model = \"gpt-5\"\n[hooks.state]\n"), 0o600); err != nil {
					t.Error(err)
				}
			}
		}
		e.h.mu.Unlock()
		if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 0 || len(toasts) != 0 {
			t.Fatalf("events %+v, toasts %q", evs, toasts)
		}
	})
	t.Run("declined", func(t *testing.T) {
		e, _ := hooksEnv(t, map[string]string{".codex/hooks.json": "{}"})
		e.codexHomeFiles(`{"hooks":{}}`)
		if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 0 || len(toasts) != 0 {
			t.Fatalf("events %+v, toasts %q", evs, toasts)
		}
	})
}
