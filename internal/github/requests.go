package github

import "time"

// Review requests as the PR's timeline records them. The poller treats a
// request for the poll login or a posting identity (or a team a watch
// names) as a request for a review round, edge-triggered by its time: the
// pending reviewRequests list cannot tell a new request from an old one,
// and GitHub drops it once the reviewer reviews.

// ReviewRequestEvent is one ReviewRequestedEvent of a pull request's
// timeline.
type ReviewRequestEvent struct {
	CreatedAt time.Time
	// Actor is the login that asked for the review ("" for a ghost).
	Actor string
	// Reviewer is who was asked: Type User, Bot, Mannequin or Team (Login is
	// then the team's slug). GraphQL logins carry no "[bot]" suffix; compare
	// them with SameLogin.
	Reviewer Reviewer
}

// reviewRequestsJSON is the timelineItems connection of detailsFragment.
type reviewRequestsJSON struct {
	Nodes []struct {
		CreatedAt         time.Time  `json:"createdAt"`
		Actor             *actorJSON `json:"actor"`
		RequestedReviewer *struct {
			Typename string `json:"__typename"`
			Login    string `json:"login"`
			Slug     string `json:"slug"`
		} `json:"requestedReviewer"`
	} `json:"nodes"`
}

// events converts the connection, oldest first as GitHub lists it. A node
// without a reviewer (a deleted account or team) or a time is left out.
func (r reviewRequestsJSON) events() []ReviewRequestEvent {
	out := []ReviewRequestEvent{}
	for _, n := range r.Nodes {
		rr := n.RequestedReviewer
		if rr == nil || n.CreatedAt.IsZero() {
			continue
		}
		ev := ReviewRequestEvent{CreatedAt: n.CreatedAt, Reviewer: Reviewer{Type: rr.Typename, Login: rr.Login}}
		if rr.Typename == "Team" {
			ev.Reviewer.Login = rr.Slug
		}
		if n.Actor != nil {
			ev.Actor = n.Actor.Login
		}
		out = append(out, ev)
	}
	return out
}
