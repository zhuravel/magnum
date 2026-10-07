package github

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	urlRefRe   = regexp.MustCompile(`^(?:https?://)?(?:www\.)?(?i:github\.com)/([^/\s]+)/([^/\s]+)/pull/(\d+)(?:[/?#].*)?$`)
	fullRefRe  = regexp.MustCompile(`^([^/#\s]+)/([^/#\s]+)#(\d+)$`)
	shortRefRe = regexp.MustCompile(`^([^/#\s]+)#(\d+)$`)
	numRefRe   = regexp.MustCompile(`^#?(\d+)$`)
)

// ParseRef resolves a pull request reference: a URL
// https://github.com/o/r/pull/N[/...], "o/r#N", "r#N" (owner from
// defaultRepo), "#N" or "N" (both from defaultRepo, "owner/name").
func ParseRef(s, defaultRepo string) (owner, repo string, number int, err error) {
	s = strings.TrimSpace(s)
	var num string
	needDefault := false
	if m := urlRefRe.FindStringSubmatch(s); m != nil {
		owner, repo, num = m[1], m[2], m[3]
	} else if m := fullRefRe.FindStringSubmatch(s); m != nil {
		owner, repo, num = m[1], m[2], m[3]
	} else if m := shortRefRe.FindStringSubmatch(s); m != nil {
		repo, num, needDefault = m[1], m[2], true
	} else if m := numRefRe.FindStringSubmatch(s); m != nil {
		num, needDefault = m[1], true
	} else {
		return "", "", 0, fmt.Errorf("github: cannot parse pull request reference %q (want a URL, owner/repo#N, repo#N, #N or N)", s)
	}
	if needDefault {
		defOwner, defRepo, ok := strings.Cut(defaultRepo, "/")
		if !ok || checkRepo(defOwner, defRepo) != nil {
			return "", "", 0, fmt.Errorf("github: %q needs a default repository as owner/name, got %q", s, defaultRepo)
		}
		owner = defOwner
		if repo == "" {
			repo = defRepo
		}
	}
	if err := checkRepo(owner, repo); err != nil {
		return "", "", 0, fmt.Errorf("github: pull request reference %q: %w", s, err)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return "", "", 0, fmt.Errorf("github: pull request reference %q: invalid number %q", s, num)
	}
	return owner, repo, n, nil
}

// NormalizeLogin strips a trailing "[bot]" so REST logins ("talkable[bot]")
// compare equal to GraphQL logins ("talkable").
func NormalizeLogin(s string) string { return strings.TrimSuffix(s, "[bot]") }

// Account is a login as REST names the account: a bot's GraphQL login
// ("zhuravel" with __typename "Bot") gets its "[bot]" suffix, so an App
// named like a user ("zhuravel[bot]", the user "zhuravel") stays another
// account. A user's login, one already suffixed and "" come back as they are.
// The registry keeps logins in this form.
func Account(login, typename string) string {
	if login == "" || typename != "Bot" || strings.HasSuffix(login, "[bot]") {
		return login
	}
	return login + "[bot]"
}

// SameAccount compares two logins in Account form case-insensitively:
// "zhuravel[bot]" and "zhuravel" are different accounts.
func SameAccount(a, b string) bool { return a != "" && b != "" && strings.EqualFold(a, b) }

// SameLogin compares two logins case-insensitively after NormalizeLogin: the
// same name whether or not either is a bot, so an App and a user of the same
// name match. Use it only next to a check of the kind (IsAccount); SameAccount
// otherwise.
func SameLogin(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(NormalizeLogin(a), NormalizeLogin(b))
}

// IsBot reports whether an author is a bot: GraphQL __typename "Bot" or a
// REST login ending in "[bot]".
func IsBot(typename, login string) bool {
	return typename == "Bot" || strings.HasSuffix(login, "[bot]")
}

// IsAccount reports whether an author (its login in either form and its
// GraphQL __typename) is the account login names, an App's when bot: the
// same name (SameLogin) and the same kind of account (IsBot), so the user
// "zhuravel" is never the App "zhuravel[bot]".
func IsAccount(author, typename, login string, bot bool) bool {
	return SameLogin(author, login) && IsBot(typename, author) == bot
}
