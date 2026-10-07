package config

import (
	"regexp"
	"strings"
	"testing"
	"time"

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
		"`fixed`, `not a bug: <why>` or `won't fix: <why>`", "New pushes are re-reviewed automatically", ".\n\n</details>"} {
		if !strings.Contains(with, want) {
			t.Errorf("default footer lacks %q:\n%s", want, with)
		}
	}
	if with != strings.TrimSpace(with) {
		t.Errorf("rendered footer is not trimmed: %q", with)
	}
}

// "New pushes are re-reviewed automatically" was false for a draft a watch
// skips and during quiet hours, and the footer said nothing of replies or
// review requests: two authors waited 64 minutes and 4 hours after pushing
// to a draft before they requested a review. The footer says, from the
// watch's configuration, when a push is re-reviewed, that a reply gets an
// answer without a push and whose review request starts a round; it asks
// for a reply on P0-P2 threads only, P3 titles saying none is needed.
func TestDefaultFooterSaysWhatStartsARound(t *testing.T) {
	const tail = " A thread reply gets an answer without a push, and a review request for `zhuravel` starts a round.\n\n</details>"
	for _, tc := range []struct {
		name          string
		quiet         string
		draftsSkipped bool
		pushes        string
	}{
		{"neither", "", false, "New pushes are re-reviewed automatically."},
		{"quiet hours", "03:00-12:00 UTC+3", false, "New pushes are re-reviewed automatically outside 03:00-12:00 UTC+3."},
		{"drafts skipped", "", true, "New pushes are re-reviewed automatically, drafts only on request."},
		{"both", "22:00-06:00 UTC", true, "New pushes are re-reviewed automatically outside 22:00-06:00 UTC, drafts only on request."},
	} {
		d := FooterData{SHA: "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3", Short: "d4e5f6a7b8", Repo: "talkable/talkable", Number: 11920,
			Login: "talkable[bot]", Event: "COMMENT", QuietHours: tc.quiet, DraftsSkipped: tc.draftsSkipped, RequestLogin: "zhuravel"}
		got, err := RenderFooter(DefaultReviewFooter, d)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if want := "Reply on a P0-P2 thread with `fixed`, `not a bug: <why>` or `won't fix: <why>`. " + tc.pushes + tail; !strings.HasSuffix(got, want) {
			t.Errorf("%s: footer ends\n%q\nwant\n%q", tc.name, got, want)
		}
	}
}

// The footer names the quiet hours in the daemon's zone, as an offset from
// UTC an author anywhere can read; unset or invalid quiet hours name none.
func TestQuietHoursLabelNamesTheWindowWithItsUTCOffset(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for spec, want := range map[string]string{
		"03:00-12:00": "03:00-12:00 UTC",
		"3:00-7:30":   "03:00-07:30 UTC",
		"":            "",
		"07:00-07:00": "",
		"later":       "",
	} {
		if got := QuietHoursLabel(spec, at); got != want {
			t.Errorf("QuietHoursLabel(%q) = %q, want %q", spec, got, want)
		}
	}
	for zone, want := range map[*time.Location]string{
		time.FixedZone("EEST", 3*3600):          "01:00-07:00 UTC+3",
		time.FixedZone("EDT", -4*3600):          "01:00-07:00 UTC-4",
		time.FixedZone("IST", 5*3600+30*60):     "01:00-07:00 UTC+5:30",
		time.FixedZone("NDT", -(2*3600 + 1800)): "01:00-07:00 UTC-2:30",
	} {
		if got := QuietHoursLabel("01:00-07:00", at.In(zone)); got != want {
			t.Errorf("QuietHoursLabel in %s = %q, want %q", zone, got, want)
		}
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
