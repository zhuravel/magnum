package factory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// siteTracker is the tracker of example/site's issues: not Jira (a Linear
// link), so its keys are never read from Jira.
const siteTracker = "https://linear.app/example/issue/EX-{num}"

// twoRepos makes the rig's factory work in two repositories: example/app,
// whose [[repo]] names the Jira tracker of PR- keys, and example/site, whose
// [[repo]] names a tracker that is not Jira (EX- keys). One [[watch]] covers
// both with no tracker of its own; [board]'s Jira tracker of PR- keys is
// every repository's, so it tells neither.
func (r *intakeRig) twoRepos() {
	r.t.Helper()
	repo := func(name string) config.FactoryRepo {
		return config.FactoryRepo{Repo: name, Base: "master", MainClone: r.t.TempDir(),
			Copies: filepath.Join(r.t.TempDir(), "copy{n}"), CopyName: "factory{n}", MaxCopies: 3}
	}
	r.cfg.Factory.Repos = []config.FactoryRepo{repo("example/app"), repo("example/site")}
	r.cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}}}
	r.cfg.Repos = []config.Repo{
		{Repo: "example/app", Trackers: []string{jiraTestSite + "/browse/PR-{num}"}},
		{Repo: "example/site", Trackers: []string{siteTracker}},
	}
}

// With two factory repositories a start's text tells its repository: a
// GitHub link of one of them first, else the first key whose prefix is a
// tracker of exactly one of them (its [[repo]]'s or its [[watch]]'s, never
// [board]'s, which every repository shares); the preview names it and why,
// and the start makes its task there. A text that tells none names no
// repository, and the start asks for one.
func TestAStartTellsItsRepositoryByALinkOrATrackersKey(t *testing.T) {
	r := newIntakeRig(t)
	r.twoRepos()
	for _, c := range []struct{ text, repo, why string }{
		{"the CSV export drops a row, see https://github.com/example/site/blob/master/app/export.rb#L40", "example/site", "from the link"},
		{"PR-12345 the advocates only", "example/app", "from the PR tracker"},
		{"EX-12 the export fails on Safari", "example/site", "from the EX tracker"},
		{"https://linear.app/example/issue/EX-14/export the export fails", "example/site", "from the EX tracker"},
		{"the export fails as in EX-12, see https://github.com/example/app/issues/88", "example/app", "from the link"},
		{"the export of https://github.com/other/thing/pull/3 fails as in EX-12", "example/site", "from the EX tracker"},
		{"the export fails on Safari", "", ""},
		// A key inside a link or a word (a branch's name) is no key, as for the keys found in a text.
		{"the export of https://github.com/other/thing/tree/zhuravel-EX-12-fix fails", "", ""},
		{"the export fails on zhuravel-EX-12-fix", "", ""},
	} {
		f, err := r.e.preview(r.ctx, c.text)
		if err != nil {
			t.Fatal(err)
		}
		if f.Repo != c.repo || f.RepoWhy != c.why || !slices.Equal(f.Repos, []string{"example/app", "example/site"}) {
			t.Errorf("%q: the preview's repository is %q (%q) of %q, want %q (%q)", c.text, f.Repo, f.RepoWhy, f.Repos, c.repo, c.why)
		}
	}
	// No repository told: the preview names none and draws nothing else,
	// and the start asks for one.
	if f, _ := r.e.preview(r.ctx, "the export fails on Safari"); f.Err != "" || f.Kind != "" || f.Branch != "" {
		t.Errorf("a preview with no repository: %+v", f)
	}
	if _, err := r.start("the export fails on Safari"); err == nil || err.Error() != "which repository? name one of example/app, example/site (--repo)" {
		t.Errorf("a start that tells no repository = %v", err)
	}
	if task := r.mustStart("PR-12345 the advocates only"); task.Repo != "example/app" {
		t.Errorf("the task of a key of example/app's tracker is in %q", task.Repo)
	}
	if task := r.mustStart("the CSV export drops a row, see https://github.com/example/site/issues/88"); task.Repo != "example/site" {
		t.Errorf("the task of a link of example/site is in %q", task.Repo)
	}
	// A tracker the watch names is both repositories': its key tells none.
	r.cfg.Watches[0].Trackers = []string{"https://linear.app/example/issue/WEB-{num}"}
	if f, _ := r.e.preview(r.ctx, "WEB-7 the export fails"); f.Repo != "" {
		t.Errorf("a key of a tracker of both repositories told %q", f.Repo)
	}
}

