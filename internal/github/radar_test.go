package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	pagedRepoID     = "R_kgDOTEST0001" // zhuravel/zhuravel, 4 open PRs, prFirst 3 -> paginated
	pagedRepoCursor = "cursor-repo-page-1"
	pagedPRCursor   = "cursor-pr-page-1"
)

// radarFake serves the captured zhuravel radar (repositories first:8, pullRequests first:3).
func radarFake(t *testing.T) *execx.Fake {
	return &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		switch {
		case strings.Contains(req.Query, "repositoryOwner(login: $org)"):
			if req.Variables["org"] != "zhuravel" || req.Variables["first"] != float64(8) || req.Variables["prFirst"] != float64(3) {
				t.Errorf("radar variables = %v", req.Variables)
			}
			switch req.Variables["after"] {
			case nil:
				return okResult(fixture(t, "radar_p1.json"))
			case pagedRepoCursor:
				return okResult(fixture(t, "radar_p2.json"))
			}
		case strings.Contains(req.Query, "node(id: $id)"):
			if req.Variables["id"] == pagedRepoID && req.Variables["after"] == pagedPRCursor && req.Variables["prFirst"] == float64(3) {
				return okResult(fixture(t, "radar_prs_p2.json"))
			}
		}
		t.Errorf("unexpected request: %s %v", oneLine(req.Query), req.Variables)
		return execx.Result{}, errors.New("unexpected request")
	})}}
}

func TestRadarPaginatesReposAndPullRequests(t *testing.T) {
	f := radarFake(t)
	c := &Client{Run: f, repoPage: 8, prPage: 3}
	repos, rate, err := c.Radar(context.Background(), "zhuravel")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 16 {
		t.Fatalf("repos = %d, want 16", len(repos))
	}
	if len(f.Calls) != 3 {
		t.Errorf("gh calls = %d, want 3 (2 repo pages + 1 PR page)", len(f.Calls))
	}
	byName := map[string]RepoRadar{}
	for _, r := range repos {
		byName[r.NameWithOwner] = r
	}
	dm := byName["zhuravel/zhuravel"]
	if dm.NodeID != pagedRepoID || !dm.PushedAt.Equal(time.Date(2026, 10, 2, 18, 45, 54, 0, time.UTC)) {
		t.Errorf("zhuravel repo = %+v", dm)
	}
	var nums []int
	for _, p := range dm.PRs {
		nums = append(nums, p.Number)
	}
	if got := fmt.Sprint(nums); got != "[649 725 727 728]" {
		t.Errorf("zhuravel PRs = %v", nums)
	}
	last := dm.PRs[3]
	want := PRRadar{
		NodeID:      "PR_kwDOLQ7yrc8AAAABGVfI2Q",
		Number:      728,
		IsDraft:     true,
		UpdatedAt:   time.Date(2026, 10, 2, 18, 46, 18, 0, time.UTC),
		HeadRefOid:  "ec92fb159de3f396691cfbc38e7e80019907f38b",
		BaseRefName: "main",
	}
	if !last.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("updatedAt = %v", last.UpdatedAt)
	}
	last.UpdatedAt = want.UpdatedAt
	if last != want {
		t.Errorf("PR 728 = %+v, want %+v", last, want)
	}
	if n := len(byName["zhuravel/site"].PRs); n != 2 {
		t.Errorf("site PRs = %d", n)
	}
	if n := len(byName["zhuravel/widgets"].PRs); n != 0 {
		t.Errorf("widgets PRs = %d", n)
	}
	// Three calls cost 1 each; the snapshot is the most conservative one seen.
	wantRate := RateLimit{Limit: 5000, Cost: 3, Remaining: 4978, Used: 22, ResetAt: time.Date(2026, 10, 2, 21, 50, 10, 0, time.UTC)}
	if rate != wantRate {
		t.Errorf("rate = %+v, want %+v", rate, wantRate)
	}
}

func TestRadarQueryIsScalarOnly(t *testing.T) {
	var queries []string
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		queries = append(queries, req.Query)
		if req.Variables["first"] != float64(100) || req.Variables["prFirst"] != float64(100) {
			t.Errorf("default page sizes = %v", req.Variables)
		}
		if _, ok := req.Variables["after"]; ok {
			t.Errorf("first page must not send a cursor: %v", req.Variables)
		}
		return okResult(compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2026-10-02T21:50:10Z"},
			"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`))
	})}}
	repos, _, err := (&Client{Run: f}).Radar(context.Background(), "talkable")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v", repos)
	}
	q := oneLine(queries[0])
	for _, want := range []string{
		"repositoryOwner(login: $org)",
		"ownerAffiliations: OWNER",
		"isArchived: false",
		"pullRequests(states: OPEN, first: $prFirst)",
		"nodes { id number isDraft updatedAt headRefOid baseRefName }",
		"id nameWithOwner pushedAt",
		"pageInfo { hasNextPage endCursor }",
		"rateLimit { limit cost remaining used resetAt }",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("radar query lacks %q:\n%s", want, q)
		}
	}
	for _, banned := range []string{"title", "author", "labels", "body"} {
		if strings.Contains(q, banned) {
			t.Errorf("radar query must stay scalar-only, found %q", banned)
		}
	}
}

