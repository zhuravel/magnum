package factory

// The end facts of the agents stages (spec 6.2 to 6.4): a stage passes when
// the files its roles were asked for are in the task's directory, newer
// than the stage's start, and hold what the next stage reads, never on an
// agent's "done". CheckEnd checks a fact; the stage runner nudges a role
// once when its file is missing or malformed, then the task waits on you
// (runAgents).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The files of a task's directory the agents write.
const (
	reportFile     = "report.md"
	acceptanceFile = "acceptance.json"
	questionsFile  = "questions.json"
	criticFile     = "critic.md"
	prBodyFile     = "pr-body.md"
	answersFile    = "answers.md"
	ticketFile     = "ticket.md"
)

// findingsFile is an investigator's file: findings-<role>.md, named by the
// role the stage names (a Codex role's Claude fallback writes the same).
func findingsFile(role string) string { return "findings-" + role + ".md" }

// roleQuestionsFile is the questions file of a role other than the judge's
// (the critic's, an investigator's that cannot go on): questions-<role>.json,
// so roles at work at once never write over each other's. The judge's
// stage writes questions.json; the gate takes them all in one batch.
func roleQuestionsFile(role string) string { return "questions-" + role + ".json" }

// ReproductionOptions are the options of the question a bug that did not
// reproduce asks (spec 6.3), in the judge's prompt's words: one of them is
// recommended.
var ReproductionOptions = []string{"give a hint", "take the other cause", "try Docker or a staging", "drop"}

// QuestionFile is one question of a questions file (questions.json, or a
// role's questions-<role>.json), as an agent writes it: the question, the
// line that says why it is asked, a concrete example, two or more options
// (a label and what it means for the example), the index of the recommended
// one (nil: no recommendation) and why, the role that raised it and what it
// rests on. The gate turns each into a registry.Question.
type QuestionFile struct {
	Text        string            `json:"text"`
	Context     string            `json:"context"`
	Example     string            `json:"example"`
	Options     []registry.Option `json:"options"`
	Recommended *int              `json:"recommended"`
	Why         string            `json:"why"`
	RaisedBy    string            `json:"raised_by"`
	Evidence    string            `json:"evidence"`
}

// AcceptanceFile is acceptance.json, the judge's checks that prove the
// task, from the ticket: the writer and QA reuse them.
type AcceptanceFile struct {
	Checks []AcceptanceCheck `json:"checks"`
}

// AcceptanceCheck is one check: its id (A1), the ticket's point it proves,
// the setup, what must hold, and how it is checked (a spec, a QA step).
type AcceptanceCheck struct {
	ID    string `json:"id"`
	Point string `json:"point"`
	Given string `json:"given"`
	Then  string `json:"then"`
	How   string `json:"how"`
}

// endProblem is why an end fact does not hold: the file at fault and what
// is wrong with it, in words a nudge and a wait can carry.
type endProblem struct{ file, why string }

func (p *endProblem) String() string { return p.file + ": " + p.why }

// CheckEnd reports whether end fact fact holds in task directory dir for a
// stage that started at since; why names the file at fault and what is
// wrong ("findings-fx-cause.md: missing"). roles are the stage's roles, as
// the stage names them:
//   - findings: findings-<role>.md of each role, newer than since and not
//     empty, and its questions-<role>.json, when it is there, valid as
//     questions.json is;
//   - report: report.md, newer than since, whose first section is
//     `## Architecture decisions` (its decisions, or none) and whose Helpers
//     and Files sections exist (Files naming at least one file);
//     acceptance.json, newer than since, with at least one check; and
//     questions.json, when it is there, newer than since and holding
//     questions the operator can answer. A report whose Proof says the bug
//     did not reproduce needs the reproduction question among them, with
//     ReproductionOptions and a recommendation;
//   - critic: critic.md, newer than since and not empty, and each role's
//     questions-<role>.json, when it is there, valid as questions.json is;
//   - pr_body: pr-body.md, newer than since and not empty.
//
// The proof and pr facts are checked by the stages that own them, not
// here (not done, saying so).
func CheckEnd(fact string, dir string, since time.Time, roles ...string) (ok bool, why string) {
	if p := checkEnd(fact, dir, since, roles); p != nil {
		return false, p.String()
	}
	return true, ""
}

