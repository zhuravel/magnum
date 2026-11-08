package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
)

// Deployment is a deployment of an environment as Deployments lists it.
type Deployment struct {
	ID int64
	// SHA is the commit GitHub resolved the deployment's ref to; Ref is the
	// ref as created (a branch, a tag or a sha).
	SHA, Ref    string
	Environment string
	CreatedAt   time.Time
}

// deploymentsPage is how many deployments Deployments reads: the newest of
// an environment, of which the newest successful one is what counts.
const deploymentsPage = 20

// statusesPage is how many statuses DeploymentStatuses reads of one
// deployment: a deploy writes one or two.
const statusesPage = 10

// Deployments lists the deployments of environment of owner/repo, newest
// first, one page of deploymentsPage: GET /repos/{o}/{r}/deployments, sending
// etag as If-None-Match ("" = none). A 304 answers notModified with no list
// and etag as it was; else newETag is the answer's ETag. The identity needs
// Deployments: read: without it GitHub answers 403 or 404 (ErrForbidden,
// ErrNotFound). An empty environment, or one or an ETag with a control
// character, is refused before any call.
func (c *Client) Deployments(ctx context.Context, owner, repo, environment, etag string) (list []Deployment, newETag string, notModified bool, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, "", false, err
	}
	if strings.TrimSpace(environment) == "" || strings.ContainsFunc(environment, unicode.IsControl) {
		return nil, "", false, fmt.Errorf("github: invalid environment %q", environment)
	}
	if strings.ContainsFunc(etag, unicode.IsControl) || len(etag) > 200 {
		return nil, "", false, fmt.Errorf("github: invalid ETag %q", etag)
	}
	var data []struct {
		ID          int64     `json:"id"`
		SHA         string    `json:"sha"`
		Ref         string    `json:"ref"`
		Environment string    `json:"environment"`
		CreatedAt   time.Time `json:"created_at"`
	}
	path := fmt.Sprintf("repos/%s/%s/deployments?environment=%s&per_page=%d", owner, repo, url.QueryEscape(environment), deploymentsPage)
	op := fmt.Sprintf("deployments %s/%s:%s", owner, repo, environment)
	newETag, notModified, err = c.restETag(ctx, op, path, etag, &data)
	if err != nil || notModified {
		return nil, newETag, notModified, err
	}
	list = make([]Deployment, 0, len(data))
	for _, d := range data {
		list = append(list, Deployment{ID: d.ID, SHA: d.SHA, Ref: d.Ref, Environment: d.Environment, CreatedAt: d.CreatedAt})
	}
	return list, newETag, false, nil
}

// DeploymentStatuses lists the states of deployment id's statuses, newest
// first, one page of statusesPage: GET
// /repos/{o}/{r}/deployments/{id}/statuses. A state is success, failure,
// error, inactive (a newer deployment replaced it), in_progress, queued or
// pending. No description is read.
func (c *Client) DeploymentStatuses(ctx context.Context, owner, repo string, id int64) ([]string, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, fmt.Errorf("github: invalid deployment %d", id)
	}
	var data []struct {
		State string `json:"state"`
	}
	path := fmt.Sprintf("repos/%s/%s/deployments/%d/statuses?per_page=%d", owner, repo, id, statusesPage)
	op := fmt.Sprintf("deployment statuses %s/%s#%d", owner, repo, id)
	if err := c.rest(ctx, op, "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	states := make([]string, 0, len(data))
	for _, s := range data {
		states = append(states, s.State)
	}
	return states, nil
}

// restETag runs one REST GET with gh's --include, so the answer's headers
// come before its body, and If-None-Match: etag when etag is not "". A 304
// (gh exits 1, "HTTP 304", an expected answer) reports notModified, etag
// kept and out untouched; a 200 decodes the body into out and returns the
// answer's ETag. Errors map as in rest, and after a 401 it calls Reauth
// and repeats the call once.
func (c *Client) restETag(ctx context.Context, op, path, etag string, out any) (string, bool, error) {
	newETag, notModified, err := c.restETagOnce(ctx, op, path, etag, out)
	retry, err := c.reauthorize(ctx, err)
	if retry {
		return c.restETagOnce(ctx, op, path, etag, out)
	}
	return newETag, notModified, err
}

func (c *Client) restETagOnce(ctx context.Context, op, path, etag string, out any) (string, bool, error) {
	args := append([]string{"api", path}, ghHost...)
	args = append(args, "--include")
	if etag != "" {
		args = append(args, "-H", "If-None-Match: "+etag)
	}
	unchanged := func(res execx.Result) bool { return res.Code != 0 && httpStatus(res.Stderr) == 304 }
	res, err := c.gh(ctx, op, args, nil, false, unchanged)
	if err != nil && unchanged(res) {
		c.meter(ctx, op, false, RateLimit{}, nil)
		return etag, true, nil
	}
	head, body := splitHeaders(res.Stdout)
	res.Stdout = body
	err = restFailure(op, res, err)
	c.meter(ctx, op, false, RateLimit{}, err)
	if err != nil {
		return "", false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "", false, fmt.Errorf("github %s: empty response", op)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", false, fmt.Errorf("github %s: decode response: %w", op, err)
	}
	return headerValue(head, "ETag"), false, nil
}

// splitHeaders splits what `gh api --include` printed into the status line
// with the headers, and the body after the blank line (CRLF or LF). Output
// that does not start with a status line is all body.
func splitHeaders(out []byte) (head, body []byte) {
	if !bytes.HasPrefix(out, []byte("HTTP/")) {
		return nil, out
	}
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if head, body, ok := bytes.Cut(out, sep); ok {
			return head, body
		}
	}
	return out, nil
}

// headerValue is the value of header name (case ignored) in head, "" when
// it has none.
func headerValue(head []byte, name string) string {
	for line := range strings.Lines(string(head)) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
