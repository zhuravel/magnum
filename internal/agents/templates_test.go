package agents

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/prompts"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

func judgeFixture() JudgeData {
	return JudgeData{
		RunID:           "r-20261003T120000-7",
		Owner:           "talkable",
		Repo:            "talkable",
		Number:          11920,
		URL:             "https://github.com/talkable/talkable/pull/11920",
		HeadSHA:         "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3",
		BaseRef:         "master",
		BaseSHA:         "0123456789abcdef0123456789abcdef01234567",
		Checkout:        "/Users/bohdan/Projects/talkable.review3",
		IdentityKind:    "app",
		ReviewerLogin:   "talkable[bot]",
		GhConfigDir:     "/Users/bohdan/Projects/magnum/state/gh/talkable-app",
		NoFindingsEvent: "COMMENT",
		BlockingEvent:   "REQUEST_CHANGES",
		SelfAuthored:    false,
		Reports: []Report{
			{Role: "claude-review", Path: "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/claude-review.md"},
			{Role: "codex-review", Detail: "timed out after 40m"},
			{Role: "claude-simplify", Path: "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/claude-simplify.md"},
		},
		ResultFile:       "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/codex-judge.json",
		Magnum:           "/Users/bohdan/Projects/magnum/bin/magnum",
		Role:             "codex-judge",
		DryRun:           false,
		SkillPath:        "/Users/bohdan/Projects/magnum/skills/magnum-review/SKILL.md",
		PreviousReviewID: 3012345678,
		PreviousEvent:    "REQUEST_CHANGES",
		PreviousHeadSHA:  "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
		Since:            "2026-10-03T09:15:00Z",
		ForcePushed:      false,
		MovedFrom:        "",
		PreviousReviews: []PreviousReview{
			{ID: 3012345678, Event: "REQUEST_CHANGES", SHA: "a1b2c3d", SubmittedAt: "2026-10-03T09:15:00Z"},
			{ID: 3012399999, Event: "COMMENT", SHA: "b2c3d4e", SubmittedAt: "2026-10-03T10:40:00Z"},
		},
	}
}

// prompt resolves a configured prompt by name (the embedded defaults in
// tests), or prompts/<name>.next when that file exists: a prompt change that
// needs template fields the running daemon's binary lacks waits there until
// the restart onto the matching build swaps it in, so the goldens describe
// it before and after the swap.
func prompt(t *testing.T, name string) config.Prompt {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join("..", "..", "prompts", name+".next")); err == nil {
		return config.Prompt{Name: name, Text: string(b)}
	}
	p, err := config.Defaults().ResolvePrompt(name)
	if err != nil {
		t.Fatalf("ResolvePrompt(%s): %v", name, err)
	}
	return p
}

// defaultRole is the built-in role named r.
func defaultRole(t *testing.T, r Role) config.Role {
	t.Helper()
	role, ok := config.Defaults().RoleByNameOrAlias(nil, string(r))
	if !ok {
		t.Fatalf("no built-in role %q", r)
	}
	return role
}

func roleFixture() RoleData {
	return RoleData{
		URL: "https://github.com/talkable/talkable/pull/11920", Effort: "high",
		HeadSHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3", PreviousHeadSHA: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
		BaseSHA:    "0123456789abcdef0123456789abcdef01234567",
		ReportPath: "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/claude-review.md",
		Budget:     "40 minutes", RunID: "r-20261003T120000-7",
		Magnum: "/Users/bohdan/Projects/magnum/bin/magnum", Checkout: "/Users/bohdan/Projects/talkable.review3", Role: "claude-review",
	}
}

// triageFixture has the shape of the engine's triage data (internal/engine
// triageData, which this package cannot import); the engine checks the
// shipped prompt against the real type (CheckPrompts).
type triageFixture struct {
	Kind    string
	OwnDiff bool
	Lines   int
	Roles   []struct{ Name, Summary string }
	Diff    string
}

func triageFixtureFor(kind string) triageFixture {
	return triageFixture{Kind: kind, Lines: 5, Diff: "--- a/app/models/coupon.rb\n+++ b/app/models/coupon.rb\n@@ -10,3 +10,3 @@\n def expired?\n-  expires_at < Time.now\n+  expires_at <= Time.current\n end",
		Roles: []struct{ Name, Summary string }{
			{"claude-review", "deep review for bugs, security and correctness"},
			{"codex-review", "Codex's own static review of the diff"},
		}}
}

// curateFixture has the shape of the engine's curator data (internal/engine
// curateData); the engine checks the shipped prompt against the real type
// (CheckPrompts).
type curateFixture struct {
	Repo, Dir, Current, CurrentHarness, Usage, Proposal, Harness, Changes string
	Limits                                                                struct {
		MaxBytes        int64
		MaxLine         int
		MaxHarnessFiles int
		MaxHarnessBytes int64
	}
	Over         []string
	UnusedRounds int
	Misses       string
	MissCount    int
	Superseded   string
	SupersededID int64
}

func curateFixtureWith(over ...string) curateFixture {
	dir := "/data/notes/.curate/talkable/talkable/20261006-101500"
	f := curateFixture{Repo: "talkable/talkable", Dir: dir, Current: dir + "/current.md", CurrentHarness: dir + "/current",
		Usage: dir + "/usage.json", Proposal: dir + "/proposal.md", Harness: dir + "/harness", Changes: dir + "/changes.json",
		Over: over, UnusedRounds: 20}
	f.Limits.MaxBytes, f.Limits.MaxLine, f.Limits.MaxHarnessFiles, f.Limits.MaxHarnessBytes = 16384, 300, 15, 131072
	return f
}

// retroFixture has the shape of the engine's retro data (internal/engine
// retroData); the engine checks the shipped prompt against the real type
// (CheckPrompts).
type retroFixture struct {
	URL          string
	ReviewedSHAs []string
	Candidates   string
	Files        string
	Output       string
	Count        int
}

func retroFixtureWith(shas ...string) retroFixture {
	dir := "/data/learn/retro/20261005-070000/talkable/talkable/11920"
	return retroFixture{URL: "https://github.com/talkable/talkable/pull/11920", ReviewedSHAs: shas,
		Candidates: dir + "/candidates.json", Files: dir + "/files", Output: dir + "/retro.json", Count: 3}
}

