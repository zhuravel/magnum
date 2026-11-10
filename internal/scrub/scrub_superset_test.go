package scrub

// R58 widened the key-value passes (a key under quotes, an env-style name, four more key words). A wider
// pass can take the place of the older one: a newly covered key with an empty value took the next key's name as
// its value (the separator crosses a line break), and the next key's value showed, where the scrub before R58
// masked it. So Text must mask every byte the scrub before R58 masked. This file holds that scrub (frozen, word
// for word) and the check, on the fuzz's seeds and on a small grammar.

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// frozenPairs is the key-value pattern of the scrub before R58, word for word. Do not change it: it is the oracle.
var frozenPairs = regexp.MustCompile(`(?i)\b((?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret))(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)

// cell is a byte of a text on its way through the passes: orig is its offset in the first text, or -1 for a byte a
// mask wrote.
type cell struct {
	b    byte
	orig int
}

// maskKind says which part of a match a pass replaces.
type maskKind int

const (
	maskWhole maskKind = iota // the whole match
	maskValue                 // the third group, the value of a key-value pair
	maskAuth                  // Redact: all of a match but the name of an Authorization header
)

// trackedPass is a pass of the scrub: its pattern, the part it replaces and the mask it writes.
type trackedPass struct {
	re   *regexp.Regexp
	kind maskKind
	mask string
}

// apply runs the pass over cells, as ReplaceAllString does over a string.
func (p trackedPass) apply(cells []cell) []cell {
	var sb strings.Builder
	for _, c := range cells {
		sb.WriteByte(c.b)
	}
	text := sb.String()
	var out []cell
	pos := 0
	for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
		from, mask := m[0], p.mask
		switch p.kind {
		case maskValue:
			from = m[6]
		case maskAuth:
			if i := strings.IndexByte(text[m[0]:m[1]], ':'); i >= 0 && strings.HasPrefix(strings.ToLower(text[m[0]:m[1]]), "authorization") {
				from, mask = m[0]+i+1, " <redacted>"
			}
		}
		out = append(out, cells[pos:from]...)
		for i := range len(mask) {
			out = append(out, cell{mask[i], -1})
		}
		pos = m[1]
	}
	return append(out, cells[pos:]...)
}

// survivors returns which bytes of text the passes leave.
func survivors(text string, passes []trackedPass) []bool {
	cells := make([]cell, len(text))
	for i := range text {
		cells[i] = cell{text[i], i}
	}
	for _, p := range passes {
		cells = p.apply(cells)
	}
	left := make([]bool, len(text))
	for _, c := range cells {
		if c.orig >= 0 {
			left[c.orig] = true
		}
	}
	return left
}

// scrubBeforeR58 are the passes of the scrub before R58, in its order.
func scrubBeforeR58(redact *regexp.Regexp) []trackedPass {
	return []trackedPass{
		{redact, maskAuth, "<redacted>"},
		{scrubTokens, maskWhole, "<redacted>"},
		{frozenPairs, maskValue, "<redacted>"},
		{scrubEmail, maskWhole, "<email>"},
		{scrubHost, maskWhole, "<host>"},
	}
}

// tracked turns the replay's passes into passes that follow each byte.
func tracked(replay []scrubPass) []trackedPass {
	var passes []trackedPass
	for _, p := range replay {
		switch {
		case p.with == "":
			passes = append(passes, trackedPass{p.re, maskAuth, "<redacted>"})
		case strings.HasPrefix(p.with, "$1$2"):
			passes = append(passes, trackedPass{p.re, maskValue, strings.TrimPrefix(p.with, "$1$2")})
		default:
			passes = append(passes, trackedPass{p.re, maskWhole, p.with})
		}
	}
	return passes
}

// scrubNow are the passes of Text, taken from the replay the fuzz holds to it.
func scrubNow(redact *regexp.Regexp) []trackedPass {
	return tracked(scrubPasses(redact))
}

// shown renders text with each run of masked bytes as a block, to read a failure.
func shown(text string, left []bool) string {
	var sb strings.Builder
	masked := false
	for i := range text {
		if left[i] {
			masked = false
			sb.WriteByte(text[i])
		} else if !masked {
			sb.WriteString("█")
			masked = true
		}
	}
	return sb.String()
}

// notMaskedNow checks that Text leaves no byte of text the scrub before R58 masked. The passes it follows are the
// replay's (scrubPasses), so it first checks that the replay is Text.
func notMaskedNow(t *testing.T, redact *regexp.Regexp, text string) {
	t.Helper()
	scrubbed := Text(text)
	if _, replayed := scrubReplay(redact, text); replayed != scrubbed {
		t.Fatalf("the replay of Text's passes parted from Text on %q", brief(text))
	}
	before, now := survivors(text, scrubBeforeR58(redact)), survivors(text, scrubNow(redact))
	for i := range text {
		if now[i] && !before[i] {
			t.Fatalf("Text shows a byte (offset %d) that the scrub before R58 masked:\ntext:   %q\nbefore: %q\nnow:    %q",
				i, brief(text), brief(shown(text, before)), brief(shown(text, now)))
		}
	}
}

func TestScrubMasksEveryByteTheScrubBeforeR58Masked(t *testing.T) {
	redact := regexp.MustCompile(strings.Join(execxSecretPatterns(t), "|"))
	for _, seed := range scrubSeeds {
		notMaskedNow(t, redact, scrubText(seed.head, seed.fill, seed.n, seed.tail))
	}
	for _, text := range grammarTexts() {
		notMaskedNow(t, redact, text)
	}
}

// grammarTexts are 12,000 texts of a grammar of the pieces of key-value pairs: keys the scrub before R58 knew and keys
// it did not, quotes, separators, values and the white space between them, and an email and a host. The same texts
// every run.
func grammarTexts() []string {
	frags := []string{`"`, `'`, `\`, `password`, `token`, `api_key`, `client_secret`, `SESSION_SECRET`, `DB_PASSWORD`, `GITHUB_TOKEN`,
		`secret_key_base`, `signing_key`, `master_key`, "_", "-", ".", " ", "\n", ":", "=", ",", "}", "]", "{", "[", "x", "MADEUP",
		"é", "S", "PWD", "0", "a b", "Bearer ", "export ", "a@b.co", "db.example.com"}
	r := rand.New(rand.NewSource(58))
	texts := make([]string, 12000)
	for i := range texts {
		var sb strings.Builder
		for range 3 + r.Intn(14) {
			sb.WriteString(frags[r.Intn(len(frags))])
		}
		texts[i] = sb.String()
	}
	return texts
}

