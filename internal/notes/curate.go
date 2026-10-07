package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
)

// A curation works in a scratch directory under the notes root
// (Repo.Curate()/<id>): magnum copies the notes and the harness there, and the
// curator writes its proposal next to them. The live notes are never
// touched; magnum stores a valid proposal in the registry, where the operator
// reviews it (`magnum notes <repo> --review`).
const (
	scratchCurrent        = "current.md"   // the notes now (read only)
	scratchCurrentHarness = "current"      // the harness now (read only)
	scratchProposal       = "proposal.md"  // the proposed notes
	scratchHarness        = "harness"      // the proposed harness, a copy of the current one at first
	scratchChanges        = "changes.json" // what became of every section and file, and why
	scratchUsage          = "usage.json"   // the limits, the sizes and the usage data
	scratchMisses         = "misses.json"  // the retro's misses of the repository, when there are any
	scratchSuperseded     = "superseded"   // the stale proposal this curation follows up on, when there is one
)

// Scratch is one curation's directory.
type Scratch struct{ Dir string }

func (s Scratch) Current() string        { return filepath.Join(s.Dir, scratchCurrent) }
func (s Scratch) CurrentHarness() string { return filepath.Join(s.Dir, scratchCurrentHarness) }
func (s Scratch) Proposal() string       { return filepath.Join(s.Dir, scratchProposal) }
func (s Scratch) Harness() string        { return filepath.Join(s.Dir, scratchHarness) }
func (s Scratch) Changes() string        { return filepath.Join(s.Dir, scratchChanges) }
func (s Scratch) Usage() string          { return filepath.Join(s.Dir, scratchUsage) }
func (s Scratch) Misses() string         { return filepath.Join(s.Dir, scratchMisses) }
func (s Scratch) Superseded() string     { return filepath.Join(s.Dir, scratchSuperseded) }

// PrepareScratch makes a new scratch directory dir holding base (the notes
// as current.md, the harness as current/ and as the harness/ the curator
// edits) and usage as usage.json.
func PrepareScratch(dir string, base State, usage []byte) (Scratch, error) {
	s := Scratch{Dir: dir}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return s, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return s, err
	}
	if err := os.WriteFile(s.Current(), base.Notes, 0o600); err != nil {
		return s, err
	}
	for _, d := range []string{s.CurrentHarness(), s.Harness()} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return s, err
		}
		if err := writeTree(d, base.Files); err != nil {
			return s, err
		}
	}
	return s, fsx.WriteFileAtomic(s.Usage(), usage, 0o600)
}

// WriteSuperseded writes the stale proposal a curation follows up on into
// s's superseded/ directory, for the curator to read: its notes as
// notes.md, its harness under harness/ and its changes.json, with a reason
// for every section and file it kept, merged or removed. The notes changed
// since that proposal was made, so it was superseded rather than applied;
// its work is not lost.
func WriteSuperseded(s Scratch, proposed State, changes []byte) error {
	dir := s.Superseded()
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), proposed.Notes, 0o600); err != nil {
		return err
	}
	harness := filepath.Join(dir, scratchHarness)
	if err := os.Mkdir(harness, 0o700); err != nil {
		return err
	}
	if err := writeTree(harness, proposed.Files); err != nil {
		return err
	}
	if len(changes) == 0 {
		changes = []byte("{}")
	}
	return os.WriteFile(filepath.Join(dir, scratchChanges), changes, 0o600)
}

// Changes is the curator's changes.json: what became of every notes section
// (by its "## " heading) and every harness file, each with a one-line reason,
// and of every miss the curation was given (misses.json).
type Changes struct {
	Sections []Change     `json:"sections"`
	Files    []Change     `json:"files"`
	Misses   []MissChange `json:"misses,omitempty"`
}

