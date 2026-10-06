package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

func TestHappyPathInitialRound(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(501, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 501 || res.Event != "CHANGES_REQUESTED" {
		t.Fatalf("result = %+v", res)
	}
	if res.Findings["P2"] != 2 || res.ReviewCommit != target || !strings.Contains(res.ReviewURL, "pullrequestreview-501") {
		t.Errorf("findings/commit/url = %v %q %q", res.Findings, res.ReviewCommit, res.ReviewURL)
	}
	if res.Pause != nil || res.Nudged || res.DismissedReviewID != 0 {
		t.Errorf("unexpected pause/nudge/dismiss: %+v", res)
	}
	dir := e.reportDir()
	if res.ReportDir != dir {
		t.Errorf("ReportDir = %q, want %q", res.ReportDir, dir)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != ReportOK || got.Path != filepath.Join(dir, "claude-review.md") {
		t.Errorf("claude report = %+v", got)
	}
	if got := res.Reports[agents.RoleCodexReview]; got.Status != ReportOK || got.Path != filepath.Join(dir, "codex-review.md") {
		t.Errorf("codex report = %+v", got)
	}
	if _, ok := res.Reports[agents.RoleSimplify]; ok {
		t.Errorf("claude-simplify (runs manual) ran without a request: %+v", res.Reports)
	}

	// Prompts.
	claude := e.ag.submitsFor(agents.RoleClaude)
	if len(claude) != 1 {
		t.Fatalf("claude submits = %d", len(claude))
	}
	mustContain(t, "claude prompt", claude[0].Text, "/code-review https://github.com/talkable/talkable/pull/11920 high",
		filepath.Join(dir, "claude-review.md"), target)
	if len(e.ag.codexCalls) != 1 {
		t.Fatalf("codex calls = %d", len(e.ag.codexCalls))
	}
	cc := e.ag.codexCalls[0]
	codexRun := e.runOf(agents.RoleCodexReview, store.RunInitial)
	if cc.Pane != "p3" || cc.Marker != agents.DoneMarker(codexRun.ID) || cc.Timeout != e.cfg.Daemon.ReviewerTimeout.Duration {
		t.Errorf("codex call = %+v (run %s)", cc, codexRun.ID)
	}
	// codex review runs at the role's effort against the merge base, which
	// does not move with the base branch.
	mustContain(t, "codex script", cc.Script, "command codex review -c model_reasoning_effort=high --base base000111222333444555666777888999aaabbb; } |", filepath.Join(dir, "codex-review.md"), "MAGNUM_DONE_"+codexRun.ID)
	// The pane's terminal title (herdr sidebar) is set first, the report
	// starts with the run's marker; the done marker stays last.
	if !strings.HasPrefix(cc.Script, `printf '\033]0;%s\007' 'PR #11920 codex-review - talkable'; DISABLE_AUTO_TITLE=true; set -o pipefail; `+
		`{ printf '`+agents.ReportMarker(codexRun.ID)+`\n'; command codex review `) ||
		!strings.HasSuffix(cc.Script, `; printf '\nMAGNUM_DONE_`+codexRun.ID+` %d\n' "$?"`) {
		t.Errorf("codex script = %q", cc.Script)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 1 {
		t.Fatalf("judge submits = %d", len(judge))
	}
	judgeRun := e.runOf(agents.RoleJudge, store.RunInitial)
	mustContain(t, "judge prompt", judge[0].Text,
		"[$magnum-review](/repo/skills/magnum-review/SKILL.md)",
		"mode: initial", "run_id: "+judgeRun.ID, "pr: talkable/talkable#11920", "head_sha: "+target,
		"base_ref: master", "checkout: "+slotPath, "identity: app", "reviewer_login: talkable[bot]",
		"gh_config_dir: /state/gh/talkable-app", "no_findings_event: COMMENT", "blocking_event: REQUEST_CHANGES",
		"self_authored: false", "claude-review: "+filepath.Join(dir, "claude-review.md"), "codex-review: "+filepath.Join(dir, "codex-review.md"),
		"result_file: "+filepath.Join(dir, "codex-judge.json"), "dry_run: false",
		// The judge posts through this build's post-review, as the round's identity.
		"post_review: "+e.r.Layout.Binary()+" post-review --repo talkable/talkable --pr 11920 --head "+target+" --run-id "+judgeRun.ID+
			" --login 'talkable[bot]' --gh-config-dir /state/gh/talkable-app --review "+filepath.Join(dir, "review.json")+"\n")
	if res.JudgeRunID != judgeRun.ID {
		t.Errorf("JudgeRunID = %q, want %q", res.JudgeRunID, judgeRun.ID)
	}

	// Run rows.
	for _, r := range e.runs() {
		if r.Round != 1 || r.TargetSHA != target || r.Identity != "talkable-app" || r.ReviewerLogin != "talkable[bot]" {
			t.Errorf("run %s %s: round/target/identity/login = %d %s %s %s", r.ID, r.Role, r.Round, r.TargetSHA, r.Identity, r.ReviewerLogin)
		}
		if r.State != store.RunVerified {
			t.Errorf("run %s %s state = %s (outcome %v, err %v)", r.ID, r.Role, r.State, store.Deref(r.Outcome), store.Deref(r.Error))
		}
	}
	if judgeRun.Outcome == nil || *judgeRun.Outcome != OutcomePosted || store.Deref(judgeRun.ReviewID) != 501 ||
		store.Deref(judgeRun.ReviewEvent) != "CHANGES_REQUESTED" || store.Deref(judgeRun.ReviewCommit) != target ||
		judgeRun.ResultJSON == nil || judgeRun.VerifiedAt == nil {
		t.Errorf("judge run = %+v", judgeRun)
	}
	if codexRun.PromptText == "" || codexRun.SubmittedAt == nil || codexRun.EndedAt == nil {
		t.Errorf("codex run = %+v", codexRun)
	}
	// GitHub: marker lookup plus REST login check, then the footer's
	// author-checked edit (one more REST read, one update).
	if len(e.gh.markerCalls) == 0 || e.gh.markerCalls[0] != "magnum:run="+judgeRun.ID || len(e.gh.restCalls) != 2 || len(e.gh.updates) != 1 {
		t.Errorf("github calls: markers %v rest %v updates %d", e.gh.markerCalls, e.gh.restCalls, len(e.gh.updates))
	}
	if len(e.gh.dismissed) != 0 {
		t.Errorf("dismissed = %v", e.gh.dismissed)
	}
	// Audit events.
	var kinds []string
	for _, ev := range e.events() {
		kinds = append(kinds, ev.Kind)
	}
	mustContain(t, "event kinds", strings.Join(kinds, " "), "round.start", "round.judge", "round.end")
}

func TestReviewerTimeoutStillJudges(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{hang()}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(502, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Event != "COMMENTED" {
		t.Fatalf("result = %+v", res)
	}
	if got := res.Reports[agents.RoleClaude]; got.Status != ReportTimeout {
		t.Errorf("claude report = %+v", got)
	}
	if elapsed := e.clock.Now().Sub(t0); elapsed < e.cfg.Daemon.ReviewerTimeout.Duration {
		t.Errorf("elapsed %s < reviewer timeout", elapsed)
	}
	claudeRun := e.runOf(agents.RoleClaude, store.RunInitial)
	if claudeRun.State != store.RunFailed || store.Deref(claudeRun.Outcome) != ReportTimeout {
		t.Errorf("claude run = %s / %v", claudeRun.State, store.Deref(claudeRun.Outcome))
	}
	if want := "agent:" + agents.AgentName("talkable/talkable", 11920, agents.RoleClaude) + ":esc"; len(e.keys.sends) != 1 || e.keys.sends[0] != want {
		t.Errorf("interrupt keys = %v, want %s", e.keys.sends, want)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 1 {
		t.Fatalf("judge submits = %d", len(judge))
	}
	mustContain(t, "judge prompt", judge[0].Text, "claude-review: missing (timeout)", "codex-review: "+filepath.Join(e.reportDir(), "codex-review.md"))
}

func TestCodexReviewTimeoutInterruptsShell(t *testing.T) {
	e := newEnv(t)
	e.ag.codex = func(*fakeAgents, codexCall) error { return fmt.Errorf("wait: %w", agents.ErrTimeout) }
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(503, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Reports[agents.RoleCodexReview].Status != ReportTimeout {
		t.Fatalf("result = %+v", res)
	}
	if len(e.keys.sends) != 1 || e.keys.sends[0] != "pane:p3:ctrl+c" {
		t.Errorf("interrupt keys = %v", e.keys.sends)
	}
	run := e.runOf(agents.RoleCodexReview, store.RunInitial)
	if run.State != store.RunFailed || store.Deref(run.Outcome) != ReportTimeout {
		t.Errorf("codex run = %s / %v", run.State, store.Deref(run.Outcome))
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "codex-review: missing (timeout)")
}

func TestJudgeSilentUsageLimitPauses(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently()}
	e.ag.reads[agents.RoleJudge] = "• Working (12m)\n■ You've hit your usage limit. Upgrade to Pro or try again at 3:45 PM.\n"

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeUsageLimit || res.Pause == nil {
		t.Fatalf("result = %+v", res)
	}
	if res.Pause.Kind != string(agents.HealthUsageLimit) || res.Pause.Tool != agents.KindCodex || res.Pause.Until.IsZero() {
		t.Errorf("pause = %+v", res.Pause)
	}
	if res.Nudged || len(e.ag.submitsFor(agents.RoleJudge)) != 1 {
		t.Errorf("nudged on a usage limit: %+v", e.ag.submitsFor(agents.RoleJudge))
	}
	run := e.runOf(agents.RoleJudge, store.RunInitial)
	if run.State != store.RunFailed || store.Deref(run.Outcome) != OutcomeUsageLimit {
		t.Errorf("judge run = %s / %v", run.State, store.Deref(run.Outcome))
	}
}

func TestJudgeNudgeThenPosts(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently(), e.judgePosts(504, "COMMENTED", "COMMENT").behavior(t)}
	e.ag.reads[agents.RoleJudge] = "Done reading the diff.\n"

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || !res.Nudged || res.ReviewID != 504 {
		t.Fatalf("result = %+v", res)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 2 {
		t.Fatalf("judge submits = %d", len(judge))
	}
	first := e.runOf(agents.RoleJudge, store.RunInitial)
	nudge := e.runOf(agents.RoleJudge, store.RunNudge)
	// The nudge quotes the original run id, so the review carries one marker.
	mustContain(t, "nudge prompt", judge[1].Text, "You stopped without posting", "run_id "+first.ID, filepath.Join(e.reportDir(), "codex-judge.json"))
	for _, r := range []store.Run{first, nudge} {
		if r.State != store.RunVerified || store.Deref(r.Outcome) != OutcomePosted || store.Deref(r.ReviewID) != 504 {
			t.Errorf("run %s (%s) = %s / %v / %v", r.ID, r.Kind, r.State, store.Deref(r.Outcome), store.Deref(r.ReviewID))
		}
	}
	if nudge.Round != first.Round {
		t.Errorf("nudge round %d != %d", nudge.Round, first.Round)
	}
}

func TestJudgeNudgeNothingNeedsAttention(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently()}
	e.ag.reads[agents.RoleJudge] = "Done.\n"

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeNeedsAttention || !res.Nudged || res.Pause != nil {
		t.Fatalf("result = %+v", res)
	}
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 2 {
		t.Errorf("judge submits = %d, want 2 (prompt + one nudge)", n)
	}
	for _, r := range e.runs() {
		if r.Role == string(agents.RoleJudge) && (r.State != store.RunFailed || store.Deref(r.Outcome) != OutcomeNeedsAttention) {
			t.Errorf("judge run %s (%s) = %s / %v", r.ID, r.Kind, r.State, store.Deref(r.Outcome))
		}
	}
}

func TestIdentityLeak(t *testing.T) {
	cases := map[string]func(e *env) judgePost{
		"graphql author is the user": func(e *env) judgePost {
			p := e.judgePosts(505, "COMMENTED", "COMMENT")
			p.graphLogin, p.graphType, p.restLogin, p.restType = "zhuravel", "User", "zhuravel", "User"
			return p
		},
		"rest login differs": func(e *env) judgePost {
			p := e.judgePosts(505, "COMMENTED", "COMMENT")
			p.restLogin, p.restType = "zhuravel", "User"
			return p
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.ag.behaviors[agents.RoleJudge] = []behavior{mk(e).behavior(t)}
			res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
			if err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			if res.Outcome != OutcomeIdentityLeak || res.ReviewID != 505 {
				t.Fatalf("result = %+v", res)
			}
			if !strings.Contains(res.Error, "zhuravel") {
				t.Errorf("error does not name the leaked login: %q", res.Error)
			}
			run := e.runOf(agents.RoleJudge, store.RunInitial)
			if run.State != store.RunFailed || store.Deref(run.Outcome) != OutcomeIdentityLeak {
				t.Errorf("judge run = %s / %v", run.State, store.Deref(run.Outcome))
			}
			if len(e.gh.dismissed) != 0 {
				t.Errorf("dismissed after a leak: %v", e.gh.dismissed)
			}
		})
	}
}

func TestDryRun(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(0, "", "COMMENT")
	p.gh, p.status, p.findings = nil, "dry_run", map[string]int{"P3": 1}
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindInitial)
	in.DryRun = true

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeDryRun || res.Event != "COMMENTED" || res.Findings["P3"] != 1 || res.ReviewID != 0 {
		t.Fatalf("result = %+v", res)
	}
	if n := e.gh.calls(); n != 0 {
		t.Errorf("GitHub calls under dry run = %d", n)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "dry_run: true")
	run := e.runOf(agents.RoleJudge, store.RunInitial)
	if run.State != store.RunVerified || store.Deref(run.Outcome) != OutcomeDryRun {
		t.Errorf("judge run = %s / %v", run.State, store.Deref(run.Outcome))
	}
}

func TestRereviewDismissesStaleChangesRequested(t *testing.T) {
	prev := &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour)}
	t.Run("dismissed", func(t *testing.T) {
		e := newEnv(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(506, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindRereview)
		in.Round, in.Previous, in.Since = 2, prev, t0.Add(-3*time.Hour)

		res, err := e.r.RunRound(e.ctx, in)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if res.Outcome != OutcomePosted || res.DismissedReviewID != 900 {
			t.Fatalf("result = %+v", res)
		}
		if len(e.gh.dismissed) != 1 || e.gh.dismissed[0].ID != 900 || !strings.Contains(e.gh.dismissed[0].Message, "pullrequestreview-506") {
			t.Errorf("dismissed = %+v", e.gh.dismissed)
		}
		mustContain(t, "claude prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, prevSHA+"` → `"+target)
		mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "mode: rereview",
			"previous_review_id: 900", "previous_head_sha: "+prevSHA, "since: 2026-10-03T09:00:00Z", "force_pushed: false")
		for _, r := range e.runs() {
			if r.Round != 2 || r.Kind != store.RunRereview {
				t.Errorf("run %s: round %d kind %s", r.Role, r.Round, r.Kind)
			}
		}
	})
	t.Run("forbidden is a warning", func(t *testing.T) {
		e := newEnv(t)
		e.gh.dismissErr = fmt.Errorf("dismiss: %w", github.ErrForbidden)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(507, "APPROVED", "APPROVE").behavior(t)}
		in := e.input(KindRereview)
		in.Previous = prev

		res, err := e.r.RunRound(e.ctx, in)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if res.Outcome != OutcomePosted || res.DismissedReviewID != 0 || len(e.gh.dismissed) != 1 {
			t.Fatalf("result = %+v, dismissed %v", res, e.gh.dismissed)
		}
		if !strings.Contains(strings.Join(res.Warnings, "\n"), "dismiss") {
			t.Errorf("warnings = %v", res.Warnings)
		}
	})
	t.Run("still blocking keeps the old review", func(t *testing.T) {
		e := newEnv(t)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(508, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
		in := e.input(KindRereview)
		in.Previous = prev
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if res.Outcome != OutcomePosted || len(e.gh.dismissed) != 0 {
			t.Fatalf("result = %+v, dismissed %v", res, e.gh.dismissed)
		}
	})
	t.Run("gh identity does not dismiss by default", func(t *testing.T) {
		e := newEnv(t)
		e.r.Identity = fakeIdentity{name: "zhuravel", login: "zhuravel", kind: "gh"}
		p := e.judgePosts(509, "COMMENTED", "COMMENT")
		p.graphLogin, p.graphType, p.restLogin, p.restType = "zhuravel", "User", "zhuravel", "User"
		e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
		in := e.input(KindRereview)
		in.Previous = prev
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		if res.Outcome != OutcomePosted || len(e.gh.dismissed) != 0 {
			t.Fatalf("result = %+v, dismissed %v", res, e.gh.dismissed)
		}
		mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "identity: gh", "reviewer_login: zhuravel",
			"gh_config_dir: \n", "no_findings_event: APPROVE")
	})
}

