package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// The size of a P2's harm depends on what the repository puts at risk, which
// only the operator's config may say (a stakes block in the checkout is PR
// content). The round's repository's stakes line (config.Config.StakesFor:
// its [[repo]] block's over its watch's) reaches the judge's <magnum> block
// and the claude reviewer's prompt; the codex-review command, to which
// magnum passes no prompt, gets nothing.
func TestTheStakesLineReachesTheJudgeAndTheClaudeReviewer(t *testing.T) {
	const stakes = "A SaaS on the public internet: merchants' money and shoppers' personal data."
	e := newEnv(t)
	e.cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"*"}, Identity: "talkable-app", Stakes: "The watch's line, which the repo's overrides."}}
	e.cfg.Repos = []config.Repo{{Repo: "talkable/talkable", Stakes: stakes}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(902, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", judge, "\nstakes: "+stakes+"\n")
	mustContain(t, "claude-review prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "as magnum's operator states it", stakes)
	if strings.Contains(judge, "The watch's line") {
		t.Errorf("the judge got the watch's line over the repo's:\n%s", judge)
	}
	for _, c := range e.ag.shellCallsFor(agents.RoleCodexReview) {
		if strings.Contains(c.Script, "merchants") {
			t.Errorf("the codex-review command carries the stakes: %s", c.Script)
		}
	}
}

// An eval replay reviews the corpus case's repository through the same
// round, a blind dry run: the operator's stakes line for that repository
// (here its watch's) reaches the judge as it would in a live round, and a
// repository without one gets no stakes field.
func TestAnEvalReplayCarriesTheStakesLine(t *testing.T) {
	const stakes = "Internal tooling with no customers: the harm is a colleague's hour at most."
	for _, set := range []bool{true, false} {
		e := newEnv(t)
		w := config.Watch{Owner: "talkable", Include: []string{"*"}, Identity: "talkable-app"}
		if set {
			w.Stakes = stakes
		}
		e.cfg.Watches = []config.Watch{w}
		p := e.judgePosts(0, "", "COMMENT")
		p.gh, p.status = nil, "dry_run"
		e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
		in := e.input(KindInitial)
		in.Blind, in.DryRun = true, true
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomeDryRun {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
		if got := strings.Contains(judge, "\nstakes: "+stakes+"\n"); got != set {
			t.Errorf("stakes set %v: the replay's judge prompt carries the line: %v\n%s", set, got, judge)
		}
		if !set && strings.Contains(judge, "\nstakes:") {
			t.Errorf("a repository without stakes got a stakes field:\n%s", judge)
		}
	}
}
