package factory

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// linearQuestion is intake's question of a Linear-tracked repository's
// prompt task, for the tracker whose keys start with prefix.
func linearQuestion(prefix string) string { return "Which Linear issue is this (" + prefix + "…)?" }

// linearRepo makes example/site, the second repository of twoRepos, the
// only Linear-tracked one that matters: [board] names no tracker, so its
// only tracker is Linear's (EX- keys), and its PRs' titles follow
// "[{key}] {title}".
func (r *intakeRig) linearRepo() {
	r.t.Helper()
	r.twoRepos()
	r.cfg.Board.Trackers = nil
	r.cfg.Factory.Repos[1].TitleFormat = "[{key}] {title}"
}

// mustStartIn starts text in repository repo and waits for the start's work
// off the loop.
func (r *intakeRig) mustStartIn(repo, text string) registry.Task {
	r.t.Helper()
	task, err := r.e.start(r.ctx, StartPayload{Text: text, Repo: repo})
	if err != nil {
		r.t.Fatalf("start %q in %s: %v", text, repo, err)
	}
	r.e.waitBackground()
	return task
}

// A free prompt in a repository whose trackers are Linear's asks one plain
// question, as the first of its questions: which Linear issue it is, with
// the tracker's key prefix, and "no issue" as the only option (the factory
// reads no Linear, so there is no search and no ticket). The question is
// stored when the task starts, without Jira, and nothing asks it again.
func TestAPromptOfALinearTrackedRepositoryAsksWhichLinearIssueOnce(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	task := r.mustStartIn("example/site", "The export fails on Safari, see https://github.com/example/site/issues/88")
	if task.Repo != "example/site" || task.JiraKey != "" {
		t.Fatalf("the task %+v, want a prompt task of example/site with no key", task)
	}
	for range 2 {
		qs, err := r.reg.Questions(r.ctx, task.ID)
		if err != nil || len(qs) != 1 {
			t.Fatalf("questions %+v (%v), want the one question which Linear issue it is", qs, err)
		}
		q := qs[0]
		if q.Text != linearQuestion("EX-") || q.Text != "Which Linear issue is this (EX-…)?" || q.Revision != 1 ||
			q.State != registry.QuestionOpen || q.RaisedBy != intakeRole {
			t.Fatalf("question %+v", q)
		}
		if len(q.Options) != 1 || q.Options[0].Label != noIssue || q.Recommended != 0 || q.Why == "" || q.Example == "" {
			t.Fatalf("options %+v, recommended %d (%q), want only %q recommended", q.Options, q.Recommended, q.Why, noIssue)
		}
		r.tick() // the loop, a restart: nothing asks again
		r.restart()
		r.tick()
		r.e.waitBackground()
	}
	if got := r.reload(task.ID); got.QuestionRev != 1 {
		t.Errorf("the task is at question revision %d, want 1", got.QuestionRev)
	}
	if n, calls := len(r.site.searched), r.site.curlCalls(); n != 0 || calls != 0 {
		t.Errorf("Jira was searched %d times and called %d times for a prompt of a Linear-tracked repository", n, calls)
	}
}

// With Jira off, a Linear-tracked repository's prompt task asks all the same.
func TestTheLinearQuestionNeedsNoJira(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	r.deps.Jira = nil
	r.restart()
	task := r.mustStartIn("example/site", "The export fails on Safari")
	if qs, _ := r.reg.Questions(r.ctx, task.ID); len(qs) != 1 || qs[0].Text != linearQuestion("EX-") {
		t.Errorf("a prompt with [jira] off asks %+v, want the Linear question", qs)
	}
}

// A Jira-tracked repository's prompt task asks the Jira question as it
// does today, and a repository whose trackers are neither Jira's nor
// Linear's asks nothing.
func TestAJiraTrackedRepositoryKeepsTheJiraQuestion(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	app := r.mustStartIn("example/app", "The reminder email comes late, see https://github.com/example/app/issues/9")
	qs, _ := r.reg.Questions(r.ctx, app.ID)
	if app.Repo != "example/app" || len(qs) != 1 || qs[0].Text != "Which Jira issue is this?" || strings.Contains(qs[0].Text, "Linear") {
		t.Errorf("a prompt in example/app (%s) asks %+v, want the Jira question", app.Repo, qs)
	}
	if len(r.site.searched) != 1 {
		t.Errorf("Jira was searched %d times for the prompt of example/app, want once", len(r.site.searched))
	}

	r.cfg.Repos[1].Trackers = []string{"https://tracker.example.com/issues/EX-{num}"}
	site := r.mustStartIn("example/site", "The export fails on Safari, see https://github.com/example/site/issues/88")
	if qs, _ := r.reg.Questions(r.ctx, site.ID); site.Repo != "example/site" || len(qs) != 0 {
		t.Errorf("a prompt in example/site (%s), whose tracker is neither Jira's nor Linear's, asks %+v", site.Repo, qs)
	}
}