func TestSelfAuthored(t *testing.T) {
	e := newEnv(t)
	e.pr.AuthorLogin = store.Ptr("Zhuravel")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(510, "COMMENTED", "COMMENT").behavior(t)}
	if _, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "self_authored: true")
}

// The read-only simplify runs in the reviewers' stage, not after it: it is
// prompted while claude-review still works, and its report is the file it
// writes, which the judge reads like any reviewer's.
func TestSimplifyRunsAlongsideTheReviewersAndWritesItsReport(t *testing.T) {
	e := newEnv(t)
	prompted := make(chan struct{})
	var overlapped bool
	e.ag.behaviors[agents.RoleSimplify] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		close(prompted)
		return writeReport("## 1. Simplification: drop the copy\n")(f, run, text)
	}}
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		select {
		case <-prompted:
			overlapped = true
		case <-time.After(5 * time.Second):
		}
		return writeReport("## P2 something\n")(f, run, text)
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(511, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"} // magnum review --simplify

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v", res)
	}
	if !overlapped {
		t.Error("claude-simplify was not prompted while claude-review worked")
	}
	path := filepath.Join(e.reportDir(), "claude-simplify.md")
	if got := res.Reports[agents.RoleSimplify]; got.Status != ReportOK || got.Path != path {
		t.Errorf("simplify report = %+v, want ok at %s", got, path)
	}
	mustContain(t, "simplify prompt", e.ag.submitsFor(agents.RoleSimplify)[0].Text,
		"never edit", "Reuse:", "Simplification:", "Efficiency:", "Altitude:", path, in.BaseSHA)
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-simplify: "+path)
	if run := e.runOf(agents.RoleSimplify, store.RunInitial); run.State != store.RunVerified {
		t.Errorf("simplify run = %s / %v", run.State, store.Deref(run.Outcome))
	}
	// A clean checkout is left alone: no git command ran outside gitx.
	if calls := e.exec.CallsWithPrefix("git"); len(calls) != 0 {
		t.Errorf("git calls = %+v", calls)
	}
}

