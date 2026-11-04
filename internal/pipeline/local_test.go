package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
)

// A local round (the factory's change before its PR exists) is a blind dry
// run whose judge reads the change's title and body and its ticket from the
// files the factory wrote: its prompt names description_file and the
// factory's ticket.md, its post_review line checks the anchors against the
// local diff (--dry-run --local-base <base>), and the round reads no Jira
// issue, even with [jira] on and a key in the title. The claude reviewer is
// told where the description is; the judge alone gets the ticket.
func TestALocalRoundsJudgeReadsTheFactorysFiles(t *testing.T) {
	e := newEnv(t)
	curl := e.withJira(nil)
	scratch := t.TempDir()
	local := &Local{DescriptionFile: filepath.Join(scratch, "description.md"), TicketFile: filepath.Join(scratch, TicketFile)}
	for _, f := range []string{local.DescriptionFile, local.TicketFile} {
		if err := os.WriteFile(f, []byte("# PS-123 Payout methods\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range e.cfg.Roles { // as the factory's review candidate has it (claude-review-local.md as claude-review.md)
		if e.cfg.Roles[i].Name == string(agents.RoleClaude) {
			e.cfg.Roles[i].Prompt = "claude-review-local.md"
		}
	}
	p := e.judgePosts(0, "", "COMMENT")
	p.gh, p.status = nil, "dry_run"
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindInitial)
	in.PR.Title = new("[PS-123] Payout methods")
	in.Blind, in.DryRun, in.Local = true, true, local
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if keys := askedKeys(curl); len(keys) != 0 {
		t.Errorf("a local round asked Jira for %q", keys)
	}
	if _, err := os.Stat(filepath.Join(res.ReportDir, TicketFile)); err == nil {
		t.Errorf("a local round wrote its own %s in %s", TicketFile, res.ReportDir)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) == 0 {
		t.Fatal("no judge prompt")
	}
	for _, j := range judge {
		mustContain(t, "judge prompt", j.Text, "\nlocal: true\n", "\ndescription_file: "+local.DescriptionFile+"\n", "\nticket: "+local.TicketFile+"\n")
		line := promptField(t, j.Text, "post_review")
		if !strings.Contains(line, " --dry-run ") || !strings.Contains(line, " --local-base "+in.BaseSHA+" ") {
			t.Errorf("post_review %q lacks --dry-run --local-base %s", line, in.BaseSHA)
		}
		if strings.Contains(j.Text, "gh api") {
			t.Errorf("a local judge prompt carries a gh call:\n%s", j.Text)
		}
	}
	if claude := e.ag.submitsFor(agents.RoleClaude); len(claude) == 0 || !strings.Contains(claude[0].Text, local.DescriptionFile) {
		t.Errorf("the claude reviewer's prompt does not name %s: %+v", local.DescriptionFile, claude)
	}
}
