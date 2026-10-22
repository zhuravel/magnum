package agents

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/notes"
)

// The roles read the repository notes in parts. Codex 0.162.1 keeps at
// most 10,000 tokens of one command's output by default and drops the
// middle (its exec tool's default request, under the policy the codex
// kind's tool_output_limit raises), and Claude Code's Read refuses a file
// above about 25,000 tokens: whole, the 97 KB notes reached the Codex
// judges cut in 94 of 103 reads, their middle (pitfalls, decisions) gone.
// So a round copies the live file into its report directory before its
// first prompt (FreezeNotes; judges rewrite the live file about 20 times a
// day, so line ranges of the live file would shift between two reads), and
// its prompts give the copy as parts of at most NotesPartMax bytes, one
// command each (SplitNotes); the judge re-reads the live file in parts,
// under the notes lock, before it merges (NotesRereadLine).
const (
	// NotesReadFile is the frozen copy of the notes in a round's (or a fix
	// job's) report directory.
	NotesReadFile = "notes-read.md"
	// NotesPartMax bounds a part: whole lines of at most this many bytes,
	// newlines counted, or a piece of one longer line.
	NotesPartMax = 16 << 10
)

// NotesParts is the frozen copy of the repository notes a round's roles
// read (RoleData and JudgeData.NotesParts), in parts. The zero value is no
// notes (none, or empty), and the prompts then name no parts.
type NotesParts struct {
	Path  string // the copy, absolute (the live file when it could not be written)
	Bytes int    // its size
	Lines int    // its lines, a last one without a newline counted
	Parts []NotesPart
}

// NotesPart is one part of NotesParts: lines From to To (1-based,
// inclusive), or, with CutFrom set, bytes CutFrom to CutTo (1-based,
// inclusive, CutTo possibly past the line's end) of the single line From,
// longer than NotesPartMax. Command prints it: `sed -n 'From,Top' <path>`,
// or `sed -n 'Fromp' <path> | cut -b CutFrom-CutTo`, the path shell-quoted.
type NotesPart struct {
	From, To       int
	CutFrom, CutTo int
	Command        string
}

// SplitNotes cuts text, the notes file at path, into NotesParts: runs of
// whole lines of at most NotesPartMax bytes (each line counted with its
// newline, a last line without one too), a line longer than that split
// inside into pieces of NotesPartMax bytes. Empty text has no parts.
// NotesRereadLine prints the same commands for a file.
func SplitNotes(path string, text []byte) NotesParts {
	np := NotesParts{Path: path, Bytes: len(text)}
	if len(text) == 0 {
		return np
	}
	lines := bytes.SplitAfter(text, []byte("\n"))
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	np.Lines = len(lines)
	q := shellQuote(path)
	start, size := 1, 0
	whole := func(end int) {
		if start <= end {
			np.Parts = append(np.Parts, NotesPart{From: start, To: end, Command: fmt.Sprintf("sed -n '%d,%dp' %s", start, end, q)})
		}
	}
	for i, line := range lines {
		n, l := i+1, len(bytes.TrimSuffix(line, []byte("\n")))+1
		if l > NotesPartMax {
			whole(n - 1)
			for from := 1; from < l; from += NotesPartMax {
				to := from + NotesPartMax - 1
				np.Parts = append(np.Parts, NotesPart{From: n, To: n, CutFrom: from, CutTo: to,
					Command: fmt.Sprintf("sed -n '%dp' %s | cut -b %d-%d", n, q, from, to)})
			}
			start, size = n+1, 0
			continue
		}
		if size+l > NotesPartMax {
			whole(n - 1)
			start, size = n, 0
		}
		size += l
	}
	whole(np.Lines)
	return np
}

// FreezeNotes copies the notes file live into dir as NotesReadFile and
// returns the copy's parts (SplitNotes), so every role of a round reads
// the same text however the judges rewrite live meanwhile. No notes (""
// live, a missing or empty file) is no copy and the zero NotesParts. A
// copy that cannot be written returns the live file's parts with the
// error: the roles still read the notes in parts, which a rewrite can
// shift.
func FreezeNotes(live, dir string) (NotesParts, error) {
	if live == "" {
		return NotesParts{}, nil
	}
	text, err := os.ReadFile(live)
	if errors.Is(err, fs.ErrNotExist) {
		return NotesParts{}, nil
	}
	if err != nil {
		return NotesParts{}, fmt.Errorf("agents: read the notes: %w", err)
	}
	if len(text) == 0 {
		return NotesParts{}, nil
	}
	frozen := filepath.Join(dir, NotesReadFile)
	if err := fsx.WriteFileAtomic(frozen, text, 0o600); err != nil {
		return SplitNotes(live, text), fmt.Errorf("agents: copy the notes: %w", err)
	}
	return SplitNotes(frozen, text), nil
}

