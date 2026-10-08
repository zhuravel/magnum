package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func readDocs(t *testing.T, path string) BaseDocs {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d BaseDocs
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return d
}

// docsPrompts gives the env the shipped claude-review and judge-initial
// prompts with a last line that renders the role data's DocsFile.
func (e *env) docsPrompts() {
	e.t.Helper()
	dir := e.t.TempDir()
	for _, name := range []string{"claude-review.md", "judge-initial.md"} {
		text, err := os.ReadFile(filepath.Join("..", "..", "prompts", name))
		if err != nil {
			e.t.Fatal(err)
		}
		text = append(text, "\ndocs-file=[{{.DocsFile}}]\n"...)
		if err := os.WriteFile(filepath.Join(dir, name), text, 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	e.cfg.Pipeline.PromptsDir = dir
}

// noDocs fails when the round wrote docs.json in dir or a prompt names one.
func (e *env) noDocs(dir string) {
	e.t.Helper()
	if _, err := os.Stat(filepath.Join(dir, DocsFile)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("%s: %v, want none", DocsFile, err)
	}
	for _, role := range []agents.Role{agents.RoleJudge, agents.RoleClaude} {
		for _, p := range e.ag.submitsFor(role) {
			if strings.Contains(p.Text, DocsFile) {
				e.t.Errorf("a %s prompt names the docs:\n%s", role, p.Text)
			}
		}
	}
}

// Agents opened a repository's docs only on the PRs that edited them, while
// most PRs change a path that a page of the base names. Before the
// reviewers, a round writes docs.json: for each changed path (lockfiles
// aside), the base's .md pages that name it, those naming it most often
// first; for the paths no page names, the pages that name their directory,
// two levels deep at least. The pages are read at origin/<base>, never in
// the checkout, and a page never counts for itself. Both prompts get the
// file; the event counts the pages and names none of them.
func TestARoundListsTheBaseDocsThatNameTheChangedFiles(t *testing.T) {
	e := newEnv(t)
	e.docsPrompts()
	e.git.modified = []string{"Gemfile.lock", "app/models/view_screenshot.rb", "spec/models/order_spec.rb", "Rakefile", "lib/thing.rb",
		".docs/wiki/index.md"}
	e.git.added = []string{"app/services/mailer/client.rb", "app/services/mailer/grouping.rb"}
	e.git.mentions = map[string]map[string]int{
		"app/models/view_screenshot.rb": {".docs/wiki/ai-assistant/view-testing.md": 1, ".docs/adr/0007-screenshots.md": 3},
		"Gemfile.lock":                  {".docs/wiki/dependencies.md": 1},
		".docs/wiki/index.md":           {".docs/wiki/index.md": 1},
		".docs/wiki/":                   {".docs/wiki/index.md": 9, "README.md": 1},
		"app/services/mailer/":          {".docs/wiki/mailer.md": 2, ".docs/wiki/errors.md": 2, ".docs/wiki/a.md": 1},
		"lib/":                          {"README.md": 9},
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(831, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Related = defaultRelated

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, DocsFile)
	want := BaseDocs{PR: "talkable/talkable#11920", HeadSHA: target, Base: "origin/master",
		Files: []PathDocs{{Path: "app/models/view_screenshot.rb", Pages: []string{".docs/adr/0007-screenshots.md", ".docs/wiki/ai-assistant/view-testing.md"}}},
		Dirs: []DirDocs{
			{Dir: ".docs/wiki", Paths: []string{".docs/wiki/index.md"}, Pages: []string{"README.md"}},
			{Dir: "app/services/mailer", Paths: []string{"app/services/mailer/client.rb", "app/services/mailer/grouping.rb"},
				Pages: []string{".docs/wiki/errors.md", ".docs/wiki/mailer.md", ".docs/wiki/a.md"}},
		},
	}
	if got := readDocs(t, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("docs.json = %+v\nwant %+v", got, want)
	}
	changes, greps := e.git.docsCalls()
	if want := []string{in.BaseSHA + "..." + target}; !slices.Equal(changes, want) {
		t.Errorf("ChangedPaths calls = %q, want %q", changes, want)
	}
	if want := []string{
		"origin/master :(top)*.md .docs/wiki/index.md,Rakefile,app/models/view_screenshot.rb,app/services/mailer/client.rb," +
			"app/services/mailer/grouping.rb,lib/thing.rb,spec/models/order_spec.rb",
		"origin/master :(top)*.md .docs/wiki/,app/services/mailer/,spec/models/",
	}; !slices.Equal(greps, want) {
		t.Errorf("Mentions calls = %q\nwant %q (the base's .md files only, no lockfile, no top-level directory)", greps, want)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "docs-file=["+path+"]")
	mustContain(t, "claude-review prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "docs-file=["+path+"]")

	evs := e.eventsOf("round.docs")
	if len(evs) != 1 || evs[0].Level != "info" {
		t.Fatalf("round.docs events = %+v", evs)
	}
	mustContain(t, "round.docs", evs[0].Message, DocsFile, "6 page(s)", "origin/master")
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data["pages"] != float64(6) || data["files"] != float64(1) || data["dirs"] != float64(2) {
		t.Errorf("round.docs data = %s (%v), want 6 pages, 1 file, 2 directories", evs[0].Data, err)
	}
	for _, ev := range e.events() {
		if strings.Contains(ev.Message+string(ev.Data), "mailer") || strings.Contains(ev.Message+string(ev.Data), "view_screenshot") {
			t.Errorf("event %s names a path: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}
}

// docs.json keeps to 10 pages per path and 40 paths or directories, the
// files first, and counts what it leaves out.
func TestTheDocsCapPagesAndEntries(t *testing.T) {
	e := newEnv(t)
	e.git.mentions = map[string]map[string]int{}
	many := map[string]int{}
	for i := range 12 {
		many[filepath.Join("docs", string(rune('a'+i))+".md")] = 12 - i
	}
	for i := range 41 {
		p := filepath.Join("app", "models", "m"+string(rune('a'+i/26))+string(rune('a'+i%26))+".rb")
		e.git.modified = append(e.git.modified, p)
		e.git.mentions[p] = many
	}
	e.git.added = []string{"app/services/x/new.rb"}
	e.git.mentions["app/services/x/"] = map[string]int{"docs/x.md": 1}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(832, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	got := readDocs(t, filepath.Join(res.ReportDir, DocsFile))
	if len(got.Files) != 40 || len(got.Dirs) != 0 || got.More != 2 {
		t.Fatalf("docs.json has %d files, %d directories, more %d; want 40, 0 and 2 (a file and the directory's path)", len(got.Files), len(got.Dirs), got.More)
	}
	if f := got.Files[0]; len(f.Pages) != 10 || f.More != 2 || f.Pages[0] != "docs/a.md" || f.Pages[9] != "docs/j.md" {
		t.Fatalf("first file = %+v, want docs/a.md to docs/j.md and 2 more", f)
	}
}

// A blind replay reads the pages at its merge base (base_sha), never at
// origin/<base>, whose pages may tell of the PR's own merge.
func TestABlindReplaysDocsAreTheMergeBases(t *testing.T) {
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.mentions = map[string]map[string]int{"app/models/order.rb": {"docs/orders.md": 1}}
	p := e.judgePosts(0, "", "COMMENT")
	p.gh, p.status = nil, "dry_run"
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindInitial)
	in.Blind, in.DryRun = true, true

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if got := readDocs(t, filepath.Join(res.ReportDir, DocsFile)); got.Base != in.BaseSHA || len(got.Files) != 1 {
		t.Fatalf("docs.json = %+v, want the merge base's", got)
	}
	if _, greps := e.git.docsCalls(); len(greps) != 1 || !strings.HasPrefix(greps[0], in.BaseSHA+" ") {
		t.Errorf("Mentions calls = %q, want the merge base only", greps)
	}
}

// No page naming a changed path leaves no file and no prompt naming one;
// the event still counts the round's pages (0), so the share of rounds
// with docs can be measured. A grep that fails or is cut only warns, and the
// review goes on.
func TestARoundWithoutDocsGoesOn(t *testing.T) {
	e := newEnv(t)
	e.docsPrompts()
	e.git.modified = []string{"app/models/order.rb", "Rakefile"}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(833, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noDocs(e.reportDir())
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "docs-file=[]")
	evs := e.eventsOf("round.docs")
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(string(evs[0].Data), `"pages":0`) {
		t.Fatalf("round.docs events = %+v", evs)
	}

	e = newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.grepErr = errors.New("fatal: bad revision 'origin/master'")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(834, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noDocs(e.reportDir())
	if evs := e.eventsOf("round.docs"); len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "bad revision") {
		t.Fatalf("round.docs events = %+v", evs)
	}
}

// The reviewers wait for the docs, so a grep that runs long is cut at
// DocsTimeout, and the round goes on without them.
func TestASlowDocsGrepIsCutShort(t *testing.T) {
	storetest.Serial(t) // swaps DocsTimeout
	defer func(d time.Duration) { DocsTimeout = d }(DocsTimeout)
	DocsTimeout = 50 * time.Millisecond
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.grepHangs = true
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(835, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noDocs(e.reportDir())
	if evs := e.eventsOf("round.docs"); len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "deadline exceeded") {
		t.Fatalf("round.docs events = %+v", evs)
	}
}

// A restart on a newer head writes that head's docs.json in its report
// directory, and the judge names that one.
func TestARestartWritesTheDocsOfTheNewHead(t *testing.T) {
	e := newEnv(t)
	e.docsPrompts()
	e.git.modified = []string{"app/models/order.rb"}
	e.git.mentions = map[string]map[string]int{"app/models/order.rb": {"docs/orders.md": 1}}
	in := e.input(KindInitial)
	e.withRestarts(&in, 2)
	pushAfterCodex := func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunVerified)
		e.push(head2)
		return nil
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushAfterCodex, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(836, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.Restarts != 1 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	old, newer := filepath.Join(e.dirOf(target), DocsFile), filepath.Join(e.dirOf(head2), DocsFile)
	if d := readDocs(t, old); d.HeadSHA != target {
		t.Fatalf("the first head's docs.json = %+v", d)
	}
	if d := readDocs(t, newer); d.HeadSHA != head2 {
		t.Fatalf("the new head's docs.json = %+v", d)
	}
	mustContain(t, "claude's first prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "docs-file=["+old+"]")
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "docs-file=["+newer+"]")
	if changes, _ := e.git.docsCalls(); !slices.Equal(changes, []string{in.BaseSHA + "..." + target, "base-" + head2[:7] + "..." + head2}) {
		t.Errorf("ChangedPaths calls = %q", changes)
	}
}