// checkEnd is CheckEnd's check: nil when the fact holds.
func checkEnd(fact, dir string, since time.Time, roles []string) *endProblem {
	switch fact {
	case config.FactoryEndFindings:
		if len(roles) == 0 {
			return &endProblem{"findings", "the stage names no role"}
		}
		for _, role := range roles {
			if _, p := readFresh(dir, findingsFile(role), since); p != nil {
				return p
			}
			if _, p := readQuestions(dir, roleQuestionsFile(role), since); p != nil {
				return p
			}
		}
		return nil
	case config.FactoryEndReport:
		return checkReport(dir, since)
	case config.FactoryEndCritic:
		if _, p := readFresh(dir, criticFile, since); p != nil {
			return p
		}
		for _, role := range roles {
			if _, p := readQuestions(dir, roleQuestionsFile(role), since); p != nil {
				return p
			}
		}
		return nil
	case config.FactoryEndPRBody:
		_, p := readFresh(dir, prBodyFile, since)
		return p
	case config.FactoryEndProof, config.FactoryEndPR, config.FactoryEndClean:
		return &endProblem{fact, "its own stage checks it, not an agents stage of this build"}
	}
	return &endProblem{fact, "no such end fact"}
}

// readFresh reads name in dir: missing, older than since or empty is the
// problem.
func readFresh(dir, name string, since time.Time) (string, *endProblem) {
	path := filepath.Join(dir, name)
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", &endProblem{name, "missing"}
	case err != nil:
		return "", &endProblem{name, "unreadable: " + err.Error()}
	case fi.IsDir():
		return "", &endProblem{name, "a directory, not a file"}
	case fi.ModTime().Before(since):
		return "", &endProblem{name, "older than this stage (written " + fi.ModTime().Local().Format("15:04:05") +
			", the stage started " + since.Local().Format("15:04:05") + "): write it again"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", &endProblem{name, "unreadable: " + err.Error()}
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", &endProblem{name, "empty"}
	}
	return string(b), nil
}

// checkReport checks the report end fact: report.md, acceptance.json and
// questions.json.
func checkReport(dir string, since time.Time) *endProblem {
	text, p := readFresh(dir, reportFile, since)
	if p != nil {
		return p
	}
	r := parseReport(text)
	if why := r.problem(); why != "" {
		return &endProblem{reportFile, why}
	}
	acc, p := readFresh(dir, acceptanceFile, since)
	if p != nil {
		return p
	}
	if why := acceptanceProblem(acc); why != "" {
		return &endProblem{acceptanceFile, why}
	}
	qs, p := readQuestions(dir, questionsFile, since)
	if p != nil {
		return p
	}
	if r.notReproduced() && !slices.ContainsFunc(qs, isReproductionQuestion) {
		return &endProblem{questionsFile, "the bug did not reproduce (report.md's Proof), so it must ask the reproduction question, " +
			"with the options " + strings.Join(ReproductionOptions, ", ") + " and one of them recommended"}
	}
	return nil
}

// report is report.md as the factory reads it: its `## ` sections in order
// (lowercased headings) with their text, and the files of its Files
// section; lead is text before the first section other than a `# ` title.
type report struct {
	order    []string
	sections map[string]string
	lead     bool
	files    []string
}

// The sections of report.md the factory reads.
const (
	sectionDecisions = "architecture decisions"
	sectionProof     = "proof"
	sectionHelpers   = "helpers"
	sectionFiles     = "files"
)

// parseReport reads report.md's sections.
func parseReport(text string) report {
	r := report{sections: map[string]string{}}
	cur := ""
	var body []string
	flush := func() {
		if cur != "" {
			r.sections[cur] = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = nil
	}
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			cur = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ":")
			if _, dup := r.sections[cur]; !dup {
				r.order = append(r.order, cur)
			}
			continue
		}
		if cur == "" {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "# ") {
				r.lead = true
			}
			continue
		}
		body = append(body, line)
	}
	flush()
	r.files = reportFiles(r.sections[sectionFiles])
	return r
}

