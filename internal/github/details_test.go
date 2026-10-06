package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

func TestDetailsFixture(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		// gh exits 1 when any alias is NOT_FOUND but still prints data.
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1},
	}}}
	c := &Client{Run: f}
	got, missing, err := c.Details(context.Background(), "talkable", "talkable", []int{11975, 11983, 11980, 11982, 99999999})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []int{99999999}) {
		t.Errorf("missing = %v", missing)
	}
	if len(got) != 4 {
		t.Fatalf("details = %d", len(got))
	}

	req := decodeReq(t, f.Calls[0])
	if req.Variables["owner"] != "talkable" || req.Variables["name"] != "talkable" {
		t.Errorf("variables = %v", req.Variables)
	}
	if a := aliases(req.Query); !reflect.DeepEqual(a, []string{"11975", "11980", "11982", "11983", "99999999"}) {
		t.Errorf("aliases = %v", a)
	}

	bot := got[11975]
	if bot.AuthorLogin != "dependabot" || bot.AuthorType != "Bot" || !IsBot(bot.AuthorType, bot.AuthorLogin) {
		t.Errorf("dependabot author = %q/%q", bot.AuthorLogin, bot.AuthorType)
	}
	if !SameLogin(bot.AuthorLogin, "dependabot[bot]") {
		t.Error("GraphQL bot login must match the REST [bot] login")
	}
	if !reflect.DeepEqual(bot.Labels, []string{"dependencies", "github_actions"}) {
		t.Errorf("labels = %v", bot.Labels)
	}
	if !reflect.DeepEqual(bot.ReviewRequests, []Reviewer{{Type: "User", Login: "zhuravel"}}) {
		t.Errorf("review requests = %+v", bot.ReviewRequests)
	}
	wantLatest := []LatestReview{{
		State:       "COMMENTED",
		SubmittedAt: time.Date(2026, 10, 1, 2, 34, 24, 0, time.UTC),
		AuthorLogin: "chatgpt-codex-connector",
		AuthorType:  "Bot",
		CommitOid:   "f2b92820612a2a8f6fb2e5436cf71db085eea1c9",
	}}
	if !reflect.DeepEqual(bot.LatestReviews, wantLatest) {
		t.Errorf("latest reviews = %+v", bot.LatestReviews)
	}
	if bot.NodeID != "PR_kwDOABr8DM8AAAABGAyEvA" || bot.Number != 11975 ||
		bot.URL != "https://github.com/talkable/talkable/pull/11975" ||
		bot.HeadRefName != "dependabot/github_actions/github-actions-b8fa58e0c8" || bot.BaseRefName != "master" ||
		bot.HeadRefOid != "b59dd4879e8d454f83a1284a24ec790a90594a2e" || bot.State != "OPEN" ||
		bot.Merged || bot.IsDraft || bot.IsCrossRepository || !bot.MergedAt.IsZero() || !bot.ClosedAt.IsZero() ||
		!bot.UpdatedAt.Equal(time.Date(2026, 10, 2, 9, 4, 36, 0, time.UTC)) ||
		!strings.HasPrefix(bot.Title, "Bump the github-actions group") {
		t.Errorf("11975 = %+v", bot)
	}

	user := got[11983]
	if user.AuthorLogin != "zhuravel" || user.AuthorType != "User" || IsBot(user.AuthorType, user.AuthorLogin) {
		t.Errorf("user author = %q/%q", user.AuthorLogin, user.AuthorType)
	}
	if len(user.ReviewRequests) != 2 || user.ReviewRequests[1].Login != "a-long-login-1" {
		t.Errorf("review requests = %+v", user.ReviewRequests)
	}
	if user.LatestReviews == nil || len(user.LatestReviews) != 0 {
		t.Errorf("no reviews must be an empty, non-nil slice: %#v", user.LatestReviews)
	}

	if !got[11980].IsDraft || !reflect.DeepEqual(got[11980].Labels, []string{"WIP", "Review effort 3/5"}) {
		t.Errorf("11980 = %+v", got[11980])
	}
	merged := got[11982]
	mergedAt := time.Date(2026, 10, 2, 14, 24, 50, 0, time.UTC)
	if merged.State != "MERGED" || !merged.Merged || !merged.MergedAt.Equal(mergedAt) || !merged.ClosedAt.Equal(mergedAt) {
		t.Errorf("11982 = %+v", merged)
	}
}