// A reviewer's follow-up report holds what is new and no "Earlier findings"
// list: each report carried the previous one's list forward, so the lists
// snowballed and the judge rejected the same findings again every round (it
// tracks earlier findings through its own threads and the reply contract).
// The reviewer still does not re-derive a finding the new commits leave
// unchanged.
func TestClaudeFollowUpPromptsListNoEarlierFindings(t *testing.T) {
	restarted := roleFixture()
	restarted.Mode, restarted.RestartedFrom = ModeRestart, "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
	restartedForced := restarted
	restartedForced.ForcePushed = true
	forced := roleFixture()
	forced.ForcePushed = true
	for name, tc := range map[string]struct {
		file string
		data RoleData
	}{
		"rereview":              {"claude-rereview.md", roleFixture()},
		"rereview, force push":  {"claude-rereview.md", forced},
		"restart of a rereview": {"claude-restart.md", restarted},
		"restart, force push":   {"claude-restart.md", restartedForced},
	} {
		got, err := RenderPrompt(prompt(t, tc.file), tc.data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(strings.ToLower(got), "earlier findings") || !strings.Contains(got, "re-derive a finding") {
			t.Errorf("%s:\n%s", name, got)
		}
	}
}

// A judge that started fresh only because its prompt cache was cold reviews
// the commits since the earlier reviews in its own pass, as a re-review
// does (cold own passes averaged 31 responses, a resumed re-review's 5-12);
// a judge whose session was lost, or a push that rewrote history, reviews
// the whole PR. Both recovery prompts say when history was rewritten.
func TestAColdOwnPassReadsTheNewCommits(t *testing.T) {
	d := judgeFixture()
	d.Mode, d.Phase, d.OwnFindings, d.Reports = "recovery", PhaseOwnPass, "/state/reviews/judge-own.md", nil
	commits := "git log --oneline " + d.PreviousHeadSHA + ".." + d.HeadSHA
	rewrote := "The author rewrote history"
	render := func(name string, d JudgeData) string {
		t.Helper()
		got, err := RenderPrompt(prompt(t, name), d)
		if err != nil {
			t.Fatalf("RenderPrompt(%s): %v", name, err)
		}
		return got
	}
	cold := d
	cold.ColdJudge = true
	if got := render("judge-own-pass.md", cold); !strings.Contains(got, commits) || !strings.Contains(got, "the earlier reviews cover the rest of the PR") ||
		!strings.Contains(got, "force_pushed: false") {
		t.Errorf("a cold judge's own pass does not name the new commits:\n%s", got)
	}
	if got := render("judge-own-pass.md", d); strings.Contains(got, commits) {
		t.Errorf("a lost session's own pass names only the new commits:\n%s", got)
	}
	forced := cold
	forced.ForcePushed = true
	if got := render("judge-own-pass.md", forced); strings.Contains(got, commits) || !strings.Contains(got, rewrote) || !strings.Contains(got, "force_pushed: true") {
		t.Errorf("a cold judge's own pass after a force push:\n%s", got)
	}
	recovery := judgeFixture()
	recovery.ForcePushed = true
	if got := render("judge-recovery.md", recovery); !strings.Contains(got, rewrote) || !strings.Contains(got, "force_pushed: true") {
		t.Errorf("the recovery prompt after a force push:\n%s", got)
	}
}

func TestRenderGolden(t *testing.T) {
	forced := judgeFixture()
	forced.ForcePushed = true
	forced.MovedFrom = "/Users/bohdan/Projects/talkable.review1"
	forced.Checkout = "/Users/bohdan/Projects/talkable.review3"
	forced.DryRun = true
	forced.SelfAuthored = true
	effortJudge := judgeFixture()
	effortJudge.Effort, effortJudge.EffortInPrompt = "high", true

	forcedRole := roleFixture()
	forcedRole.ForcePushed, forcedRole.EffortInPrompt, forcedRole.Effort = true, true, "medium"
	restarted := roleFixture()
	restarted.Mode, restarted.PreviousHeadSHA = ModeRestart, ""
	restarted.RestartedFrom = "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
	restartedRereview := restarted
	restartedRereview.PreviousHeadSHA = roleFixture().PreviousHeadSHA
	restartedRereview.NotesPath = "/Users/bohdan/Projects/magnum/state/notes/talkable/talkable.md"
	restartedForced := restartedRereview
	restartedForced.ForcePushed, restartedForced.NotesPath = true, ""
	noBase := forcedRole
	noBase.BaseSHA, noBase.BaseRef, noBase.ForcePushed, noBase.EffortInPrompt, noBase.Effort = "", "origin/master", false, false, "high"

	blindJudge := judgeFixture()
	blindJudge.DryRun, blindJudge.Blind = true, true
	blindRole := roleFixture()
	blindRole.Blind = true
	// A post-merge round: the pipeline passes COMMENT for both events,
	// whatever the identity or repository says.
	postMerge := judgeFixture()
	postMerge.PostMerge, postMerge.NoFindingsEvent, postMerge.BlockingEvent = true, "COMMENT", "COMMENT"
	postMergeRole := roleFixture()
	postMergeRole.PostMerge = true
	// The simplify role writes its own report.
	simplifyData := func(d RoleData) RoleData {
		d.ReportPath, d.Role = filepath.Join(filepath.Dir(d.ReportPath), "claude-simplify.md"), "claude-simplify"
		return d
	}
	// The <magnum> fields of the repository notes (with a threads file, as a
	// re-review of a reviewed PR has).
	withNotes := judgeFixture()
	withNotes.NotesPath = "/Users/bohdan/Projects/magnum/state/notes/talkable/talkable.md"
	withNotes.NotesHarness, withNotes.NotesHarnessMore = []string{"fixtures/", "run-spec.sh"}, 0
	withNotes.ThreadsFile = "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/review-threads.json"
	withNotes.ThreadSummary = "3 threads (1 resolved); replies: 1 fixed, 1 not a bug; 1 thread without a reply"

	// A delta check: the judge alone, on a small delta whose files a JSON
	// file in the report directory lists.
	deltaCheck := judgeFixture()
	deltaCheck.DeltaCheck, deltaCheck.DeltaLines = true, 4
	deltaCheck.DeltaFile = "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/delta-check.json"

	// A curation given the retro's misses for the repository's notes, which
	// it reads from misses.json beside usage.json.
	curateMisses := curateFixtureWith("max_bytes")
	curateMisses.Misses, curateMisses.MissCount = curateMisses.Dir+"/misses.json", 2
	// A curation that follows up a stale proposal the operator (or the
	// daemon) superseded reads it from its scratch directory.
	curateSuperseded := curateFixtureWith("max_bytes")
	curateSuperseded.Superseded, curateSuperseded.SupersededID = curateSuperseded.Dir+"/superseded", 12
	// A re-review after a push that merged the base branch: the commits
	// since the previous head carry the base branch's, so the prompts hand
	// out the PR's own diff and compare it before and after.
	simplifyRereview := roleFixture()
	simplifyRereview.Mode = ModeRereview
	simplifyMerged := simplifyRereview
	simplifyMerged.BaseMerged = true
	recoveryMerged := judgeFixture()
	recoveryMerged.BaseMerged = true
	deltaCheckMerged := deltaCheck
	deltaCheckMerged.BaseMerged = true
	// A re-review of the head the judge last reviewed (no new commits): the
	// judge alone re-decides its earlier findings from the replies, in its
	// session or a fresh one.
	sameHead := withNotes
	sameHead.SameHead, sameHead.PreviousHeadSHA, sameHead.Reports = true, sameHead.HeadSHA, nil
	// A reply round: replies came on the review with no new commits, so the
	// judge alone re-decides the threads and may answer there instead of
	// posting a review (post_replies), in its session or a fresh one; one
	// thread is marked stop after two rebuttals. A continued turn of it keeps
	// the post_replies line. An ordinary re-review names a stopped thread too.
	replies := sameHead
	replies.Replies, replies.StopThreads = 2, 1
	stopped := withNotes
	stopped.StopThreads = 2

	// A round whose judge does its own pass while the reviewers work: the
	// own-pass prompt (no reports, no posting fields) per round kind, then
	// the candidates phase of the usual prompts.
	ownDir := "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/"
	ownPass := func(mode string) JudgeData {
		d := judgeFixture()
		d.Mode, d.Phase, d.OwnFindings, d.Reports = mode, PhaseOwnPass, ownDir+OwnFindingsFile, nil
		return d
	}
	ownRereview := ownPass("rereview")
	ownRereview.NotesPath, ownRereview.NotesHarness = withNotes.NotesPath, withNotes.NotesHarness
	ownRereview.ThreadsFile, ownRereview.ThreadSummary = withNotes.ThreadsFile, withNotes.ThreadSummary
	ownRecovery := ownPass("recovery")
	ownRecovery.FormerLogins = []string{"alice"}
	// A recovery whose judge started fresh only because its prompt cache
	// was cold: its own pass reads the commits since the earlier reviews,
	// unless the push rewrote history (the full PR again, as after a lost
	// session).
	ownCold := ownRecovery
	ownCold.ColdJudge = true
	ownColdForced := ownCold
	ownColdForced.ForcePushed = true
	recoveryForced := judgeFixture()
	recoveryForced.ForcePushed = true
	ownBlind := ownPass("initial")
	ownBlind.DryRun, ownBlind.Blind = true, true
	ownDryRun := ownPass("initial")
	ownDryRun.DryRun, ownDryRun.SelfAuthored = true, true
	ownRestart := ownPass("rereview")
	ownRestart.RestartedFrom = "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
	candidates := func(d JudgeData) JudgeData {
		d.Phase, d.OwnFindings = PhaseCandidates, ownDir+OwnFindingsFile
		return d
	}
	noOwn := candidates(judgeFixture())
	noOwn.OwnFindingsMissing = true
	// The related PRs: every judge prompt names related.json when the
	// repository has any (never in a blind replay: the pipeline writes none).
	withRelated := func(d JudgeData) JudgeData {
		d.RelatedPRs = ownDir + "related.json"
		return d
	}
	// The changed files' history: the judge prompts with a block (the
	// continued turn aside) name history.json, the claude reviewers get one
	// sentence naming it.
	withHistory := func(d JudgeData) JudgeData {
		d.HistoryFile = ownDir + "history.json"
		return d
	}
	// The head's failing checks: the posting judge prompts name the file.
	withFailingChecks := func(d JudgeData) JudgeData {
		d.FailingChecks = ownDir + "failing-checks.json"
		return d
	}
	roleHistory := func(d RoleData) RoleData {
		d.HistoryFile = ownDir + "history.json"
		return d
	}
	// A round whose Codex sessions ran with the checkout untrusted because
	// the PR changes .codex/: the posting judge prompts say so, for the
	// review's Checks.
	withCodexProject := func(d JudgeData) JudgeData {
		d.CodexProjectDeclined = true
		return d
	}
	// Its Claude sessions loaded the user's settings only because the PR
	// changes .claude/ or .mcp.json: the same prompts say so too.
	withClaudeProject := func(d JudgeData) JudgeData {
		d.ClaudeProjectDeclined = true
		return d
	}

	cases := []struct {
		golden, name string
		data         any
	}{
		{"simplify_rereview", "claude-simplify.md", simplifyData(simplifyRereview)},
		{"simplify_rereview_base_merged", "claude-simplify.md", simplifyData(simplifyMerged)},
		{"judge_recovery_base_merged", "judge-recovery.md", recoveryMerged},
		{"judge_recovery_delta_check_base_merged", "judge-recovery.md", deltaCheckMerged},
		{"judge_own_pass_initial", "judge-own-pass.md", ownPass("initial")},
		{"judge_own_pass_rereview", "judge-own-pass.md", ownRereview},
		{"judge_own_pass_recovery", "judge-own-pass.md", ownRecovery},
		{"judge_own_pass_recovery_cold", "judge-own-pass.md", ownCold},
		{"judge_own_pass_recovery_cold_forced", "judge-own-pass.md", ownColdForced},
		{"judge_recovery_forced", "judge-recovery.md", recoveryForced},
		{"judge_own_pass_blind", "judge-own-pass.md", ownBlind},
		{"judge_own_pass_dry_run", "judge-own-pass.md", ownDryRun},
		{"judge_own_pass_restart", "judge-own-pass.md", ownRestart},
		{"judge_own_pass_related", "judge-own-pass.md", withRelated(ownPass("initial"))},
		{"judge_initial_related", "judge-initial.md", withRelated(judgeFixture())},
		{"judge_recovery_candidates_related", "judge-recovery.md", withRelated(candidates(judgeFixture()))},
		{"judge_rereview_delta_check_related", "judge-rereview.md", withRelated(deltaCheck)},
		{"judge_continue_related", "judge-continue.md", withRelated(withNotes)},
		{"judge_own_pass_history", "judge-own-pass.md", withHistory(withRelated(ownPass("initial")))},
		{"judge_initial_history", "judge-initial.md", withHistory(withRelated(judgeFixture()))},
		{"judge_rereview_candidates_history", "judge-rereview.md", withHistory(candidates(withNotes))},
		{"judge_recovery_history", "judge-recovery.md", withHistory(judgeFixture())},
		{"judge_initial_failing_checks", "judge-initial.md", withFailingChecks(withHistory(candidates(judgeFixture())))},
		{"judge_rereview_failing_checks", "judge-rereview.md", withFailingChecks(withHistory(withNotes))},
		{"judge_continue_failing_checks", "judge-continue.md", withFailingChecks(withRelated(judgeFixture()))},
		{"judge_recovery_failing_checks", "judge-recovery.md", withFailingChecks(candidates(judgeFixture()))},
		{"claude_initial_history", "claude-review.md", roleHistory(roleFixture())},
		{"claude_rereview_history", "claude-rereview.md", roleHistory(roleFixture())},
		{"claude_restart_history", "claude-restart.md", roleHistory(restartedRereview)},
		{"judge_initial_codex_project", "judge-initial.md", withCodexProject(candidates(judgeFixture()))},
		{"judge_rereview_codex_project", "judge-rereview.md", withCodexProject(withNotes)},
		{"judge_recovery_codex_project", "judge-recovery.md", withCodexProject(judgeFixture())},
		{"judge_initial_claude_project", "judge-initial.md", withClaudeProject(withCodexProject(candidates(judgeFixture())))},
		{"judge_rereview_claude_project", "judge-rereview.md", withClaudeProject(withNotes)},
		{"judge_recovery_claude_project", "judge-recovery.md", withClaudeProject(judgeFixture())},
		{"judge_initial_candidates", "judge-initial.md", candidates(judgeFixture())},
		{"judge_initial_candidates_no_own", "judge-initial.md", noOwn},
		{"judge_rereview_candidates", "judge-rereview.md", candidates(withNotes)},
		{"judge_recovery_candidates", "judge-recovery.md", candidates(judgeFixture())},
		{"judge_rereview_delta_check", "judge-rereview.md", deltaCheck},
		// The same check by a judge in a fresh session (its old one is gone,
		// or the PR's identity migrated): the recovery prompt carries it.
		{"judge_recovery_delta_check", "judge-recovery.md", deltaCheck},
		{"judge_rereview_same_head", "judge-rereview.md", sameHead},
		{"judge_recovery_same_head", "judge-recovery.md", sameHead},
		{"judge_rereview_replies", "judge-rereview.md", replies},
		{"judge_recovery_replies", "judge-recovery.md", replies},
		{"judge_continue_replies", "judge-continue.md", replies},
		{"judge_rereview_stop", "judge-rereview.md", stopped},
		{"judge_initial", "judge-initial.md", judgeFixture()},
		{"judge_initial_blind", "judge-initial.md", blindJudge},
		{"judge_initial_post_merge", "judge-initial.md", postMerge},
		{"judge_rereview_post_merge", "judge-rereview.md", postMerge},
		{"claude_initial_post_merge", "claude-review.md", postMergeRole},
		{"claude_rereview_post_merge", "claude-rereview.md", postMergeRole},
		{"simplify_post_merge", "claude-simplify.md", simplifyData(postMergeRole)},
		{"claude_initial_blind", "claude-review.md", blindRole},
		{"simplify_blind", "claude-simplify.md", simplifyData(blindRole)},
		{"judge_rereview", "judge-rereview.md", judgeFixture()},
		{"judge_rereview_forced_moved", "judge-rereview.md", forced},
		{"judge_continue", "judge-continue.md", judgeFixture()},
		{"judge_nudge", "judge-nudge.md", judgeFixture()},
		{"judge_recovery", "judge-recovery.md", judgeFixture()},
		{"judge_rereview_effort", "judge-rereview.md", effortJudge},
		{"judge_initial_notes", "judge-initial.md", withNotes},
		{"judge_rereview_notes", "judge-rereview.md", withNotes},
		{"judge_continue_notes", "judge-continue.md", withNotes},
		{"judge_recovery_notes", "judge-recovery.md", withNotes},
		{"claude_initial", "claude-review.md", roleFixture()},
		{"claude_rereview", "claude-rereview.md", roleFixture()},
		{"claude_rereview_forced_effort", "claude-rereview.md", forcedRole},
		{"claude_restart", "claude-restart.md", restarted},
		{"claude_restart_rereview", "claude-restart.md", restartedRereview},
		{"claude_restart_rereview_forced", "claude-restart.md", restartedForced},
		{"claude_rereview_no_base", "claude-rereview.md", noBase},
		{"simplify", "claude-simplify.md", simplifyData(roleFixture())},
		{"model_fallback", FallbackPromptName, FallbackData{Model: "opus", Previous: "fable", Role: "claude-review",
			URL: "https://github.com/talkable/talkable/pull/11920", HeadSHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3",
			ReportPath: "/state/reviews/talkable/talkable/11920/d4e5f6a/claude-review.md"}},
		{"triage_initial", "triage.md", triageFixtureFor("initial")},
		{"triage_rereview", "triage.md", triageFixtureFor("rereview")},
		{"retro", "retro.md", retroFixtureWith("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0")},
		{"notes_curate", "notes-curate.md", curateFixtureWith("max_bytes", "max_harness_files")},
		{"notes_curate_within", "notes-curate.md", curateFixtureWith()},
		{"notes_curate_misses", "notes-curate.md", curateMisses},
		{"notes_curate_superseded", "notes-curate.md", curateSuperseded},
		{"retro_two_reviews", "retro.md", retroFixtureWith("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3")},
		{"model_fallback_no_report", FallbackPromptName, FallbackData{Model: "sonnet", Previous: "opus", Role: "claude-simplify",
			URL: "https://github.com/talkable/talkable/pull/11920", HeadSHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := RenderPrompt(prompt(t, tc.name), tc.data)
			if tc.golden == "notes_curate_misses" {
				// The misses are data in misses.json: the prompt names the
				// file and the rule, never a login, a PR or a miss's text.
				for _, leak := range []string{"rev-ann", "alice", "#11920", "pull/", "Coupon lookup"} {
					if strings.Contains(got, leak) {
						t.Errorf("the curator's prompt carries %q", leak)
					}
				}
			}
			if err != nil {
				t.Fatalf("RenderPrompt(%s): %v", tc.name, err)
			}
			checkGolden(t, tc.golden, got)
		})
	}

	codexReview := defaultRole(t, RoleCodexReview)
	withArgs := codexReview
	withArgs.Args = []string{"-c", "model_reasoning_effort=medium"} // after the role's effort, so it wins
	shells := []struct {
		golden string
		role   config.Role
		data   ShellData
	}{
		{"codex_review", codexReview, ShellData{BaseRef: "origin/master", Marker: DoneMarker("r-20261003T120000-8"),
			ReportPath: "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/codex-review.md"}},
		{"codex_review_args", withArgs, ShellData{BaseRef: "origin/feature/base-pr", ReportPath: "/tmp/with space/codex-review.md",
			Marker: DoneMarker("r-20261003T120000-9")}},
	}
	for _, tc := range shells {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := ShellLine(tc.role, tc.data)
			if err != nil {
				t.Fatalf("ShellLine: %v", err)
			}
			checkGolden(t, tc.golden, got)
			// The shipped full-line template types the same line.
			sh := tc.role
			sh.Command, sh.Prompt = "", "codex-review.sh"
			line, err := ShellLine(sh, tc.data)
			if err != nil || line != got {
				t.Fatalf("codex-review.sh line = %q, %v\nwant %q", line, err, got)
			}
		})
	}
}