// A repository you name wins over the one the text tells, on the screen
// (its pick) as in the CLI (--repo); with one factory repository it is that
// one, and the screen draws no row for it.
func TestARepositoryYouNameAlwaysWins(t *testing.T) {
	r := newIntakeRig(t)
	r.twoRepos()
	const text = "the CSV export drops a row, see https://github.com/example/site/issues/88"
	f, err := r.e.previewIn(r.ctx, text, "example/app")
	if err != nil {
		t.Fatal(err)
	}
	if f.Repo != "example/app" || f.RepoWhy != "your pick" || f.Kind == "" {
		t.Errorf("the preview of your pick: %+v", f)
	}
	task, err := r.e.start(r.ctx, StartPayload{Text: text, Repo: "example/app"})
	if err != nil || task.Repo != "example/app" {
		t.Fatalf("a start with the repository named is in %q (%v)", task.Repo, err)
	}
	if _, err := r.e.start(r.ctx, StartPayload{Text: text, Repo: "example/other"}); err == nil ||
		!strings.Contains(err.Error(), "example/app, example/site") {
		t.Errorf("an unknown repository named = %v", err)
	}

	one := newIntakeRig(t)
	for _, text := range []string{"The weekly digest is off", "The weekly digest is off, see https://github.com/talkable/talkable/issues/1"} {
		if f, _ := one.e.preview(one.ctx, text); f.Repo != "talkable/talkable" || f.RepoWhy != "" || len(f.Repos) > 1 {
			t.Errorf("%q: the preview of the only repository: %q (%q) of %q", text, f.Repo, f.RepoWhy, f.Repos)
		}
	}
}

// A prefix that two factory repositories send to different trackers tells
// neither (config.TrackersFor lets one repository's PR- lead to Linear while
// [board]'s PR- leads to Jira for the others): there is no inference, you
// pick, and the key is read from Jira only when your pick's tracker for it
// is Jira's. A [[repo]] that repeats [board]'s tracker still tells its
// repository (TestAStartTellsItsRepositoryByALinkOrATrackersKey).
func TestAPrefixOfTwoTrackersTellsNoRepository(t *testing.T) {
	r := newIntakeRig(t)
	r.twoRepos()
	r.cfg.Repos = []config.Repo{{Repo: "example/site", Trackers: []string{"https://linear.app/example/issue/PR-{num}"}}}
	const text = "PR-12345 the advocates only"
	if f, _ := r.e.preview(r.ctx, text); f.Repo != "" || f.Kind != "" || f.NotJira != "" {
		t.Errorf("a prefix of two trackers told a repository: %+v", f)
	}
	if _, err := r.start(text); err == nil || !strings.Contains(err.Error(), "which repository?") {
		t.Errorf("a start of a prefix of two trackers = %v, want the question which repository", err)
	}
	asked := func() int {
		r.site.mu.Lock()
		defer r.site.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(r.site.asked), func(k string) bool { return k != "PR-12345" }))
	}
	if n := asked(); n != 0 {
		t.Fatalf("Jira was asked for PR-12345 %d times before a repository was chosen", n)
	}
	if f, _ := r.e.previewIn(r.ctx, text, "example/app"); f.Jira == nil || f.Jira.Key != "PR-12345" || f.NotJira != "" {
		t.Errorf("example/app's PR- is Jira's, but the preview: %+v", f)
	}
	if f, _ := r.e.previewIn(r.ctx, text, "example/site"); f.Jira != nil || f.NotJira != "PR-12345" {
		t.Errorf("example/site's PR- is Linear's, but the preview: %+v", f)
	}
	task, err := r.e.start(r.ctx, StartPayload{Text: text, Repo: "example/site"})
	if err != nil || task.Repo != "example/site" || task.JiraKey != "PR-12345" || task.Prompt != text {
		t.Fatalf("the start in example/site: %+v, %v", task, err)
	}
	if n := asked(); n != 1 {
		t.Errorf("Jira was asked for PR-12345 %d times, want once: the preview of example/app's", n)
	}
}

