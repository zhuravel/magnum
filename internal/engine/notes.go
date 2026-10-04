package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// Repository notes are what the reviewers and the judge learned about a
// repository over earlier rounds: what it is, how to run its tests and lint,
// how to QA a change, known pitfalls. The roles read the file first and the
// judge rewrites it when a round taught something durable. Next to the file
// sits a harness directory (the same path without ".md") where the judge
// saves the QA scripts the notes mention, and the lock (a directory) the
// judge holds while it re-reads, merges and rewrites the file, because the
// judges of a repository's PRs run at the same time (agents.NotesLockLine).
//
//	<home>/state/notes/<owner>/<repo>.md
//	<home>/state/notes/<owner>/<repo>/
//	<home>/state/notes/<owner>/<repo>.lock/
//
// Owner and repo are lower-cased, so Talkable/Magnum and talkable/magnum
// share one file.

// NotesPath is the repository notes file of owner/repo under l, "" when l has
// no home or owner or repo is not a single plain path element (empty, "." or
// "..", or containing a path separator), which GitHub names never are.
func NotesPath(l paths.Layout, owner, repo string) string {
	dir := NotesDir(l, owner, repo)
	if dir == "" {
		return ""
	}
	return dir + ".md"
}

// NotesDir is the harness directory next to the notes file: the notes path
// without ".md". "" under the same conditions as NotesPath.
func NotesDir(l paths.Layout, owner, repo string) string {
	root := NotesRoot(l)
	if root == "" {
		return ""
	}
	owner, repo = strings.ToLower(owner), strings.ToLower(repo)
	if !notesElement(owner) || !notesElement(repo) {
		return ""
	}
	return filepath.Join(root, owner, repo)
}

// NotesLockPath is the lock of the notes file: the harness directory with
// ".lock". "" under the same conditions as NotesPath. The judge takes it
// with agents.NotesLockLine, which derives the same path from the notes
// file (agents.NotesFiles).
func NotesLockPath(l paths.Layout, owner, repo string) string {
	dir := NotesDir(l, owner, repo)
	if dir == "" {
		return ""
	}
	return dir + ".lock"
}

// NotesRoot is the directory holding every repository's notes, "" when l
// names no place (paths.Layout.Valid).
func NotesRoot(l paths.Layout) string {
	if !l.Valid() {
		return ""
	}
	return l.Notes()
}

// notesElement reports whether s is one plain path element.
func notesElement(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`+"\x00")
}

// roundNotes is the notes file the round's roles get (RoundInput.NotesPath).
// Unless the daemon is a dry run it also creates the harness directory (and
// with it the parents), so the judge can save scripts there; a failure is
// logged and the path is returned anyway.
func (e *Engine) roundNotes(repo store.Repo) string {
	path := NotesPath(e.d.Layout, repo.Owner, repo.Name)
	if path == "" || e.d.DryRun {
		return path
	}
	dir := NotesDir(e.d.Layout, repo.Owner, repo.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.log.Warn("create repository notes directory", "repo", repo.FullName(), "dir", dir, "err", err)
	}
	return path
}
