package tui

// The scopes the PR board and the factory screen share: the owner scope O
// cycles (the user or organization owning a row's repository) and the
// repository scope --repo names. Both screens order, cycle and match them
// with these, so O means one thing on both and the cli keeps one owner
// scope for the two.

import (
	"cmp"
	"slices"
	"strings"
)

// sameOwner reports whether a and b name one owner (case does not matter).
func sameOwner(a, b string) bool { return strings.EqualFold(a, b) }

// homeOwner is the owner O shows first: daemon.default_repo's ("owner/name"),
// else the repository scope's.
func homeOwner(defaultRepo, repo string) string {
	home, _, _ := strings.Cut(cmp.Or(defaultRepo, repo), "/")
	return home
}

// ownerOrder are the owners of a screen's rows (each as the first of them
// spells it; "" is none): home's first, then alphabetically, case aside.
func ownerOrder(owners []string, home string) []string {
	var out []string
	for _, o := range owners {
		if o != "" && !slices.ContainsFunc(out, func(s string) bool { return sameOwner(s, o) }) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if ha, hb := sameOwner(a, home), sameOwner(b, home); ha != hb {
			if ha {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	return out
}

// ownerAfter is the owner scope O moves to from cur among owners (in
// ownerOrder's order): from every owner ("") the first, from each the
// next, and from the last, or from one with no rows left, every owner again.
func ownerAfter(owners []string, cur string) string {
	switch i := slices.IndexFunc(owners, func(o string) bool { return sameOwner(o, cur) }); {
	case len(owners) == 0:
		return ""
	case cur == "":
		return owners[0]
	case i < 0 || i == len(owners)-1:
		return ""
	default:
		return owners[i+1]
	}
}

// ownerNote is the footer's word for an owner scope: "owner: example", or
// "owner: all".
func ownerNote(owner string) string { return "owner: " + cmp.Or(owner, "all") }

// repoInScope reports whether repository owner/name is in the repository
// scope ("name" or "owner/name", case aside; "" is every repository).
func repoInScope(owner, name, scope string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return true
	}
	if strings.Contains(scope, "/") {
		return scope == strings.ToLower(owner+"/"+name)
	}
	return scope == strings.ToLower(name)
}
