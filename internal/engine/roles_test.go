package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// addRole declares an extra [[role]] (normalized like a loaded config).
func addRole(h *harness, r config.Role) {
	h.cfg.Roles = append(h.cfg.Roles, r)
	h.cfg.Normalize()
}

func TestWatchRolesRestrictPanesAndAgents(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.cfg.Watches[0].Roles = []string{"judge", "codex-review"} // an alias works too
	})
	pr := h.reviewedPR(2, "b1")

	ins := h.rd.all()
	if len(ins) != 1 || len(ins[0].Roles) != 2 || ins[0].Roles[0].Name != store.RoleJudge || ins[0].Roles[1].Name != store.RoleCodexReview {
		t.Fatalf("round roles: %+v", ins)
	}
	calls := h.ag.all()
	ws := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "ensure_workspace:"+itoa(pr.ID)+":") })
	if ws < 0 || !strings.HasSuffix(calls[ws], ":codex-judge,codex-review") {
		t.Fatalf("workspace: %v", calls)
	}
	var starts []string
	for _, c := range calls {
		if strings.HasPrefix(c, "start:") {
			starts = append(starts, c)
		}
	}
	if !slices.Equal(starts, []string{"start:" + itoa(pr.ID) + ":codex-judge:"}) {
		t.Fatalf("starts = %v, want the judge only (codex-review is a shell pane)", starts)
	}
	if slices.Contains(calls, "preflight:claude") || !slices.Contains(calls, "preflight:codex") {
		t.Fatalf("preflights: %v", calls)
	}
}

func TestRequestedRoleGetsAPaneForThatRound(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		addRole(h, config.Role{Name: "droid-simplify", Kind: config.KindDroid, Runs: config.RunsManual,
			After: []string{store.RoleClaude, store.RoleCodexReview}})
	})
	pr := h.reviewedPR(2, "b1")
	if h.lastWorkspaceHas(pr.ID, "droid-simplify") {
		t.Fatal("a manual role was laid out without a request")
	}

	h.advance(time.Minute)
	bad := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Roles: []string{"nope"}})
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Roles: []string{"Droid-Simplify"}})
	h.tick()
	if r := h.request(bad); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), `unknown role "nope"`) {
		t.Fatalf("unknown role request: %+v %q", r, deref(r.Result))
	}
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "with droid-simplify") {
		t.Fatalf("request: %+v %q", r, deref(r.Result))
	}
	ins := h.rd.all()
	if len(ins) != 2 || !slices.Equal(ins[1].Requested, []string{"droid-simplify"}) {
		t.Fatalf("rounds: %+v", ins)
	}
	if !h.lastWorkspaceHas(pr.ID, "droid-simplify") {
		t.Fatalf("droid-simplify not laid out: %v", h.ag.all())
	}
	start := "start:" + itoa(pr.ID) + ":droid-simplify:"
	if !slices.Contains(h.ag.all(), start) || !slices.Contains(h.ag.all(), "preflight:droid") {
		t.Fatalf("droid-simplify not started: %v", h.ag.all())
	}
	if v, ok := h.e.getKV(h.ctx, kvPRRequested(pr.ID)); ok {
		t.Fatalf("request kept after the review posted: %q", v)
	}

	// The next round is back to the watch's usual roles.
	h.advance(time.Minute)
	again := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(again); r.State != store.RequestDone {
		t.Fatalf("second request: %+v %q", r, deref(r.Result))
	}
	if n := len(h.rd.all()); n != 3 {
		t.Fatalf("rounds: %d", n)
	}
	if h.lastWorkspaceHas(pr.ID, "droid-simplify") || h.ag.count(start) != 1 {
		t.Fatalf("droid-simplify laid out again: %v", h.ag.all())
	}
}

func TestReviewRequestRefusesDisabledRole(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		addRole(h, config.Role{Name: "omp-review", Kind: config.KindOMP, Runs: config.RunsNever})
	})
	h.reviewedPR(2, "b1")
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Roles: []string{"omp-review"}})
	h.tick()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "disabled") {
		t.Fatalf("request: %+v %q", r, deref(r.Result))
	}
}

