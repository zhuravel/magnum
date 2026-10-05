package config

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
)

// Every identity ends its reviews with the built-in footer unless it sets its
// own, and review_footer = "" turns the footer off.
func TestReviewFooterDefaultsToTheBuiltInLineAndEmptyTurnsItOff(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": `
[[identity]]
name = "plain"
kind = "gh"
login = "zhuravel"
[[identity]]
name = "own"
kind = "gh"
login = "alice"
review_footer = "_Reviewed by the team's bot; reply on the thread._"
[[identity]]
name = "off"
kind = "gh"
login = "rev-ann"
review_footer = ""
`})
	for name, want := range map[string]string{
		"plain": DefaultReviewFooter,
		"own":   "_Reviewed by the team's bot; reply on the thread._",
		"off":   "",
	} {
		if got := cfg.IdentityByName(name).Footer(); got != want {
			t.Errorf("identity %s: Footer() = %q, want %q", name, got, want)
		}
	}
	if got := (Identity{}).Footer(); got != DefaultReviewFooter {
		t.Errorf("a zero identity's footer = %q", got)
	}
	if strings.ContainsAny(DefaultReviewFooter, "\r\n") || len([]rune(DefaultReviewFooter)) >= ReviewFooterMax {
		t.Errorf("the built-in footer breaks its own rule: %q", DefaultReviewFooter)
	}
}

// The footer is one paragraph under 400 characters: it is the last line of a
// review and a one-line field of the judge's <magnum> block.
func TestReviewFooterMustBeOneShortParagraph(t *testing.T) {
	long := strings.Repeat("a", ReviewFooterMax)
	for name, footer := range map[string]string{
		"two paragraphs": "_Automated review._\n\nReply on the thread.",
		"a line break":   "_Automated review._\nReply on the thread.",
		"a carriage":     "_Automated review._\rReply.",
		"a tab":          "_Automated\treview._",
		"400 characters": long,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Identities[0].ReviewFooter = &footer
			wantError(t, cfg.Validate(), "identity z: review_footer")
		})
	}
	for _, footer := range []string{"", long[:ReviewFooterMax-1], "_Automated review by magnum. Reply `fixed`._", "Ünïcödé — fine"} {
		cfg := validPipelineConfig()
		cfg.Identities[0].ReviewFooter = &footer
		if err := cfg.Validate(); err != nil {
			t.Errorf("footer %q: %v", footer, err)
		}
	}
}

// config.defaults.toml documents the footer every identity gets as a
// commented key; it is the Go default, word for word.
func TestDefaultsFileDocumentsTheBuiltInFooter(t *testing.T) {
	m := regexp.MustCompile(`(?m)^# review_footer = ("(?:[^"\\]|\\.)*")`).FindSubmatch(magnum.DefaultConfig)
	if m == nil {
		t.Fatal("config.defaults.toml has no commented `# review_footer = \"…\"` line")
	}
	got, err := strconv.Unquote(string(m[1]))
	if err != nil {
		t.Fatalf("unquote %s: %v", m[1], err)
	}
	if got != DefaultReviewFooter {
		t.Fatalf("config.defaults.toml documents footer %q, the built-in one is %q", got, DefaultReviewFooter)
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
