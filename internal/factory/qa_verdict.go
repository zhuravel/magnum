package factory

// The QA walker's verdict and the factory's own reading of the pages it
// saved (R55 slice 1). The walker walks the changed pages in a headless
// browser and writes verdict.json in its run's folder (qa/<n>-<sha7>):
// the pages with their shot, the probe's JSON and the console's log, its
// defects, what it could not reach, and the acceptance checks no page
// shows. The factory refuses a verdict whose files it cannot trust
// (readQAVerdict), reads each page's probe itself (detectQA) and decides
// the run's outcome from what it found and the walker's defects together
// (qaOutcome): a "looks fine" of the walker's proves nothing.

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/textx"
)

// QAVerdictVersion is the version of the QA verdict this build reads; a
// verdict of any other version is refused.
const QAVerdictVersion = 1

// QAMaxShots is the most screenshots one QA run may leave in its folder.
const QAMaxShots = 30

// The classes of a QA defect. The factory finds the first three, the
// console's errors and a page that scrolls sideways itself; flow is the
// walker's alone (a step of a check that does not do what it says).
const (
	QAEscapedMarkup = "escaped_markup"
	QARawI18nKey    = "raw_i18n_key"
	QAErrorPage     = "error_page"
	QABrokenLayout  = "broken_layout"
	QAConsoleError  = "console_error"
	QAFlow          = "flow"
)

// The outcomes of a QA run: clean, defects (one blocks), unavailable (no
// page to judge), not_needed (no user-facing file changed), pressure (the
// Mac's memory stopped it) and interrupted (the daemon stopped it).
const (
	QAOutcomeClean       = "clean"
	QAOutcomeDefects     = "defects"
	QAOutcomeUnavailable = "unavailable"
	QAOutcomeNotNeeded   = "not_needed"
	QAOutcomePressure    = "pressure"
	QAOutcomeInterrupted = "interrupted"
)

// QAVerdict is verdict.json, the walker's verdict on one QA run: its
// version (QAVerdictVersion), the copy's head it walked, the pages it
// captured, its defects, the states it could not reach (Blocked) and the
// acceptance checks no page shows (NotShown). Base, URL and Walker are the
// factory's to fill.
type QAVerdict struct {
	Version  int          `json:"version"`
	Head     string       `json:"head"`
	Base     string       `json:"base"`
	URL      string       `json:"url"`
	Walker   string       `json:"walker"`
	Pages    []QAPage     `json:"pages"`
	Defects  []QADefect   `json:"defects"`
	Blocked  []QABlocked  `json:"blocked"`
	NotShown []QANotShown `json:"not_shown"`
}

// QAPage is one page the walker captured: its id (P1), the path it loaded,
// the state it reached, its side (after), its shot, probe and console
// files relative to the run's folder, its HTTP status as the walker saw it,
// and the acceptance checks it shows.
type QAPage struct {
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	State   string   `json:"state"`
	Side    string   `json:"side"`
	Shot    string   `json:"shot"`
	Probe   string   `json:"probe"`
	Console string   `json:"console"`
	Status  int      `json:"status"`
	Checks  []string `json:"checks"`
}

// QADefect is one defect: its id, class, severity (P1 to P3) and page id,
// a title, the quote as the page shows it, the code paths suspected, who
// found it (Source: walker or factory), whether the walker thinks it was
// there before the change (read, never trusted in slice 1), and the
// fingerprint the factory follows it by (qaFingerprint).
type QADefect struct {
	ID          string   `json:"id"`
	Class       string   `json:"class"`
	Severity    string   `json:"severity"`
	Page        string   `json:"page"`
	Title       string   `json:"title"`
	Quote       string   `json:"quote"`
	Source      string   `json:"source"`
	Paths       []string `json:"paths"`
	PreExisting bool     `json:"pre_existing"`
	Fingerprint string   `json:"fingerprint"`
}

// QABlocked is a state or page the walker could not reach, why, and the
// acceptance check it was for.
type QABlocked struct {
	What  string `json:"what"`
	Why   string `json:"why"`
	Check string `json:"check"`
}

// QANotShown is an acceptance check no page shows (a background job, an
// email), and why.
type QANotShown struct {
	Check string `json:"check"`
	Why   string `json:"why"`
}