func TestPauseByKindOmp(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		addRole(h, config.Role{Name: "omp-review", Kind: config.KindOMP})
	})
	resetAt := h.clock.Now().Add(3 * time.Hour).Truncate(time.Second)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 1, ReviewID: 9, Event: "COMMENTED",
			Reports: map[agents.Role]pipeline.RoleReport{
				store.RoleClaude: {Role: store.RoleClaude, Kind: config.KindClaude, Status: pipeline.ReportOK},
				// Kind left empty: the engine resolves the role's kind.
				"omp-review": {Role: "omp-review", Status: string(agents.HealthUsageLimit), Detail: "You've hit your limit",
					Health: &agents.Health{Kind: agents.HealthUsageLimit, ResetAt: &resetAt}},
			}}, nil
	}
	pr := h.reviewedPR(2, "b1")
	if !slices.Contains(h.ag.all(), "start:"+itoa(pr.ID)+":omp-review:") || !slices.Contains(h.ag.all(), "preflight:omp") {
		t.Fatalf("omp-review not started: %v", h.ag.all())
	}
	if until, ok := h.e.kvTime(h.ctx, KVToolPausedUntil(config.KindOMP)); !ok || !until.Equal(resetAt) {
		t.Fatalf("omp paused until %v (%v), want %v", until, ok, resetAt)
	}
	for _, kind := range []string{config.KindCodex, config.KindClaude} {
		if _, ok := h.e.toolPause(h.ctx, kind); ok {
			t.Fatalf("%s paused by omp's usage limit", kind)
		}
	}
	// The omp pause holds rounds that use omp, not dispatch as a whole.
	if reason := h.e.pauseReason(h.ctx); reason != "" {
		t.Fatalf("daemon pause reason = %q", reason)
	}
	if reason := h.e.kindPauseReason(h.ctx, []string{config.KindCodex, config.KindOMP}); !strings.Contains(reason, "omp paused (usage_limit)") {
		t.Fatalf("kind pause reason = %q", reason)
	}
	if reason := h.e.kindPauseReason(h.ctx, []string{config.KindCodex, config.KindClaude}); reason != "" {
		t.Fatalf("codex and claude held by the omp pause: %q", reason)
	}
	if bar := h.e.tabBar(h.ctx); !strings.Contains(bar, "omp paused") {
		t.Fatalf("tab bar = %q", bar)
	}
	if _, err := h.e.requestPause(h.ctx, PausePayload{Tool: "bogus"}, false); err == nil || !strings.Contains(err.Error(), "omp") {
		t.Fatalf("unknown tool: %v", err)
	}
	if res, err := h.e.requestPause(h.ctx, PausePayload{Tool: config.KindOMP}, false); err != nil || res != "resumed omp" {
		t.Fatalf("resume omp: %q %v", res, err)
	}
	if _, ok := h.e.toolPause(h.ctx, config.KindOMP); ok {
		t.Fatal("omp pause not cleared")
	}
}

// A continued round whose judge lost its conversation becomes a full round
// again (recovery, or initial before any review): the roles that did not
// take part in the continue get their panes and agents.
func TestContinueWithLostJudgeLaysOutTheOtherRoles(t *testing.T) {
	h := newHarness(t)
	resetAt := h.clock.Now().Add(time.Hour)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.PR.Number != 2 || in.Kind == pipeline.KindContinue || len(h.rd.all()) > 1 {
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: max(in.Round, 1), ReviewID: 7, Event: "COMMENTED"}, nil
		}
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: 1, Role: store.RoleJudge, Kind: in.Kind,
			TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunPending, PromptText: "judge"})
		if err == nil {
			err = h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) { u.Set("submitted_at", h.clock.Now()) })
		}
		if err != nil {
			t.Fatal(err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 1, JudgeRunID: run.ID,
			Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: config.KindCodex, Until: resetAt}}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRPaused)

	// The sessions are lost meanwhile (no conversation to resume).
	ss, err := h.st.SessionsByPR(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if err := h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionLost, nil); err != nil {
			t.Fatal(err)
		}
	}
	earlier := len(h.ag.all())
	h.advance(2 * time.Hour)
	h.tick()

	var last *pipeline.RoundInput
	for _, in := range h.rd.all() {
		if in.PR.Number == 2 {
			last = &in
		}
	}
	if last == nil || last.Kind != pipeline.KindInitial {
		t.Fatalf("the continue after a lost judge: %+v", last)
	}
	calls := h.ag.all()[earlier:]
	if !slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasPrefix(c, "ensure_workspace:"+itoa(pr.ID)+":") && strings.HasSuffix(c, ":codex-judge")
	}) {
		t.Fatalf("the continue lays out the judge only first: %v", calls)
	}
	for _, want := range []string{"start:" + itoa(pr.ID) + ":codex-judge:", "preflight:claude",
		"pane:" + itoa(pr.ID) + ":claude-review", "pane:" + itoa(pr.ID) + ":codex-review", "start:" + itoa(pr.ID) + ":claude-review:"} {
		if !slices.Contains(calls, want) {
			t.Fatalf("calls lack %q: %v", want, calls)
		}
	}
	h.wantState(2, store.PRReviewed)
}
