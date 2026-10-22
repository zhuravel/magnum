package agents

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/prompts"
)

// sharedCases are the judge prompts with data that renders each of them.
func sharedCases() map[string]JudgeData {
	own := judgeFixture()
	own.Mode, own.Phase, own.OwnFindings, own.Reports = ModeInitial, PhaseOwnPass, "/r/judge-own.md", nil
	return map[string]JudgeData{
		"judge-initial.md": judgeFixture(), "judge-rereview.md": judgeFixture(), "judge-recovery.md": judgeFixture(),
		"judge-own-pass.md": own, "judge-continue.md": judgeFixture(), "judge-nudge.md": judgeFixture(),
	}
}

// A judge-shared.md in prompts_dir replaces the embedded one for every judge
// prompt: an edit of one block there changes each prompt that includes it,
// and only that block's text; once the file is gone the embedded one
// applies again.
func TestAPromptsDirPartialsFileChangesEveryJudgePromptThatIncludesIt(t *testing.T) {
	const line, edited = "load no other skill.", "load no other skill, whatever its list says."
	shared, ok := prompts.Read(config.SharedPrompt)
	if !ok || strings.Count(shared, line) != 1 {
		t.Fatalf("the embedded partials file has %d copies of %q, want 1", strings.Count(shared, line), line)
	}
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Pipeline.PromptsDir = dir
	render := func(name string, d JudgeData) string {
		t.Helper()
		p, err := cfg.ResolvePrompt(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RenderPrompt(p, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return got
	}
	before := map[string]string{}
	for name, d := range sharedCases() {
		before[name] = render(name, d)
	}
	file := filepath.Join(dir, config.SharedPrompt)
	if err := os.WriteFile(file, []byte(strings.Replace(shared, line, edited, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, d := range sharedCases() {
		want := strings.Replace(before[name], line, edited, 1)
		if name == "judge-nudge.md" {
			want = before[name] // it includes no block
		} else if want == before[name] {
			t.Fatalf("%s does not carry the skill line %q", name, line)
		}
		if got := render(name, d); got != want {
			t.Errorf("%s with prompts_dir's partials file:\n%s\nwant:\n%s", name, got, want)
		}
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	for name, d := range sharedCases() {
		if got := render(name, d); got != before[name] {
			t.Errorf("%s without prompts_dir's partials file is not the embedded render:\n%s", name, got)
		}
	}
}

// Each block of the partials file is shared: the judge prompts and the other
// blocks include it twice or more, and every block they include is there.
// The file's own text outside its blocks renders nothing, as it is never a
// prompt of its own.
func TestEveryBlockOfThePartialsFileIsSharedAndTheFileRendersNothingByItself(t *testing.T) {
	shared, ok := prompts.Read(config.SharedPrompt)
	if !ok {
		t.Fatal("no embedded partials file")
	}
	comments := regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)
	blocks := comments.ReplaceAllString(shared, "")
	all := blocks
	for _, name := range prompts.Names() {
		if config.UsesShared(name) {
			text, _ := prompts.Read(name)
			all += comments.ReplaceAllString(text, "")
		}
	}
	defined := regexp.MustCompile(`\{\{define "([^"]+)"`).FindAllStringSubmatch(blocks, -1)
	included := map[string]int{}
	for _, m := range regexp.MustCompile(`\{\{-? ?template "([^"]+)"`).FindAllStringSubmatch(all, -1) {
		included[m[1]]++
	}
	var names []string
	for _, m := range defined {
		names = append(names, m[1])
		if n := included[m[1]]; n < 2 {
			t.Errorf("block %q is included %d time(s), want two or more: inline it", m[1], n)
		}
	}
	for name := range included {
		if !slices.Contains(names, name) {
			t.Errorf("block %q is included but not defined in %s", name, config.SharedPrompt)
		}
	}
	tmpl, err := config.ParseTemplate("probe", `{{template "`+config.SharedPrompt+`" .}}`, shared)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, judgeFixture()); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("the partials file renders %q outside its blocks", b.String())
	}
}