func checkGolden(t *testing.T, golden, got string) {
	t.Helper()
	path := filepath.Join("testdata", golden+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
	if strings.Contains(got, "<no value>") {
		t.Errorf("%s left <no value>:\n%s", golden, got)
	}
}

func TestEveryDefaultPromptHasAGolden(t *testing.T) {
	want := []string{"claude-rereview.md", "claude-restart.md", "claude-review.md", "claude-simplify.md", "codex-review.sh", "judge-continue.md",
		"judge-initial.md", "judge-nudge.md", "judge-own-pass.md", "judge-recovery.md", "judge-rereview.md", "model-fallback.md", "notes-curate.md", "retro.md", "triage.md"}
	if got := prompts.Names(); !slices.Equal(got, want) {
		t.Fatalf("prompts.Names() = %v, want %v (add a golden case)", got, want)
	}
}

// Every judge prompt whose <magnum> block the skill reads (a post-merge
// round may be initial, rereview, continue or recovery) carries
// `post_merge: true` inside the block for a post-merge round, and only then.
func TestJudgePromptsSayPostMergeOnlyForAPostMergeRound(t *testing.T) {
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-continue.md", "judge-recovery.md"} {
		p := prompt(t, name)
		d := judgeFixture()
		normal, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(normal, "post_merge") {
			t.Errorf("%s names post_merge in a normal round:\n%s", name, normal)
		}
		d.PostMerge = true
		got, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		block := got[strings.Index(got, "<magnum>"):]
		if !strings.Contains(block, "\npost_merge: true\n") || !strings.HasSuffix(strings.TrimSpace(block), "</magnum>") {
			t.Errorf("%s: post_merge is not in the <magnum> block:\n%s", name, got)
		}
	}
}

