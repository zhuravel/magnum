package agents

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

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
// never the names. A missing or unreadable directory lists nothing.
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
