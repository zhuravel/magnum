package registry_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func attemptFor(t *testing.T, r *registry.Registry, taskID int64, stage string) registry.Attempt {
	t.Helper()
	a, err := r.BeginAttempt(t.Context(), taskID, stage)
	if err != nil {
		t.Fatalf("BeginAttempt(%d, %s): %v", taskID, stage, err)
	}
	return a
}

func TestRecordRoleRunKeepsEarlierFields(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a := attemptFor(t, r, task.ID, "investigate")

	launch := registry.RoleRun{AttemptID: a.ID, Role: "claude-reproducer", CLI: "claude", Model: "opus", CLIVersion: "2.1.4",
		MagnumBuild: "abc1234", PromptHash: "p1", SkillHash: "s1", ConfigHash: "c1", SessionID: "sess-1",
		Transcript: "/transcripts/sess-1.jsonl", Rules: []registry.RuleRef{{ID: "r-7", Version: 2}}, Launched: t0}
	if err := r.RecordRoleRun(ctx, launch); err != nil {
		t.Fatal(err)
	}
	finish := registry.RoleRun{AttemptID: a.ID, Role: "claude-reproducer", FirstOutput: t0.Add(5 * time.Second),
		Ended: t0.Add(10 * time.Minute), Verified: t0.Add(11 * time.Minute), EndFact: "reproduction.md written",
		EndWhy: "the file exists and names a failing spec", Nudges: 2, Outcome: "verified",
		Tokens: map[string]int64{"opus": 1200, "haiku": 40}, ActiveSecs: 540, IdleSecs: 60}
	if err := r.RecordRoleRun(ctx, finish); err != nil {
		t.Fatal(err)
	}

	want := launch
	want.FirstOutput, want.Ended, want.Verified = finish.FirstOutput, finish.Ended, finish.Verified
	want.EndFact, want.EndWhy, want.Nudges, want.Outcome = finish.EndFact, finish.EndWhy, finish.Nudges, finish.Outcome
	want.Tokens, want.ActiveSecs, want.IdleSecs = finish.Tokens, finish.ActiveSecs, finish.IdleSecs
	got, err := r.RoleRuns(ctx, a.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("RoleRuns = %+v, %v; want one row for the one role", got, err)
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("role run\n got %+v\nwant %+v", got[0], want)
	}

	// Zero values never clear a set field, and a later value replaces an earlier one.
	if err := r.RecordRoleRun(ctx, registry.RoleRun{AttemptID: a.ID, Role: "claude-reproducer", Nudges: 3, Model: "opus-4"}); err != nil {
		t.Fatal(err)
	}
	want.Nudges, want.Model = 3, "opus-4"
	if got, _ := r.RoleRuns(ctx, a.ID); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("role run after a sparse update\n got %+v\nwant %+v", got, want)
	}

	// Outcomes: failed:<cause> and the five words are taken, anything else is refused.
	for _, outcome := range []string{"failed:no_end_fact", "voided", "stopped", "lost", "paused", "verified"} {
		rr := registry.RoleRun{AttemptID: a.ID, Role: "codex-finder", Outcome: outcome}
		if err := r.RecordRoleRun(ctx, rr); err != nil {
			t.Errorf("outcome %q refused: %v", outcome, err)
		}
	}
	for _, outcome := range []string{"exploded", "failed", "failed:", "Verified", "failed no_end_fact"} {
		err := r.RecordRoleRun(ctx, registry.RoleRun{AttemptID: a.ID, Role: "codex-finder", Outcome: outcome})
		if err == nil || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("outcome %q: RecordRoleRun = %v, want a refusal", outcome, err)
		}
	}
	runs, _ := r.RoleRuns(ctx, a.ID)
	if len(runs) != 2 || runs[1].Role != "codex-finder" || runs[1].Outcome != "verified" {
		t.Errorf("RoleRuns = %+v, want the second role last, with the last outcome that was taken", runs)
	}

	if err := r.RecordRoleRun(ctx, registry.RoleRun{AttemptID: "t9-nope-1", Role: "r"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordRoleRun for a missing attempt = %v, want ErrNotFound", err)
	}
	if err := r.RecordRoleRun(ctx, registry.RoleRun{AttemptID: a.ID}); err == nil {
		t.Error("RecordRoleRun without a role succeeded")
	}
	if none, err := r.RoleRuns(ctx, "t9-nope-1"); err != nil || len(none) != 0 {
		t.Errorf("RoleRuns of a missing attempt = %+v, %v; want none", none, err)
	}
}

