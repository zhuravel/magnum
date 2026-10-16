package github

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// answersAnswer answers a ReviewAnswers call for ids, each node drawn by
// node ("null" for one GitHub does not know, with its NOT_FOUND error).
func answersAnswer(t *testing.T, ids []any, node func(id string) string) []byte {
	t.Helper()
	var nodes, errs []string
	for i, id := range ids {
		n := node(id.(string))
		nodes = append(nodes, n)
		if n == "null" {
			errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":["nodes",%d],"message":"Could not resolve to a node with the global id of '%s'"}`, i, id))
		}
	}
	body := `{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4900,"used":100,"resetAt":"2026-10-16T10:00:00Z"},"nodes":[` + strings.Join(nodes, ",") + `]}`
	if len(errs) > 0 {
		body += `,"errors":[` + strings.Join(errs, ",") + `]`
	}
	return compact(t, body+"}")
}

// The live case (2026-10-15): the author's own review tool posted eight
// reviews in a minute, each starting a thread, and Details resolves the
// threads of a pull request's last two reviews only, so six still read as
// replies. ReviewAnswers reads, by node id, what the inline comments of
// the reviews the poll needs beyond those two answer, as Details does for
// its two: the threads' authors in Account form (a ghost's left out, each
// once) and the count of comments, so a review whose comments start threads
// reads apart from one with a body alone. A review GitHub does not know is
// missing, and a hundred reviews go in one call.
func TestReviewAnswersReadsWhatTheReviewsInlineCommentsAnswer(t *testing.T) {
	var queries []string
	var asked [][]any
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, req gqlReq) (execx.Result, error) {
		queries = append(queries, oneLine(req.Query))
		ids, _ := req.Variables["ids"].([]any)
		asked = append(asked, ids)
		return okResult(answersAnswer(t, ids, func(id string) string {
			switch id {
			case "PRR_own":
				return `{"id":"PRR_own","comments":{"nodes":[{"replyTo":null},{"replyTo":null}]}}`
			case "PRR_reply":
				return `{"id":"PRR_reply","comments":{"nodes":[{"replyTo":{"author":{"login":"talkable","__typename":"Bot"}}},` +
					`{"replyTo":{"author":{"login":"talkable","__typename":"Bot"}}},{"replyTo":{"author":null}},{"replyTo":null}]}}`
			case "PRR_body":
				return `{"id":"PRR_body","comments":{"nodes":[]}}`
			case "PRR_gone":
				return "null"
			}
			return fmt.Sprintf(`{"id":%q,"comments":{"nodes":[{"replyTo":{"author":{"login":"rev-ann","__typename":"User"}}}]}}`, id)
		}))
	})}}
	got, err := (&Client{Run: f}).ReviewAnswers(context.Background(), []string{"PRR_own", "PRR_reply", "PRR_body", "PRR_gone"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ReviewAnswer{
		"PRR_own":   {Comments: 2},
		"PRR_reply": {Answers: []string{"talkable[bot]"}, Comments: 4},
		"PRR_body":  {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("answers = %+v\nwant %+v", got, want)
	}
	if len(queries) != 1 || !strings.Contains(queries[0],
		"nodes(ids: $ids) { ... on PullRequestReview { id comments(first: 10) { nodes { replyTo { author { login __typename } } } } } }") {
		t.Errorf("queries = %q", queries)
	}

	// A resolved remark reads as one Details resolved.
	r := Remark{Review: true, ID: "PRR_reply"}
	r.Resolve(got["PRR_reply"])
	if !r.AnswersKnown || r.Comments != 4 || !reflect.DeepEqual(r.Answers, []string{"talkable[bot]"}) {
		t.Errorf("resolved remark = %+v", r)
	}

	// 150 reviews: two calls, a hundred and fifty; none asked twice.
	queries, asked = nil, nil
	var many []string
	for i := range 150 {
		many = append(many, fmt.Sprintf("PRR_%d", i))
	}
	got, err = (&Client{Run: f}).ReviewAnswers(context.Background(), many)
	if err != nil || len(got) != 150 || len(asked) != 2 || len(asked[0]) != 100 || len(asked[1]) != 50 {
		t.Fatalf("150 reviews: %d answers, calls %d, err %v", len(got), len(asked), err)
	}

	// Nothing to ask: no call.
	queries = nil
	if got, err := (&Client{Run: f}).ReviewAnswers(context.Background(), nil); err != nil || len(got) != 0 || len(queries) != 0 {
		t.Errorf("no ids: %+v, %v, %d calls", got, err, len(queries))
	}
}