// QAProbe is what the factory's probe (qa_probe.js) returned in one page:
// the run's nonce, the page's URL, title, visible text and HTML, its HTTP
// status (0 when unknown), whether it scrolls sideways, whether the text or
// the HTML was cut at its cap, and the error that broke the probe in the
// page ("" when none did).
type QAProbe struct {
	Nonce     string `json:"nonce"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	HTML      string `json:"html"`
	Status    int    `json:"status"`
	OverflowX bool   `json:"overflow_x"`
	Truncated bool   `json:"truncated"`
	Error     string `json:"error"`
}

// qaWant is what a verdict is checked against: the copy's head, the run's
// probe nonce, the copy's port, the most pages, and the acceptance checks'
// ids, each of which a page, a blocked step or a not_shown entry covers.
type qaWant struct {
	Head, Nonce    string
	Port, MaxPages int
	Checks         []string
}

// Who found a defect.
const (
	qaSourceWalker  = "walker"
	qaSourceFactory = "factory"
)

// The caps of a probe's text and HTML, in bytes, as qa_probe.js has them;
// a probe's file larger than qaProbeFileCap is refused.
const (
	qaTextCap      = 200_000
	qaHTMLCap      = 400_000
	qaProbeFileCap = 8 << 20
	qaQuoteRunes   = 120
	qaQuoteLead    = 24
)

// qaClasses are the classes a verdict may name; qaBlockingClasses block
// whatever their severity and whoever found them.
var (
	qaClasses         = []string{QAEscapedMarkup, QARawI18nKey, QAErrorPage, QABrokenLayout, QAConsoleError, QAFlow}
	qaBlockingClasses = []string{QAEscapedMarkup, QARawI18nKey, QAErrorPage}
	qaSeverities      = []string{"P1", "P2", "P3"}
)

// qaPageClasses are the classes the factory's detectors judge a whole page
// by: a walker's defect of one of them on a page where the factory found
// the same class is the factory's defect, counted once.
var qaPageClasses = []string{QAEscapedMarkup, QARawI18nKey, QAErrorPage, QAConsoleError}

//go:embed qa_probe.js
var qaProbeJS []byte

// qaProbeScript is the probe the walker evaluates in each page: the
// embedded qa_probe.js with this run's nonce in place of its placeholder.
func qaProbeScript(nonce string) []byte {
	q, err := json.Marshal(nonce)
	if err != nil {
		q = []byte(`""`)
	}
	return bytes.Replace(qaProbeJS, []byte(`"__MAGNUM_NONCE__"`), q, 1)
}

// htmlTags are the names of the elements whose tag, shown as text, the
// escaped_markup detector finds; qaCustomTag adds every custom element's
// (a name with a hyphen: turbo-frame, sl-tooltip).
var htmlTags = []string{"a", "abbr", "b", "br", "button", "code", "dd", "del", "details", "div", "dl", "dt", "em", "footer",
	"form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hr", "i", "img", "input", "ins", "label", "li", "mark", "nav", "ol",
	"option", "p", "pre", "s", "script", "section", "select", "small", "span", "strong", "style", "sub", "summary", "sup", "svg",
	"table", "tbody", "td", "textarea", "th", "thead", "time", "tr", "turbo-frame", "u", "ul"}

// qaCustomTag is the name of a custom element: words joined by hyphens.
const qaCustomTag = `[a-z][a-z0-9]*(?:-[a-z0-9]+)+`

// errorPageSigns are the words of the frameworks' error pages, found in a
// page's title or the text it shows, never its HTML, where a normal page
// keeps them hidden (a toast, a data attribute); errorPageTitleSigns only
// in its title, as a log page shows them as text.
var (
	errorPageSigns = []string{
		"Action Controller: Exception caught",
		"We're sorry, but something went wrong",
		"Traceback (most recent call last)",
		"Application error: a client-side exception has occurred",
		"Unhandled Runtime Error",
		"ActionView::Template::Error",
	}
	errorPageTitleSigns = []string{"Internal Server Error"}
)

// The detectors' patterns. A tag shown as text is a closing tag, an
// opening tag with an attribute and its value, or a line break: a bare
// opening tag alone (<time>, <label>, <b>) reads as a placeholder in
// prose, and its closing tag finds the markup it opens.
var (
	qaTagPattern = func() *regexp.Regexp {
		names := strings.Join(htmlTags, "|") + "|" + qaCustomTag
		return regexp.MustCompile(`(?i)</(?:` + names + `)\s*>` +
			`|<(?:` + names + `)\s+[a-z_:@][-\w:.@]*\s*=\s*(?:["']|[^\s"'<>=` + "`" + `]+[\s/>])` +
			`|<br\s*/?>`)
	}()
	qaEntityPattern  = regexp.MustCompile(`&(?:[A-Za-z][A-Za-z0-9]{1,31}|#\d{1,7}|#[xX][0-9A-Fa-f]{1,6});`)
	qaI18nText       = regexp.MustCompile(`(?i)translation missing|\[missing "[^"\n]{1,200}" translation\]`)
	qaI18nHTML       = regexp.MustCompile(`class\s*=\s*["'][^"']*\btranslation_missing\b`)
	errQAOutside     = errors.New("is outside the run folder")
	errQAProbeFormat = errors.New("is not the JSON the probe returns")
)

