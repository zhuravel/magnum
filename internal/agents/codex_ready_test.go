package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// clockedSleep makes the manager's sleeps move the env's clock, so a fake
// screen can change at a given time.
func (e *env) clockedSleep() {
	e.m.sleep = func(ctx context.Context, d time.Duration) error {
		e.slept = append(e.slept, d)
		e.clock.Add(d)
		return ctx.Err()
	}
}

var codexStalled = &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentPromptStalled,
	Message: "agent prompt produced no observed working or blocked state within 5000 ms; current status is idle"}

// codexFirstPrompt is hooksEnv with the judge's composer on screen and the
// manager's sleeps moving the clock: the judge, never prompted, in a
// checkout that declares no hooks.
func codexFirstPrompt(t *testing.T) (*env, string) {
	t.Helper()
	e, name := hooksEnv(t, nil)
	e.clockedSleep()
	e.h.mu.Lock()
	e.h.reads[name] = codexIdleScreen
	e.h.mu.Unlock()
	return e, name
}

func (e *env) retriedEvents() []store.Event {
	e.t.Helper()
	all, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(ev store.Event) bool { return ev.Kind != EventPromptRetried })
}

// Codex may draw its composer and show its hooks review a moment later
// (here 2 s): the first prompt waits until the composer has shown with no
// dialog for CodexPromptSettle, so the review that comes meanwhile is
// answered first, the wait starts again, and the prompt goes out once,
// after it.
func TestTheFirstCodexPromptWaitsOutAHooksReviewThatFollowsItsComposer(t *testing.T) {
	e, name := codexFirstPrompt(t)
	shown := e.clock.Now()
	var answeredAt, promptedAt time.Time
	e.h.mu.Lock()
	e.h.onRead = func(f *fakeHerdr, target string) {
		if target == name && answeredAt.IsZero() && !e.clock.Now().Before(shown.Add(2*time.Second)) {
			f.reads[target] = codexHooksScreen
		}
	}
	answer := e.h.onKeys
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		answer(f, target, keys)
		if slices.Contains(keys, "enter") {
			answeredAt = e.clock.Now()
		}
	}
	e.h.onPrompt = func() { promptedAt = e.clock.Now() }
	e.h.mu.Unlock()

	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := strings.Join(e.keysSent(), " "); got != name+":down "+name+":enter" {
		t.Fatalf("keys = %s, want the hooks review trusted", got)
	}
	if n := len(e.h.prompts); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	if answeredAt.IsZero() || promptedAt.Before(answeredAt.Add(CodexPromptSettle)) {
		t.Fatalf("answered at %s, prompted at %s: want the prompt %s after the answer at least", answeredAt, promptedAt, CodexPromptSettle)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %+v", r)
	}
	if evs := hooksEvents(t, e); len(evs) != 1 || evs[0].Kind != EventHooksTrusted {
		t.Fatalf("hooks events %+v", evs)
	}
	if evs := e.retriedEvents(); len(evs) != 0 {
		t.Fatalf("retried %+v", evs)
	}
}

// A screen with the composer and no dialog gets the first prompt after
// CodexPromptSettle, once, with no key and no retry.
func TestAFirstCodexPromptWithNoDialogGoesOutAfterTheSettle(t *testing.T) {
	e, _ := codexFirstPrompt(t)
	start := e.clock.Now()
	var promptedAt time.Time
	e.h.mu.Lock()
	e.h.onPrompt = func() { promptedAt = e.clock.Now() }
	e.h.mu.Unlock()
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := promptedAt.Sub(start); got != CodexPromptSettle {
		t.Fatalf("prompted %s after the composer showed, want %s", got, CodexPromptSettle)
	}
	if n := len(e.h.prompts); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	if keys := e.keysSent(); len(keys) != 0 {
		t.Fatalf("keys %v", keys)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %+v", r)
	}
	if evs := e.retriedEvents(); len(evs) != 0 {
		t.Fatalf("retried %+v", evs)
	}
	// A later prompt to the session does not wait for the composer.
	slept := len(e.slept)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunContinue, "go on"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if len(e.slept) != slept {
		t.Fatalf("the second prompt slept %v", e.slept[slept:])
	}
}

