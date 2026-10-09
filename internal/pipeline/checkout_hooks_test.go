package pipeline

import (
	"maps"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
)

// The restore of a checkout a role edited runs git as gitx does: with the
// clone's hooks off (core.hooksPath=/dev/null), so a hook a PR's setup
// script installed never runs outside the sandbox when the tree is reset.
func TestTheCheckoutRestoreRunsNoHooks(t *testing.T) {
	e := newEnv(t)
	e.restoreRules(false)
	e.ag.behaviors[agents.RoleSimplify] = []behavior{e.dirty("")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(512, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"}
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	calls := e.exec.CallsWithPrefix("git", "-C", slotPath)
	if len(calls) != 2 {
		t.Fatalf("git calls = %+v, want the reset and the clean", calls)
	}
	for _, c := range calls {
		if c.Env["GIT_CONFIG_COUNT"] != "1" || c.Env["GIT_CONFIG_KEY_0"] != "core.hooksPath" || c.Env["GIT_CONFIG_VALUE_0"] != "/dev/null" ||
			!maps.Equal(c.Env, gitx.Env(true)) {
			t.Errorf("%s: env %v, want gitx's (%v)", c.String(), c.Env, gitx.Env(true))
		}
	}
}