func TestRecordInputTakesTextOrADropReasonNeverBoth(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")

	typed := registry.Input{TaskID: task.ID, Kind: "guidance", Surface: "tui", Stage: "investigate", Role: "claude-reproducer",
		Text: "only the refund path", Origin: "say_one_line", Before: "Which path should I read?", AgentState: "idle"}
	id, err := r.RecordInput(ctx, typed)
	if err != nil || id == 0 {
		t.Fatalf("RecordInput = %d, %v", id, err)
	}
	clk.Add(time.Minute)
	dropped := registry.Input{TaskID: task.ID, Kind: "pane_typed", Surface: "pane", Stage: "coding",
		DroppedReason: "within minutes of an access card", Origin: "herdr showed the operator in the pane"}
	if _, err := r.RecordInput(ctx, dropped); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	stop := registry.Input{TaskID: task.ID, Kind: "stop", Surface: "tui"}
	if _, err := r.RecordInput(ctx, stop); err != nil {
		t.Fatalf("an input with neither text nor reason (a stop): %v", err)
	}

	both := registry.Input{TaskID: task.ID, Kind: "guidance", Text: "x", DroppedReason: "y"}
	if _, err := r.RecordInput(ctx, both); err == nil || errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordInput with text and a drop reason = %v, want a refusal", err)
	}
	for name, in := range map[string]registry.Input{
		"no kind":      {TaskID: task.ID, Text: "x"},
		"unknown kind": {TaskID: task.ID, Kind: "yell", Text: "x"},
	} {
		if _, err := r.RecordInput(ctx, in); err == nil {
			t.Errorf("%s: RecordInput succeeded", name)
		}
	}
	if _, err := r.RecordInput(ctx, registry.Input{TaskID: 999, Kind: "guidance", Text: "x"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordInput for a missing task = %v, want ErrNotFound", err)
	}

	got, err := r.Inputs(ctx, task.ID)
	if err != nil || len(got) != 3 {
		t.Fatalf("Inputs = %+v, %v; want the three that were accepted", got, err)
	}
	typed.ID, typed.At = id, t0
	dropped.ID, dropped.At = got[1].ID, t0.Add(time.Minute)
	stop.ID, stop.At = got[2].ID, t0.Add(2*time.Minute)
	for i, want := range []registry.Input{typed, dropped, stop} {
		if got[i] != want {
			t.Errorf("input %d\n got %+v\nwant %+v", i, got[i], want)
		}
	}

	// A class is empty until a classifier sets it, and only the five are taken.
	if got[0].Class != "" {
		t.Errorf("a new input has class %q, want none", got[0].Class)
	}
	for _, class := range []string{"step_start", "approval", "correction", "access", "info"} {
		if err := r.ClassifyInput(ctx, id, class); err != nil {
			t.Errorf("ClassifyInput(%q): %v", class, err)
		}
	}
	if after, _ := r.Inputs(ctx, task.ID); after[0].Class != "info" {
		t.Errorf("class after the last classification = %q, want info", after[0].Class)
	}
	for _, class := range []string{"", "chat", "Correction"} {
		if err := r.ClassifyInput(ctx, id, class); err == nil {
			t.Errorf("ClassifyInput(%q) succeeded", class)
		}
	}
	if err := r.ClassifyInput(ctx, id+100, "info"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ClassifyInput of a missing input = %v, want ErrNotFound", err)
	}
}

func TestRecordInputCutsTheTurnsBeforeItToOneKiB(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	ascii := strings.Repeat("a", 1000) + strings.Repeat("b", 25) // 1025 bytes
	cases := map[string]struct{ before, want string }{
		"short":         {"Which path?", "Which path?"},
		"exactly 1 KiB": {strings.Repeat("a", 1024), strings.Repeat("a", 1024)},
		"one over":      {ascii, ascii[1:]},
		// 2000 bytes of two-byte runes: the cut must not split one.
		"multibyte": {strings.Repeat("é", 1000), strings.Repeat("é", 512)},
	}
	for name, c := range cases {
		if _, err := r.RecordInput(ctx, registry.Input{TaskID: task.ID, Kind: "answer", Text: name, Before: c.before}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, err := r.Inputs(ctx, task.ID)
	if err != nil || len(got) != len(cases) {
		t.Fatalf("Inputs = %d rows, %v", len(got), err)
	}
	for _, in := range got {
		c := cases[in.Text]
		if len(in.Before) > 1024 || !utf8.ValidString(in.Before) {
			t.Errorf("%s: Before is %d bytes, valid UTF-8 %v; want at most 1024 and valid", in.Text, len(in.Before), utf8.ValidString(in.Before))
		}
		if in.Before != c.want {
			t.Errorf("%s: Before has %d bytes, want the last turns kept (%d bytes)", in.Text, len(in.Before), len(c.want))
		}
	}
}

func TestFlagQuestionTakesOnlyItsThreeFlags(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	q := mustQuestions(t, r, task.ID)[0]
	if q.Flag != "" || q.FlagWhy != "" {
		t.Fatalf("a new question is flagged: %+v", q)
	}
	for _, flag := range []string{"needless", "needed", "unclear"} {
		if err := r.FlagQuestion(ctx, q.ID, flag, "because "+flag); err != nil {
			t.Fatalf("FlagQuestion(%q): %v", flag, err)
		}
		if got := mustQuestions(t, r, task.ID)[0]; got.Flag != flag || got.FlagWhy != "because "+flag {
			t.Errorf("flag after %q = %q (%q)", flag, got.Flag, got.FlagWhy)
		}
	}
	for _, flag := range []string{"", "useless", "Needless", "needless "} {
		if err := r.FlagQuestion(ctx, q.ID, flag, "x"); err == nil || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("FlagQuestion(%q) = %v, want a refusal", flag, err)
		}
	}
	if got := mustQuestions(t, r, task.ID)[0]; got.Flag != "unclear" || got.FlagWhy != "because unclear" {
		t.Errorf("a refused flag changed the question: %+v", got)
	}
	if err := r.FlagQuestion(ctx, q.ID+100, "needed", ""); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("FlagQuestion of a missing question = %v, want ErrNotFound", err)
	}
}