// The re-review prompt says delta_check and asks for a short review of the
// commits since the last one only in a delta check: every other round's
// prompt stays as it was.
func TestJudgeRereviewSaysDeltaCheckOnlyForADeltaCheck(t *testing.T) {
	const instruction = "Only the commits since your last review changed (4 lines; files listed in /state/delta-check.json). " +
		"Review just those changes against the PR's purpose and your earlier findings; the rest stands as reviewed. Post one short review."
	p := prompt(t, "judge-rereview.md")
	d := judgeFixture()
	d.DeltaLines, d.DeltaFile = 4, "/state/delta-check.json" // ignored unless DeltaCheck
	normal, err := RenderPrompt(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if want, err := RenderPrompt(p, judgeFixture()); err != nil || normal != want {
		t.Fatalf("an ordinary re-review's prompt changed with the delta fields set (%v)", err)
	}
	if strings.Contains(normal, "delta_check") || strings.Contains(normal, "Only the commits since") {
		t.Fatalf("an ordinary re-review names the delta check:\n%s", normal)
	}
	d.DeltaCheck = true
	got, err := RenderPrompt(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\n"+instruction+"\n") {
		t.Errorf("the delta check's prompt lacks %q:\n%s", instruction, got)
	}
	block := got[strings.Index(got, "<magnum>"):]
	if !strings.Contains(block, "\ndelta_check: true\n") {
		t.Errorf("delta_check is not in the <magnum> block:\n%s", got)
	}
	// A file list that could not be written leaves its mention out.
	d.DeltaFile = ""
	if got, err := RenderPrompt(p, d); err != nil || !strings.Contains(got, "changed (4 lines). Review just") {
		t.Errorf("without the file: %v\n%s", err, got)
	}
}

// A reviewer's prompt that names the PR says it is merged, so the review
// does not stop at a closed PR, only in a post-merge round; the restart
// prompt too (a post-merge round never restarts, but a configured one may).
func TestRolePromptsSayTheMergedPRIsReviewedAnyway(t *testing.T) {
	const sentence = "The PR is already merged; review it anyway."
	for _, name := range []string{"claude-review.md", "claude-rereview.md", "claude-simplify.md", "claude-restart.md"} {
		p := prompt(t, name)
		d := roleFixture()
		d.RestartedFrom = "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
		normal, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(normal, "merged") {
			t.Errorf("%s mentions a merge in a normal round:\n%s", name, normal)
		}
		d.PostMerge = true
		got, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(got, sentence) {
			t.Errorf("%s lacks %q:\n%s", name, sentence, got)
		}
	}
}

func TestRenderPromptAcceptsPointer(t *testing.T) {
	d := judgeFixture()
	p := prompt(t, "judge-initial.md")
	a, err := RenderPrompt(p, &d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderPrompt(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("pointer render differs:\n%s\n%s", a, b)
	}
	if d.Reports[1].Missing || d.Reports[1].Label != "" {
		t.Fatalf("RenderPrompt modified its data: %+v", d.Reports[1])
	}
}

func TestJudgeInitialCountsReports(t *testing.T) {
	p := prompt(t, "judge-initial.md")
	d := judgeFixture()
	d.Reports = []Report{{Role: "omp-review", Path: "/r/omp-review.md"}}
	got, err := RenderPrompt(p, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Candidate reports from 1 independent reviewer role are listed below") ||
		!strings.Contains(got, "  - omp-review: /r/omp-review.md\n") {
		t.Fatalf("one report:\n%s", got)
	}
	// New-style fields: an explicit label, a missing report with a status only.
	d.Reports = []Report{
		{Role: "claude-review", Label: "Claude", Path: "/r/claude-review.md"},
		{Role: "codex-review", Path: "/r/codex-review.md", Missing: true, Status: "timeout"},
		{Role: "droid-simplify", Missing: true, Detail: "lost"},
	}
	if got, err = RenderPrompt(p, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Candidate reports from 3 independent reviewer roles are listed below",
		"  - Claude: /r/claude-review.md\n", "  - codex-review: missing (timeout)\n", "  - droid-simplify: missing (lost)\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	d.Reports = nil
	if got, err = RenderPrompt(p, d); err != nil || !strings.Contains(got, "No other reviewer role ran for this head; run your own full pass, then post.") {
		t.Fatalf("no reports = %v:\n%s", err, got)
	}
	// The older prompt shape ({{.Role}}, {{.Path}}, {{.Reason}}) keeps working.
	old := config.Prompt{Name: "old.md", Text: "{{range .Reports}}{{.Role}}: {{if .Path}}{{.Path}}{{else}}missing ({{.Reason}}){{end}};{{end}}"}
	d.Reports = []Report{{Role: "claude-review", Path: "/r/c.md"}, {Role: "codex-review", Detail: "failed"}, {Role: "x", Path: "/r/x.md", Missing: true, Status: "lost"}}
	if got, err = RenderPrompt(old, d); err != nil || got != "claude-review: /r/c.md;codex-review: missing (failed);x: missing (lost);" {
		t.Fatalf("old shape = %q, %v", got, err)
	}
}

func TestRenderPromptRoleData(t *testing.T) {
	p := config.Prompt{Name: "omp-review.md", Text: `Review {{.URL}} ({{.Owner}}/{{.Repo}}#{{.Number}}) at {{.HeadSHA}} against {{.BaseRef}} ({{.BaseSHA}}).
{{- if eq .Mode "rereview"}} Previous head {{.PreviousHeadSHA}}, comments since {{.Since}}.{{end}}
Model {{.Model}}, effort {{.Effort}}. Write {{.ReportPath}}.
`}
	d := RoleData{URL: "https://github.com/o/r/pull/7", Owner: "o", Repo: "r", Number: 7, HeadSHA: "h2", PreviousHeadSHA: "h1",
		BaseSHA: "b0", BaseRef: "origin/main", ReportPath: "/rep/omp-review.md", Model: "gpt-5.2", Effort: "high",
		Mode: ModeRereview, Since: "2026-10-03T09:15:00Z"}
	got, err := RenderPrompt(p, d)
	if err != nil {
		t.Fatal(err)
	}
	want := "Review https://github.com/o/r/pull/7 (o/r#7) at h2 against origin/main (b0). Previous head h1, comments since 2026-10-03T09:15:00Z.\n" +
		"Model gpt-5.2, effort high. Write /rep/omp-review.md."
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	d.Mode = ModeInitial
	if got, err = RenderPrompt(p, &d); err != nil || strings.Contains(got, "Previous head") {
		t.Fatalf("initial mode = %q, %v", got, err)
	}
}

func TestRolePrompt(t *testing.T) {
	e := newEnv(t)
	got, err := e.m.RolePrompt(e.spec(RoleClaude), config.PromptRereview, roleFixture())
	if err != nil || !strings.HasPrefix(got, "/code-review https://github.com/talkable/talkable/pull/11920 high\n\nNew commits were pushed") {
		t.Fatalf("claude-review rereview = %q, %v", got, err)
	}
	// prompts_dir wins over the embedded default.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude-review.md"), []byte("custom {{.URL}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.cfg.Pipeline.PromptsDir = dir
	if got, err := e.m.RolePrompt(e.spec(RoleClaude), config.PromptInitial, roleFixture()); err != nil || got != "custom https://github.com/talkable/talkable/pull/11920" {
		t.Fatalf("prompts_dir override = %q, %v", got, err)
	}
	// Non-judge roles have no continue prompt; unknown names wrap ErrUnknownTemplate.
	if _, err := e.m.RolePrompt(e.spec(RoleClaude), config.PromptContinue, roleFixture()); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("no continue prompt: err = %v", err)
	}
	r := e.spec(RoleClaude)
	r.Prompt = "nope.md"
	if _, err := e.m.RolePrompt(r, config.PromptInitial, roleFixture()); !errors.Is(err, ErrUnknownTemplate) || !errors.Is(err, config.ErrPromptNotFound) {
		t.Fatalf("unknown prompt: err = %v", err)
	}
}

func TestRenderErrors(t *testing.T) {
	// A data type without the template's fields fails instead of rendering blanks.
	if _, err := RenderPrompt(prompt(t, "judge-initial.md"), RoleData{}); err == nil {
		t.Fatal("judge-initial.md with RoleData: want error")
	}
	if _, err := RenderPrompt(config.Prompt{Name: "bad.md", Text: "{{.URL"}, RoleData{}); err == nil {
		t.Fatal("unparsable template: want error")
	}
	role := defaultRole(t, RoleCodexReview)
	// The shell line refuses markers that could break out of the echo.
	if _, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/x.md", Marker: "MAGNUM_DONE_r-1; rm -rf ~"}); err == nil {
		t.Fatal("unsafe marker: want error")
	}
	if _, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/x.md"}); err == nil {
		t.Fatal("no marker: want error")
	}
	if _, err := ShellLine(role, ShellData{BaseRef: "", ReportPath: "/tmp/x.md", Marker: DoneMarker("r-1")}); err == nil || !strings.Contains(err.Error(), ".BaseRef") {
		t.Fatalf("command using an empty base ref: err = %v", err)
	}
	if _, err := ShellLine(role, ShellData{BaseRef: "origin/master", Marker: DoneMarker("r-1")}); err == nil {
		t.Fatal("capture stdout without a report path: want error")
	}
	if _, err := ShellLine(defaultRole(t, RoleClaude), ShellData{Marker: DoneMarker("r-1")}); err == nil {
		t.Fatal("agent role: want error")
	}
	noMarker := role
	noMarker.Command, noMarker.Prompt = "", "x.sh"
	tmpl := &config.Prompt{Name: "x.sh", Text: "echo done"}
	if _, err := ShellLine(noMarker, ShellData{Marker: DoneMarker("r-1"), Template: tmpl}); err == nil {
		t.Fatal("a full-line template without the marker: want error")
	}
}

func TestCodexReviewScriptQuoting(t *testing.T) {
	role := defaultRole(t, RoleCodexReview)
	role.Args = []string{"$(whoami)"}
	got, err := ShellLine(role, ShellData{BaseRef: "origin/it's", ReportPath: "/tmp/a b/codex.md", Marker: DoneMarker("r-1")})
	if err != nil {
		t.Fatal(err)
	}
	want := `set -o pipefail; { printf '<!-- magnum:run=r-1 -->\n'; command codex review -c model_reasoning_effort=high --base 'origin/it'\''s' '$(whoami)'; } | tee '/tmp/a b/codex.md'; printf '\nMAGNUM_DONE_r-1 %d\n' "$?"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if DoneMarker("r-1") != "MAGNUM_DONE_r-1" {
		t.Fatalf("DoneMarker = %q", DoneMarker("r-1"))
	}
}

// codex-review set only its model, so `codex review` ran at the effort of
// the operator's global Codex config (xhigh): 23% of the Codex spend. Its
// command passes the role's effort (high) as a config override, the
// round's effort (ShellData.Effort) when given, a user's role effort when
// set, and the role's args after it, so a `-c model_reasoning_effort=` in
// args wins too (Codex applies -c overrides in order).
func TestCodexReviewRunsAtItsOwnEffortNotTheUsersGlobalOne(t *testing.T) {
	role := defaultRole(t, RoleCodexReview)
	if role.Effort != "high" {
		t.Fatalf("codex-review effort = %q, want high", role.Effort)
	}
	d := ShellData{BaseRef: "origin/master", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1")}
	line := func(r config.Role, d ShellData) string {
		t.Helper()
		got, err := ShellLine(r, d)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := line(role, d); !strings.Contains(got, "command codex review -c model_reasoning_effort=high --base origin/master; } |") {
		t.Fatalf("default: %s", got)
	}
	user := role
	user.Effort = "medium"
	if got := line(user, d); !strings.Contains(got, "command codex review -c model_reasoning_effort=medium --base origin/master; } |") {
		t.Fatalf("the user's effort: %s", got)
	}
	round := d
	round.Effort = "low"
	if got := line(role, round); !strings.Contains(got, "-c model_reasoning_effort=low --base") {
		t.Fatalf("the round's effort: %s", got)
	}
	args := role
	args.Args = []string{"-c", "model_reasoning_effort=minimal"}
	if got := line(args, d); !strings.Contains(got, "review -c model_reasoning_effort=high --base origin/master -c model_reasoning_effort=minimal; } |") {
		t.Fatalf("args after the effort: %s", got)
	}
	none := role
	none.Effort = ""
	if got := line(none, d); !strings.Contains(got, "command codex review --base origin/master; } |") {
		t.Fatalf("no effort: %s", got)
	}
	// The full-line template does the same.
	sh := role
	sh.Command, sh.Prompt = "", "codex-review.sh"
	if got := line(sh, d); !strings.Contains(got, "command codex review -c model_reasoning_effort=high --base origin/master; } |") {
		t.Fatalf("codex-review.sh: %s", got)
	}
}

func TestShellLineCommandArgsCapture(t *testing.T) {
	role := config.Role{Name: "lint", Kind: config.KindShell, Mode: config.ModeShell,
		Command: "bin/lint --since {{.BaseRef}} --url {{.URL}} --head {{.HeadSHA}}", Args: []string{"--format", "md table"},
		Capture: config.CaptureStdout}
	d := ShellData{Title: "PR #7 lint - r", BaseRef: "origin/main", URL: "https://github.com/o/r/pull/7", HeadSHA: "abc123",
		ReportPath: "/rep/lint.md", Marker: DoneMarker("r-5")}
	got, err := ShellLine(role, d)
	if err != nil {
		t.Fatal(err)
	}
	// capture = stdout: the run's marker, then the command's output, tee'd
	// into the report.
	want := `printf '\033]0;%s\007' 'PR #7 lint - r'; DISABLE_AUTO_TITLE=true; set -o pipefail; ` +
		`{ printf '<!-- magnum:run=r-5 -->\n'; bin/lint --since origin/main --url https://github.com/o/r/pull/7 --head abc123 --format 'md table'; } | tee /rep/lint.md; printf '\nMAGNUM_DONE_r-5 %d\n' "$?"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// capture = file: the command writes its report itself, no tee and no
	// marker.
	role.Capture = config.CaptureFile
	role.Command = "bin/lint --out {{.ReportPath}}"
	role.Args = nil
	d.Title = ""
	if got, err = ShellLine(role, d); err != nil || got != `set -o pipefail; bin/lint --out /rep/lint.md; printf '\nMAGNUM_DONE_r-5 %d\n' "$?"` {
		t.Fatalf("capture file = %q, %v", got, err)
	}
}

// Every shipped reviewer prompt, in every mode, asks for the run's marker
// as the report's first line: a report without it is stale.
func TestReviewerPromptsAskForTheRunMarker(t *testing.T) {
	for _, file := range []string{"claude-review.md", "claude-rereview.md", "claude-restart.md", "claude-simplify.md"} {
		for _, mode := range []string{ModeInitial, ModeRereview, ModeRestart} {
			d := roleFixture()
			d.Mode = mode
			if mode == ModeRestart {
				d.RestartedFrom = "f1cc4f9e0d1c2b3a4f5e6d7c8b9a0f1e2d3c4b5a"
			}
			got, err := RenderPrompt(prompt(t, file), d)
			if err != nil {
				t.Fatalf("%s (%s): %v", file, mode, err)
			}
			if !strings.Contains(got, "first line must be `"+ReportMarker(d.RunID)+"`") {
				t.Errorf("%s (%s) does not ask for the marker:\n%s", file, mode, got)
			}
		}
	}
}

// A report starts with its run's marker; ReportRun reads it back and what
// follows its line, past leading blank lines (a byte-order mark too).
func TestReportRunReadsTheMarkerOnAReportsFirstLine(t *testing.T) {
	if got := ReportMarker("r-20261003T120000-8"); got != "<!-- magnum:run=r-20261003T120000-8 -->" {
		t.Fatalf("ReportMarker = %q", got)
	}
	for _, tc := range []struct {
		name, report, run, rest string
	}{
		{"its marker", ReportMarker("r-1") + "\n## P2 x\n", "r-1", "## P2 x\n"},
		{"after blank lines", "\ufeff\n  \n" + ReportMarker("r-1") + "\r\n## P2 x\n", "r-1", "## P2 x\n"},
		{"the marker alone", ReportMarker("r-1"), "r-1", ""},
		{"no marker", "## P2 x\n", "", "## P2 x\n"},
		{"a marker below the first line", "# Review\n" + ReportMarker("r-1") + "\n", "", "# Review\n" + ReportMarker("r-1") + "\n"},
		{"a marker with text around it", "see " + ReportMarker("r-1") + "\n", "", "see " + ReportMarker("r-1") + "\n"},
		{"a malformed marker", "<!-- magnum:run=r 1 -->\n", "", "<!-- magnum:run=r 1 -->\n"},
		{"empty", "", "", ""},
	} {
		run, rest := ReportRun([]byte(tc.report))
		if run != tc.run || string(rest) != tc.rest {
			t.Errorf("%s: ReportRun = %q, %q; want %q, %q", tc.name, run, rest, tc.run, tc.rest)
		}
	}
}

func TestShellLinePrompt(t *testing.T) {
	role := config.Role{Name: "semgrep", Kind: config.KindShell, Mode: config.ModeShell, Prompt: "semgrep.sh",
		Args: []string{"--config", "p/ci"}, Capture: config.CaptureFile}
	tmpl := &config.Prompt{Name: "semgrep.sh", Text: "semgrep scan{{range .Args}} {{.}}{{end}} --json -o {{.ReportPath}} --baseline-commit {{.BaseRef}}; echo {{.Marker}}\n"}
	d := ShellData{BaseRef: "origin/it's", ReportPath: "/rep/semgrep.md", Marker: DoneMarker("r-6"), Template: tmpl}
	got, err := ShellLine(role, d)
	if err != nil {
		t.Fatal(err)
	}
	want := `semgrep scan --config p/ci --json -o /rep/semgrep.md --baseline-commit 'origin/it'\''s'; echo MAGNUM_DONE_r-6`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}

	// Manager.ShellLine resolves the template through prompts_dir.
	e := newEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "semgrep.sh"), []byte(tmpl.Text), 0o644); err != nil {
		t.Fatal(err)
	}
	e.cfg.Pipeline.PromptsDir = dir
	d.Template = nil
	if got, err := e.m.ShellLine(e.ctx, e.pr.ID, role, d); err != nil || got != want {
		t.Fatalf("Manager.ShellLine = %q, %v", got, err)
	}
	// The free function only knows the embedded defaults.
	if _, err := ShellLine(role, d); !errors.Is(err, config.ErrPromptNotFound) {
		t.Fatalf("ShellLine without a template: err = %v", err)
	}
}

var agentNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func TestAgentName(t *testing.T) {
	cases := []struct {
		repo   string
		number int
		role   Role
		want   string
	}{
		{"talkable/talkable", 11920, RoleJudge, "mg-11920-codex-judge-5d01cf"}, // 5d01cf = sha256("talkable/talkable")
		{"Talkable/TALKABLE", 11920, RoleJudge, "mg-11920-codex-judge-5d01cf"}, // GitHub names ignore case
		{"talkable/talkable", 11920, RoleClaude, "mg-11920-claude-review-5d01cf"},
		{"talkable/talkable", 11920, RoleCodexReview, "mg-11920-codex-review-5d01cf"},
		{"talkable/talkable", 11920, RoleSimplify, "mg-11920-claude-simplify-5d01cf"},
		{"talkable/talkable", 729, RoleSimplify, "mg-729-claude-simplify-5d01cf"},
		{"example/talkable", 11920, RoleJudge, "mg-11920-codex-judge-6fd941"},
		{"example/Web.App_v2", 7, RoleSimplify, "mg-7-claude-simplify-b133df"},
		// User-defined role names are sanitized.
		{"talkable/talkable", 7, "Omp Review!", "mg-7-omp-review-5d01cf"},
		{"talkable/talkable", 7, "--x__", "mg-7-x-5d01cf"},
		{"talkable/talkable", 7, "!!!", "mg-7-role-5d01cf"},
		// A role too long for the name is trimmed; its hash covers the role.
		{"talkable/talkable", 123456, "a-very-long-role-name-xyz", "mg-123456-a-very-long-rol-2a5066"},
		{"talkable/talkable", 123456, "a-very-long-role-name-abc", "mg-123456-a-very-long-rol-453bbb"},
	}
	for _, tc := range cases {
		got := AgentName(tc.repo, tc.number, tc.role)
		if got != tc.want {
			t.Errorf("AgentName(%q, %d, %s) = %q, want %q", tc.repo, tc.number, tc.role, got, tc.want)
		}
		if !agentNameRE.MatchString(got) {
			t.Errorf("AgentName(%q) = %q does not match herdr's name rule", tc.repo, got)
		}
	}
	// Even absurd inputs stay valid.
	for _, role := range []Role{RoleSimplify, "ÜBER Rolle", Role(strings.Repeat("Z!", 40)), ""} {
		got := AgentName(strings.Repeat("x", 100), 1234567890, role)
		if !agentNameRE.MatchString(got) {
			t.Errorf("AgentName(long, %q) = %q does not match", role, got)
		}
	}
}

