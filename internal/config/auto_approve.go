package config

// Auto-approval: GitHub does not count a GitHub App's approval toward a
// branch's required approvals, so a PR magnum found clean still waits for
// the operator. A [[watch]] may name repositories on whose PRs magnum posts
// the operator's own approval (auto_approve, auto_approve_as) once its review
// of the head found nothing that must be fixed before merging, except on the
// PRs of the authors auto_approve_skip_authors lists and, where
// auto_approve_authors is set, of every author it does not list.

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
	"unicode"
)

// DefaultAutoApproveBody is the body of an automatic approval when the
// watch sets no auto_approve_body.
const DefaultAutoApproveBody = "Auto-approved: Magnum's review of `{{.Short}}` found no blocking problems{{if .ReviewURL}} ([review]({{.ReviewURL}})){{end}}."

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

// AutoApproveSkipsAuthor reports whether auto_approve_skip_authors keeps
// magnum from approving repository fullName's ("owner/name") PRs by login
// as the operator: the covering watch's list or the repository's [[repo]]
// block's names login, in any case, with or without "[bot]" (as
// skip_authors matches). A blank login matches nothing.
func (c *Config) AutoApproveSkipsAuthor(fullName, login string) bool {
	if w := c.WatchFor(fullName); w != nil && listsLogin(w.AutoApproveSkipAuthors, login) {
		return true
	}
	r := c.RepoFor(fullName)
	return r != nil && listsLogin(r.AutoApproveSkipAuthors, login)
}

// AutoApproveAllowsAuthor reports whether auto_approve_authors lets magnum
// approve repository fullName's ("owner/name") PRs by login as the operator:
// the repository's [[repo]] list when it is set (non-empty), which replaces
// the watch's there, else the covering watch's when it is set, names login
// in any case, with or without "[bot]" (as AutoApproveSkipsAuthor
// matches). With neither list set there is no allowlist: true for every
// login. A blank login is on no list. It does not ask the skip list, which
// wins: the engine asks AutoApproveSkipsAuthor first.
func (c *Config) AutoApproveAllowsAuthor(fullName, login string) bool {
	var allow []string
	if r := c.RepoFor(fullName); r != nil && len(r.AutoApproveAuthors) > 0 {
		allow = r.AutoApproveAuthors
	} else if w := c.WatchFor(fullName); w != nil {
		allow = w.AutoApproveAuthors
	}
	if len(allow) == 0 {
		return true
	}
	return strings.TrimSpace(login) != "" && listsLogin(allow, login)
}

// AutoApproveRefusesAuthor reports whether the author lists keep magnum
// from approving repository fullName's PRs by login on its own:
// auto_approve_skip_authors names login (AutoApproveSkipsAuthor), or an
// auto_approve_authors that applies does not (AutoApproveAllowsAuthor).
// The automatic approval asks each list to say why (the engine's
// authorRefusal); the judge's no-findings event asks this
// (pipeline.JudgeEvents), so a clean review of such an author is no
// approval either. A nil c refuses nobody.
func (c *Config) AutoApproveRefusesAuthor(fullName, login string) bool {
	return c != nil && (c.AutoApproveSkipsAuthor(fullName, login) || !c.AutoApproveAllowsAuthor(fullName, login))
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
// renders; auto_approve_skip_authors and auto_approve_authors list logins.
// ids are the declared identities by name.
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
	errs = append(errs, validateLogins(where, "auto_approve_skip_authors", w.AutoApproveSkipAuthors)...)
	return append(errs, validateLogins(where, "auto_approve_authors", w.AutoApproveAuthors)...)
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