// A hooks review that came with the first prompt ate it: herdr saw no turn
// (agent_prompt_stalled) and the screen shows the review. magnum answers
// it, records one retry and sends the prompt again; the run goes on.
func TestAFirstCodexPromptADialogAteIsSentAgainAfterTheAnswer(t *testing.T) {
	e, name := codexFirstPrompt(t)
	var enterAt int // index in the calls of the Enter that answered the review
	e.h.mu.Lock()
	e.h.onPrompt = func() {
		if len(e.h.prompts) == 1 {
			e.h.reads[name] = codexHooksScreen
			e.h.errs["AgentPrompt"] = codexStalled
			return
		}
		delete(e.h.errs, "AgentPrompt")
	}
	answer := e.h.onKeys
	e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
		answer(f, target, keys)
		if slices.Contains(keys, "enter") {
			enterAt = len(f.calls)
		}
	}
	e.h.mu.Unlock()

	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if n := len(e.h.prompts); n != 2 {
		t.Fatalf("prompts = %d, want the eaten one and one more", n)
	}
	if last := slices.IndexFunc(e.h.calls[enterAt:], func(c string) bool { return strings.HasPrefix(c, "AgentPrompt") }); enterAt == 0 || last < 0 {
		t.Fatalf("calls %v: want the second prompt after the answer", e.h.calls)
	}
	if got := strings.Join(e.keysSent(), " "); got != name+":down "+name+":enter" {
		t.Fatalf("keys = %s", got)
	}
	if r := e.run1(id); r.State != store.RunWorking || r.Error != nil {
		t.Fatalf("run = %+v, want working", r)
	}
	evs := e.retriedEvents()
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "the hooks review was on screen") {
		t.Fatalf("retried %+v, want one", evs)
	}
}

// A first prompt the dialog eats again after the retry fails the run with
// the reason, never "finished without writing ...".
func TestAFirstCodexPromptLostTwiceFailsTheRunWithTheReason(t *testing.T) {
	e, name := codexFirstPrompt(t)
	e.h.mu.Lock()
	e.h.onPrompt = func() {
		e.h.reads[name] = codexHooksScreen
		e.h.errs["AgentPrompt"] = codexStalled
	}
	e.h.mu.Unlock()

	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	const reason = "Codex did not take the prompt (a startup dialog ate it)"
	if !errors.Is(err, ErrNotTaken) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Prompt = %v, want ErrNotTaken", err)
	}
	if n := len(e.h.prompts); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
	r := e.run1(id)
	if r.State != store.RunFailed || store.Deref(r.Error) != reason {
		t.Fatalf("run = %s %q, want failed with %q", r.State, store.Deref(r.Error), reason)
	}
	if evs := e.retriedEvents(); len(evs) != 1 {
		t.Fatalf("retried %+v, want one", evs)
	}
}

// herdr not seeing a first prompt taken is no loss when Codex shows it
// took it: a free composer and no dialog, or a user message in its rollout
// since the run. The run is observed as before, never sent again.
func TestAStalledFirstCodexPromptCodexTookIsNotSentAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		screen  string
		rollout bool
	}{
		"free composer": {screen: codexIdleScreen},
		"rollout":       {screen: "› review\n\n  esc to interrupt\n", rollout: true},
	} {
		t.Run(name, func(t *testing.T) {
			e, agent := codexFirstPrompt(t)
			home := t.TempDir()
			e.m.d.CodexHome = home
			e.h.mu.Lock()
			e.h.onPrompt = func() {
				e.h.reads[agent] = tc.screen
				e.h.errs["AgentPrompt"] = codexStalled
				e.h.agents[slices.IndexFunc(e.h.agents, func(a herdr.AgentInfo) bool { return a.Name == agent })].AgentSession =
					&herdr.AgentSession{Kind: "id", Value: "01a0fe66-0000-7000-8000-000000000001"}
			}
			e.h.mu.Unlock()
			if tc.rollout {
				dir := filepath.Join(home, "sessions", "2026", "10", "03")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				line := `{"timestamp":"` + e.clock.Now().Add(CodexPromptSettle).UTC().Format(time.RFC3339Nano) +
					`","type":"event_msg","payload":{"type":"user_message","message":"review"}}` + "\n"
				if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-03T12-00-00-01a0fe66-0000-7000-8000-000000000001.jsonl"),
					[]byte(line), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
			if !errors.Is(err, ErrStalled) {
				t.Fatalf("Prompt = %v, want the stall as before", err)
			}
			if n := len(e.h.prompts); n != 1 {
				t.Fatalf("prompts = %d, want 1", n)
			}
			if r := e.run1(id); r.State != store.RunSubmitted {
				t.Fatalf("run = %+v, want submitted (observed)", r)
			}
			if evs := e.retriedEvents(); len(evs) != 0 {
				t.Fatalf("retried %+v", evs)
			}
		})
	}
}
