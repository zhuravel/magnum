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
			{Role: "claude-simplify", Path: "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/claude-simplify.patch"},
		},
		ResultFile:       "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/codex-judge.json",
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
	// The <magnum> fields of an identity's footer and the repository notes
	// (with a threads file, as a re-review of a reviewed PR has).
	withNotes := judgeFixture()
	withNotes.Footer = "_Automated review by [Magnum](https://github.com/zhuravel/magnum). Reply on a thread with `fixed`, `not a bug: <why>` or `won't fix: <why>`; " +
		"simplifications are optional. New pushes are re-reviewed automatically._"
	withNotes.NotesPath = "/Users/bohdan/Projects/magnum/state/notes/talkable/talkable.md"
	withNotes.NotesHarness, withNotes.NotesHarnessMore = []string{"fixtures/", "run-spec.sh"}, 0
	withNotes.ThreadsFile = "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/review-threads.json"
	withNotes.ThreadSummary = "3 threads (1 resolved); replies: 1 fixed, 1 not a bug; 1 thread without a reply"

	// A delta check: the judge alone, on a small delta whose files a JSON
	// file in the report directory lists.
	deltaCheck := judgeFixture()
	deltaCheck.DeltaCheck, deltaCheck.DeltaLines = true, 4
	deltaCheck.DeltaFile = "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/delta-check.json"

	cases := []struct {
		golden, name string
		data         any
	}{
		{"judge_rereview_delta_check", "judge-rereview.md", deltaCheck},
		{"judge_initial", "judge-initial.md", judgeFixture()},
		{"judge_initial_blind", "judge-initial.md", blindJudge},
		{"judge_initial_post_merge", "judge-initial.md", postMerge},
		{"judge_rereview_post_merge", "judge-rereview.md", postMerge},
		{"claude_initial_post_merge", "claude-review.md", postMergeRole},
		{"claude_rereview_post_merge", "claude-rereview.md", postMergeRole},
		{"simplify_post_merge", "claude-simplify.md", postMergeRole},
		{"claude_initial_blind", "claude-review.md", blindRole},
		{"simplify_blind", "claude-simplify.md", blindRole},
		{"judge_rereview", "judge-rereview.md", judgeFixture()},
		{"judge_rereview_forced_moved", "judge-rereview.md", forced},
		{"judge_continue", "judge-continue.md", judgeFixture()},
		{"judge_nudge", "judge-nudge.md", judgeFixture()},
		{"judge_recovery", "judge-recovery.md", judgeFixture()},
		{"judge_rereview_effort", "judge-rereview.md", effortJudge},
		{"judge_initial_notes_footer", "judge-initial.md", withNotes},
		{"judge_rereview_notes_footer", "judge-rereview.md", withNotes},
		{"judge_continue_notes_footer", "judge-continue.md", withNotes},
		{"judge_recovery_notes_footer", "judge-recovery.md", withNotes},
		{"claude_initial", "claude-review.md", roleFixture()},
		{"claude_rereview", "claude-rereview.md", roleFixture()},
		{"claude_rereview_forced_effort", "claude-rereview.md", forcedRole},
		{"claude_restart", "claude-restart.md", restarted},
		{"claude_restart_rereview", "claude-restart.md", restartedRereview},
		{"claude_restart_rereview_forced", "claude-restart.md", restartedForced},
		{"claude_rereview_no_base", "claude-rereview.md", noBase},
		{"simplify", "claude-simplify.md", roleFixture()},
		{"model_fallback", FallbackPromptName, FallbackData{Model: "opus", Previous: "fable", Role: "claude-review",
			URL: "https://github.com/talkable/talkable/pull/11920", HeadSHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3",
			ReportPath: "/state/reviews/talkable/talkable/11920/d4e5f6a/claude-review.md"}},
		{"triage_initial", "triage.md", triageFixtureFor("initial")},
		{"triage_rereview", "triage.md", triageFixtureFor("rereview")},
		{"retro", "retro.md", retroFixtureWith("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0")},
		{"retro_two_reviews", "retro.md", retroFixtureWith("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3")},
		{"model_fallback_patch", FallbackPromptName, FallbackData{Model: "sonnet", Previous: "opus", Role: "claude-simplify",
			URL: "https://github.com/talkable/talkable/pull/11920", HeadSHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := RenderPrompt(prompt(t, tc.name), tc.data)
			if err != nil {
				t.Fatalf("RenderPrompt(%s): %v", tc.name, err)
			}
			checkGolden(t, tc.golden, got)
		})
	}

	codexReview := defaultRole(t, RoleCodexReview)
	withArgs := codexReview
	withArgs.Args = []string{"-c", "model_reasoning_effort=high"}
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
		"judge-initial.md", "judge-nudge.md", "judge-recovery.md", "judge-rereview.md", "model-fallback.md", "retro.md", "triage.md"}
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
		{Role: "droid-simplify", Missing: true, Detail: "no changes"},
	}
	if got, err = RenderPrompt(p, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Candidate reports from 3 independent reviewer roles are listed below",
		"  - Claude: /r/claude-review.md\n", "  - codex-review: missing (timeout)\n", "  - droid-simplify: missing (no changes)\n"} {
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
	d.Reports = []Report{{Role: "claude-review", Path: "/r/c.md"}, {Role: "codex-review", Detail: "failed"}, {Role: "x", Path: "/r/x.md", Missing: true, Status: "empty"}}
	if got, err = RenderPrompt(old, d); err != nil || got != "claude-review: /r/c.md;codex-review: missing (failed);x: missing (empty);" {
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
	want := `set -o pipefail; command codex review --base 'origin/it'\''s' '$(whoami)' | tee '/tmp/a b/codex.md'; printf '\nMAGNUM_DONE_r-1 %d\n' "$?"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if DoneMarker("r-1") != "MAGNUM_DONE_r-1" {
		t.Fatalf("DoneMarker = %q", DoneMarker("r-1"))
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
	want := `printf '\033]0;%s\007' 'PR #7 lint - r'; DISABLE_AUTO_TITLE=true; set -o pipefail; ` +
		`bin/lint --since origin/main --url https://github.com/o/r/pull/7 --head abc123 --format 'md table' | tee /rep/lint.md; printf '\nMAGNUM_DONE_r-5 %d\n' "$?"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// capture = file: the command writes its report itself, no tee.
	role.Capture = config.CaptureFile
	role.Command = "bin/lint --out {{.ReportPath}}"
	role.Args = nil
	d.Title = ""
	if got, err = ShellLine(role, d); err != nil || got != `set -o pipefail; bin/lint --out /rep/lint.md; printf '\nMAGNUM_DONE_r-5 %d\n' "$?"` {
		t.Fatalf("capture file = %q, %v", got, err)
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
	if got, err := e.m.ShellLine(role, d); err != nil || got != want {
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
