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
		"nodes { id number isDraft updatedAt headRefOid baseRefName isCrossRepository }",
		"id nameWithOwner pushedAt defaultBranchRef { name }",
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
	// A connection under pullRequests is priced once per pull request
	// (commits(last: 1): 101 points a page instead of 1, rateLimit(dryRun:
	// true) on 2026-10-05).
	for _, banned := range []string{"commits", "contexts", "last:"} {
		if strings.Contains(q, banned) {
			t.Errorf("radar query must not open a connection per pull request, found %q", banned)
		}
	}
	// The check rollup costs no point (1 a page with or without it) but
	// GitHub computes it for every open pull request of every repository
	// the page lists: with it the busiest owner's first page ran out of
	// time (HTTP 502/504) on 9% of the polls. CIStates reads it apart.
	for _, banned := range []string{"statusCheckRollup", "headRef "} {
		if strings.Contains(q, banned) {
			t.Errorf("radar query must not read the check rollup, found %q", banned)
		}
	}
}

// TestRadarReadsForksAndTheDefaultBranch: the radar says which pull
// requests come from a fork (CIStates skips them) and leaves their CI
// unknown; the repository's default branch comes along.
func TestRadarReadsForksAndTheDefaultBranch(t *testing.T) {
	node := func(n int, cross bool) string {
		return fmt.Sprintf(`{"id":"PR_%d","number":%d,"isDraft":false,"updatedAt":"2026-10-04T09:00:00Z","headRefOid":"h%d","baseRefName":"master","isCrossRepository":%v}`,
			n, n, n, cross)
	}
	body := compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2026-10-04T10:00:00Z"},
		"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"R_1","nameWithOwner":"talkable/app","pushedAt":"2026-10-04T09:00:00Z","defaultBranchRef":{"name":"main"},
		"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[`+node(1, false)+","+node(2, true)+`]}}]}}}}`)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: body}}}}
	repos, _, err := (&Client{Run: f}).Radar(context.Background(), "talkable")
	if err != nil || len(repos) != 1 || repos[0].DefaultBranch != "main" || len(repos[0].PRs) != 2 {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	for i, p := range repos[0].PRs {
		if p.IsCrossRepository != (i == 1) || p.CIKnown || p.CIState != "" {
			t.Errorf("PR %d = %+v", p.Number, p)
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

// syntheticRadarPage is a repositories page of the repositories from..to-1
// ("talkable/r<i>", one open PR each) that continues at next ("" = last).
func syntheticRadarPage(t *testing.T, from, to int, next string) []byte {
	t.Helper()
	var nodes []string
	for i := from; i < to; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"id":"R_%d","nameWithOwner":"talkable/r%d","pushedAt":"2026-10-05T09:00:00Z","defaultBranchRef":{"name":"main"},
			"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[`+syntheticPR(i)+`]}}`, i, i))
	}
	return compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-05T10:00:00Z"},
		"repositoryOwner":{"repositories":{"pageInfo":`+syntheticPageInfo(next)+`,"nodes":[`+strings.Join(nodes, ",")+`]}}}}`)
}

func syntheticPR(n int) string {
	return fmt.Sprintf(`{"id":"PR_%d","number":%d,"isDraft":false,"updatedAt":"2026-10-05T09:00:00Z","headRefOid":"h%d","baseRefName":"main","isCrossRepository":false}`, n, n, n)
}

func syntheticPageInfo(next string) string {
	if next == "" {
		return `{"hasNextPage":false,"endCursor":null}`
	}
	return fmt.Sprintf(`{"hasNextPage":true,"endCursor":%q}`, next)
}

// What gh prints when GitHub could not answer a query in time: a bare
// status line over GitHub's HTML error page (502), or GitHub's JSON message
// (504).
var (
	html502 = []byte("<!DOCTYPE html><html><body>Unicorn!</body></html>")
	gh502   = []byte("gh: HTTP 502\n")
	json504 = []byte(`{"message":"We couldn't respond to your request in time. Sorry about that. Please try resubmitting your request and contact us if the problem persists."}`)
	gh504   = []byte("gh: We couldn't respond to your request in time. Sorry about that. Please try resubmitting your request and contact us if the problem persists. (HTTP 504)\n")
)

// TestRadarRetriesAServerErrorAtHalfThePageSize: a page GitHub could not
// answer in time (HTTP 502) is asked again once with half as many
// repositories, and the pages after it keep the smaller size.
func TestRadarRetriesAServerErrorAtHalfThePageSize(t *testing.T) {
	var asked []string
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		asked = append(asked, fmt.Sprint(req.Variables["first"], "@", req.Variables["after"]))
		switch {
		case req.Variables["first"] == float64(100):
			return failResult(c, html502, gh502)
		case req.Variables["after"] == nil:
			return okResult(syntheticRadarPage(t, 0, 50, "c50"))
		default:
			return okResult(syntheticRadarPage(t, 50, 60, ""))
		}
	})}}
	repos, rate, err := (&Client{Run: f}).Radar(context.Background(), "talkable")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 60 {
		t.Errorf("repos = %d, want 60", len(repos))
	}
	if want := []string{"100@<nil>", "50@<nil>", "50@c50"}; !slices.Equal(asked, want) {
		t.Errorf("pages asked = %v, want %v", asked, want)
	}
	if rate.Cost != 2 {
		t.Errorf("cost = %d, want 2 (the failed call reports none)", rate.Cost)
	}
}

// TestRadarGivesUpAfterOneRetry: when the smaller page fails too, the radar
// fails for this poll with the retry's error.
func TestRadarGivesUpAfterOneRetry(t *testing.T) {
	var asked []any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		asked = append(asked, req.Variables["first"])
		return failResult(c, json504, gh504)
	})}}
	repos, _, err := (&Client{Run: f}).Radar(context.Background(), "talkable")
	if err == nil || repos != nil {
		t.Fatalf("want an error and no repositories, got %d, %v", len(repos), err)
	}
	if want := []any{float64(100), float64(50)}; !slices.Equal(asked, want) {
		t.Errorf("pages asked = %v, want %v", asked, want)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 504 || !strings.Contains(err.Error(), "retried at 50 repositories") {
		t.Errorf("err = %v", err)
	}
}

// TestRadarRetryStopsAtTheFloor: halving never goes below minPage; a page
// already at or below it is retried at its own size.
func TestRadarRetryStopsAtTheFloor(t *testing.T) {
	for _, tc := range []struct{ page, retry int }{{12, minPage}, {minPage, minPage}, {8, 8}} {
		var asked []any
		f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
			asked = append(asked, req.Variables["first"])
			return failResult(c, html502, gh502)
		})}}
		if _, _, err := (&Client{Run: f, repoPage: tc.page}).Radar(context.Background(), "talkable"); err == nil {
			t.Errorf("page %d: want an error", tc.page)
		}
		if want := []any{float64(tc.page), float64(tc.retry)}; !slices.Equal(asked, want) {
			t.Errorf("page %d: asked %v, want %v", tc.page, asked, want)
		}
	}
}

// TestRadarRetriesAPullRequestPageAtHalfSize: a repository's further pull
// request page is retried the same way, with half as many pull requests.
func TestRadarRetriesAPullRequestPageAtHalfSize(t *testing.T) {
	var asked []any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		if strings.Contains(req.Query, "repositoryOwner(login: $org)") {
			return okResult(compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-05T10:00:00Z"},
				"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"R_1","nameWithOwner":"talkable/app",
				"pushedAt":"2026-10-05T09:00:00Z","defaultBranchRef":{"name":"main"},
				"pullRequests":{"pageInfo":`+syntheticPageInfo("p1")+`,"nodes":[`+syntheticPR(1)+`]}}]}}}}`))
		}
		asked = append(asked, req.Variables["prFirst"])
		if req.Variables["prFirst"] == float64(100) {
			return failResult(c, html502, gh502)
		}
		return okResult(compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4989,"used":11,"resetAt":"2026-10-05T10:00:00Z"},
			"node":{"pullRequests":{"pageInfo":`+syntheticPageInfo("")+`,"nodes":[`+syntheticPR(2)+`]}}}}`))
	})}}
	repos, _, err := (&Client{Run: f}).Radar(context.Background(), "talkable")
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{float64(100), float64(50)}; !slices.Equal(asked, want) {
		t.Errorf("pull request pages asked = %v, want %v", asked, want)
	}
	if len(repos) != 1 || len(repos[0].PRs) != 2 {
		t.Errorf("repos = %+v", repos)
	}
}

// TestRadarRetriesOnlyWhenGitHubRanOutOfTime: other failures (no network,
// a bad query, the runner's own failure) are not retried.
func TestRadarRetriesOnlyWhenGitHubRanOutOfTime(t *testing.T) {
	for name, answer := range map[string]func(c execx.Cmd) (execx.Result, error){
		"no network": func(c execx.Cmd) (execx.Result, error) {
			return failResult(c, nil, []byte("error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com\n"))
		},
		"bad query": func(c execx.Cmd) (execx.Result, error) {
			return failResult(c, fixture(t, "gql_syntax.json"), fixture(t, "gql_syntax.stderr"))
		},
		"runner": func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, context.DeadlineExceeded },
	} {
		f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Fn: answer}}}
		if _, _, err := (&Client{Run: f}).Radar(context.Background(), "talkable"); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(f.Calls) != 1 {
			t.Errorf("%s: calls = %d, want 1", name, len(f.Calls))
		}
	}
}
