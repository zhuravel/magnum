package notes

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// archiveBase is notes with a standing decision, a pitfall and an old
// section, a script, and no archive yet.
func archiveBase() State {
	s := State{Exists: true, Notes: []byte("# Notes for talkable/talkable (updated 2026-10-01)\n\n## Tests\nRun `run_spec.sh <spec>`.\n\n" +
		"## Decisions\n- Coupons never stack: the authors decided so, do not re-flag.\n- Old pitfall: the legacy importer skips blank rows.\n\n" +
		"## Old QA\n- Smoke a widget by hand in the sandbox.\n"),
		Files: []Blob{{Path: "run_spec.sh", Body: []byte("bin/rspec \"$@\"\n")}}}
	s.Files[0].SHA256 = TextSHA(s.Files[0].Body)
	return s
}

// archivedNotes keeps the decision, retires the legacy pitfall and folds
// the old QA section into a new one; it names the archive.
const archivedNotes = "# Notes for talkable/talkable (updated 2026-10-22)\n\n## Tests\nRun `run_spec.sh <spec>`.\n\n" +
	"## Decisions\n- Coupons never stack: the authors decided so, do not re-flag.\n\n" +
	"## QA\n- Smoke a widget in the sandbox, by hand or with a browser.\n\n" +
	"Retired lines: `ARCHIVE.md` (" + ArchiveLine + ").\n"

const archivedLines = "# Archive of the notes of talkable/talkable\n\n## Retired 2026-10-22\n" +
	"- Old pitfall: the legacy importer skips blank rows.\n→ obsolete because the legacy importer was removed from the base branch\n" +
	"- Smoke a widget by hand in the sandbox.\n→ merged into \"## QA\": Smoke a widget in the sandbox, by hand or with a browser.\n"

func archivedChanges() Changes {
	return Changes{
		Sections: []Change{
			{Name: "Tests", Action: ActionKept, Reason: "every review runs the specs it reads"},
			{Name: "Decisions", Action: ActionKept, Reason: "standing decisions stay in full"},
			{Name: "QA", Action: ActionAdded, Reason: "how to smoke any widget change"},
			{Name: "Old QA", Action: ActionMerged, Into: "QA", Reason: "its one line is the QA section's now"},
		},
		Files: []Change{{Name: "run_spec.sh", Action: ActionKept, Reason: "runs any spec"}},
	}
}

func archivedFiles(archive string) map[string]string {
	files := map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n"}
	if archive != "" {
		files[ArchiveName] = archive
	}
	return files
}

// A proposal that moves the lines it retires or rewrites to the archive
// word for word, each with what became of it, and accounts for every
// section, the old ones too, is valid: the archive needs no changes.json
// item and counts as no harness file.
func TestAProposalThatMovesALineToTheArchiveIsValid(t *testing.T) {
	p := proposal(t, archiveBase(), archivedNotes, archivedFiles(archivedLines), archivedChanges())
	if problems := Validate(p, Check{Base: archiveBase()}); len(problems) != 0 {
		t.Fatalf("problems: %q", problems)
	}
	size := p.State.Size(300)
	if size.HarnessFiles != 1 || size.HarnessBytes != int64(len("bin/rspec \"$@\"\n")) || size.ArchiveBytes != int64(len(archivedLines)) || size.ArchiveLines != 7 {
		t.Errorf("size = %+v: the archive is measured apart from the harness", size)
	}
	// Trailing spaces aside: a line kept with its trailing spaces trimmed is kept.
	base := archiveBase()
	base.Notes = []byte(strings.Replace(string(base.Notes), "do not re-flag.\n", "do not re-flag.  \n", 1))
	if problems := Validate(proposal(t, base, archivedNotes, archivedFiles(archivedLines), archivedChanges()), Check{Base: base}); len(problems) != 0 {
		t.Errorf("a kept line without its trailing spaces: %q", problems)
	}
}