// A free prompt in a repository whose trackers are all not Jira starts from
// your words: no question asks which Jira issue it is, and no Jira search is
// made. A repository with a Jira tracker, or with no tracker at all, keeps
// the question.
func TestAPromptInARepositoryWithoutAJiraTrackerAsksNoJiraIssue(t *testing.T) {
	r := newIntakeRig(t)
	r.twoRepos()
	r.cfg.Board.Trackers = nil // example/site's only tracker is Linear's
	site := r.mustStart("the export fails on Safari, see https://github.com/example/site/issues/88")
	if qs, _ := r.reg.Questions(r.ctx, site.ID); site.Repo != "example/site" || len(qs) != 0 {
		t.Errorf("a prompt in example/site (%s) asks %+v", site.Repo, qs)
	}
	r.site.mu.Lock()
	searched := len(r.site.searched)
	r.site.mu.Unlock()
	if searched != 0 {
		t.Errorf("Jira was searched %d times for a prompt of example/site", searched)
	}
	app := r.mustStart("the reminder email comes late, see https://github.com/example/app/issues/9")
	if qs, _ := r.reg.Questions(r.ctx, app.ID); app.Repo != "example/app" || len(qs) != 1 || qs[0].Text != whichIssue {
		t.Errorf("a prompt in example/app (%s), whose tracker is Jira's, asks %+v", app.Repo, qs)
	}
	bare := newIntakeRig(t)
	bare.cfg.Board.Trackers = nil
	task := bare.mustStart("The weekly digest is off")
	if qs, _ := bare.reg.Questions(bare.ctx, task.ID); len(qs) != 1 || qs[0].Text != whichIssue {
		t.Errorf("a prompt in a repository with no tracker asks %+v", qs)
	}
}

