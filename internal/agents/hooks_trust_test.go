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

// moshiHooks is a hooks.json with one hook, moshi's PreToolUse hook at
// pre_tool_use:0:0.
const moshiHooks = `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook codex-hook"}]}]}}`

// codexHomeFiles writes the Codex home's config.toml (an hour old, no
// [hooks.state]) and hooks.json beside e.codexConfig.
func (e *env) codexHomeFiles(hooks string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.codexConfig), 0o700); err != nil {
		e.t.Fatal(err)
	}
	e.writeCodexConfig("model = \"gpt-5\"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(e.codexConfig, old, old); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.codexHooksFile(), []byte(hooks), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// codexHooksFile is the hooks.json beside e.codexConfig.
func (e *env) codexHooksFile() string {
	return filepath.Join(filepath.Dir(e.codexConfig), "hooks.json")
}

// writeCodexConfig writes e.codexConfig.
func (e *env) writeCodexConfig(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.codexConfig, []byte(content), 0o600); err != nil {
		e.t.Error(err)
	}
}

// trustOn makes the fake's Enter on a hooks review write config, as Codex
// writes its config.toml when it saves a trust ({hooks}: the hooks.json
// path).
func (e *env) trustOn(config string) {
	e.t.Helper()
	answer := e.h.onKeys
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		answer(f, target, keys)
		if slices.Contains(keys, "enter") {
			e.writeCodexConfig(strings.ReplaceAll(config, "{hooks}", e.codexHooksFile()))
		}
	}
}

// reviewAgain puts the hooks review back on the judge's screen and answers
// it with m.
func (e *env) reviewAgain(m *Manager, name string) {
	e.t.Helper()
	e.h.mu.Lock()
	e.h.reads[name] = codexHooksScreen
	e.h.mu.Unlock()
	ok, err := m.answerHooks(e.ctx, e.pr.ID, RoleJudge, KindCodex, paneRef{name: name}, store.Deref(e.session(RoleJudge).Cwd))
	if !ok || err != nil {
		e.t.Fatalf("answerHooks = %v, %v", ok, err)
	}
}

// Codex saves a hook trust in its config.toml's [hooks.state]. A "Trust all
// and continue" after which a hook the review could list (its place had no
// trust entry before Enter) still has none gives one warning and one toast
// naming it, and a later session (after a daemon restart, here a new
// manager on the same registry) trusting the same hooks.json gives no second
// one; a changed hooks.json is told about again.
func TestAHookTrustCodexDidNotSaveIsToldOncePerHooksJSON(t *testing.T) {
	e, name := hooksEnv(t, nil)
	e.codexHomeFiles(moshiHooks)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := e.trustUnsavedEvents()
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "Codex did not save the hook trust") ||
		!strings.Contains(evs[0].Message, "$CODEX_HOME/config.toml still has no trust entry for moshi-hook at pre_tool_use:0:0") ||
		!strings.Contains(evs[0].Message, "Run `codex` once in a terminal and choose Trust all.") {
		t.Fatalf("events %+v, want one warning", evs)
	}
	toasts := e.toasts.all()
	if len(toasts) != 1 || !strings.Contains(toasts[0], "|magnum: Codex did not save the hook trust|Every new Codex session will ask again.") {
		t.Fatalf("toasts %q, want one", toasts)
	}

	restarted := New(e.m.d)
	restarted.sleep = e.m.sleep
	e.reviewAgain(restarted, name)
	if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 1 || len(toasts) != 1 {
		t.Fatalf("the same hooks.json again: %d events, %d toasts, want no second warning", len(evs), len(toasts))
	}

	if err := os.WriteFile(e.codexHooksFile(), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook stop"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.reviewAgain(restarted, name)
	if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 2 || len(toasts) != 2 {
		t.Fatalf("a changed hooks.json: %d events, %d toasts, want a second warning", len(evs), len(toasts))
	}
}

// A trust Codex saved (its config.toml written as the answer went in) and
// a declined review give no warning.
func TestASavedOrDeclinedHookTrustGivesNoWarning(t *testing.T) {
	t.Run("saved", func(t *testing.T) {
		e, _ := hooksEnv(t, nil)
		e.codexHomeFiles(moshiHooks)
		e.trustOn("model = \"gpt-5\"\n[hooks.state.\"{hooks}:pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:aa\"\n")
		if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 0 || len(toasts) != 0 {
			t.Fatalf("events %+v, toasts %q", evs, toasts)
		}
	})
	t.Run("declined", func(t *testing.T) {
		e, _ := hooksEnv(t, map[string]string{".codex/hooks.json": "{}"})
		e.codexHomeFiles(moshiHooks)
		if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 0 || len(toasts) != 0 {
			t.Fatalf("events %+v, toasts %q", evs, toasts)
		}
	})
}

// Two sessions can show the same hooks review: the first answer saves the
// trust, and the second Enter writes nothing new, so config.toml's
// modification time stays. magnum used to read that as a trust Codex did
// not save and toasted although it was saved; it now reads the trust
// entries, and every hook has one. A hooks.json whose hooks all had trust
// entries before Enter (the review listed a hook changed in place, which
// magnum cannot see) says nothing either.
func TestASecondAnswerToAReviewTheFirstAnswerSavedGivesNoWarning(t *testing.T) {
	e, name := hooksEnv(t, nil)
	e.codexHomeFiles(moshiHooks)
	answer := e.h.onKeys // the fake's answer, which writes nothing
	e.trustOn("model = \"gpt-5\"\n[hooks.state.\"{hooks}:pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:aa\"\n")
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	e.h.mu.Lock()
	e.h.onKeys = answer // the second Enter writes nothing new
	e.h.mu.Unlock()
	saved, err := os.Stat(e.codexConfig)
	if err != nil {
		t.Fatal(err)
	}
	e.reviewAgain(e.m, name)
	if after, err := os.Stat(e.codexConfig); err != nil || !after.ModTime().Equal(saved.ModTime()) {
		t.Fatalf("config.toml changed on the second answer: %v", err)
	}
	if evs, toasts := e.trustUnsavedEvents(), e.toasts.all(); len(evs) != 0 || len(toasts) != 0 {
		t.Fatalf("a second answer to a saved trust warned: events %+v, toasts %q", evs, toasts)
	}
}

// Any other config.toml write between Enter and the check (a project's
// trust, a /model switch, another session's notice flag) used to hide a
// trust Codex did not save. Now an answer after which a hook from the
// review still has no trust entry warns whatever the file's modification
// time did.
func TestAnAnswerAfterWhichAHookStillHasNoTrustEntryWarnsWhateverTheModificationTime(t *testing.T) {
	e, _ := hooksEnv(t, nil)
	e.codexHomeFiles(moshiHooks)
	e.trustOn("model = \"gpt-5\"\n[projects.\"/tmp/x\"]\ntrust_level = \"trusted\"\n[hooks.state.\"{hooks}:stop:0:0\"]\ntrusted_hash = \"sha256:aa\"\n")
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if st, err := os.Stat(e.codexConfig); err != nil || time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("config.toml was not written as the answer went in: %v", err)
	}
	evs := e.trustUnsavedEvents()
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "still has no trust entry for moshi-hook at pre_tool_use:0:0") {
		t.Fatalf("events %+v, want one warning naming the hook", evs)
	}
	if !strings.Contains(string(evs[0].Data), `"places":["pre_tool_use:0:0"]`) {
		t.Errorf("data = %s, want the places", evs[0].Data)
	}
	if toasts := e.toasts.all(); len(toasts) != 1 {
		t.Fatalf("toasts %q, want one", toasts)
	}
}