// A proposal that drops a line without archiving it is invalid; the
// problem names how many lines are missing and quotes the first ones to the
// nudge and the operator's review.
func TestAProposalThatDropsALineWithoutArchivingItIsInvalid(t *testing.T) {
	archive := strings.Replace(archivedLines, "- Old pitfall: the legacy importer skips blank rows.\n", "", 1)
	problems := Validate(proposal(t, archiveBase(), archivedNotes, archivedFiles(archive), archivedChanges()), Check{Base: archiveBase()})
	want := `1 line of current.md is neither in proposal.md nor added to the archive (harness/ARCHIVE.md) word for word`
	if !hasProblem(problems, want) || !hasProblem(problems, `"- Old pitfall: the legacy importer skips blank rows."`) {
		t.Errorf("problems %q lack %q with the line", problems, want)
	}
	// A rewritten line is a dropped one: the decision reworded, its old words archived nowhere.
	notes := strings.Replace(archivedNotes, "do not re-flag.", "never flag it.", 1)
	problems = Validate(proposal(t, archiveBase(), notes, archivedFiles(archivedLines), archivedChanges()), Check{Base: archiveBase()})
	if !hasProblem(problems, "1 line of current.md is neither") || !hasProblem(problems, "Coupons never stack") {
		t.Errorf("a reworded decision: %q", problems)
	}
	// Nothing archived at all: every dropped line is counted, the first three quoted.
	problems = Validate(proposal(t, archiveBase(), archivedNotes, archivedFiles(""), archivedChanges()), Check{Base: archiveBase()})
	if !hasProblem(problems, "2 lines of current.md are neither") {
		t.Errorf("two dropped lines: %q", problems)
	}
}

// The title a curation dates anew and a line the leak rules keep out of
// any notes text (a home directory path) may go without the archive.
func TestTheTitleAndALeakingLineNeedNoArchive(t *testing.T) {
	base := archiveBase()
	base.Notes = append(base.Notes, []byte("- Ruby: /Users/someone/.local/share/mise/installs/ruby/3.3/bin\n")...)
	if problems := Validate(proposal(t, base, archivedNotes, archivedFiles(archivedLines), archivedChanges()), Check{Base: base}); len(problems) != 0 {
		t.Errorf("problems: %q", problems)
	}
}

// A proposal that drops a whole old section, its lines archived, but with
// no entry for it in changes.json is invalid.
func TestAProposalThatDropsAWholeOldSectionWithoutAnEntryIsInvalid(t *testing.T) {
	ch := archivedChanges()
	ch.Sections = slices.DeleteFunc(ch.Sections, func(c Change) bool { return c.Name == "Old QA" })
	problems := Validate(proposal(t, archiveBase(), archivedNotes, archivedFiles(archivedLines), ch), Check{Base: archiveBase()})
	if want := `current section "Old QA" of current.md is not in changes.json's sections (kept, merged or removed, with a reason)`; !hasProblem(problems, want) {
		t.Errorf("problems %q lack %q", problems, want)
	}
}

// The archive keeps every line it held: a proposal that drops one, or one
// of two copies, is invalid; one that only adds to it is valid.
func TestAProposalThatRemovesALineFromTheArchiveIsInvalid(t *testing.T) {
	base := archiveBase().WithArchive([]byte(archivedLines))
	base.Notes = []byte(archivedNotes)
	notes := archivedNotes + "\n## Pitfalls\n- A new pitfall.\n"
	ch := archivedChanges()
	ch.Sections = append(ch.Sections[:3], Change{Name: "Pitfalls", Action: ActionAdded, Reason: "a pitfall any review checks"})
	if problems := Validate(proposal(t, base, notes, archivedFiles(archivedLines), ch), Check{Base: base}); len(problems) != 0 {
		t.Fatalf("an archive kept as it was: %q", problems)
	}
	trimmed := strings.Replace(archivedLines, "→ obsolete because the legacy importer was removed from the base branch\n", "", 1)
	problems := Validate(proposal(t, base, notes, archivedFiles(trimmed), ch), Check{Base: base})
	if want := "the archive (harness/ARCHIVE.md) lost 1 line it held; the archive keeps every line"; !hasProblem(problems, want) {
		t.Errorf("problems %q lack %q", problems, want)
	}
	problems = Validate(proposal(t, base, notes, archivedFiles(""), ch), Check{Base: base})
	if !hasProblem(problems, "the archive (harness/ARCHIVE.md) lost 6 lines it held") {
		t.Errorf("the archive deleted: %q", problems)
	}
	twice := base.WithArchive([]byte(archivedLines + "- Smoke a widget by hand in the sandbox.\n"))
	problems = Validate(proposal(t, twice, notes, archivedFiles(archivedLines), ch), Check{Base: twice})
	if !hasProblem(problems, "lost 1 line it held") {
		t.Errorf("one of two copies dropped: %q", problems)
	}
}