func TestDetailsTeamAndGhostReviewers(t *testing.T) {
	body := compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-02T21:50:10Z"},
	"repository":{"p5":{"id":"PR_x","number":5,"title":"t","url":"u","author":null,"labels":{"nodes":[]},
	"headRefName":"h","baseRefName":"master","isCrossRepository":true,"state":"OPEN","merged":false,"mergedAt":null,
	"closedAt":null,"updatedAt":"2026-10-02T09:04:36Z","isDraft":false,"headRefOid":"abc",
	"reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"Team","slug":"backend"}},{"requestedReviewer":{"__typename":"Bot","login":"copilot"}},{"requestedReviewer":null}]},
	"latestReviews":{"nodes":[{"state":"APPROVED","submittedAt":"2026-10-01T02:34:24Z","author":null,"commit":null}]}}}}}`)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: body}}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "o", "r", []int{5})
	if err != nil || len(missing) != 0 {
		t.Fatalf("err = %v missing = %v", err, missing)
	}
	d := got[5]
	if d.AuthorLogin != "" || d.AuthorType != "" || !d.IsCrossRepository {
		t.Errorf("ghost author = %+v", d)
	}
	want := []Reviewer{{Type: "Team", Login: "backend"}, {Type: "Bot", Login: "copilot"}}
	if !reflect.DeepEqual(d.ReviewRequests, want) {
		t.Errorf("review requests = %+v", d.ReviewRequests)
	}
	if len(d.LatestReviews) != 1 || d.LatestReviews[0].AuthorLogin != "" || d.LatestReviews[0].CommitOid != "" {
		t.Errorf("latest = %+v", d.LatestReviews)
	}
}

// synthBatch answers any batched query with a minimal PR per alias, leaving
// the numbers in absent as NOT_FOUND.
func synthBatch(t *testing.T, absent map[int]bool, sizes *[]int) execx.Rule {
	return gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		as := aliases(req.Query)
		*sizes = append(*sizes, len(as))
		var parts, errs []string
		for _, a := range as {
			var n int
			fmt.Sscan(a, &n)
			if absent[n] {
				parts = append(parts, fmt.Sprintf(`"p%d":null`, n))
				errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":["repository","p%d"],"message":"Could not resolve to a PullRequest with the number of %d."}`, n, n))
				continue
			}
			parts = append(parts, fmt.Sprintf(`"p%d":{"id":"PR_%d","number":%d,"state":"OPEN","merged":false,"mergedAt":null,"closedAt":null,"headRefOid":"sha%d","labels":{"nodes":[]},"reviewRequests":{"nodes":[]},"latestReviews":{"nodes":[]}}`, n, n, n, n))
		}
		body := `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4000,"used":1000,"resetAt":"2026-10-02T21:50:10Z"},"repository":{` + strings.Join(parts, ",") + `}}`
		if len(errs) > 0 {
			body += `,"errors":[` + strings.Join(errs, ",") + `]`
			return failResult(c, []byte(body+"}"), []byte("gh: Could not resolve to a PullRequest\n"))
		}
		return okResult([]byte(body + "}"))
	})
}

func TestDetailsBatchesOf40(t *testing.T) {
	var sizes []int
	absent := map[int]bool{7: true, 83: true}
	f := &execx.Fake{Rules: []execx.Rule{synthBatch(t, absent, &sizes)}}
	var numbers []int
	for n := 85; n >= 1; n-- {
		numbers = append(numbers, n)
	}
	numbers = append(numbers, 3, 3, 50) // duplicates are queried once
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", numbers)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sizes, []int{40, 40, 5}) {
		t.Errorf("batch sizes = %v", sizes)
	}
	if len(got) != 83 || !reflect.DeepEqual(missing, []int{7, 83}) {
		t.Errorf("got %d, missing %v", len(got), missing)
	}
	if got[42].HeadRefOid != "sha42" || got[42].Number != 42 {
		t.Errorf("42 = %+v", got[42])
	}
}

