package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// A PR that changes .codex/ gets its checkout untrusted in the round's
// Codex sessions (agents' checkoutProject records it for the head they ran
// on): the judge's prompt carries the Checks line naming it,
// `project_checks: Codex ran without the PR's .codex/ changes`; a record of
// another head says nothing about this round.
func TestTheJudgeLearnsItsCodexSessionsRanWithoutThePRsCodexChanges(t *testing.T) {
	for head, want := range map[string]bool{target: true, "fff0000fff0000fff0000fff0000fff0000fff00": false} {
		e := newEnv(t)
		dry := e.judgePosts(0, "", "COMMENT")
		dry.gh, dry.status = nil, "dry_run"
		e.ag.behaviors[agents.RoleJudge] = []behavior{dry.behavior(t)}
		// The judge's launch recorded it; codex-review's line, which would
		// record its own look at the checkout, keeps out of it here.
		codex := e.cfg.Kinds[agents.KindCodex]
		codex.ProjectUntrust = nil
		e.cfg.Kinds[agents.KindCodex] = codex
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, agents.KindCodex), `{"head":"`+head+`","files":1,"compared":true,"paths":[".codex/"]}`); err != nil {
			t.Fatal(err)
		}
		in := e.input(KindInitial)
		in.DryRun = true
		if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		// The own pass posts nothing; the prompt that posts is the last.
		prompts := e.ag.submitsFor(agents.RoleJudge)
		if got := strings.Contains(prompts[len(prompts)-1].Text, "\nproject_checks: Codex ran without the PR's .codex/ changes\n"); got != want {
			t.Errorf("record of head %s: the judge prompt names Codex's decline %v, want %v", head, got, want)
		}
	}
}

// A PR that changes a CLAUDE.md gets the round's Claude sessions started
// with the user's settings only (recorded by their launches for the head):
// the judge's prompt carries `project_checks` naming CLAUDE.md, and nothing
// of Codex, whose record is another one.
func TestTheJudgeLearnsItsClaudeSessionsRanWithoutThePRsClaudeChanges(t *testing.T) {
	for head, want := range map[string]bool{target: true, "fff0000fff0000fff0000fff0000fff0000fff00": false} {
		e := newEnv(t)
		dry := e.judgePosts(0, "", "COMMENT")
		dry.gh, dry.status = nil, "dry_run"
		e.ag.behaviors[agents.RoleJudge] = []behavior{dry.behavior(t)}
		codex := e.cfg.Kinds[agents.KindCodex]
		codex.ProjectUntrust = nil
		e.cfg.Kinds[agents.KindCodex] = codex
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, agents.KindClaude), `{"head":"`+head+`","files":2,"compared":true,"paths":["CLAUDE.md"]}`); err != nil {
			t.Fatal(err)
		}
		in := e.input(KindInitial)
		in.DryRun = true
		if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		prompts := e.ag.submitsFor(agents.RoleJudge)
		last := prompts[len(prompts)-1].Text
		if got := strings.Contains(last, "\nproject_checks: Claude ran without the PR's CLAUDE.md changes\n"); got != want {
			t.Errorf("record of head %s: the judge prompt names Claude's decline %v, want %v", head, got, want)
		}
		if strings.Contains(last, "Codex ran without") {
			t.Errorf("record of head %s: a Claude record made the prompt name Codex", head)
		}
	}
}

// A record names the last launch of the kind on the head, which may be a
// session this round did not run (a claude-review paused or left out of
// the round, launched for the head before): the judge's prompt carries a
// kind's decline only when one of the round's roles, the judge included,
// is of that kind. A round of codex-review and the Codex judge says
// nothing of Claude; one without codex-review still names Codex's, which
// the Codex judge ran under.
func TestTheJudgeHearsOfADeclinedProjectOnlyFromAKindTheRoundRan(t *testing.T) {
	for _, c := range []struct {
		roles  []string
		kind   string
		line   string
		wanted bool
	}{
		{[]string{"codex-judge", "codex-review"}, agents.KindClaude, "Claude ran without the PR's .claude/ changes", false},
		{[]string{"codex-judge", "claude-review"}, agents.KindClaude, "Claude ran without the PR's .claude/ changes", true},
		{[]string{"codex-judge", "claude-review"}, agents.KindCodex, "Codex ran without the PR's AGENTS.md changes", true},
	} {
		e := newEnv(t)
		dry := e.judgePosts(0, "", "COMMENT")
		dry.gh, dry.status = nil, "dry_run"
		e.ag.behaviors[agents.RoleJudge] = []behavior{dry.behavior(t)}
		codex := e.cfg.Kinds[agents.KindCodex]
		codex.ProjectUntrust = nil
		e.cfg.Kinds[agents.KindCodex] = codex
		paths := map[string]string{agents.KindClaude: `[".claude/"]`, agents.KindCodex: `["AGENTS.md"]`}[c.kind]
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, c.kind), `{"head":"`+target+`","files":1,"compared":true,"paths":`+paths+`}`); err != nil {
			t.Fatal(err)
		}
		in := e.input(KindInitial)
		in.DryRun = true
		in.Roles = e.cfg.RolesFor(&config.Watch{Roles: c.roles})
		if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		prompts := e.ag.submitsFor(agents.RoleJudge)
		if got := strings.Contains(prompts[len(prompts)-1].Text, "\nproject_checks: "+c.line+"\n"); got != c.wanted {
			t.Errorf("roles %v, a %s record: the judge prompt says %q %v, want %v", c.roles, c.kind, c.line, got, c.wanted)
		}
	}
}
