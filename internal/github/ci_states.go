package github

import (
	"context"
	"fmt"
)

// ciPage is how many pull requests one CIStates call asks about, the most
// nodes(ids:) takes. The price does not grow with it (no connection: 1 point
// a call); the time does, as GitHub computes each head's rollup from all
// its check suites and statuses: 99 pull requests took 1.3 to 1.5 s
// (2026-10-05), well within GitHub's 10 s.
const ciPage = 100

// ciStatesQuery reads each pull request's head branch tip and its check
// rollup state by node id.
const ciStatesQuery = `query($ids: [ID!]!) {
  ` + rateLimitFields + `
  nodes(ids: $ids) {
    ... on PullRequest { id headRef { target { oid ... on Commit { statusCheckRollup { state } } } } }
  }
}`

type ciStatesJSON struct {
	Nodes []*struct {
		ID      string `json:"id"`
		HeadRef *struct {
			Target *struct {
				Oid               string `json:"oid"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"target"`
		} `json:"headRef"`
	} `json:"nodes"`
}

// CIStates reads the check rollup of each pull request's head (SUCCESS,
// FAILURE, PENDING, ERROR, EXPECTED; "" when the head has no checks) by node
// id. A CI run does not move a pull request's updatedAt, so the radar alone
// cannot tell that it ended. The rollup is read through the head branch: it
// is the pull request's only while the branch's tip is the HeadRefOid the
// radar read, and a fork's pull request is not asked about (its branch
// carries the fork's checks, not the ones the pull request runs); a pull
// request missing from the result is unknown.
//
// Each call asks about ciPage pull requests; one GitHub could not answer in
// time is asked once more at half the size (retrySmaller), which the calls
// after it keep. A call that still fails ends the read: the states read
// before it come back with the error. The RateLimit sums Cost over the
// calls (1 point each).
func (c *Client) CIStates(ctx context.Context, prs []PRRadar) (map[string]string, RateLimit, error) {
	var ask []PRRadar
	for _, p := range prs {
		if !p.IsCrossRepository && p.NodeID != "" && p.HeadRefOid != "" {
			ask = append(ask, p)
		}
	}
	states := map[string]string{}
	var sum rateSum
	size := ciPage
	for call := 1; len(ask) > 0; call++ {
		op := fmt.Sprintf("ci states call %d", call)
		var (
			data  ciStatesJSON
			rate  RateLimit
			batch []PRRadar
			err   error
		)
		size, err = retrySmaller(size, "pull requests a call", func(n int) error {
			batch = ask[:min(n, len(ask))]
			ids := make([]string, len(batch))
			for i, p := range batch {
				ids[i] = p.NodeID
			}
			data = ciStatesJSON{}
			var err error
			rate, _, err = c.graphql(ctx, op, ciStatesQuery, map[string]any{"ids": ids}, &data)
			return err
		})
		if err != nil {
			return states, sum.total, err
		}
		sum.add(rate)
		heads := make(map[string]string, len(batch))
		for _, p := range batch {
			heads[p.NodeID] = p.HeadRefOid
		}
		for _, n := range data.Nodes {
			if n == nil || n.HeadRef == nil || n.HeadRef.Target == nil {
				continue // gone (NOT_FOUND), or its branch was deleted
			}
			if head, ok := heads[n.ID]; !ok || head != n.HeadRef.Target.Oid {
				continue // pushed to since the radar read it
			}
			state := ""
			if r := n.HeadRef.Target.StatusCheckRollup; r != nil {
				state = r.State
			}
			states[n.ID] = state
		}
		ask = ask[len(batch):]
	}
	return states, sum.total, nil
}
