package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRRenamedFiles holds renamedFiles for a PR (same naming as store.KVPR*).
func kvPRRenamedFiles(id int64) string { return fmt.Sprintf("pr.%d.renamed_files", id) }

// renamedFiles is a head's file list with the paths its renames leave
// (withRenamedFrom): the Details' list it was read for, and the list the
// registry keeps for it.
type renamedFiles struct {
	Head   string   `json:"head"`
	Listed []string `json:"listed"` // the Details' list (PRDetails.Files)
	Paths  []string `json:"paths"`  // Listed and the previous paths of its renames
	Cut    bool     `json:"cut"`    // the REST list was cut too (ListFiles' complete is false)
}

// withRenamedFrom adds to in's file list, which the Details d name a rename
// in by its new path only (d.FilesRenamed), the paths its renames leave,
// from GitHub's REST list of the PR's files (ListFiles names both): a PR
// that moves .claude/hooks/x.rb to scripts/ changes .claude/ as much as one
// that deletes it, and what reads pr_files must see it (the auto-approval's
// gate, the judge's approval gates, the related PRs). The list it read for
// the same head and the same Details' list (kvPRRenamedFiles of prID, 0
// for a PR the registry does not know yet) serves again without a call.
// It returns the record to keep once the PR is upserted (nil: none new).
// When the REST list cannot be read (no lister, an error, the poll's file
// fetches spent), in keeps no file list and no updatedAt: the registry
// keeps the list it had, which for a head that moved is no list of the
// head (the gates refuse without a class and decide again), and the next
// poll reads the Details and tries again.
func (e *Engine) withRenamedFrom(ctx context.Context, gh GitHub, repo store.Repo, prID int64, d github.PRDetails, in *store.GitHubPR, budget *int) *renamedFiles {
	if !d.FilesRenamed || in.Files == nil {
		return nil
	}
	if prID != 0 {
		var r renamedFiles
		if v, ok := e.getKV(ctx, kvPRRenamedFiles(prID)); ok && json.Unmarshal([]byte(v), &r) == nil &&
			r.Head == in.Files.HeadSHA && slices.Equal(r.Listed, d.Files) {
			in.Files.Paths, in.Files.Truncated = slices.Clone(r.Paths), in.Files.Truncated && r.Cut
			return nil
		}
	}
	full := repo.FullName()
	lister, ok := gh.(fileLister)
	var err error
	switch {
	case !ok:
		err = fmt.Errorf("its GitHub client cannot list a PR's files")
	case *budget <= 0:
		err = fmt.Errorf("the poll's %d file lists are spent", maxFileFetchesPerRepo)
	}
	var files []string
	complete := false
	if err == nil {
		*budget--
		files, complete, err = lister.ListFiles(ctx, repo.Owner, repo.Name, in.Number)
	}
	if err != nil {
		in.Files, in.GHUpdatedAt = nil, nil
		if e.changed("renamed:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.files_error",
				fmt.Sprintf("the paths #%d's renames leave: %v (it has no file list until a later poll reads them)", in.Number, err), nil)
		}
		return nil
	}
	e.changed("renamed:"+full, "")
	paths := slices.Clone(in.Files.Paths)
	for _, f := range files {
		if !slices.Contains(paths, f) {
			paths = append(paths, f)
		}
	}
	in.Files.Paths, in.Files.Truncated = paths, in.Files.Truncated && !complete
	return &renamedFiles{Head: in.Files.HeadSHA, Listed: slices.Clone(d.Files), Paths: slices.Clone(paths), Cut: !complete}
}

// keepRenamedFiles records r for PR prID (withRenamedFrom); nil keeps what
// is there.
func (e *Engine) keepRenamedFiles(ctx context.Context, prID int64, r *renamedFiles) {
	if r == nil {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		e.log.Warn("encode renamed files", "pr", prID, "err", err)
		return
	}
	e.setKV(ctx, kvPRRenamedFiles(prID), string(b))
}
