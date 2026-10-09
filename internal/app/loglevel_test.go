package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// captureHandler keeps every record with its attributes.
type captureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// find returns the attributes of the first record with kind, and its level.
func (h *captureHandler) find(kind string) (slog.Level, map[string]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		attrs := map[string]string{"msg": r.Message}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		if attrs["kind"] == kind {
			return r.Level, attrs, true
		}
	}
	return 0, nil, false
}

// The layers' failures reach the daemon's log at their level through the
// App's wiring, with the subject and kind of the event they record and the
// layer as src: a round that ends in error at ERROR, a model limit and a
// skipped teardown at WARN. They used to arrive at INFO (pipeline, agents) or
// not at all (slots), so `magnum daemon --log-level warn` dropped them.
func TestTheLayersLogTheirFailuresAtTheirLevel(t *testing.T) {
	logs := &captureHandler{}
	a, _ := testApp(t, Options{Logger: slog.New(RedactHandler{Inner: logs}), Getenv: func(string) string { return "" }})
	ctx := context.Background()

	// The App identity has no private key, so the round ends in error.
	_, err := a.Pipeline["talkable-app"].RunRound(ctx, pipeline.RoundInput{
		PR:   store.PR{ID: 1, Number: 7, URL: "https://github.com/talkable/talkable/pull/7"},
		Repo: store.Repo{Owner: "talkable", Name: "talkable"}, SlotPath: t.TempDir(), Kind: pipeline.KindInitial,
		TargetSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err == nil {
		t.Fatal("a round without the App's key must fail")
	}

	codex := "codex"
	if _, err := a.Agents.NoteModelLimit(ctx, store.Session{ID: 1, PRID: 1, Role: "codex-review", AgentKind: &codex},
		agents.Health{Model: "gpt-test", Detail: "usage limit"}); err != nil {
		t.Fatalf("NoteModelLimit: %v", err)
	}

	// A per-PR worktree whose PR number is unknown skips its teardown.
	sl, err := a.Store.CreateSlot(ctx, store.Slot{Name: "talkable-worktree", RepoFullName: "talkable/talkable",
		Kind: store.SlotKindPerPR, Path: t.TempDir(), State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Slots.RemovePRWorktree(ctx, sl, true) // git is a fake without rules: the removal fails after the teardown

	for _, c := range []struct {
		kind, subject, src string
		level              slog.Level
	}{
		{"round.end", "pr:talkable/talkable#7", "pipeline", slog.LevelError},
		{agents.EventModelLimited, "tool:codex", "agents", slog.LevelWarn},
		{"slot.hook_failed", "slot:talkable-worktree", "slots", slog.LevelWarn},
	} {
		level, attrs, ok := logs.find(c.kind)
		switch {
		case !ok:
			t.Errorf("no %s record", c.kind)
		case level != c.level || attrs["subject"] != c.subject || attrs["src"] != c.src:
			t.Errorf("%s record: level %v, attrs %v; want %v, subject %s, src %s", c.kind, level, attrs, c.level, c.subject, c.src)
		}
	}

	// cleanup and notify get the same bridge, so their records keep a
	// subject too.
	for name, l := range map[string]execx.Logger{"cleanup": a.Cleanup.Log, "notify": a.Notify.Log} {
		if _, ok := l.(execx.AttrLogger); !ok {
			t.Errorf("%s logger %T takes no attributes", name, l)
		}
	}
}

type ptrErr struct{ msg string }

func (e *ptrErr) Error() string { return e.msg } // panics on a nil receiver

type ptrStringer struct{ s string }

func (v *ptrStringer) String() string { return v.s } // panics on a nil receiver

type valueErr struct{}

func (valueErr) Error() string { return "value" } // a nil *valueErr panics as soon as the method is bound

type panicStringer struct{}

func (panicStringer) String() string { panic("boom") }

// A typed-nil error or Stringer renders as "<nil>", the way slog's own
// handlers render it, instead of panicking inside RedactHandler.
func TestRedactHandlerRendersATypedNilAsNil(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(RedactHandler{Inner: slog.NewJSONHandler(&buf, nil)})
	var (
		e *ptrErr
		s *ptrStringer
		v *valueErr
	)
	l.Info("typed nils", "err", error(e), "who", s, "val", error(v), "boom", panicStringer{})
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	for _, k := range []string{"err", "who", "val"} {
		if got[k] != "<nil>" {
			t.Errorf("%s = %v, want <nil>", k, got[k])
		}
	}
	if b, _ := got["boom"].(string); !strings.HasPrefix(b, "!PANIC: boom") {
		t.Errorf("boom = %v, want !PANIC: boom", got["boom"])
	}
}
