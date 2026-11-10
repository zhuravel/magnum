package runtime

// The scrub that runs before every place the factory keeps text (spec
// section 11.1): the prompt ledger, the operator's inputs, the events.

import (
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

// The limits of a message Scrub keeps: a longer one is a pasted block (a
// log, a stack trace, a file), which a capture of what the operator typed
// drops.
const (
	maxKeptLines = 40
	maxKeptBytes = 4000
)

// scrubKeyWords are the key names whose value is a secret, for both key-value
// patterns.
const scrubKeyWords = `(?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret)`

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
	// the key stays.
	scrubPairs = regexp.MustCompile(`(?i)\b(` + scrubKeyWords + `)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
	// scrubQuotedPairs are the same pairs under a quoted key, as JSON, YAML
	// and TOML write them: "password": "x", 'token' = y. The key word starts
	// at a word boundary, as scrubPairs' does (a hyphen or a dot before it
	// is one: "x-api-key"; an underscore or a letter is not), and ends the
	// key. Only the key word is case-insensitive: a prefix that ends in the
	// long s or the Kelvin sign is one of non-word characters, as \b counts
	// them. A value is a quoted string (also over lines) or a word up to a
	// comma, a brace, a bracket or white space. The key and the separator
	// stay.
	scrubQuotedPairs = regexp.MustCompile(`(["'](?:[^"'\s]*[^0-9A-Za-z_"'\s])?(?i:` + scrubKeyWords + `)["'])(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,}\]]+)`)
	scrubEmail       = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// scrubHost is a dotted host name under a top-level domain a file name
	// does not end in, and an IPv4 address.
	scrubHost = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|dev|app|ai|cloud|internal|local|lan|corp|intra|info|biz|tech|xyz|site|us|uk|de|ua|eu|me|cc|tv|gg)\b|\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// Scrub cleans text before the factory keeps it: execx.Redact (GitHub
// tokens, JWTs, Authorization values, private keys), then AWS keys and
// other token shapes, the values of password=, token: and the like, also under
// a quoted key ("password": "x") (the key stays), emails and host names. clean
// is the scrubbed text; kept is false for a long pasted block (more than 40
// lines or 4000 bytes), which a capture of what the operator typed drops (the
// ledger keeps the factory's own prompts, scrubbed, whatever their length).
func Scrub(text string) (clean string, kept bool) {
	clean = execx.Redact(text)
	clean = scrubTokens.ReplaceAllString(clean, "<redacted>")
	clean = scrubQuotedPairs.ReplaceAllString(clean, "$1$2<redacted>")
	clean = scrubPairs.ReplaceAllString(clean, "$1$2<redacted>")
	clean = scrubEmail.ReplaceAllString(clean, "<email>")
	clean = scrubHost.ReplaceAllString(clean, "<host>")
	kept = len(text) <= maxKeptBytes && strings.Count(strings.TrimRight(text, "\n"), "\n")+1 <= maxKeptLines
	return clean, kept
}