// Names are unique per (owner, repo, number, role): herdr agent names are
// global, and a shared name makes one PR adopt another's agent.
func TestAgentNameUnique(t *testing.T) {
	seen := map[string]string{}
	for _, repo := range []string{"talkable/talkable", "talkable/talkable-frontend", "talkable/talkable-backend", "example/talkable"} {
		for _, n := range []int{7, 1234, 11920, 1234567} {
			for _, role := range []Role{RoleJudge, RoleClaude, RoleCodexReview, RoleSimplify, "claude-simplify-extra-a", "claude-simplify-extra-b"} {
				key := fmt.Sprintf("%s#%d %s", repo, n, role)
				name := AgentName(repo, n, role)
				if prev, dup := seen[name]; dup {
					t.Errorf("%s and %s share agent name %s", prev, key, name)
				}
				seen[name] = key
			}
		}
	}
}

// A tagged agent (magnum eval) never shares a name with the PR's own agent of
// the same role, still matches herdr's name rule, and is titled with its tag;
// the empty tag is the untagged name and title.
func TestTaggedAgentNameNeverMeetsThePRsOwnAgent(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	for _, role := range []Role{RoleJudge, RoleClaude, RoleCodexReview, RoleSimplify, "claude-simplify-extra-long-role"} {
		own, tagged := AgentName("talkable/talkable", 11932, role), TaggedAgentName("eval", "talkable/talkable", 11932, role)
		if own == tagged || !valid.MatchString(tagged) {
			t.Errorf("%s: own %q, tagged %q", role, own, tagged)
		}
		if TaggedAgentName("", "talkable/talkable", 11932, role) != own {
			t.Errorf("%s: empty tag changed the name", role)
		}
	}
	if got := TaggedTitle("eval", "talkable", 729, RoleJudge); got != "eval PR #729 codex-judge - talkable" {
		t.Errorf("tagged title %q", got)
	}
	if TaggedTitle("", "talkable", 729, RoleJudge) != Title("talkable", 729, RoleJudge) {
		t.Error("empty tag changed the title")
	}
}

