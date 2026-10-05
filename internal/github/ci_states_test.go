package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// ciPRs are n same-repository pull requests PR_0..PR_<n-1> with heads h<i>.
func ciPRs(n int) []PRRadar {
	out := make([]PRRadar, n)
	for i := range out {
		out[i] = PRRadar{NodeID: fmt.Sprintf("PR_%d", i), Number: i, HeadRefOid: fmt.Sprintf("h%d", i)}
	}
	return out
}

// ciAnswer answers a CIStates call for ids, each node drawn by node ("null"
// for one GitHub does not know, with its NOT_FOUND error).
func ciAnswer(t *testing.T, ids []any, node func(id string) string) []byte {
	t.Helper()
	var nodes, errs []string
	for i, id := range ids {
		n := node(id.(string))
		nodes = append(nodes, n)
		if n == "null" {
			errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":["nodes",%d],"message":"Could not resolve to a node with the global id of '%s'"}`, i, id))
		}
	}
	body := `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4900,"used":100,"resetAt":"2026-10-05T10:00:00Z"},"nodes":[` + strings.Join(nodes, ",") + `]}`
	if len(errs) > 0 {
		body += `,"errors":[` + strings.Join(errs, ",") + `]`
	}
	return compact(t, body+"}")
}

// tipNode is a pull request whose head branch's tip is oid with rollup
// state ("" = no checks).
func tipNode(id, oid, state string) string {
	rollup := "null"
	if state != "" {
		rollup = fmt.Sprintf(`{"state":%q}`, state)
	}
	return fmt.Sprintf(`{"id":%q,"headRef":{"target":{"oid":%q,"statusCheckRollup":%s}}}`, id, oid, rollup)
}

// TestCIStatesReadsTheHeadRollupByNodeID: the rollup of the head branch is
// a pull request's CI only while the branch's tip is the head the radar
// read; a fork's pull request is not asked about (its branch carries the
// fork's checks), and a branch or pull request that is gone is unknown.
func TestCIStatesReadsTheHeadRollupByNodeID(t *testing.T) {
	prs := ciPRs(6)
	prs[3].IsCrossRepository = true
	var queries []string
	var asked []any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		queries = append(queries, oneLine(req.Query))
		asked, _ = req.Variables["ids"].([]any)
		return okResult(ciAnswer(t, asked, func(id string) string {
			switch id {
			case "PR_0":
				return tipNode(id, "h0", "FAILURE")
			case "PR_1":
				return tipNode(id, "h1", "")
			case "PR_2":
				return tipNode(id, "newer", "PENDING") // pushed since the radar
			case "PR_4":
				return fmt.Sprintf(`{"id":%q,"headRef":null}`, id) // branch deleted
			}
			return "null"
		}))
	})}}
	states, rate, err := (&Client{Run: f}).CIStates(context.Background(), prs)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"PR_0": "FAILURE", "PR_1": ""}; !maps.Equal(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
	if fmt.Sprint(asked) != "[PR_0 PR_1 PR_2 PR_4 PR_5]" {
		t.Errorf("asked = %v, want every pull request but the fork's", asked)
	}
	if len(queries) != 1 {
		t.Fatalf("calls = %d, want 1", len(queries))
	}
	q := queries[0]
	for _, want := range []string{"nodes(ids: $ids)", "... on PullRequest", "headRef { target { oid ... on Commit { statusCheckRollup { state } } } }",
		"rateLimit { limit cost remaining used resetAt }"} {
		if !strings.Contains(q, want) {
			t.Errorf("CI query lacks %q:\n%s", want, q)
		}
	}
	// No connection: a call costs 1 point whatever its size (rateLimit(dryRun:
	// true) said 1 for 25, 50 and 100 ids on 2026-10-05), and no check is
	// listed (Details reads them when the rollup changed).
	for _, banned := range []string{"first:", "last:", "contexts", "commits"} {
		if strings.Contains(q, banned) {
			t.Errorf("CI query must not open a connection, found %q", banned)
		}
	}
	if rate.Cost != 1 || rate.Remaining != 4900 {
		t.Errorf("rate = %+v", rate)
	}
}

