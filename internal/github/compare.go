package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// CompareStats is the size of base...head as GitHub's compare API reports it
// (three-dot: what head adds on top of its merge base with base).
type CompareStats struct {
	Commits   int // total_commits
	Files     int // changed files; -1 when GitHub truncated the file list (CompareFileLimit)
	Additions int // summed over the listed files: a lower bound when Files is -1
	Deletions int // likewise
}

// CompareFileLimit is the most files GitHub lists for one comparison; a
// comparison listing exactly this many is treated as truncated.
const CompareFileLimit = 300

// compareRefRe accepts commit SHAs and plain ref names; anything that could
// change the REST path (spaces, "?", "#", "..") is refused.
var compareRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// Compare reads GET /repos/{o}/{r}/compare/{base}...{head} with per_page=1:
// total_commits still counts every commit and the first page carries the whole
// (≤300) file list, but only one commit object comes back. A missing
// repository or commit is an error matching ErrNotFound.
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (CompareStats, error) {
	if err := checkRepo(owner, repo); err != nil {
		return CompareStats{}, err
	}
	for _, ref := range []string{base, head} {
		if !compareRefRe.MatchString(ref) || strings.Contains(ref, "..") {
			return CompareStats{}, fmt.Errorf("github: invalid compare ref %q", ref)
		}
	}
	var r struct {
		TotalCommits int `json:"total_commits"`
		Files        []struct {
			Additions int `json:"additions"`
			Deletions int `json:"deletions"`
		} `json:"files"`
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s?per_page=1", owner, repo, base, head)
	op := fmt.Sprintf("compare %s/%s %s...%s", owner, repo, shortRef(base), shortRef(head))
	if err := c.rest(ctx, op, "GET", path, nil, false, &r); err != nil {
		return CompareStats{}, err
	}
	out := CompareStats{Commits: r.TotalCommits, Files: len(r.Files)}
	for _, f := range r.Files {
		out.Additions += f.Additions
		out.Deletions += f.Deletions
	}
	if len(r.Files) >= CompareFileLimit {
		out.Files = -1
	}
	return out, nil
}

// shortRef abbreviates a 40-hex SHA for operation labels.
func shortRef(s string) string {
	if len(s) == 40 {
		return s[:7]
	}
	return s
}
