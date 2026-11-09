package eval

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/fsx"
)

// Corpus is the set of pull requests with seeded defects a replay is scored against.
type Corpus struct {
	Cases []Case `toml:"case"`
}

// Case is one pull request pinned to the head that is reviewed, with the defects a review must find.
// A local case (Kind KindLocal) is one round of the factory's local review instead, before any PR
// existed: its PR is the round's synthetic number (LocalNumberBase + task × 10 + round, as the round
// ran under), its head the one the round reviewed, and the rest of what the round read (its base
// commit, branch, title, plan, ticket, the operator's answers and roles) is frozen in a directory of
// its own beside the learned corpus when it is added (`magnum eval add`, evalrun.LocalCaseDir), never
// read from the factory's task directory again.
type Case struct {
	Name string `toml:"name"`           // unique, [a-z0-9][a-z0-9-]{0,47}
	Kind string `toml:"kind,omitempty"` // KindPosted ("", every case before kinds) or KindLocal
	PR   string `toml:"pr"`             // owner/repo#N
	Head string `toml:"head"`           // the reviewed head, 40 lower-case hex
	Base string `toml:"base,omitempty"` // base branch name, optional; never on a local case
	Why  string `toml:"why,omitempty"`  // why the case is in the corpus, optional

	Defects []Defect `toml:"defect"`

	// Owner, Repo and Number are parsed from PR.
	Owner  string `toml:"-"`
	Repo   string `toml:"-"`
	Number int    `toml:"-"`
}

// Defect is one thing a review must report, or with Expect ExpectAbsent one it must not. A finding
// matches it when its path matches one of Paths (any path when empty), its range comes within three
// lines of Lines (any line when empty) and one of Match matches its body (anything when empty).
// Paths and Match must not both be empty.
type Defect struct {
	ID    string `toml:"id"`    // unique within the case, same charset as a case name
	Title string `toml:"title"` // shown in reports
	// Severity is, for a defect to find, the weakest severity that still counts as right; for an
	// absent one, the weakest at which a matching finding is a hit (a weaker one is neither a hit
	// nor noise). P0..P3, optional.
	Severity string   `toml:"severity,omitempty"`
	Paths    []string `toml:"paths,omitempty"`  // globs on the slash path, "**" spans directories
	Lines    []int    `toml:"lines,omitempty"`  // inclusive head-side range [from, to]
	Match    []string `toml:"match,omitempty"`  // case-insensitive regexps on the finding body
	Body     bool     `toml:"body,omitempty"`   // a mention in the review body also counts; needs Match
	Expect   string   `toml:"expect,omitempty"` // ExpectFound ("" too) or ExpectAbsent
	// Question asks that the finding be a question for the operator (a local round's provenance
	// marks it, Finding.Question) at Severity or stronger, P2 when it gives none:
	// the factory asks the operator only P0 to P2 questions, so a P3 question goes to the task's
	// leftovers, never to the operator, and a P3 Severity is refused; a finding that is no question
	// has the writer decide. Only a local case's defect to find may ask it.
	Question bool `toml:"question,omitempty"`

	res []*regexp.Regexp // Match, compiled
}

// The kinds of case (Case.Kind): a pull request's posted round, and a round of the factory's local
// review.
const (
	KindPosted = ""
	KindLocal  = "local"
)

// LocalNumberBase is where the synthetic PR numbers of the factory's local rounds start
// (evalrun.LocalNumber: LocalNumberBase + task id × 10 + round), far above any PR number of the
// repositories magnum reviews. A local case's PR number is one of them.
const LocalNumberBase = 900000

// Local reports whether cs is a round of the factory's local review (KindLocal).
func (cs Case) Local() bool { return cs.Kind == KindLocal }

// ValidName reports whether name may name a case (and a defect): [a-z0-9][a-z0-9-]{0,47}.
func ValidName(name string) bool { return idPattern.MatchString(name) }

// What a review is expected to do with a defect (Defect.Expect): report it, the default, or not
// report it, a finding an author declined as wrong (a match is a false positive, Score.AbsentHits).
const (
	ExpectFound  = "found"
	ExpectAbsent = "absent"
)

