// Package notes measures, versions and curates the repository notes (the
// file every review role reads first and the judge rewrites, engine.NotesPath)
// and their harness directory of QA scripts. It reads and writes files only:
// the engine decides when (after a round, at startup, on a schedule) and the
// CLI shows and applies what is here.
//
//	<root>/<owner>/<repo>.md                    the notes
//	<root>/<owner>/<repo>/                      the harness
//	<root>/<owner>/<repo>.lock/                 the lock the judges take (agents.NotesLockLine)
//	<root>/.curate/<owner>/<repo>/<id>/         one curation's scratch copy and proposal (curate.go)
//
// Every version of the notes and the harness is kept in the registry
// (store.RecordNotesVersion), not here. GitHub owners never start with a dot,
// so ".curate" never collides with an owner's directory.
package notes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// CurateDirName is the curations' directory under the notes root.
const CurateDirName = ".curate"

// Repo is one repository's notes under Root; Owner and Name are lower-case
// single path elements (engine.NotesPath checks them).
type Repo struct {
	Root, Owner, Name string
}

// RepoOf is the Repo whose notes file is notesPath (<root>/<owner>/<name>.md);
// false when the path does not have that shape.
func RepoOf(notesPath string) (Repo, bool) {
	name, ok := strings.CutSuffix(filepath.Base(notesPath), ".md")
	owner := filepath.Base(filepath.Dir(notesPath))
	root := filepath.Dir(filepath.Dir(notesPath))
	if !ok || name == "" || owner == "" || owner == "." || owner == string(filepath.Separator) || !filepath.IsAbs(notesPath) {
		return Repo{}, false
	}
	return Repo{Root: root, Owner: owner, Name: name}, true
}

// FullName is "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Notes is the notes file.
func (r Repo) Notes() string { return filepath.Join(r.Root, r.Owner, r.Name+".md") }

// Harness is the harness directory next to the notes file.
func (r Repo) Harness() string { return filepath.Join(r.Root, r.Owner, r.Name) }

// Lock is the lock directory the judges take (agents.NotesFiles).
func (r Repo) Lock() string { return r.Harness() + ".lock" }

// Curate is the directory of the repository's curations.
func (r Repo) Curate() string { return filepath.Join(r.Root, CurateDirName, r.Owner, r.Name) }

// Limits are the [notes] curation triggers: past any of them the repository
// is marked for curation. None of them blocks a review or a proposal.
type Limits struct {
	MaxBytes        int64 `json:"max_bytes"`
	MaxLine         int   `json:"max_line"` // characters
	MaxHarnessFiles int   `json:"max_harness_files"`
	MaxHarnessBytes int64 `json:"max_harness_bytes"`
}

// The limits by their [notes] key, in the order Over reports them.
const (
	LimitBytes        = "max_bytes"
	LimitLine         = "max_line"
	LimitHarnessFiles = "max_harness_files"
	LimitHarnessBytes = "max_harness_bytes"
)

// Size is what the notes and the harness hold.
type Size struct {
	Exists       bool  `json:"exists"` // the notes file exists
	Bytes        int64 `json:"bytes"`
	Lines        int   `json:"lines"`
	LongLines    int   `json:"long_lines"`   // lines longer than Limits.MaxLine characters
	LongestLine  int   `json:"longest_line"` // characters
	HarnessFiles int   `json:"harness_files"`
	HarnessBytes int64 `json:"harness_bytes"`
}

// Over names the limits s is past, in the order of the Limit* constants; a
// limit of 0 or less is off.
func (s Size) Over(l Limits) []string {
	var out []string
	if l.MaxBytes > 0 && s.Bytes > l.MaxBytes {
		out = append(out, LimitBytes)
	}
	if l.MaxLine > 0 && s.LongLines > 0 {
		out = append(out, LimitLine)
	}
	if l.MaxHarnessFiles > 0 && s.HarnessFiles > l.MaxHarnessFiles {
		out = append(out, LimitHarnessFiles)
	}
	if l.MaxHarnessBytes > 0 && s.HarnessBytes > l.MaxHarnessBytes {
		out = append(out, LimitHarnessBytes)
	}
	return out
}