// ciFake answers every CIStates call with SUCCESS on the asked heads, after
// fail(size, call) had its say (nil: answer).
func ciFake(t *testing.T, sizes *[]int, fail func(size, call int) []byte) *execx.Fake {
	return &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		ids, _ := req.Variables["ids"].([]any)
		*sizes = append(*sizes, len(ids))
		if stderr := fail(len(ids), len(*sizes)); stderr != nil {
			return failResult(c, html502, stderr)
		}
		return okResult(ciAnswer(t, ids, func(id string) string { return tipNode(id, "h"+strings.TrimPrefix(id, "PR_"), "SUCCESS") }))
	})}}
}

// TestCIStatesAsksAHundredPullRequestsACall: the open pull requests are
// read in calls of ciPage, a point each.
func TestCIStatesAsksAHundredPullRequestsACall(t *testing.T) {
	var sizes []int
	f := ciFake(t, &sizes, func(int, int) []byte { return nil })
	states, rate, err := (&Client{Run: f}).CIStates(context.Background(), ciPRs(250))
	if err != nil || len(states) != 250 {
		t.Fatalf("states = %d, %v", len(states), err)
	}
	if fmt.Sprint(sizes) != "[100 100 50]" || rate.Cost != 3 {
		t.Errorf("calls %v cost %d, want [100 100 50] and 3", sizes, rate.Cost)
	}
}

// TestCIStatesHalvesACallGitHubCouldNotAnswer: a call that ran out of time
// is retried once with half as many pull requests, and the calls after it
// keep the smaller size.
func TestCIStatesHalvesACallGitHubCouldNotAnswer(t *testing.T) {
	var sizes []int
	f := ciFake(t, &sizes, func(size, _ int) []byte {
		if size == 100 {
			return gh502
		}
		return nil
	})
	states, _, err := (&Client{Run: f}).CIStates(context.Background(), ciPRs(220))
	if err != nil || len(states) != 220 {
		t.Fatalf("states = %d, %v", len(states), err)
	}
	if fmt.Sprint(sizes) != "[100 50 50 50 50 20]" {
		t.Errorf("calls = %v, want [100 50 50 50 50 20]", sizes)
	}
}

// TestCIStatesKeepsWhatItReadWhenACallFails: a call that fails again after
// its retry ends the read; the states read before it are returned with the
// error.
func TestCIStatesKeepsWhatItReadWhenACallFails(t *testing.T) {
	var sizes []int
	f := ciFake(t, &sizes, func(_, call int) []byte {
		if call > 1 {
			return gh502
		}
		return nil
	})
	states, rate, err := (&Client{Run: f}).CIStates(context.Background(), ciPRs(250))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || !strings.Contains(err.Error(), "retried at 50 pull requests a call") {
		t.Fatalf("err = %v, want the retry's 502", err)
	}
	if len(states) != 100 || fmt.Sprint(sizes) != "[100 100 50]" || rate.Cost != 1 {
		t.Errorf("states %d, calls %v, cost %d; want 100, [100 100 50], 1", len(states), sizes, rate.Cost)
	}
}

// TestCIStatesWithNothingToAskMakesNoCall: no pull requests, or only forks'.
func TestCIStatesWithNothingToAskMakesNoCall(t *testing.T) {
	forks := ciPRs(2)
	for i := range forks {
		forks[i].IsCrossRepository = true
	}
	for _, prs := range [][]PRRadar{nil, forks} {
		f := &execx.Fake{}
		states, _, err := (&Client{Run: f}).CIStates(context.Background(), prs)
		if err != nil || len(states) != 0 || len(f.Calls) != 0 {
			t.Errorf("%d PRs: states %v, err %v, calls %d", len(prs), states, err, len(f.Calls))
		}
	}
}