// A proposal with an archive names it in its notes, once, as what it is.
func TestAProposalNamesItsArchive(t *testing.T) {
	notes := strings.Replace(archivedNotes, "Retired lines: `ARCHIVE.md` ("+ArchiveLine+").\n", "", 1)
	problems := Validate(proposal(t, archiveBase(), notes, archivedFiles(archivedLines), archivedChanges()), Check{Base: archiveBase()})
	if want := "proposal.md does not name the archive ARCHIVE.md"; !hasProblem(problems, want) {
		t.Errorf("problems %q lack %q", problems, want)
	}
}

// The archive is history: it may name a pull request, but not a secret or
// a home directory path.
func TestTheArchiveMayNameAPullRequestButNoSecret(t *testing.T) {
	base := archiveBase()
	base.Notes = append(base.Notes, []byte("- The importer fix of #11920 covers blank rows.\n")...)
	archive := archivedLines + "- The importer fix of #11920 covers blank rows.\n→ obsolete because the importer is gone\n"
	if problems := Validate(proposal(t, base, archivedNotes, archivedFiles(archive), archivedChanges()), Check{Base: base, PRNumbers: []int{11920}}); len(problems) != 0 {
		t.Errorf("an archived pull request reference: %q", problems)
	}
	leaky := archivedLines + "- GH_TOKEN=ghp_" + strings.Repeat("a", 36) + "\n"
	problems := Validate(proposal(t, archiveBase(), archivedNotes, archivedFiles(leaky), archivedChanges()), Check{Base: archiveBase()})
	if !hasProblem(problems, "harness/ARCHIVE.md holds what looks like a secret") {
		t.Errorf("a secret in the archive: %q", problems)
	}
}