// detectQA reads one page's probe and console lines and returns what the
// factory finds in them, at most one defect of each class, on page:
//   - escaped_markup (P2): markup or an entity in the text the page shows;
//     never in its HTML, where attributes hold escaped markup legally;
//   - raw_i18n_key (P2): "translation missing" or i18n-js's
//     `[missing "…" translation]` in the text, or an element of class
//     translation_missing in the HTML;
//   - error_page (P2): a status of 500 or more, or 404, or the words of an
//     error page in the title or the text (errorPageSigns);
//   - console_error (P3): console lines that start with [error];
//   - broken_layout (P3): a page that scrolls sideways.
//
// A defect's title carries no page text; its quote is at most 120 runes
// around the find, secrets masked.
func detectQA(page QAPage, p QAProbe, console []string) []QADefect {
	var ds []QADefect
	add := func(class, severity, title, quote string) {
		d := QADefect{ID: page.ID + ":" + class, Class: class, Severity: severity, Page: page.ID, Title: title,
			Quote: quote, Source: qaSourceFactory}
		d.Fingerprint = qaFingerprint(d, page.Path)
		ds = append(ds, d)
	}
	if locs := escapedMarkup(p.Text); len(locs) > 0 {
		add(QAEscapedMarkup, "P2", "HTML markup shown as text"+qaFinds(len(locs)), qaQuote(p.Text, locs[0]))
	}
	if loc := qaI18nText.FindStringIndex(p.Text); loc != nil {
		add(QARawI18nKey, "P2", "a missing translation shown as text", qaQuote(p.Text, loc))
	} else if loc := qaI18nHTML.FindStringIndex(p.HTML); loc != nil {
		add(QARawI18nKey, "P2", "a missing translation shown by its key", qaQuote(p.HTML, loc))
	}
	if why := errorPage(p); why != "" {
		add(QAErrorPage, "P2", "an error page ("+why+")", "")
	}
	var errs []string
	for _, line := range console {
		if l := strings.TrimSpace(line); strings.HasPrefix(strings.ToLower(l), "[error]") {
			errs = append(errs, l)
		}
	}
	if len(errs) > 0 {
		add(QAConsoleError, "P3", plural(len(errs), "console error"), qaClip(errs[0]))
	}
	if p.OverflowX {
		add(QABrokenLayout, "P3", "the page scrolls sideways", "")
	}
	return ds
}

// escapedMarkup is where text shows a tag or an entity, in order: an
// entity is one HTML decodes whole (&hellip;, &#39;), so "AT&T;" or
// "&notit;" (&not and "it;") is none.
func escapedMarkup(text string) [][]int {
	ents := slices.DeleteFunc(qaEntityPattern.FindAllStringIndex(text, -1), func(loc []int) bool {
		m := text[loc[0]:loc[1]]
		u := html.UnescapeString(m)
		return u == m || strings.HasSuffix(u, ";")
	})
	locs := slices.Concat(qaTagPattern.FindAllStringIndex(text, -1), ents)
	slices.SortFunc(locs, func(a, b []int) int { return a[0] - b[0] })
	return locs
}