func TestDetailsEmptyAndInvalid(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	got, missing, err := c.Details(context.Background(), "talkable", "talkable", nil)
	if err != nil || len(got) != 0 || len(missing) != 0 || got == nil {
		t.Errorf("empty input = %v %v %v", got, missing, err)
	}
	if _, _, err := c.Details(context.Background(), "talkable", "talkable", []int{1, 0}); err == nil {
		t.Error("non-positive number accepted")
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d", len(f.Calls))
	}
}

func TestDetailsNonNotFoundErrorFails(t *testing.T) {
	body := compact(t, `{"data":{"repository":{"p1":null}},"errors":[{"type":"FORBIDDEN","path":["repository","p1"],"message":"Resource not accessible by integration"}]}`)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: body, Code: 1}}}}
	_, _, err := (&Client{Run: f}).Details(context.Background(), "o", "r", []int{1})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("FORBIDDEN must not read as not found")
	}
}

func TestConfirmStatesFixture(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "confirm.json"), Stderr: fixture(t, "confirm.stderr"), Code: 1},
	}}}
	got, notFound, err := (&Client{Run: f}).ConfirmStates(context.Background(), "talkable", "talkable", []int{11982, 11965, 11984, 99999999})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(notFound, []int{99999999}) {
		t.Errorf("notFound = %v", notFound)
	}
	want := map[int]PRState{
		11982: {State: "MERGED", Merged: true, MergedAt: time.Date(2026, 10, 2, 14, 24, 50, 0, time.UTC), ClosedAt: time.Date(2026, 10, 2, 14, 24, 50, 0, time.UTC), HeadRefOid: "d2bdbcf862fbc3f586c00b0cb66f6db6b38bdcb9",
			MergeCommitOid: "5f1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d5"},
		11965: {State: "CLOSED", ClosedAt: time.Date(2026, 9, 30, 14, 3, 37, 0, time.UTC), HeadRefOid: "1eb72bdac4ebb6b4072b60835ab4b78d1216fdc9"},
		11984: {State: "OPEN", HeadRefOid: "3cdf82c3f06b32e07dcee9396ae32e904ff33ae3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("states =\n%+v\nwant\n%+v", got, want)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	if !strings.Contains(q, "number state merged mergedAt closedAt headRefOid mergeCommit { oid }") || strings.Contains(q, "latestReviews") {
		t.Errorf("confirm query should be scalar-only: %s", q)
	}
}

