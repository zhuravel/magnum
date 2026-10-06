package pipeline

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// A round whose reviewer left no usable report never yields an APPROVE: the
// judge's no_findings_event is COMMENT for that round (an APPROVE was posted
// while claude-review had hit a usage limit). A simplify with nothing to
// propose wrote a report that says so; it is no missing reviewer. The
// blocking event and a post-merge round are unchanged.
func TestJudgeEventsGiveNoApproveWhileAReviewerReportIsMissing(t *testing.T) {
	cfg := config.Defaults()
	id := &config.Identity{Name: "zhuravel", Kind: "gh", Login: "zhuravel", NoFindingsEvent: "APPROVE", BlockingEvent: "REQUEST_CHANGES"}
	ok := []agents.Report{{Role: "claude-review", Path: "/r/claude-review.md", Status: ReportOK}, {Role: "codex-review", Path: "/r/codex-review.md", Status: ReportOK}}
	noProposals := agents.Report{Role: "claude-simplify", Path: "/r/claude-simplify.md", Status: ReportOK}
	for name, tc := range map[string]struct {
		reports      []agents.Report
		postMerge    bool
		wantNF, want string
	}{
		"every report":                 {ok, false, "APPROVE", "REQUEST_CHANGES"},
		"no reviewer ran":              {nil, false, "APPROVE", "REQUEST_CHANGES"},
		"a simplify without proposals": {append(ok[:2:2], noProposals), false, "APPROVE", "REQUEST_CHANGES"},
		"a usage limit":                {[]agents.Report{{Role: "claude-review", Status: "usage_limit", Missing: true}, ok[1]}, false, "COMMENT", "REQUEST_CHANGES"},
		"a timeout":                    {[]agents.Report{ok[0], {Role: "codex-review", Status: ReportTimeout, Missing: true, Detail: "timed out after 40m"}}, false, "COMMENT", "REQUEST_CHANGES"},
		"a report without a path":      {[]agents.Report{ok[0], {Role: "codex-review", Status: ReportBusy}}, false, "COMMENT", "REQUEST_CHANGES"},
		"post-merge":                   {[]agents.Report{{Role: "claude-review", Status: "usage_limit", Missing: true}}, true, "COMMENT", "COMMENT"},
	} {
		if nf, be := JudgeEvents(cfg, "talkable/talkable", id, tc.postMerge, tc.reports...); nf != tc.wantNF || be != tc.want {
			t.Errorf("%s: events %s/%s, want %s/%s", name, nf, be, tc.wantNF, tc.want)
		}
	}
}

// The round's judge prompt says COMMENT for no findings when claude-review
// ended without a report, though the repository allows APPROVE, and the
// round says why in a warning.
func TestNoApproveWhenClaudeReviewHasNoReport(t *testing.T) {
	e := newEnv(t)
	e.cfg.Repos = []config.Repo{{Repo: "talkable/talkable", NoFindingsEvent: "APPROVE"}}
	e.ag.behaviors[agents.RoleClaude] = []behavior{endSilently()}
	e.ag.reads[agents.RoleClaude] = "⏺ Reviewing…\n  ⎿ You've hit your limit · resets 5pm (UTC)\n"
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(661, "COMMENTED", "COMMENT").behavior(t)}
	if _, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text,
		"no_findings_event: COMMENT", "blocking_event: REQUEST_CHANGES", "claude-review: missing (usage_limit)")
	found := false
	for _, ev := range e.events() {
		found = found || strings.Contains(ev.Message, "no APPROVE this round") && strings.Contains(ev.Message, "claude-review (usage_limit)")
	}
	if !found {
		t.Errorf("no event says why the round gives no APPROVE: %+v", e.events())
	}

	// Control: with every report the repository's APPROVE stands.
	e2 := newEnv(t)
	e2.cfg.Repos = e.cfg.Repos
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgePosts(662, "APPROVED", "APPROVE").behavior(t)}
	if _, err := e2.r.RunRound(e2.ctx, e2.input(KindInitial)); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	mustContain(t, "judge prompt", e2.ag.submitsFor(agents.RoleJudge)[0].Text, "no_findings_event: APPROVE")
}

// magnum appends the identity's footer after verification
// (appendFooter): the judge's prompt names none, whatever the identity sets.
func TestJudgePromptNamesNoFooter(t *testing.T) {
	own := "_Reviewed by the team's bot._"
	for name, footer := range map[string]*string{"built-in": nil, "own": &own} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.Identities[0].ReviewFooter = footer
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(663, "COMMENTED", "COMMENT").behavior(t)}
			if _, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
			for _, s := range []string{"\nfooter:", "Reviewed commit", "About Magnum", own} {
				if strings.Contains(judge, s) {
					t.Errorf("the judge prompt carries %q:\n%s", s, judge)
				}
			}
		})
	}
}

// A simplify run with nothing to propose leaves the identity's APPROVE in
// place: its report says "No proposals.", which is no failure.
func TestSimplifyWithoutProposalsKeepsTheApprove(t *testing.T) {
	e := newEnv(t)
	e.cfg.Repos = []config.Repo{{Repo: "talkable/talkable", NoFindingsEvent: "APPROVE"}}
	e.ag.behaviors[agents.RoleSimplify] = []behavior{writeReport("No proposals.\n\nSkipped: none.\n")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(664, "APPROVED", "APPROVE").behavior(t)}
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	path := filepath.Join(e.reportDir(), "claude-simplify.md")
	if got := res.Reports[agents.RoleSimplify]; got.Status != ReportOK || got.Path != path {
		t.Fatalf("simplify report = %+v", got)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-simplify: "+path, "no_findings_event: APPROVE")
	if run := e.runOf(agents.RoleSimplify, store.RunInitial); run.State != store.RunVerified {
		t.Errorf("simplify run = %s", run.State)
	}
}