// absent reports whether the defect is one a review must not report.
func (d *Defect) absent() bool { return d.Expect == ExpectAbsent }

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	prPattern   = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)#([1-9][0-9]*)$`)
	headPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// LoadCorpus reads and parses the corpus file at path.
func LoadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: corpus %s: %w", path, err)
	}
	return parseCorpus(path, data)
}

// LoadCorpora reads the operator's corpus at user ("" = none) and the learned corpus the daemon
// writes at learned (paths.Layout.LearnCorpus; "" or a file that does not exist adds nothing) and
// returns their cases, the user's first. A case name in both files is an error that names it.
// Without a user corpus and without learned cases the error is the learned file's. One path given
// as both (a run given the learned corpus as --corpus, scored again) is read once.
func LoadCorpora(user, learned string) (*Corpus, error) {
	if user != "" && learned != "" && filepath.Clean(user) == filepath.Clean(learned) {
		learned = ""
	}
	var out Corpus
	if user != "" {
		c, err := LoadCorpus(user)
		if err != nil {
			return nil, err
		}
		out.Cases = c.Cases
	}
	if learned == "" {
		if user == "" {
			return nil, errors.New("eval: no corpus")
		}
		return &out, nil
	}
	lc, err := LoadCorpus(learned)
	switch {
	case errors.Is(err, fs.ErrNotExist) && user != "":
		return &out, nil
	case err != nil:
		return nil, err
	}
	var both []string
	for _, cs := range lc.Cases {
		if slices.ContainsFunc(out.Cases, func(u Case) bool { return u.Name == cs.Name }) {
			both = append(both, strconv.Quote(cs.Name))
		}
	}
	if len(both) > 0 {
		return nil, fmt.Errorf("eval: case %s is in both %s and the learned corpus %s: rename it in %s",
			strings.Join(both, ", "), user, learned, user)
	}
	out.Cases = append(out.Cases, lc.Cases...)
	return &out, nil
}

// WriteCorpus writes c to path as TOML that LoadCorpus reads back as c, atomically (a temporary file,
// then a rename) with mode 0600, creating the directory (0700). A corpus LoadCorpus would refuse is
// not written, and a file that already holds the same text is left as it is.
func WriteCorpus(path string, c Corpus) error {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return fmt.Errorf("eval: write corpus %s: %w", path, err)
	}
	if _, err := parseCorpus(path, b.Bytes()); err != nil {
		return fmt.Errorf("eval: write corpus: %w", err)
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b.Bytes()) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("eval: write corpus %s: %w", path, err)
	}
	if err := fsx.WriteFileAtomic(path, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("eval: write corpus %s: %w", path, err)
	}
	return nil
}