func TestConfirmStatesRepoNotFound(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "confirm_norepo.json"), Stderr: fixture(t, "confirm_norepo.stderr"), Code: 1},
	}}}
	got, notFound, err := (&Client{Run: f}).ConfirmStates(context.Background(), "talkable", "no-such-repo-magnum", []int{3, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(notFound)
	if len(got) != 0 || !reflect.DeepEqual(notFound, []int{1, 2, 3}) {
		t.Errorf("got %v notFound %v; a missing repo means every PR is unknown", got, notFound)
	}
}

// details_board.json was captured on 2026-10-03 with the current fragment
// (assignees, baseRefOid, PR size) for #11922 (team request), #11980 (two
// assignees, draft), #11975 (bot author) and a missing number.
func TestDetailsBoardFieldsFixture(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details_board.json"), Stderr: fixture(t, "details_board.stderr"), Code: 1},
	}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{11922, 11980, 11975, 99999999})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []int{99999999}) || len(got) != 3 {
		t.Fatalf("got %d, missing %v", len(got), missing)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	for _, want := range []string{"baseRefOid", "additions deletions changedFiles commits { totalCount }", "assignees(first: 10) { nodes { login } }",
		"reviewRequests(first: 30)", "labels(first: 100) { totalCount pageInfo { hasNextPage } nodes { name } }",
		"latestReviews(first: 100) { totalCount pageInfo { hasNextPage } nodes {"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}

	team := got[11922]
	if !reflect.DeepEqual(team.ReviewRequests, []Reviewer{{Type: "Team", Login: "engineers"}}) {
		t.Errorf("team request = %+v", team.ReviewRequests)
	}
	if !reflect.DeepEqual(team.Assignees, []string{"rev-ann"}) {
		t.Errorf("assignees = %v", team.Assignees)
	}
	if team.BaseRefOid != "44cb4c7c7b200bd9cddb16457d1e17c72639b835" || team.HeadRefOid != "bca3f44adc43c2d07bd4e9ca131a1b838a68580f" {
		t.Errorf("oids = %s...%s", team.BaseRefOid, team.HeadRefOid)
	}
	if team.Additions != 1523 || team.Deletions != 138 || team.ChangedFiles != 17 || team.Commits != 12 {
		t.Errorf("size = +%d -%d files %d commits %d", team.Additions, team.Deletions, team.ChangedFiles, team.Commits)
	}
	if len(team.LatestReviews) != 4 {
		t.Fatalf("latest reviews = %+v", team.LatestReviews)
	}
	approved := team.LatestReviews[3]
	if approved.State != "APPROVED" || approved.AuthorLogin != "rev-ann" || approved.CommitOid != team.HeadRefOid ||
		!approved.SubmittedAt.Equal(time.Date(2026, 9, 25, 11, 8, 57, 0, time.UTC)) {
		t.Errorf("approval = %+v", approved)
	}
	if bot := team.LatestReviews[2]; bot.AuthorType != "Bot" || bot.AuthorLogin != "chatgpt-codex-connector" {
		t.Errorf("bot review = %+v", bot)
	}

	draft := got[11980]
	if !draft.IsDraft || !reflect.DeepEqual(draft.Assignees, []string{"zhuravel", "a-long-login-1"}) ||
		draft.ReviewRequests == nil || len(draft.ReviewRequests) != 0 {
		t.Errorf("11980 = %+v", draft)
	}
	if draft.LatestReviews[0].State != "CHANGES_REQUESTED" || draft.LatestReviews[0].AuthorLogin != "zhuravel" {
		t.Errorf("11980 reviews = %+v", draft.LatestReviews)
	}
	if bot := got[11975]; !reflect.DeepEqual(bot.Assignees, []string{"zhuravel"}) || bot.Commits != 1 {
		t.Errorf("11975 = %+v", bot)
	}
}

func TestDetailsOldFixtureHasEmptyAssignees(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1},
	}}}
	got, _, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{11975})
	if err != nil {
		t.Fatal(err)
	}
	if d := got[11975]; d.Assignees == nil || len(d.Assignees) != 0 || d.BaseRefOid != "" || d.Commits != 0 {
		t.Errorf("absent fields must decode to empty values: %+v", d)
	}
}

// connBody is a one-PR details answer whose labels and latestReviews carry the
// given connection metadata (a JSON object body without the nodes).
func connBody(t *testing.T, labelsMeta, reviewsMeta string) []byte {
	return compact(t, `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4990,"used":10,"resetAt":"2026-10-02T21:50:10Z"},
	"repository":{"p5":{"id":"PR_x","number":5,"title":"t","url":"u","author":{"login":"a","__typename":"User"},
	"labels":{`+labelsMeta+`"nodes":[{"name":"WIP"},{"name":"hold"}]},
	"headRefName":"h","baseRefName":"master","isCrossRepository":false,"state":"OPEN","merged":false,"mergedAt":null,
	"closedAt":null,"updatedAt":"2026-10-02T09:04:36Z","isDraft":false,"headRefOid":"abc",
	"reviewRequests":{"nodes":[]},
	"latestReviews":{`+reviewsMeta+`"nodes":[{"state":"APPROVED","submittedAt":"2026-10-01T02:34:24Z","author":{"login":"b","__typename":"User"},"commit":{"oid":"c1"}}]}}}}}`)
}

