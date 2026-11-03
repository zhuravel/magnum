package registry_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

// askTwo is a revision of two questions: one with a recommendation, one without.
func askTwo() []registry.Question {
	return []registry.Question{
		{
			Text: "When does the reminder go out?", Context: "The ticket names no day.", Example: "A credit that expires on Fri 14 Nov.",
			Options: []registry.Option{
				{Label: "Seven days before it expires", Means: "the email goes out Fri 7 Nov"},
				{Label: "One day before it expires", Means: "Thu 13 Nov"},
			},
			Recommended: 0, Why: "the coupon reminder already uses 7 days",
		},
		{
			Text: "Can a merchant turn it off?", Context: "The ticket does not say.", Example: "A merchant opens Settings.",
			Options:     []registry.Option{{Label: "Yes", Means: "a switch"}, {Label: "No", Means: "always on"}},
			Recommended: -1,
		},
	}
}

func states(qs []registry.Question) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.State
	}
	return out
}

func TestReplaceQuestionsKeepsTheCardsFieldsInPositionOrder(t *testing.T) {
	t.Parallel()
	r, _ := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	got, err := r.Questions(ctx, task.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("Questions = %d rows, %v", len(got), err)
	}
	want := askTwo()
	for i := range want {
		want[i].ID, want[i].TaskID, want[i].Revision, want[i].Index = got[i].ID, task.ID, 1, i
		want[i].State, want[i].Asked = registry.QuestionOpen, t0
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("questions\n got %+v\nwant %+v", got, want)
	}
	if got[0].Recommended != 0 || got[1].Recommended != -1 {
		t.Errorf("recommendations = %d, %d; want 0 and -1 (no recommendation)", got[0].Recommended, got[1].Recommended)
	}
}

func TestReplaceQuestionsVoidsTheOpenOnes(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	if err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "Seven days before it expires"}); err != nil {
		t.Fatal(err)
	}

	// The evidence changed: revision 2 asks the second question again.
	if err := r.ReplaceQuestions(ctx, task.ID, 2, askTwo()[1:]); err != nil {
		t.Fatal(err)
	}
	got, err := r.Questions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{registry.QuestionAnswered, registry.QuestionVoided, registry.QuestionOpen}
	if !reflect.DeepEqual(states(got), want) {
		t.Errorf("question states = %v, want %v (an answer stays, the open one is voided)", states(got), want)
	}
	if got[2].Revision != 2 || got[2].Index != 0 {
		t.Errorf("new question = revision %d index %d, want revision 2 index 0", got[2].Revision, got[2].Index)
	}

	// An empty revision voids what is open and asks nothing.
	if err := r.ReplaceQuestions(ctx, task.ID, 3, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Questions(ctx, task.ID)
	if got[2].State != registry.QuestionVoided {
		t.Errorf("the revision 2 question is %s after an empty revision 3", got[2].State)
	}
}

func TestReplaceQuestionsRefusesARevisionThatIsNotNewer(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	if err := r.ReplaceQuestions(ctx, task.ID, 2, askTwo()); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []int{2, 1, 0} {
		if err := r.ReplaceQuestions(ctx, task.ID, rev, askTwo()); !errors.Is(err, registry.ErrStaleRevision) {
			t.Errorf("ReplaceQuestions at revision %d = %v, want ErrStaleRevision", rev, err)
		}
	}
	if got, _ := r.Questions(ctx, task.ID); len(got) != 2 || got[0].State != registry.QuestionOpen {
		t.Errorf("a refused replacement changed the questions: %v", states(got))
	}
}

func TestReplaceQuestionsRefusesWhatTheCardCannotShow(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	noText := askTwo()
	noText[1].Text = ""
	badPick := askTwo()
	badPick[0].Recommended = 2 // two options: 0 and 1
	zeroValue := []registry.Question{{Text: "Open question with no options and the zero Recommended"}}
	for name, qs := range map[string][]registry.Question{"no text": noText, "recommendation past the options": badPick, "zero recommended with no options": zeroValue} {
		if err := r.ReplaceQuestions(ctx, task.ID, 1, qs); err == nil {
			t.Errorf("%s: ReplaceQuestions succeeded", name)
		}
	}
	if got, _ := r.Questions(ctx, task.ID); len(got) != 0 {
		t.Errorf("refused questions left %d rows", len(got))
	}
	if err := r.ReplaceQuestions(ctx, 999, 1, askTwo()); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ReplaceQuestions for a missing task = %v, want ErrNotFound", err)
	}
}

