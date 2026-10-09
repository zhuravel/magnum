package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// A per-model limit ("You've reached your Fable limit") is handled inside the
// round by switching the session's model, so a finished round carrying a
// model_limit report or pause must leave the agent kind running, while an
// account-wide usage_limit in the same position still pauses it.
func TestRoundResultPausesByHealthKind(t *testing.T) {
	claudeReport := func(status agents.HealthKind, detail string, h *agents.Health) map[agents.Role]pipeline.RoleReport {
		return map[agents.Role]pipeline.RoleReport{
			store.RoleClaude: {Role: store.RoleClaude, Kind: config.KindClaude, Status: string(status), Detail: detail, Health: h},
		}
	}
	cases := map[string]struct {
		result     func(reset time.Time) pipeline.RoundResult
		wantPaused map[string]string // kind -> reason; every other kind must stay running
	}{
		"report model_limit": {
			result: func(time.Time) pipeline.RoundResult {
				return pipeline.RoundResult{Reports: claudeReport(agents.HealthModelLimit, "You've reached your Fable limit", &agents.Health{Kind: agents.HealthModelLimit, Model: "fable"})}
			},
		},
		"report model_limit with a reset time": {
			result: func(reset time.Time) pipeline.RoundResult {
				return pipeline.RoundResult{Reports: claudeReport(agents.HealthModelLimit, "You've hit your Opus limit · resets 3pm", &agents.Health{Kind: agents.HealthModelLimit, Model: "opus", ResetAt: &reset})}
			},
		},
		"pause model_limit": {
			result: func(reset time.Time) pipeline.RoundResult {
				return pipeline.RoundResult{Pause: &pipeline.Pause{Kind: string(agents.HealthModelLimit), Tool: config.KindClaude, Until: reset, Detail: "fable"}}
			},
		},
		"report and pause model_limit": {
			result: func(reset time.Time) pipeline.RoundResult {
				return pipeline.RoundResult{
					Reports: claudeReport(agents.HealthModelLimit, "fable", nil),
					Pause:   &pipeline.Pause{Kind: string(agents.HealthModelLimit), Tool: config.KindClaude, Until: reset},
				}
			},
		},
		"report usage_limit": {
			result: func(reset time.Time) pipeline.RoundResult {
				return pipeline.RoundResult{Reports: claudeReport(agents.HealthUsageLimit, "You've hit your limit", &agents.Health{Kind: agents.HealthUsageLimit, ResetAt: &reset})}
			},
			wantPaused: map[string]string{config.KindClaude: string(agents.HealthUsageLimit)},
		},
		"pause usage_limit": {
			result: func(reset time.Time) pipeline.RoundResult {
				return pipeline.RoundResult{Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: config.KindClaude, Until: reset}}
			},
			wantPaused: map[string]string{config.KindClaude: string(agents.HealthUsageLimit)},
		},
		// Two kinds in one round: only the one with an account-wide limit pauses.
		"claude model_limit beside codex usage_limit": {
			result: func(reset time.Time) pipeline.RoundResult {
				reports := claudeReport(agents.HealthModelLimit, "fable", nil)
				reports["codex-review"] = pipeline.RoleReport{Role: "codex-review", Kind: config.KindCodex, Status: string(agents.HealthUsageLimit),
					Health: &agents.Health{Kind: agents.HealthUsageLimit, ResetAt: &reset}}
				return pipeline.RoundResult{Reports: reports}
			},
			wantPaused: map[string]string{config.KindCodex: string(agents.HealthUsageLimit)},
		},
		"claude usage_limit beside codex model_limit": {
			result: func(reset time.Time) pipeline.RoundResult {
				reports := claudeReport(agents.HealthUsageLimit, "You've hit your limit", &agents.Health{Kind: agents.HealthUsageLimit, ResetAt: &reset})
				reports["codex-review"] = pipeline.RoleReport{Role: "codex-review", Kind: config.KindCodex, Status: string(agents.HealthModelLimit)}
				return pipeline.RoundResult{Reports: reports}
			},
			wantPaused: map[string]string{config.KindClaude: string(agents.HealthUsageLimit)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			reset := h.clock.Now().Add(3 * time.Hour).Truncate(time.Second)
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				res := tc.result(reset)
				res.Outcome, res.Round, res.ReviewID, res.Event = pipeline.OutcomePosted, 1, 9, "COMMENTED"
				return res, nil
			}
			h.reviewedPR(2, "b1") // the round posted: model limits and pauses are side channels

			for _, kind := range []string{config.KindClaude, config.KindCodex, config.KindOMP} {
				want, paused := tc.wantPaused[kind]
				p, got := h.e.toolPause(h.ctx, kind)
				if got != paused {
					t.Fatalf("%s paused = %v (%+v), want %v", kind, got, p, paused)
				}
				if !paused {
					for _, key := range []string{store.KVToolPausedUntil(kind), store.KVToolPausedReason(kind), store.KVToolPausedDetail(kind), store.KVToolBackoff(kind)} {
						if v, ok := h.e.getKV(h.ctx, key); ok {
							t.Errorf("%s left kv %s = %q although it is not paused", kind, key, v)
						}
					}
					if h.hasEvent("tool:"+kind, "tool.paused") {
						t.Errorf("%s: a tool.paused event was recorded", kind)
					}
					continue
				}
				if p.Reason != want || !p.Until.Equal(reset) {
					t.Errorf("%s pause = %+v, want reason %q until %v", kind, p, want, reset)
				}
				if !h.hasEvent("tool:"+kind, "tool.paused") {
					t.Errorf("%s: no tool.paused event", kind)
				}
			}
			toasts := strings.Join(h.nh.all(), "\n")
			if strings.Contains(toasts, "model_limit") {
				t.Errorf("a model limit raised a pause toast: %v", h.nh.all())
			}
			if _, claudePaused := tc.wantPaused[config.KindClaude]; !claudePaused && strings.Contains(toasts, "Claude") {
				t.Errorf("a toast names Claude although it is running: %v", h.nh.all())
			}
			if len(tc.wantPaused) == 0 {
				if reason := h.e.pauseReason(h.ctx); reason != "" {
					t.Errorf("daemon pause reason = %q", reason)
				}
				if reason := h.e.kindPauseReason(h.ctx, []string{config.KindClaude, config.KindCodex}); reason != "" {
					t.Errorf("kind pause reason = %q", reason)
				}
				if bar := h.e.tabBar(h.ctx); strings.Contains(bar, "paused") {
					t.Errorf("tab bar = %q", bar)
				}
			}
		})
	}
}

