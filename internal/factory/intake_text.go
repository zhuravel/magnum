package factory

// The words of intake: what a start's text holds (ParseStart), the kind of
// task its words pick (PickKind), a branch's slug, a prompt's title, and the
// words a free prompt shares with a Jira title.

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/textx"
)

var (
	// startKey is a Jira key at the start of a start's text: upper case, so
	// a prompt that opens with "covid-19" stays a prompt.
	startKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
	// anyKey is the shape of a key anywhere in a text (foundKeys looks at
	// what touches it), textLink a link in a text: it ends at white space, a
	// quote or an angle bracket.
	anyKey   = regexp.MustCompile(`[A-Z][A-Z0-9_]*-[0-9]+`)
	textLink = regexp.MustCompile("https?://[^\\s<>\"'`]+")
	// browsePath is a Jira site's link to an issue.
	browsePath = regexp.MustCompile(`^/browse/([A-Z][A-Z0-9_]*-[0-9]+)/?$`)
	// prPath and runPath are a GitHub PR's and an Actions run's links.
	prPath  = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)(?:/.*)?$`)
	runPath = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/actions/runs/([0-9]+)(?:/.*)?$`)
)

// ParseStart reads a start's text. trackers are the Jira sites and the
// tracker templates (config.ParseTracker) that tell a Jira link, separated
// by white space: a site's URL ("https://example.atlassian.net") or a
// template ("https://example.atlassian.net/browse/PR-{num}"). The text is
// trimmed; its first word is:
//   - a Jira key in upper case ("PR-12345"), or a Jira link: /browse/<KEY>
//     on the host of one of trackers (on any host when trackers is empty),
//     or a template's link; the words after it are the guidance (a colon or
//     a dash between the two is dropped);
//   - a GitHub PR link (PR) or an Actions run link (Run), the words after
//     it the guidance;
//   - anything else: the whole text is the prompt, and Found are the keys
//     it names elsewhere (foundKeys). ParseStart reads no Jira: a word of
//     the shape of a key ("UTF-8") is found, and the start asks Jira which
//     of them count.
func ParseStart(text string, trackers string) StartInput {
	text = strings.TrimSpace(text)
	first, rest := text, ""
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		first, rest = text[:i], text[i:]
	}
	guidance := strings.TrimSpace(strings.TrimLeft(rest, " \t\r\n:,;-–—"))
	word := strings.TrimRight(first, ":,;")
	if startKey.MatchString(word) {
		return StartInput{Key: word, Guidance: guidance}
	}
	prompt := StartInput{Prompt: text, Found: foundKeys(text, trackers)}
	u, err := url.Parse(word)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return prompt
	}
	if isGitHub(u.Hostname()) {
		in := StartInput{Guidance: guidance}
		if m := prPath.FindStringSubmatch(u.Path); m != nil {
			in.PR = &Link{URL: word, Repo: m[1] + "/" + m[2], Number: parseNumber(m[3])}
			return in
		}
		if m := runPath.FindStringSubmatch(u.Path); m != nil {
			in.Run = &Link{URL: word, Repo: m[1] + "/" + m[2], Number: parseNumber(m[3])}
			return in
		}
		return prompt
	}
	if key := jiraLinkKey(u, word, trackers); key != "" {
		return StartInput{Key: key, Guidance: guidance}
	}
	return prompt
}

// isGitHub reports whether host is GitHub's: its links are PRs and runs,
// never Jira's.
func isGitHub(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "www.github.com"
}

// foundKeys are the distinct Jira keys of a prompt, in the order of the text:
// the keys of its Jira links (as the first word's rule reads one) and its
// bare upper-case keys. A key counts as bare when no letter, digit,
// underscore or dash touches it ("PR-12345's" and "(PR-12345)" are keys;
// "xPR-12345", "PR-12345x" and "ABC-DEF-12" are not), and a link is read as
// a link only, so the key in a link of another site is nobody's. A GitHub
// link stays words (the first word's rule handles it).
func foundKeys(text, trackers string) []string {
	type hit struct {
		at  int
		key string
	}
	var hits []hit
	bare := []byte(text) // the text with its links blanked out
	for _, loc := range textLink.FindAllStringIndex(text, -1) {
		link := strings.TrimRight(text[loc[0]:loc[1]], ".,;:!?)]}")
		if u, err := url.Parse(link); err == nil && u.Host != "" && !isGitHub(u.Hostname()) {
			if key := jiraLinkKey(u, link, trackers); key != "" {
				hits = append(hits, hit{loc[0], key})
			}
		}
		for i := loc[0]; i < loc[1]; i++ {
			bare[i] = ' '
		}
	}
	for _, loc := range anyKey.FindAllIndex(bare, -1) {
		before, _ := utf8.DecodeLastRune(bare[:loc[0]])
		after, _ := utf8.DecodeRune(bare[loc[1]:])
		if !touchesKey(before) && !touchesKey(after) {
			hits = append(hits, hit{loc[0], string(bare[loc[0]:loc[1]])})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return a.at - b.at })
	var keys []string
	for _, h := range hits {
		if !slices.Contains(keys, h.key) {
			keys = append(keys, h.key)
		}
	}
	return keys
}