func TestAnswerHistoryKeepsEveryEdit(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	qs := mustQuestions(t, r, task.ID)

	clk.Add(time.Minute)
	if err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "One day before it expires", 1: "No"}); err != nil {
		t.Fatal(err)
	}
	if got := mustQuestions(t, r, task.ID); got[0].TookRecommendation || got[1].TookRecommendation {
		t.Errorf("TookRecommendation = %v, %v after answers that are not the recommendation (and none for the second)", got[0].TookRecommendation, got[1].TookRecommendation)
	}
	clk.Add(time.Minute)
	if err := r.ReviseAnswer(ctx, task.ID, 1, 0, "Seven days before it expires"); err != nil {
		t.Fatal(err)
	}
	if got := mustQuestions(t, r, task.ID)[0]; !got.TookRecommendation || got.Answer != "Seven days before it expires" || !got.Answered.Equal(t0.Add(time.Minute)) {
		t.Errorf("question after taking the recommendation = %+v; want the answer, TookRecommendation and the first answered time", got)
	}
	// The same words again are not an edit.
	clk.Add(time.Minute)
	if err := r.ReviseAnswer(ctx, task.ID, 1, 0, "Seven days before it expires"); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	if err := r.ReviseAnswer(ctx, task.ID, 1, 0, "Never"); err != nil {
		t.Fatal(err)
	}
	if got := mustQuestions(t, r, task.ID)[0]; got.TookRecommendation {
		t.Error("TookRecommendation stayed true after the answer moved away from the recommendation")
	}

	hist, err := r.AnswerHistory(ctx, qs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []registry.AnswerEdit{
		{Answer: "One day before it expires", At: t0.Add(time.Minute)},
		{Answer: "Seven days before it expires", At: t0.Add(2 * time.Minute)},
		{Answer: "Never", At: t0.Add(4 * time.Minute)},
	}
	if !reflect.DeepEqual(hist, want) {
		t.Errorf("AnswerHistory\n got %+v\nwant %+v", hist, want)
	}
	if one, _ := r.AnswerHistory(ctx, qs[1].ID); len(one) != 1 || one[0].Answer != "No" {
		t.Errorf("history of an answer that was never edited = %+v", one)
	}
	if none, err := r.AnswerHistory(ctx, qs[0].ID+100); err != nil || len(none) != 0 {
		t.Errorf("AnswerHistory of a missing question = %+v, %v; want none", none, err)
	}

	// An edit follows the same rules as a batch: the newest revision, an answered question, real words.
	if err := r.ReplaceQuestions(ctx, task.ID, 2, askTwo()); err != nil {
		t.Fatal(err)
	}
	if err := r.ReviseAnswer(ctx, task.ID, 1, 0, "Late"); !errors.Is(err, registry.ErrStaleRevision) {
		t.Errorf("ReviseAnswer for an older revision = %v, want ErrStaleRevision", err)
	}
	if err := r.ReviseAnswer(ctx, task.ID, 2, 0, "Early"); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("ReviseAnswer of a question that is still open = %v, want ErrConflict", err)
	}
	if err := r.AnswerBatch(ctx, task.ID, 2, map[int]string{0: "Seven days before it expires"}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReviseAnswer(ctx, task.ID, 2, 0, "  "); err == nil {
		t.Error("ReviseAnswer with a blank answer succeeded")
	}
	if err := r.ReviseAnswer(ctx, 999, 1, 0, "x"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ReviseAnswer for a missing task = %v, want ErrNotFound", err)
	}
}

func TestShowQuestionsKeepsTheFirstShownTime(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	if got := mustQuestions(t, r, task.ID); !got[0].Shown.IsZero() {
		t.Fatalf("a question nobody has seen has Shown %v", got[0].Shown)
	}
	clk.Add(time.Minute)
	if err := r.ShowQuestions(ctx, task.ID, 1); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	if err := r.ShowQuestions(ctx, task.ID, 1); err != nil {
		t.Fatal(err)
	}
	for _, q := range mustQuestions(t, r, task.ID) {
		if !q.Shown.Equal(t0.Add(time.Minute)) {
			t.Errorf("question %d Shown = %v, want the first time %v", q.Index, q.Shown, t0.Add(time.Minute))
		}
	}

	clk.Add(time.Minute)
	if err := r.ReplaceQuestions(ctx, task.ID, 2, askTwo()[:1]); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	if err := r.ShowQuestions(ctx, task.ID, 2); err != nil {
		t.Fatal(err)
	}
	got := mustQuestions(t, r, task.ID)
	if !got[0].Shown.Equal(t0.Add(time.Minute)) || !got[2].Shown.Equal(t0.Add(4*time.Minute)) {
		t.Errorf("Shown after revision 2 = %v (revision 1) and %v (revision 2); want %v and %v", got[0].Shown, got[2].Shown, t0.Add(time.Minute), t0.Add(4*time.Minute))
	}
	if err := r.ShowQuestions(ctx, task.ID, 9); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ShowQuestions of a revision that does not exist = %v, want ErrNotFound", err)
	}
	if err := r.ShowQuestions(ctx, 999, 1); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ShowQuestions of a missing task = %v, want ErrNotFound", err)
	}
}

func TestReplaceQuestionsKeepsWhoRaisedAQuestionAndWhatItCited(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	qs := askTwo()
	qs[0].RaisedBy, qs[0].Evidence = "codex-finder", "app/models/credit.rb:41 sends no mail"
	if err := r.ReplaceQuestions(ctx, task.ID, 1, qs); err != nil {
		t.Fatal(err)
	}
	got := mustQuestions(t, r, task.ID)
	if got[0].RaisedBy != "codex-finder" || got[0].Evidence != "app/models/credit.rb:41 sends no mail" || got[1].RaisedBy != "" {
		t.Errorf("questions = %+v, want who raised the first and what it cited", got)
	}
	if got[0].VoidedBy != 0 {
		t.Errorf("an open question has VoidedBy %d", got[0].VoidedBy)
	}
	if err := r.ReplaceQuestions(ctx, task.ID, 2, askTwo()[:1]); err != nil {
		t.Fatal(err)
	}
	got = mustQuestions(t, r, task.ID)
	if got[0].VoidedBy != 2 || got[1].VoidedBy != 2 || got[2].VoidedBy != 0 {
		t.Errorf("VoidedBy = %d, %d, %d; want 2, 2, 0", got[0].VoidedBy, got[1].VoidedBy, got[2].VoidedBy)
	}
}

