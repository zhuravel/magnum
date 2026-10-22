// Package textx holds the small text helpers several packages share:
// clipping to a number of runes, the first line, a short commit SHA, plurals,
// thousands separators, durations in words and login folding. It imports
// only the standard library.
package textx

import (
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Clip cuts s to at most n runes: a longer s becomes its first n-1 runes,
// spaces at their end dropped, and an ellipsis. n <= 0 means no limit.
// Normalizing s (trimming, collapsing spaces) is the caller's.
func Clip(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRightFunc(string(r[:n-1]), unicode.IsSpace) + "…"
}

// FirstLine is the first line of s that is not blank, trimmed.
func FirstLine(s string) string {
	for line := range strings.Lines(s) {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// ShortSHA is a commit SHA cut to 7 characters; a shorter string is kept.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Plural is one when n is 1, otherwise many.
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Count is n and its noun: "1 file", "3 files".
func Count(n int, one, many string) string {
	return strconv.Itoa(n) + " " + Plural(n, one, many)
}

// Thousands writes n with a comma between each three digits: "4,412".
func Thousands(n int) string {
	s := strconv.Itoa(n)
	digits, sign := strings.CutPrefix(s, "-")
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	if sign {
		return "-" + digits
	}
	return digits
}

// DurationWords is d in words, as a question says it, in its two largest
// units at most and whole: "45 seconds", "30 minutes", "1 hour", "1 hour 30
// minutes", "2 days 3 hours". A negative d is its absolute value.
func DurationWords(d time.Duration) string {
	const day = 24 * time.Hour
	d = max(d, -d)
	switch {
	case d < time.Minute:
		return Count(int(d/time.Second), "second", "seconds")
	case d < time.Hour:
		return Count(int(d/time.Minute), "minute", "minutes")
	case d < day:
		return countPair(int(d/time.Hour), "hour", "hours", int(d%time.Hour/time.Minute), "minute", "minutes")
	}
	return countPair(int(d/day), "day", "days", int(d%day/time.Hour), "hour", "hours")
}

// countPair is n of a unit and, when m is not 0, m of the next one.
func countPair(n int, one, many string, m int, nextOne, nextMany string) string {
	s := Count(n, one, many)
	if m > 0 {
		s += " " + Count(m, nextOne, nextMany)
	}
	return s
}

// FoldLogin folds a GitHub login for comparison: case, surrounding spaces, a
// leading "@" and a "[bot]" suffix do not matter, so "@Talkable[bot]" and
// "talkable" fold the same.
func FoldLogin(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(strings.TrimPrefix(s, "@"), "[bot]")
}

// MatchLogins returns a test of whether a login is one of logins, as
// FoldLogin folds them; an empty login matches none.
func MatchLogins(logins []string) func(login string) bool {
	set := make(map[string]bool, len(logins))
	for _, l := range logins {
		if k := FoldLogin(l); k != "" {
			set[k] = true
		}
	}
	return func(login string) bool { k := FoldLogin(login); return k != "" && set[k] }
}