// MissChange is what a curation did with one miss it was given: noted, with
// the "## " section of the proposal that covers it now, or skipped, with a
// one-line reason.
type MissChange struct {
	ID      int64  `json:"id"`
	Action  string `json:"action"`
	Section string `json:"section,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// The actions of a MissChange.
const (
	MissNoted   = "noted"
	MissSkipped = "skipped"
)

// Change is one item of Changes.
type Change struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Into   string `json:"into,omitempty"` // merged: where it went
	Reason string `json:"reason"`
}

// The actions of a Change.
const (
	ActionKept    = "kept"
	ActionAdded   = "added"
	ActionMerged  = "merged"
	ActionRemoved = "removed" // a section
	ActionDeleted = "deleted" // a harness file
)

// maxReasonRunes bounds a change's one-line reason.
const maxReasonRunes = 300

// Proposal is what a curator wrote: the proposed notes and harness, and its
// changes.json (raw and parsed).
type Proposal struct {
	State       State
	Changes     Changes
	ChangesJSON []byte
}

// ReadProposal reads what the curator wrote in s, regular files of its
// directory only (readRegular). Problems name what is missing or
// unreadable, in words fit for a nudge (paths, never content).
func ReadProposal(s Scratch) (Proposal, []string) {
	var p Proposal
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return p, []string{"the curation's directory cannot be read: " + err.Error()}
	}
	defer root.Close()
	var problems []string
	text, err := readRegular(root, scratchProposal)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		problems = append(problems, scratchProposal+" was not written")
	case errors.Is(err, errNotRegular):
		problems = append(problems, scratchProposal+" is "+errNotRegular.Error())
	case err != nil:
		problems = append(problems, scratchProposal+" cannot be read: "+err.Error())
	case len(strings.TrimSpace(string(text))) == 0:
		problems = append(problems, scratchProposal+" is empty")
	default:
		p.State.Exists, p.State.Notes = true, text
	}
	files, bad := readTree(s.Harness())
	problems = append(problems, bad...)
	p.State.Files = files
	b, err := readRegular(root, scratchChanges)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		problems = append(problems, scratchChanges+" was not written")
	case errors.Is(err, errNotRegular):
		problems = append(problems, scratchChanges+" is "+errNotRegular.Error())
	case err != nil:
		problems = append(problems, scratchChanges+" cannot be read: "+err.Error())
	default:
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p.Changes); err != nil {
			problems = append(problems, scratchChanges+" is not valid: "+err.Error())
		} else {
			p.ChangesJSON = b
		}
	}
	return p, problems
}

// errNotRegular is readRegular's error for a symbolic link or a special
// file.
var errNotRegular = errors.New("not a regular file (no symbolic links or special files)")

// readRegular reads name, a regular file of root's directory. The curator
// reads misses that quote other people's comments, so what it leaves is
// not trusted: a symbolic link (even to a file inside the directory) or a
// special file is errNotRegular and nothing it points to is read, and
// root keeps every path inside the directory.
func readRegular(root *os.Root, name string) ([]byte, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !os.SameFile(fi, st) {
		return nil, errNotRegular // replaced since it was checked
	}
	return io.ReadAll(f)
}

// readTree reads every file under dir as harness blobs; symbolic links and
// special files are problems (a proposal's harness is plain files).
func readTree(dir string) ([]Blob, []string) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{scratchHarness + "/ cannot be read: " + err.Error()}
	}
	defer root.Close()
	var out []Blob
	var problems []string
	total := 0
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			problems = append(problems, fmt.Sprintf("%s/%s is not a regular file (no symbolic links or special files)", scratchHarness, p))
			return nil
		case !cleanName(p):
			problems = append(problems, fmt.Sprintf("%s/%s has a name that is not a plain relative path", scratchHarness, p))
			return nil
		}
		body, err := root.ReadFile(p)
		if err != nil {
			return err
		}
		if total += len(body); len(body) > MaxFileBytes || total > MaxStateBytes || len(out) >= maxListed {
			return ErrTooLarge
		}
		out = append(out, Blob{Path: p, SHA256: TextSHA(body), Body: body})
		return nil
	})
	if err != nil {
		problems = append(problems, scratchHarness+"/ cannot be read: "+err.Error())
	}
	sortBlobs(out)
	return out, problems
}

// Check is what Validate compares a proposal with: the state the curation
// started from and what names one pull request of the repository.
type Check struct {
	Base State
	// PRNumbers are the repository's pull request numbers; Branches their
	// head branches. A harness file named after a number, or notes naming a
	// branch, carry one pull request's content.
	PRNumbers []int
	Branches  []string
	// Misses are the ids of the misses the curation was given: the proposal
	// accounts for each.
	Misses []int64
}

var (
	// prRefRe matches a pull request or issue reference: #123, PR 123,
	// PR-123, pull/123, "pull request 123".
	prRefRe = regexp.MustCompile(`(?i)(?:\bPRs?[ \t#-]*\d+\b|\bpull/\d+|\bpull request #?\d+|(?:^|[^\w&/])#\d{2,}\b)`)
	// probeRe matches the name of a one-off probe file.
	probeRe = regexp.MustCompile(`(?i)(?:^|[/_.-])probes?(?:[/_.-]|$)`)
	// pathTokenRe finds the tokens of notes text that may name a file: runs
	// of word characters, dots, slashes and dashes.
	pathTokenRe = regexp.MustCompile(`[\w./-]+`)
	// fileExtRe matches a file extension ending a token: short and lower
	// case, so a JavaScript global's property (window.PROBE_SELECTOR) or a
	// setting's key (probe.enabled) is none.
	fileExtRe = regexp.MustCompile(`\.[a-z][a-z0-9]{0,4}$`)
	// homePathRe matches a machine-specific path: a home directory.
	homePathRe = regexp.MustCompile(`(?:/Users|/home)/[^/\s` + "`" + `'"]+/`)
	// headingRe matches a "## " section heading.
	headingRe = regexp.MustCompile(`(?m)^##[ \t]+(.+?)[ \t#]*$`)
)

// Sections lists the "## " headings of notes text.
func Sections(text []byte) []string {
	var out []string
	for _, m := range headingRe.FindAllSubmatch(text, -1) {
		out = append(out, strings.TrimSpace(string(m[1])))
	}
	return out
}

// Validate returns what is wrong with proposal p (nil = valid), in words fit
// for a nudge and the registry (names and rules, never notes text): the
// notes and changes.json must be there; every proposed section and harness
// file must be accounted for as kept or added with a one-line reason, and
// every current harness file with what became of it; every proposed harness
// file must be named in the notes; nothing may name a pull request, one of
// its branches or probe files, carry what looks like a secret or a home
// directory path; every miss given must be accounted for once, noted with a
// section of the proposal or skipped with a reason. Size is not checked: the
// limits trigger curations, they do not cap them. A proposal that changes
// nothing is invalid unless it was given misses and skips them all (the
// operator confirms the skips).
func Validate(p Proposal, c Check) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	text := string(p.State.Notes)
	if !utf8.ValidString(text) {
		add("%s is not UTF-8 text", scratchProposal)
	}
	if strings.ContainsFunc(text, func(r rune) bool { return r != '\r' && isControl(r) }) {
		add("%s holds control characters", scratchProposal)
	}
	if p.State.Same(c.Base) && (len(c.Misses) == 0 || slices.ContainsFunc(p.Changes.Misses, func(m MissChange) bool { return m.Action == MissNoted })) {
		add("the proposal changes nothing")
	}
	if p.ChangesJSON == nil {
		return problems // the readers' problems say why
	}

	sections := map[string]Change{}
	for _, ch := range p.Changes.Sections {
		problems = append(problems, checkChange("section", ch, []string{ActionKept, ActionAdded, ActionMerged, ActionRemoved})...)
		sections[normalize(ch.Name)] = ch
	}
	for _, s := range Sections(p.State.Notes) {
		ch, ok := sections[normalize(s)]
		switch {
		case !ok:
			add("section %q of %s is not in %s's sections", s, scratchProposal, scratchChanges)
		case ch.Action != ActionKept && ch.Action != ActionAdded:
			add("section %q of %s is %s in %s; a section the proposal has is kept or added", s, scratchProposal, ch.Action, scratchChanges)
		}
	}
	files := map[string]Change{}
	for _, ch := range p.Changes.Files {
		problems = append(problems, checkChange("file", ch, []string{ActionKept, ActionAdded, ActionMerged, ActionDeleted})...)
		files[ch.Name] = ch
	}
	proposed := map[string]bool{}
	for _, b := range p.State.Files {
		proposed[b.Path] = true
		ch, ok := files[b.Path]
		switch {
		case !ok:
			add("harness file %s is not in %s's files", b.Path, scratchChanges)
		case ch.Action != ActionKept && ch.Action != ActionAdded:
			add("harness file %s is %s in %s but still in %s/", b.Path, ch.Action, scratchChanges, scratchHarness)
		}
		if !strings.Contains(text, b.Path) {
			add("harness file %s is not named in %s", b.Path, scratchProposal)
		}
	}
	for _, ch := range p.Changes.Files {
		if (ch.Action == ActionKept || ch.Action == ActionAdded) && !proposed[ch.Name] {
			add("harness file %q is %s in %s but not in %s/", ch.Name, ch.Action, scratchChanges, scratchHarness)
		}
	}
	for _, b := range c.Base.Files {
		ch, ok := files[b.Path]
		switch {
		case !ok:
			add("current harness file %s is not in %s's files (kept, merged or deleted, with a reason)", b.Path, scratchChanges)
		case ch.Action == ActionMerged && !proposed[ch.Into]:
			add("harness file %s is merged into %q, which is not in %s/", b.Path, ch.Into, scratchHarness)
		case ch.Action == ActionDeleted && proposed[b.Path]:
			add("harness file %s is deleted in %s but still in %s/", b.Path, scratchChanges, scratchHarness)
		}
	}

	problems = append(problems, checkMisses(p, c.Misses)...)

	// One pull request's content.
	reasons := make([]string, 0, len(p.Changes.Sections)+len(p.Changes.Files)+len(p.Changes.Misses))
	for _, ch := range slices.Concat(p.Changes.Sections, p.Changes.Files) {
		reasons = append(reasons, ch.Reason)
	}
	for _, m := range p.Changes.Misses {
		reasons = append(reasons, m.Reason)
	}
	if prRefRe.MatchString(text) {
		add("%s names a pull request or issue number", scratchProposal)
	}
	if slices.ContainsFunc(reasons, prRefRe.MatchString) {
		add("a reason in %s names a pull request or issue number", scratchChanges)
	}
	for _, br := range c.Branches {
		if branchLike(br) && containsToken(text, br) {
			add("%s names a pull request's branch", scratchProposal)
			break
		}
	}
	for _, b := range p.State.Files {
		if probeName(b.Path, c.PRNumbers) {
			add("harness file %s is one pull request's probe: merge what is general into a parameterized script, or delete it", b.Path)
		}
	}
	for _, m := range pathTokenRe.FindAllString(text, -1) {
		if tok := strings.Trim(m, "."); probeFileName(tok) {
			add("%s names a probe file (%s)", scratchProposal, tok)
			break
		}
	}
	for _, b := range c.Base.Files {
		if probeName(b.Path, c.PRNumbers) && containsToken(text, b.Path) {
			add("%s names the probe file %s", scratchProposal, b.Path)
		}
	}

	// Secrets and machine-specific paths, in the notes and in every script.
	bodies := map[string]string{scratchProposal: text, scratchChanges: string(p.ChangesJSON)}
	for _, b := range p.State.Files {
		bodies[scratchHarness+"/"+b.Path] = string(b.Body)
	}
	for _, name := range slices.Sorted(maps.Keys(bodies)) {
		body := bodies[name]
		if execx.Redact(body) != body {
			add("%s holds what looks like a secret (a token or a key)", name)
		}
		if name != scratchChanges && homePathRe.MatchString(body) {
			add("%s names a home directory path (machine-specific)", name)
		}
	}
	return problems
}

// checkMisses checks changes.json's misses against the ids given: each one
// given appears once, noted with a section of the proposal or skipped with
// a reason, and none that was not given.
func checkMisses(p Proposal, given []int64) []string {
	var out []string
	sections := map[string]bool{}
	for _, s := range Sections(p.State.Notes) {
		sections[normalize(s)] = true
	}
	seen := map[int64]bool{}
	for _, m := range p.Changes.Misses {
		switch {
		case !slices.Contains(given, m.ID):
			out = append(out, fmt.Sprintf("miss %d in %s was not in %s", m.ID, scratchChanges, scratchMisses))
			continue
		case seen[m.ID]:
			out = append(out, fmt.Sprintf("miss %d is in %s's misses twice", m.ID, scratchChanges))
			continue
		}
		seen[m.ID] = true
		reason := strings.TrimSpace(m.Reason)
		switch m.Action {
		case MissNoted:
			switch section := strings.TrimSpace(m.Section); {
			case section == "":
				out = append(out, fmt.Sprintf("miss %d in %s is noted without the section that covers it", m.ID, scratchChanges))
			case !sections[normalize(section)]:
				out = append(out, fmt.Sprintf("miss %d in %s is noted in %q, which is not a section of %s", m.ID, scratchChanges, section, scratchProposal))
			}
		case MissSkipped:
			if reason == "" {
				out = append(out, fmt.Sprintf("miss %d in %s is skipped without a reason", m.ID, scratchChanges))
			}
		default:
			out = append(out, fmt.Sprintf("miss %d in %s: action %q is not one of %s, %s", m.ID, scratchChanges, m.Action, MissNoted, MissSkipped))
		}
		switch {
		case strings.ContainsAny(reason, "\r\n"):
			out = append(out, fmt.Sprintf("miss %d in %s: the reason is not one line", m.ID, scratchChanges))
		case utf8.RuneCountInString(reason) > maxReasonRunes:
			out = append(out, fmt.Sprintf("miss %d in %s: the reason is longer than %d characters", m.ID, scratchChanges, maxReasonRunes))
		}
	}
	for _, id := range given {
		if !seen[id] {
			out = append(out, fmt.Sprintf("miss %d of %s is not in %s's misses (noted with its section, or skipped with a reason)", id, scratchMisses, scratchChanges))
		}
	}
	return out
}

// checkChange checks one item of changes.json.
func checkChange(what string, ch Change, actions []string) []string {
	var out []string
	name := strings.TrimSpace(ch.Name)
	if name == "" {
		out = append(out, fmt.Sprintf("a %s in %s has no name", what, scratchChanges))
		name = "?"
	}
	if !slices.Contains(actions, ch.Action) {
		out = append(out, fmt.Sprintf("%s %q in %s: action %q is not one of %s", what, name, scratchChanges, ch.Action, strings.Join(actions, ", ")))
	}
	if ch.Action == ActionMerged && strings.TrimSpace(ch.Into) == "" {
		out = append(out, fmt.Sprintf("%s %q in %s is merged without saying into what", what, name, scratchChanges))
	}
	reason := strings.TrimSpace(ch.Reason)
	switch {
	case reason == "":
		out = append(out, fmt.Sprintf("%s %q in %s has no reason", what, name, scratchChanges))
	case strings.ContainsAny(reason, "\r\n"):
		out = append(out, fmt.Sprintf("%s %q in %s: the reason is not one line", what, name, scratchChanges))
	case utf8.RuneCountInString(reason) > maxReasonRunes:
		out = append(out, fmt.Sprintf("%s %q in %s: the reason is longer than %d characters", what, name, scratchChanges, maxReasonRunes))
	}
	return out
}

// normalize folds a section heading for matching: case, spaces and the
// heading marks.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.Trim(s, "# \t"))), " ")
}

