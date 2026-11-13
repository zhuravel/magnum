package fixwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// The settle's confirmation of the fixer's push when GitHub does not
// answer (DECISIONS "A GitHub blip never fails a fix whose push landed"):
// ConfirmPush retries a transient read (transientRead) with a backoff, for
// about 3 minutes, on the job's own context, so a shutdown cuts it at
// once. The engine's fix_confirm.go keeps what follows when GitHub still
// does not answer: the loop's settle stays open, the ticks try again, and
// a fix that fails for want of the push is settled late.

// ConfirmWindow bounds the settle's own retries of a transient read
// (confirmBackoff bounds their number too): past it, the ticks try
// again.
const ConfirmWindow = 3 * time.Minute

// confirmBackoff are the waits before each new read of the PR's head
// after a transient failure: 3 minutes in all (ConfirmWindow).
var confirmBackoff = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, 45 * time.Second}

// PushNotConfirmed is the settle's failure for want of the push on GitHub:
// GitHub answered with another head, its read failed for good, or GitHub
// did not answer within fixConfirmCap. Its words are as before; the record
// carries its kind (PushUnconfirmed), never the words.
type PushNotConfirmed struct {
	Head string
	Err  error
}

// Error says the push was not confirmed, and why.
func (p PushNotConfirmed) Error() string { return "push not confirmed on GitHub: " + p.Err.Error() }

// Unwrap is why.
func (p PushNotConfirmed) Unwrap() error { return p.Err }

// PushUnanswered is ConfirmPush's end when GitHub did not answer within
// ConfirmWindow, only transient failures: the ticks try again.
type PushUnanswered struct {
	Head string
	Err  error
}

// Error says GitHub did not answer, and how it failed last.
func (p PushUnanswered) Error() string {
	return "push not confirmed on GitHub yet: GitHub does not answer: " + p.Err.Error()
}

// Unwrap is the last failure.
func (p PushUnanswered) Unwrap() error { return p.Err }

// transientRead reports whether err, a failed read of the PR's head
// (remoteHead), is GitHub or the network failing to answer, which a retry
// may get past: a connection error (no connection, a reset, a DNS
// failure), a timeout, or an HTTP 5xx from GitHub. It reads what the GitHub
// client returns: a typed error (*github.APIError, net.Error), or gh's or
// git's exit and its output (github.ConnectionCause, its rate limits left
// out, then transientExitRe: what git, curl, ssh and Go's HTTP/2 print when
// GitHub fails to answer, which the shared classifier does not name). A 499
// is GitHub's own timeout. An answer cut short is a timeout too: the empty body GraphQL
// answers a query past its time limit with, which the client cannot decode
// ("unexpected end of JSON input"), and a cut one (io.ErrUnexpectedEOF).
// GitHub's own answer (401, 403, 404, a GraphQL error, a rate limit) and
// every other error are not, nor is a stop. remoteHead's joined error (the
// client's read, then `git ls-remote`) is transient only when each part
// is.
func transientRead(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs := j.Unwrap()
		for _, e := range errs {
			if !transientRead(e) {
				return false
			}
		}
		return len(errs) > 0
	}
	if api, ok := errors.AsType[*github.APIError](err); ok {
		return len(api.Errors) == 0 && (api.Status == 499 || api.Status >= 500 && api.Status <= 599)
	}
	if _, ok := errors.AsType[net.Error](err); ok { // a dial, a DNS lookup, a reset, a timeout (context.DeadlineExceeded is one)
		return true
	}
	if se, ok := errors.AsType[*json.SyntaxError](err); ok && se.Offset == 0 && se.Error() == "unexpected end of JSON input" {
		return true // an empty answer: GraphQL's time limit (github's overloaded reads it so too)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if exit, ok := errors.AsType[*execx.ExitError](err); ok {
		if cause := github.ConnectionCause(exit.Stderr); cause != "" {
			return cause != github.RateLimited
		}
		return transientExitRe.MatchString(strings.ToLower(exit.Stderr))
	}
	return false
}

// transientExitRe is what an exit prints, lower case, when GitHub fails to
// answer and github.ConnectionCause names nothing: git's (curl's) HTTP 5xx
// ("The requested URL returned error: 503"), an empty or cut answer, an
// HTTP/2 stream git or gh lost (curl's "HTTP/2 stream 1 was not closed
// cleanly", Go's REFUSED_STREAM, CANCEL, GOAWAY and lost connection, which
// the client's overloaded reads so too), and ssh's connection GitHub
// closed. A 4xx, a missing repository or a refused key match nothing.
var transientExitRe = regexp.MustCompile(`requested url returned error: 5\d\d\b|empty reply from server|failure when receiving data from the peer|` +
	`http/2 stream \d+ was not closed cleanly|refused_stream|cancel; received from peer|server sent goaway|http2: client connection lost|` +
	`connection closed by \S+ port \d+|kex_exchange_identification`)

const (
	// pushLooks bounds how often ConfirmPush looks at a PR's head that
	// GitHub shows otherwise, pushWait apart: GitHub moves a PR's head a
	// moment after the push to its branch.
	pushLooks = 3
	pushWait  = 5 * time.Second
)

// PushConfirm is what ConfirmPush confirms a push through: the read of
// the PR's head on GitHub (Read; the engine's remoteHead), the clock and
// its wait (Now, Sleep, which ends with ctx), and the marks on the loop's
// row while GitHub does not answer: Mark at the first transient failure,
// Unmark once GitHub answers after one.
type PushConfirm struct {
	Read   func(ctx context.Context) (string, error)
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Mark   func()
	Unmark func()
}

// ConfirmPush checks that GitHub has head, the commit the fixer says it
// pushed, as the head of the PR's branch (c.Read). A head GitHub shows
// otherwise is looked at again pushWait later, up to pushLooks times; a
// read that fails with a transient error (transientRead) is tried again
// after each wait of confirmBackoff, within ConfirmWindow, and the loop's
// row says meanwhile that it confirms the push (c.Mark); any other failed
// read ends it at once. It returns nil once GitHub shows head,
// PushUnanswered when GitHub did not answer within the window,
// PushNotConfirmed otherwise, or ctx's error the moment ctx ends: the
// retry holds no shutdown or drain. The errors name the shas, never the
// fixer's words.
func ConfirmPush(ctx context.Context, c PushConfirm, head string) error {
	start := c.Now()
	looks, tries := 0, 0
	for {
		got, err := c.Read(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var wait time.Duration
		switch {
		case err == nil && got == head:
			if tries > 0 { // GitHub answered after a blip: the loop is at work again, its settle no longer confirming
				c.Unmark()
			}
			return nil
		case err == nil:
			if looks++; looks >= pushLooks {
				return PushNotConfirmed{Head: head, Err: fmt.Errorf("the PR's head is %s, not the pushed %s", textx.ShortSHA(got), textx.ShortSHA(head))}
			}
			wait = pushWait
		case !transientRead(err):
			return PushNotConfirmed{Head: head, Err: err}
		case tries >= len(confirmBackoff) || c.Now().Sub(start) >= ConfirmWindow:
			return PushUnanswered{Head: head, Err: err}
		default:
			if tries == 0 {
				c.Mark()
			}
			wait = confirmBackoff[tries]
			tries++
		}
		if err := c.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}