// restoreRules stub the restore's git commands; the reset cleans the fake
// tree unless stillDirty.
func (e *env) restoreRules(stillDirty bool) {
	e.exec.Rules = []execx.Rule{
		{Prefix: []string{"git", "-C", slotPath, "reset", "--hard", "--quiet"}, Fn: func(execx.Cmd) (execx.Result, error) {
			if !stillDirty {
				e.git.mu.Lock()
				e.git.status = gitx.Status{}
				e.git.mu.Unlock()
			}
			return execx.Result{}, nil
		}},
		{Prefix: []string{"git", "-C", slotPath, "clean", "-fd"}},
	}
}

// dirty is a behavior that edits the checkout (and commits, moving HEAD)
// despite the prompt, then writes its report.
func (e *env) dirty(head string) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		e.git.mu.Lock()
		e.git.status = gitx.Status{Tracked: 2, Untracked: 1}
		if head != "" {
			e.git.head = head
		}
		e.git.mu.Unlock()
		return writeReport("## 1. Simplification\n")(f, run, text)
	}
}

// A role that edits the checkout is caught after its stage: the round warns
// (round.checkout_dirty), resets and cleans the tree, switches HEAD back to
// the target, and only then prompts the judge.
func TestAReviewerThatEditsTheCheckoutIsCaughtAndReset(t *testing.T) {
	e := newEnv(t)
	e.restoreRules(false)
	e.ag.behaviors[agents.RoleSimplify] = []behavior{e.dirty("fffffffffffffffffffffffffffffffffffffff0")}
	var judgeSaw gitx.Status
	judge := e.judgePosts(512, "COMMENTED", "COMMENT").behavior(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		e.git.mu.Lock()
		judgeSaw = e.git.status
		e.git.mu.Unlock()
		return judge(f, run, text)
	}}
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if judgeSaw.Dirty() {
		t.Errorf("the judge ran on a dirty checkout: %+v", judgeSaw)
	}
	for _, prefix := range [][]string{{"git", "-C", slotPath, "reset", "--hard", "--quiet"}, {"git", "-C", slotPath, "clean", "-fd"}} {
		calls := e.exec.CallsWithPrefix(prefix...)
		if len(calls) != 1 || !calls[0].Mutates || !slices.Equal(calls[0].Unset, gitx.ScrubbedEnv()) {
			t.Errorf("%v calls = %+v", prefix, calls)
		}
	}
	if len(e.git.switches) != 1 || e.git.switches[0] != target || e.git.head != target {
		t.Errorf("head not restored: switches %v head %s", e.git.switches, e.git.head)
	}
	var warned bool
	for _, ev := range e.events() {
		if ev.Kind == "round.checkout_dirty" {
			warned = true
			mustContain(t, "event", ev.Message, "claude-review", "claude-simplify", "HEAD moved to fffffff", "2 tracked and 1 untracked")
		}
	}
	if !warned {
		t.Error("no round.checkout_dirty event")
	}
}

