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
// the next value would then show where the first pass masked it. The passes R60 adds run after them, for what they
// cannot see, in this order: scrubURLPassword, scrubURLUser, scrubCurlUser, scrubMySQLPassword, scrubJoinedEnv (by
// maskJoinedEnv). The last runs last for the same reason: a name with no value takes the first word of the next
// line for its value, and that word must already be masked text when a command or a URL starts it.

// scrubKeyWords are the key names whose value is a secret, for the two newer key-value patterns. scrubPairs keeps
// the names it had.
const scrubKeyWords = `(?:aws_)?(?:secret(?:_access)?_key(?:_base)?|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|master[_-]?key|signing[_-]?key|encryption[_-]?key|client[_-]?secret|authorization)`

// scrubQuotedValue is the value of a quoted key or an env-style name that starts with a quote: a double-quoted or
// single-quoted string, in which a backslash escapes the next character (`"a\"b"`), over lines too. It is
// written unrolled: the form (?:[^"\\]|\\.)* backtracks badly in a backtracking engine (scrub.py's) on a string that
// never ends, and this one does not.
const scrubQuotedValue = `"[^"\\]*(?:\\[\s\S][^"\\]*)*"|'[^'\\]*(?:\\[\s\S][^'\\]*)*'`

// scrubCommandSpan is what lies between a command's name and an option of it on one line: it stops at a line's end
// (a backslash continues the line), at a pipe, at a semicolon and at &&, so that an option of the next command on
// the line is not read as this command's. A lone & stays inside (a quoted URL's query: "x?a=1&b=2" -u user:pw).
const scrubCommandSpan = `(?:[^\n|;&\\]|&[^&\n]|\\[\s\S])*?`