func TestRoleLabel(t *testing.T) {
	for _, r := range []Role{RoleJudge, RoleClaude, RoleCodexReview, RoleSimplify, "droid-simplify", "some_new"} {
		if r.Label() != string(r) {
			t.Errorf("%s.Label() = %q", r, r.Label())
		}
	}
	if RoleJudge != "codex-judge" || RoleClaude != "claude-review" || RoleCodexReview != "codex-review" || RoleSimplify != "claude-simplify" {
		t.Fatal("built-in role names changed")
	}
}

// The own-pass prompt runs while the reviewers work: it names the file the
// pass goes to and the phase in its <magnum> block, and nothing the judge
// posts with or reads only later (no reports, result file, post-review line
// or events), whatever the round's kind.
func TestOwnPassPromptPostsNothing(t *testing.T) {
	p := prompt(t, "judge-own-pass.md")
	for _, mode := range []string{"initial", "rereview", "recovery"} {
		d := judgeFixture()
		d.Mode, d.Phase, d.OwnFindings = mode, PhaseOwnPass, "/state/reviews/x/judge-own.md"
		got, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		block := got[strings.Index(got, "<magnum>"):]
		for _, want := range []string{"\nmode: " + mode + "\n", "\nphase: own_pass\n", "\nown_findings: /state/reviews/x/judge-own.md\n", "\ndry_run: false\n"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: the block lacks %q:\n%s", mode, want, got)
			}
		}
		for _, leak := range []string{"reports:", "claude-review", "result_file", "post_review", "post-review", "codex-judge.json",
			"no_findings_event", "blocking_event"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: the own-pass prompt carries %q:\n%s", mode, leak, got)
			}
		}
		if !strings.Contains(got, "post nothing") {
			t.Errorf("%s: the own-pass prompt does not say to post nothing:\n%s", mode, got)
		}
	}
}

