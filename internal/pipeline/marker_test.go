package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// Every shipped reviewer is asked to start its report with its run's
// marker: the claude prompts name it, and codex-review's line prints it
// before the output it tees into the report.
func TestEveryReviewerIsAskedForItsRunMarker(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleSimplify] = []behavior{writeReport("No proposals.\n")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(610, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	for _, role := range []agents.Role{agents.RoleClaude, agents.RoleSimplify} {
		run := e.runOf(role, store.RunInitial)
		mustContain(t, string(role)+" prompt", e.ag.submitsFor(role)[0].Text, "first line must be `"+agents.ReportMarker(run.ID)+"`")
		if got := res.Reports[role]; got.Status != ReportOK {
			t.Errorf("%s report = %+v, want ok", role, got)
		}
	}
	codex := e.runOf(agents.RoleCodexReview, store.RunInitial)
	calls := e.ag.shellCallsFor(agents.RoleCodexReview)
	if len(calls) != 1 {
		t.Fatalf("codex-review lines: %+v", calls)
	}
	mustContain(t, "codex-review line", calls[0].Script, "{ printf '"+agents.ReportMarker(codex.ID)+`\n'; command codex review`, "; } | tee ")
	if got := res.Reports[agents.RoleCodexReview]; got.Status != ReportOK {
		t.Errorf("codex-review report = %+v, want ok", got)
	}
}

// A report is its run's only when it starts with its run's marker: report
// paths are per head, so a reviewer that kept working after its run ended
// can write where a later run of the same head looks. A report without the
// marker, or with another run's, is stale: missing (the judge's candidate
// list says so as for any missing report), with why in the run and its
// warning.
func TestAReportIsItsRunsOnlyWithItsRunMarker(t *testing.T) {
	const earlier = "r-20261003T090000-1"
	for _, tc := range []struct {
		name    string
		write   behavior
		status  string
		detail  string
		inJudge string
	}{
		{"its own run's marker", writeReport("## P2 something\n"), ReportOK, "", "claude-review: /"},
		{"no marker", writeRawReport("## P2 something\n"), ReportMissing, "stale report (no run marker)",
			"claude-review: missing (missing)"},
		{"another run's marker", writeRawReport(agents.ReportMarker(earlier) + "\n## P1 a late finding\n"), ReportMissing,
			"stale report from run " + earlier, "claude-review: missing (missing)"},
		{"its own marker and nothing else", writeReport("\n"), ReportMissing, "empty claude-review.md",
			"claude-review: missing (missing)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.ag.behaviors[agents.RoleClaude] = []behavior{tc.write}
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(611, "COMMENTED", "COMMENT").behavior(t)}

			res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
			if err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			got := res.Reports[agents.RoleClaude]
			if got.Status != tc.status || got.Detail != tc.detail {
				t.Fatalf("claude report = %q (%q), want %q (%q)", got.Status, got.Detail, tc.status, tc.detail)
			}
			mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, tc.inJudge)
			run := e.runOf(agents.RoleClaude, store.RunInitial)
			if store.Deref(run.Outcome) != tc.status || (tc.detail != "" && store.Deref(run.Error) != tc.detail) {
				t.Fatalf("claude run = %q (%q), want %q (%q)", store.Deref(run.Outcome), store.Deref(run.Error), tc.status, tc.detail)
			}
			if warns := e.reviewerWarnings(agents.RoleClaude); tc.detail != "" &&
				(len(warns) != 1 || !strings.HasPrefix(warns[0], "claude-review report missing: "+tc.detail+"; interrupted claude-review")) {
				t.Fatalf("claude warnings: %q", warns)
			}
		})
	}
}

// A role whose prompt names no run marker (a prompt file of its own, the
// judge's own pass) needs none: its report is read as before.
func TestARoleWhosePromptNamesNoMarkerNeedsNone(t *testing.T) {
	e := newEnv(t)
	droid := e.addRole(config.Role{Name: "droid-review", Kind: "droid"}, "")
	e.ag.behaviors[agents.Role(droid.Name)] = []behavior{writeRawReport("## P2 a droid finding\n")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(612, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if strings.Contains(e.ag.submitsFor(agents.Role(droid.Name))[0].Text, "magnum:run=") {
		t.Fatalf("the generic prompt names a marker")
	}
	if got := res.Reports[agents.Role(droid.Name)]; got.Status != ReportOK {
		t.Fatalf("droid report = %+v, want ok without a marker", got)
	}
}

// A reviewer that continues on a fallback model was asked for its first
// run's marker; that marker makes the continuation's report its own.
func TestAReportWrittenOnAFallbackModelCarriesTheFirstRunsMarker(t *testing.T) {
	e := newEnv(t)
	var first string
	hit := hitsLimit(fableLimitPane)
	e.ag.behaviors[agents.RoleClaude] = []behavior{
		func(f *fakeAgents, run store.Run, text string) error { first = run.ID; return hit(f, run, text) },
		func(f *fakeAgents, run store.Run, text string) error {
			if strings.Contains(text, "magnum:run=") {
				t.Errorf("the fallback prompt names a marker: %q", text)
			}
			return writeRawReport(agents.ReportMarker(first)+"\n## P2 found on the fallback model\n")(f, run, text)
		},
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(613, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != ReportOK || got.RunID == first {
		t.Fatalf("claude report = %+v, want ok from the continuation", got)
	}
}
