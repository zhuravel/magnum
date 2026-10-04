package agents

import (
	"fmt"
	"os"
	"slices"
	"strings"
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

// NotesHarness lists the harness directory dir for a judge prompt
// (JudgeData.NotesHarness): entry names sorted, a directory with a trailing
// "/", at most NotesHarnessMax, and how many more there are. A missing or
// unreadable directory lists nothing.
func NotesHarness(dir string) (names []string, more int) {
	if dir == "" {
		return nil, 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > NotesHarnessMax {
		return names[:NotesHarnessMax], len(names) - NotesHarnessMax
	}
	return names, 0
}