// MeasureText measures notes text: its bytes, lines and the lines longer than
// maxLine characters (0 = none counted).
func MeasureText(text []byte, maxLine int) Size {
	s := Size{Exists: true, Bytes: int64(len(text))}
	for line := range strings.Lines(string(text)) {
		line = strings.TrimSuffix(line, "\n")
		n := utf8.RuneCountInString(line)
		s.Lines++
		s.LongestLine = max(s.LongestLine, n)
		if maxLine > 0 && n > maxLine {
			s.LongLines++
		}
	}
	return s
}

// Measure measures r's notes file and harness. A missing notes file or
// harness measures zero; any other read error is returned.
func Measure(r Repo, l Limits) (Size, []File, error) {
	var s Size
	text, err := os.ReadFile(r.Notes())
	switch {
	case err == nil:
		s = MeasureText(text, l.MaxLine)
	case !errors.Is(err, fs.ErrNotExist):
		return Size{}, nil, err
	}
	files, err := List(r.Harness())
	if err != nil {
		return Size{}, nil, err
	}
	s.HarnessFiles = len(files)
	for _, f := range files {
		s.HarnessBytes += f.Size
	}
	return s, files, nil
}

// File is one harness file: its slash-separated path relative to the
// harness directory, its size and the SHA-256 of its content.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// maxListed bounds the files List reads, so a runaway directory cannot make
// every round hash it all.
const maxListed = 2000

// List lists the regular files under dir, recursively, sorted by name;
// symbolic links and other special files are left out (never followed). A
// missing dir lists nothing.
func List(dir string) ([]File, error) {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var out []File
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(out) >= maxListed {
			return fmt.Errorf("notes: more than %d files under %s", maxListed, dir)
		}
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, File{Name: p, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// HarnessDelta compares two listings: the names only in after (added), only
// in before (removed) and in both with another content (changed), each
// sorted.
func HarnessDelta(before, after []File) (added, removed, changed []string) {
	old := map[string]string{}
	for _, f := range before {
		old[f.Name] = f.SHA256
	}
	seen := map[string]bool{}
	for _, f := range after {
		seen[f.Name] = true
		sum, ok := old[f.Name]
		switch {
		case !ok:
			added = append(added, f.Name)
		case sum != f.SHA256:
			changed = append(changed, f.Name)
		}
	}
	for _, f := range before {
		if !seen[f.Name] {
			removed = append(removed, f.Name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return added, removed, changed
}

// Fingerprint identifies a state of the notes and the harness: the notes'
// content (a missing file is empty notes, as a version records it) and every
// harness file's name and content.
func Fingerprint(notes []byte, files []File) string {
	return fingerprintOf(TextSHA(notes), files)
}

func fingerprintOf(notesSHA string, files []File) string {
	h := sha256.New()
	fmt.Fprintf(h, "notes %s\n", notesSHA)
	for _, f := range files {
		fmt.Fprintf(h, "%s %s\n", f.Name, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// TextSHA is the SHA-256 of notes text, hex.
func TextSHA(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// readNotes reads a notes file: its text and whether it exists.
func readNotes(p string) ([]byte, bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// cleanName reports whether name is a harness file name a proposal may
// carry: a relative, slash-separated, clean path without "..".
func cleanName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00") && !path.IsAbs(name) &&
		path.Clean(name) == name && name != "." && !slices.Contains(strings.Split(name, "/"), "..")
}

// Printable replaces the control characters other than newline and tab in
// text written by an agent, so it cannot drive a terminal.
func Printable(text string) string {
	if !bytes.ContainsFunc([]byte(text), isControl) {
		return text
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, text)
}

func isControl(r rune) bool {
	return r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0))
}
