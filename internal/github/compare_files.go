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
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	for _, ref := range []string{base, head} {
		if !compareRefRe.MatchString(ref) || strings.Contains(ref, "..") {
			return nil, fmt.Errorf("github: invalid compare ref %q", ref)
		}
	}
	var r struct {
		Files []struct {
			Filename         string  `json:"filename"`
			PreviousFilename string  `json:"previous_filename"`
			Status           string  `json:"status"`
			Patch            *string `json:"patch"`
		} `json:"files"`
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s?per_page=1", owner, repo, base, head)
	op := fmt.Sprintf("compare files %s/%s %s...%s", owner, repo, shortRef(base), shortRef(head))
	if err := c.rest(ctx, op, "GET", path, nil, false, &r); err != nil {
		return nil, err
	}
	cut := len(r.Files) >= CompareFileLimit
	out := make([]FileDelta, 0, len(r.Files))
	for _, f := range r.Files {
		d := FileDelta{Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status, Truncated: cut || f.Patch == nil}
		if f.Patch != nil {
			d.Patch = *f.Patch
		}
		out = append(out, d)
	}
	return out, nil
}