func TestRecordDecisionKeepsWhatTheCardShowedAndWhatFollowed(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	d := registry.Decision{TaskID: task.ID, Kind: "ready", Choice: "push", HeadSHA: "abc123", ReportHash: "r1", DiffHash: "d1",
		DiffOpened: t0.Add(2 * time.Second), Shown: t0, DiffDwell: 95 * time.Second,
		RecommendationShown: true, RecommendationFollowed: true, Patch: sha("diff --git"), Proof: sha("{}")}
	clk.Add(3 * time.Minute)
	if err := r.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := r.Decisions(ctx, task.ID)
	d.At = t0.Add(3 * time.Minute)
	if err != nil || len(got) != 1 || got[0] != d {
		t.Errorf("Decisions = %+v, %v\nwant %+v", got, err, d)
	}
	// A card that showed no recommendation and was never opened keeps zero values.
	if err := r.RecordDecision(ctx, registry.Decision{TaskID: task.ID, Kind: "ready", Choice: "send back"}); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Decisions(ctx, task.ID)
	if !got[1].Shown.IsZero() || got[1].DiffDwell != 0 || got[1].RecommendationShown || got[1].RecommendationFollowed || got[1].Patch != "" {
		t.Errorf("a bare decision = %+v", got[1])
	}
}

func TestRecordRatingKeepsTheReasonAndWhetherTheDiffCameFirst(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	if err := r.RecordRating(ctx, registry.Rating{TaskID: task.ID, Value: 1, ReasonKey: "small_diff", DiffOpenedFirst: true}); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	if err := r.RecordRating(ctx, registry.Rating{TaskID: task.ID, Value: -1, ReasonKey: "wrong_place"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Ratings(ctx, task.ID)
	want := []registry.Rating{
		{TaskID: task.ID, Value: 1, ReasonKey: "small_diff", DiffOpenedFirst: true, At: t0},
		{TaskID: task.ID, Value: -1, ReasonKey: "wrong_place", At: t0.Add(time.Minute)},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Ratings = %+v, %v\nwant %+v", got, err, want)
	}
	for _, v := range []int{0, 2, -2} {
		if err := r.RecordRating(ctx, registry.Rating{TaskID: task.ID, Value: v}); err == nil {
			t.Errorf("a rating of %d was taken", v)
		}
	}
	if err := r.RecordRating(ctx, registry.Rating{TaskID: 999, Value: 1}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordRating for a missing task = %v, want ErrNotFound", err)
	}
}

func TestBeginCycleNumbersPerTaskAndEndCycleOnce(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-12301")
	two := createTask(t, r, "PR-12302")
	c1, err := r.BeginCycle(ctx, one.ID, "head0", "skill-1")
	if err != nil {
		t.Fatal(err)
	}
	if c1.N != 1 || c1.TaskID != one.ID || c1.HeadBefore != "head0" || c1.SkillHash != "skill-1" || !c1.Started.Equal(t0) || !c1.Ended.IsZero() || c1.Exit != "" || c1.ID == 0 {
		t.Errorf("first cycle = %+v", c1)
	}
	clk.Add(39 * time.Minute)
	if err := r.EndCycle(ctx, c1.ID, "head1", "sha256-of-the-patch", "", 52000); err != nil {
		t.Fatal(err)
	}
	if err := r.EndCycle(ctx, c1.ID, "head2", "d", "clean", 1); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("EndCycle twice = %v, want ErrConflict", err)
	}
	c2, _ := r.BeginCycle(ctx, one.ID, "head1", "skill-1")
	other, _ := r.BeginCycle(ctx, two.ID, "headX", "skill-1")
	if c2.N != 2 || other.N != 1 {
		t.Errorf("cycle numbers = %d and %d, want 2 and 1 (per task)", c2.N, other.N)
	}
	if err := r.EndCycle(ctx, c2.ID, "head1", "", "wedged", 0); err == nil {
		t.Error("EndCycle with an unknown exit succeeded")
	}
	for _, exit := range []string{"clean", "cap", "operator", "oscillation", "unavailable"} {
		c, _ := r.BeginCycle(ctx, two.ID, "h", "s")
		if err := r.EndCycle(ctx, c.ID, "h", "", exit, 0); err != nil {
			t.Errorf("exit %q refused: %v", exit, err)
		}
	}
	if err := r.EndCycle(ctx, 9999, "h", "", "clean", 0); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("EndCycle of a missing cycle = %v, want ErrNotFound", err)
	}
	if _, err := r.BeginCycle(ctx, 999, "h", "s"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("BeginCycle for a missing task = %v, want ErrNotFound", err)
	}

	got, err := r.Cycles(ctx, one.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("Cycles = %+v, %v", got, err)
	}
	wantFirst := registry.Cycle{ID: c1.ID, TaskID: one.ID, N: 1, HeadBefore: "head0", HeadAfter: "head1", Delta: "sha256-of-the-patch",
		SkillHash: "skill-1", Started: t0, Ended: t0.Add(39 * time.Minute), Tokens: 52000}
	if got[0] != wantFirst {
		t.Errorf("first cycle after it ended\n got %+v\nwant %+v", got[0], wantFirst)
	}
	if !got[1].Ended.IsZero() || got[1].N != 2 {
		t.Errorf("second cycle = %+v, want it still running", got[1])
	}
}

