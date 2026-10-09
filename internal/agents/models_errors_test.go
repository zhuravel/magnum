package agents

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// failLimitReads makes the registry reads of the claude kind's limit of
// model fail, until the test ends.
func failLimitReads(t *testing.T, model string) {
	t.Helper()
	key := KVModelLimited(KindClaude, model)
	readKV = func(ctx context.Context, st *store.Store, k string) (string, bool, error) {
		if k == key {
			return "", false, errors.New("disk I/O error")
		}
		return st.GetKV(ctx, k)
	}
	t.Cleanup(func() { readKV = getKV })
}

// recordLimits records the claude kind's limits of models, each until until.
func (e *env) recordLimits(until time.Time, models ...string) {
	e.t.Helper()
	kv := map[string]string{KVModelLimits(KindClaude): strings.Join(models, ",")}
	for _, m := range models {
		kv[KVModelLimited(KindClaude, m)] = store.FormatTime(until)
	}
	for k, v := range kv {
		if err := e.st.SetKV(e.ctx, k, v); err != nil {
			e.t.Fatal(err)
		}
	}
}

// A limit that cannot be read is not a limit that ended: NoteModelLimit
// returns the error before it writes anything, so the index keeps every
// active limit and no row of one is deleted.
func TestNoteModelLimitWritesNothingWhenALimitCannotBeRead(t *testing.T) {
	storetest.Serial(t)
	e := newEnv(t)
	e.started()
	until := e.clock.Now().Add(time.Hour)
	e.recordLimits(until, "fable", "opus")
	failLimitReads(t, "fable")
	if _, err := e.m.NoteModelLimit(e.ctx, e.session(RoleClaude), Health{Kind: HealthModelLimit, Model: "sonnet"}); err == nil ||
		!strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("NoteModelLimit = %v, want the read's error", err)
	}
	readKV = getKV
	if v, _ := e.kv(KVModelLimits(KindClaude)); v != "fable,opus" {
		t.Fatalf("index = %q, want fable,opus kept", v)
	}
	if v, ok := e.kv(KVModelLimited(KindClaude, "fable")); !ok || v != store.FormatTime(until) {
		t.Fatalf("fable's limit = %q %v, want it kept", v, ok)
	}
	if _, ok := e.kv(KVModelLimited(KindClaude, "sonnet")); ok {
		t.Fatal("sonnet's limit was written from a partial view")
	}
}

// With a limit that cannot be read, no fallback is chosen and a prompt
// switches nothing: the session keeps its model, and the failure is logged.
func TestAnUnreadableLimitKeepsTheCurrentModel(t *testing.T) {
	storetest.Serial(t)
	e := newEnv(t)
	e.started()
	e.claudeScreen(false, "")
	e.recordLimits(e.clock.Now().Add(time.Hour), "fable")
	failLimitReads(t, "fable")
	s := e.session(RoleClaude)
	if m, ok := e.m.FallbackModel(e.ctx, s, nil); ok {
		t.Fatalf("fallback = %q, want none while the limits cannot be read", m)
	}
	e.m.ensureModel(e.ctx, s)
	if slices.ContainsFunc(e.h.runs, func(c paneRunCall) bool { return strings.HasPrefix(c.Command, "/model") }) {
		t.Fatalf("pane runs = %+v, want no switch", e.h.runs)
	}
	if !slices.ContainsFunc(e.logs.all(), func(l string) bool { return strings.Contains(l, "disk I/O error") }) {
		t.Fatalf("logs = %q, want the read failure", e.logs.all())
	}
}

// cancelOn returns a context the hook ends, and makes the manager's sleep
// honour it.
func (e *env) cancelOn() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(e.ctx)
	e.t.Cleanup(cancel)
	e.m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return ctx, cancel
}

// A switch whose context ends while magnum's own "Switch model?" dialog is
// open backs out of it (Esc): the session's next prompt is not refused as
// blocked.
func TestASwitchCutShortBacksOutOfItsDialog(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(true, "no")
	ctx, cancel := e.cancelOn()
	e.h.mu.Lock()
	run := e.h.onRun
	e.h.onRun = func(f *fakeHerdr, pane, cmd string) { run(f, pane, cmd); cancel() }
	e.h.mu.Unlock()
	s := e.session(RoleClaude)
	if err := e.m.SwitchModel(ctx, s, "opus", SwitchLimitHit); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if got := e.keysTo(claudeAgent); !slices.EqualFunc(got, [][]string{{"esc"}}, slices.Equal) {
		t.Fatalf("keys = %q, want one Esc out of the dialog", got)
	}
	if _, ok := e.kv(KVSessionModel(s.ID)); ok {
		t.Fatal("a switch backed out of records no model")
	}
}

// A switch whose context ends after the dialog was confirmed, with the
// status line already naming the new model, is recorded.
func TestASwitchCutShortAfterItTookIsRecorded(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.claudeScreen(true, "yes")
	ctx, cancel := e.cancelOn()
	e.h.mu.Lock()
	keys := e.h.onKeys
	e.h.onKeys = func(f *fakeHerdr, target string, k []string) { keys(f, target, k); cancel() }
	e.h.mu.Unlock()
	s := e.session(RoleClaude)
	if err := e.m.SwitchModel(ctx, s, "opus", SwitchLimitHit); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if got := e.keysTo(claudeAgent); !slices.EqualFunc(got, [][]string{{"enter"}}, slices.Equal) {
		t.Fatalf("keys = %q, want only the Enter", got)
	}
	if v, _ := e.kv(KVSessionModel(s.ID)); v != "opus" {
		t.Fatalf("session model = %q, want opus recorded", v)
	}
	if evs := e.eventsOf(EventModelSwitched); len(evs) != 1 {
		t.Fatalf("events = %+v, want the switch", evs)
	}
}
