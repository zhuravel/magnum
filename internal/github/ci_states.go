package github

import (
	"context"
	"fmt"
)

// ciPage is how many pull requests one CIStates call asks about, the most
// nodes(ids:) takes. The price does not grow with it (no connection that
// lists a node: 1 point a call); the time does, as GitHub computes each
// head's rollup from all its check suites and statuses: 99 pull requests
// took 1.3 to 1.5 s (2026-10-05), well within GitHub's 10 s.
const ciPage = 100

// ciStatesQuery reads each pull request's head branch tip and its check
// rollup state, with the rollup's counts by state, by node id. The counts
// come through the rollup's contexts connection asked for no node (first:
// 0), so the call lists nothing; Details selects the same contexts with the
// state, so both read the state alike.
const ciStatesQuery = `query($ids: [ID!]!) {
  ` + rateLimitFields + `
  nodes(ids: $ids) {
    ... on PullRequest { id headRef { target { oid ... on Commit { statusCheckRollup { state contexts(first: 0) { ` + stateCountFields + ` } } } } } }
  }
}`

// stateCountFields are the counts by state of a rollup's contexts
// connection (stateCountsJSON).
const stateCountFields = `checkRunCountsByState { state count } statusContextCountsByState { state count }`

type ciStatesJSON struct {
	Nodes []*struct {
		ID      string `json:"id"`
		HeadRef *struct {
			Target *struct {
				Oid               string `json:"oid"`
				StatusCheckRollup *struct {
					State    string          `json:"state"`
					Contexts stateCountsJSON `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"target"`
		} `json:"headRef"`
	} `json:"nodes"`
}

// CIState is a pull request's head check rollup as CIStates reads it: its
// State (SUCCESS, FAILURE, PENDING, ERROR, EXPECTED; "" when the head has no
// checks) and its Counts by state (a zero Total when GitHub gave none).
type CIState struct {
	State  string
	Counts CICounts
}

// CICounts are a check rollup's check runs and commit statuses by state as
// GitHub counts them (checkRunCountsByState, statusContextCountsByState),
// normalized as Check.State is: every run the rollup holds, a superseded
// one and those past a page of 100 included, so they are not
// CIRollup.Checks' tally. A zero Total is unknown.
type CICounts struct {
	Failed, Pending, Passed, Skipped, Total int
}

// stateCountsJSON is a rollup contexts connection's counts by state.
type stateCountsJSON struct {
	CheckRuns []stateCountJSON `json:"checkRunCountsByState"`
	Statuses  []stateCountJSON `json:"statusContextCountsByState"`
}

type stateCountJSON struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// counts normalizes the counts: a check run by checkRunState (a state that
// is a run's status before it completed is pending, any other the
// conclusion it completed with), a status by statusState.
func (s stateCountsJSON) counts() CICounts {
	var out CICounts
	add := func(state string, n int) {
		switch state {
		case CheckFailed:
			out.Failed += n
		case CheckPending:
			out.Pending += n
		case CheckPassed:
			out.Passed += n
		case CheckSkipped:
			out.Skipped += n
		}
		out.Total += n
	}
	for _, c := range s.CheckRuns {
		switch c.State {
		case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED":
			add(checkRunState(c.State, ""), c.Count)
		default:
			add(checkRunState("COMPLETED", c.State), c.Count)
		}
	}
	for _, c := range s.Statuses {
		add(statusState(c.State), c.Count)
	}
	return out
}

// CIStates reads the check rollup of each pull request's head (its state
// and counts by state, CIState) by node id. A CI run does not move a pull
// request's updatedAt, so the radar alone cannot tell that it ended, and
// the state alone cannot tell that a further check failed on a head whose
// rollup failed already: the counts can. The rollup is read through the
// head branch: it is the pull request's only while the branch's tip is the
// HeadRefOid the radar read, and a fork's pull request is not asked about
// (its branch carries the fork's checks, not the ones the pull request
// runs); a pull request missing from the result is unknown.
//
// Each call asks about ciPage pull requests; one GitHub could not answer in
// time is asked once more at half the size (retrySmaller), which the calls
// after it keep. A call that still fails ends the read: the states read
// before it come back with the error. The RateLimit sums Cost over the
// calls (1 point each).
func (c *Client) CIStates(ctx context.Context, prs []PRRadar) (map[string]CIState, RateLimit, error) {
	var ask []PRRadar
	for _, p := range prs {
		if !p.IsCrossRepository && p.NodeID != "" && p.HeadRefOid != "" {
			ask = append(ask, p)
		}
	}
	states := map[string]CIState{}
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
		sum.add(rate, c.clock())
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
			var st CIState
			if r := n.HeadRef.Target.StatusCheckRollup; r != nil {
				st = CIState{State: r.State, Counts: r.Contexts.counts()}
			}
			states[n.ID] = st
		}
		ask = ask[len(batch):]
	}
	return states, sum.total, nil
}