func TestSeeFindingReopensAFixedFinding(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	c1, _ := r.BeginCycle(ctx, task.ID, "h0", "s")
	c2, _ := r.BeginCycle(ctx, task.ID, "h1", "s")
	c3, _ := r.BeginCycle(ctx, task.ID, "h2", "s")

	f := registry.Finding{Fingerprint: "fp-1", Path: "app/models/credit.rb", StartLine: 40, EndLine: 44, TitleHash: "t1", Priority: "P1",
		Class: "bug", Sources: []string{"claude-review", "codex-review"}, Verdict: "confirmed", VerdictWhy: "the guard is gone", Blocking: true}
	seen, err := r.SeeFinding(ctx, c1.ID, f)
	if err != nil {
		t.Fatal(err)
	}
	want := f
	want.ID, want.TaskID, want.FirstCycle, want.LastCycle, want.Outcome = seen.ID, task.ID, 1, 1, "open"
	if seen.ID == 0 || !reflect.DeepEqual(seen, want) {
		t.Errorf("finding at first sight\n got %+v\nwant %+v", seen, want)
	}

	if err := r.SettleFinding(ctx, seen.ID, "fixed", "abc1234"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.SeeFinding(ctx, c2.ID, registry.Finding{Fingerprint: "other-finding", Path: "x.rb", Priority: "P3"}); got.ID == seen.ID {
		t.Error("two fingerprints share one finding")
	}
	back, err := r.SeeFinding(ctx, c3.ID, f)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != seen.ID || back.Outcome != "open" || back.Reopened != 1 || back.FirstCycle != 1 || back.LastCycle != 3 {
		t.Errorf("a fixed finding that came back = %+v, want open, Reopened 1, cycles 1 to 3", back)
	}
	if back.FixCommit != "abc1234" {
		t.Errorf("FixCommit = %q, want the commit that failed to fix it kept", back.FixCommit)
	}

	// Seen again while open: only LastCycle moves, and a second return counts again.
	if again, _ := r.SeeFinding(ctx, c3.ID, f); again.Reopened != 1 || again.LastCycle != 3 {
		t.Errorf("a finding seen twice in a cycle = %+v", again)
	}
	if err := r.SettleFinding(ctx, seen.ID, "fixed", "def5678"); err != nil {
		t.Fatal(err)
	}
	c4, _ := r.BeginCycle(ctx, task.ID, "h3", "s")
	if again, _ := r.SeeFinding(ctx, c4.ID, f); again.Reopened != 2 || again.LastCycle != 4 || again.FixCommit != "def5678" {
		t.Errorf("a finding that came back twice = %+v, want Reopened 2", again)
	}

	all, err := r.Findings(ctx, task.ID)
	if err != nil || len(all) != 2 {
		t.Errorf("Findings = %+v, %v; want one row per fingerprint", all, err)
	}
	if _, err := r.SeeFinding(ctx, 9999, f); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("SeeFinding in a missing cycle = %v, want ErrNotFound", err)
	}
	if _, err := r.SeeFinding(ctx, c1.ID, registry.Finding{Path: "x.rb"}); err == nil {
		t.Error("SeeFinding without a fingerprint succeeded")
	}
	foreign := f
	foreign.TaskID = task.ID + 1
	if _, err := r.SeeFinding(ctx, c1.ID, foreign); err == nil {
		t.Error("SeeFinding took a finding of another task into this cycle")
	}
}

func TestSeeFindingFollowsAFindingThroughTheRounds(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	c1, _ := r.BeginCycle(ctx, task.ID, "h0", "s")
	c2, _ := r.BeginCycle(ctx, task.ID, "h1", "s")
	f := registry.Finding{Fingerprint: "fp-1", Path: "a.rb", StartLine: 1, EndLine: 2, Priority: "P2", Sources: []string{"claude-review"}}
	seen, _ := r.SeeFinding(ctx, c1.ID, f)

	// The next round sees it a few lines lower, from a second reviewer, at a higher priority.
	moved := registry.Finding{Fingerprint: "fp-1", Path: "a.rb", StartLine: 5, EndLine: 6, Priority: "P1", Blocking: true,
		Sources: []string{"codex-review", "claude-review"}, Verdict: "confirmed", VerdictWhy: "reproduced"}
	got, err := r.SeeFinding(ctx, c2.ID, moved)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != seen.ID || got.StartLine != 5 || got.Priority != "P1" || !got.Blocking || got.Verdict != "confirmed" || got.Reopened != 0 ||
		!reflect.DeepEqual(got.Sources, []string{"claude-review", "codex-review"}) {
		t.Errorf("finding after a second sighting = %+v", got)
	}

	for outcome, commit := range map[string]string{"waived": "", "rebutted": "", "open": "", "fixed": "abc1234"} {
		if err := r.SettleFinding(ctx, seen.ID, outcome, commit); err != nil {
			t.Errorf("SettleFinding(%q): %v", outcome, err)
		}
		if after, _ := r.Findings(ctx, task.ID); after[0].Outcome != outcome {
			t.Errorf("outcome = %q, want %q", after[0].Outcome, outcome)
		}
	}
	if err := r.SettleFinding(ctx, seen.ID, "exploded", ""); err == nil {
		t.Error("SettleFinding with an unknown outcome succeeded")
	}
	if err := r.SettleFinding(ctx, seen.ID+100, "fixed", "x"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("SettleFinding of a missing finding = %v, want ErrNotFound", err)
	}

	// A finding the operator waived stays waived when it is seen again.
	if err := r.SettleFinding(ctx, seen.ID, "waived", ""); err != nil {
		t.Fatal(err)
	}
	c3, _ := r.BeginCycle(ctx, task.ID, "h2", "s")
	if again, _ := r.SeeFinding(ctx, c3.ID, f); again.Outcome != "waived" || again.Reopened != 0 || again.LastCycle != 3 {
		t.Errorf("a waived finding seen again = %+v, want it waived and not reopened", again)
	}
}

