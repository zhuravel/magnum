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
// (withRenamedFrom): the head, the base branch and its tip, and the
// Details' list it was read for, and the list the registry keeps for it.
type renamedFiles struct {
	Head string `json:"head"`
	// Base and BaseOid are the PR's base branch and its tip as the Details
	// named them: what GitHub compared the head with, which decides the
	// path a rename leaves (a retarget can change it while the head and
	// the new paths stay). A record without them (an earlier build's)
	// serves no list.
	Base    string   `json:"base"`
	BaseOid string   `json:"base_oid"`
	Listed  []string `json:"listed"` // the Details' list (PRDetails.Files)
	Paths   []string `json:"paths"`  // Listed and the previous paths of its renames
	Cut     bool     `json:"cut"`    // the REST list was cut too (ListFiles' complete is false)
}

// serves reports whether r, a record read for an earlier poll, is the
// list of in's head as the Details d name it: the same head, base branch,
// base tip and Details' list.
func (r renamedFiles) serves(d github.PRDetails, head string) bool {
	return r.Head == head && r.Base != "" && r.Base == d.BaseRefName && r.BaseOid == d.BaseRefOid && slices.Equal(r.Listed, d.Files)
}

// withRenamedFrom adds to in's file list, which the Details d name a rename
// in by its new path only (d.FilesRenamed), the paths its renames leave,
// from GitHub's REST list of the PR's files (ListFiles names both): a PR
// that moves .claude/hooks/x.rb to scripts/ changes .claude/ as much as one
// that deletes it, and what reads pr_files must see it (the auto-approval's
// gate, the judge's approval gates, the related PRs). The list it read for
// the same head, base branch, base tip and Details' list (kvPRRenamedFiles
// of prID, 0 for a PR the registry does not know yet; renamedFiles.serves)
// serves again without a call: a PR retargeted to another base reads it
// again, since the path a rename leaves is the new base's.
// The list stays cut where the Details cut it (at 100 files), whatever the
// REST list says (it was read at another moment), and is cut where the
// REST list is. It returns the record to keep once the PR is upserted
// (nil: none new).
// When the REST list cannot be read (no lister, an error, the poll's file
// fetches spent), in keeps no file list and no updatedAt, and names the
// head unlisted (store.GitHubPR.FilesUnlisted): the registry drops a list
// it kept for that head (an earlier build's, without the paths its renames
// leave, or one GitHub gave before a retarget) and keeps one of another
// head, which is no list of this one either, so the gates refuse without a
// class and decide again; the next poll reads the Details and tries again.
// A failure spends the poll's file reads (budget), as fetchChangedFiles'.
func (e *Engine) withRenamedFrom(ctx context.Context, gh GitHub, repo store.Repo, prID int64, d github.PRDetails, in *store.GitHubPR, budget *int) *renamedFiles {
	if !d.FilesRenamed || in.Files == nil {
		return nil
	}
	if prID != 0 {
		var r renamedFiles
		if v, ok := e.getKV(ctx, kvPRRenamedFiles(prID)); ok && json.Unmarshal([]byte(v), &r) == nil && r.serves(d, in.Files.HeadSHA) {
			in.Files.Paths, in.Files.Truncated = slices.Clone(r.Paths), in.Files.Truncated || r.Cut
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
		*budget = 0 // as fetchChangedFiles: the rest of the poll asks a GitHub that just failed nothing more
		in.FilesUnlisted, in.Files, in.GHUpdatedAt = in.Files.HeadSHA, nil, nil
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
	in.Files.Paths, in.Files.Truncated = paths, in.Files.Truncated || !complete
	return &renamedFiles{Head: in.Files.HeadSHA, Base: d.BaseRefName, BaseOid: d.BaseRefOid, Listed: slices.Clone(d.Files),
		Paths: slices.Clone(paths), Cut: !complete}
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