// A checkout that stays dirty after the restore fails the round before the
// judge: nothing may run on it.
func TestDirtyCheckoutAfterRestoreFailsRound(t *testing.T) {
	e := newEnv(t)
	e.restoreRules(true)
	e.ag.behaviors[agents.RoleClaude] = []behavior{e.dirty("")}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "still dirty") {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if len(e.ag.submitsFor(agents.RoleJudge)) != 0 {
		t.Fatal("the judge must not run")
	}
}

// A file the readiness step left modified is no role's edit: the check
// compares with the checkout as the stages found it, so it neither warns nor
// resets.
func TestACheckoutDirtyBeforeTheReviewersIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.git.status = gitx.Status{Tracked: 1}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(513, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if calls := e.exec.CallsWithPrefix("git"); len(calls) != 0 {
		t.Errorf("git calls = %+v", calls)
	}
	for _, ev := range e.events() {
		if ev.Kind == "round.checkout_dirty" {
			t.Errorf("unexpected event: %s", ev.Message)
		}
	}
}

func TestResultFileCompletesJudge(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(512, "COMMENTED", "COMMENT")
	p.keepWorking = true // the agent never goes idle
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 512 {
		t.Fatalf("result = %+v", res)
	}
	if elapsed := e.clock.Now().Sub(t0); elapsed < ResultSettle || elapsed >= e.cfg.Daemon.JudgeTimeout.Duration {
		t.Errorf("elapsed = %s", elapsed)
	}
}