func TestRecordArtifactAddsARevisionOnlyForNewContent(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	report := registry.Artifact{TaskID: task.ID, Kind: "report", Path: "report.md", SHA256: sha("one")}
	if err := r.RecordArtifact(ctx, report); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	if err := r.RecordArtifact(ctx, report); err != nil {
		t.Fatalf("the same content again: %v", err)
	}
	if got, _ := r.Artifacts(ctx, task.ID); len(got) != 1 || got[0].Rev != 1 || !got[0].At.Equal(t0) {
		t.Fatalf("Artifacts after the same content twice = %+v, want one revision at the first time", got)
	}

	report.SHA256 = sha("two")
	report.Rev = 7 // the caller's number does not count
	if err := r.RecordArtifact(ctx, report); err != nil {
		t.Fatal(err)
	}
	// Other kinds and other paths count their own revisions.
	if err := r.RecordArtifact(ctx, registry.Artifact{TaskID: task.ID, Kind: "report", Path: "round2/report.md", SHA256: sha("one")}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordArtifact(ctx, registry.Artifact{TaskID: task.ID, Kind: "proof", Path: "report.md", SHA256: sha("one")}); err != nil {
		t.Fatal(err)
	}
	// Going back to earlier content is new content: the latest one is what the file holds.
	report.SHA256 = sha("one")
	if err := r.RecordArtifact(ctx, report); err != nil {
		t.Fatal(err)
	}

	got, err := r.Artifacts(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		kind, path string
		rev        int
	}
	var rows []row
	for _, a := range got {
		rows = append(rows, row{a.Kind, a.Path, a.Rev})
	}
	want := []row{{"report", "report.md", 1}, {"report", "report.md", 2}, {"report", "round2/report.md", 1}, {"proof", "report.md", 1}, {"report", "report.md", 3}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("artifact revisions = %v, want %v", rows, want)
	}
	if got[1].TaskID != task.ID || got[1].SHA256 != sha("two") || !got[1].At.Equal(t0.Add(time.Minute)) {
		t.Errorf("second revision = %+v", got[1])
	}

	for name, a := range map[string]registry.Artifact{
		"unknown kind":  {TaskID: task.ID, Kind: "notes", Path: "n.md", SHA256: sha("x")},
		"absolute path": {TaskID: task.ID, Kind: "report", Path: "/etc/passwd", SHA256: sha("x")},
		"parent path":   {TaskID: task.ID, Kind: "report", Path: "../report.md", SHA256: sha("x")},
		"unclean path":  {TaskID: task.ID, Kind: "report", Path: "a/../report.md", SHA256: sha("x")},
		"no path":       {TaskID: task.ID, Kind: "report", SHA256: sha("x")},
		"short hash":    {TaskID: task.ID, Kind: "report", Path: "r.md", SHA256: "abc"},
		"upper hash":    {TaskID: task.ID, Kind: "report", Path: "r.md", SHA256: strings.ToUpper(sha("x"))},
	} {
		if err := r.RecordArtifact(ctx, a); err == nil || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("%s: RecordArtifact = %v, want a refusal", name, err)
		}
	}
	if err := r.RecordArtifact(ctx, registry.Artifact{TaskID: 999, Kind: "report", Path: "r.md", SHA256: sha("x")}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordArtifact for a missing task = %v, want ErrNotFound", err)
	}
}

// The judge's acceptance checks (acceptance.json) are an artifact of their
// own, beside its report and the answers it read.
func TestEveryFileTheJudgeWritesIsAnArtifactKind(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	task := createTask(t, r, "PR-12303")
	for kind, path := range map[string]string{"report": "report.md", "acceptance": "acceptance.json", "answers": "answers.md"} {
		if err := r.RecordArtifact(t.Context(), registry.Artifact{TaskID: task.ID, Kind: kind, Path: path, SHA256: sha(path)}); err != nil {
			t.Errorf("RecordArtifact(%s) = %v", kind, err)
		}
	}
}

func TestLinkPRAllowsOnePRPerTask(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	one := createTask(t, r, "PR-12301")
	two := createTask(t, r, "PR-12302")
	if _, err := r.LinkedPR(ctx, one.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("LinkedPR before a link = %v, want ErrNotFound", err)
	}
	link := registry.PRLink{TaskID: one.ID, Repo: "example/app", Number: 77, Branch: "zhuravel-PR-12301-fix", OpenedHead: "abc123", ApprovedHead: "abc123"}
	clk.Add(time.Minute)
	if err := r.LinkPR(ctx, link); err != nil {
		t.Fatal(err)
	}
	link.Opened = t0.Add(time.Minute)
	if got, err := r.LinkedPR(ctx, one.ID); err != nil || got != link {
		t.Errorf("LinkedPR = %+v, %v; want %+v", got, err, link)
	}

	// The same PR again is a no-op, whatever else the caller knows now.
	clk.Add(time.Minute)
	again := link
	again.OpenedHead, again.Opened = "def456", t0.Add(2*time.Minute)
	if err := r.LinkPR(ctx, again); err != nil {
		t.Errorf("linking the same PR again: %v", err)
	}
	if got, _ := r.LinkedPR(ctx, one.ID); got != link {
		t.Errorf("a repeated link changed the row: %+v", got)
	}
	other := link
	other.Number = 78
	if err := r.LinkPR(ctx, other); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("a second PR for the task = %v, want ErrConflict", err)
	}
	otherRepo := link
	otherRepo.Repo = "example/other"
	if err := r.LinkPR(ctx, otherRepo); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("the same number in another repository = %v, want ErrConflict", err)
	}
	if got, _ := r.LinkedPR(ctx, one.ID); got != link {
		t.Errorf("a refused link changed the row: %+v", got)
	}
	theirs := link
	theirs.TaskID = two.ID
	if err := r.LinkPR(ctx, theirs); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("one PR for two tasks = %v, want ErrConflict", err)
	}

	for name, l := range map[string]registry.PRLink{
		"no repository": {TaskID: two.ID, Number: 5},
		"no number":     {TaskID: two.ID, Repo: "example/app"},
	} {
		if err := r.LinkPR(ctx, l); err == nil || errors.Is(err, registry.ErrNotFound) {
			t.Errorf("%s: LinkPR = %v, want a refusal", name, err)
		}
	}
	if err := r.LinkPR(ctx, registry.PRLink{TaskID: 999, Repo: "example/app", Number: 5}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("LinkPR for a missing task = %v, want ErrNotFound", err)
	}
}