// A key of a tracker that is not Jira (example/site's EX- keys) is never
// read from Jira: at the start of the text, as a key or a link of its
// tracker, it is the task's key and the first word of its title, and the
// task starts from your words (prompt.md, no ticket, no question which Jira
// issue it is), with no "not in Jira" question; the task's first stage reads
// no ticket for it either. A key alone is no task: there are no words to
// start from. In the middle of the words such a key is never found.
func TestAKeyOfATrackerThatIsNotJiraIsNeverReadFromJira(t *testing.T) {
	r := newIntakeRig(t)
	r.twoRepos()
	const text = "EX-12 the export fails on Safari"
	f, err := r.e.preview(r.ctx, text)
	if err != nil {
		t.Fatal(err)
	}
	if f.NotJira != "EX-12" || f.Err != "" || f.WordsOnly || f.Jira != nil || f.Title != "EX-12 the export fails on Safari" ||
		f.Kind != "bug" || f.Branch != "zhuravel-EX-12-export-fails-safari" {
		t.Fatalf("the preview of a key of another tracker: %+v", f)
	}
	task := r.mustStart(text)
	if task.JiraKey != "EX-12" || task.Repo != "example/site" || task.Title != "EX-12 the export fails on Safari" || task.Prompt != text ||
		task.Kind != "bug" || task.Branch != "zhuravel-EX-12-export-fails-safari" {
		t.Fatalf("the task of a key of another tracker: %+v", task)
	}
	if prompt := r.taskFile(task.ID, "prompt.md"); !strings.Contains(prompt, text) {
		t.Errorf("prompt.md:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(r.e.d.Layout.FactoryTaskDir(task.ID), ticketFile)); err == nil {
		t.Error("a key of another tracker has a ticket.md")
	}
	if qs, _ := r.reg.Questions(r.ctx, task.ID); len(qs) != 0 {
		t.Errorf("the task asks %+v", qs)
	}
	if _, err := r.start(text); err == nil || !strings.Contains(err.Error(), "EX-12 already runs as task") {
		t.Errorf("a second start of the key = %v, want the task that holds it", err)
	}
	link := r.mustStart("https://linear.app/example/issue/EX-14/export-fails the export fails for long names")
	if link.JiraKey != "EX-14" || link.Title != "EX-14 the export fails for long names" {
		t.Errorf("the task of a link of another tracker: %+v", link)
	}
	// A key alone says why it is no task and what to do, in the preview, in
	// the start and in the start request's answer (what the CLI prints).
	const alone = "the factory reads no linear.app issue: add what to do after EX-15"
	if _, err := r.start("EX-15"); err == nil || err.Error() != alone {
		t.Errorf("a key of another tracker alone = %v", err)
	}
	if f, _ := r.e.preview(r.ctx, "EX-15"); f.Err != alone || f.WordsOnly || f.Kind != "" {
		t.Errorf("the preview of a key of another tracker alone: %+v", f)
	}
	id, err := r.reg.EnqueueRequest(r.ctx, ReqStart, StartPayload{Text: "EX-15"})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	if q, _ := r.reg.RequestByID(r.ctx, id); q.State != registry.RequestFailed || q.Result != alone {
		t.Errorf("the start request of a key alone: %s %q", q.State, q.Result)
	}
	// Your pick of a repository without a tracker of EX- keeps the key
	// another factory repository's tracker that is not Jira names: never read.
	if f, _ := r.e.previewIn(r.ctx, "EX-17 the export fails", "example/app"); f.NotJira != "EX-17" || f.Err != "" || f.WordsOnly {
		t.Errorf("the preview of EX-17 in example/app: %+v", f)
	}
	if picked, err := r.e.start(r.ctx, StartPayload{Text: "EX-17 the export fails", Repo: "example/app"}); err != nil ||
		picked.JiraKey != "EX-17" || picked.Repo != "example/app" {
		t.Errorf("the start of EX-17 in example/app: %+v, %v", picked, err)
	}
	r.tick() // the first stage begins: no linked ticket is read for the key
	r.e.waitBackground()
	if got := r.reload(task.ID); got.JiraKey != "EX-12" {
		t.Errorf("the first stage dropped the key: %+v", got)
	}

	// In the words, with no Jira project named to tell a key from a word,
	// a key of another tracker is never found.
	r.cfg.Board.Trackers = nil
	words := r.mustStart("the export fails on Safari as in EX-16, see https://github.com/example/site/issues/88")
	if words.JiraKey != "" || words.Repo != "example/site" {
		t.Errorf("a prompt naming a key of another tracker: %+v", words)
	}
	r.site.mu.Lock()
	asked := slices.Clone(r.site.asked)
	r.site.mu.Unlock()
	if i := slices.IndexFunc(asked, func(k string) bool { return strings.HasPrefix(k, "EX-") }); i >= 0 {
		t.Errorf("Jira was asked for %s: %q", asked[i], asked)
	}
	if n := len(r.tasks()); n != 4 {
		t.Errorf("%d tasks, want 4", n)
	}
	// The task's prompts call its key the issue of its tracker, never a
	// Jira issue; a Jira key's prompts keep "the Jira issue".
	if d := r.e.promptData(task, config.FactoryStage{}, config.Role{}, config.Role{}, Copy{}); d.Key != "EX-12" || d.Tracker != "linear.app" {
		t.Errorf("the prompt data of EX-12: key %q, tracker %q", d.Key, d.Tracker)
	}
	jira := r.mustStart("PR-12345 the advocates only")
	if d := r.e.promptData(jira, config.FactoryStage{}, config.Role{}, config.Role{}, Copy{}); d.Key != "PR-12345" || d.Tracker != "" {
		t.Errorf("the prompt data of PR-12345: key %q, tracker %q", d.Key, d.Tracker)
	}
	if ins, _ := r.reg.Inputs(r.ctx, task.ID); len(ins) != 1 || ins[0].Kind != registry.InputTaskText || ins[0].Text != text {
		t.Errorf("the start's input: %+v", ins)
	}
}