// probeFileName reports whether a token of notes text names a probe file: a
// file name (with an extension) or a path (with a slash) whose name, or a
// directory of it, says probe. A bare identifier that says PROBE
// (window.PROBE_SELECTOR, PROBE_TIMEOUT) names no file; a harness file the
// notes name by a name without either is checked against the harness itself.
func probeFileName(tok string) bool {
	ext := fileExtRe.FindString(tok)
	if ext == "" && !strings.Contains(tok, "/") {
		return false
	}
	return probeRe.MatchString(strings.TrimSuffix(tok, ext))
}

// probeName reports whether a harness path is a one-off probe: "probe" or
// "probes" as a word of its name, or one of the repository's pull request
// numbers as a token.
func probeName(p string, prs []int) bool {
	if probeRe.MatchString(p) {
		return true
	}
	for _, n := range prs {
		if n >= 100 && numberToken(p, n) {
			return true
		}
	}
	return false
}

// branchLike reports whether a branch name is specific enough to look for
// in notes: long, with a separator ("feature/x", "fix-coupon-expiry").
func branchLike(b string) bool {
	return len(b) >= 6 && strings.ContainsAny(b, "/-_")
}

// containsToken reports whether s holds tok as a whole name: no letter,
// digit, "_" or "-" right before it (a "/" may be: a path prefix) and none of
// those nor a "/" right after it.
func containsToken(s, tok string) bool {
	if tok == "" {
		return false
	}
	word := func(r rune) bool {
		return r == '_' || r == '-' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
	}
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (start == 0 || !word(before)) && (end == len(s) || (!word(after) && after != '/')) {
			return true
		}
		i = start + 1
	}
}

// numberToken reports whether s holds the number n with no digit around it.
func numberToken(s string, n int) bool {
	num := strconv.Itoa(n)
	for i := 0; ; {
		j := strings.Index(s[i:], num)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(num)
		if (start == 0 || s[start-1] < '0' || s[start-1] > '9') && (end == len(s) || s[end] < '0' || s[end] > '9') {
			return true
		}
		i = start + 1
	}
}
