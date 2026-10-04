package agents

import (
	"strings"
	"testing"

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
	if d, ok := detectHooksDialog(codexHooksScreen); !ok || d.cursor != 0 || d.decline != 2 {
		t.Fatalf("hooks review: %+v %v", d, ok)
	}
	if d, ok := detectHooksDialog(hooksCursorAt(2)); !ok || d.cursor != 2 {
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

// A Codex resumed in a checkout whose hooks changed shows its hooks review;
// Submit declines it (never trusts: the PR controls the hooks), confirms the
// cursor reached "Continue without trusting", and only then sends the
// prompt, once.
func TestSubmitDeclinesTheCodexHooksReviewFirst(t *testing.T) {
	e := newEnv(t)
	e.started()
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
			case k == "enter" && d.cursor != d.decline:
				panic("enter pressed off the decline option")
			case k == "enter":
				f.reads[target] = codexIdleScreen
			}
		}
	}
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "re-review")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := strings.Join(e.keysSent(), " "); got != name+":down "+name+":down "+name+":enter" {
		t.Fatalf("keys = %s", got)
	}
	if n := len(e.h.prompts); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %+v", r)
	}
	all, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	var evs []store.Event
	for _, ev := range all {
		if ev.Kind == EventHooksDeclined {
			evs = append(evs, ev)
		}
	}
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Message, "continued without trusting") {
		t.Fatalf("events %+v %v", evs, err)
	}
}

// A cursor that does not move is never confirmed: no Enter on "Review hooks"
// or "Trust all and continue".
func TestDeclineHooksNeverConfirmsAnotherOption(t *testing.T) {
	e := newEnv(t)
	e.started()
	name := "mg-11920-codex-judge-5d01cf"
	e.h.mu.Lock()
	e.h.reads[name] = codexHooksScreen // keys change nothing on this screen
	e.h.mu.Unlock()
	ok, err := e.m.declineHooks(e.ctx, e.pr.ID, RoleJudge, paneRef{name: name})
	if ok || err == nil || !strings.Contains(err.Error(), "not confirming") {
		t.Fatalf("declined %v, err %v", ok, err)
	}
	for _, k := range e.keysSent() {
		if strings.HasSuffix(k, ":enter") {
			t.Fatalf("pressed %v", e.keysSent())
		}
	}
}
