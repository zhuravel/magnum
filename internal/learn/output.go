package learn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// Files of a pull request's retro directory.
const (
	CandidatesFile = "candidates.json" // what Build kept, for the classifier
	OutputFile     = "retro.json"      // the classifier's answer (ParseOutput)
	FilesDir       = "files"           // the commented files: files/<sha12>/<path>
)

// Limits of the classifier's answer.
const (
	TitleMax   = 80  // runes
	MatchMax   = 3   // patterns per miss
	PatternMax = 120 // bytes per pattern
)

// Candidates is the candidates file.
type Candidates struct {
	PR           string      `json:"pr"`            // the pull request's URL
	ReviewedSHAs []string    `json:"reviewed_shas"` // what magnum reviewed, oldest first
	FilesDir     string      `json:"files_dir"`     // relative to the file's directory
	Candidates   []Candidate `json:"candidates"`
}

// ReadCandidates reads a candidates file.
func ReadCandidates(p string) (Candidates, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Candidates{}, err
	}
	var c Candidates
	if err := json.Unmarshal(b, &c); err != nil {
		return Candidates{}, fmt.Errorf("learn: %s: %w", p, err)
	}
	return c, nil
}

// FilePath is where the copy of path at sha lives under a retro
// directory's files: files/<sha12>/<path>, slash-separated.
func FilePath(sha, p string) string { return path.Join(FilesDir, short(sha), p) }

// Item is the classifier's verdict on one candidate.
type Item struct {
	ID       string   `json:"id"`
	Class    string   `json:"class"`
	Severity string   `json:"severity,omitempty"`
	Title    string   `json:"title,omitempty"`
	Lesson   string   `json:"lesson,omitempty"`
	Scope    string   `json:"scope,omitempty"`
	Lines    []int    `json:"lines,omitempty"`
	Match    []string `json:"match,omitempty"`
}

// Output is the classifier's answer file.
type Output struct {
	Items []Item `json:"items"`
}

// ParseOutput reads the classifier's answer for cands: one item per
// candidate, no other id, a known class; a miss also has a severity (P0 to
// P3), a title of at most TitleMax runes, a lesson, a scope (repo or
// general), lines [from, to] with 1 <= from <= to and one to MatchMax
// patterns of at most PatternMax bytes that compile as case-insensitive Go
// regular expressions. Only a miss keeps those fields: the other classes
// keep their id and class. The error names ids and fields, never the
// classifier's text, so it can go back into a prompt.
func ParseOutput(data []byte, cands []Candidate) (map[string]Item, error) {
	var out Output
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("not a JSON object with an \"items\" list: %s", jsonProblem(err))
	}
	want := map[string]bool{}
	for _, c := range cands {
		want[c.ID] = true
	}
	items := map[string]Item{}
	seen := map[string]bool{}
	var errs []error
	for i, it := range out.Items {
		id := strings.TrimSpace(it.ID)
		switch {
		case !want[id]:
			errs = append(errs, fmt.Errorf("item %d: its id is no candidate's", i+1))
			continue
		case seen[id]:
			errs = append(errs, fmt.Errorf("item %d: a second item for %s", i+1, id))
			continue
		}
		seen[id] = true
		it.ID = id
		v, err := checkItem(it)
		if err != nil {
			errs = append(errs, fmt.Errorf("item %s: %w", id, err))
			continue
		}
		items[id] = v
	}
	for _, c := range cands {
		if !seen[c.ID] {
			errs = append(errs, fmt.Errorf("no item for %s", c.ID))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return items, nil
}

// checkItem validates one item and returns what is kept of it.
func checkItem(it Item) (Item, error) {
	switch it.Class {
	case store.MissNotIssue, store.MissStyle, store.MissOutside:
		return Item{ID: it.ID, Class: it.Class}, nil
	case store.MissMiss:
	default:
		return Item{}, fmt.Errorf("class must be miss, not_issue, style or outside")
	}
	var errs []error
	switch it.Severity {
	case "P0", "P1", "P2", "P3":
	default:
		errs = append(errs, errors.New("a miss needs a severity P0, P1, P2 or P3"))
	}
	it.Title = strings.TrimSpace(it.Title)
	if it.Title == "" || utf8.RuneCountInString(it.Title) > TitleMax {
		errs = append(errs, fmt.Errorf("a miss needs a title of 1 to %d characters", TitleMax))
	}
	it.Lesson = strings.TrimSpace(it.Lesson)
	if it.Lesson == "" {
		errs = append(errs, errors.New("a miss needs a lesson"))
	}
	if it.Scope != store.MissScopeRepo && it.Scope != store.MissScopeGeneral {
		errs = append(errs, errors.New("a miss needs a scope, repo or general"))
	}
	if len(it.Lines) != 2 || it.Lines[0] < 1 || it.Lines[0] > it.Lines[1] {
		errs = append(errs, errors.New("a miss needs lines [from, to] with 1 <= from <= to"))
	}
	if n := len(it.Match); n < 1 || n > MatchMax {
		errs = append(errs, fmt.Errorf("a miss needs 1 to %d match patterns", MatchMax))
	}
	for j, m := range it.Match {
		if len(m) == 0 || len(m) > PatternMax {
			errs = append(errs, fmt.Errorf("match %d must be 1 to %d characters", j+1, PatternMax))
		} else if _, err := regexp.Compile("(?i)" + m); err != nil {
			errs = append(errs, fmt.Errorf("match %d does not compile as a Go regular expression", j+1))
		}
	}
	if len(errs) > 0 {
		return Item{}, errors.Join(errs...)
	}
	return it, nil
}

// jsonProblem is a decode error without the input's text.
func jsonProblem(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("syntax error at byte %d", se.Offset)
	case errors.As(err, &te):
		return fmt.Sprintf("%s has the wrong type", te.Field)
	}
	return "unreadable"
}