// The usual judge prompts name a phase and the own pass's file only in the
// candidates phase of a two-phase round: a round with one judge prompt
// renders them as before.
func TestJudgePromptsNamePhaseOnlyInATwoPhaseRound(t *testing.T) {
	for _, name := range []string{"judge-initial.md", "judge-rereview.md", "judge-recovery.md"} {
		p := prompt(t, name)
		one, err := RenderPrompt(p, judgeFixture())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(one, "phase") || strings.Contains(one, "own_findings") || strings.Contains(one, "own pass") {
			t.Errorf("%s names a phase in a one-prompt round:\n%s", name, one)
		}
		d := judgeFixture()
		d.Phase, d.OwnFindings = PhaseCandidates, "/state/reviews/x/judge-own.md"
		got, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		block := got[strings.Index(got, "<magnum>"):]
		if !strings.Contains(block, "\nphase: candidates\n") || !strings.Contains(block, "\nown_findings: /state/reviews/x/judge-own.md\n") ||
			!strings.Contains(block, "\npost_review: ") || !strings.Contains(got, "Your own pass") {
			t.Errorf("%s: candidates phase:\n%s", name, got)
		}
		d.OwnFindingsMissing = true
		if got, _ := RenderPrompt(p, d); !strings.Contains(got, "Your own pass left no /state/reviews/x/judge-own.md") {
			t.Errorf("%s: a missing own pass is not named:\n%s", name, got)
		}
	}
}
