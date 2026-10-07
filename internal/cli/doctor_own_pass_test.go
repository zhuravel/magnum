package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// doctor checks the judge's own-pass prompt like its others: one that does
// not resolve fails the prompts check, naming the role.
func TestDoctorChecksTheJudgesOwnPassPrompt(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	d.Config.Roles = []config.Role{
		{Name: "codex-judge", Kind: config.KindCodex, Judge: true, OwnPass: "own-nope.md"},
		{Name: "claude-review", Kind: config.KindClaude},
	}
	d.Config.Normalize()
	pm := doctorByName(doctorPrompts(context.Background(), d))
	if c := pm["prompt own-nope.md"]; c.Status != doctorFail || !strings.Contains(c.Detail, "role codex-judge: own_pass prompt") {
		t.Errorf("own-pass prompt check = %+v (checks %v)", c, pm)
	}
}

// doctor checks every prompt kind a role may name (config.PromptKinds): its
// own copy of the list lacked restart, so a reviewer's restart prompt that
// does not resolve passed until a push restarted a round.
func TestDoctorChecksARolesRestartPrompt(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	d.Config.Roles = []config.Role{
		{Name: "codex-judge", Kind: config.KindCodex, Judge: true},
		{Name: "claude-review", Kind: config.KindClaude, Restart: "restart-nope.md"},
	}
	d.Config.Normalize()
	pm := doctorByName(doctorPrompts(context.Background(), d))
	if c := pm["prompt restart-nope.md"]; c.Status != doctorFail || !strings.Contains(c.Detail, "role claude-review: restart prompt") {
		t.Errorf("restart prompt check = %+v (checks %v)", c, pm)
	}
}
