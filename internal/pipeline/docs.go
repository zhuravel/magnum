package pipeline

// The base docs that name the changed files (DECISIONS "A round lists the
// base docs that name the changed files"): agents opened a repository's
// docs only on the PRs that edited them (5 of 51 talkable judge sessions),
// while 66 of 113 talkable PRs changed a path that one of its wiki or ADR
// pages names. Before the reviewers of each head, beside history.json, a
// round writes docs.json in its report directory: for each changed path
// (the watch's related_ignore paths aside), the .md pages of the base whose
// text names it, those naming it most often first; for the paths no page
// names, the pages that name their directory ("<dir>/", two levels deep at
// least: nearly every page names "app/"). The pages are read at
// origin/<base> (a blind replay: its merge base) from the object store with
// one git grep for the paths and one for the directories, never in the
// checkout, whose copy of a page is PR text: a PR that edits a page gets
// the base's version listed. Paths and page names are data in the file,
// never in a prompt or an event.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

// DocsFile is the base docs that name the changed files, in the round's
// report directory.
const DocsFile = "docs.json"

// docsPages is the pathspec of the pages: every .md file of the base.
const docsPages = ":(top)*.md"

// docs.json's caps.
const (
	maxDocsPaths   = 400 // changed paths searched (the words of one git grep)
	maxDocsEntries = 40  // paths and directories listed, the paths first
	maxDocsPages   = 10  // pages per path or directory
)

// DocsTimeout bounds the two greps: the docs are a hint the reviewers wait
// for (on talkable, 191 pages and 400 paths, the grep took 0.05 s).
var DocsTimeout = 30 * time.Second

// BaseDocs is docs.json.
type BaseDocs struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, whose changed paths are looked up
	// Base is the revision the pages were read at: origin/<base branch>, or
	// the merge base in a blind replay.
	Base  string     `json:"base"`
	Files []PathDocs `json:"files,omitempty"` // the changed paths that pages name
	Dirs  []DirDocs  `json:"dirs,omitempty"`  // the directories of the changed paths no page names, that pages name
	// More counts the changed paths left out: past the 40 paths and
	// directories listed, and past the 400 searched.
	More int `json:"more,omitempty"`
}

// PathDocs is one changed path of BaseDocs and the pages that name it.
type PathDocs struct {
	Path  string   `json:"path"`
	Pages []string `json:"pages"`          // at most 10, those naming the path most often first
	More  int      `json:"more,omitempty"` // pages past the 10
}

// DirDocs is one directory of BaseDocs: changed paths in it that no page
// names, and the pages that name the directory ("<dir>/").
type DirDocs struct {
	Dir   string   `json:"dir"`
	Paths []string `json:"paths"`
	Pages []string `json:"pages"`          // at most 10, those naming the directory most often first
	More  int      `json:"more,omitempty"` // pages past the 10
}

// pages counts the distinct pages d lists.
func (d BaseDocs) pages() int {
	seen := map[string]bool{}
	for _, f := range d.Files {
		for _, p := range f.Pages {
			seen[p] = true
		}
	}
	for _, dir := range d.Dirs {
		for _, p := range dir.Pages {
			seen[p] = true
		}
	}
	return len(seen)
}

// rankPages orders the pages of counts (page: occurrences) most
// occurrences first, then by name, leaves out the pages self names (a page
// never counts for itself), and keeps maxDocsPages; more counts the rest.
func rankPages(counts map[string]int, self ...string) (pages []string, more int) {
	pages = slices.DeleteFunc(slices.Collect(maps.Keys(counts)), func(p string) bool { return slices.Contains(self, p) })
	slices.SortFunc(pages, func(a, b string) int { return cmp.Or(cmp.Compare(counts[b], counts[a]), strings.Compare(a, b)) })
	if len(pages) > maxDocsPages {
		return pages[:maxDocsPages], len(pages) - maxDocsPages
	}
	return pages, 0
}

