package github

import (
	"context"
	"fmt"
	"strings"
)

// Pull is a pull request as REST reports it (GET /repos/{o}/{r}/pulls/{n}):
// what `magnum comment` checks before it writes.
type Pull struct {
	Number int
	Title  string
	State  string // open | closed
	URL    string // html_url
	// Author is who opened it, in Account form (REST logins of Apps end
	// in "[bot]"); "" for a deleted account.
	Author string
	// RequestedReviewers are the users asked for a review now (REST
	// logins); GitHub drops a user from the list once they review.
	RequestedReviewers []string
}

// pullJSON is the part of GitHub's pull request the client reads.
type pullJSON struct {
	Number             int         `json:"number"`
	Title              string      `json:"title"`
	State              string      `json:"state"`
	HTMLURL            string      `json:"html_url"`
	User               *restUser   `json:"user"`
	RequestedReviewers []*restUser `json:"requested_reviewers"`
}

// restUser is a REST user object: an App's login carries its "[bot]".
type restUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// account is u's login in Account form, "" for none.
func (u *restUser) account() string {
	if u == nil {
		return ""
	}
	return Account(u.Login, u.Type)
}

func (p pullJSON) pull() Pull {
	out := Pull{Number: p.Number, Title: p.Title, State: p.State, URL: p.HTMLURL, Author: p.User.account()}
	for _, u := range p.RequestedReviewers {
		if l := u.account(); l != "" {
			out.RequestedReviewers = append(out.RequestedReviewers, l)
		}
	}
	return out
}

// Pull reads pull request number (GET /repos/{o}/{r}/pulls/{n}). A missing
// repository or pull request is an error matching ErrNotFound.
func (c *Client) Pull(ctx context.Context, owner, repo string, number int) (Pull, error) {
	if err := checkRepo(owner, repo); err != nil {
		return Pull{}, err
	}
	if number < 1 {
		return Pull{}, fmt.Errorf("github: invalid pull request number %d", number)
	}
	var out pullJSON
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number)
	if err := c.rest(ctx, fmt.Sprintf("pull %s/%s#%d", owner, repo, number), "GET", path, nil, false, &out); err != nil {
		return Pull{}, err
	}
	return out.pull(), nil
}

// RequestReviewers asks the users logins for a review of pull request number
// (POST /repos/{o}/{r}/pulls/{n}/requested_reviewers) as the client's
// identity, the request {"reviewers": […]} sent as JSON on gh's stdin.
// Each login is a user's (ValidLogin: no App, no team, no "@"); GitHub
// refuses the PR's author and anyone who is no collaborator with HTTP 422.
// Asking someone who reviewed already asks again. It is marked Mutates, so
// execx.DryRun only plans it.
func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if number < 1 {
		return fmt.Errorf("github: invalid pull request number %d", number)
	}
	if len(logins) == 0 {
		return fmt.Errorf("github: a review request on %s/%s#%d needs a login", owner, repo, number)
	}
	for _, l := range logins {
		if !ValidLogin(l) {
			return fmt.Errorf("github: %q is not a user's login", l)
		}
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/requested_reviewers", owner, repo, number)
	op := fmt.Sprintf("request reviews %s/%s#%d", owner, repo, number)
	return c.restInput(ctx, op, "POST", path, map[string][]string{"reviewers": logins}, true, nil)
}

// UpdatePullTitle sets the title of pull request number (PATCH
// /repos/{o}/{r}/pulls/{n}) as the client's identity, the request
// {"title": …} sent as JSON on gh's stdin so the text never reaches argv,
// and returns the pull request as GitHub answered. A title that is blank
// or spans lines is refused before gh runs. It is marked Mutates, so
// execx.DryRun only plans it.
func (c *Client) UpdatePullTitle(ctx context.Context, owner, repo string, number int, title string) (Pull, error) {
	if err := checkRepo(owner, repo); err != nil {
		return Pull{}, err
	}
	if number < 1 {
		return Pull{}, fmt.Errorf("github: invalid pull request number %d", number)
	}
	if strings.TrimSpace(title) == "" || strings.ContainsAny(title, "\r\n") {
		return Pull{}, fmt.Errorf("github: the title of %s/%s#%d must be one line of text", owner, repo, number)
	}
	var out pullJSON
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number)
	op := fmt.Sprintf("retitle %s/%s#%d", owner, repo, number)
	if err := c.restInput(ctx, op, "PATCH", path, map[string]string{"title": title}, true, &out); err != nil {
		return Pull{}, err
	}
	return out.pull(), nil
}
