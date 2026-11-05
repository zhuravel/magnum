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
	if f, _ := one.e.preview(one.ctx, "The weekly digest is off"); f.Repo != "talkable/talkable" || f.RepoWhy != "" || len(f.Repos) > 1 {
		t.Errorf("the preview of the only repository: %q (%q) of %q", f.Repo, f.RepoWhy, f.Repos)
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
	if _, err := r.start("EX-15"); err == nil || !strings.Contains(err.Error(), "EX-15 is not a Jira issue") {
		t.Errorf("a key of another tracker alone = %v", err)
	}
	if f, _ := r.e.preview(r.ctx, "EX-15"); !strings.Contains(f.Err, "EX-15 is not a Jira issue") || f.WordsOnly {
		t.Errorf("the preview of a key of another tracker alone: %+v", f)
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
	if n := len(r.tasks()); n != 3 {
		t.Errorf("%d tasks, want 3", n)
	}
	if ins, _ := r.reg.Inputs(r.ctx, task.ID); len(ins) != 1 || ins[0].Kind != registry.InputTaskText || ins[0].Text != text {
		t.Errorf("the start's input: %+v", ins)
	}
}
