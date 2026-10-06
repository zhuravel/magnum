package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// A PR that changes .codex/ gets its checkout untrusted in the round's
// Codex sessions (agents' checkoutProject records it for the head they ran
// on): the judge's prompt carries `codex_project: declined`, so the
// review's Checks say so; a record of another head says nothing about
// this round.
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
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, agents.KindCodex), `{"head":"`+head+`","files":1,"compared":true}`); err != nil {
			t.Fatal(err)
		}
		in := e.input(KindInitial)
		in.DryRun = true
		if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		// The own pass posts nothing; the prompt that posts is the last.
		prompts := e.ag.submitsFor(agents.RoleJudge)
		if got := strings.Contains(prompts[len(prompts)-1].Text, "\ncodex_project: declined\n"); got != want {
			t.Errorf("record of head %s: the judge prompt says codex_project %v, want %v", head, got, want)
		}
	}
}

// A PR that changes .claude/ or .mcp.json gets the round's Claude sessions
// started with the user's settings only (recorded by their launches for
// the head): the judge's prompt carries `claude_project: declined`, and
// not `codex_project`, whose record is another one.
func TestTheJudgeLearnsItsClaudeSessionsRanWithoutThePRsClaudeChanges(t *testing.T) {
	for head, want := range map[string]bool{target: true, "fff0000fff0000fff0000fff0000fff0000fff00": false} {
		e := newEnv(t)
		dry := e.judgePosts(0, "", "COMMENT")
		dry.gh, dry.status = nil, "dry_run"
		e.ag.behaviors[agents.RoleJudge] = []behavior{dry.behavior(t)}
		codex := e.cfg.Kinds[agents.KindCodex]
		codex.ProjectUntrust = nil
		e.cfg.Kinds[agents.KindCodex] = codex
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, agents.KindClaude), `{"head":"`+head+`","files":2,"compared":true}`); err != nil {
			t.Fatal(err)
		}
		in := e.input(KindInitial)
		in.DryRun = true
		if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		prompts := e.ag.submitsFor(agents.RoleJudge)
		last := prompts[len(prompts)-1].Text
		if got := strings.Contains(last, "\nclaude_project: declined\n"); got != want {
			t.Errorf("record of head %s: the judge prompt says claude_project %v, want %v", head, got, want)
		}
		if strings.Contains(last, "codex_project") {
			t.Errorf("record of head %s: a Claude record made the prompt name codex_project", head)
		}
	}
}