// touchesKey reports whether r, next to a key's shape, makes it part of a
// longer word.
func touchesKey(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// jiraLinkKey is the key of a Jira link (ParseStart), "" for a link that is
// none.
func jiraLinkKey(u *url.URL, link, trackers string) string {
	hosts := map[string]bool{}
	var templates []config.Tracker
	for f := range strings.FieldsSeq(trackers) {
		if t, err := config.ParseTracker(f); err == nil {
			templates = append(templates, t)
		}
		if tu, err := url.Parse(f); err == nil && tu.Host != "" {
			hosts[strings.ToLower(tu.Hostname())] = true
		}
	}
	bare, _, _ := strings.Cut(link, "?")
	bare, _, _ = strings.Cut(bare, "#")
	for _, t := range templates {
		if key, at := config.Issue(bare, []config.Tracker{t}); key != "" && at == bare {
			return key
		}
	}
	if m := browsePath.FindStringSubmatch(u.Path); m != nil && (len(hosts) == 0 || hosts[strings.ToLower(u.Hostname())]) {
		return m[1]
	}
	return ""
}

// parseNumber is a link's number (digits the pattern matched; one past
// int64 is 0).
func parseNumber(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// PickKind picks the kind of a task: the kind kind_by_jira_type maps the
// ticket's issue type to (the type's case ignored), else the kind whose
// kind_words the ticket's summary and prompt use most (a word's own forms,
// "crashes" for "crash", count; a tie goes to the kind used first), else
// bug. Only a configured kind is picked. why says how, in the words the
// new-task screen shows.
func PickKind(cfg config.Factory, t *jira.Ticket, prompt string) (kind, why string) {
	if t != nil && t.Type != "" {
		for _, jt := range slices.Sorted(maps.Keys(cfg.KindByJiraType)) {
			if k := cfg.KindByJiraType[jt]; strings.EqualFold(jt, t.Type) && hasKind(cfg, k) {
				return k, "from the Jira type " + t.Type
			}
		}
	}
	text := prompt
	if t != nil {
		text = t.Summary + " " + prompt
	}
	tokens := kindTokens(text)
	best, bestN, bestAt, bestWord := "", 0, 0, ""
	for _, k := range slices.Sorted(maps.Keys(cfg.KindWords)) {
		if !hasKind(cfg, k) {
			continue
		}
		n, at, word := 0, -1, ""
		for _, w := range cfg.KindWords[k] {
			i := slices.IndexFunc(tokens, func(tok string) bool { return formOf(tok, strings.ToLower(w)) })
			if i < 0 {
				continue
			}
			n++
			if at < 0 || i < at {
				at, word = i, w
			}
		}
		if n > bestN || (n == bestN && n > 0 && at < bestAt) {
			best, bestN, bestAt, bestWord = k, n, at, word
		}
	}
	if best != "" {
		return best, fmt.Sprintf("guessed from %q", bestWord)
	}
	return defaultKind(cfg), "no hint; tab changes it"
}

// hasKind reports whether k is a configured kind of task.
func hasKind(cfg config.Factory, k string) bool {
	_, ok := cfg.Kinds[k]
	return ok
}

// defaultKind is the kind of a task nothing hints at: bug, or the first
// configured kind when there is no bug kind.
func defaultKind(cfg config.Factory) string {
	if hasKind(cfg, "bug") || len(cfg.Kinds) == 0 {
		return "bug"
	}
	return slices.Sorted(maps.Keys(cfg.Kinds))[0]
}

// kindTokens are the words of text, lowercased, an apostrophe inside a
// word dropped.
func kindTokens(text string) []string {
	text = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(text))
	return strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// formOf reports whether token is word or one of its forms: word with s,
// es, ed, d or ing after it (an e at word's end dropped before ed and ing).
func formOf(token, word string) bool {
	if token == word {
		return true
	}
	stem := strings.TrimSuffix(word, "e")
	for _, suffix := range []string{"s", "es", "ed", "d", "ing"} {
		if base, ok := strings.CutSuffix(token, suffix); ok && (base == word || base == stem) {
			return true
		}
	}
	return false
}

// slugStop are the words a branch's slug leaves out.
var slugStop = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"for": true, "and": true, "or": true, "is": true, "are": true, "be": true, "it": true, "its": true,
	"with": true, "from": true, "that": true, "this": true, "as": true,
}

// slug is the branch's words of text: the first line's words, lowercased,
// ASCII letters and digits only (an apostrophe dropped, anything else a
// break), common words left out, joined with dashes, at most maxSlug
// characters (whole words; a first word longer than that is cut).
func slug(text string) string {
	line := strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(textx.FirstLine(text)))
	var b strings.Builder
	for _, w := range strings.FieldsFunc(line, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') }) {
		if slugStop[w] {
			continue
		}
		if len(w) > maxSlug {
			w = w[:maxSlug]
		}
		if b.Len() > 0 && b.Len()+1+len(w) > maxSlug {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteString(w)
	}
	return b.String()
}

// promptTitle is a prompt task's title: its first line, at most maxTitle
// runes.
func promptTitle(prompt string) string { return textx.Clip(textx.FirstLine(prompt), maxTitle) }

// sharedWords are the words of words whose stem a title's words have.
func sharedWords(words []string, title string) []string {
	have := map[string]bool{}
	for _, w := range kindTokens(title) {
		have[stem(w)] = true
	}
	var out []string
	for _, w := range words {
		if have[stem(w)] {
			out = append(out, w)
		}
	}
	return out
}

// stem is a word without a plural's s ("weeks" and "week" match).
func stem(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

func quoteAll(ws []string) string {
	q := make([]string, len(ws))
	for i, w := range ws {
		q[i] = fmt.Sprintf("%q", w)
	}
	return strings.Join(q, ", ")
}
