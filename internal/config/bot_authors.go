package config

// Login lists: skip_bot_authors skips every bot's PR, and allow_bot_authors
// on a [[watch]] (a [[repo]] block's adds to it) names the bots whose PRs
// are reviewed all the same. The lists here and auto_approve_skip_authors
// match logins as github.SameLogin does.

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/github"
)

// AllowsBotAuthor reports whether allow_bot_authors lets repository
// fullName's ("owner/name") PRs by login through skip_bot_authors: the
// covering watch's list or the repository's [[repo]] block's names login,
// in any case, with or without "[bot]" (github.SameLogin, so a bot's GraphQL
// login "helper" is helper[bot]). A blank login matches nothing. Whether
// login is a bot is the caller's business: the list only lifts the bot rule.
func (c *Config) AllowsBotAuthor(fullName, login string) bool {
	if w := c.WatchFor(fullName); w != nil && listsLogin(w.AllowBotAuthors, login) {
		return true
	}
	r := c.RepoFor(fullName)
	return r != nil && listsLogin(r.AllowBotAuthors, login)
}

// listsLogin reports whether logins names login: github.SameLogin of the
// two lowercased, so a "[Bot]" suffix in any case is one too, as
// skip_authors reads it.
func listsLogin(logins []string, login string) bool {
	login = strings.ToLower(login)
	return slices.ContainsFunc(logins, func(l string) bool { return github.SameLogin(strings.ToLower(l), login) })
}

// validateLogins checks a list of GitHub logins under key (allow_bot_authors,
// auto_approve_skip_authors) of a [[watch]] or [[repo]] block (where names
// it): none empty or with whitespace in it. Logins that differ only in case
// or "[bot]" match as one, so a duplicate is no error.
func validateLogins(where, key string, logins []string) []error {
	var errs []error
	for _, l := range logins {
		if l == "" || strings.ContainsFunc(l, unicode.IsSpace) {
			errs = append(errs, fmt.Errorf("%s%s entry %q must be a GitHub login (not empty, no whitespace)", where, key, l))
		}
	}
	return errs
}
