package tui

// PR references as people type them, shared by the picker's filter and
// the board's rows.

import (
	"regexp"
	"strconv"
	"strings"
)

// pickRef is a PR reference typed into the filter.
type pickRef struct {
	owner, repo string // empty when not given
	number      int
}

var (
	pickURLRe   = regexp.MustCompile(`^https?://[^/\s]+/([^/\s]+)/([^/\s]+)/pull/(\d+)(?:[/?#]\S*)?$`)
	pickFullRe  = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)
	pickShortRe = regexp.MustCompile(`^([\w.-]+)#(\d+)$`)
	pickNumRe   = regexp.MustCompile(`^#?(\d+)$`)
)

// parsePickRef reads s as a PR URL, owner/repo#N, repo#N, #N or N.
func parsePickRef(s string) (pickRef, bool) {
	s = strings.TrimSpace(s)
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	if g := pickURLRe.FindStringSubmatch(s); g != nil {
		return pickRef{owner: g[1], repo: g[2], number: atoi(g[3])}, true
	}
	if g := pickFullRe.FindStringSubmatch(s); g != nil {
		return pickRef{owner: g[1], repo: g[2], number: atoi(g[3])}, true
	}
	if g := pickShortRe.FindStringSubmatch(s); g != nil {
		return pickRef{repo: g[1], number: atoi(g[2])}, true
	}
	if g := pickNumRe.FindStringSubmatch(s); g != nil {
		return pickRef{number: atoi(g[1])}, true
	}
	return pickRef{}, false
}
