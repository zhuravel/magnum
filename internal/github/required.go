package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// RequiredChecks reads the status checks a pull request into branch of
// owner/repo must pass: the contexts of the rulesets' required_status_checks
// rules (GET /repos/{o}/{r}/rules/branches/{branch}, readable with read
// access), else of the classic branch protection
// (.../branches/{branch}/protection/required_status_checks, which usually
// answers 404 without admin access). known is false when GitHub does not
// say: 403 (a private repository on a free plan) or 404 from the endpoint
// that would have to answer. Any other failure is an error.
func (c *Client) RequiredChecks(ctx context.Context, owner, repo, branch string) (checks []string, known bool, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, false, err
	}
	if !compareRefRe.MatchString(branch) || strings.Contains(branch, "..") {
		return nil, false, fmt.Errorf("github: invalid branch %q", branch)
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	op := fmt.Sprintf("rules %s/%s %s", owner, repo, branch)
	err = c.rest(ctx, op, "GET", fmt.Sprintf("repos/%s/%s/rules/branches/%s?per_page=100", owner, repo, branch), nil, false, &rules)
	if err != nil && !unanswered(err) {
		return nil, false, err
	}
	for _, r := range rules {
		if r.Type != "required_status_checks" {
			continue
		}
		for _, s := range r.Parameters.RequiredStatusChecks {
			if s.Context != "" && !slices.Contains(checks, s.Context) {
				checks = append(checks, s.Context)
			}
		}
	}
	if len(checks) > 0 {
		return checks, true, nil
	}

	var classic struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	op = fmt.Sprintf("branch protection %s/%s %s", owner, repo, branch)
	err = c.rest(ctx, op, "GET", fmt.Sprintf("repos/%s/%s/branches/%s/protection/required_status_checks", owner, repo, branch), nil, false, &classic)
	switch {
	case unanswered(err):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	checks = []string{}
	for _, s := range classic.Contexts {
		if s != "" && !slices.Contains(checks, s) {
			checks = append(checks, s)
		}
	}
	for _, s := range classic.Checks {
		if s.Context != "" && !slices.Contains(checks, s.Context) {
			checks = append(checks, s.Context)
		}
	}
	return checks, true, nil
}

// unanswered reports a 403 (not rate limiting) or 404: GitHub would not tell
// this identity.
func unanswered(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden)
}