// Why ScrubLesson dropped a lesson (the reason of a lesson_rejected event).
const (
	LessonIssueRef  = "issue_ref"  // a pull request or issue number (#123)
	LessonURL       = "url"        // a link
	LessonLogin     = "login"      // a login of the pull request, with or without "@"
	LessonNamesRepo = "names_repo" // the repository's owner or name, in a general lesson
)

var (
	refRe = regexp.MustCompile(`#\d+`)
	urlRe = regexp.MustCompile(`(?i)\b(?:https?://|www\.)`)
)

// ScrubLesson returns lesson when it teaches without retelling the pull
// request, else "" and why (Lesson*): a pull request or issue reference
// (#123), a URL, one of people (the author's and the reviewers' logins),
// with or without "@", or an @mention of one of own (magnum's logins, whose
// bare name is often the organization's); a "[bot]" suffix is ignored, and
// "@" before anything else is code (@property, @Transactional). A lesson of
// scope general may not name the repository's owner or name either (repo),
// backticks or not, since general lessons can reach the public judge
// skill; a repo lesson goes to that repository's own notes and may. Words
// match whole and case-insensitively.
func ScrubLesson(lesson, scope string, people, own, repo []string) (string, string) {
	low := strings.ToLower(lesson)
	has := func(words []string, prefix string) bool {
		for _, w := range words {
			w = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(w), "[bot]"))
			if w != "" && containsWord(low, prefix+w) {
				return true
			}
		}
		return false
	}
	switch {
	case refRe.MatchString(lesson):
		return "", LessonIssueRef
	case urlRe.MatchString(lesson):
		return "", LessonURL
	case has(people, ""), has(own, "@"):
		return "", LessonLogin
	case scope != store.MissScopeRepo && has(repo, ""):
		return "", LessonNamesRepo
	}
	return lesson, ""
}

// containsWord reports whether s holds w with no letter or digit right
// before or after it.
func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(w)
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if !wordRune(before) && !wordRune(after) {
			return true
		}
		i = start + 1
	}
}

func wordRune(r rune) bool { return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// WriteFileAtomic writes data to name inside root: a temporary file next to
// it, then a rename, so a reader never sees half a file. name comes from
// GitHub (a commented path), and root confines it to the retro directory.
func WriteFileAtomic(root *os.Root, name string, data []byte) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return fsx.WriteFileAtomicIn(root, name, data, 0o600)
}
