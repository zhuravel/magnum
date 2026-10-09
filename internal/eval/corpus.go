package eval

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Corpus is the set of pull requests with seeded defects a replay is scored against.
type Corpus struct {
	Cases []Case `toml:"case"`
}

// Case is one pull request pinned to the head that is reviewed, with the defects a review must find.
type Case struct {
	Name    string   `toml:"name"` // unique, [a-z0-9][a-z0-9-]{0,47}
	PR      string   `toml:"pr"`   // owner/repo#N
	Head    string   `toml:"head"` // the reviewed head, 40 lower-case hex
	Base    string   `toml:"base"` // base branch name, optional
	Why     string   `toml:"why"`  // why the case is in the corpus, optional
	Defects []Defect `toml:"defect"`

	// Owner, Repo and Number are parsed from PR.
	Owner  string `toml:"-"`
	Repo   string `toml:"-"`
	Number int    `toml:"-"`
}

// Defect is one thing a review must report. A finding matches it when its path matches one of Paths
// (any path when empty), its range comes within three lines of Lines (any line when empty) and one
// of Match matches its body (anything when empty). Paths and Match must not both be empty.
type Defect struct {
	ID       string   `toml:"id"`       // unique within the case, same charset as a case name
	Title    string   `toml:"title"`    // shown in reports
	Severity string   `toml:"severity"` // weakest severity that still counts as right, P0..P3, optional
	Paths    []string `toml:"paths"`    // globs on the slash path, "**" spans directories
	Lines    []int    `toml:"lines"`    // inclusive head-side range [from, to]
	Match    []string `toml:"match"`    // case-insensitive regexps on the finding body
	Body     bool     `toml:"body"`     // a mention in the review body also counts; needs Match

	res []*regexp.Regexp // Match, compiled
}

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
		switch {
		case cs.Name == "":
			p.add(ctx, "name is required")
		case !idPattern.MatchString(cs.Name):
			p.add(ctx, "name must match %s", idPattern)
		default:
			if first, dup := names[cs.Name]; dup {
				p.add(ctx, "duplicate name (case #%d has it too)", first)
			} else {
				names[cs.Name] = i + 1
			}
		}
		cs.validatePR(ctx, p)
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
		}
	}
}

func (cs *Case) validatePR(ctx string, p *problems) {
	m := prPattern.FindStringSubmatch(cs.PR)
	if m == nil || m[2] == "." || m[2] == ".." {
		p.add(ctx, "pr must be owner/repo#N with N > 0, got %q", cs.PR)
		return
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		p.add(ctx, "pr number in %q is out of range", cs.PR)
		return
	}
	cs.Owner, cs.Repo, cs.Number = m[1], m[2], n
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
