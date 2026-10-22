package pipeline

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// A [[role]]'s own fallback_models apply to a round as they do to a pane
// job: the reviewer and the judge each switch to the first model of their
// role's list, never to their kind's.
func TestARolesFallbackListAppliesToARoundsReviewerAndJudge(t *testing.T) {
	e := newEnv(t)
	k := e.cfg.Kinds["codex"]
	k.SwitchModel, k.FallbackModels = "/model {model}", []string{"gpt-5"}
	e.cfg.Kinds["codex"] = k
	for i := range e.cfg.Roles {
		switch e.cfg.Roles[i].Name {
		case string(agents.RoleClaude):
			e.cfg.Roles[i].FallbackModels = []string{"sonnet"} // the kind's list starts with opus
		case string(agents.RoleJudge):
			e.cfg.Roles[i].FallbackModels = []string{"o3"}
		}
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{hitsLimit(fableLimitPane), writeReport("## P2 something\n")}
	var marker string
	post := e.judgePosts(707, "COMMENTED", "COMMENT").behavior(t)
	hit := hitsLimit(fableLimitPane)
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		func(f *fakeAgents, run store.Run, text string) error {
			marker = markerRunID(t, text)
			return hit(f, run, text)
		},
		func(f *fakeAgents, run store.Run, text string) error {
			return post(f, run, "run_id: "+marker)
		},
	}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Pause != nil {
		t.Fatalf("result = %+v", res)
	}
	e.ag.mu.Lock()
	switches := slices.Clone(e.ag.switches)
	e.ag.mu.Unlock()
	slices.SortFunc(switches, byRole)
	want := []switchCall{
		{Role: string(agents.RoleJudge), Model: "o3", Reason: agents.SwitchLimitHit},
		{Role: string(agents.RoleClaude), Model: "sonnet", Reason: agents.SwitchLimitHit},
	}
	slices.SortFunc(want, byRole)
	if !slices.Equal(switches, want) {
		t.Fatalf("switches = %+v, want each role's own first fallback %+v", switches, want)
	}
}

func byRole(a, b switchCall) int { return strings.Compare(a.Role, b.Role) }