// A PR stays one task's while that task holds its work (it is not done or
// stopped), with a Jira key or from a prompt; once that task stopped, a new
// task may take the PR over (adoption, milestone 2) and keeps it, and the
// stopped task's link stays as it was.
func TestLinkPRAdoptsThePROfATaskThatEnded(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	first := createTask(t, r, "PR-12303")
	link := registry.PRLink{TaskID: first.ID, Repo: "talkable/talkable", Number: 4527, Branch: "zhuravel-PR-12303-fix"}
	if err := r.LinkPR(ctx, link); err != nil {
		t.Fatal(err)
	}
	prompt, err := r.CreateTask(ctx, registry.Task{Prompt: "take over the fix", Kind: "bug", Repo: "talkable/talkable"})
	if err != nil {
		t.Fatal(err)
	}
	theirs := link
	theirs.TaskID = prompt.ID
	if err := r.LinkPR(ctx, theirs); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("the PR of a task at work for another task = %v, want ErrConflict", err)
	}

	if err := r.TransitionTask(ctx, first.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.LinkPR(ctx, theirs); err != nil {
		t.Fatalf("adopting the PR of a stopped task: %v", err)
	}
	if got, err := r.LinkedPR(ctx, prompt.ID); err != nil || got.Number != 4527 || got.Repo != "talkable/talkable" {
		t.Fatalf("LinkedPR of the adopter = %+v, %v", got, err)
	}
	if got, err := r.LinkedPR(ctx, first.ID); err != nil || got.Number != 4527 {
		t.Fatalf("the stopped task's link = %+v, %v; want it kept", got, err)
	}
	// The adopter holds the PR now: a third task cannot take it.
	third := createTask(t, r, "PR-12304")
	again := link
	again.TaskID = third.ID
	if err := r.LinkPR(ctx, again); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("the PR of the adopter for a third task = %v, want ErrConflict", err)
	}
}

func TestRecordCheckRefusesACauseOnAPass(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a := attemptFor(t, r, task.ID, "coding")
	pass := registry.Check{TaskID: task.ID, AttemptID: a.ID, HeadSHA: "abc123", Kind: "spec", Result: "pass", Source: "factory",
		Command: "bin/rspec spec/models/credit_spec.rb", Exit: 0, Took: 94 * time.Second}
	clk.Add(time.Minute)
	id, err := r.RecordCheck(ctx, pass)
	if err != nil || id == 0 {
		t.Fatalf("RecordCheck = %d, %v", id, err)
	}
	fail := registry.Check{TaskID: task.ID, HeadSHA: "abc123", Kind: "ci", Result: "fail", Cause: "flaky", Source: "github", Exit: 1}
	if _, err := r.RecordCheck(ctx, fail); err != nil {
		t.Fatal(err)
	}
	unclassified := registry.Check{TaskID: task.ID, Kind: "lint", Result: "fail", Source: "writer"}
	if _, err := r.RecordCheck(ctx, unclassified); err != nil {
		t.Fatalf("a failure whose cause is not yet known: %v", err)
	}
	for _, cause := range []string{"code", "infra", "flaky", "env", "access", "not_ours", "unknown"} {
		c := fail
		c.Cause = cause
		if _, err := r.RecordCheck(ctx, c); err != nil {
			t.Errorf("cause %q refused: %v", cause, err)
		}
	}

	for name, c := range map[string]registry.Check{
		"a cause on a pass":   {TaskID: task.ID, Kind: "spec", Result: "pass", Cause: "code", Source: "factory"},
		"a cause on a skip":   {TaskID: task.ID, Kind: "qa", Result: "skipped", Cause: "env", Source: "factory"},
		"an unknown cause":    {TaskID: task.ID, Kind: "ci", Result: "fail", Cause: "bad luck", Source: "github"},
		"an unknown kind":     {TaskID: task.ID, Kind: "vibes", Result: "pass", Source: "factory"},
		"an unknown result":   {TaskID: task.ID, Kind: "ci", Result: "green", Source: "github"},
		"no source":           {TaskID: task.ID, Kind: "ci", Result: "pass"},
		"an unknown source":   {TaskID: task.ID, Kind: "ci", Result: "pass", Source: "someone"},
		"an unknown attempt":  {TaskID: task.ID, AttemptID: "t1-nope-1", Kind: "ci", Result: "pass", Source: "github"},
		"a task that is gone": {TaskID: 999, Kind: "ci", Result: "pass", Source: "github"},
	} {
		if _, err := r.RecordCheck(ctx, c); err == nil {
			t.Errorf("%s: RecordCheck succeeded", name)
		}
	}
	if _, err := r.RecordCheck(ctx, registry.Check{TaskID: 999, Kind: "ci", Result: "pass", Source: "github"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordCheck for a missing task = %v, want ErrNotFound", err)
	}

	got, err := r.Checks(ctx, task.ID)
	if err != nil || len(got) != 3+7 {
		t.Fatalf("Checks = %d rows, %v; want only the accepted ones", len(got), err)
	}
	pass.ID, pass.At = id, t0.Add(time.Minute)
	if got[0] != pass {
		t.Errorf("first check\n got %+v\nwant %+v", got[0], pass)
	}
	if got[1].AttemptID != "" || got[1].Cause != "flaky" || got[2].Cause != "" {
		t.Errorf("checks 2 and 3 = %+v and %+v", got[1], got[2])
	}
}

func TestASuspensionResumesOnce(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a := attemptFor(t, r, task.ID, "investigate")
	if _, err := r.OpenSuspension(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("OpenSuspension of a task that never suspended = %v, want ErrNotFound", err)
	}
	s := registry.Suspension{TaskID: task.ID, Stage: "investigate", AttemptID: a.ID, Role: "claude-reproducer", Open: []string{"q:1", "q:2"}}
	clk.Add(time.Minute)
	if err := r.Suspend(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.Suspended = t0.Add(time.Minute)
	got, err := r.OpenSuspension(ctx, task.ID)
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Errorf("OpenSuspension = %+v, %v\nwant %+v", got, err, s)
	}
	if err := r.Suspend(ctx, s); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("suspending a task that is suspended = %v, want ErrConflict", err)
	}

	clk.Add(time.Minute)
	if err := r.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenSuspension(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("OpenSuspension after Resume = %v, want ErrNotFound", err)
	}
	if err := r.Resume(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a second Resume = %v, want ErrNotFound", err)
	}

	// A resumed task can be suspended again; the first suspension stays in the registry.
	clk.Add(time.Minute)
	if err := r.Suspend(ctx, registry.Suspension{TaskID: task.ID, Stage: "coding", Role: "codex-writer"}); err != nil {
		t.Fatalf("a second suspension after the first resumed: %v", err)
	}
	if again, err := r.OpenSuspension(ctx, task.ID); err != nil || again.Stage != "coding" || again.AttemptID != "" || len(again.Open) != 0 {
		t.Errorf("second suspension = %+v, %v", again, err)
	}
	var n int
	if err := r.DBForTest().QueryRowContext(ctx, "SELECT count(*) FROM suspensions WHERE task_id = ?", task.ID).Scan(&n); err != nil || n != 2 {
		t.Errorf("suspensions kept = %d, %v; want both", n, err)
	}
	if err := r.Resume(ctx, 999); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Resume of a missing task = %v, want ErrNotFound", err)
	}
	if err := r.Suspend(ctx, registry.Suspension{TaskID: 999, Stage: "coding"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Suspend of a missing task = %v, want ErrNotFound", err)
	}
	if err := r.Suspend(ctx, registry.Suspension{TaskID: task.ID}); err == nil {
		t.Error("Suspend without a stage succeeded")
	}
}