func TestDetailsReportsTruncatedConnections(t *testing.T) {
	cases := []struct {
		name                  string
		labelsMeta, revMeta   string
		wantLabels, wantRevws bool
	}{
		{"single pages", `"totalCount":2,"pageInfo":{"hasNextPage":false},`, `"totalCount":1,"pageInfo":{"hasNextPage":false},`, true, true},
		{"no metadata (old fixtures)", ``, ``, true, true},
		{"labels have another page", `"totalCount":150,"pageInfo":{"hasNextPage":true},`, `"totalCount":1,"pageInfo":{"hasNextPage":false},`, false, true},
		{"reviews have another page", `"totalCount":2,"pageInfo":{"hasNextPage":false},`, `"totalCount":120,"pageInfo":{"hasNextPage":true},`, true, false},
		{"totalCount alone reveals truncation", `"totalCount":3,"pageInfo":{"hasNextPage":false},`, `"totalCount":2,"pageInfo":{"hasNextPage":false},`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Stdout: connBody(t, tc.labelsMeta, tc.revMeta)}}}}
			got, _, err := (&Client{Run: f}).Details(context.Background(), "o", "r", []int{5})
			if err != nil {
				t.Fatal(err)
			}
			d := got[5]
			if d.LabelsComplete != tc.wantLabels || d.LatestReviewsComplete != tc.wantRevws {
				t.Errorf("LabelsComplete=%v LatestReviewsComplete=%v, want %v and %v", d.LabelsComplete, d.LatestReviewsComplete, tc.wantLabels, tc.wantRevws)
			}
			if !reflect.DeepEqual(d.Labels, []string{"WIP", "hold"}) || len(d.LatestReviews) != 1 {
				t.Errorf("fetched nodes are still returned: %+v / %+v", d.Labels, d.LatestReviews)
			}
		})
	}
}

// details_ci.json is synthetic, in the shape GitHub answered the fragment
// with on 2026-10-04: #101 has check runs and commit statuses in every
// state, re-runs and a job of the same name in two workflows, #102 no
// checks, #103 more than one page of them, #104 (a draft) only skipped ones.
func TestDetailsCIFixture(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "details_ci.json")}}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{101, 102, 103, 104})
	if err != nil || len(missing) != 0 || len(got) != 4 {
		t.Fatalf("got %d, missing %v, err %v", len(got), missing, err)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	for _, want := range []string{"commits { totalCount }", "headCommit: commits(last: 1) { nodes { commit { oid statusCheckRollup { state",
		"contexts(first: 100) { totalCount pageInfo { hasNextPage }",
		"... on CheckRun { name status conclusion startedAt completedAt checkSuite { workflowRun { workflow { name } } } }",
		"... on StatusContext { context state createdAt }"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}

	at := func(hm string) time.Time {
		ts, err := time.Parse(time.RFC3339, "2026-10-04T"+hm+":00Z")
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	want := CIRollup{SHA: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", State: "FAILURE", Total: 15, Complete: true, Checks: []Check{
		{"rspec (1)", CheckPassed, "CI", at("09:10")},
		{"rspec (2)", CheckPassed, "CI", at("09:40")}, // the re-run replaces the timeout
		{"rspec (3)", CheckFailed, "CI", at("09:15")}, // a stale SKIPPED run listed later does not
		{"jest", CheckPending, "CI", at("09:05")},     // started, not finished
		{"lint", CheckPassed, "CI", at("09:10")},      // NEUTRAL
		{"deploy-preview", CheckSkipped, "CI", at("09:01")},
		{"cancelled-job", CheckFailed, "CI", at("09:02")},
		{"e2e", CheckPending, "CI", time.Time{}}, // a queued re-run is the newest
		{"Completion", CheckSkipped, "CI", at("08:00")},
		{"Completion", CheckPassed, "Pronto", at("09:30")}, // same name, another workflow: kept
		{"danger", CheckPassed, "", at("09:12")},           // not an Actions run
		{"completion", CheckPending, "", at("09:00")},
		{"coverage", CheckFailed, "", at("09:01")},
		{"license", CheckPassed, "", at("09:02")},
		{"expected-context", CheckPending, "", at("09:03")},
	}}
	if ci := got[101].CI; !reflect.DeepEqual(ci, want) {
		t.Errorf("#101 CI =\n%+v\nwant\n%+v", ci, want)
	}
	if got[101].Commits != 2 {
		t.Errorf("the aliased head commit must leave commits.totalCount alone: %d", got[101].Commits)
	}
	none := CIRollup{SHA: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", Complete: true, Checks: []Check{}}
	if ci := got[102].CI; !reflect.DeepEqual(ci, none) {
		t.Errorf("#102 without checks = %+v", ci)
	}
	if ci := got[103].CI; ci.State != "PENDING" || ci.Total != 130 || ci.Complete || len(ci.Checks) != 2 {
		t.Errorf("#103 with a second page = %+v", ci)
	}
	if ci := got[104].CI; ci.State != "SUCCESS" || ci.Total != 2 || len(ci.Checks) != 2 ||
		ci.Checks[0].State != CheckSkipped || ci.Checks[1].State != CheckSkipped {
		t.Errorf("#104 all skipped = %+v", ci)
	}

	// A fixture captured before the field existed reads as no commit.
	old := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1}}}}
	prev, _, err := (&Client{Run: old}).Details(context.Background(), "talkable", "talkable", []int{11975})
	if err != nil {
		t.Fatal(err)
	}
	if ci := prev[11975].CI; !reflect.DeepEqual(ci, CIRollup{Complete: true, Checks: []Check{}}) {
		t.Errorf("absent headCommit = %+v", ci)
	}
}

