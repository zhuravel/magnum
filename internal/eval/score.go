package eval

import (
	"regexp"
	"slices"
)

// lineSlack is how many lines a finding may be off a defect's range and still count.
const lineSlack = 3

var (
	// descriptionLine is a review body's line on a description claim proved false without impact
	// (`Description: ✗ <claim>: <why>`, the skill's section 2): it reports no defect.
	descriptionLine = regexp.MustCompile(`(?m)^[ \t]*(?:[-*][ \t]+)?Description:[ \t]*[✗✘❌].*$`)
	// checksBlock is a review body's collapsed Checks block: the commands that ran, no defects.
	checksBlock = regexp.MustCompile(`(?is)<details>\s*<summary>\s*Checks\b.*?</details>`)
)

// bodyFindings is the part of a review body that reports defects: the body without its
// `Description: ✗` lines and its Checks block, so that only the finding list and the nearby block
// are matched.
func bodyFindings(body string) string {
	return checksBlock.ReplaceAllString(descriptionLine.ReplaceAllString(body, ""), "")
}

// DefectResult is the outcome for one defect of a case.
type DefectResult struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Want       string `json:"want,omitempty"` // the defect's severity
	Found      bool   `json:"found"`
	By         []int  `json:"by,omitempty"`       // indexes into Result.Findings that matched
	Severity   string `json:"severity,omitempty"` // the strongest among matching inline findings, "" if none
	SeverityOK bool   `json:"severity_ok"`        // found, and at Want or stronger (or no Want)
}

// Score is the outcome for one case.
type Score struct {
	Case            string         `json:"case"`
	Status          string         `json:"status,omitempty"`
	Event           string         `json:"event,omitempty"`
	Defects         []DefectResult `json:"defects"`
	Found           int            `json:"found"`
	Total           int            `json:"total"`
	Noise           int            `json:"noise"`               // inline findings that matched no defect, simplifications aside
	Simplifications int            `json:"simplifications"`     // optional simplification suggestions, neither defect nor noise
	Unmatched       []Finding      `json:"unmatched,omitempty"` // the noise findings
}

// SeverityOK counts the found defects that were reported at the wanted severity or stronger.
func (s Score) SeverityOK() int {
	n := 0
	for _, d := range s.Defects {
		if d.SeverityOK {
			n++
		}
	}
	return n
}

// ScoreCase tells, for every defect of c, whether the findings of r report it. An inline finding
// matches a defect when its path matches one of the defect's paths (any when none), its line range
// comes within three lines of the defect's lines (when set) and its body matches one of the defect's
// regexps (when set). A review body matches only a defect with body = true, by its regexps alone,
// and only with its finding list and nearby block (bodyFindings).
// Simplification suggestions match nothing and are not noise. One finding may match several defects.
func ScoreCase(c Case, r Result) Score {
	s := Score{Case: c.Name, Status: r.Status, Event: r.Event, Total: len(c.Defects)}
	used := make([]bool, len(r.Findings))
	for _, d := range c.Defects {
		res := d.regexps()
		dr := DefectResult{ID: d.ID, Title: d.Title, Want: d.Severity}
		for i, f := range r.Findings {
			if !d.matches(res, f) {
				continue
			}
			used[i] = true
			dr.By = append(dr.By, i)
			if !f.InBody && f.Severity != "" && (dr.Severity == "" || f.Severity < dr.Severity) {
				dr.Severity = f.Severity
			}
		}
		dr.Found = len(dr.By) > 0
		dr.SeverityOK = dr.Found && (dr.Want == "" || dr.Severity != "" && dr.Severity <= dr.Want)
		if dr.Found {
			s.Found++
		}
		s.Defects = append(s.Defects, dr)
	}
	for i, f := range r.Findings {
		switch {
		case f.Simplification:
			s.Simplifications++
		case f.InBody, used[i]:
		default:
			s.Noise++
			s.Unmatched = append(s.Unmatched, f)
		}
	}
	return s
}

func (d *Defect) matches(res []*regexp.Regexp, f Finding) bool {
	if f.Simplification || (len(d.Paths) == 0 && len(d.Match) == 0) {
		return false
	}
	if f.InBody {
		return d.Body && anyMatch(res, bodyFindings(f.Body))
	}
	if len(d.Paths) > 0 && !slices.ContainsFunc(d.Paths, func(g string) bool { return matchGlob(g, f.Path) }) {
		return false
	}
	if len(d.Lines) == 2 && !overlaps(f, d.Lines[0]-lineSlack, d.Lines[1]+lineSlack) {
		return false
	}
	return len(d.Match) == 0 || anyMatch(res, f.Body)
}

// overlaps reports whether the lines of the finding share one with [from, to]. A finding without
// a line (a comment on a whole file) overlaps nothing.
func overlaps(f Finding, from, to int) bool {
	if f.Line <= 0 {
		return false
	}
	lo := f.Line
	if f.StartLine > 0 && f.StartLine < f.Line {
		lo = f.StartLine
	}
	return lo <= to && f.Line >= from
}

func anyMatch(res []*regexp.Regexp, body string) bool {
	return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(body) })
}
