package github

import (
	"context"
	"fmt"
	"strings"
)

// FileDelta is one changed file of a comparison (CompareFiles).
type FileDelta struct {
	Path         string // the file's path at head
	PreviousPath string // its path at base when renamed or copied; "" otherwise
	Status       string // added | removed | modified | renamed | copied | changed | unchanged
	// Patch holds the file's unified-diff hunks ("@@ ... @@" lines and
	// " ", "+", "-" lines); "" when GitHub left it out.
	Patch string
	// Truncated: the patch is missing or may be incomplete: GitHub sends none
	// for a binary or too large file, and a comparison listing
	// CompareFileLimit files may have dropped some, so every file of it is
	// marked.
	Truncated bool
}

// CompareFiles reads the changed files of base...head with their patches in
// one call (GET /repos/{o}/{r}/compare/{base}...{head}, per_page=1: the first
// page carries the whole file list, at most CompareFileLimit files). A
// missing repository or commit is an error matching ErrNotFound.
func (c *Client) CompareFiles(ctx context.Context, owner, repo, base, head string) ([]FileDelta, error) {
	_, files, err := c.CompareFilesStatus(ctx, owner, repo, base, head)
	return files, err
}

// CompareFilesStatus is CompareFiles that also returns the comparison's
// status as GitHub reports it: "ahead" (head descends from base),
// "identical", "behind" (base descends from head) or "diverged". The file
// list is three-dot (head's changes since the merge base), so only "ahead"
// and "identical" make it the difference between base and head.
func (c *Client) CompareFilesStatus(ctx context.Context, owner, repo, base, head string) (string, []FileDelta, error) {
	pc, err := c.compareFiles(ctx, owner, repo, base, head, 1)
	return pc.Status, pc.Files, err
}

// PushComparison is a comparison read with its commits (ComparePush).
type PushComparison struct {
	// Status is GitHub's: "ahead", "identical", "behind" or "diverged"
	// (CompareFilesStatus).
	Status string
	// Commits is total_commits: the commits head has since the merge base.
	Commits int
	// Merge: one of those commits has more than one parent, or GitHub
	// listed fewer of them than Commits (more than ComparePushCommits), so
	// a merge cannot be ruled out.
	Merge bool
	Files []FileDelta
	// Stats is what Compare reads of the same range (the commits, the
	// files, -1 at GitHub's file cap, and their additions and deletions),
	// so this comparison answers that question too.
	Stats CompareStats
}

// ComparePushCommits is the most commits ComparePush reads (one page at
// GitHub's per_page maximum).
const ComparePushCommits = 100

// ComparePush is CompareFilesStatus that also reads up to
// ComparePushCommits of the commits (per_page=100: the first page still
// carries the whole file list) to tell whether the range brings a merge
// commit: a push that merged the base branch into the PR.
func (c *Client) ComparePush(ctx context.Context, owner, repo, base, head string) (PushComparison, error) {
	return c.compareFiles(ctx, owner, repo, base, head, ComparePushCommits)
}

// compareFiles reads base...head with perPage commits on its first page.
func (c *Client) compareFiles(ctx context.Context, owner, repo, base, head string, perPage int) (PushComparison, error) {
	if err := checkRepo(owner, repo); err != nil {
		return PushComparison{}, err
	}
	for _, ref := range []string{base, head} {
		if !compareRefRe.MatchString(ref) || strings.Contains(ref, "..") {
			return PushComparison{}, fmt.Errorf("github: invalid compare ref %q", ref)
		}
	}
	var r struct {
		Status       string `json:"status"`
		TotalCommits int    `json:"total_commits"`
		Commits      []struct {
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		} `json:"commits"`
		Files []struct {
			Filename         string  `json:"filename"`
			PreviousFilename string  `json:"previous_filename"`
			Status           string  `json:"status"`
			Patch            *string `json:"patch"`
			Additions        int     `json:"additions"`
			Deletions        int     `json:"deletions"`
		} `json:"files"`
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s?per_page=%d", owner, repo, base, head, perPage)
	op := fmt.Sprintf("compare files %s/%s %s...%s", owner, repo, shortRef(base), shortRef(head))
	if err := c.rest(ctx, op, "GET", path, nil, false, &r); err != nil {
		return PushComparison{}, err
	}
	cut := len(r.Files) >= CompareFileLimit
	out := PushComparison{Status: r.Status, Commits: r.TotalCommits, Files: make([]FileDelta, 0, len(r.Files)),
		Stats: CompareStats{Commits: r.TotalCommits, Files: len(r.Files)}}
	if cut {
		out.Stats.Files = -1
	}
	for _, f := range r.Files {
		d := FileDelta{Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status, Truncated: cut || f.Patch == nil}
		if f.Patch != nil {
			d.Patch = *f.Patch
		}
		out.Files = append(out.Files, d)
		out.Stats.Additions += f.Additions
		out.Stats.Deletions += f.Deletions
	}
	out.Merge = len(r.Commits) < r.TotalCommits
	for _, cm := range r.Commits {
		if len(cm.Parents) > 1 {
			out.Merge = true
		}
	}
	return out, nil
}