// problem is what keeps r from the report end fact, "" for nothing.
func (r report) problem() string {
	switch {
	case len(r.order) == 0 || r.order[0] != sectionDecisions || r.lead:
		return "its first section must be `## Architecture decisions` (the decisions, or none)"
	case r.sections[sectionDecisions] == "":
		return "its Architecture decisions section is empty: list the decisions, or write none"
	}
	if _, ok := r.sections[sectionHelpers]; !ok {
		return "it has no `## Helpers` section (the code the change reuses, with file:line, or none)"
	}
	if r.sections[sectionHelpers] == "" {
		return "its Helpers section is empty: list the code the change reuses, or write none"
	}
	if _, ok := r.sections[sectionFiles]; !ok {
		return "it has no `## Files` section (the files the change touches, one per line)"
	}
	if len(r.files) == 0 {
		return "its Files section names no file (one path per line, `- path`)"
	}
	return ""
}

// notReprodRe and reprodRe are the first line of the Proof section of a bug
// that did not reproduce, "Reproduced: no", and of one that did,
// "Reproduced: yes": in any case and emphasis, after a list marker or not.
var (
	notReprodRe = regexp.MustCompile(`(?i)^[-+*_\s]*reproduced[*_\s]*:[*_\s]*no\b`)
	reprodRe    = regexp.MustCompile(`(?i)^[-+*_\s]*reproduced[*_\s]*:[*_\s]*yes\b`)
)

