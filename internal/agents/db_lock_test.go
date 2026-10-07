package agents

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// The db-lock line is typed into a shell: the binary, the checkout and the
// role are shell-quoted, and it ends in `--` for the role's command.
func TestDBLockLineQuotesEveryValue(t *testing.T) {
	if got, want := DBLockLine("/opt/magnum dir/bin/magnum", "/slots/talkable review3", "claude-review"),
		`'/opt/magnum dir/bin/magnum' db-lock --checkout '/slots/talkable review3' --role claude-review --`; got != want {
		t.Errorf("DBLockLine = %s\nwant %s", got, want)
	}
	// Without a checkout db-lock takes the git work tree it runs in.
	if got := DBLockLine("", "", ""); got != "magnum db-lock --" {
		t.Errorf("DBLockLine with nothing = %s", got)
	}
}

// The own pass and the reviewers run specs on the slot's databases at the
// same time: every prompt of a role that runs tests carries the exact
// db-lock line for its checkout and role, quoted, the judge's in its
// <magnum> block as `db_lock`.
func TestTestRunningPromptsCarryTheDBLockLine(t *testing.T) {
	const magnum, checkout = "/opt/magnum/bin/magnum", "/slots/talkable review3"
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md", "judge-continue.md"} {
		d := judgeFixture()
		d.Magnum, d.Checkout, d.Role = magnum, checkout, "codex-judge"
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "\ndb_lock: /opt/magnum/bin/magnum db-lock --checkout '/slots/talkable review3' --role codex-judge --\n"
		if !strings.Contains(magnumBlock(t, got), want) {
			t.Errorf("%s's <magnum> block lacks %q:\n%s", name, want, magnumBlock(t, got))
		}
	}
	for _, name := range []string{"claude-review.md", "claude-rereview.md", "claude-restart.md", "claude-simplify.md"} {
		d := roleFixture()
		d.Magnum, d.Checkout, d.Role = magnum, checkout, "claude-review"
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "`/opt/magnum/bin/magnum db-lock --checkout '/slots/talkable review3' --role claude-review -- <command>`"
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %s:\n%s", name, want, got)
		}
	}
}

// The prompt names the role it is rendered for: the roles waiting for the
// lock are told who holds it.
func TestRolePromptNamesTheRoleInTheDBLockLine(t *testing.T) {
	e := newEnv(t)
	role, judge := roleFixture(), judgeFixture()
	role.Role, judge.Role = "", ""
	for _, tc := range []struct {
		role Role
		kind string
		data any
	}{
		{RoleClaude, config.PromptInitial, role},
		{RoleJudge, config.PromptInitial, judge},
		{RoleJudge, config.PromptContinue, &judge},
	} {
		got, err := e.m.RolePrompt(e.spec(tc.role), tc.kind, tc.data)
		if err != nil {
			t.Fatal(err)
		}
		if want := " db-lock --checkout /Users/bohdan/Projects/talkable.review3 --role " + string(tc.role) + " --"; !strings.Contains(got, want) {
			t.Errorf("%s %s lacks %q:\n%s", tc.role, tc.kind, want, got)
		}
	}
	if judge.Role != "" {
		t.Errorf("RolePrompt changed the caller's data: Role %q", judge.Role)
	}
}
