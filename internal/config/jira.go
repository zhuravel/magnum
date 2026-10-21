package config

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/fsx"
)

// Jira is the [jira] table: the site a round reads the PR's issue from
// (internal/jira), named by the first issue key of a tracker on that site
// in the PR's title or head branch. Off by default (no url).
type Jira struct {
	// URL is the Jira site, https only ("https://example.atlassian.net");
	// "" = off. Only a [board], [[watch]] or [[repo]] tracker whose template
	// starts with it names an issue to read.
	URL string `toml:"url"`
	// User is the account's email, which reads the issues with the token.
	User string `toml:"user"`
	// TokenFile holds the account's API token alone on one line: a regular
	// file of mode 0600 (~ and a path relative to the user config's
	// directory resolved). The token goes to curl on stdin, never in argv,
	// a log or an error.
	TokenFile string `toml:"token_file"`
}

// On reports whether rounds read the PR's Jira ticket.
func (j Jira) On() bool { return j.URL != "" }

// Names reports whether a tracker template (Tracker.URL) is one of the
// site's: it starts with the site's URL and a slash.
func (j Jira) Names(tracker string) bool {
	return j.On() && strings.HasPrefix(tracker, strings.TrimRight(j.URL, "/")+"/")
}

// validateJira checks the [jira] table when it is on: an https URL, the
// account's email, and a token file named. The token file itself is a
// warning's (jiraWarnings): a ticket is a hint, so a token file that is
// missing or open to others keeps the rounds from reading one, never the
// daemon from starting.
func (c *Config) validateJira() []error {
	j := c.Jira
	if !j.On() {
		return nil
	}
	var errs []error
	if err := checkHTTPSURL(j.URL, "the Jira site's https:// URL", "the site (https://example.atlassian.net)"); err != nil {
		errs = append(errs, fmt.Errorf("jira.url %w", err))
	}
	if j.User == "" || strings.ContainsFunc(j.User, func(r rune) bool { return r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) }) {
		errs = append(errs, fmt.Errorf("jira.user must be the Jira account's email (no colon, no space), got %q", j.User))
	}
	if j.TokenFile == "" {
		errs = append(errs, errors.New("jira.token_file must name the file holding the account's API token"))
	}
	return errs
}

// jiraWarnings are the [jira] problems that do not make the config invalid,
// when it is on: a token file that does not exist, is not a regular file or
// is open to its group or others (no round reads a ticket with it, and
// round.ticket says why).
func (c *Config) jiraWarnings() []string {
	j := c.Jira
	if !j.On() || j.TokenFile == "" {
		return nil
	}
	switch err := fsx.CheckPrivate(j.TokenFile); {
	case errors.Is(err, fs.ErrNotExist):
		return []string{fmt.Sprintf("jira.token_file %s does not exist: rounds hand the judge no Jira ticket until it does", j.TokenFile)}
	case err != nil:
		return []string{fmt.Sprintf("jira.token_file %v; rounds hand the judge no Jira ticket with it", err)}
	}
	return nil
}
