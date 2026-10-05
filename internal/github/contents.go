package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// FileAtLimit caps FileAt: a larger file is not returned.
const FileAtLimit = 512 << 10

// ErrFileTooLarge matches FileAt's error for a file above FileAtLimit; callers
// skip such a file.
var ErrFileTooLarge = errors.New("github: file too large")

// fileAtAccept asks the contents API for the file's bytes, not a JSON
// envelope with base64 content.
const fileAtAccept = "application/vnd.github.raw"

// FileAt reads the raw content of path at ref (a commit SHA, a branch or a
// tag): GET /repos/{o}/{r}/contents/{path}?ref={ref} with
// Accept: application/vnd.github.raw. The bytes come back verbatim; an empty
// file is an empty, non-nil slice. path is relative to the repository root
// ("app/models/order.rb"): empty, "." and ".." segments, backslashes and NULs
// are refused before any call, as is a ref that is not a plain ref name or
// SHA (the shape Compare accepts, without ".."). Call it only for paths that
// name files (a diff does): what GitHub answers for a directory is not file
// content. A missing file, ref or repository is an error matching
// ErrNotFound; a file above FileAtLimit an error matching ErrFileTooLarge
// (checked on the bytes received).
func (c *Client) FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !compareRefRe.MatchString(ref) || strings.Contains(ref, "..") {
		return nil, fmt.Errorf("github: invalid ref %q", ref)
	}
	escaped, err := escapeRepoPath(path)
	if err != nil {
		return nil, err
	}
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, repo, escaped, url.QueryEscape(ref))
	op := fmt.Sprintf("file %s/%s@%s:%s", owner, repo, shortRef(ref), path)
	body, err := c.restRaw(ctx, op, apiPath, fileAtAccept)
	if err != nil {
		return nil, err
	}
	if len(body) > FileAtLimit {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrFileTooLarge, op, FileAtLimit)
	}
	return body, nil
}

// escapeRepoPath validates a repository-relative file path and escapes each
// segment for a REST path, keeping the "/" separators. gh substitutes the
// placeholders :owner, :repo and :branch in any endpoint it is given, and
// url.PathEscape leaves ":" alone, so ":" is escaped too: a file named
// ":owner" must not turn into the current repository's owner.
func escapeRepoPath(path string) (string, error) {
	if path == "" || strings.ContainsAny(path, "\\\x00") {
		return "", fmt.Errorf("github: invalid file path %q", path)
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("github: invalid file path %q", path)
		}
		segs[i] = strings.ReplaceAll(url.PathEscape(s), ":", "%3A")
	}
	return strings.Join(segs, "/"), nil
}