func TestAnswerBatchRefusesAStaleRevision(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceQuestions(ctx, task.ID, 2, askTwo()); err != nil {
		t.Fatal(err)
	}

	err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "Seven days before it expires", 1: "No"})
	if !errors.Is(err, registry.ErrStaleRevision) {
		t.Fatalf("AnswerBatch for revision 1 = %v, want ErrStaleRevision", err)
	}
	for _, q := range mustQuestions(t, r, task.ID) {
		if q.State == registry.QuestionAnswered || q.Answer != "" {
			t.Errorf("a stale batch answered question %d of revision %d", q.Index, q.Revision)
		}
	}

	if err := r.AnswerBatch(ctx, task.ID, 3, map[int]string{0: "x"}); !errors.Is(err, registry.ErrStaleRevision) {
		t.Errorf("AnswerBatch for a revision that does not exist = %v, want ErrStaleRevision", err)
	}

	clk.Add(time.Minute)
	if err := r.AnswerBatch(ctx, task.ID, 2, map[int]string{1: "No"}); err != nil {
		t.Fatal(err)
	}
	var answered registry.Question
	for _, q := range mustQuestions(t, r, task.ID) {
		if q.State == registry.QuestionAnswered {
			answered = q
		}
	}
	if answered.Revision != 2 || answered.Index != 1 || answered.Answer != "No" || !answered.Answered.Equal(t0.Add(time.Minute)) {
		t.Errorf("answered question = %+v", answered)
	}
}

func TestAnswerBatchWithoutQuestionsIsStale(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	task := createTask(t, r, "PR-1")
	if err := r.AnswerBatch(t.Context(), task.ID, 1, map[int]string{0: "x"}); !errors.Is(err, registry.ErrStaleRevision) {
		t.Errorf("AnswerBatch on a task with no questions = %v, want ErrStaleRevision", err)
	}
}

func TestAnswerBatchIsAllOrNothing(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	if err := r.ReplaceQuestions(ctx, task.ID, 1, askTwo()); err != nil {
		t.Fatal(err)
	}
	if err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "Seven days before it expires", 5: "?"}); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("a batch naming a question that does not exist = %v, want ErrConflict", err)
	}
	if err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "Seven days before it expires", 1: "  "}); err == nil {
		t.Error("a batch with a blank answer succeeded")
	}
	if err := r.AnswerBatch(ctx, task.ID, 1, nil); err == nil {
		t.Error("an empty batch succeeded")
	}
	for _, q := range mustQuestions(t, r, task.ID) {
		if q.State != registry.QuestionOpen {
			t.Errorf("a refused batch left question %d %s", q.Index, q.State)
		}
	}

	if err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "Seven days before it expires"}); err != nil {
		t.Fatal(err)
	}
	if err := r.AnswerBatch(ctx, task.ID, 1, map[int]string{0: "Another answer"}); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("answering an answered question = %v, want ErrConflict", err)
	}
}

func mustQuestions(t *testing.T, r *registry.Registry, task int64) []registry.Question {
	t.Helper()
	qs, err := r.Questions(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func TestRecordDecisionBindsTheApprovalToTheHeadAndHashes(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-1")
	opened := t0.Add(-time.Minute)
	first := registry.Decision{TaskID: task.ID, Kind: "ready", Choice: "push", Note: "looks right",
		HeadSHA: "abc123", ReportHash: "r1", DiffHash: "d1", DiffOpened: opened}
	if err := r.RecordDecision(ctx, first); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	second := registry.Decision{TaskID: task.ID, Kind: "ready", Choice: "send back", HeadSHA: "def456"}
	if err := r.RecordDecision(ctx, second); err != nil {
		t.Fatal(err)
	}

	got, err := r.Decisions(ctx, task.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("Decisions = %d rows, %v", len(got), err)
	}
	first.At, second.At = t0, t0.Add(time.Minute)
	if got[0] != first || got[1] != second {
		t.Errorf("decisions\n got %+v\nwant %+v", got, []registry.Decision{first, second})
	}
	if !got[1].DiffOpened.IsZero() {
		t.Errorf("a decision made before the diff was opened has DiffOpened %v", got[1].DiffOpened)
	}

	if err := r.RecordDecision(ctx, registry.Decision{TaskID: 999, Kind: "ready", Choice: "push"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RecordDecision for a missing task = %v, want ErrNotFound", err)
	}
	if err := r.RecordDecision(ctx, registry.Decision{TaskID: task.ID, Kind: "ready"}); err == nil {
		t.Error("RecordDecision without a choice succeeded")
	}
}
