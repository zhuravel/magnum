package notes

// The archive (DECISIONS "Curation organizes the notes and never loses a
// line"): one reserved file of the harness, ArchiveName, that holds every
// line a curation took out of the notes, word for word, with what became of
// it. Being a harness file, it is part of the notes state: the fingerprint,
// the snapshot, every version in the registry, the stale merge, apply and
// restore carry it with no code of their own. What treats harness files as
// QA scripts leaves it out: the limits (Size), the judge's listing, the
// rounds that count a file's uses, the rules that every file be named in the
// notes and accounted for in changes.json. Only a curation or a recovery
// (Recover) writes it; a judge never edits it (the engine puts back what a
// judge changed).

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

// ArchiveName is the archive's path in the harness directory.
const ArchiveName = "ARCHIVE.md"

// ArchiveLine is how the notes name the archive, once: what it holds and
// when to read it. Roles do not read the archive by default.
const ArchiveLine = "retired lines, word for word; search it when a lesson seems missing"

// IsArchive reports whether the harness path p is the archive.
func IsArchive(p string) bool { return p == ArchiveName }

// Archive is s's archive text, nil when s has none.
func (s State) Archive() []byte {
	for _, b := range s.Files {
		if IsArchive(b.Path) {
			return b.Body
		}
	}
	return nil
}

// WithArchive is s with its archive text set to text (no archive when text
// is empty); s itself is left as it was.
func (s State) WithArchive(text []byte) State {
	files := make([]Blob, 0, len(s.Files)+1)
	for _, b := range s.Files {
		if !IsArchive(b.Path) {
			files = append(files, b)
		}
	}
	if len(text) > 0 {
		files = append(files, Blob{Path: ArchiveName, SHA256: TextSHA(text), Body: text})
		sortBlobs(files)
	}
	s.Files = files
	return s
}

// Scripts is a listing without the archive: the harness of QA scripts.
func Scripts(files []File) []File {
	return slices.DeleteFunc(slices.Clone(files), func(f File) bool { return IsArchive(f.Name) })
}