func TestSchemaRefusesWhatTheEvidenceMethodsNeverWrite(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12301")
	a := attemptFor(t, r, task.ID, "coding")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BeginCycle(ctx, task.ID, "h", "s"); err != nil {
		t.Fatal(err)
	}
	const at = "'2026-11-03T12:00:00.000000000Z'"
	bad := []string{
		"INSERT INTO task_spans (task_id, to_state, at) VALUES (1, 'paused', " + at + ")",
		"INSERT INTO task_spans (task_id, from_state, to_state, at) VALUES (1, 'paused', 'active', " + at + ")",
		"INSERT INTO attempt_roles (attempt_id, role, outcome) VALUES ('" + a.ID + "', 'r', 'exploded')",
		"INSERT INTO attempt_roles (attempt_id, role, outcome) VALUES ('" + a.ID + "', 'r', 'failed:')",
		"INSERT INTO operator_inputs (task_id, kind, at) VALUES (1, 'yell', " + at + ")",
		"INSERT INTO operator_inputs (task_id, kind, class, at) VALUES (1, 'guidance', 'chat', " + at + ")",
		"INSERT INTO operator_inputs (task_id, kind, text, dropped_reason, at) VALUES (1, 'guidance', 'a', 'b', " + at + ")",
		"INSERT INTO operator_inputs (task_id, kind, before_text, at) VALUES (1, 'guidance', '" + strings.Repeat("a", 1025) + "', " + at + ")",
		"INSERT INTO ratings (task_id, value, at) VALUES (1, 0, " + at + ")",
		"INSERT INTO review_cycles (task_id, n, exit, started_at) VALUES (1, 9, 'wedged', " + at + ")",
		"INSERT INTO review_cycles (task_id, n, started_at) VALUES (1, 1, " + at + ")", // the number is taken
		"INSERT INTO review_findings (task_id, fingerprint, outcome, first_cycle, last_cycle) VALUES (1, 'f', 'meh', 1, 1)",
		"INSERT INTO artifacts (task_id, kind, path, sha256, rev, at) VALUES (1, 'notes', 'p', '" + sha("x") + "', 1, " + at + ")",
		"INSERT INTO artifacts (task_id, kind, path, sha256, rev, at) VALUES (1, 'report', 'p', 'short', 1, " + at + ")",
		"INSERT INTO checks (task_id, kind, result, cause, source, at) VALUES (1, 'ci', 'pass', 'code', 'github', " + at + ")",
		"INSERT INTO checks (task_id, kind, result, source, at) VALUES (1, 'ci', 'fail', 'someone', " + at + ")",
		"INSERT INTO suspensions (task_id, stage, suspended_at) VALUES (1, 'coding', " + at + ")",
		"UPDATE questions SET flag = 'useless' WHERE id = 1",
	}
	// The suspension row above is only refused as the second open one.
	if err := r.Suspend(ctx, registry.Suspension{TaskID: task.ID, Stage: "coding"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range bad {
		if _, err := r.DBForTest().ExecContext(ctx, q); err == nil {
			t.Errorf("schema accepted: %s", q)
		}
	}
}
