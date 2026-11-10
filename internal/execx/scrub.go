package execx

import (
	"regexp"
	"strings"
)

var (
	// scrubTokens are token shapes Redact leaves: AWS access keys, Slack,
	// OpenAI and Anthropic, GitLab, Stripe and Google API keys, as the
	// factory's scrub (internal/factory/runtime) masks them.
	scrubTokens = regexp.MustCompile(strings.Join([]string{
		`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`,
		`\bxox[abposr]-[A-Za-z0-9-]{10,}`,
		`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`,
		`\bglpat-[A-Za-z0-9_-]{16,}`,
		`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`,
		`\bAIza[0-9A-Za-z_-]{35}\b`,
	}, "|"))
	// scrubPairs are key=value and key: value pairs whose value is a
	// secret; the key stays.
	scrubPairs = regexp.MustCompile(`(?i)\b((?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret))(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
	scrubEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// Scrub cleans output of code magnum does not trust before a file an agent
// reads keeps it (the CI reader's ci-failures.json, of a PR's own CI):
// Redact (GitHub tokens, JWTs, Authorization values, private keys), then
// the token shapes and secret pairs the factory's scrub masks (the values
// of password=, token: and the like, the key kept) and emails. Host names
// stay: an infrastructure failure names its host, and the file stays the
// fixer's. Redact's private-key rule spans lines, so Scrub runs on a whole
// text, never line by line.
func Scrub(s string) string {
	s = Redact(s)
	s = scrubTokens.ReplaceAllString(s, "<redacted>")
	s = scrubPairs.ReplaceAllString(s, "$1$2<redacted>")
	return scrubEmail.ReplaceAllString(s, "<email>")
}