// details_files.json is synthetic, in the shape GitHub answers the fragment
// with: #201 lists its three files, #202 (a draft) more than one page of
// them, #203 none, and #204 came back without a list (files: null).
func TestDetailsReadsChangedFilePaths(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "details_files.json")}}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{201, 202, 203, 204})
	if err != nil || len(missing) != 0 || len(got) != 4 {
		t.Fatalf("got %d, missing %v, err %v", len(got), missing, err)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	if want := "files(first: 100) { totalCount pageInfo { hasNextPage } nodes { path } }"; !strings.Contains(q, want) {
		t.Errorf("query lacks %q: %s", want, q)
	}
	if d := got[201]; !reflect.DeepEqual(d.Files, []string{"Gemfile.lock", "app/models/order.rb", "spec/models/order_spec.rb"}) || !d.FilesComplete {
		t.Errorf("#201 files = %v complete %v", d.Files, d.FilesComplete)
	}
	if d := got[202]; !reflect.DeepEqual(d.Files, []string{"app/a.rb", "app/b.rb"}) || d.FilesComplete {
		t.Errorf("#202 (a second page) files = %v complete %v", d.Files, d.FilesComplete)
	}
	if d := got[203]; d.Files == nil || len(d.Files) != 0 || !d.FilesComplete {
		t.Errorf("#203 (no files) = %#v complete %v: want an empty, non-nil list", d.Files, d.FilesComplete)
	}
	if d := got[204]; d.Files != nil || d.FilesComplete {
		t.Errorf("#204 (files: null) = %#v complete %v: want no list", d.Files, d.FilesComplete)
	}

	// A fixture captured before the field existed reads as no list.
	old := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1}}}}
	prev, _, err := (&Client{Run: old}).Details(context.Background(), "talkable", "talkable", []int{11975})
	if err != nil {
		t.Fatal(err)
	}
	if d := prev[11975]; d.Files != nil || d.FilesComplete {
		t.Errorf("absent files = %#v complete %v", d.Files, d.FilesComplete)
	}
}