// parseCorpus parses and validates a corpus read from src ("" when it has no
// file). It is strict: an unknown key is an error, and every problem found is
// reported (joined), each naming the case, the defect and the rule broken.
func parseCorpus(src string, data []byte) (*Corpus, error) {
	p := &problems{prefix: "eval: corpus"}
	if src != "" {
		p.prefix += " " + src
	}
	var c Corpus
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.prefix, err)
	}
	var raw map[string]any
	_, _ = toml.Decode(string(data), &raw) // only to name the case of an unknown key
	p.unknownKeys(md.Undecoded(), raw)
	c.validate(p)
	if err := p.err(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Select returns the cases called names, in corpus order; no names selects every case. A name the
// corpus does not know is an error.
func (c *Corpus) Select(names []string) ([]Case, error) {
	if len(names) == 0 {
		return slices.Clone(c.Cases), nil
	}
	known := make(map[string]bool, len(c.Cases))
	for _, cs := range c.Cases {
		known[cs.Name] = true
	}
	var unknown []string
	for _, n := range names {
		if !known[n] {
			unknown = append(unknown, strconv.Quote(n))
		}
	}
	if len(unknown) > 0 {
		have := make([]string, 0, len(c.Cases))
		for _, cs := range c.Cases {
			have = append(have, cs.Name)
		}
		return nil, fmt.Errorf("eval: unknown case %s (corpus has: %s)", strings.Join(unknown, ", "), strings.Join(have, ", "))
	}
	var out []Case
	for _, cs := range c.Cases {
		if slices.Contains(names, cs.Name) {
			out = append(out, cs)
		}
	}
	return out, nil
}

// problems collects validation errors; every one reads "<prefix>: <context>: <message>".
type problems struct {
	prefix string
	errs   []error
}

func (p *problems) add(ctx, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ctx != "" {
		msg = ctx + ": " + msg
	}
	p.errs = append(p.errs, fmt.Errorf("%s: %s", p.prefix, msg))
}

func (p *problems) err() error { return errors.Join(p.errs...) }

func caseCtx(i int, name string) string {
	if name == "" {
		return fmt.Sprintf("case #%d", i+1)
	}
	return fmt.Sprintf("case %q", name)
}

func defectCtx(ctx string, j int, id string) string {
	if id == "" {
		return fmt.Sprintf("%s: defect #%d", ctx, j+1)
	}
	return fmt.Sprintf("%s: defect %q", ctx, id)
}

// unknownKeys reports the keys the decoder did not use, naming the case (and defect) that holds
// them by looking them up in the generic decode of the same document.
func (p *problems) unknownKeys(keys []toml.Key, raw map[string]any) {
	rawCases, _ := raw["case"].([]map[string]any)
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key.String()] {
			continue
		}
		seen[key.String()] = true
		k := []string(key)
		last := k[len(k)-1]
		switch {
		case len(k) == 2 && k[0] == "case":
			for i, rc := range rawCases {
				if _, ok := rc[last]; ok {
					name, _ := rc["name"].(string)
					p.add(caseCtx(i, name), "unknown key %q", last)
				}
			}
		case len(k) == 3 && k[0] == "case" && k[1] == "defect":
			for i, rc := range rawCases {
				name, _ := rc["name"].(string)
				rds, _ := rc["defect"].([]map[string]any)
				for j, rd := range rds {
					if _, ok := rd[last]; ok {
						id, _ := rd["id"].(string)
						p.add(defectCtx(caseCtx(i, name), j, id), "unknown key %q", last)
					}
				}
			}
		default:
			p.add("", "unknown key %q", key.String())
		}
	}
}

func (c *Corpus) validate(p *problems) {
	if len(c.Cases) == 0 {
		p.add("", "no cases (at least one [[case]] is required)")
	}
	names := map[string]int{}
	for i := range c.Cases {
		cs := &c.Cases[i]
		ctx := caseCtx(i, cs.Name)
		if !cs.validate(ctx, p) {
			continue
		}
		if first, dup := names[cs.Name]; dup {
			p.add(ctx, "duplicate name (case #%d has it too)", first)
		} else {
			names[cs.Name] = i + 1
		}
	}
}

// Validate reports every rule cs breaks, as LoadCorpus does for a corpus that holds it, and fills
// Owner, Repo and Number from PR; nil when cs is valid.
func (cs *Case) Validate() error {
	p := &problems{prefix: "eval"}
	cs.validate(caseCtx(0, cs.Name), p)
	return p.err()
}

// validate adds the rules cs breaks to p, each in ctx, and reports whether its name is valid (the
// corpus then checks that no other case has it).
func (cs *Case) validate(ctx string, p *problems) (named bool) {
	switch {
	case cs.Name == "":
		p.add(ctx, "name is required")
	case !idPattern.MatchString(cs.Name):
		p.add(ctx, "name must match %s", idPattern)
	default:
		named = true
	}
	if cs.Kind != KindPosted && cs.Kind != KindLocal {
		p.add(ctx, "kind must be %q (or left out for a posted round), got %q", KindLocal, cs.Kind)
	}
	if cs.validatePR(ctx, p) && cs.Local() && cs.Number < LocalNumberBase {
		p.add(ctx, "a local case's pr is its round's synthetic number, %d + task × 10 + round (`magnum eval add` prints it), got %q",
			LocalNumberBase, cs.PR)
	}
	if cs.Local() && cs.Base != "" {
		p.add(ctx, "base is frozen with a local case's inputs (`magnum eval add`): leave it out")
	}
	if !headPattern.MatchString(cs.Head) {
		p.add(ctx, "head must be a full 40-character lower-case hex SHA, got %q", cs.Head)
	}
	if cs.Base != "" && (strings.HasPrefix(cs.Base, "-") || strings.ContainsFunc(cs.Base, func(r rune) bool { return r <= ' ' || r == 0x7f })) {
		p.add(ctx, "base %q is not a branch name", cs.Base)
	}
	if len(cs.Defects) == 0 {
		p.add(ctx, "needs at least one [[case.defect]]")
	}
	ids := map[string]bool{}
	for j := range cs.Defects {
		d := &cs.Defects[j]
		dctx := defectCtx(ctx, j, d.ID)
		if d.ID != "" && idPattern.MatchString(d.ID) {
			if ids[d.ID] {
				p.add(dctx, "duplicate id within the case")
			}
			ids[d.ID] = true
		}
		d.validate(dctx, p)
		if d.Question && !cs.Local() {
			p.add(dctx, "question = true needs a local case: a posted round asks no question")
		}
	}
	return named
}