func TestJudgeBlockedStatus(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(0, "", "")
	p.gh, p.status = nil, "blocked"
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeBlocked || res.Nudged {
		t.Fatalf("result = %+v", res)
	}
}

func TestMarkerlessReviewAcceptedWithWarning(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(513, "COMMENTED", "COMMENT")
	p.noMarker, p.status = true, ""
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 513 || !strings.Contains(strings.Join(res.Warnings, "\n"), "marker") {
		t.Fatalf("result = %+v", res)
	}
}

func TestMarkerNeedsExactRunID(t *testing.T) {
	e := newEnv(t)
	// A review of another run whose id extends this one's must not count.
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		id := markerRunID(t, text)
		e.gh.add(github.Review{DatabaseID: 514, State: "COMMENTED", Body: "<!-- magnum:run=" + id + "0 head=abc1234 -->",
			CommitOid: prevSHA, AuthorLogin: "talkable", AuthorType: "Bot", SubmittedAt: t0.Add(-time.Hour)},
			github.RESTReview{ID: 514, UserLogin: "talkable[bot]", UserType: "Bot", CommitID: prevSHA})
		return f.end(run.ID)
	}}
	e.ag.reads[agents.RoleJudge] = "Done.\n"
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeNeedsAttention || res.ReviewID != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestCodexLoginRequiredPausesBeforePrompting(t *testing.T) {
	e := newEnv(t)
	e.ag.preflight[agents.KindCodex] = fmt.Errorf("codex: %w", agents.ErrLoginRequired)
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeLoginRequired || res.Pause == nil || res.Pause.Tool != agents.KindCodex || res.Pause.Kind != string(agents.HealthLoginRequired) {
		t.Fatalf("result = %+v", res)
	}
	if len(e.ag.submits) != 0 || len(e.ag.codexCalls) != 0 {
		t.Errorf("prompted while logged out: %v %v", e.ag.submits, e.ag.codexCalls)
	}
	for _, r := range e.runs() {
		if r.State == store.RunPending {
			t.Errorf("run %s left pending", r.ID)
		}
	}
}

