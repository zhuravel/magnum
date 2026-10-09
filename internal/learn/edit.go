package learn

// The skill editor's inputs and answer (DECISIONS "The editor proposes skill
// and prompt changes on a branch"). An editor run gives a pane agent, in a
// git worktree of the checkout, the general misses and the general declined
// findings as inputs.json, and the sentences the judge skill must keep with
// its size cap as pins.json; the agent edits the skill and the judge and
// reviewer prompts and writes changes.json, accounting for every file and
// input and drafting the DECISIONS entry. ValidateEdit is magnum's check of
// what it wrote; its problems name files, ids and rules, never text from a
// pull request.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// The files of an editor run.
const (
	// InputsFile and PinsFile are in the run directory, which the agent
	// reads; ChangesFile is the answer it writes at the root of its
	// worktree (magnum keeps a copy in the run directory).
	InputsFile  = "inputs.json"
	PinsFile    = "pins.json"
	ChangesFile = "changes.json"
	// SkillFile is the judge skill, relative to a checkout.
	SkillFile = "skills/magnum-review/SKILL.md"
	// PinsSource is the pins file the skill tests generate, relative to a
	// checkout (FormatPins).
	PinsSource = "internal/agents/testdata/skill_pins.txt"
	// DecisionsFile is where magnum appends the edit's DECISIONS entry.
	DecisionsFile = "docs/DECISIONS.md"
)

// Limits of the editor's answer.
const (
	DecisionMaxRunes = 1200 // the DECISIONS entry
	EditReasonRunes  = 300  // a one-line reason
	// ChangesMaxBytes bounds changes.json.
	ChangesMaxBytes = 256 << 10
)

// What changes.json says of a file and of an input.
const (
	FileEdited    = "edited"
	FileUnchanged = "unchanged"
	InputNoted    = store.MissNoted
	InputSkipped  = store.MissSkipped
)

// EditAllowedGlobs are the files the editor may change, relative to the
// checkout: the judge skill, the judge prompts and the Claude reviewers'
// prompts. Nothing else: no Go code, no test, no configuration, not the
// retro's or the curator's prompts.
var EditAllowedGlobs = []string{"skills/magnum-review/SKILL.md", "prompts/judge-*.md", "prompts/claude-*.md"}

// AllowedEditFile reports whether p (slash-separated, relative to the
// checkout) is one of EditAllowedGlobs.
func AllowedEditFile(p string) bool {
	return slices.ContainsFunc(EditAllowedGlobs, func(g string) bool {
		ok, err := path.Match(g, p)
		return err == nil && ok
	})
}

// EditInputs is inputs.json: the inputs to account for, the operator's
// reasons for rejecting earlier proposals, the skill's size and cap and the
// files the editor may change.
type EditInputs struct {
	Misses     []EditMiss      `json:"misses"`
	Declines   []EditDecline   `json:"declines"`
	Rejections []EditRejection `json:"rejections"`
	SkillBytes int             `json:"skill_bytes"`
	SkillCap   int             `json:"skill_cap"`
	Files      []string        `json:"files"`
}

// EditMiss is a general miss: what another reviewer caught that magnum did
// not.
type EditMiss struct {
	ID       string   `json:"id"` // "m<id>"
	Severity string   `json:"severity,omitempty"`
	Title    string   `json:"title,omitempty"`
	Lesson   string   `json:"lesson,omitempty"`
	Match    []string `json:"match"`
}

// EditDecline is a general finding of magnum's an author declined: wrong,
// or ranked too high.
type EditDecline struct {
	ID       string   `json:"id"` // "d<id>"
	Priority string   `json:"priority,omitempty"`
	Verdict  string   `json:"verdict"`
	Title    string   `json:"title,omitempty"`
	Lesson   string   `json:"lesson,omitempty"`
	Match    []string `json:"match"`
}

// EditRejection is the operator's reason for rejecting an earlier
// proposal: of one input (Input, its id) or of the proposal (Input "").
type EditRejection struct {
	Input  string `json:"input,omitempty"`
	Reason string `json:"reason"`
}

// MissInputID is a miss's id in inputs.json.
func MissInputID(id int64) string { return "m" + strconv.FormatInt(id, 10) }

// DeclineInputID is a decline's id in inputs.json.
func DeclineInputID(id int64) string { return "d" + strconv.FormatInt(id, 10) }

