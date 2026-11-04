package cli

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// seedQuestions makes task PR-12345 in the factory registry of dt with
// revision 1 of two questions, the first recommending its first option.
func seedQuestions(t *testing.T, dt *daemonGroupTest) registry.Task {
	t.Helper()
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12345", Kind: "feature", Repo: "talkable/talkable", State: registry.TaskNeedsYou,
		Stage: "questions", WaitsWho: "you", WaitsWhat: "2 questions before coding"})
	if err != nil {
		t.Fatal(err)
	}
	qs := []registry.Question{
		{Text: "When does the reminder go out?", Context: "The ticket names no day.", Example: "a credit that expires on Fri 14 Nov",
			Options:     []registry.Option{{Label: "Seven days before it expires", Means: "Fri 7 Nov"}, {Label: "One day before", Means: "Thu 13 Nov"}},
			Recommended: 0, Why: "the coupon reminder uses 7 days", RaisedBy: "fx-judge"},
		{Text: "Can a merchant turn it off?", Options: []registry.Option{{Label: "yes"}, {Label: "no"}}, Recommended: -1,
			Why: "the ticket gives no hint", RaisedBy: "fx-critic"},
	}
	if err := reg.ReplaceQuestions(ctx, task.ID, 1, qs); err != nil {
		t.Fatal(err)
	}
	return task
}

// answerPayload is the daemon's side of the test: it decodes the request.
func answerPayload(t *testing.T, got *factory.AnswerPayload) func(registry.Request) (string, string) {
	t.Helper()
	return func(q registry.Request) (string, string) {
		if q.Kind != factory.ReqAnswer {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, got); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "PR-12345: 1 answer; it waits on you: 1 question before coding"
	}
}

// --json sends a batch, the questions by the numbers the listing prints:
// the request carries the registry's indexes, the revision and the flags.
func TestFactoryAnswerSendsTheBatchOfAJSONFile(t *testing.T) {
	storetest.Serial(t)
	var got factory.AnswerPayload
	dt := newFactoryStartTest(t, 250, answerPayload(t, &got))
	task := seedQuestions(t, dt)
	file := filepath.Join(t.TempDir(), "answers.json")
	if err := os.WriteFile(file, []byte(`{"revision": 1, "answers": {"1": "Seven days before it expires", "2": "ask:rev-ann"},
		"needless": {"2": "obvious"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "answer", "PR-12345", "--json", file); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	want := factory.AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "Seven days before it expires", 1: "ask:rev-ann"},
		Needless: map[int]string{1: "obvious"}, Surface: "cli"}
	if got.TaskID != want.TaskID || got.Revision != want.Revision || !maps.Equal(got.Answers, want.Answers) ||
		!maps.Equal(got.Needless, want.Needless) || got.Surface != want.Surface {
		t.Fatalf("payload %+v, want %+v", got, want)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "PR-12345: 1 answer") {
		t.Fatalf("stdout %q, want the daemon's answer", out)
	}

	// A file without the revision it answers, or with a key it does not
	// know, sends nothing.
	for _, bad := range []string{`{"answers": {"1": "yes"}}`, `{"revision": 1, "answer": {"1": "yes"}}`, `{"revision": 1, "answers": {"0": "yes"}}`} {
		if err := os.WriteFile(file, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		dt.stderr.Reset()
		if code := dt.run("factory", "answer", "1", "--json", file); code != 1 {
			t.Errorf("%s: exit %d, want 1 (stderr %s)", bad, code, dt.stderr)
		}
	}
}

// --question n --text file answers one question of the newest revision
// with the file's text: the answer of someone you asked.
func TestFactoryAnswerGivesTheTextOfAFileToOneQuestion(t *testing.T) {
	storetest.Serial(t)
	var got factory.AnswerPayload
	dt := newFactoryStartTest(t, 250, answerPayload(t, &got))
	task := seedQuestions(t, dt)
	file := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(file, []byte("Yes: a switch in the email settings, on by default.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "answer", "1", "--question", "2", "--text", file); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if got.TaskID != task.ID || got.Revision != 1 || !maps.Equal(got.Answers, map[int]string{1: "Yes: a switch in the email settings, on by default."}) {
		t.Fatalf("payload %+v, want question 2 (index 1) of revision 1 answered with the file's text", got)
	}
}

// Without a flag the command prints the questions of the newest revision,
// numbered as --json and --question take them; it sends nothing.
func TestFactoryAnswerWithoutAFlagListsTheQuestions(t *testing.T) {
	storetest.Serial(t)
	dt := newFactoryStartTest(t, 250, func(registry.Request) (string, string) {
		t.Error("the listing sent a request")
		return registry.RequestFailed, "unexpected"
	})
	seedQuestions(t, dt)
	if code := dt.run("factory", "answer", "PR-12345"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	for _, want := range []string{"revision 1", "2 of 2 open", "1  When does the reminder go out?", "Seven days before it expires",
		"(recommended: the coupon reminder uses 7 days)", "2  Can a merchant turn it off?", "no recommendation: the ticket gives no hint",
		"--json"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing lacks %q:\n%s", want, out)
		}
	}
}

// Flags that do not go together, and no task, are usage errors.
func TestFactoryAnswerRefusesFlagsThatDoNotGoTogether(t *testing.T) {
	storetest.Serial(t)
	dt := newFactoryStartTest(t, 250, nil)
	for _, args := range [][]string{
		{"answer"},
		{"answer", "1", "--json", "a.json", "--question", "1", "--text", "t"},
		{"answer", "1", "--question", "1"},
		{"answer", "1", "--text", "t"},
	} {
		dt.stderr.Reset()
		if code := dt.run("factory", args...); code != 2 || !strings.Contains(dt.stderr.String(), "usage") {
			t.Errorf("%q: exit %d, stderr %q; want a usage error", args, code, dt.stderr)
		}
	}
}
