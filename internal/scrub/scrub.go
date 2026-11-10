// Package scrub masks the secrets, emails and host names in text before magnum keeps or shows it. It is a leaf
// both daemons import: the factory's runtime.Scrub keeps what you typed and what it sent through Text (spec
// section 11.1), and the review daemon masks the tails of failed CI job logs with Secrets. The improvement loop's
// scrub.py (.claude/skills/magnum-improve/scripts) is held to the patterns below by a test and a fuzz in this package.
package scrub

import (
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

// The key-value passes, in the order Secrets runs them: scrubPairs, scrubQuotedPairs, scrubEnvPairs. Since R58 the
// last two take keys the first does not, so the first runs first, on the text it saw before: a pass that takes a
// new key with no value may take the name of the next key for its value (the separator crosses a line break), and
// the next value would then show where the first pass masked it.

// scrubKeyWords are the key names whose value is a secret, for the two newer key-value patterns. scrubPairs keeps
// the names it had.
const scrubKeyWords = `(?:aws_)?(?:secret(?:_access)?_key(?:_base)?|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|master[_-]?key|signing[_-]?key|encryption[_-]?key|client[_-]?secret|authorization)`

// scrubQuotedValue is the value of a quoted key or an env-style name that starts with a quote: a double-quoted or
// single-quoted string, in which a backslash escapes the next character (`"a\"b"`), over lines too. It is
// written unrolled: the form (?:[^"\\]|\\.)* backtracks badly in a backtracking engine (scrub.py's) on a string that
// never ends, and this one does not.
const scrubQuotedValue = `"[^"\\]*(?:\\[\s\S][^"\\]*)*"|'[^'\\]*(?:\\[\s\S][^'\\]*)*'`

var (
	// scrubTokens are token shapes execx.Redact leaves: AWS access keys,
	// Slack, OpenAI and Anthropic, GitLab, Stripe and Google API keys.
	scrubTokens = regexp.MustCompile(strings.Join([]string{
		`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`,
		`\bxox[abposr]-[A-Za-z0-9-]{10,}`,
		`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`,
		`\bglpat-[A-Za-z0-9_-]{16,}`,
		`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`,
		`\bAIza[0-9A-Za-z_-]{35}\b`,
	}, "|"))
	// scrubPairs are key=value and key: value pairs whose value is a secret;
	// the key stays. This is the pattern as it was before R58, word for word,
	// and it stays so: scrubEnvPairs masks more, and runs after it.
	scrubPairs = regexp.MustCompile(`(?i)\b((?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret))(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
	// scrubQuotedPairs are the same pairs under a quoted key, as JSON, YAML
	// and TOML write them: "password": "x", 'token' = y. The key word starts
	// the key or follows a prefix of no space or quote that ends in a
	// character that is no ASCII letter or digit (an underscore, a hyphen or
	// a dot: "DB_PASSWORD", "x-api-key"), and it ends the key. Only the key
	// word is case-insensitive: a prefix that ends in the long s or the
	// Kelvin sign is one of non-word characters, as \b counts them. A value
	// is a quoted string (scrubQuotedValue) or a word up to a comma, a brace,
	// a bracket or white space. The key and the separator stay.
	scrubQuotedPairs = regexp.MustCompile(`(["'](?:[^"'\s]*[^0-9A-Za-z"'\s])?(?i:` + scrubKeyWords + `)["'])(\s*[=:]\s*)(` + scrubQuotedValue + `|[^\s,}\]]+)`)
	// scrubEnvPairs are the pairs of scrubPairs with the key words of
	// scrubKeyWords, and the key word may also follow an underscore
	// (DB_PASSWORD=, GITHUB_TOKEN:) or sit in a longer name that it ends.
	scrubEnvPairs = regexp.MustCompile(`(?i)((?:\b|_)` + scrubKeyWords + `)(\s*[=:]\s*)(` + scrubQuotedValue + `|\S+)`)
	scrubEmail    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// scrubHost is a dotted host name under a top-level domain a file name
	// does not end in, and an IPv4 address.
	scrubHost = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|dev|app|ai|cloud|internal|local|lan|corp|intra|info|biz|tech|xyz|site|us|uk|de|ua|eu|me|cc|tv|gg)\b|\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// Secrets masks the secrets in text and leaves emails and host names: execx.Redact (GitHub tokens, JWTs,
// Authorization values, private keys), then AWS keys and other token shapes, then the values of password=, token: and
// the like, also under a quoted key ("password": "x") and an env-style name (DB_PASSWORD=x). The key stays. It is for
// text where a host or an address is what the reader needs, such as a failing test's output in a CI log.
func Secrets(text string) string {
	text = execx.Redact(text)
	text = scrubTokens.ReplaceAllString(text, "<redacted>")
	text = scrubPairs.ReplaceAllString(text, "$1$2<redacted>")
	text = scrubQuotedPairs.ReplaceAllString(text, "$1$2<redacted>")
	text = scrubEnvPairs.ReplaceAllString(text, "$1$2<redacted>")
	return text
}

// Text masks what Secrets masks, then emails and host names. It is what the factory keeps of the words that pass
// through it (runtime.Scrub).
func Text(text string) string {
	text = Secrets(text)
	text = scrubEmail.ReplaceAllString(text, "<email>")
	text = scrubHost.ReplaceAllString(text, "<host>")
	return text
}
