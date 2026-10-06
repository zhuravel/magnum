package config

// Auto-approval: GitHub does not count a GitHub App's approval toward a
// branch's required approvals, so a PR magnum found clean still waits for
// the operator. A [[watch]] may name repositories on whose PRs magnum posts
// the operator's own approval (auto_approve, auto_approve_as) once its review
// of the head found nothing that must be fixed before merging.

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
	"unicode"
)

// DefaultAutoApproveBody is the body of an automatic approval when the
// watch sets no auto_approve_body.
const DefaultAutoApproveBody = "Auto-approved: magnum's review of `{{.Short}}` found no blocking problems{{if .ReviewURL}} ([review]({{.ReviewURL}})){{end}}."

// AutoApproveBodyMax bounds auto_approve_body, in characters.
const AutoApproveBodyMax = 500

// AutoApproveData is what an auto_approve_body template renders with.
type AutoApproveData struct {
	SHA       string // the approved commit: the head magnum reviewed
	Short     string // its first 7 characters
	ReviewURL string // magnum's review the approval follows ("" when unknown)
	Repo      string // owner/name
	Number    int    // the PR's number
}

// AutoApproves reports whether any watch auto-approves a repository.
func (c *Config) AutoApproves() bool {
	for _, w := range c.Watches {
		if len(w.AutoApprove) > 0 && w.AutoApproveAs != "" {
			return true
		}
	}
	return false
}

// AutoApproveFor is the identity magnum auto-approves repository fullName's
// ("owner/name") PRs as: the covering watch's auto_approve_as when its
// auto_approve names the repository, or "*"; nil otherwise.
func (c *Config) AutoApproveFor(fullName string) *Identity {
	w := c.WatchFor(fullName)
	if w == nil || w.AutoApproveAs == "" {
		return nil
	}
	_, name, _ := strings.Cut(fullName, "/")
	for _, n := range w.AutoApprove {
		if n == "*" || strings.EqualFold(strings.TrimSpace(n), name) {
			return c.IdentityByName(w.AutoApproveAs)
		}
	}
	return nil
}

// AutoApproveBodyFor is the approval body template of repository
// fullName's PRs: its watch's auto_approve_body, trimmed, else
// DefaultAutoApproveBody.
func (c *Config) AutoApproveBodyFor(fullName string) string {
	if w := c.WatchFor(fullName); w != nil && w.AutoApproveBody != nil {
		return strings.TrimSpace(*w.AutoApproveBody)
	}
	return DefaultAutoApproveBody
}

// RenderAutoApproveBody renders an auto_approve_body template with d (Short
// filled from SHA when empty), trimmed.
func RenderAutoApproveBody(tmpl string, d AutoApproveData) (string, error) {
	if d.Short == "" {
		d.Short = d.SHA[:min(7, len(d.SHA))]
	}
	t, err := template.New("auto_approve_body").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// validateAutoApprove checks a watch's auto-approval keys: auto_approve
// names repositories the watch covers (or "*") and needs auto_approve_as,
// which names an identity of kind gh; auto_approve_body is one line that
// renders. ids are the declared identities by name.
func (w Watch) validateAutoApprove(ids map[string]Identity) []error {
	var errs []error
	where := "watch " + w.Owner + ": "
	if w.AutoApproveAs != "" {
		switch id, ok := ids[w.AutoApproveAs]; {
		case !ok:
			errs = append(errs, fmt.Errorf("%sunknown auto_approve_as %q", where, w.AutoApproveAs))
		case id.Kind != "gh":
			errs = append(errs, fmt.Errorf("%sauto_approve_as %q must be an identity of kind gh (your own account): GitHub does not count an App's approval", where, w.AutoApproveAs))
		}
	} else if len(w.AutoApprove) > 0 {
		errs = append(errs, fmt.Errorf("%sauto_approve needs auto_approve_as (the gh identity of your own account to approve as)", where))
	}
	for _, n := range w.AutoApprove {
		switch {
		case n == "*":
		case strings.TrimSpace(n) == "" || strings.ContainsAny(n, "/*?[]\\ "):
			errs = append(errs, fmt.Errorf("%sauto_approve entry %q must be a repository name of the watch (no owner, no pattern) or \"*\"", where, n))
		case !w.Matches(w.Owner, n):
			errs = append(errs, fmt.Errorf("%sauto_approve names %q, which the watch does not cover (include/exclude)", where, n))
		}
	}
	if w.AutoApproveBody != nil {
		if err := validateAutoApproveBody(strings.TrimSpace(*w.AutoApproveBody)); err != nil {
			errs = append(errs, fmt.Errorf("%sauto_approve_body %w", where, err))
		}
	}
	return errs
}

// validateAutoApproveBody checks an auto_approve_body template: one line of
// at most AutoApproveBodyMax characters that renders.
func validateAutoApproveBody(body string) error {
	if body == "" {
		return errors.New("must not be empty (leave it out for the default)")
	}
	if n := len([]rune(body)); n > AutoApproveBodyMax {
		return fmt.Errorf("must be at most %d characters (it has %d)", AutoApproveBodyMax, n)
	}
	if strings.ContainsFunc(body, unicode.IsControl) {
		return errors.New("must be one line (no line breaks or control characters)")
	}
	d := AutoApproveData{SHA: "0123456789abcdef0123456789abcdef01234567", ReviewURL: "https://github.com/example/repo/pull/1#pullrequestreview-1",
		Repo: "example/repo", Number: 1}
	if _, err := RenderAutoApproveBody(body, d); err != nil {
		return fmt.Errorf("does not render: %w", err)
	}
	return nil
}