// Your answer to the Linear question gives the task its key, where a Jira
// key is kept: the PR's title follows the repository's title_format with
// it ("[EX-12] Title"), and the branch the PR is pushed to carries it. The
// key is never read from Jira. A link of the tracker names its key too.
func TestAnAnswerNamingALinearKeyGivesItsPRTitleAndBranchTheKey(t *testing.T) {
	for name, answer := range map[string]string{
		"key":             "EX-12",
		"key and words":   "EX-12 Export fails on Safari",
		"link":            "https://linear.app/example/issue/EX-12/export-fails-on-safari",
		"key then colon":  "EX-12: the export, I think",
		"lower case":      "ex-12",
		"mixed case":      "Ex-12 export fails",
		"branch name":     "alice/ex-12-export-fails",
		"key in words":    "it is EX-12",
		"key in a line":   "The ticket is (EX-12), see the export",
		"link in words":   "this one: https://linear.app/example/issue/EX-12/export-fails-on-safari",
		"first key wins":  "EX-12 or EX-13, not sure",
		"other key first": "EXP-9 is not it, EX-12 is",
	} {
		t.Run(name, func(t *testing.T) {
			r := newIntakeRig(t)
			r.linearRepo()
			task := r.mustStartIn("example/site", "Export fails on Safari")
			r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: answer}})
			got := r.reload(task.ID)
			if got.JiraKey != "EX-12" || got.Title != "Export fails on Safari" {
				t.Fatalf("task with key %q titled %q, want EX-12 linked, its title kept", got.JiraKey, got.Title)
			}
			if title := prTitle(got, r.e.titleFormat(got.Repo)); title != "[EX-12] Export fails on Safari" {
				t.Errorf("the PR's title is %q, want [EX-12] Export fails on Safari", title)
			}
			if err := r.e.nameRemoteBranch(r.ctx, &got); err != nil || got.RemoteBranch != "zhuravel-EX-12-export-fails-safari" {
				t.Errorf("the PR's branch %q (%v), want zhuravel-EX-12-export-fails-safari", got.RemoteBranch, err)
			}
			r.tick()
			r.e.waitBackground()
			if n := r.site.curlCalls(); n != 0 {
				t.Errorf("Jira was called %d times for a Linear key", n)
			}
			if again := r.reload(task.ID); again.JiraKey != "EX-12" || again.WaitsWho == "Jira" {
				t.Errorf("after a tick the task is %+v, want EX-12 linked and no wait for Jira", again)
			}
			if answers := r.taskFile(task.ID, answersFile); !strings.Contains(answers, answer) {
				t.Errorf("answers.md does not hold the answer:\n%s", answers)
			}
		})
	}
}

// "no issue" links nothing: the PR's title is the title alone, with no
// empty brackets.
func TestNoIssueGivesALinearTrackedTasksPRTheTitleAlone(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	task := r.mustStartIn("example/site", "Export fails on Safari")
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: noIssue}})
	got := r.reload(task.ID)
	if got.JiraKey != "" {
		t.Fatalf("task with key %q, want none", got.JiraKey)
	}
	if title := prTitle(got, r.e.titleFormat(got.Repo)); title != "Export fails on Safari" {
		t.Errorf("the PR's title is %q, want Export fails on Safari", title)
	}
}

// A key of no tracker of the repository (a typo of the prefix) is no
// silent miss, in any case: the answer is refused, naming the prefix to type,
// and the question stays open. A key another task holds is refused as a Jira
// key is. An answer with no key at all is words, and is taken.
func TestAWrongLinearKeyIsRefusedAndTheQuestionStaysOpen(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	task := r.mustStartIn("example/site", "Export fails on Safari")
	for _, wrong := range []string{"EXP-12", "exp-12", "Exp-12 the export", "EXP-12: export fails"} {
		q := r.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: wrong}})
		if q.State != registry.RequestFailed || !strings.Contains(strings.ToUpper(q.Result), "EXP-12") || !strings.Contains(q.Result, "EX-") {
			t.Fatalf("%q: request %s: %q; want refused, naming EXP-12 and the prefix EX-", wrong, q.State, q.Result)
		}
	}
	if got := r.reload(task.ID); got.JiraKey != "" {
		t.Fatalf("task with key %q, want none", got.JiraKey)
	}
	if qs := r.newestQuestions(task.ID); len(qs) != 1 || qs[0].State != registry.QuestionOpen {
		t.Fatalf("questions %+v, want the one still open", qs)
	}

	other, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "EX-12", Kind: "feature", Repo: "example/site", State: registry.TaskActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, held := range []string{"EX-12", "ex-12", "branch alice/ex-12-export"} {
		q := r.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: held}})
		if q.State != registry.RequestFailed || !strings.Contains(q.Result, "EX-12") || !strings.Contains(q.Result, fmt.Sprintf("task %d", other.ID)) {
			t.Fatalf("%q: request %s: %q; want refused, naming the task %d that holds EX-12", held, q.State, q.Result, other.ID)
		}
	}
	// Words that name no key are words: the answer is taken, the task links nothing.
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "no ticket yet, it is a quick fix"}})
	if got := r.reload(task.ID); got.JiraKey != "" {
		t.Errorf("words linked the key %q", got.JiraKey)
	}
}

