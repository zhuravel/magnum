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
	// codex review runs against the merge base, which does not move with the base branch.
	mustContain(t, "codex script", cc.Script, "command codex review --base base000111222333444555666777888999aaabbb |", filepath.Join(dir, "codex-review.md"), "MAGNUM_DONE_"+codexRun.ID)
	// The pane's terminal title (herdr sidebar) is set first; the marker stays last.
	if !strings.HasPrefix(cc.Script, `printf '\033]0;%s\007' 'PR #11920 codex-review - talkable'; DISABLE_AUTO_TITLE=true; set -o pipefail; command codex review `) ||
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
		"result_file: "+filepath.Join(dir, "codex-judge.json"), "dry_run: false")
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
	// GitHub: marker lookup plus REST login check.
	if len(e.gh.markerCalls) == 0 || e.gh.markerCalls[0] != "magnum:run="+judgeRun.ID || len(e.gh.restCalls) != 1 {
		t.Errorf("github calls: markers %v rest %v", e.gh.markerCalls, e.gh.restCalls)
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

func TestSimplifyCollectsPatchAndRestoresTree(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleSimplify] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		e.git.mu.Lock()
		e.git.head = "fffffffffffffffffffffffffffffffffffffff0" // it committed despite the prompt
		e.git.mu.Unlock()
		return f.end(run.ID)
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(511, "COMMENTED", "COMMENT").behavior(t)}
	patch := "diff --git a/app/x.rb b/app/x.rb\n--- a/app/x.rb\n+++ b/app/x.rb\n@@ -1 +1 @@\n-a\n+b\n"
	e.exec.Rules = []execx.Rule{
		{Prefix: []string{"git", "-C", slotPath, "diff"}, Result: execx.Result{Stdout: []byte(patch)}},
		{Prefix: []string{"git", "-C", slotPath, "reset", "--hard", "--quiet"}},
		{Prefix: []string{"git", "-C", slotPath, "clean", "-fd"}},
	}
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"} // magnum review --simplify

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted {
		t.Fatalf("result = %+v", res)
	}
	path := filepath.Join(e.reportDir(), "claude-simplify.patch")
	if got := res.Reports[agents.RoleSimplify]; got.Status != ReportOK || got.Path != path {
		t.Errorf("simplify report = %+v", got)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != patch {
		t.Errorf("claude-simplify.patch = %q, %v", b, err)
	}
	diff := e.exec.CallsWithPrefix("git", "-C", slotPath, "diff")
	if len(diff) != 1 || diff[0].Mutates || !strings.Contains(strings.Join(diff[0].Args, " "), target) {
		t.Errorf("diff calls = %+v", diff)
	}
	for _, prefix := range [][]string{{"git", "-C", slotPath, "reset", "--hard", "--quiet"}, {"git", "-C", slotPath, "clean", "-fd"}} {
		calls := e.exec.CallsWithPrefix(prefix...)
		if len(calls) != 1 || !calls[0].Mutates {
			t.Errorf("%v calls = %+v", prefix, calls)
		}
	}
	// Every git command the round runs outside gitx drops the variables that
	// would point git at another repository.
	gitCalls := e.exec.CallsWithPrefix("git")
	if len(gitCalls) == 0 {
		t.Fatal("no git command ran")
	}
	for _, c := range gitCalls {
		if !slices.Equal(c.Unset, gitx.ScrubbedEnv()) {
			t.Errorf("%s: Unset = %q, want gitx.ScrubbedEnv() %q", c, c.Unset, gitx.ScrubbedEnv())
		}
	}
	if len(e.git.switches) != 1 || e.git.switches[0] != target || e.git.head != target {
		t.Errorf("head not restored: switches %v head %s", e.git.switches, e.git.head)
	}
	// Simplify runs after both reviewers finished, before the judge.
	order := strings.Join(e.ag.order, " ")
	if !strings.HasSuffix(order, "submit:claude-simplify submit:codex-judge") {
		t.Errorf("call order = %s", order)
	}
	mustContain(t, "simplify prompt", e.ag.submitsFor(agents.RoleSimplify)[0].Text, "/simplify", in.BaseSHA)
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-simplify: "+path)
	if run := e.runOf(agents.RoleSimplify, store.RunInitial); run.State != store.RunVerified {
		t.Errorf("simplify run = %s / %v", run.State, store.Deref(run.Outcome))
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
