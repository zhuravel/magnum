package factory

import (
	"strings"
	"testing"

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
		"key":            "EX-12",
		"key and words":  "EX-12 Export fails on Safari",
		"link":           "https://linear.app/example/issue/EX-12/export-fails-on-safari",
		"key then colon": "EX-12: the export, I think",
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
			if answers := r.taskFile(task.ID, answersFile); !strings.Contains(answers, "EX-12") {
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
// silent miss: the answer is refused, naming the prefix to type, and the
// question stays open. A key another task holds is refused as a Jira
// key is.
func TestAWrongLinearKeyIsRefusedAndTheQuestionStaysOpen(t *testing.T) {
	r := newIntakeRig(t)
	r.linearRepo()
	task := r.mustStartIn("example/site", "Export fails on Safari")
	q := r.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "EXP-12"}})
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "EXP-12") || !strings.Contains(q.Result, "EX-") {
		t.Fatalf("request %s: %q; want refused, naming EXP-12 and the prefix EX-", q.State, q.Result)
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
	q = r.sendAnswers(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "EX-12"}})
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "EX-12") || !strings.Contains(q.Result, "task") {
		t.Fatalf("request %s: %q; want refused, naming the task %d that holds EX-12", q.State, q.Result, other.ID)
	}
	// Words that name no key are words: the answer is taken, the task links nothing.
	r.mustAnswer(AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "no ticket yet, it is a quick fix"}})
	if got := r.reload(task.ID); got.JiraKey != "" {
		t.Errorf("words linked the key %q", got.JiraKey)
	}
}
