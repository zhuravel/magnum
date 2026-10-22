package agents

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// notesText builds a notes file of n lines, line i (1-based) width(i) bytes
// long before its newline; noFinal leaves the last newline out.
func notesText(n int, width func(i int) int, noFinal bool) []byte {
	var b bytes.Buffer
	for i := 1; i <= n; i++ {
		w := width(i)
		line := strings.Repeat(string(rune('a'+i%26)), max(w, 0))
		b.WriteString(line)
		if i < n || !noFinal {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// runParts runs each part's command with sh and joins what they print the
// way a reader rebuilds the file: whole-line parts as printed, the pieces of
// one long line without the newline cut adds to each but the line's last.
func runParts(t *testing.T, np NotesParts) []byte {
	t.Helper()
	var out bytes.Buffer
	for i, p := range np.Parts {
		b, err := exec.Command("sh", "-c", p.Command).Output()
		if err != nil {
			t.Fatalf("part %d %q: %v", i+1, p.Command, err)
		}
		if p.CutFrom > 0 && i+1 < len(np.Parts) && np.Parts[i+1].CutFrom > 0 && np.Parts[i+1].From == p.From {
			b = bytes.TrimSuffix(b, []byte("\n"))
		}
		out.Write(b)
	}
	return out.Bytes()
}

// Codex keeps 10,000 tokens of one command's output by default and drops
// the middle, so a role reads the notes in parts of at most 16 KiB of
// whole lines, one command each: the parts cover every line of the copy
// once, in order, and only a line longer than a part is split inside it,
// into pieces of at most 16 KiB. Run in order, the commands print the file.
func TestTheNotesPartsCoverEveryLineOnceWithinTheBound(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := filepath.Join(t.TempDir(), "it's a") // quoting
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string][]byte{
		"the real size":        notesText(527, func(i int) int { return 120 + i*37%181 }, false),
		"no final newline":     notesText(300, func(i int) int { return 299 }, true),
		"exactly one part":     notesText(64, func(int) int { return NotesPartMax/64 - 1 }, false),
		"one byte over a part": notesText(65, func(int) int { return NotesPartMax/64 - 1 }, false),
		"a long line inside":   notesText(90, func(i int) int { return map[bool]int{true: 2*NotesPartMax + 5, false: 200}[i == 40] }, false),
		"a line of one part":   notesText(3, func(i int) int { return map[bool]int{true: NotesPartMax, false: 10}[i == 2] }, false),
		"empty lines":          notesText(40, func(i int) int { return i % 2 * 50 }, false),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "notes-read.md")
			if err := os.WriteFile(path, text, 0o600); err != nil {
				t.Fatal(err)
			}
			np := SplitNotes(path, text)
			lines := bytes.Count(text, []byte("\n"))
			if !bytes.HasSuffix(text, []byte("\n")) {
				lines++
			}
			if np.Path != path || np.Bytes != len(text) || np.Lines != lines {
				t.Fatalf("parts of %s: %d bytes, %d lines; want %d, %d", np.Path, np.Bytes, np.Lines, len(text), lines)
			}
			split := strings.SplitAfter(string(text), "\n")
			next := 1 // the next line a part must start at
			for i, p := range np.Parts {
				if p.CutFrom > 0 {
					if p.From != p.To || len(split[p.From-1]) <= NotesPartMax || p.CutTo-p.CutFrom+1 > NotesPartMax {
						t.Fatalf("part %d cuts line %d-%d bytes %d-%d", i+1, p.From, p.To, p.CutFrom, p.CutTo)
					}
					if p.CutFrom == 1 && p.From != next || p.CutFrom > 1 && p.From != next-1 {
						t.Fatalf("part %d cuts line %d, want the line after %d", i+1, p.From, next-1)
					}
					next = p.From + 1
					continue
				}
				if p.From != next || p.To < p.From {
					t.Fatalf("part %d covers lines %d-%d, want it to start at %d", i+1, p.From, p.To, next)
				}
				size := 0
				for _, l := range split[p.From-1 : p.To] {
					size += len(strings.TrimSuffix(l, "\n")) + 1
				}
				if size > NotesPartMax {
					t.Fatalf("part %d (lines %d-%d) holds %d bytes, over %d", i+1, p.From, p.To, size, NotesPartMax)
				}
				next = p.To + 1
			}
			if next != lines+1 {
				t.Fatalf("the parts end at line %d of %d", next-1, lines)
			}
			if got := runParts(t, np); !bytes.Equal(got, text) && !bytes.Equal(got, append(bytes.Clone(text), '\n')) {
				t.Fatalf("the parts print %d bytes, not the file's %d", len(got), len(text))
			}
		})
	}
	if np := SplitNotes("/x/notes-read.md", nil); np.Lines != 0 || len(np.Parts) != 0 {
		t.Errorf("empty notes: %+v", np)
	}
}

