package github

import (
	"context"
	"fmt"
)

const (
	filesPerPage  = 100
	filesMaxPages = 3
)

// ListFiles reads the files a pull request changes: GET
// /repos/{o}/{r}/pulls/{n}/files, up to three pages of 100. A rename
// contributes both its new and its previous name (moving a file out of src/
// into docs/ still touches src/). complete is false when the third page is
// full: the PR changes more files than were read and a caller must not draw
// conclusions from the list (GitHub itself caps this endpoint at 3000 files).
// A missing repository or pull request is an error matching ErrNotFound.
func (c *Client) ListFiles(ctx context.Context, owner, repo string, number int) (files []string, complete bool, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, false, err
	}
	if number < 1 {
		return nil, false, fmt.Errorf("github: invalid pull request number %d", number)
	}
	for page := 1; page <= filesMaxPages; page++ {
		var entries []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		path := fmt.Sprintf("repos/%s/%s/pulls/%d/files?per_page=%d&page=%d", owner, repo, number, filesPerPage, page)
		op := fmt.Sprintf("list files %s/%s#%d page %d", owner, repo, number, page)
		if err := c.rest(ctx, op, "GET", path, nil, false, &entries); err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			if e.Filename != "" {
				files = append(files, e.Filename)
			}
			if e.PreviousFilename != "" && e.PreviousFilename != e.Filename {
				files = append(files, e.PreviousFilename)
			}
		}
		if len(entries) < filesPerPage {
			return files, true, nil
		}
	}
	return files, false, nil
}