func TestRadarUnknownOwner(t *testing.T) {
	// repositoryOwner answers null without errors for an unknown login; an
	// explicit NOT_FOUND error is handled the same way.
	for name, body := range map[string][]byte{
		"null owner": compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2026-10-02T21:50:10Z"},"repositoryOwner":null}}`),
		"not found": compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2026-10-02T21:50:10Z"},"repositoryOwner":null},
			"errors":[{"type":"NOT_FOUND","path":["repositoryOwner"],"message":"Could not resolve to a RepositoryOwner with the login of 'nope'."}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			code := 0
			if name == "not found" {
				code = 1
			}
			f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: body, Code: code}}}}
			_, _, err := (&Client{Run: f}).Radar(context.Background(), "nope")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

// ownerFake answers like GitHub does: organization(login:) knows only
// organizations (it is NOT_FOUND for a user), repositoryOwner(login:) knows
// both. Each owner has exactly one repository, "<login>/app", with one PR.
func ownerFake(t *testing.T, users, orgs []string) *execx.Fake {
	rate := `"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2026-10-02T21:50:10Z"}`
	data := func(key, login string) string {
		return fmt.Sprintf(`{"data":{%s,%q:{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
			{"id":"R_%s","nameWithOwner":"%s/app","pushedAt":"2026-10-02T18:45:54Z","pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},
			"nodes":[{"id":"PR_%s","number":7,"isDraft":false,"updatedAt":"2026-10-02T18:46:18Z","headRefOid":"abc","baseRefName":"main"}]}}]}}}}`,
			rate, key, login, login, login)
	}
	return &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		login, _ := req.Variables["org"].(string)
		isOrg, isUser := slices.Contains(orgs, login), slices.Contains(users, login)
		switch {
		case strings.Contains(req.Query, "organization(login: $org)"):
			if isOrg {
				return okResult(compact(t, data("organization", login)))
			}
			body := compact(t, `{"data":{`+rate+`,"organization":null},"errors":[{"type":"NOT_FOUND","path":["organization"],"message":"Could not resolve to an Organization with the login of '`+login+`'."}]}`)
			return failResult(c, body, []byte("gh: Could not resolve to an Organization\n"))
		case strings.Contains(req.Query, "repositoryOwner(login: $org)"):
			if isOrg || isUser {
				return okResult(compact(t, data("repositoryOwner", login)))
			}
			return okResult(compact(t, `{"data":{`+rate+`,"repositoryOwner":null}}`))
		}
		t.Errorf("unexpected request: %s", oneLine(req.Query))
		return execx.Result{}, errors.New("unexpected request")
	})}}
}

func TestRadarUserAndOrganizationOwners(t *testing.T) {
	f := ownerFake(t, []string{"example-user"}, []string{"example-org"})
	c := &Client{Run: f}
	for _, owner := range []string{"example-user", "example-org"} {
		repos, _, err := c.Radar(context.Background(), owner)
		if err != nil {
			t.Fatalf("%s: %v", owner, err)
		}
		if len(repos) != 1 || repos[0].NameWithOwner != owner+"/app" || len(repos[0].PRs) != 1 || repos[0].PRs[0].Number != 7 {
			t.Errorf("%s: repos = %+v", owner, repos)
		}
	}
	if _, _, err := c.Radar(context.Background(), "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown owner: err = %v, want ErrNotFound", err)
	}
}

func TestRadarFailsWholeWhenAPageFails(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		if req.Variables["after"] == pagedRepoCursor {
			return failResult(c, nil, []byte("gh: Something went wrong (HTTP 502)\n"))
		}
		if strings.Contains(req.Query, "node(id: $id)") {
			return okResult(fixture(t, "radar_prs_p2.json"))
		}
		return okResult(fixture(t, "radar_p1.json"))
	})}}
	repos, rate, err := (&Client{Run: f, repoPage: 8, prPage: 3}).Radar(context.Background(), "zhuravel")
	if err == nil {
		t.Fatal("want error")
	}
	if repos != nil {
		t.Errorf("partial radar returned %d repos; a half poll would look like closed PRs", len(repos))
	}
	// Repository page 1 and the pull request page it triggered succeeded
	// (1 point each); their budget observation must survive the failure of page 2.
	if rate.Cost != 2 || rate.Remaining != 4978 || rate.Limit != 5000 {
		t.Errorf("rate after a later-page failure = %+v, want the first two calls'", rate)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 {
		t.Errorf("err = %v", err)
	}
}

func TestRadarStopsOnStuckCursor(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		return okResult(compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2026-10-02T21:50:10Z"},
			"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":null},"nodes":[]}}}}`))
	})}}
	if _, _, err := (&Client{Run: f}).Radar(context.Background(), "talkable"); err == nil {
		t.Fatal("hasNextPage without a cursor must fail instead of looping")
	}
	if len(f.Calls) != 1 {
		t.Errorf("calls = %d", len(f.Calls))
	}
}

func TestRadarKeepsRateLimitWhenPullRequestPageFails(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		switch {
		case strings.Contains(req.Query, "node(id: $id)"):
			return failResult(c, nil, []byte("gh: Something went wrong (HTTP 502)\n"))
		case req.Variables["after"] == pagedRepoCursor:
			return okResult(fixture(t, "radar_p2.json"))
		}
		return okResult(fixture(t, "radar_p1.json"))
	})}}
	repos, rate, err := (&Client{Run: f, repoPage: 8, prPage: 3}).Radar(context.Background(), "example")
	if err == nil || repos != nil {
		t.Fatalf("want an error and no partial list, got %d repos, err %v", len(repos), err)
	}
	// Only repository page 1 succeeded before its pull request page failed.
	if rate.Cost != 1 || rate.Remaining != 4980 || rate.ResetAt.IsZero() {
		t.Errorf("rate = %+v, want page 1's", rate)
	}
}
