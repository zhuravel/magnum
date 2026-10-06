package github

import (
	"context"
	"fmt"
)

// PullFilesMaxPages is how many pages of 100 PullFiles reads: GitHub's own
// cap for the endpoint, 3000 files.
const PullFilesMaxPages = 30

// PullFiles reads the files a pull request changes with their patches as
// GitHub shows them in the PR's diff (GET /repos/{o}/{r}/pulls/{n}/files,
// up to PullFilesMaxPages pages of 100). A file GitHub sends no patch for
// (binary, or too large) is Truncated with an empty Patch. complete is false
// when the last page read was full: the PR changes more files than were
// read. A missing repository or pull request is an error matching
// ErrNotFound.
func (c *Client) PullFiles(ctx context.Context, owner, repo string, number int) (files []FileDelta, complete bool, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, false, err
	}
	if number < 1 {
		return nil, false, fmt.Errorf("github: invalid pull request number %d", number)
	}
	for page := 1; page <= PullFilesMaxPages; page++ {
		var entries []struct {
			Filename         string  `json:"filename"`
			PreviousFilename string  `json:"previous_filename"`
			Status           string  `json:"status"`
			Patch            *string `json:"patch"`
		}
		path := fmt.Sprintf("repos/%s/%s/pulls/%d/files?per_page=%d&page=%d", owner, repo, number, filesPerPage, page)
		op := fmt.Sprintf("pull files %s/%s#%d page %d", owner, repo, number, page)
		if err := c.rest(ctx, op, "GET", path, nil, false, &entries); err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			d := FileDelta{Path: e.Filename, PreviousPath: e.PreviousFilename, Status: e.Status, Truncated: e.Patch == nil}
			if e.Patch != nil {
				d.Patch = *e.Patch
			}
			files = append(files, d)
		}
		if len(entries) < filesPerPage {
			return files, true, nil
		}
	}
	return files, false, nil
}

// PullSHAs reads a pull request's base and head commits as REST reports
// them (GET /repos/{o}/{r}/pulls/{n}: base.sha, head.sha). A missing
// repository or pull request is an error matching ErrNotFound.
func (c *Client) PullSHAs(ctx context.Context, owner, repo string, number int) (base, head string, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return "", "", err
	}
	if number < 1 {
		return "", "", fmt.Errorf("github: invalid pull request number %d", number)
	}
	var pr struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number)
	op := fmt.Sprintf("pull %s/%s#%d", owner, repo, number)
	if err := c.rest(ctx, op, "GET", path, nil, false, &pr); err != nil {
		return "", "", err
	}
	return pr.Base.SHA, pr.Head.SHA, nil
}
