package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// How many simplifications the judge posts is the operator's call per
// repository (config.Config.MaxSimplificationsFor: a [[repo]] block's
// max_simplifications over [pipeline]'s): a round of a repository whose
// block sets 2 asks the judge for at most 2 of claude-simplify's proposals,
// and one of a repository without the key for [pipeline]'s 5.
func TestTheRepositorysSimplificationCapReachesTheJudge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		repos []config.Repo
		want  string
	}{
		{"the repo block's", []config.Repo{{Repo: "talkable/talkable", MaxSimplifications: 2}}, "Post at most 2, the most substantial"},
		{"[pipeline]'s", []config.Repo{{Repo: "talkable/other", MaxSimplifications: 2}}, "Post at most 5, the most substantial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.Repos = tc.repos
			e.ag.behaviors[agents.RoleSimplify] = []behavior{writeReport("## Simplification: one helper fewer\n")}
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(903, "COMMENTED", "COMMENT").behavior(t)}
			in := e.input(KindInitial)
			in.Requested = []string{"simplify"}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
			mustContain(t, "judge prompt", judge, tc.want)
			if strings.Contains(judge, "Post at most three") {
				t.Errorf("the judge prompt still caps simplifications at three:\n%s", judge)
			}
		})
	}
}