var (
	// scrubTokens are token shapes execx.Redact leaves: AWS access keys,
	// Slack, OpenAI and Anthropic, GitLab, Stripe and Google API keys, and
	// npm tokens (npm_ and 36 letters or digits, or more).
	scrubTokens = regexp.MustCompile(strings.Join([]string{
		`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`,
		`\bxox[abposr]-[A-Za-z0-9-]{10,}`,
		`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`,
		`\bglpat-[A-Za-z0-9_-]{16,}`,
		`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`,
		`\bAIza[0-9A-Za-z_-]{35}\b`,
		`\bnpm_[A-Za-z0-9]{36,}`,
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
	// scrubURLPassword is the password in a URL's userinfo, scheme://user:password@host (R60). The scheme, the user
	// before the colon and the host stay; the user may be empty (redis://:pw@host). The password runs to the last @
	// before the first slash, question mark, number sign, white space or quote, so a password that holds an @ goes
	// whole. Only :// is read, no scheme: a host and a port (http://host:8080/x) have no @ before the slash, and an
	// email address has no ://.
	scrubURLPassword = regexp.MustCompile(`(://[^\s/?#@:"']*:)[^\s/?#"']+@`)
	// scrubURLUser is the user of an http or https URL with no password, https://key@host (R60): a Sentry DSN's
	// public key, or a token used as the user name. It is read as a secret, since the URL carries no other. The cost
	// is a user name in a clone URL (https://alice@host/x). The user of another scheme stays (ssh://git@host), and so
	// do an email address (no ://) and a URL with no @ in its authority. The user is made of the characters of a key,
	// so that no punctuation of the text around a URL is taken for it.
	scrubURLUser = regexp.MustCompile(`(https?://)[A-Za-z0-9._~%+=-]+@`)
	// scrubCurlUser is the password of curl's -u user:password and --user user:password, anywhere after curl on its
	// line (R60); the user and the colon stay. The password is a word up to white space or a quote. A -u or --user of
	// another command (docker run -u 1000:1000) is read only when curl comes first on the same command.
	scrubCurlUser = regexp.MustCompile(`(\bcurl\b` + scrubCommandSpan + `[ \t](?:-u[ \t]*|--user[ \t]+))(["']?[^\s:"']*:)([^\s"']+)`)
	// scrubMySQLPassword is the password of mysql and mysqldump's -p<password>, --password=<password> and
	// --password <password> (R60); the option stays. The short option takes its value with no space after it (with a
	// space, mysql asks for the password and the word is the database), and only these two commands are read, so
	// that -p of ssh, docker or psql (a port) stays. The value is a word up to white space or a quote, or a quoted
	// string.
	scrubMySQLPassword = regexp.MustCompile(`(\bmysql(?:dump)?\b` + scrubCommandSpan + `[ \t])(--password=|--password[ \t]+|-p)(` + scrubQuotedValue + `|[^\s"']+)`)
	// scrubJoinedEnv is an all-caps env name that ends in a key word, with no word boundary or underscore needed
	// before the word: PGPASSWORD=, REDISPASS=, MYSQL_PWD=, GITHUBTOKEN: (R60). The word must end the name and the
	// name must start with a capital after a boundary, so a lower-case word (bypass=, compass:) and a name that
	// goes on (PGPASSWORD_FILE) stay; at least one character comes before the key word, so that the verdict of a
	// test (--- PASS: TestX) is no name. The key and the separator stay; the value is read as scrubEnvPairs reads
	// it. A quoted key ("PGPASSWORD": "x") is not read. maskJoinedEnv applies it, and not for the names
	// scrubJoinedKept lists.
	scrubJoinedEnv = regexp.MustCompile(`(\b[A-Z][A-Z0-9_]*(?:PASSWORD|PASSWD|PASS|PWD|TOKEN))(\s*[=:]\s*)(` + scrubQuotedValue + `|\S+)`)
	// scrubJoinedKept are the names scrubJoinedEnv's pattern takes and that are no secret's: the English words that
	// end in PASS (BYPASS= is a flag, COMPASS= a direction) and the shell's OLDPWD. A name that ends in one of them
	// is kept, whatever comes before it (SKIP_BYPASS=).
	scrubJoinedKept = regexp.MustCompile(`(?:BYPASS|COMPASS|IMPASS|OVERPASS|SURPASS|TRESPASS|UNDERPASS|OLDPWD)$`)
	scrubEmail      = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// scrubUserinfoHost is, for Text, the host after a userinfo the URL passes masked (R60). Before them the email
	// pass masked user@host.tld whole, a host the host pass does not list (https://key@host.co/x) with it; the
	// <redacted> they leave is no part of an address, so this pass takes the domain the email pass would have taken.
	scrubUserinfoHost = regexp.MustCompile(`(<redacted>@)[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// scrubHost is a dotted host name under a top-level domain a file name
	// does not end in, and an IPv4 address.
	scrubHost = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|dev|app|ai|cloud|internal|local|lan|corp|intra|info|biz|tech|xyz|site|us|uk|de|ua|eu|me|cc|tv|gg)\b|\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// Secrets masks the secrets in text and leaves emails and host names: execx.Redact (GitHub tokens, JWTs,
// Authorization values, private keys), then AWS keys and other token shapes, then the values of password=, token: and
// the like, also under a quoted key ("password": "x") and an env-style name (DB_PASSWORD=x), then (R60) the password
// of a URL (postgres://app:x@host), the user of an http URL (a Sentry DSN's key), the password of curl -u and
// mysql -p, and the value of an all-caps name that ends in a key word (PGPASSWORD=x). The key stays. It is for text
// where a host or an address is what the reader needs, such as a failing test's output in a CI log.
func Secrets(text string) string {
	text = execx.Redact(text)
	text = scrubTokens.ReplaceAllString(text, "<redacted>")
	text = scrubPairs.ReplaceAllString(text, "$1$2<redacted>")
	text = scrubQuotedPairs.ReplaceAllString(text, "$1$2<redacted>")
	text = scrubEnvPairs.ReplaceAllString(text, "$1$2<redacted>")
	text = scrubURLPassword.ReplaceAllString(text, "$1<redacted>@")
	text = scrubURLUser.ReplaceAllString(text, "$1<redacted>@")
	text = scrubCurlUser.ReplaceAllString(text, "$1$2<redacted>")
	text = scrubMySQLPassword.ReplaceAllString(text, "$1$2<redacted>")
	text = maskJoinedEnv(text)
	return text
}

// maskJoinedEnv masks the value of every scrubJoinedEnv name that is no scrubJoinedKept name. A kept name is passed
// over without its value: the search goes on from after its separator, since a name with no value takes the next
// line's first word for its value, and that word may be the name of a secret (BYPASS:\nPGPASSWORD=x). The search
// restarts after a separator or a masked value, where no word character comes before the next name, so slicing the
// text there changes no \b.
func maskJoinedEnv(text string) string {
	var out strings.Builder
	for {
		m := scrubJoinedEnv.FindStringSubmatchIndex(text)
		if m == nil {
			out.WriteString(text)
			return out.String()
		}
		if scrubJoinedKept.MatchString(text[m[2]:m[3]]) {
			out.WriteString(text[:m[5]])
			text = text[m[5]:]
			continue
		}
		out.WriteString(text[:m[6]])
		out.WriteString("<redacted>")
		text = text[m[1]:]
	}
}

// Text masks what Secrets masks, then emails and host names (and the host after a masked userinfo, which the email
// pass cannot read). It is what the factory keeps of the words that pass through it (runtime.Scrub).
func Text(text string) string {
	text = Secrets(text)
	text = scrubEmail.ReplaceAllString(text, "<email>")
	text = scrubUserinfoHost.ReplaceAllString(text, "$1<host>")
	text = scrubHost.ReplaceAllString(text, "<host>")
	return text
}