// ParseInputID reads an id of inputs.json: its kind
// (store.LearnInputMiss or store.LearnInputDecline) and row id.
func ParseInputID(s string) (kind string, id int64, ok bool) {
	if len(s) < 2 {
		return "", 0, false
	}
	switch s[0] {
	case 'm':
		kind = store.LearnInputMiss
	case 'd':
		kind = store.LearnInputDecline
	default:
		return "", 0, false
	}
	n, err := strconv.ParseInt(s[1:], 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s[1:] {
		return "", 0, false
	}
	return kind, n, true
}

// IDs lists every input's id, the misses first.
func (in EditInputs) IDs() []string {
	out := make([]string, 0, len(in.Misses)+len(in.Declines))
	for _, m := range in.Misses {
		out = append(out, m.ID)
	}
	for _, d := range in.Declines {
		out = append(out, d.ID)
	}
	return out
}

// Changes is changes.json: what the editor did with every file it may
// change and every input it was given, and the DECISIONS entry it drafts.
type Changes struct {
	Files    []FileChange  `json:"files"`
	Inputs   []InputChange `json:"inputs"`
	Decision string        `json:"decision"`
}

// FileChange is one file of Changes: edited or unchanged, and why.
type FileChange struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// InputChange is one input of Changes: noted, with where the change covers
// it, or skipped, with a one-line reason.
type InputChange struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Where  string `json:"where,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ParseChanges decodes changes.json strictly (an unknown field is an
// error).
func ParseChanges(b []byte) (Changes, error) {
	var c Changes
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Changes{}, fmt.Errorf("%s is not valid: %w", ChangesFile, err)
	}
	if dec.More() {
		return Changes{}, fmt.Errorf("%s holds more than one JSON value", ChangesFile)
	}
	return c, nil
}

// EditCheck is what ValidateEdit compares an answer with.
type EditCheck struct {
	Inputs EditInputs
	// Changed are the paths the worktree's status lists (relative to it,
	// slash-separated), changes.json and magnum's own files left out.
	Changed []string
	// Allowed are the globs a changed file must match (nil =
	// EditAllowedGlobs).
	Allowed []string
	// Skill is the edited SKILL.md, Pins the sentences it must keep.
	Skill []byte
	Pins  []string
	// Diff is the worktree's diff against the base (its added lines are
	// checked), Decision the DECISIONS entry.
	Diff     string
	Decision string
	// Leaks is what names one pull request (notes.CheckLeaks): the
	// branches of the inputs' pull requests. People, Own and Repos are
	// ScrubLesson's for the decision: the inputs' authors and reviewers,
	// magnum's logins, the inputs' repositories' owners and names.
	Leaks              notes.Check
	People, Own, Repos []string
}

// ValidateEdit returns what is wrong with an editor's answer (nil = valid),
// in words fit for a nudge and the registry: every input of c is accounted
// for exactly once, noted with where or skipped with a one-line reason of
// at most EditReasonRunes; every changed file is an allowed one and listed
// edited, and a file listed edited changed; the skill is within its cap and
// keeps every pin; the diff's added lines and the decision pass
// notes.CheckLeaks; and the decision is 1 to DecisionMaxRunes runes that
// ScrubLesson keeps as a general lesson (no pull request number, link,
// login or repository).
func ValidateEdit(c EditCheck, changes Changes) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	given := c.Inputs.IDs()
	seen := map[string]bool{}
	for _, in := range changes.Inputs {
		id := strings.TrimSpace(in.ID)
		switch {
		case !slices.Contains(given, id):
			add("input %q in %s was not in %s", clipName(id), ChangesFile, InputsFile)
			continue
		case seen[id]:
			add("input %s is in %s's inputs twice", id, ChangesFile)
			continue
		}
		seen[id] = true
		reason := strings.TrimSpace(in.Reason)
		switch in.Action {
		case InputNoted:
			if strings.TrimSpace(in.Where) == "" {
				add("input %s in %s is noted without where the change covers it", id, ChangesFile)
			}
		case InputSkipped:
			if reason == "" {
				add("input %s in %s is skipped without a reason", id, ChangesFile)
			}
		default:
			add("input %s in %s: action %q is not one of %s, %s", id, ChangesFile, clipName(in.Action), InputNoted, InputSkipped)
		}
		problems = append(problems, oneLineProblems(fmt.Sprintf("input %s in %s", id, ChangesFile), reason, in.Where)...)
	}
	for _, id := range given {
		if !seen[id] {
			add("input %s of %s is not in %s's inputs (noted with where, or skipped with a reason)", id, InputsFile, ChangesFile)
		}
	}

	allowed := c.Allowed
	if allowed == nil {
		allowed = EditAllowedGlobs
	}
	match := func(p string) bool {
		return slices.ContainsFunc(allowed, func(g string) bool { ok, err := path.Match(g, p); return err == nil && ok })
	}
	listed := map[string]FileChange{}
	for _, f := range changes.Files {
		name := strings.TrimSpace(f.Name)
		if _, dup := listed[name]; dup {
			add("file %s is in %s's files twice", clipName(name), ChangesFile)
		}
		listed[name] = f
		switch f.Action {
		case FileEdited, FileUnchanged:
		default:
			add("file %s in %s: action %q is not one of %s, %s", clipName(name), ChangesFile, clipName(f.Action), FileEdited, FileUnchanged)
		}
		reason := strings.TrimSpace(f.Reason)
		if reason == "" {
			add("file %s in %s has no reason", clipName(name), ChangesFile)
		}
		problems = append(problems, oneLineProblems(fmt.Sprintf("file %s in %s", clipName(name), ChangesFile), reason)...)
		if f.Action == FileEdited && !slices.Contains(c.Changed, name) {
			add("file %s is edited in %s but did not change", clipName(name), ChangesFile)
		}
	}
	for _, p := range c.Changed {
		switch f, ok := listed[p]; {
		case !match(p):
			add("%s changed, but only %s may: undo it", clipName(p), strings.Join(allowed, ", "))
		case !ok:
			add("%s changed but is not in %s's files", p, ChangesFile)
		case f.Action != FileEdited:
			add("%s changed but is %s in %s", p, clipName(f.Action), ChangesFile)
		}
	}
	if !slices.ContainsFunc(c.Changed, match) {
		add("no allowed file changed: the proposal changes nothing")
	}

	if c.Inputs.SkillCap > 0 && len(c.Skill) > c.Inputs.SkillCap {
		add("%s is %d bytes, over its cap of %d: shorten it", SkillFile, len(c.Skill), c.Inputs.SkillCap)
	}
	skill := string(c.Skill)
	for _, p := range c.Pins {
		if !strings.Contains(skill, p) {
			add("%s lost a pinned sentence: %q", SkillFile, clipRunes(p, 120))
		}
	}

	for _, leak := range notes.CheckLeaks(addedLines(c.Diff), c.Leaks) {
		add("the change %s", leak)
	}
	decision := strings.TrimSpace(changes.Decision)
	if c.Decision != "" {
		decision = strings.TrimSpace(c.Decision)
	}
	switch n := utf8.RuneCountInString(decision); {
	case n == 0:
		add("%s has no decision (the DECISIONS entry)", ChangesFile)
	case n > DecisionMaxRunes:
		add("the decision in %s is %d characters, over %d", ChangesFile, n, DecisionMaxRunes)
	}
	for _, leak := range notes.CheckLeaks(decision, c.Leaks) {
		add("the decision in %s %s", ChangesFile, leak)
	}
	if decision != "" {
		if _, why := ScrubLesson(decision, store.MissScopeGeneral, c.People, c.Own, c.Repos); why != "" {
			add("the decision in %s %s", ChangesFile, scrubProblem(why))
		}
	}
	if strings.ContainsFunc(decision, func(r rune) bool { return r < ' ' && r != '\n' }) {
		add("the decision in %s holds control characters", ChangesFile)
	}
	return problems
}

// scrubProblem words a ScrubLesson reason for a problem.
func scrubProblem(why string) string {
	switch why {
	case LessonIssueRef:
		return "names a pull request or issue number"
	case LessonURL:
		return "holds a link"
	case LessonLogin:
		return "names a person's login"
	case LessonNamesRepo:
		return "names a repository or its owner"
	}
	return "is not a general rule (" + why + ")"
}

// oneLineProblems checks that each of texts is one line of at most
// EditReasonRunes runes.
func oneLineProblems(what string, texts ...string) []string {
	var out []string
	for _, s := range texts {
		switch {
		case strings.ContainsAny(s, "\r\n"):
			out = append(out, what+": the reason is not one line")
		case utf8.RuneCountInString(s) > EditReasonRunes:
			out = append(out, fmt.Sprintf("%s: the reason is longer than %d characters", what, EditReasonRunes))
		}
	}
	return out
}

// clipName keeps a name the agent wrote short and printable in a problem.
func clipName(s string) string { return clipRunes(notes.Printable(s), 80) }

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// addedLines is the text of the lines a unified diff adds (not its
// "+++" headers), one per line.
func addedLines(diff string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			b.WriteString(line[1:])
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// DecisionWidth is the design log's line width.
const DecisionWidth = 104

// Headline splits a decision into its first sentence (up to the first ".",
// "!" or "?" that ends the text or is followed by a space) and the rest.
func Headline(decision string) (headline, rest string) {
	text := strings.Join(strings.Fields(decision), " ")
	for i := range len(text) {
		if c := text[i]; (c == '.' || c == '!' || c == '?') && (i+1 == len(text) || text[i+1] == ' ') {
			return text[:i+1], strings.TrimSpace(text[i+1:])
		}
	}
	return text, ""
}

// DecisionEntry is the design log's entry for an editor's decision: its
// headline in bold without the final stop, the day and the inputs the
// proposal answers in parentheses, then the rest, one paragraph wrapped at
// DecisionWidth runes with two-space continuation lines, as every entry of
// docs/DECISIONS.md.
func DecisionEntry(decision, day string, inputs []string) string {
	head, rest := Headline(decision)
	text := "**" + strings.TrimRight(head, ".!?") + "** (" + day + ", the skill editor's proposal"
	if len(inputs) > 0 {
		text += " for " + strings.Join(inputs, ", ")
	}
	text += ")."
	if rest != "" {
		text += " " + rest
	}
	return wrap(text, DecisionWidth, "- ", "  ")
}

// AppendDecision appends entry to the design log doc, after a line break
// when doc does not end with one.
func AppendDecision(doc []byte, entry string) []byte {
	out := slices.Clone(doc)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, entry...)
	if !strings.HasSuffix(entry, "\n") {
		out = append(out, '\n')
	}
	return out
}

// wrap fills text's words into lines of at most width runes (a longer word
// stays whole on its own line), the first after first and the others after
// next.
func wrap(text string, width int, first, next string) string {
	var b strings.Builder
	line, n := first, utf8.RuneCountInString(first)
	empty := true
	for w := range strings.FieldsSeq(text) {
		wn := utf8.RuneCountInString(w)
		if !empty && n+1+wn > width {
			b.WriteString(line + "\n")
			line, n, empty = next, utf8.RuneCountInString(next), true
		}
		if !empty {
			line += " "
			n++
		}
		line += w
		n += wn
		empty = false
	}
	b.WriteString(line + "\n")
	return b.String()
}

// pinsCap starts the pins file's cap line.
const pinsCap = "# cap: "

// FormatPins renders the pins file (PinsSource): a comment, the skill's
// size cap, then one pinned sentence per line, sorted.
func FormatPins(pins []string, limit int) []byte {
	var b bytes.Buffer
	b.WriteString("# The sentences skills/magnum-review/SKILL.md must keep, one per line, and the size it must stay within.\n")
	b.WriteString("# Generated by internal/agents' skill tests (go test ./internal/agents -update); the skill editor reads it.\n")
	fmt.Fprintf(&b, "%s%d\n", pinsCap, limit)
	for _, p := range slices.Sorted(slices.Values(pins)) {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ParsePins reads a pins file: the pins and the skill's cap. A file
// without a cap line is an error.
func ParsePins(b []byte) ([]string, int, error) {
	pins := []string{}
	limit := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, pinsCap):
			n, err := strconv.Atoi(strings.TrimPrefix(line, pinsCap))
			if err != nil || n <= 0 {
				return nil, 0, fmt.Errorf("pins: bad cap line %q", line)
			}
			limit = n
		case line == "", strings.HasPrefix(line, "#"):
		default:
			pins = append(pins, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("pins: %w", err)
	}
	if limit == 0 {
		return nil, 0, errors.New("pins: no cap line")
	}
	return pins, limit, nil
}