// qaFinds is " (n tags or entities)" for more than one find, else "".
func qaFinds(n int) string {
	if n < 2 {
		return ""
	}
	return " (" + strconv.Itoa(n) + " tags or entities)"
}

// errorPage is why p is an error page ("status 500", the sign it shows),
// "" when it is none.
func errorPage(p QAProbe) string {
	if p.Status >= 500 || p.Status == 404 {
		return "status " + strconv.Itoa(p.Status)
	}
	title, text := signText(p.Title), signText(p.Text)
	for _, sign := range errorPageSigns {
		if strings.Contains(title, sign) || strings.Contains(text, sign) {
			return sign
		}
	}
	for _, sign := range errorPageTitleSigns {
		if strings.Contains(title, sign) {
			return sign
		}
	}
	return ""
}

// signText is s as errorPage matches signs in it: its whitespace runs one
// space, its typographic apostrophes straight.
func signText(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "’", "'")
}

// qaQuote is the quote of the find at loc (byte offsets) in s: up to 24
// runes before it ("…" first when that cuts the text) and the rest of 120
// runes from it, on one line. Each side is masked (execx.Redact) on a wider
// window before it is cut, so a token at the quote's edge is masked whole,
// never shown in part.
func qaQuote(s string, loc []int) string {
	lead := []rune(oneLine(execx.Redact(strings.ToValidUTF8(s[max(0, loc[0]-2048):loc[0]], ""))))
	if len(lead) > qaQuoteLead {
		lead = append([]rune("…"), lead[len(lead)-qaQuoteLead+1:]...)
	}
	after := oneLine(execx.Redact(strings.ToValidUTF8(s[loc[0]:min(len(s), loc[1]+4096)], "")))
	if len(lead) > 0 && strings.IndexByte(" \t\n\r\f\v", s[loc[0]-1]) >= 0 {
		after = " " + after
	}
	return textx.Clip(string(lead)+after, qaQuoteRunes)
}

// qaClip is a quote or a title of the walker's, or a console line: masked,
// on one line, at most 120 runes.
func qaClip(s string) string { return textx.Clip(oneLine(execx.Redact(s)), qaQuoteRunes) }

// qaFingerprint is how the factory follows defect d on the page at
// pagePath across runs: the first 16 hex digits of the sha256 of its
// class, the page's path and the first code path it names.
func qaFingerprint(d QADefect, pagePath string) string {
	first := ""
	if len(d.Paths) > 0 {
		first = d.Paths[0]
	}
	return sha256Hex([]byte(d.Class + "|" + pagePath + "|" + first))[:16]
}

// walkerDefect is the walker's defect d on the page at pagePath as the
// factory keeps it: its source the walker's and its fingerprint the
// factory's, whatever the walker wrote, its title and quote clipped and
// masked.
func walkerDefect(d QADefect, pagePath string) QADefect {
	d.Source = qaSourceWalker
	d.Title, d.Quote = qaClip(d.Title), qaClip(d.Quote)
	d.Fingerprint = qaFingerprint(d, pagePath)
	return d
}

// readQAProbe reads the probe's JSON at rel in the run's folder runPath: a
// local path that stays inside the folder, a file at most 8 MiB. A browser
// tool may print the probe's string quoted as JSON, or follow it with a
// status line: the file's first JSON value is read, and a string is read
// once more as the probe's JSON. A text or HTML longer than the probe's
// caps is cut, as the probe cuts them.
func readQAProbe(runPath, rel string) (QAProbe, error) {
	f, err := openInRun(runPath, rel)
	if err != nil {
		return QAProbe{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, qaProbeFileCap+1))
	switch {
	case err != nil:
		return QAProbe{}, err
	case len(b) > qaProbeFileCap:
		return QAProbe{}, fmt.Errorf("%w (larger than %d bytes)", errQAProbeFormat, qaProbeFileCap)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(b))).Decode(&raw); err != nil {
		return QAProbe{}, fmt.Errorf("%w (%w)", errQAProbeFormat, err)
	}
	if raw[0] == '"' {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return QAProbe{}, fmt.Errorf("%w (%w)", errQAProbeFormat, err)
		}
		raw = json.RawMessage(strings.TrimSpace(inner))
	}
	var p QAProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		return QAProbe{}, fmt.Errorf("%w (%w)", errQAProbeFormat, err)
	}
	var cut bool
	if len(p.Text) > qaTextCap {
		p.Text, cut = strings.ToValidUTF8(p.Text[:qaTextCap], ""), true
	}
	if len(p.HTML) > qaHTMLCap {
		p.HTML, cut = strings.ToValidUTF8(p.HTML[:qaHTMLCap], ""), true
	}
	p.Truncated = p.Truncated || cut
	return p, nil
}

