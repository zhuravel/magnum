package config

import (
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum"
)

// Every identity ends its reviews with the built-in footer template unless
// it sets its own, and review_footer = "" turns the footer off.
func TestReviewFooterDefaultsToTheBuiltInTemplateAndEmptyTurnsItOff(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": `
[[identity]]
name = "plain"
kind = "gh"
login = "zhuravel"
[[identity]]
name = "own"
kind = "gh"
login = "alice"
review_footer = """
_Reviewed by the team's bot at {{.Short}}._

Reply on the thread.
"""
[[identity]]
name = "off"
kind = "gh"
login = "rev-ann"
review_footer = ""
`})
	for name, want := range map[string]string{
		"plain": DefaultReviewFooter,
		"own":   "_Reviewed by the team's bot at {{.Short}}._\n\nReply on the thread.",
		"off":   "",
	} {
		if got := cfg.IdentityByName(name).Footer(); got != want {
			t.Errorf("identity %s: Footer() = %q, want %q", name, got, want)
		}
	}
	if got := (Identity{}).Footer(); got != DefaultReviewFooter {
		t.Errorf("a zero identity's footer = %q", got)
	}
	if err := validateFooter(DefaultReviewFooter); err != nil {
		t.Errorf("the built-in footer breaks its own rules: %v", err)
	}
}

// The footer is a text/template of FooterData: one that does not parse,
// names a field FooterData lacks, fails to render for any combination of
// Simplify and Clean, or is longer than 2,000 characters fails validation.
// Line breaks are allowed; "" turns it off.
func TestReviewFooterMustBeATemplateThatRenders(t *testing.T) {
	long := strings.Repeat("a", ReviewFooterMax+1)
	for name, footer := range map[string]string{
		"unparsable":          "Reviewed {{.Short",
		"unknown field":       "Reviewed {{.Commit}}",
		"unclosed if":         "{{if .Clean}}clean",
		"fails only if clean": "{{if .Clean}}{{.Nope}}{{end}}",
		"fails only if plain": "{{if not .Simplify}}{{index .Short 99}}{{end}}",
		"a function it lacks": "{{shout .Short}}",
		"over 2000":           long,
		"a NUL":               "_Automated review._\x00",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Identities[0].ReviewFooter = &footer
			wantError(t, cfg.Validate(), "identity z: review_footer")
		})
	}
	for _, footer := range []string{
		"", long[:ReviewFooterMax], "_Automated review by magnum. Reply `fixed`._", "Ünïcödé — fine",
		"two paragraphs\n\nare fine\r\nnow\twith a tab",
		"{{.SHA}} {{.Short}} {{.Repo}}#{{.Number}} {{.Login}} {{.Event}}{{if .Simplify}} s{{end}}{{if .Clean}} c{{end}}{{if .PostMerge}} pm{{end}}{{if .DeltaCheck}} dc{{end}}",
	} {
		cfg := validPipelineConfig()
		cfg.Identities[0].ReviewFooter = &footer
		if err := cfg.Validate(); err != nil {
			t.Errorf("footer %q: %v", footer, err)
		}
	}
}

// The built-in footer names the reviewed commit and collapses the rest
// under <details>; it says simplifications are optional only when the PR's
// watch runs a simplify role.
func TestDefaultFooterCollapsesAndNamesSimplificationsOnlyWhenSimplifyRuns(t *testing.T) {
	d := FooterData{SHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3", Short: "d4e5f6a7b8", Repo: "talkable/talkable", Number: 11920,
		Login: "talkable[bot]", Event: "COMMENT"}
	without, err := RenderFooter(DefaultReviewFooter, d)
	if err != nil {
		t.Fatal(err)
	}
	d.Simplify = true
	with, err := RenderFooter(DefaultReviewFooter, d)
	if err != nil {
		t.Fatal(err)
	}
	const phrase = "; simplifications are optional"
	if strings.Contains(without, phrase) || !strings.Contains(with, phrase) {
		t.Errorf("simplify phrase: without simplify %q, with %q", without, with)
	}
	for _, want := range []string{"**Reviewed commit:** `d4e5f6a7b8`\n\n<details><summary>",
		"About Magnum</summary>\n\n", "Automated review by [Magnum](https://github.com/zhuravel/magnum).",
		"`fixed`, `not a bug: <why>` or `won't fix: <why>`", "New pushes are re-reviewed automatically.\n\n</details>"} {
		if !strings.Contains(with, want) {
			t.Errorf("default footer lacks %q:\n%s", want, with)
		}
	}
	if with != strings.TrimSpace(with) {
		t.Errorf("rendered footer is not trimmed: %q", with)
	}
}

// config.defaults.toml documents the footer every identity gets as a
// commented multi-line key; uncommented, it is valid TOML that holds the Go
// default, word for word.
func TestDefaultsFileDocumentsTheBuiltInFooter(t *testing.T) {
	m := regexp.MustCompile(`(?ms)^# review_footer = '''\n.*?'''`).Find(magnum.DefaultConfig)
	if m == nil {
		t.Fatal("config.defaults.toml has no commented `# review_footer = '''…'''` block")
	}
	var lines []string
	for _, l := range strings.Split(string(m), "\n") {
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(l, "#"), " "))
	}
	doc := strings.Join(lines, "\n")
	var got struct {
		ReviewFooter string `toml:"review_footer"`
	}
	if _, err := toml.Decode(doc, &got); err != nil {
		t.Fatalf("the documented footer is not valid TOML once uncommented: %v\n%s", err, doc)
	}
	if strings.TrimSpace(got.ReviewFooter) != DefaultReviewFooter {
		t.Fatalf("config.defaults.toml documents footer %q, the built-in one is %q", got.ReviewFooter, DefaultReviewFooter)
	}
}

// claude-review re-reviews new commits at medium effort: of the candidates
// only claude-review raised, 3 were posted and 302 rejected, most of them
// speculative, style-only or pre-existing.
func TestClaudeReviewRereviewsAtMediumEffort(t *testing.T) {
	cr, ok := Defaults().RoleByNameOrAlias(nil, RoleClaudeReview)
	if !ok || cr.EffortFor(false) != "high" || cr.EffortFor(true) != "medium" {
		t.Fatalf("claude-review effort %q, rereview %q", cr.EffortFor(false), cr.EffortFor(true))
	}
}
