package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// On talkable#11932 a launch that never came up held the judge's agent name,
// so every start of the judge failed with herdr's agent_name_taken: three
// attempts, needs_attention, and the board said "retry 3/3 setup" for hours
// with nothing naming what held the start. The stored error keeps the
// start's own words, so the reason a retry shows names the dead launch, the
// judge's pane and the agent, and once the attempts are spent the
// needs-attention reason and the card's line say the next retry closes it.
func TestTheSetupRetryNamesTheDeadLaunch(t *testing.T) {
	h := newHarness(t)
	h.ag.startErr = func(pr store.PR, role config.Role, pane string) error {
		if !role.Judge {
			return nil
		}
		name := agents.AgentName("talkable/talkable", pr.Number, agents.Role(role.Name))
		return fmt.Errorf("agents: start %s in %s: %w", name, pane,
			&herdr.Error{Method: "agent.start", Code: "agent_name_taken", Message: "agent name " + name + " is already in use"})
	}
	name := agents.AgentName("talkable/talkable", 2, agents.RoleJudge)
	line := "a dead launch holds the codex-judge's pane p-judge (agent " + name + "): closed on the next retry"
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "abc1234def5678"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if r := attention.Explain("", deref(pr.LastError), ""); pr.Attempts != 1 || r.Summary != "agents failed: "+line {
		t.Fatalf("after the first attempt: attempts %d, reason %q", pr.Attempts, r.Summary)
	}
	h.advance(time.Minute)
	h.tick()
	h.advance(2 * time.Minute)
	h.tick()
	pr = h.wantState(2, store.PRNeedsAttention)
	kind, _ := kvValue(h, KVPRAttention(pr.ID))
	r := attention.Explain(kind, deref(pr.LastError), "talkable#2")
	if pr.Attempts != 3 || r.Kind != attention.KindFailed || r.Summary != "agents failed 3× on abc1234: "+line {
		t.Fatalf("needs attention: attempts %d, reason %+v", pr.Attempts, r)
	}
	if !strings.Contains(r.Fix, "`magnum review talkable#2`") || !strings.Contains(r.Fix, "`magnum open talkable#2`") {
		t.Fatalf("fix %q", r.Fix)
	}
}