// openInRun opens the file at rel in the run's folder runPath: a local
// path, opened through os.Root so a link cannot lead out of the folder
// (errQAOutside, as for a path that is not local); a missing file is
// fs.ErrNotExist.
func openInRun(runPath, rel string) (*os.File, error) {
	if !filepath.IsLocal(rel) {
		return nil, errQAOutside
	}
	root, err := os.OpenRoot(runPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case err != nil:
		if _, serr := os.Stat(filepath.Join(runPath, rel)); serr == nil {
			return nil, errQAOutside
		}
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, cmp.Or(err, errors.New("is not a file"))
	}
	return f, nil
}

// qaFileWhy is why the file at rel cannot be used, from the error opening
// or reading it: "pages/01.png is missing".
func qaFileWhy(rel string, err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return qaWord(rel) + " is missing"
	case errors.Is(err, errQAOutside), errors.Is(err, errQAProbeFormat):
		return qaWord(rel) + " " + err.Error()
	}
	return qaWord(rel) + " cannot be read (" + err.Error() + ")"
}

// qaWord is a word of the walker's (an id, a file, a class) as a problem
// quotes it: masked, clipped and quoted, if it needs quotes.
func qaWord(s string) string {
	s = textx.Clip(execx.Redact(s), 64)
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == '"' || r >= utf8.RuneSelf }) {
		return s
	}
	return strconv.Quote(s)
}

// readQAVerdict reads verdict.json in the QA run's folder runRel (relative
// to task directory taskDir, "qa/1-abc1234") for a stage that started at
// since, and checks it against want: version QAVerdictVersion at want's
// head; at most want.MaxPages pages and QAMaxShots PNGs in the folder; each
// page with an id of its own and a path, its shot a PNG, its probe this
// run's (want.Nonce) on the copy's own server (a host on this Mac and
// want.Port), its console log there, each file a local path inside the
// folder; each defect with an id of its own, a known class and severity,
// on one of the pages; and each acceptance check of want covered by a
// page, a blocked step or a not_shown entry. The walker's defects come back
// as the factory keeps them (walkerDefect). A problem's file is
// <runRel>/verdict.json.
func readQAVerdict(taskDir, runRel string, since time.Time, want qaWant) (QAVerdict, *endProblem) {
	file := path.Join(filepath.ToSlash(runRel), qaVerdictFile)
	bad := func(format string, a ...any) (QAVerdict, *endProblem) {
		return QAVerdict{}, &endProblem{file: file, why: fmt.Sprintf(format, a...)}
	}
	text, p := readFresh(taskDir, file, since)
	if p != nil {
		return QAVerdict{}, p
	}
	var v QAVerdict
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return bad("not valid JSON (%v): write the object the prompt shows", err)
	}
	if v.Version != QAVerdictVersion {
		return bad("version %d: the factory reads version %d only", v.Version, QAVerdictVersion)
	}
	if v.Head != want.Head {
		return bad("head %s is not the copy's HEAD %s: write the copy's git rev-parse HEAD", qaWord(v.Head), want.Head)
	}
	runPath := filepath.Join(taskDir, filepath.FromSlash(runRel))
	if most := min(cmp.Or(want.MaxPages, config.DefaultQAMaxPages), config.MaxQAPages); len(v.Pages) > most {
		return bad("%d pages: at most %d", len(v.Pages), most)
	}
	shots, err := countQAShots(runPath)
	if err != nil {
		return bad("its folder cannot be read (%v)", err)
	}
	if shots > QAMaxShots {
		return bad("%d shots in its folder: at most %d", shots, QAMaxShots)
	}
	pages := map[string]string{}
	for _, pg := range v.Pages {
		if why := checkQAPage(runPath, pg, want, pages); why != "" {
			return bad("page %s: %s", qaWord(pg.ID), why)
		}
		pages[pg.ID] = pg.Path
	}
	ids := map[string]bool{}
	for i, d := range v.Defects {
		if why := checkQADefect(d, pages, ids); why != "" {
			return bad("defect %s: %s", qaWord(d.ID), why)
		}
		ids[d.ID] = true
		v.Defects[i] = walkerDefect(d, pages[d.Page])
	}
	if missing := uncoveredQAChecks(v, want.Checks); len(missing) > 0 {
		return bad("acceptance check %s is in no page's checks, no blocked step and no not_shown entry", strings.Join(missing, ", "))
	}
	return v, nil
}

