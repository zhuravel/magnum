package github

// The cost meter's side of the client: what every gh api command tells the
// Client's Meter (DECISIONS "A GitHub cost meter per call and identity").
// The caller says what it reads GitHub for by a label in the call's context
// (WithOp: "round.setup", "fix.external"); a call whose context carries
// none is named by its own kind ("details", "review threads"), which the
// call's error label starts with.

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
)

// Call is one gh api command a Client ran, as its Meter hears of it.
type Call struct {
	// Op is what the caller read GitHub for: the label WithOp put into the
	// call's context ("" when it carries none).
	Op string
	// Kind is the call's own name ("details", "review threads", "compare").
	Kind string
	// GraphQL: a GraphQL call, whose Rate is the rateLimit block GitHub
	// answered (Cost: the points it charged); zero when no answer carried
	// one (a call that failed before GraphQL answered). Otherwise a REST
	// call: one request against the core budget, with a zero Rate.
	GraphQL bool
	Rate    RateLimit
	// RateLimited: GitHub refused the call for its rate limit (an *APIError
	// matching ErrRateLimited), so the identity's budget is spent, whatever
	// its last reading said.
	RateLimited bool
}

// opKey carries the label WithOp puts into a context.
type opKey struct{}

// WithOp returns ctx labelled with what the calls made with it read GitHub
// for, which each call hands its client's Meter (Call.Op). A label set
// deeper in the call chain wins: a round's setup reads under "round.setup"
// though the round's context says "round".
func WithOp(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, opKey{}, op)
}

// OpOf is the label WithOp put into ctx ("" for none).
func OpOf(ctx context.Context) string {
	op, _ := ctx.Value(opKey{}).(string)
	return op
}

// callKinds name the calls by the start of their error labels (the op each
// method builds, "details talkable/talkable (3)"): a label is a prefix
// followed by a space. Longer prefixes are matched first.
var callKinds = func() [][2]string {
	kinds := [][2]string{
		{"ci states call", "ci states"},
		{"commit checks", "commit checks"},
		{"compare files", "compare files"},
		{"compare", "compare"},
		{"file", "file"},
		{"comment on", "post comment"},
		{"edit issue comment", "edit comment"},
		{"issue comments", "issue comments"},
		{"review dismissals", "review dismissals"},
		{"details", "details"},
		{"confirm", "confirm states"},
		{"list files", "list files"},
		{"pull files", "pull files"},
		{"pull", "pull"},
		{"review answers", "review answers"},
		{"rules", "branch rules"},
		{"branch protection", "branch protection"},
		{"radar", "radar"},
		{"reply to review comment", "reply"},
		{"review comments", "review comments"},
		{"delete pending review", "delete pending review"},
		{"review threads", "review threads"},
		{"reviews", "reviews"},
		{"all reviews", "all reviews"},
		{"review", "review"},
		{"dismiss review", "dismiss review"},
		{"update review", "update review"},
	}
	slices.SortStableFunc(kinds, func(a, b [2]string) int { return cmp.Compare(len(b[0]), len(a[0])) })
	return kinds
}()

// callKind is the kind of the call whose error label is op ("other" for a
// label no kind starts).
func callKind(op string) string {
	for _, k := range callKinds {
		if op == k[0] || strings.HasPrefix(op, k[0]+" ") {
			return k[1]
		}
	}
	return "other"
}

// meter tells the client's Meter of one gh api command, the call whose
// error label is op, made with ctx, which failed with err (nil: it did
// not). A Meter that panics never fails the call: the call's result is
// GitHub's, whatever the meter does with it.
func (c *Client) meter(ctx context.Context, op string, graphql bool, rate RateLimit, err error) {
	m := c.Meter
	if m == nil {
		return
	}
	defer func() { _ = recover() }()
	m(Call{Op: OpOf(ctx), Kind: callKind(op), GraphQL: graphql, Rate: rate, RateLimited: errors.Is(err, ErrRateLimited)})
}