func TestClaudeLoginRequiredSkipsReviewerOnly(t *testing.T) {
	e := newEnv(t)
	e.ag.preflight[agents.KindClaude] = fmt.Errorf("claude: %w", agents.ErrLoginRequired)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(515, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Reports[agents.RoleClaude].Status != string(agents.HealthLoginRequired) {
		t.Fatalf("result = %+v", res)
	}
	if len(e.ag.submitsFor(agents.RoleClaude)) != 0 {
		t.Errorf("claude prompted while logged out")
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-review: missing (login_required)")
}

func TestContinueSkipsReviewersAndReusesReports(t *testing.T) {
	e := newEnv(t)
	dir := e.reportDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-review.md"), []byte("old claude"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The paused round verified claude-review's report.
	if _, err := e.st.CreateRun(e.ctx, store.Run{PRID: e.pr.ID, Round: 1, Role: string(agents.RoleClaude), Kind: store.RunInitial,
		TargetSHA: target, State: store.RunVerified, Outcome: store.Ptr(ReportOK), ReportPath: store.Ptr(filepath.Join(dir, "claude-review.md"))}); err != nil {
		t.Fatal(err)
	}
	if err := writeJudgeJSON(filepath.Join(dir, "codex-judge.json"), map[string]any{"status": "error"}); err != nil {
		t.Fatal(err)
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(516, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindContinue)
	in.ContinueRunID = "r-20261003T100000-7"

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 516 {
		t.Fatalf("result = %+v", res)
	}
	if len(e.ag.submitsFor(agents.RoleClaude)) != 0 || len(e.ag.codexCalls) != 0 {
		t.Errorf("reviewers ran on continue")
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	mustContain(t, "continue prompt", judge[0].Text, "mode: continue", "run_id: r-20261003T100000-7")
	if res.Reports[agents.RoleClaude].Status != ReportOK || res.Reports[agents.RoleCodexReview].Status != ReportMissing {
		t.Errorf("reports = %+v", res.Reports)
	}
	if _, err := os.Stat(filepath.Join(dir, "codex-judge.json.prev")); err != nil {
		t.Errorf("stale codex-judge.json not set aside: %v", err)
	}
	if run := e.runOf(agents.RoleJudge, store.RunContinue); run.State != store.RunVerified {
		t.Errorf("continue run = %s", run.State)
	}
}

func TestCancelledRoundStops(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		cancel()
		return nil
	}}
	res, err := e.r.RunRound(ctx, e.input(KindInitial))
	if !errorsIs(err, context.Canceled) || res.Outcome != OutcomeStopped {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	for _, r := range e.runs() {
		if r.State == store.RunPending {
			t.Errorf("run %s (%s) left pending", r.ID, r.Role)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	in := e.input("bogus")
	if _, err := e.r.RunRound(e.ctx, in); err == nil {
		t.Errorf("unknown kind accepted")
	}
	in = e.input(KindInitial)
	in.TargetSHA = ""
	if _, err := e.r.RunRound(e.ctx, in); err == nil {
		t.Errorf("empty target accepted")
	}
}