// checkQAPage is what is wrong with page pg of a verdict ("" for nothing);
// seen holds the ids of the pages before it.
func checkQAPage(runPath string, pg QAPage, want qaWant, seen map[string]string) string {
	if _, dup := seen[pg.ID]; pg.ID == "" || dup {
		return "its id is empty or taken twice: give each page an id of its own"
	}
	switch {
	case strings.TrimSpace(pg.Path) == "":
		return "no path"
	case pg.Shot == "":
		return "no shot"
	case pg.Probe == "":
		return "no probe"
	case pg.Console == "":
		return "no console log: save the page's console messages (an empty file when there are none)"
	}
	f, err := openInRun(runPath, pg.Shot)
	if err != nil {
		return "shot " + qaFileWhy(pg.Shot, err)
	}
	magic := make([]byte, 8)
	_, err = io.ReadFull(f, magic)
	f.Close()
	if err != nil || string(magic) != "\x89PNG\r\n\x1a\n" {
		return "shot " + qaWord(pg.Shot) + " is not a PNG"
	}
	probe, err := readQAProbe(runPath, pg.Probe)
	if err != nil {
		return "probe " + qaFileWhy(pg.Probe, err)
	}
	switch {
	case probe.Nonce != want.Nonce:
		return "probe " + qaWord(pg.Probe) + " is not the factory's probe of this run: evaluate the run's probe script in the page " +
			"and save the JSON it returns, unchanged"
	case probe.Error != "":
		return "probe " + qaWord(pg.Probe) + " says the probe failed in the page: evaluate it again once the page has loaded"
	}
	if !qaOnServer(probe.URL, want.Port) {
		return fmt.Sprintf("probe %s shows a page at %s, which is not on the copy's own server (port %d on this Mac): "+
			"leave it out", qaWord(pg.Probe), qaURLHost(probe.URL), want.Port)
	}
	f, err = openInRun(runPath, pg.Console)
	if err != nil {
		return "console " + qaFileWhy(pg.Console, err)
	}
	f.Close()
	return ""
}

// qaOnServer reports whether raw is a URL on the copy's own server: http
// or https, a host on this Mac (config.LocalHost), and port.
func qaOnServer(raw string, port int) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return config.LocalHost(u.Hostname()) && u.Port() == strconv.Itoa(port)
}

// qaURLHost is the host and port of URL raw, never its path or query, as
// a problem names it.
func qaURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "an address that names no host"
	}
	return qaWord(u.Host)
}

// checkQADefect is what is wrong with the walker's defect d ("" for
// nothing): pages maps the verdict's page ids to their paths, ids holds
// the ids of the defects before it.
func checkQADefect(d QADefect, pages map[string]string, ids map[string]bool) string {
	_, onPage := pages[d.Page]
	switch {
	case d.ID == "" || ids[d.ID]:
		return "its id is empty or taken twice: give each defect an id of its own"
	case !slices.Contains(qaClasses, d.Class):
		return "class " + qaWord(d.Class) + " is not one of " + strings.Join(qaClasses, ", ")
	case !slices.Contains(qaSeverities, d.Severity):
		return "severity " + qaWord(d.Severity) + " is not P1, P2 or P3"
	case !onPage:
		return "page " + qaWord(d.Page) + " is not the id of one of its pages"
	}
	return ""
}