// Dispatch keeps going after a round hit a model limit (the session switched
// to a fallback model), where a usage limit holds the next PR back.
func TestModelLimitDoesNotHoldDispatch(t *testing.T) {
	run := func(t *testing.T, kind agents.HealthKind) int {
		t.Helper()
		h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 2 })
		pool := h.cfg.Pools[0]
		if _, err := h.st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(2), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
			Path: pool.Path(2), MainClone: pool.MainClone, State: store.SlotFree}); err != nil {
			t.Fatal(err)
		}
		h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
			reset := h.clock.Now().Add(2 * time.Hour)
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 1, ReviewID: 9, Event: "COMMENTED",
				Reports: map[agents.Role]pipeline.RoleReport{
					store.RoleClaude: {Role: store.RoleClaude, Kind: config.KindClaude, Status: string(kind),
						Health: &agents.Health{Kind: kind, ResetAt: &reset}},
				}}, nil
		}
		h.reviewedPR(2, "b1")
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
		h.tick()
		h.advance(10 * time.Minute)
		h.tick()
		return len(h.rd.all())
	}
	if got := run(t, agents.HealthModelLimit); got < 2 {
		t.Errorf("rounds after a model_limit = %d, want the next PR dispatched (>= 2)", got)
	}
	if got := run(t, agents.HealthUsageLimit); got != 1 {
		t.Errorf("rounds after a usage_limit = %d, want the claude pause to hold the next PR (1)", got)
	}
}