// validatePR fills Owner, Repo and Number from PR, or adds why it cannot; it reports whether it did.
func (cs *Case) validatePR(ctx string, p *problems) bool {
	m := prPattern.FindStringSubmatch(cs.PR)
	if m == nil || m[2] == "." || m[2] == ".." {
		p.add(ctx, "pr must be owner/repo#N with N > 0, got %q", cs.PR)
		return false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		p.add(ctx, "pr number in %q is out of range", cs.PR)
		return false
	}
	cs.Owner, cs.Repo, cs.Number = m[1], m[2], n
	return true
}

func (d *Defect) validate(ctx string, p *problems) {
	switch {
	case d.ID == "":
		p.add(ctx, "id is required")
	case !idPattern.MatchString(d.ID):
		p.add(ctx, "id must match %s", idPattern)
	}
	if strings.TrimSpace(d.Title) == "" {
		p.add(ctx, "title is required")
	}
	if d.Severity != "" && !validSeverity(d.Severity) {
		p.add(ctx, "severity must be one of P0, P1, P2, P3, got %q", d.Severity)
	}
	if d.Expect != "" && d.Expect != ExpectFound && d.Expect != ExpectAbsent {
		p.add(ctx, "expect must be %q or %q, got %q", ExpectFound, ExpectAbsent, d.Expect)
	}
	for _, g := range d.Paths {
		if err := checkGlob(g); err != nil {
			p.add(ctx, "%v", err)
		}
	}
	if d.Lines != nil {
		switch {
		case len(d.Lines) != 2:
			p.add(ctx, "lines must be exactly two numbers [from, to], got %d", len(d.Lines))
		case d.Lines[0] < 1 || d.Lines[0] > d.Lines[1]:
			p.add(ctx, "lines must satisfy 1 <= from <= to, got [%d, %d]", d.Lines[0], d.Lines[1])
		}
	}
	d.res = nil
	for _, m := range d.Match {
		if m == "" {
			p.add(ctx, "match has an empty regexp, which matches every finding")
			continue
		}
		if _, err := regexp.Compile(m); err != nil {
			p.add(ctx, "match %q: %v", m, err)
			continue
		}
		d.res = append(d.res, regexp.MustCompile("(?i)"+m))
	}
	if len(d.Paths) == 0 && len(d.Match) == 0 {
		p.add(ctx, "needs paths or match, otherwise it cannot be told apart from any finding")
	}
	if d.Body && len(d.Match) == 0 {
		p.add(ctx, "body = true needs match")
	}
	switch {
	case d.Question && d.absent():
		p.add(ctx, "question = true needs a defect to find, not an absent one")
	case d.Question && d.Body:
		p.add(ctx, "question = true and body = true: a mention in the review body is no question")
	case d.Question && validSeverity(d.Severity) && d.Severity > questionSeverity:
		p.add(ctx, "question = true with severity %q: the factory asks you only P0 to P2 questions (a %s one stays among the task's leftovers), "+
			"so give P0 to P2 or leave it out (P2)", d.Severity, d.Severity)
	}
}

// regexps returns the compiled Match. A defect built by hand rather than parsed gets them compiled
// here; one that does not compile never matches.
func (d *Defect) regexps() []*regexp.Regexp {
	if len(d.res) == len(d.Match) {
		return d.res
	}
	var res []*regexp.Regexp
	for _, m := range d.Match {
		if re, err := regexp.Compile("(?i)" + m); err == nil {
			res = append(res, re)
		}
	}
	return res
}

func validSeverity(s string) bool {
	return len(s) == 2 && s[0] == 'P' && s[1] >= '0' && s[1] <= '3'
}