// NotesRereadLine is the shell line that prints, for the notes file path
// as it is when it runs, the commands of its parts, as SplitNotes gives
// them (awk under LC_ALL=C, so lengths are bytes). The judge runs it under
// the notes lock, before it merges its lessons into the live file, which
// the round's frozen copy can be older than.
func NotesRereadLine(path string) string {
	q := shellQuote(path)
	return fmt.Sprintf(`LC_ALL=C awk -v m=%d 'function p(a, b) { if (a <= b) printf "sed -n %%c%%d,%%dp%%c %%s\n", 39, a, b, 39, f } `+
		`BEGIN { f = "%s"; s = 1 } `+
		`{ l = length($0) + 1; if (l > m) { p(s, NR - 1); for (o = 1; o < l; o += m) printf "sed -n %%c%%dp%%c %%s | cut -b %%d-%%d\n", 39, NR, 39, f, o, o + m - 1; s = NR + 1; n = 0; next } `+
		`if (n + l > m) { p(s, NR - 1); s = NR; n = 0 } n += l } END { p(s, NR) }' %s`,
		NotesPartMax, awkString(q), q)
}

// awkString escapes s for an awk string literal inside a single-quoted
// shell word: a quote becomes \047, so the word stays one.
func awkString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `'`, `\047`, "\n", `\n`).Replace(s)
}

// Repository notes (engine.NotesPath) are rewritten by the judge of every
// round of a repository, and rounds of different PRs of one repository run
// at the same time. Three judges once rewrote the same file within four
// minutes, each from the text it had read half an hour earlier, and the
// result named one of six harness scripts. So the judge takes a lock for
// its read-merge-write: a directory created with mkdir (atomic, and it
// outlives the shell command that made it, which an flock would not: the
// judge reads, merges and writes in separate tool calls), waited for at
// most three minutes and taken over when older than ten (its judge died
// holding it).
const (
	notesLockTries = 90 // × notesLockPoll: the wait is 3 minutes
	notesLockPoll  = 2  // seconds
	notesLockStale = 10 // minutes

	// NotesHarnessMax bounds the harness entries a judge prompt lists.
	NotesHarnessMax = 40
)

// NotesFiles returns the harness directory and the lock of the repository
// notes file notesPath: the same path without ".md", and that with ".lock"
// (engine.NotesDir and engine.NotesLockPath). Both are "" for "".
func NotesFiles(notesPath string) (dir, lock string) {
	if notesPath == "" {
		return "", ""
	}
	dir = strings.TrimSuffix(notesPath, ".md")
	return dir, dir + ".lock"
}

// NotesLockLine is the shell line a judge runs to take the notes lock
// (a directory): it prints "notes locked" once it holds it, or "notes busy"
// after three minutes. A lock older than ten minutes is removed first.
func NotesLockLine(lock string) string {
	return notesLockLine(lock, notesLockTries, notesLockPoll, notesLockStale)
}

func notesLockLine(lock string, tries, pollSeconds, staleMinutes int) string {
	q := shellQuote(lock)
	return fmt.Sprintf(`n=0; until mkdir %[1]s 2>/dev/null; do n=$((n+1)); if [ "$n" -ge %[2]d ]; then echo 'notes busy'; break; fi; `+
		`find %[1]s -maxdepth 0 -mmin +%[4]d -exec rmdir {} + 2>/dev/null; sleep %[3]d; done; [ "$n" -lt %[2]d ] && echo 'notes locked'`,
		q, tries, pollSeconds, staleMinutes)
}

// NotesUnlockLine is the shell line that releases the notes lock.
func NotesUnlockLine(lock string) string { return "rmdir " + shellQuote(lock) }

// NotesHarness is NotesHarnessLogged without a log.
func NotesHarness(dir string) (names []string, more int) {
	return NotesHarnessLogged(dir, nil)
}

// harnessName matches the harness entry names a judge prompt lists. A judge,
// whom a PR can steer, names the files, and the listing goes into the
// <magnum> block of every later judge prompt of the repository: a newline in
// a name would add a line to that block, an escape would drive a terminal,
// and a comma would split one name into two.
var harnessName = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)

// NotesHarnessLogged lists the harness directory dir for a judge prompt
// (JudgeData.NotesHarness): entry names sorted, a directory with a trailing
// "/", at most NotesHarnessMax, and how many more there are. A name with
// anything but letters, digits and "._@+-" is never listed: it counts under
// more, and log (optional) gets one line with how many such names there are,
// never the names. The archive of retired notes lines (notes.ArchiveName) is
// no script and is left out: the judge reads it only where the notes name
// it. A missing or unreadable directory lists nothing.
func NotesHarnessLogged(dir string, log execx.Logger) (names []string, more int) {
	if dir == "" {
		return nil, 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	unlisted := 0
	for _, e := range ents {
		name := e.Name()
		if notes.IsArchive(name) && !e.IsDir() {
			continue
		}
		if !harnessName.MatchString(name) {
			unlisted++
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	if unlisted > 0 && log != nil {
		log.Printf("agents: notes harness %s: %d entry name(s) with characters outside [A-Za-z0-9._@+-] left out of the judge prompt", dir, unlisted)
	}
	slices.Sort(names)
	if len(names) > NotesHarnessMax {
		return names[:NotesHarnessMax], len(names) - NotesHarnessMax + unlisted
	}
	return names, unlisted
}