// WriteArchive makes r's archive text (removes it when text is empty),
// confined to the harness directory. The caller holds r's lock.
func WriteArchive(r Repo, text []byte) error {
	if len(text) == 0 {
		if err := os.Remove(filepath.Join(r.Harness(), ArchiveName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(r.Harness(), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(r.Harness(), ArchiveName), text, 0o600)
}

// ArchiveHeader is the start of a new archive.
func ArchiveHeader(full string) string {
	return "# Archive of the notes of " + full + "\n\n" +
		"Retired lines of the notes, word for word, each with what became of it. Only a curation or a recovery adds to this " +
		"file, and no line leaves it. Search it when a lesson seems missing from the notes.\n"
}

// contentLines are text's non-empty lines in order, trailing spaces aside.
func contentLines(text []byte) []string {
	var out []string
	for line := range strings.Lines(string(text)) {
		line = strings.TrimRight(line, " \t\r\n")
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// lineLeaks reports whether a notes line holds what the leak rules refuse in
// any notes text: a secret or a home directory path. Such a line goes
// without the archive (the rules keep it out of the archive too); the
// registry keeps it in the versions that held it.
func lineLeaks(line string) bool {
	return execx.Redact(line) != line || homePathRe.MatchString(line)
}

// quoteLines quotes the first few of lines for a problem: the operator's
// review and the curator's nudge (never an event) say which lines.
func quoteLines(lines []string) string {
	const shown, runes = 3, 100
	quoted := make([]string, 0, shown)
	for _, l := range lines[:min(shown, len(lines))] {
		quoted = append(quoted, fmt.Sprintf("%q", textx.Clip(Printable(strings.TrimSpace(l)), runes)))
	}
	out := strings.Join(quoted, ", ")
	if len(lines) > shown {
		out += fmt.Sprintf(" and %d more", len(lines)-shown)
	}
	return out
}

// lineWord is "line" or "lines" for n.
func lineWord(n int) string { return textx.Count(n, "line", "lines") }

// checkPreserved is Validate's preservation check: curation organizes the
// notes and never loses a line. Every non-empty line of the base notes
// (trailing spaces aside) is in the proposed notes or among the lines the
// proposal adds to the archive, word for word; the base notes' title (a
// first line that starts with "# ", which a curation dates anew) and a line
// that holds a secret or a home directory path are exempt, and so is a "## "
// heading changes.json accounts for (what became of its section is said
// there). The archive keeps
// every line it held, as many times as it held it. Every "## " section of
// the base notes is in changes.json's sections (sections, by normalized
// heading). A proposal with an archive names it in its notes.
func checkPreserved(p Proposal, base State, sections map[string]Change) []string {
	names := preserveNames{current: scratchCurrent, proposal: scratchProposal, archive: scratchHarness + "/" + ArchiveName}
	out := lostLines(base, p.State, sections, names)
	for _, s := range Sections(base.Notes) {
		if _, ok := sections[normalize(s)]; !ok {
			out = append(out, fmt.Sprintf("current section %q of %s is not in %s's sections (kept, merged or removed, with a reason)", s, scratchCurrent, scratchChanges))
		}
	}
	return append(out, archiveNamed(p.State, names)...)
}

// CheckPreserved is the preservation check an apply runs before it writes
// anything (engine.ApplyNotesProposal), for the operator and the critic
// alike: after, the state the apply would write (a curation's proposed
// state, or a stale one's merge with the notes' changes since), loses no
// line of before, the notes now. Every non-empty line of before's notes is
// in after's notes or among the lines after adds to the archive, word for
// word (the title, a leaking line and a "## " heading the curator's changes
// account for exempt, as in Validate); the archive keeps every line it held.
// Unlike Validate it asks only that nothing be lost: not that changes.json
// account for every section of before (a section a judge added since the
// proposal was made is kept by the merge, line by line), nor that the notes
// name the archive (a recovery leaves the notes text as it is). nil = the
// apply may go on; a proposal that fails it can only be rejected or curated
// again.
func CheckPreserved(before, after State, changes Changes) []string {
	sections := map[string]Change{}
	for _, ch := range changes.Sections {
		sections[normalize(ch.Name)] = ch
	}
	return lostLines(before, after, sections, preserveNames{current: "the notes now", proposal: "what the apply would write", archive: ArchiveName})
}

// preserveNames are how the preservation problems name the notes before,
// the proposed notes and the archive: the scratch directory's files for a
// curator's nudge, plainer words for an apply's refusal.
type preserveNames struct{ current, proposal, archive string }

// lostLines lists what after loses of before: the archive's lines it no
// longer holds, then the notes' lines neither in after's notes nor added to
// its archive (checkPreserved's exemptions apply).
func lostLines(before, after State, sections map[string]Change, names preserveNames) []string {
	var out []string
	had, has := counts(contentLines(before.Archive())), counts(contentLines(after.Archive()))
	var lost []string
	added := map[string]bool{}
	for line, n := range has {
		if n > had[line] {
			added[line] = true
		}
	}
	for _, line := range contentLines(before.Archive()) {
		if has[line] < had[line] {
			lost = append(lost, line)
			had[line]-- // each missing occurrence once
		}
	}
	if len(lost) > 0 {
		out = append(out, fmt.Sprintf("the archive (%s) lost %s it held; the archive keeps every line: %s", names.archive, lineWord(len(lost)), quoteLines(lost)))
	}
	proposed := counts(contentLines(after.Notes))
	var missing []string
	seen := map[string]bool{}
	for i, line := range contentLines(before.Notes) {
		switch {
		case i == 0 && strings.HasPrefix(line, "# "), proposed[line] > 0, added[line], seen[line], lineLeaks(line), accountedHeading(line, sections):
			continue
		}
		seen[line] = true
		missing = append(missing, line)
	}
	if len(missing) > 0 {
		out = append(out, fmt.Sprintf("%s of %s %s neither in %s nor added to the archive (%s) word for word (trailing spaces aside); "+
			"keep each line, or move it to the archive with what became of it: %s",
			lineWord(len(missing)), names.current, isAre(len(missing)), names.proposal, names.archive, quoteLines(missing)))
	}
	return out
}

// archiveNamed is the problem of a state with an archive its notes do not
// name.
func archiveNamed(s State, names preserveNames) []string {
	if len(s.Archive()) > 0 && !strings.Contains(string(s.Notes), ArchiveName) {
		return []string{fmt.Sprintf("%s does not name the archive %s: name it once, with %q", names.proposal, ArchiveName, ArchiveLine)}
	}
	return nil
}

// accountedHeading reports whether line is a "## " heading that changes.json
// accounts for: what became of a section is said there, with a reason.
func accountedHeading(line string, sections map[string]Change) bool {
	m := headingRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	_, ok := sections[normalize(m[1])]
	return ok
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// counts counts each line.
func counts(lines []string) map[string]int {
	out := make(map[string]int, len(lines))
	for _, l := range lines {
		out[l]++
	}
	return out
}

// HistoryNotes is the notes text of one version of a repository's history
// (store.NotesHistory: the judges', the people's, the imports and the
// applied curations', never a proposal's own state).
type HistoryNotes struct {
	ID     int64
	At     time.Time
	Source string
	Notes  []byte
}

// Recovery is what Recover proposes: the archive that holds every line a
// version of the history held and the notes now lack.
type Recovery struct {
	Lines  int             `json:"lines"`    // lines recovered
	Left   int             `json:"left_out"` // lines left out: each holds a secret or a home directory path
	Groups []RecoveryGroup `json:"groups"`
	// Archive is the archive to propose: the one the notes have now, with
	// the recovered lines added (nil when there are none).
	Archive []byte `json:"-"`
}

// RecoveryGroup is the recovered lines that left the notes on one day.
type RecoveryGroup struct {
	Date     string   `json:"date"`     // the day the versions that dropped them were recorded, local
	Versions []int64  `json:"versions"` // the versions that dropped them
	Count    int      `json:"lines"`
	Lines    []string `json:"-"`
}

// Recover finds every line that a version of history (oldest first) held
// and the notes now (live) lack word for word, trailing spaces aside, and
// proposes live's archive with those lines added, grouped by the day of the
// version that dropped them (the last time, for a line that came back and
// left again), in the order they had there. A line live's archive holds
// already is not added again; a line with a secret or a home directory path
// is left out (counted). The notes now count as the history's last state:
// a line they dropped since the last version is dated now. The archive is
// history: a line may name a pull request.
func Recover(full string, history []HistoryNotes, live State, now time.Time) Recovery {
	type drop struct {
		at      int // the index of the version that dropped it
		pos     int // its place in the version before
		version int64
		date    string
	}
	steps := slices.Clone(history)
	if n := len(steps); n == 0 || TextSHA(steps[n-1].Notes) != TextSHA(live.Notes) {
		steps = append(steps, HistoryNotes{At: now, Notes: live.Notes})
	}
	dropped := map[string]drop{}
	sources := map[int64]string{}
	var prev []string
	for i, v := range steps {
		sources[v.ID] = v.Source
		cur := contentLines(v.Notes)
		have := counts(cur)
		for pos, line := range prev {
			if have[line] == 0 {
				dropped[line] = drop{at: i, pos: pos, version: v.ID, date: v.At.Local().Format(time.DateOnly)}
			}
		}
		prev = cur
	}
	kept := counts(contentLines(live.Notes))
	archived := counts(contentLines(live.Archive()))
	var out Recovery
	lines := make([]string, 0, len(dropped))
	for line := range maps.Keys(dropped) {
		switch {
		case kept[line] > 0, archived[line] > 0:
		case lineLeaks(line):
			out.Left++
		default:
			lines = append(lines, line)
		}
	}
	slices.SortFunc(lines, func(a, b string) int {
		da, db := dropped[a], dropped[b]
		return cmp.Or(cmp.Compare(da.at, db.at), cmp.Compare(da.pos, db.pos), strings.Compare(a, b))
	})
	for _, line := range lines {
		d := dropped[line]
		if n := len(out.Groups); n == 0 || out.Groups[n-1].Date != d.date {
			out.Groups = append(out.Groups, RecoveryGroup{Date: d.date})
		}
		g := &out.Groups[len(out.Groups)-1]
		if !slices.Contains(g.Versions, d.version) {
			g.Versions = append(g.Versions, d.version)
		}
		g.Lines = append(g.Lines, line)
		g.Count++
	}
	out.Lines = len(lines)
	if out.Lines == 0 {
		return out
	}
	var b strings.Builder
	if old := live.Archive(); len(old) > 0 {
		b.Write(old)
		if !strings.HasSuffix(string(old), "\n") {
			b.WriteString("\n")
		}
	} else {
		b.WriteString(ArchiveHeader(full))
	}
	for _, g := range out.Groups {
		var by []string
		for _, id := range g.Versions {
			if id == 0 {
				by = append(by, "the notes on disk, not recorded yet")
			} else {
				by = append(by, fmt.Sprintf("version %d (%s)", id, sources[id]))
			}
		}
		fmt.Fprintf(&b, "\n## Recovered %s: dropped on %s\n", now.Local().Format(time.DateOnly), g.Date)
		fmt.Fprintf(&b, "→ dropped by %s; recovered from the notes history (`magnum notes %s --recover`), which recorded no disposition\n",
			strings.Join(by, ", "), full)
		for _, line := range g.Lines {
			b.WriteString(line + "\n")
		}
	}
	out.Archive = []byte(b.String())
	return out
}