// A task started with its Linear key (startOther) has a title that begins
// with the bare key, "EX-12 Export fails": its PR's title under
// "[{key}] {title}" holds the key once, "[EX-12] Export fails".
func TestATaskStartedWithALinearKeyGetsItsKeyOnceInThePRTitle(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	task := r.mustStart("EX-12 Export fails on Safari")
	if task.JiraKey != "EX-12" || task.Title != "EX-12 Export fails on Safari" || task.Repo != "example/site" {
		t.Fatalf("the task %+v, want EX-12 in example/site, its title beginning with the key", task)
	}
	if title := prTitle(task, r.e.titleFormat(task.Repo)); title != "[EX-12] Export fails on Safari" {
		t.Errorf("the PR's title is %q, want [EX-12] Export fails on Safari", title)
	}
	if qs, _ := r.reg.Questions(r.ctx, task.ID); len(qs) != 0 {
		t.Errorf("a task of a key asks %+v", qs)
	}
}

// The Linear question recommends what the Jira question recommends when no
// issue can be told: "no issue", the one option. It is the same default for
// both trackers, so the tracker does not change how a task is asked.
func TestTheLinearQuestionRecommendsWhatTheJiraQuestionDoes(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	r.site.search = searchAnswer() // Jira knows no title that shares a word
	jiraQ, _ := r.reg.Questions(r.ctx, r.mustStartIn("example/app", "The reminder email comes late").ID)
	linearQ, _ := r.reg.Questions(r.ctx, r.mustStartIn("example/site", "The export fails on Safari").ID)
	if len(jiraQ) != 1 || len(linearQ) != 1 {
		t.Fatalf("questions %+v and %+v, want one each", jiraQ, linearQ)
	}
	label := func(q registry.Question) string { return q.Options[q.Recommended].Label }
	if label(jiraQ[0]) != noIssue || label(linearQ[0]) != label(jiraQ[0]) {
		t.Errorf("the Jira question recommends %q, the Linear one %q, want both %q", label(jiraQ[0]), label(linearQ[0]), noIssue)
	}
}

// The gate amends the report for an answer that changes the plan, and for
// no answer to intake's question: a Linear key you type costs no agent turn
// (the factory reads no ticket of it), and neither does "no issue".
func TestALinearAnswerNeedsNoAmend(t *testing.T) {
	for name, answer := range map[string]string{"a key": "EX-12", "a lower-case branch name": "alice/ex-12-export-fails", "no issue": noIssue} {
		t.Run(name, func(t *testing.T) {
			r, task := linkRigOn(t, func(r *intakeRig) { r.cfg.Board.Trackers = []string{siteTracker} },
				registry.Question{Text: whichLinear("EX-"), RaisedBy: intakeRole, Recommended: 0,
					Options: []registry.Option{{Label: noIssue, Means: "the PR's title carries no issue key"}}})
			r.clock.Add(time.Minute)
			r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: answer}})
			got := r.reload(task.ID)
			wantKey := map[bool]string{true: "EX-12"}[answer != noIssue]
			if got.Stage != "coding" || got.JiraKey != wantKey || r.site.curlCalls() != 0 || len(r.rt.launchedRoles()) != 0 {
				t.Fatalf("task %s at %s, key %q, %d Jira calls, launches %v; want coding, key %q, no Jira call and no amend", got.State, got.Stage,
					got.JiraKey, r.site.curlCalls(), r.rt.launchedRoles(), wantKey)
			}
		})
	}
}

// A title that starts with a longer key the task's key is a prefix of
// (ABC-12 for the key ABC-1) does not hold the task's key: the key goes in
// front, in either case and either format.
func TestATitleStartingWithALongerKeyStillGetsTheTasksKey(t *testing.T) {
	for _, tc := range []struct{ title, format, want string }{
		{"ABC-12 Title", "", "ABC-1 ABC-12 Title"},
		{"abc-12 Title", "", "ABC-1 abc-12 Title"},
		{"[ABC-12] Title", "[{key}] {title}", "[ABC-1] [ABC-12] Title"},
		{"ABC-1 Title", "", "ABC-1 Title"},
		{"[abc-1] Title", "[{key}] {title}", "[ABC-1] Title"},
	} {
		got := prTitle(registry.Task{Title: tc.title, JiraKey: "ABC-1"}, tc.format)
		if got != tc.want {
			t.Errorf("prTitle(%q, %q) = %q, want %q", tc.title, tc.format, got, tc.want)
		}
	}
}
