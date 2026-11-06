package cli

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// --revision r --question n names question n as revision r asked it: the
// answer goes to that question, wherever a newer revision carried it (a
// revision puts the open questions it carries over first), and a question
// no longer open anywhere is refused.
func TestFactoryAnswerBindsTheQuestionToTheRevisionItWasAskedIn(t *testing.T) {
	storetest.Serial(t)
	var got factory.AnswerPayload
	dt := newFactoryStartTest(t, 250, answerPayload(t, &got))
	task := seedQuestions(t, dt)
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	carried := registry.Question{Text: "Can a merchant turn it off?", Options: []registry.Option{{Label: "yes"}, {Label: "no"}}, Recommended: -1,
		RaisedBy: "fx-critic"}
	added := registry.Question{Text: "Which email template?", Options: []registry.Option{{Label: "the coupon one"}, {Label: "a new one"}},
		Recommended: 0, RaisedBy: "fx-points"}
	err = reg.ReplaceQuestions(context.Background(), task.ID, 2, []registry.Question{carried, added})
	reg.Close()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(file, []byte("No, always on.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "answer", "1", "--revision", "1", "--question", "2", "--text", file); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if got.Revision != 2 || !maps.Equal(got.Answers, map[int]string{0: "No, always on."}) {
		t.Fatalf("payload %+v, want revision 2's question 1, which carries revision 1's question 2", got)
	}

	// Revision 1's first question was voided and carried nowhere.
	dt.stderr.Reset()
	if code := dt.run("factory", "answer", "1", "--revision", "1", "--question", "1", "--text", file); code != 1 ||
		!strings.Contains(dt.stderr.String(), "no longer open") {
		t.Fatalf("exit %d, stderr %q; want 1: no longer open", code, dt.stderr)
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

// The liaison records what the operator said in its chat with --surface
// liaison and --shown, the time it noted before it asked: the daemon
// stores the surface with the input and counts the operator's time on the
// batch or the card from that time. --diff-read says the operator read the
// card's diff, as the time it was shown; without it the decision records an
// unread diff. Without the flags a command is the CLI's, as before.
func TestAnswerAndDecideRecordTheLiaisonsSurfaceAndTime(t *testing.T) {
	storetest.Serial(t)
	var answer factory.AnswerPayload
	var decide factory.DecidePayload
	var raw string
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		raw = string(q.Payload)
		switch q.Kind {
		case factory.ReqAnswer:
			answer = factory.AnswerPayload{}
			if err := json.Unmarshal(q.Payload, &answer); err != nil {
				t.Error(err)
			}
		case factory.ReqDecide:
			decide = factory.DecidePayload{}
			if err := json.Unmarshal(q.Payload, &decide); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("request kind %q", q.Kind)
		}
		return registry.RequestDone, "recorded"
	})
	task := seedQuestions(t, dt)
	decideTask, _ := seedReadyCard(t, dt)
	file := filepath.Join(t.TempDir(), "answers.json")
	if err := os.WriteFile(file, []byte(`{"revision": 1, "answers": {"1": "Seven days before it expires"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	shown := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

	if code := dt.run("factory", "answer", "PR-12345", "--json", file, "--surface", "liaison", "--shown", "2026-11-06T10:00:00Z"); code != 0 {
		t.Fatalf("answer: exit %d, stderr %s", code, dt.stderr)
	}
	if answer.TaskID != task.ID || answer.Surface != "liaison" || !answer.Shown.Equal(shown) {
		t.Errorf("answer payload %+v, want task %d, surface liaison, shown %v", answer, task.ID, shown)
	}

	// An offset is a time too, and the same instant.
	if code := dt.run("factory", "answer", "PR-12345", "--json", file, "--shown", "2026-11-06T12:00:00+02:00"); code != 0 {
		t.Fatalf("answer with an offset: exit %d, stderr %s", code, dt.stderr)
	}
	if answer.Surface != "cli" || !answer.Shown.Equal(shown) {
		t.Errorf("answer payload %+v, want the CLI's surface and the instant %v", answer, shown)
	}

	// Without the flags: the CLI's surface, and no shown time. The field is
	// left out of the request, so a daemon that predates it still takes the
	// answer.
	if code := dt.run("factory", "answer", "PR-12345", "--json", file); code != 0 {
		t.Fatalf("answer without the flags: exit %d, stderr %s", code, dt.stderr)
	}
	if answer.Surface != "cli" || !answer.Shown.IsZero() || strings.Contains(raw, "Shown") {
		t.Errorf("answer payload %+v (%s), want the CLI's surface and no shown time in the request", answer, raw)
	}

	if code := dt.run("factory", "decide", "PR-12360", "push", "--card", "2", "--surface", "liaison", "--shown", "2026-11-06T10:00:00Z",
		"--diff-read"); code != 0 {
		t.Fatalf("decide: exit %d, stderr %s", code, dt.stderr)
	}
	if decide.TaskID != decideTask.ID || decide.Choice != factory.ChoicePush || decide.Surface != "liaison" || !decide.Shown.Equal(shown) ||
		!decide.DiffOpened.Equal(shown) {
		t.Errorf("decide payload %+v, want a push from the liaison, shown %v, its diff read at that time", decide, shown)
	}

	// Without --diff-read the diff is unread, whatever else is said.
	if code := dt.run("factory", "decide", "PR-12360", "send_back", "--card", "2", "--reason", "too-much", "--note", "keep it small",
		"--surface", "liaison", "--shown", "2026-11-06T10:00:00Z"); code != 0 {
		t.Fatalf("send_back: exit %d, stderr %s", code, dt.stderr)
	}
	if decide.Choice != factory.ChoiceSendBack || decide.Surface != "liaison" || !decide.Shown.Equal(shown) || !decide.DiffOpened.IsZero() {
		t.Errorf("decide payload %+v, want a send back from the liaison with an unread diff", decide)
	}

	// A drop is bound to no card but takes the surface and the time.
	if code := dt.run("factory", "decide", "PR-12360", "drop", "--surface", "liaison", "--shown", "2026-11-06T10:00:00Z"); code != 0 {
		t.Fatalf("drop: exit %d, stderr %s", code, dt.stderr)
	}
	if decide.Choice != factory.ChoiceDrop || decide.Surface != "liaison" || !decide.Shown.Equal(shown) || !decide.DiffOpened.IsZero() {
		t.Errorf("decide payload %+v, want a drop from the liaison, shown %v", decide, shown)
	}

	// Without the flags: the CLI's surface, the time the command read the card
	// and an unread diff.
	read := time.Now()
	if code := dt.run("factory", "decide", "PR-12360", "push", "--card", "2"); code != 0 {
		t.Fatalf("decide without the flags: exit %d, stderr %s", code, dt.stderr)
	}
	if decide.Surface != "cli" || decide.Shown.Before(read) || !decide.DiffOpened.IsZero() {
		t.Errorf("decide payload %+v, want the CLI's surface, shown at the read of the card and an unread diff", decide)
	}

	// --diff-read without --shown is the time the command read the card.
	read = time.Now()
	if code := dt.run("factory", "decide", "PR-12360", "push", "--card", "2", "--diff-read"); code != 0 {
		t.Fatalf("decide --diff-read: exit %d, stderr %s", code, dt.stderr)
	}
	if decide.Shown.Before(read) || !decide.DiffOpened.Equal(decide.Shown) {
		t.Errorf("decide payload %+v, want the diff read when the card was shown", decide)
	}
}

// answer and decide take the surfaces cli and liaison and nothing else: the
// screen and a pane are not places this command is run from, and a typo
// would put the operator's input under a surface that means nothing. A shown
// time is an RFC 3339 time. Each is a usage error, before anything is read
// or sent.
func TestAnUnknownSurfaceIsRefused(t *testing.T) {
	storetest.Serial(t)
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		t.Errorf("request %q was sent", q.Kind)
		return registry.RequestDone, ""
	})
	for _, args := range [][]string{
		{"answer", "PR-12345", "--json", "a.json", "--surface", "screen"},
		{"answer", "PR-12345", "--json", "a.json", "--surface", "x"},
		{"decide", "PR-12360", "drop", "--surface", "screen"},
		{"decide", "PR-12360", "drop", "--surface", "x"},
		{"decide", "PR-12360", "drop", "--surface", "pane"},
	} {
		dt.stderr.Reset()
		code := dt.run("factory", args...)
		if got := dt.stderr.String(); code != 2 || !strings.Contains(got, "cli") || !strings.Contains(got, "liaison") ||
			!strings.Contains(got, args[len(args)-1]) {
			t.Errorf("%q: exit %d, stderr %q; want a usage error naming cli and liaison", args, code, got)
		}
	}
	for _, args := range [][]string{
		{"answer", "PR-12345", "--json", "a.json", "--shown", "yesterday"},
		{"answer", "PR-12345", "--json", "a.json", "--shown", "2026-11-06 10:00"},
		{"decide", "PR-12360", "drop", "--shown", "10:00"},
	} {
		dt.stderr.Reset()
		code := dt.run("factory", args...)
		if got := dt.stderr.String(); code != 2 || !strings.Contains(got, "RFC 3339") || !strings.Contains(got, args[len(args)-1]) {
			t.Errorf("%q: exit %d, stderr %q; want a usage error naming RFC 3339", args, code, got)
		}
	}
}