// The inputs where a key the scrub before R58 did not know, with no value, took the name of the next key for its value
// and left the next value in the clear. The next key's name is masked as the first key's value now, and its own value
// is masked by the first key-value pass, which runs on the text the scrub before R58 saw.
func TestAKeyWithNoValueLeavesTheNextKeysValueMasked(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a compose entry with no value, then a known key", "SESSION_SECRET:\napi_key: hunter2", "SESSION_SECRET:\n<redacted> <redacted>"},
		{"an env line with no value, then a known key", "DB_PASSWORD=\npassword: hunter2", "DB_PASSWORD=\n<redacted> <redacted>"},
		{"a YAML key with no value, then a known key", "signing_key:\n  password: hunter2", "signing_key:\n  <redacted> <redacted>"},
		{"a quoted key with no value, then a known key", "\"DB_PASSWORD\":\n  client_secret: hunter2", "\"DB_PASSWORD\":\n  <redacted> <redacted>"},
		{"a value up to a comma, then a known key", "GITHUB_TOKEN=x,password: hunter2", "GITHUB_TOKEN=<redacted> <redacted>"},
		{"a known key after a quoted key", `"secret": password: hunter2`, `"secret": <redacted> <redacted>`},
	} {
		if clean := Text(c.in); clean != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, clean, c.want)
		}
	}
}
