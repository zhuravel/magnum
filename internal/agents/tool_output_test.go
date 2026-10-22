package agents

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// codexCeiling is the arg pair of the codex kind's tool_output_limit.
const codexCeiling = "-c tool_output_token_limit=65536"

// Codex 0.162.1 keeps at most 10,000 tokens of a tool call's output by
// default, so every Codex process magnum starts raises the ceiling: a
// judge's launch and resume (the curator, the retro and the eval replays
// launch through StartAgent too) and codex-review's line, its command and
// codex-review.sh alike. A Claude launch carries none, and a codex kind set
// to tool_output_limit = 0 keeps Codex's own.
func TestEveryCodexProcessCarriesTheToolOutputCeiling(t *testing.T) {
	launch := func(e *env, r Role, resume string) string {
		t.Helper()
		ws := e.workspace()
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(r), ws.Panes[r], resume); err != nil {
			t.Fatal(err)
		}
		return strings.Join(e.h.starts[len(e.h.starts)-1].Args, " ")
	}
	review := func(e *env, prompt bool) string {
		t.Helper()
		role := e.spec(RoleCodexReview)
		if prompt {
			role.Command, role.Prompt = "", "codex-review.sh"
		}
		line, err := e.m.ShellLine(e.ctx, e.pr.ID, role, ShellData{BaseRef: "origin/master", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1")})
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	for _, limit := range []int{config.DefaultCodexToolOutputLimit, 0} {
		carry := func(what, got string) {
			t.Helper()
			if has := strings.Contains(got, "tool_output_token_limit="); has != (limit > 0) || limit > 0 && !strings.Contains(got, codexCeiling) {
				t.Errorf("limit %d: %s = %q, want the ceiling %v", limit, what, got, limit > 0)
			}
		}
		for _, resume := range []string{"", "01a0-uuid"} {
			e := newEnv(t)
			e.setKind(KindCodex, func(k *config.Kind) { k.ToolOutputLimit = limit })
			carry("judge launch (resume "+resume+")", launch(e, RoleJudge, resume))
		}
		e := newEnv(t)
		e.setKind(KindCodex, func(k *config.Kind) { k.ToolOutputLimit = limit })
		carry("codex-review command", review(e, false))
		carry("codex-review.sh", review(e, true))
		if got := launch(e, RoleClaude, ""); strings.Contains(got, "tool_output_token_limit") {
			t.Errorf("limit %d: a claude launch carries the codex ceiling: %q", limit, got)
		}
	}
}
