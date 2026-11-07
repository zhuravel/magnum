package factory

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// qaFixture is a probe of testdata/qa: a page as the factory's probe saw
// it, read the way the factory reads a run's probes.
func qaFixture(t *testing.T, name string) QAProbe {
	t.Helper()
	p, err := readQAProbe(filepath.Join("testdata", "qa"), name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// qaDefectClasses lists the classes of ds, for messages.
func qaDefectClasses(ds []QADefect) []string {
	cs := make([]string, 0, len(ds))
	for _, d := range ds {
		cs = append(cs, d.Class+"/"+d.Severity)
	}
	return cs
}

// wantOneQADefect fails unless ds is one defect of class and severity, the
// factory's, on the page P1, with a fingerprint.
func wantOneQADefect(t *testing.T, ds []QADefect, class, severity string) QADefect {
	t.Helper()
	if len(ds) != 1 {
		t.Fatalf("defects = %v, want one %s", qaDefectClasses(ds), class)
	}
	d := ds[0]
	if d.Class != class || d.Severity != severity || d.Source != "factory" || d.Page != "P1" || d.Fingerprint == "" {
		t.Fatalf("defect = %+v, want class %s, severity %s, source factory, page P1 and a fingerprint", d, class, severity)
	}
	return d
}

// The #785 case: a date's tooltip span reached the page escaped, so the
// page shows the span's markup as text. The factory finds it in the
// probe's text, whatever the walker said: one escaped_markup defect, P2,
// the factory's, quoting the markup, with a fingerprint.
func TestDetectQAFindsTheEscapedTooltipSpan(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/settings/security"}
	classFirst := qaFixture(t, "escaped-tooltip.json")
	classFirst.Text = "Passkeys\nAdded <span data-bs-custom-class=\"tooltip-long\" data-bs-toggle=\"tooltip\" " +
		"data-bs-title=\"Nov 07, 2026 10:15\">Nov 07, 2026</span>\nnever used"
	for _, c := range []struct {
		name  string
		probe QAProbe
		quote string
	}{
		{"the fixture", qaFixture(t, "escaped-tooltip.json"), `<span data-bs-toggle`},
		{"its custom class first", classFirst, `<span data-bs-custom-class="tooltip-long"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := wantOneQADefect(t, detectQA(page, c.probe, nil), QAEscapedMarkup, "P2")
			if !strings.Contains(d.Quote, c.quote) || !strings.Contains(d.Quote, "tooltip-long") {
				t.Errorf("quote = %q, want the markup %s with its tooltip-long class", d.Quote, c.quote)
			}
			if strings.Contains(d.Title, "Nov 07") || strings.Contains(d.Title, "span") {
				t.Errorf("title = %q carries the page's text", d.Title)
			}
		})
	}
}

// The same page with the tooltip rendered as a real element: its span is
// markup in the HTML, and the text shows only the date. Nothing to find.
func TestDetectQAPassesTheSamePageRenderedRight(t *testing.T) {
	p := qaFixture(t, "clean-tooltip.json")
	if !strings.Contains(p.HTML, `<span data-bs-toggle="tooltip" data-bs-custom-class="tooltip-long"`) {
		t.Fatal("the fixture lost its real tooltip span")
	}
	if ds := detectQA(QAPage{ID: "P1", Path: "/settings/security"}, p, nil); len(ds) != 0 {
		t.Errorf("a page rendered right got %v: %+v", qaDefectClasses(ds), ds)
	}
}

// Attributes hold escaped markup legally (a Bootstrap tooltip's
// data-bs-title with data-bs-html, a confirm text, a hidden input's
// value): the escaped_markup detector reads the page's text, never its
// HTML.
func TestDetectQAIgnoresEscapedHTMLInsideAttributes(t *testing.T) {
	p := qaFixture(t, "attribute-html.json")
	if !strings.Contains(p.HTML, `data-bs-title="&lt;b&gt;Admins&lt;/b&gt;`) || !strings.Contains(p.HTML, "&amp;amp;") {
		t.Fatal("the fixture lost its escaped attributes")
	}
	if ds := detectQA(QAPage{ID: "P1", Path: "/settings/roles"}, p, nil); len(ds) != 0 {
		t.Errorf("escaped HTML inside attributes got %v: %+v", qaDefectClasses(ds), ds)
	}
}

// Prose uses angle brackets for keys and placeholders (<Enter>, <name>,
// even <time> or <label>, which are tags' names), and < and > as
// comparisons; a help page shows code in <pre><code>, which the probe
// leaves out of the text. None of it is markup shown by mistake: a bare
// opening tag alone is no defect.
func TestDetectQAIgnoresAnglePlaceholdersThatAreNoHTMLTags(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/help/shortcuts"}
	p := qaFixture(t, "angle-placeholder.json")
	if !strings.Contains(p.HTML, "<pre><code>&lt;span class=\"badge\"") {
		t.Fatal("the fixture lost its code block")
	}
	if ds := detectQA(page, p, nil); len(ds) != 0 {
		t.Errorf("the help page got %v: %+v", qaDefectClasses(ds), ds)
	}
	for _, text := range []string{
		"Press <Enter> to save, then <name>",
		"Remind me at <time>, or pick a <label>",
		"Select <a> or <b> from the list",
		"if a<b and b>c, or x <= y",
		"Rate it <3, AT&T; R&D",
		"Use <Ctrl+S> or <Cmd-S>",
		"<i of n> results",
		"Total: 5 < 6",
		"Write to <e-mail> as <first-name>",
		"Tom & Jerry, R&D; Q&A; Salt&Pepper; &notareal;",
	} {
		q := p
		q.Text = text
		if ds := detectQA(page, q, nil); len(ds) != 0 {
			t.Errorf("text %q got %v", text, qaDefectClasses(ds))
		}
	}
}

// What a page never means to show: a closing tag, a tag with an
// attribute, a line break, whatever the case of its name.
func TestDetectQAFindsAClosingTagAnAttributeOrABreakShownAsText(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/notes"}
	for _, text := range []string{
		"Note: <b>important</b>",
		"Line one<br>Line two",
		"Line one<br/>Line two",
		"Line one<BR />Line two",
		"See <a href=\"/terms\">the terms",
		"See <a href=/terms>the terms",
		"<div class='alert'>Saved",
		"Saved</p>",
		"<STRONG>Done</STRONG>",
		"<h3 id=\"x\">Heading",
		"<turbo-frame id=\"passkeys\">",
		"Saved</turbo-frame>",
		"<nav class=\"navbar\">Home",
		"<sl-tooltip content=\"Nov 07\">",
		"Footer</footer>",
		"<hr class=\"my-2\">",
	} {
		ds := detectQA(page, QAProbe{Status: 200, Text: text}, nil)
		if len(ds) != 1 || ds[0].Class != QAEscapedMarkup {
			t.Errorf("text %q got %v, want one escaped_markup", text, qaDefectClasses(ds))
		}
	}
}

// An entity the page shows as text is markup escaped twice: any entity
// HTML names, not a few of them.
func TestDetectQAFindsADisplayedEntity(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/teams"}
	for _, text := range []string{"Tom &amp; Jerry", "It&#39;s saved", "a&nbsp;b", "x &#x27;y&#X27;", "&lt;b&gt;",
		"Loading&hellip;", "2020&ndash;2026 &copy; Shop", "&times;", "It&rsquo;s saved"} {
		d := wantOneQADefect(t, detectQA(page, QAProbe{Status: 200, Text: "Teams\n" + text}, nil), QAEscapedMarkup, "P2")
		if !strings.Contains(d.Quote, "&") {
			t.Errorf("text %q: quote %q lacks the entity", text, d.Quote)
		}
	}
}

// A translation that is missing shows as "translation missing: …" in the
// text, as Rails' translation_missing span in the HTML, whose text is only
// the key's last word, or as i18n-js's `[missing "…" translation]`.
func TestDetectQAFindsAMissingTranslation(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/users"}
	p := qaFixture(t, "missing-translation.json")
	d := wantOneQADefect(t, detectQA(page, p, nil), QARawI18nKey, "P2")
	if !strings.Contains(d.Quote, "translation_missing") {
		t.Errorf("quote = %q, want the span", d.Quote)
	}
	text := QAProbe{Status: 200, Text: "Users\ntranslation missing: en.users.title", HTML: "<html><body><h1>Users</h1></body></html>"}
	d = wantOneQADefect(t, detectQA(page, text, nil), QARawI18nKey, "P2")
	if !strings.Contains(d.Quote, "en.users.title") {
		t.Errorf("quote = %q, want the key", d.Quote)
	}
	i18nJS := QAProbe{Status: 200, Text: "Users\n[missing \"en.users.title\" translation]"}
	d = wantOneQADefect(t, detectQA(page, i18nJS, nil), QARawI18nKey, "P2")
	if !strings.Contains(d.Quote, "en.users.title") {
		t.Errorf("i18n-js quote = %q, want the key", d.Quote)
	}
}

// An error page: a 5xx or a 404, or the page of a framework's error
// whatever its status. One defect per page, whatever shows it.
func TestDetectQAFindsAnErrorPage(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/settings/security"}
	rails := qaFixture(t, "error-page.json")
	notFound := QAProbe{Status: 404, Title: "The page you were looking for doesn't exist (404)", Text: "The page you were looking for doesn't exist."}
	caught := rails
	caught.Status = 200
	sorry := QAProbe{Status: 200, Title: "Example Shop", Text: "We’re sorry,\nbut something went wrong."}
	titled := QAProbe{Status: 200, Title: "500 Internal Server Error", Text: "Internal Server Error"}
	for name, p := range map[string]QAProbe{"status 500": rails, "status 404": notFound, "a 200 with the exception page's title": caught,
		"a 200 with the 500 page's words": sorry, "a 200 titled Internal Server Error": titled} {
		t.Run(name, func(t *testing.T) {
			d := wantOneQADefect(t, detectQA(page, p, nil), QAErrorPage, "P2")
			if strings.Contains(d.Title, "NoMethodError") || strings.Contains(d.Title, "created_at") {
				t.Errorf("title = %q carries the page's text", d.Title)
			}
		})
	}
	for _, status := range []int{0, 200, 302, 403} {
		if ds := detectQA(page, QAProbe{Status: status, Title: "Security", Text: "Security"}, nil); len(ds) != 0 {
			t.Errorf("status %d got %v", status, qaDefectClasses(ds))
		}
	}
}

// A normal page holds an error page's words where the page does not show
// them as its own: in a hidden toast, in a data attribute, or as a line of
// a log it lists. Only the title and the text the page shows count, and
// "Internal Server Error" only in the title.
func TestDetectQAIgnoresErrorWordsANormalPageHolds(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/webhooks"}
	for name, p := range map[string]QAProbe{
		"a hidden toast": {Status: 200, Title: "Webhooks", Text: "Webhooks\nNo deliveries yet",
			HTML: `<html><body><h1>Webhooks</h1><p>No deliveries yet</p><div class="toast d-none">We're sorry, but something went wrong.</div></body></html>`},
		"a data attribute": {Status: 200, Title: "Webhooks", Text: "Webhooks\nNo deliveries yet",
			HTML: `<html><body data-i18n='{"e500":"Internal Server Error","e404":"Unhandled Runtime Error"}'><h1>Webhooks</h1></body></html>`},
		"a visible log line": {Status: 200, Title: "Webhooks", Text: "Webhooks\nLast attempt: 500 Internal Server Error\nRetry",
			HTML: `<html><body><h1>Webhooks</h1><td>Last attempt: 500 Internal Server Error</td></body></html>`},
	} {
		if ds := detectQA(page, p, nil); len(ds) != 0 {
			t.Errorf("%s got %v", name, qaDefectClasses(ds))
		}
	}
}

// The console's errors are one P3 note per page; its warnings are none. A
// page that scrolls sideways is a P3 broken_layout. Neither blocks alone.
func TestDetectQANotesConsoleErrorsAndSidewaysScroll(t *testing.T) {
	page := QAPage{ID: "P1", Path: "/settings/security"}
	p := qaFixture(t, "clean-tooltip.json")
	console := []string{"[warning] x", "[error] Uncaught TypeError: x", "[log] y", "[error] Failed to load resource"}
	d := wantOneQADefect(t, detectQA(page, p, console), QAConsoleError, "P3")
	if !strings.Contains(d.Quote, "Uncaught TypeError") || !strings.Contains(d.Title, "2") {
		t.Errorf("console defect = %+v, want the first error quoted and both counted", d)
	}
	if ds := detectQA(page, p, []string{"[warning] x", "[info] y"}); len(ds) != 0 {
		t.Errorf("warnings got %v", qaDefectClasses(ds))
	}
	p.OverflowX = true
	wantOneQADefect(t, detectQA(page, p, nil), QABrokenLayout, "P3")
}

// A quote is at most 120 runes around what the factory found, on one
// line, with a secret near it masked, never its first half.
func TestDetectQAQuotesAtMost120RunesAndMasksSecrets(t *testing.T) {
	token := "ghp_" + strings.Repeat("A1b2", 9)
	text := strings.Repeat("lorem ipsum ", 40) + "token " + token + "\n<span class=\"x\">Nov 07</span>" + strings.Repeat(" dolor", 60)
	d := wantOneQADefect(t, detectQA(QAPage{ID: "P1", Path: "/x"}, QAProbe{Status: 200, Text: text}, nil), QAEscapedMarkup, "P2")
	if n := utf8.RuneCountInString(d.Quote); n > 120 {
		t.Errorf("quote has %d runes, want at most 120", n)
	}
	if strings.Contains(d.Quote, "A1b2") || strings.Contains(d.Quote, "\n") || !strings.Contains(d.Quote, `<span class="x">`) ||
		!strings.HasPrefix(d.Quote, "…") {
		t.Errorf("quote = %q, want the markup on one line, the token masked and the cut lead marked", d.Quote)
	}
}

// qaRun is a QA run's folder as a walker leaves it: a verdict of two
// pages, their shots, probes and console logs, in task directory taskDir.
type qaRun struct {
	t       *testing.T
	taskDir string
	runPath string
	since   time.Time
	v       QAVerdict
	raw     string // the verdict's file as it is, when set
	want    qaWant
}

const (
	qaTestRun  = "qa/1-abc1234"
	qaTestHead = "abc1234def5678abc1234def5678abc1234def56"
)

// pngMagic is the signature every PNG file starts with.
var pngMagic = "\x89PNG\r\n\x1a\n"

func newQARun(t *testing.T) *qaRun {
	t.Helper()
	dir := t.TempDir()
	r := &qaRun{t: t, taskDir: dir, runPath: filepath.Join(dir, qaTestRun), since: time.Now().Add(-time.Minute),
		want: qaWant{Head: qaTestHead, Nonce: "n0nce", Port: 3001, MaxPages: 12, Checks: []string{"A1", "A2"}}}
	r.v = QAVerdict{Version: 1, Head: qaTestHead}
	r.page("P1", "/settings/security", "http://app.example.localhost:3001/settings/security", "A1")
	r.page("P2", "/settings/passkeys", "http://127.0.0.1:3001/settings/passkeys?tab=all", "A2")
	r.v.Defects = []QADefect{{ID: "D1", Class: QAFlow, Severity: "P3", Page: "P2", Title: "the Add button needs two clicks",
		Quote: "Add a passkey", Paths: []string{"app/javascript/passkeys.js"}, Source: "factory", Fingerprint: "not-the-walkers"}}
	return r
}

// page adds a page to the verdict with its shot, its probe at url and its
// console log, numbered after the pages before it.
func (r *qaRun) page(id, path, url string, checks ...string) {
	r.t.Helper()
	n := len(r.v.Pages) + 1
	base := fmt.Sprintf("pages/%02d-%s-after", n, strings.ToLower(id))
	pg := QAPage{ID: id, Path: path, State: "signed_in", Side: "after", Shot: base + ".png", Probe: base + ".json",
		Console: fmt.Sprintf("console/%02d.log", n), Status: 200, Checks: checks}
	r.write(pg.Shot, pngMagic+"rest of the image")
	r.probe(pg.Probe, "n0nce", url)
	r.write(pg.Console, "[log] ready\n")
	r.v.Pages = append(r.v.Pages, pg)
}

// probe writes a probe of the page at url with nonce at rel.
func (r *qaRun) probe(rel, nonce, url string) {
	r.t.Helper()
	b, err := json.Marshal(QAProbe{Nonce: nonce, URL: url, Status: 200, Title: "Security", Text: "Security", HTML: "<html></html>"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.write(rel, string(b))
}

// write writes text to rel in the run's folder.
func (r *qaRun) write(rel, text string) {
	r.t.Helper()
	p := filepath.Join(r.runPath, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// read writes the verdict (raw, else v) and reads it as the factory does.
func (r *qaRun) read() (QAVerdict, *endProblem) {
	r.t.Helper()
	b, err := json.Marshal(r.v)
	if err != nil {
		r.t.Fatal(err)
	}
	r.write("verdict.json", cmp.Or(r.raw, string(b)))
	return readQAVerdict(r.taskDir, qaTestRun, r.since, r.want)
}

// A verdict of this run's walker, at the copy's head, whose pages are on
// the copy's own server (a name under .localhost or 127.0.0.1, on its
// port), whose shots are PNGs and whose probes are the factory's, covering
// every acceptance check: the factory takes it, and marks the walker's
// defects as the walker's, with the factory's own fingerprint.
func TestReadQAVerdictTakesAValidRun(t *testing.T) {
	r := newQARun(t)
	v, p := r.read()
	if p != nil {
		t.Fatalf("a valid run was refused: %s", p)
	}
	if len(v.Pages) != 2 || v.Pages[1].Probe != "pages/02-p2-after.json" {
		t.Fatalf("pages = %+v", v.Pages)
	}
	d := v.Defects[0]
	if d.Source != "walker" || d.Fingerprint != qaFingerprint(d, "/settings/passkeys") || len(d.Fingerprint) != 16 {
		t.Errorf("the walker's defect = %+v, want source walker and the factory's fingerprint", d)
	}
	blocked := newQARun(t)
	blocked.v.Pages[1].Checks = nil
	blocked.v.Blocked = []QABlocked{{What: "the passkeys page with a key added", Why: "no virtual authenticator", Check: "A2"}}
	if _, p := blocked.read(); p != nil {
		t.Errorf("a check in blocked was refused: %s", p)
	}
	notShown := newQARun(t)
	notShown.v.Pages[1].Checks = nil
	notShown.v.NotShown = []QANotShown{{Check: "A2", Why: "a background job"}}
	if _, p := notShown.read(); p != nil {
		t.Errorf("a check in not_shown was refused: %s", p)
	}
}

// What the factory refuses in a verdict: one written before the stage, of
// another version or head, files outside the run's folder or missing, a
// shot that is not a PNG, a probe that is not this run's, a page off the
// copy's own server, more pages or shots than allowed, a class or a
// severity the contract lacks, a defect on no page, an acceptance check
// left out. The problem names the verdict's file.
func TestReadQAVerdictRefuses(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(r *qaRun)
		wants string
	}{
		{"stale", func(r *qaRun) { r.since = time.Now().Add(time.Hour) }, "older than this stage"},
		{"not JSON", func(r *qaRun) { r.raw = "{" }, "not valid JSON"},
		{"version 2", func(r *qaRun) { r.v.Version = 2 }, "version 2"},
		{"head", func(r *qaRun) { r.v.Head = "0000000def5678abc1234def5678abc1234def56" }, "head"},
		{"head a token", func(r *qaRun) { r.v.Head = "ghp_" + strings.Repeat("x9", 18) }, "<redacted>"},
		{"shot ../x.png", func(r *qaRun) { r.write("../x.png", pngMagic); r.v.Pages[0].Shot = "../x.png" }, "outside the run folder"},
		{"shot linked out", func(r *qaRun) {
			r.write("../x.png", pngMagic)
			if err := os.Symlink(filepath.Join(r.taskDir, "qa", "x.png"), filepath.Join(r.runPath, "pages", "link.png")); err != nil {
				t.Fatal(err)
			}
			r.v.Pages[0].Shot = "pages/link.png"
		}, "outside the run folder"},
		{"missing shot", func(r *qaRun) { r.v.Pages[0].Shot = "pages/09-gone.png" }, "missing"},
		{"shot not a PNG", func(r *qaRun) { r.write(r.v.Pages[0].Shot, "<html>not an image</html>") }, "not a PNG"},
		{"missing probe", func(r *qaRun) { r.v.Pages[1].Probe = "pages/09-gone.json" }, "missing"},
		{"probe nonce", func(r *qaRun) { r.probe(r.v.Pages[0].Probe, "other", "http://app.example.localhost:3001/") }, "not the factory's probe"},
		{"probe failed", func(r *qaRun) {
			r.write(r.v.Pages[0].Probe, `{"nonce": "n0nce", "url": "http://app.example.localhost:3001/", "error": "boom"}`)
		}, "the probe failed in the page"},
		{"probe wrapped twice", func(r *qaRun) {
			b, _ := json.Marshal(`"{\"nonce\": \"n0nce\"}"`)
			r.write(r.v.Pages[0].Probe, string(b))
		}, "not the JSON the probe returns"},
		{"off-host: http://staging.example.com/x", func(r *qaRun) { r.probe(r.v.Pages[0].Probe, "n0nce", "http://staging.example.com/x") },
			"not on the copy's own server"},
		{"other-port: :3000", func(r *qaRun) { r.probe(r.v.Pages[0].Probe, "n0nce", "http://app.example.localhost:3000/settings") },
			"not on the copy's own server"},
		{"no port", func(r *qaRun) { r.probe(r.v.Pages[0].Probe, "n0nce", "http://localhost/settings") }, "not on the copy's own server"},
		{"missing console", func(r *qaRun) { r.v.Pages[0].Console = "console/09.log" }, "missing"},
		{"two pages P1", func(r *qaRun) { r.v.Pages[1].ID = "P1" }, "twice"},
		{"13 pages with max 12", func(r *qaRun) {
			for i := 3; i <= 13; i++ {
				r.page(fmt.Sprintf("P%d", i), "/x", "http://app.example.localhost:3001/x", "A1")
			}
		}, "13 pages"},
		{"31 shots", func(r *qaRun) {
			for i := range 29 {
				r.write(fmt.Sprintf("pages/extra-%02d.png", i), pngMagic)
			}
		}, "31 shots"},
		{"defect class ugly", func(r *qaRun) { r.v.Defects[0].Class = "ugly" }, "class"},
		{"severity P0", func(r *qaRun) { r.v.Defects[0].Severity = "P0" }, "severity"},
		{"defect on page P9", func(r *qaRun) { r.v.Defects[0].Page = "P9" }, "P9"},
		{"A2 uncovered", func(r *qaRun) { r.v.Pages[1].Checks = nil }, "A2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newQARun(t)
			c.edit(r)
			_, p := r.read()
			if p == nil {
				t.Fatalf("the factory took a verdict with %s", c.name)
			}
			if p.file != "qa/1-abc1234/verdict.json" || !strings.Contains(p.why, c.wants) || strings.Contains(p.why, "ghp_") {
				t.Errorf("problem = %s, want qa/1-abc1234/verdict.json saying %q", p, c.wants)
			}
		})
	}
}

// The outcome is the factory's, from the saved pages: no page is QA
// unavailable; what the factory found blocks whatever the walker said; the
// walker's own defects block only as P1 or P2 or in a class that always
// blocks, and its pre_existing never unblocks one (slice 1 has no before
// pass). The walker's copy of a page-wide find of the factory's on the same
// page counts once: the factory's copy is kept, at the more severe
// severity, with the walker's code paths and title.
func TestQAOutcomeDecidesFromTheFilesNotTheWalker(t *testing.T) {
	pages := []QAPage{{ID: "P1", Path: "/settings/security"}, {ID: "P2", Path: "/settings/passkeys"}}
	escaped := detectQA(pages[0], qaFixture(t, "escaped-tooltip.json"), nil)
	console := detectQA(pages[1], QAProbe{Status: 200}, []string{"[error] Uncaught TypeError: x"})
	walker := func(class, sev, page string, paths ...string) QADefect {
		return QADefect{ID: "D1", Class: class, Severity: sev, Page: page, Title: "t", Paths: paths}
	}
	for _, c := range []struct {
		name            string
		v               QAVerdict
		found           []QADefect
		outcome         string
		blocking, known int
	}{
		{"no pages", QAVerdict{Version: 1}, nil, QAOutcomeUnavailable, 0, 0},
		{"the factory's escaped markup, the walker's nothing", QAVerdict{Pages: pages}, escaped, QAOutcomeDefects, 1, 0},
		{"a walker P3 flow note", QAVerdict{Pages: pages, Defects: []QADefect{walker(QAFlow, "P3", "P2")}}, nil, QAOutcomeClean, 0, 1},
		{"a walker P2 flow", QAVerdict{Pages: pages, Defects: []QADefect{walker(QAFlow, "P2", "P2")}}, nil, QAOutcomeDefects, 1, 0},
		{"a walker P3 escaped markup", QAVerdict{Pages: pages, Defects: []QADefect{walker(QAEscapedMarkup, "P3", "P1")}}, nil, QAOutcomeDefects, 1, 0},
		{"a pre-existing walker escaped markup", QAVerdict{Pages: pages, Defects: []QADefect{func() QADefect {
			d := walker(QAEscapedMarkup, "P2", "P1")
			d.PreExisting = true
			return d
		}()}}, nil, QAOutcomeDefects, 1, 0},
		{"the factory's console error alone", QAVerdict{Pages: pages}, console, QAOutcomeClean, 0, 1},
		{"the walker's copy of the factory's find", QAVerdict{Pages: pages, Defects: []QADefect{walker(QAEscapedMarkup, "P2", "P1")}}, escaped,
			QAOutcomeDefects, 1, 0},
		{"the walker's copy with its paths", QAVerdict{Pages: pages,
			Defects: []QADefect{walker(QAEscapedMarkup, "P2", "P1", "app/helpers/time_helper.rb")}}, escaped, QAOutcomeDefects, 1, 0},
		{"the walker's escaped markup on another page", QAVerdict{Pages: pages, Defects: []QADefect{walker(QAEscapedMarkup, "P2", "P2")}}, escaped,
			QAOutcomeDefects, 2, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			outcome, blocking, known := qaOutcome(c.v, c.found)
			if outcome != c.outcome || len(blocking) != c.blocking || len(known) != c.known {
				t.Fatalf("outcome = %s, blocking %v, known %v; want %s, %d blocking, %d known",
					outcome, qaDefectClasses(blocking), qaDefectClasses(known), c.outcome, c.blocking, c.known)
			}
		})
	}
	merged := walker(QAEscapedMarkup, "P1", "P1", "app/helpers/time_helper.rb")
	merged.Title = "the passkey's date shows a span"
	_, blocking, _ := qaOutcome(QAVerdict{Pages: pages, Defects: []QADefect{merged}}, escaped)
	if d := blocking[0]; d.Source != "factory" || d.Severity != "P1" || !slices.Equal(d.Paths, []string{"app/helpers/time_helper.rb"}) ||
		d.Fingerprint != escaped[0].Fingerprint || d.Quote != escaped[0].Quote || !strings.Contains(d.Title, "the passkey's date shows a span") {
		t.Errorf("merged = %+v, want the factory's copy and quote at P1 with the walker's path and title", d)
	}
	if len(escaped[0].Paths) != 0 || strings.Contains(escaped[0].Title, "passkey") {
		t.Errorf("the merge changed the caller's defect: %+v", escaped[0])
	}
	lie := QAVerdict{Pages: pages, Defects: []QADefect{walker(QAFlow, "P1", "P2", "b.js")}}
	lie.Defects[0].Fingerprint, lie.Defects[0].Source = escaped[0].Fingerprint, "factory"
	if _, blocking, _ := qaOutcome(lie, escaped); len(blocking) != 2 {
		t.Errorf("a walker's defect claiming the factory's fingerprint hid one: %v", qaDefectClasses(blocking))
	}
}

// The walker's own defects are never folded into each other, whatever
// their fingerprints: two flow defects on one page without code paths are
// two problems. Nor is a walker's broken_layout folded into the factory's
// sideways scroll: the walker's may be another problem (a modal over the
// Save button), and its title must reach the fix turn.
func TestQAOutcomeKeepsTheWalkersOwnDefectsApart(t *testing.T) {
	pages := []QAPage{{ID: "P1", Path: "/settings/security"}}
	two := QAVerdict{Pages: pages, Defects: []QADefect{
		{ID: "D1", Class: QAFlow, Severity: "P3", Page: "P1", Title: "the Add button needs two clicks"},
		{ID: "D2", Class: QAFlow, Severity: "P1", Page: "P1", Title: "Delete does nothing"},
	}}
	if outcome, blocking, known := qaOutcome(two, nil); outcome != QAOutcomeDefects || len(blocking) != 1 || len(known) != 1 ||
		blocking[0].Title != "Delete does nothing" {
		t.Errorf("two walker flows = %s, blocking %+v, known %+v; want each kept", outcome, blocking, known)
	}
	sideways := detectQA(pages[0], QAProbe{Status: 200, OverflowX: true}, nil)
	modal := QAVerdict{Pages: pages, Defects: []QADefect{{ID: "D1", Class: QABrokenLayout, Severity: "P2", Page: "P1",
		Title: "the modal covers the Save button"}}}
	_, blocking, known := qaOutcome(modal, sideways)
	if len(blocking) != 1 || blocking[0].Title != "the modal covers the Save button" || len(known) != 1 {
		t.Errorf("the walker's modal = blocking %+v, known %+v; want the walker's P2 apart from the factory's P3", blocking, known)
	}
}

// Two pages of one path (two states) keep the factory's find of each, with
// its own quote: neither is folded into the other.
func TestQAOutcomeKeepsTheFactorysFindOfEachPage(t *testing.T) {
	pages := []QAPage{{ID: "P1", Path: "/settings/security"}, {ID: "P2", Path: "/settings/security"}}
	found := slices.Concat(detectQA(pages[0], QAProbe{Status: 200, Text: "one &amp; two"}, nil),
		detectQA(pages[1], QAProbe{Status: 200, Text: "three &amp; four"}, nil))
	_, blocking, _ := qaOutcome(QAVerdict{Pages: pages}, found)
	if len(blocking) != 2 || !strings.Contains(blocking[1].Quote, "three") {
		t.Errorf("blocking = %+v, want the find of each page", blocking)
	}
}

// The probe's file as a browser tool leaves it: the JSON the probe
// returns, or that JSON quoted once as a string, either one followed by
// the tool's status line. All of them read as the probe; a string quoted
// twice does not.
func TestReadQAProbeTakesTheJSONQuotedOnceOrFollowedByAStatusLine(t *testing.T) {
	dir := t.TempDir()
	plain, err := json.Marshal(QAProbe{Nonce: "n0nce", URL: "http://app.example.localhost:3001/", Status: 200, Text: "Tom &amp; Jerry"})
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(plain))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"plain":                     string(plain),
		"plain with a status line":  "\n" + string(plain) + "\n✓ Done\n",
		"quoted once":               string(quoted),
		"quoted with a status line": string(quoted) + "\n✓ Done in 120ms\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "p.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := readQAProbe(dir, "p.json")
		if err != nil || p.Nonce != "n0nce" || p.Text != "Tom &amp; Jerry" {
			t.Errorf("%s: probe = %+v, %v", name, p, err)
		}
	}
	twice, err := json.Marshal(string(quoted))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.json"), twice, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readQAProbe(dir, "p.json"); err == nil {
		t.Error("a probe quoted twice was read")
	}
}

// The probe the walker evaluates carries this run's nonce, reads only the
// text a page shows (code, inputs, scripts and styles left out), caps its
// text and HTML, reports the page's HTTP status, and starts with its
// expression, so a tool that runs it as `return <script>` gets its value.
func TestTheProbeScriptCarriesTheNonceAndSkipsCode(t *testing.T) {
	s := string(qaProbeScript("n0nce"))
	for _, want := range []string{`"n0nce"`, "PRE", "CODE", "TEXTAREA", "SCRIPT", "STYLE", "NOSCRIPT", "TEMPLATE", "200000", "400000",
		"responseStatus", "overflow_x", "truncated", "JSON.stringify", `"contents"`, "getClientRects", "catch", "createHTMLDocument"} {
		if !strings.Contains(s, want) {
			t.Errorf("the probe lacks %q", want)
		}
	}
	if strings.Contains(s, "__MAGNUM_NONCE__") {
		t.Error("the probe kept its nonce placeholder")
	}
	if !strings.HasPrefix(s, "(() => {") {
		t.Errorf("the probe starts with %q, want its expression", s[:min(len(s), 20)])
	}
	if q := string(qaProbeScript(`a"b`)); !strings.Contains(q, `"a\"b"`) {
		t.Error("a nonce with a quote broke out of its string")
	}
}

// runQAProbe runs the probe of nonce n0nce in Node against the fake DOM of
// testdata/qa/<page> (probe-harness.js, which follows the browser's rules
// the probe relies on), and reads what it returns as the factory reads a
// probe's file. Without node on PATH the test is skipped: no browser runs.
func runQAProbe(t *testing.T, page string, args ...string) QAProbe {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the probe's page tests run qa_probe.js in it")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.js")
	if err := os.WriteFile(script, qaProbeScript("n0nce"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness, err := filepath.Abs(filepath.Join("testdata", "qa", "probe-harness.js"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), node, append([]string{harness, script, filepath.Join("testdata", "qa", page)}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the probe failed on %s: %v\n%s", page, err, stderr.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := readQAProbe(dir, "probe.json")
	if err != nil {
		t.Fatalf("the probe's output on %s does not read: %v\n%s", page, err, out)
	}
	return p
}

// The #785 span inside a turbo-frame styled display: contents. Such a frame
// has no box of its own, so checkVisibility() is false on it while its
// content shows: the probe walks into it, and keeps a text node right
// under a display: contents element, which the factory then finds.
func TestTheProbeReadsTextUnderDisplayContents(t *testing.T) {
	p := runQAProbe(t, "display-contents.dom.json")
	for _, want := range []string{`Added <span data-bs-toggle="tooltip" data-bs-custom-class="tooltip-long"`, "Shared with Tom &amp; Jerry"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("the probe's text lacks %q:\n%s", want, p.Text)
		}
	}
	if p.Nonce != "n0nce" || p.Status != 200 || p.URL != "http://app.example.localhost:3001/settings/security" {
		t.Errorf("probe = %+v", p)
	}
	d := wantOneQADefect(t, detectQA(QAPage{ID: "P1", Path: "/settings/security"}, p, nil), QAEscapedMarkup, "P2")
	if !strings.Contains(d.Quote, "tooltip-long") {
		t.Errorf("quote = %q, want the tooltip span", d.Quote)
	}
}

// What the probe leaves out of the text: display none, visibility hidden
// (but not a visible child of it), code (PRE, CODE, SAMP), input
// (TEXTAREA, editable elements), code editors (CodeMirror, Monaco, Ace),
// and what is never shown (SCRIPT, STYLE, TEMPLATE, NOSCRIPT); it keeps
// prose and an open shadow root's text. Its HTML has the scripts' and the
// templates' contents emptied. Nothing on such a page is a defect.
func TestTheProbeLeavesOutHiddenTextCodeAndEditors(t *testing.T) {
	p := runQAProbe(t, "hidden-and-code.dom.json")
	for _, want := range []string{"Plain visible prose", "Visible inside hidden", "Shadow text that shows"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("the probe's text lacks %q:\n%s", want, p.Text)
		}
	}
	for _, gone := range []string{"hidden</b>", "ghost", "badge", "</div>", "draft", "typed by", "{{ title }}", "</span>", `<div class="x">`,
		"tpl", "enable JavaScript", "window.flash"} {
		if strings.Contains(p.Text, gone) {
			t.Errorf("the probe's text holds %q:\n%s", gone, p.Text)
		}
	}
	for _, gone := range []string{"window.flash", "content: ", "tpl"} {
		if strings.Contains(p.HTML, gone) {
			t.Errorf("the probe's HTML holds %q:\n%s", gone, p.HTML)
		}
	}
	if ds := detectQA(QAPage{ID: "P1", Path: "/help/templates"}, p, nil); len(ds) != 0 {
		t.Errorf("the page got %v: %+v", qaDefectClasses(ds), ds)
	}
}

// A tool that evaluates the probe as a function body (`return <script>`)
// gets the probe's JSON, not undefined: the script starts with its
// expression, not with a comment.
func TestTheProbeRunsAsAFunctionBody(t *testing.T) {
	if p := runQAProbe(t, "display-contents.dom.json", "--function-body"); p.Nonce != "n0nce" || p.Text == "" {
		t.Errorf("probe = %+v", p)
	}
}

// A page that breaks the probe gets a probe that says so, with the run's
// nonce, never no file or an empty text the factory would read as clean;
// the factory refuses a verdict with such a page (TestReadQAVerdictRefuses).
func TestTheProbeReportsItsOwnFailure(t *testing.T) {
	p := runQAProbe(t, "throws.dom.json")
	if p.Nonce != "n0nce" || p.Error == "" || p.Text != "" {
		t.Errorf("probe = %+v, want the nonce and the error", p)
	}
}

// A page whose code throws the empty string still gets a probe that says
// it failed: an empty error would read as a page that probed clean.
func TestAnEmptyThrowStillReportsTheProbesFailure(t *testing.T) {
	p := runQAProbe(t, "throws-empty.dom.json")
	if p.Error == "" || p.Text != "" {
		t.Errorf("probe = %+v, want a non-empty error and no text", p)
	}
}
