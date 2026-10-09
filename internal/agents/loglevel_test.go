package agents

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store/storetest"
)

// A warn event reaches the log at Warn, with its subject and kind: a model
// limit used to be logged at Info like every other line.
func TestAModelLimitLogsAtWarnWithItsSubject(t *testing.T) {
	e := newEnv(t)
	e.started()
	if _, err := e.m.NoteModelLimit(e.ctx, e.session(RoleClaude), Health{Kind: HealthModelLimit, Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	recs := e.logs.records("fable limited until")
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want the limit's", e.logs.recs)
	}
	if r := recs[0]; r.level != slog.LevelWarn || r.attrs["subject"] != "tool:"+KindClaude || r.attrs["kind"] != EventModelLimited {
		t.Fatalf("record = %+v, want warn with subject tool:%s and kind %s", r, KindClaude, EventModelLimited)
	}
}

// A failure the manager goes on after logs at Warn, and at Info when it is
// the daemon stopping (the context ended).
func TestAToleratedFailureLogsAtWarnUnlessTheDaemonStops(t *testing.T) {
	storetest.Serial(t)
	e := newEnv(t)
	e.started()
	failLimitReads(t, "fable")
	e.recordLimits(e.clock.Now().Add(time.Hour), "fable")
	s := e.session(RoleClaude)
	if _, ok := e.m.FallbackModel(e.ctx, s, nil); ok {
		t.Fatal("a limit that cannot be read must give no fallback")
	}
	stopped, cancel := context.WithCancel(e.ctx)
	cancel()
	e.m.FallbackModel(stopped, s, nil)
	recs := e.logs.records("agents: claude-review: no fallback model: ")
	if len(recs) != 2 || recs[0].level != slog.LevelWarn || recs[1].level != slog.LevelInfo {
		t.Fatalf("records = %+v, want one at warn, then one at info once the daemon stops", recs)
	}
}