// proofLine is the first line of the report's Proof section that is not
// blank ("" when there is none).
func (r report) proofLine() string {
	for l := range strings.SplitSeq(r.sections[sectionProof], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// notReproduced reports whether the report's Proof says the bug did not
// reproduce.
func (r report) notReproduced() bool { return notReprodRe.MatchString(r.proofLine()) }

// reproduced reports whether the report's Proof says, in so many words, that
// the bug reproduced: anything else (no Proof, "Reproduced: no", a Proof
// that names no reproduction) is not a reproduction.
func (r report) reproduced() bool { return reprodRe.MatchString(r.proofLine()) }

// lineSuffixRe is a :line (or :line-line) a file path may carry.
var lineSuffixRe = regexp.MustCompile(`:\d+(-\d+)?$`)

// reportFiles are the paths of a Files section: the first word of each
// line, its list marker, quotes, a trailing colon and a :line left out;
// "none" names none.
func reportFiles(section string) []string {
	var out []string
	for line := range strings.SplitSeq(section, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*+ ")
		if i := strings.IndexByte(line, '.'); i > 0 && i < 3 && strings.Trim(line[:i], "0123456789") == "" {
			line = strings.TrimSpace(line[i+1:]) // "1. path"
		}
		fields := strings.Fields(strings.NewReplacer("`", "", `"`, "", "'", "").Replace(line))
		if len(fields) == 0 {
			continue
		}
		f := lineSuffixRe.ReplaceAllString(strings.TrimRight(fields[0], ",;:"), "")
		if f == "" || strings.EqualFold(f, "none") || strings.EqualFold(f, "n/a") {
			continue
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// acceptanceProblem is what keeps acceptance.json's text from the report
// end fact, "" for nothing.
func acceptanceProblem(text string) string {
	var a AcceptanceFile
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		return "not valid JSON (" + err.Error() + `): write {"checks": [{"id": ..., "point": ..., "given": ..., "then": ..., "how": ...}]}`
	}
	if len(a.Checks) == 0 {
		return "it holds no check: write at least one, from the ticket"
	}
	seen := map[string]bool{}
	for i, c := range a.Checks {
		switch {
		case strings.TrimSpace(c.ID) == "":
			return fmt.Sprintf("check %d has no id", i+1)
		case seen[c.ID]:
			return "two checks are " + c.ID
		case strings.TrimSpace(c.Then) == "":
			return "check " + c.ID + " has no then (what must hold)"
		}
		seen[c.ID] = true
	}
	return ""
}

// readQuestions reads the questions file name in dir: none is no problem
// (no question is open); one that is there must be newer than since and
// hold questions the operator can answer.
func readQuestions(dir, name string, since time.Time) ([]QuestionFile, *endProblem) {
	if _, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	text, p := readFresh(dir, name, since)
	if p != nil {
		return nil, p
	}
	var qs []QuestionFile
	if err := json.Unmarshal([]byte(text), &qs); err != nil {
		return nil, &endProblem{name, "not valid JSON (" + err.Error() + "): write a list of questions, `[]` for none"}
	}
	for i, q := range qs {
		if why := questionProblem(q); why != "" {
			return nil, &endProblem{name, fmt.Sprintf("question %d %s", i+1, why)}
		}
	}
	return qs, nil
}

// questionProblem is what keeps q from being asked, "" for nothing.
func questionProblem(q QuestionFile) string {
	if strings.TrimSpace(q.Text) == "" {
		return "has no text"
	}
	if len(q.Options) < 2 {
		return "needs two or more options"
	}
	for _, o := range q.Options {
		if strings.TrimSpace(o.Label) == "" {
			return "has an option without a label"
		}
	}
	if q.Recommended != nil && (*q.Recommended < 0 || *q.Recommended >= len(q.Options)) {
		return fmt.Sprintf("recommends option %d of %d (the index counts from 0; null for no recommendation)", *q.Recommended, len(q.Options))
	}
	return ""
}

// isReproductionQuestion reports whether q is the question of a bug that
// did not reproduce: every one of ReproductionOptions among its options,
// and one option recommended.
func isReproductionQuestion(q QuestionFile) bool {
	if q.Recommended == nil {
		return false
	}
	labels := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		labels = append(labels, strings.TrimRight(strings.ToLower(strings.TrimSpace(o.Label)), "."))
	}
	for _, want := range ReproductionOptions {
		if !slices.Contains(labels, strings.ToLower(want)) {
			return false
		}
	}
	return true
}

// wideFiles is how many files make a plan wide.
const wideFiles = 3

// securityWords are the words of a path (its directories and its file
// name, split at _ . and -) that make it a security path.
var securityWords = map[string]bool{
	"auth": true, "authn": true, "authz": true, "authentication": true, "authenticate": true, "authenticatable": true,
	"authorization": true, "authorize": true, "authorizer": true, "security": true, "session": true, "sessions": true,
	"password": true, "passwords": true, "token": true, "tokens": true, "crypt": true, "crypto": true, "encrypt": true,
	"encryption": true, "encryptor": true, "permission": true, "permissions": true, "secret": true, "secrets": true,
	"oauth": true, "oauth2": true, "csrf": true, "cors": true, "policy": true, "policies": true, "ability": true,
	"abilities": true, "credential": true, "credentials": true, "devise": true, "login": true, "signin": true,
	"sso": true, "saml": true, "jwt": true, "otp": true, "mfa": true, "2fa": true,
}

// wideChange reports whether a plan that touches files is wide (spec 6.2):
// 3 or more files, a file of the database (db/) or a security path. why
// says which, or why not.
func wideChange(files []string) (wide bool, why string) {
	for _, f := range files {
		if strings.HasPrefix(f, "db/") || strings.Contains(f, "/db/") {
			return true, f + " is the database's"
		}
	}
	for _, f := range files {
		words := strings.FieldsFunc(strings.ToLower(f), func(r rune) bool { return strings.ContainsRune("/_.-", r) })
		if slices.ContainsFunc(words, func(w string) bool { return securityWords[w] }) {
			return true, f + " is a security path"
		}
	}
	if len(files) >= wideFiles {
		return true, fmt.Sprintf("the plan touches %d files", len(files))
	}
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	return false, fmt.Sprintf("the plan touches %d %s, none of the database or security", len(files), noun)
}