// details_activity.json is synthetic, in the shape GitHub answers the
// fragment with; every PR's updatedAt is 2026-10-06 09:00, moved by something
// a reviewer never sees. A PR's activity time is the latest of its activity
// timeline items (wherever the timeline lists them), its latest reviews,
// its description edit, its head commit (never past updatedAt), its merge or
// close and its opening; a pending review does not count, and a PR whose
// timeline GitHub did not return has none.
func TestDetailsReadsTheLastActivity(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: fixture(t, "details_activity.json")}}}}
	got, missing, err := (&Client{Run: f}).Details(context.Background(), "talkable", "talkable", []int{301, 302, 303, 304, 305, 306, 307, 308})
	if err != nil || len(missing) != 0 || len(got) != 8 {
		t.Fatalf("got %d, missing %v, err %v", len(got), missing, err)
	}
	q := oneLine(decodeReq(t, f.Calls[0]).Query)
	for _, want := range []string{"createdAt updatedAt lastEditedAt",
		"activity: timelineItems(last: 10, itemTypes: [ISSUE_COMMENT, PULL_REQUEST_REVIEW, LABELED_EVENT, UNLABELED_EVENT, " +
			"REVIEW_REQUESTED_EVENT, REVIEW_REQUEST_REMOVED_EVENT, READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT, RENAMED_TITLE_EVENT, " +
			"BASE_REF_CHANGED_EVENT, AUTOMATIC_BASE_CHANGE_SUCCEEDED_EVENT, CLOSED_EVENT, REOPENED_EVENT, MERGED_EVENT, HEAD_REF_FORCE_PUSHED_EVENT])",
		"... on PullRequestReview { submittedAt }", "} committedDate } } }"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q: %s", want, q)
		}
	}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for n, want := range map[int]time.Time{
		301: at("2026-10-03T10:00:00Z"), // a review the timeline lists before a label and a removed request
		302: at("2026-10-01T12:00:00Z"), // nothing but its last commit
		303: at("2026-10-04T08:00:00Z"), // a description edit
		304: at("2026-10-05T07:00:00Z"), // a label removed; a pending review never counts
		305: at("2026-10-06T09:00:00Z"), // a commit dated past updatedAt (the committer's clock)
		306: at("2026-10-05T10:00:00Z"), // merged
		307: {},                         // no timeline: unknown
		308: at("2026-10-04T12:00:00Z"), // a latest review the timeline's last ten left out
	} {
		if d := got[n]; !d.ActivityAt.Equal(want) {
			t.Errorf("#%d activity = %v, want %v", n, d.ActivityAt, want)
		}
	}

	// A fixture captured before the fields existed has no activity time.
	old := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"},
		Result: execx.Result{Stdout: fixture(t, "details.json"), Stderr: fixture(t, "details.stderr"), Code: 1}}}}
	prev, _, err := (&Client{Run: old}).Details(context.Background(), "talkable", "talkable", []int{11975})
	if err != nil {
		t.Fatal(err)
	}
	if d := prev[11975]; !d.ActivityAt.IsZero() {
		t.Errorf("absent timeline: activity = %v", d.ActivityAt)
	}
}

func TestCheckStateNormalization(t *testing.T) {
	runs := map[string][][2]string{ // want -> status, conclusion
		CheckPassed:  {{"COMPLETED", "SUCCESS"}, {"COMPLETED", "NEUTRAL"}},
		CheckSkipped: {{"COMPLETED", "SKIPPED"}},
		CheckFailed: {{"COMPLETED", "FAILURE"}, {"COMPLETED", "TIMED_OUT"}, {"COMPLETED", "CANCELLED"}, {"COMPLETED", "ACTION_REQUIRED"},
			{"COMPLETED", "STARTUP_FAILURE"}, {"COMPLETED", "STALE"}, {"COMPLETED", "SOMETHING_NEW"}},
		CheckPending: {{"QUEUED", ""}, {"IN_PROGRESS", ""}, {"WAITING", ""}, {"REQUESTED", ""}, {"PENDING", ""}},
	}
	for want, ins := range runs {
		for _, in := range ins {
			if got := checkRunState(in[0], in[1]); got != want {
				t.Errorf("check run %s/%s = %s, want %s", in[0], in[1], got, want)
			}
		}
	}
	statuses := map[string]string{"SUCCESS": CheckPassed, "FAILURE": CheckFailed, "ERROR": CheckFailed,
		"PENDING": CheckPending, "EXPECTED": CheckPending}
	for in, want := range statuses {
		if got := statusState(in); got != want {
			t.Errorf("status %s = %s, want %s", in, got, want)
		}
	}
}
