package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
)

// DefaultReviewFooter is the footer template of every identity that sets no
// review_footer: the reviewed commit, then what the review is, how to answer
// a P0-P2 thread, in the words the reply classifier knows, and what starts a
// round (a push, outside the quiet hours and not on a draft the watch skips;
// a thread reply; a review request), collapsed under <details>
// (config.defaults.toml documents it word for word).
const DefaultReviewFooter = "**Reviewed commit:** `{{.Short}}`\n" +
	"\n" +
	"<details><summary>ℹ️ About Magnum</summary>\n" +
	"\n" +
	"Automated review by [Magnum](https://github.com/zhuravel/magnum). Reply on a P0-P2 thread with `fixed`, `not a bug: <why>` or `won't fix: <why>`" +
	"{{if .Simplify}}; simplifications are optional{{end}}. New pushes are re-reviewed automatically" +
	"{{with .QuietHours}} outside {{.}}{{end}}{{if .DraftsSkipped}}, drafts only on request{{end}}. " +
	"A thread reply gets an answer without a push, and a review request for `{{.RequestLogin}}` starts a round.\n" +
	"\n" +
	"</details>"

// ReviewFooterMax bounds review_footer, in characters.
const ReviewFooterMax = 2000

// FooterData is what a review_footer template renders with: the verified
// review magnum appends the footer to and the round that posted it.
type FooterData struct {
	SHA    string // the reviewed commit
	Short  string // its first 10 characters
	Repo   string // owner/name
	Number int    // the PR's number
	Login  string // the login that posted the review
	// Simplify: the PR's watch runs a role answering to the alias
	// "simplify", so the review may carry optional simplifications.
	Simplify bool
	// Clean: the review has no findings (still-open earlier ones included)
	// and no simplifications, by the judge's result counts.
	Clean      bool
	Event      string // APPROVE, COMMENT or REQUEST_CHANGES
	PostMerge  bool   // a review of commits GitHub merged before magnum reviewed them
	DeltaCheck bool   // the judge alone checked a small delta since its last review
	// QuietHours is [daemon] quiet_hours with the daemon's offset from UTC
	// (QuietHoursLabel: "03:00-12:00 UTC+3"); "" = none.
	QuietHours string
	// DraftsSkipped: the PR's watch skips drafts (include_drafts = false),
	// so a push to a draft starts no round; a review request does.
	DraftsSkipped bool
	// RequestLogin is the login a review request names to start a round:
	// the watch's poll login when it is a person's (a gh identity), else
	// Login.
	RequestLogin string
}

// QuietHoursLabel is the quiet-hours window spec (ParseQuietHours) as an
// author anywhere can read it: both clocks as HH:MM and at's offset from
// UTC ("03:00-12:00 UTC+3", "01:00-07:00 UTC-2:30", "22:00-06:00 UTC"). ""
// when spec is blank or invalid.
func QuietHoursLabel(spec string, at time.Time) string {
	w, ok, err := ParseQuietHours(spec)
	if !ok || err != nil {
		return ""
	}
	zone := "UTC"
	if _, off := at.Zone(); off != 0 {
		sign := "+"
		if off < 0 {
			sign, off = "-", -off
		}
		zone += sign + strconv.Itoa(off/3600)
		if m := off % 3600 / 60; m != 0 {
			zone += fmt.Sprintf(":%02d", m)
		}
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d %s", w.Start/60, w.Start%60, w.End/60, w.End%60, zone)
}

// Footer is the identity's footer template, which magnum renders
// (RenderFooter) and appends to every review the identity posts once it is
// verified: review_footer, trimmed, else DefaultReviewFooter; "" = no
// footer.
func (i Identity) Footer() string {
	if i.ReviewFooter != nil {
		return strings.TrimSpace(*i.ReviewFooter)
	}
	return DefaultReviewFooter
}

// RenderFooter renders footer template tmpl with d, trimmed ("" = the
// footer is left out).
func RenderFooter(tmpl string, d FooterData) (string, error) {
	t, err := template.New("review_footer").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// validateFooter checks an identity's footer template: at most
// ReviewFooterMax characters, no control characters but line breaks and
// tabs, and it renders with every field set, each bool both ways and
// QuietHours set and not.
func validateFooter(footer string) error {
	if footer == "" {
		return nil
	}
	if n := len([]rune(footer)); n > ReviewFooterMax {
		return fmt.Errorf("must be at most %d characters (it has %d)", ReviewFooterMax, n)
	}
	if strings.ContainsFunc(footer, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
		return errors.New("must not hold control characters other than line breaks and tabs")
	}
	for _, simplify := range []bool{true, false} {
		for _, clean := range []bool{true, false} {
			d := FooterData{SHA: "0123456789abcdef0123456789abcdef01234567", Short: "0123456789", Repo: "example/repo", Number: 1,
				Login: "example[bot]", Simplify: simplify, Clean: clean, Event: "COMMENT", PostMerge: !clean, DeltaCheck: clean,
				DraftsSkipped: simplify != clean, RequestLogin: "example"}
			if clean {
				d.QuietHours = "01:00-07:00 UTC"
			}
			if _, err := RenderFooter(footer, d); err != nil {
				return fmt.Errorf("does not render: %w", err)
			}
		}
	}
	return nil
}