// baseDocs reads docs.json's content for the head under review: the pages
// at rev naming the paths it changes against base, within DocsTimeout.
// Files and Dirs are empty when no page names a changed path.
func (rd *round) baseDocs(ctx context.Context, rev, base string) (BaseDocs, error) {
	ctx, cancel := context.WithTimeout(ctx, DocsTimeout)
	defer cancel()
	in := rd.in
	out := BaseDocs{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, in.PR.Number), HeadSHA: in.TargetSHA, Base: rev}
	paths, err := rd.r.Git.ChangedPaths(ctx, in.SlotPath, base, in.TargetSHA)
	if err != nil {
		return out, err
	}
	paths = slices.DeleteFunc(paths, func(p string) bool {
		return strings.ContainsFunc(p, unicode.IsControl) ||
			slices.ContainsFunc(in.Related.Ignore, func(g string) bool { return config.MatchPath(g, p) })
	})
	if len(paths) > maxDocsPaths {
		out.More, paths = len(paths)-maxDocsPaths, paths[:maxDocsPaths]
	}
	if len(paths) == 0 {
		return out, nil
	}
	named, err := rd.r.Git.Mentions(ctx, in.SlotPath, rev, paths, docsPages)
	if err != nil {
		return out, err
	}
	var dirs []string                // in the order of their first path
	unnamed := map[string][]string{} // a directory's paths no page names
	for _, p := range paths {
		if pages, more := rankPages(named[p], p); len(pages) > 0 {
			out.Files = append(out.Files, PathDocs{Path: p, Pages: pages, More: more})
			continue
		}
		if d := path.Dir(p); strings.Contains(d, "/") {
			if unnamed[d] == nil {
				dirs = append(dirs, d)
			}
			unnamed[d] = append(unnamed[d], p)
		}
	}
	if len(dirs) > 0 {
		words := make([]string, len(dirs))
		for i, d := range dirs {
			words[i] = d + "/"
		}
		named, err := rd.r.Git.Mentions(ctx, in.SlotPath, rev, words, docsPages)
		if err != nil {
			return out, err
		}
		for _, d := range dirs {
			if pages, more := rankPages(named[d+"/"], unnamed[d]...); len(pages) > 0 {
				out.Dirs = append(out.Dirs, DirDocs{Dir: d, Paths: unnamed[d], Pages: pages, More: more})
			}
		}
	}
	if len(out.Files) > maxDocsEntries {
		out.More += len(out.Files) - maxDocsEntries
		out.Files = out.Files[:maxDocsEntries]
	}
	if keep := maxDocsEntries - len(out.Files); len(out.Dirs) > keep {
		for _, d := range out.Dirs[keep:] {
			out.More += len(d.Paths)
		}
		out.Dirs = out.Dirs[:keep]
	}
	return out, nil
}

// writeDocs writes docs.json for the head under review, beside its
// history.json (a restart writes the new head's), and keeps its path for
// the prompts in rd.docsFile; "" when there is none: a continued turn, no
// Git, a blind replay without its merge base, no page naming a changed
// path, or a failure, which warns: the review goes on without it. Each
// head's round.docs event counts the pages listed (0 for none).
func (rd *round) writeDocs(ctx context.Context) {
	rd.docsFile = ""
	if rd.r.Git == nil || rd.in.Kind == KindContinue {
		return
	}
	rev, base, ok := rd.historyBase()
	if !ok {
		rd.event(ctx, "info", "round.docs", "a blind replay without its merge base: no "+DocsFile, map[string]any{"pages": 0})
		return
	}
	at := rev
	if rd.in.Blind {
		at = "the merge base " + textx.ShortSHA(rev)
	}
	d, err := rd.baseDocs(ctx, rev, base)
	pages := d.pages()
	if err == nil && pages == 0 {
		rd.event(ctx, "info", "round.docs", fmt.Sprintf("no page on %s names a changed path: no %s", at, DocsFile),
			map[string]any{"pages": 0, "base": rev})
		return
	}
	path := filepath.Join(rd.dir, DocsFile)
	if err == nil {
		var b []byte
		if b, err = json.MarshalIndent(d, "", "  "); err == nil {
			err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.docs", fmt.Sprintf("could not list the base docs that name the changed files; the reviewers go without them: %v", err), nil)
		}
		return
	}
	rd.docsFile = path
	msg := fmt.Sprintf("%s: %d page(s) on %s name %d changed file(s) and %d directory(ies)", DocsFile, pages, at, len(d.Files), len(d.Dirs))
	if d.More > 0 {
		msg += fmt.Sprintf(" (%d more changed file(s) left out)", d.More)
	}
	rd.event(ctx, "info", "round.docs", msg, map[string]any{"pages": pages, "files": len(d.Files), "dirs": len(d.Dirs), "more": d.More, "base": rev, "file": path})
}