// WithArchive sets the archive among the harness files, sorted, and an
// empty text removes it; WriteArchive writes it alone into the harness.
func TestWithArchiveAndWriteArchive(t *testing.T) {
	s := archiveBase().WithArchive([]byte("a\n"))
	if got := []string{s.Files[0].Path, s.Files[1].Path}; !slices.Equal(got, []string{ArchiveName, "run_spec.sh"}) || string(s.Archive()) != "a\n" {
		t.Fatalf("files %v, archive %q", got, s.Archive())
	}
	if s := s.WithArchive(nil); len(s.Files) != 1 || s.Archive() != nil {
		t.Errorf("an empty archive kept: %+v", s.Files)
	}
	if got := Scripts(s.Listing()); len(got) != 1 || got[0].Name != "run_spec.sh" {
		t.Errorf("Scripts = %+v", got)
	}
	r := testRepo(t)
	if err := WriteState(r, archiveBase()); err != nil {
		t.Fatal(err)
	}
	if err := WriteArchive(r, []byte("kept\n")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(r)
	if err != nil || string(got.Archive()) != "kept\n" || len(got.Files) != 2 {
		t.Fatalf("read back %+v, %v", got.Files, err)
	}
	size, _, err := Measure(r, Limits{MaxHarnessFiles: 1})
	if err != nil || size.HarnessFiles != 1 || size.ArchiveBytes != 5 || size.ArchiveLines != 1 || len(size.Over(Limits{MaxHarnessFiles: 1})) != 0 {
		t.Errorf("Measure = %+v, %v: the archive counts against no limit", size, err)
	}
	if err := WriteArchive(r, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadState(r); got.Archive() != nil || !got.Same(archiveBase()) {
		t.Errorf("the archive is still there: %+v", got.Files)
	}
}

func day(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.Local) }

// Recover archives every line a version of the history held and the notes
// now lack, once, grouped by the day of the version that dropped it (the
// last drop of a line that came back), in the order it had; lines the notes
// or the archive hold now, and a line with a home directory path, are not
// added; the notes stay as they are.
func TestRecoverArchivesEveryDroppedLineOnceByTheDayItLeft(t *testing.T) {
	history := []HistoryNotes{
		{ID: 1, At: day(1), Source: "judge", Notes: []byte("# Notes\n\n## Tests\nRun specs.\n- pitfall A\n- pitfall B\n- run /Users/someone/bin/x\n")},
		{ID: 2, At: day(3), Source: "curation", Notes: []byte("# Notes\n\n## Tests\nRun specs.\n")},
		{ID: 3, At: day(4), Source: "judge", Notes: []byte("# Notes\n\n## Tests\nRun specs.\n- pitfall A\n- pitfall C\n")},
		{ID: 4, At: day(6), Source: "curation", Notes: []byte("# Notes\n\n## Tests\nRun specs and lint.\n- pitfall C\n")},
	}
	live := State{Exists: true, Notes: history[3].Notes}.WithArchive([]byte("# Archive of the notes of talkable/talkable\n\n- pitfall B\n"))
	r := Recover("talkable/talkable", history, live, day(22))
	if r.Lines != 2 || r.Left != 1 || len(r.Groups) != 1 || r.Groups[0].Date != "2026-10-06" || !slices.Equal(r.Groups[0].Versions, []int64{4}) ||
		!slices.Equal(r.Groups[0].Lines, []string{"Run specs.", "- pitfall A"}) {
		t.Fatalf("recovery = %+v", r)
	}
	text := string(r.Archive)
	if !strings.HasPrefix(text, "# Archive of the notes of talkable/talkable\n\n- pitfall B\n\n## Recovered 2026-10-22: dropped on 2026-10-06\n") ||
		strings.Count(text, "- pitfall A\n") != 1 || strings.Contains(text, "pitfall C") || strings.Contains(text, "/Users/") ||
		!strings.Contains(text, "→ dropped by version 4 (curation);") {
		t.Errorf("archive:\n%s", text)
	}
	// The proposed state keeps the notes as they are and passes the leak rules.
	proposed := live.WithArchive(r.Archive)
	if string(proposed.Notes) != string(live.Notes) {
		t.Error("the recovery changed the notes")
	}
	for _, b := range proposed.Files {
		if leaks := CheckLeaks(string(b.Body), Check{}); slices.ContainsFunc(leaks, func(l string) bool { return !strings.Contains(l, "pull request") }) {
			t.Errorf("%s: %q", b.Path, leaks)
		}
	}
	// Two days of drops make two groups, oldest first; a new archive starts with its header.
	r = Recover("talkable/talkable", history[:3], State{Exists: true, Notes: history[2].Notes}, day(22))
	if len(r.Groups) != 1 || r.Groups[0].Date != "2026-10-03" || !strings.HasPrefix(string(r.Archive), ArchiveHeader("talkable/talkable")) {
		t.Errorf("recovery = %+v\n%s", r, r.Archive)
	}
	notesNow := State{Exists: true, Notes: []byte("# Notes\n")}
	r = Recover("talkable/talkable", history, notesNow, day(22))
	var dates []string
	for _, g := range r.Groups {
		dates = append(dates, g.Date)
	}
	if !slices.Equal(dates, []string{"2026-10-03", "2026-10-06", "2026-10-22"}) || r.Groups[2].Versions[0] != 0 ||
		!strings.Contains(string(r.Archive), "the notes on disk, not recorded yet") {
		t.Errorf("dropped since the last version: %+v", r.Groups)
	}
	if r := Recover("talkable/talkable", history[3:], live, day(22)); r.Lines != 0 || r.Archive != nil {
		t.Errorf("nothing dropped: %+v", r)
	}
}

// The proposal check and the recovery read the archive where a proposal
// writes it: harness/ARCHIVE.md of the scratch directory.
func TestPrepareScratchCopiesTheArchive(t *testing.T) {
	base := archiveBase().WithArchive([]byte(archivedLines))
	s, err := PrepareScratch(filepath.Join(t.TempDir(), "c1"), base, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := readTree(s.Harness(), 0)
	byName := map[string]string{}
	for _, b := range files {
		byName[b.Path] = string(b.Body)
	}
	if byName[ArchiveName] != archivedLines {
		t.Errorf("harness/ holds %v", slices.Sorted(maps.Keys(byName)))
	}
}