// uncoveredQAChecks are the acceptance checks of checks that no page,
// blocked step or not_shown entry of v names, in order.
func uncoveredQAChecks(v QAVerdict, checks []string) []string {
	covered := map[string]bool{}
	for _, pg := range v.Pages {
		for _, c := range pg.Checks {
			covered[c] = true
		}
	}
	for _, b := range v.Blocked {
		covered[b.Check] = true
	}
	for _, n := range v.NotShown {
		covered[n.Check] = true
	}
	var missing []string
	for _, c := range checks {
		if !covered[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

// countQAShots is the number of PNG files in the run's folder runPath and
// below it.
func countQAShots(runPath string) (int, error) {
	root, err := os.OpenRoot(runPath)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	n := 0
	err = fs.WalkDir(root.FS(), ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.Type().IsRegular() && strings.EqualFold(path.Ext(p), ".png") {
			n++
		}
		return nil
	})
	return n, err
}

// qaOutcome decides a QA run from the walker's verdict v and the defects
// found, the factory's own (detectQA's, of every page): unavailable when v
// has no page; else defects when one blocks, clean when none does. A
// defect blocks when its class is in qaBlockingClasses, whoever found it,
// or it is P1 or P2; its pre_existing never unblocks it (slice 1 has no
// before pass). The walker's defect of a class the factory judges whole
// pages by (qaPageClasses), on a page where the factory found that class,
// is the factory's, found again: it counts once, the factory's copy kept
// (mergeQADefect). Nothing else is folded: not the factory's finds on two
// pages of one path, each with its own quote; not the walker's defects
// into each other; not the walker's broken_layout or flow, which may be
// another problem than the factory's. The walker's defects are its own,
// whatever source and fingerprint they claim (walkerDefect). known are
// the defects that do not block.
func qaOutcome(v QAVerdict, found []QADefect) (outcome string, blocking, known []QADefect) {
	if len(v.Pages) == 0 {
		return QAOutcomeUnavailable, nil, nil
	}
	paths := map[string]string{}
	for _, pg := range v.Pages {
		paths[pg.ID] = pg.Path
	}
	var all []QADefect
	onPage := map[string]int{}
	for _, d := range found {
		d.Source = qaSourceFactory
		if d.Fingerprint == "" {
			d.Fingerprint = qaFingerprint(d, paths[d.Page])
		}
		onPage[d.Class+"|"+d.Page] = len(all)
		all = append(all, d)
	}
	for _, d := range v.Defects {
		d = walkerDefect(d, paths[d.Page])
		if i, ok := onPage[d.Class+"|"+d.Page]; ok && slices.Contains(qaPageClasses, d.Class) {
			mergeQADefect(&all[i], d)
			continue
		}
		all = append(all, d)
	}
	for _, d := range all {
		if slices.Contains(qaBlockingClasses, d.Class) || d.Severity == "P1" || d.Severity == "P2" {
			blocking = append(blocking, d)
		} else {
			known = append(known, d)
		}
	}
	if len(blocking) > 0 {
		return QAOutcomeDefects, blocking, known
	}
	return QAOutcomeClean, nil, known
}

// mergeQADefect counts the walker's defect src as the factory's dst, found
// again: dst keeps its quote (src's when it has none), adds src's title,
// and takes the more severe of the two severities and src's code paths it
// lacks, in a slice of its own, so the caller's defect never changes.
func mergeQADefect(dst *QADefect, src QADefect) {
	if qaRank(src.Severity) < qaRank(dst.Severity) {
		dst.Severity = src.Severity
	}
	if src.Title != "" && src.Title != dst.Title {
		dst.Title += "; the walker: " + src.Title
	}
	dst.Quote = cmp.Or(dst.Quote, src.Quote)
	for _, p := range src.Paths {
		if !slices.Contains(dst.Paths, p) {
			dst.Paths = append(slices.Clip(dst.Paths), p)
		}
	}
}

// qaRank orders severities: P1 first, an unknown one last.
func qaRank(sev string) int {
	if i := slices.Index(qaSeverities, sev); i >= 0 {
		return i
	}
	return len(qaSeverities)
}