// The judge re-reads the live notes under the lock, where the frozen copy
// can be older: NotesRereadLine prints, for the file as it is then, the
// same part commands SplitNotes gives the copy, its path quoted as a shell
// reads it.
func TestTheRereadLinePrintsThePartsOfTheLiveFile(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	dir := filepath.Join(t.TempDir(), `it's "a" \dir`)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "talkable.md")
	for name, text := range map[string][]byte{
		"the real size":      notesText(527, func(i int) int { return 120 + i*37%181 }, false),
		"no final newline":   notesText(300, func(int) int { return 299 }, true),
		"a long line inside": notesText(90, func(i int) int { return map[bool]int{true: 2*NotesPartMax + 5, false: 200}[i == 40] }, false),
		"empty":              nil,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(live, text, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("sh", "-c", NotesRereadLine(live)).Output()
			if err != nil {
				t.Fatalf("reread line: %v\n%s", err, out)
			}
			var want strings.Builder
			for _, p := range SplitNotes(live, text).Parts {
				want.WriteString(p.Command + "\n")
			}
			if string(out) != want.String() {
				t.Fatalf("the reread line printed\n%s\nwant\n%s", out, want.String())
			}
		})
	}
}

// Before a role is prompted, the round copies the live notes beside its
// reports and every role reads that copy: a judge's rewrite of the live
// file afterwards leaves the copy and its parts as they were. No notes, or
// empty ones, give no copy and no parts.
func TestFreezeNotesCopiesTheLiveFileOnce(t *testing.T) {
	home := t.TempDir()
	live := filepath.Join(home, "notes", "talkable", "talkable.md")
	reports := filepath.Join(home, "reviews", "11920", "d4e5f6a")
	for _, d := range []string{filepath.Dir(live), reports} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if np, err := FreezeNotes(live, reports); err != nil || np.Path != "" || len(np.Parts) != 0 {
		t.Fatalf("no notes: %+v, %v", np, err)
	}
	text := notesText(400, func(i int) int { return 150 + i%100 }, false)
	if err := os.WriteFile(live, text, 0o600); err != nil {
		t.Fatal(err)
	}
	np, err := FreezeNotes(live, reports)
	if err != nil {
		t.Fatal(err)
	}
	frozen := filepath.Join(reports, NotesReadFile)
	if np.Path != frozen || len(np.Parts) < 2 || np.Bytes != len(text) {
		t.Fatalf("frozen parts: %s, %d bytes, %d parts", np.Path, np.Bytes, len(np.Parts))
	}
	if err := os.WriteFile(live, []byte("# Notes for talkable/talkable (updated 2026-10-22)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(frozen); err != nil || !bytes.Equal(got, text) {
		t.Fatalf("the copy changed with the live file: %d bytes, %v", len(got), err)
	}
	if err := os.WriteFile(live, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if np, err := FreezeNotes(live, t.TempDir()); err != nil || np.Path != "" {
		t.Fatalf("empty notes: %+v, %v", np, err)
	}
}
