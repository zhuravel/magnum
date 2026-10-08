// Package github is magnum's GitHub access layer. Every call is a `gh api`
// subprocess run through execx.Runner, so the identity is chosen by the
// client's env (GH_CONFIG_DIR per identity), tests script gh with execx.Fake
// and --dry-run turns writes into no-ops.
//
// Reads use GraphQL (`gh api graphql --input -`): the per-owner Radar poll
// (a user or an organization) and the CIStates of its pull requests' heads,
// batched Details, ConfirmStates for PRs that left the OPEN list,
// ReviewsWithMarker for verification, and Reviews and ReviewThreads for the
// whole conversation of one pull request. A Radar page or CIStates call
// GitHub could not answer in time is asked again once at half the size
// (retrySmaller). REST is used
// where only REST carries the data (ReviewREST: the "[bot]"-suffixed author
// login; Compare, CompareFiles and ComparePush: the size, the patches and
// the merge commits of an arbitrary base...head range; FileAt: the raw
// content of a file at a ref, a response that is bytes and not JSON) and
// for writes (DismissReview).
//
// GraphQL partial errors are tolerated only for NOT_FOUND paths, which the
// batched calls report as missing numbers; any other error fails the whole
// call, so a half-read poll never looks like closed pull requests. Failures
// that reached GitHub come back as *APIError, which matches ErrNotFound,
// ErrUnauthorized, ErrForbidden and ErrRateLimited through errors.Is.
//
// Every command carries --hostname github.com, so an inherited GH_HOST can
// never send a call (or an identity's token) to another server. Client.Reauth
// lets the caller refresh an identity's credentials after a 401.
package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// Client runs gh as one identity. The zero value is unusable; set Run.
type Client struct {
	// Run executes gh. Required.
	Run execx.Runner
	// Env overlays gh's environment, e.g. GH_CONFIG_DIR for an identity.
	// It wins over the defaults (GH_PROMPT_DISABLED=1, GH_NO_UPDATE_NOTIFIER=1).
	Env map[string]string
	// Dir is gh's working directory ("" = inherit).
	Dir string
	// Reauth, when set, runs once after a call fails with ErrUnauthorized
	// (the identity's token was revoked or expired); the call is then retried
	// once. A 401 means GitHub applied nothing, so retrying a write is safe.
	// If Reauth fails the call returns the unauthorized error together with
	// Reauth's error. It may be called from several goroutines at once.
	Reauth func(ctx context.Context) error

	// last is the most conservative rate-limit snapshot any GraphQL call of
	// this client saw (latest reset window, lowest remaining; observe).
	mu   sync.Mutex
	last RateLimit

	// Radar page sizes; zero means 100. Tests shrink them to match fixtures.
	repoPage, prPage int
}

// RateLimit is GitHub's GraphQL rateLimit{} block.
type RateLimit struct {
	Limit     int
	Cost      int // points the call(s) cost
	Remaining int
	Used      int
	ResetAt   time.Time
}

// tighter reports whether r is a more current/conservative snapshot than o.
func (r RateLimit) tighter(o RateLimit) bool {
	if o.ResetAt.IsZero() {
		return true
	}
	if !r.ResetAt.Equal(o.ResetAt) {
		return r.ResetAt.After(o.ResetAt)
	}
	return r.Remaining <= o.Remaining
}

func (c *Client) observe(r RateLimit) {
	if r.ResetAt.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.tighter(c.last) {
		c.last = r
	}
}

// Errors matched by *APIError through errors.Is.
var (
	ErrNotFound     = errors.New("github: not found")
	ErrUnauthorized = errors.New("github: unauthorized")
	ErrForbidden    = errors.New("github: forbidden")
	ErrRateLimited  = errors.New("github: rate limited")
)

// GraphQLError is one entry of a GraphQL response's "errors" array.
type GraphQLError struct {
	Type    string   // NOT_FOUND, FORBIDDEN, RATE_LIMITED, ...; "" for validation errors
	Message string   //
	Path    []string // e.g. ["repository", "p123"]
}

// UnmarshalJSON accepts GitHub's mixed string/number paths.
func (g *GraphQLError) UnmarshalJSON(b []byte) error {
	var raw struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Path    []any  `json:"path"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	g.Type, g.Message, g.Path = raw.Type, raw.Message, nil
	for _, p := range raw.Path {
		g.Path = append(g.Path, fmt.Sprint(p))
	}
	return nil
}

// APIError is a failure GitHub reported: an HTTP status from gh, a REST error
// body, or GraphQL errors.
type APIError struct {
	Op      string         // what magnum was doing ("details talkable/talkable")
	Status  int            // HTTP status when known, else 0
	Message string         // REST message or gh's stderr line
	Errors  []GraphQLError // GraphQL errors, if any
	// Details are the entries of a REST error body's "errors" array (a
	// string as it is, an object as its field and message), e.g. "Can not
	// approve your own pull request" under the message "Unprocessable
	// Entity".
	Details []string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("github")
	if e.Op != "" {
		b.WriteString(" " + e.Op)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	msgs := []string{}
	if e.Message != "" {
		msgs = append(msgs, e.Message)
	}
	msgs = append(msgs, e.Details...)
	for _, g := range e.Errors {
		m := g.Message
		if g.Type != "" {
			m = g.Type + ": " + m
		}
		msgs = append(msgs, m)
	}
	if len(msgs) > 0 {
		b.WriteString(": " + strings.Join(msgs, "; "))
	}
	return execx.Redact(b.String())
}

// Is maps the error onto the package sentinels.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.rateLimited()
	case ErrNotFound:
		if e.Status == 404 {
			return true
		}
		return len(e.Errors) > 0 && e.allErrors("NOT_FOUND")
	case ErrUnauthorized:
		return e.Status == 401
	case ErrForbidden:
		return !e.rateLimited() && (e.Status == 403 || e.anyError("FORBIDDEN"))
	}
	return false
}

func (e *APIError) rateLimited() bool {
	if e.Status == 429 || e.anyError("RATE_LIMITED") {
		return true
	}
	return e.Status == 403 && strings.Contains(strings.ToLower(e.Message), "rate limit")
}

func (e *APIError) anyError(typ string) bool {
	for _, g := range e.Errors {
		if g.Type == typ {
			return true
		}
	}
	return false
}

func (e *APIError) allErrors(typ string) bool {
	for _, g := range e.Errors {
		if g.Type != typ {
			return false
		}
	}
	return true
}

var ghDefaults = map[string]string{"GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1"}

// betweenKey carries the function WithBetweenCalls puts into a context.
type betweenKey struct{}

// WithBetweenCalls returns ctx carrying fn, which the client runs before
// every gh command it makes with that context (BetweenCalls), on the
// caller's goroutine: the daemon's poll answers the requests a CLI queued
// meanwhile, so one waits for a single GitHub call, not the whole poll. fn
// gets no context: it must not make its calls with this one.
func WithBetweenCalls(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, betweenKey{}, fn)
}

// BetweenCalls runs the function WithBetweenCalls put into ctx, if any. The
// client calls it before each gh command; a fake GitHub can do the same.
func BetweenCalls(ctx context.Context) {
	if fn, ok := ctx.Value(betweenKey{}).(func()); ok && fn != nil {
		fn()
	}
}

// gh runs one gh command with the client's env and directory, after
// BetweenCalls; expected (nil = none) picks the failed exits that are
// answers (execx.Cmd.Expected).
func (c *Client) gh(ctx context.Context, label string, args []string, stdin []byte, mutates bool, expected func(execx.Result) bool) (execx.Result, error) {
	if c.Run == nil {
		return execx.Result{}, errors.New("github: Client.Run is nil")
	}
	BetweenCalls(ctx)
	env := maps.Clone(ghDefaults)
	maps.Copy(env, c.Env)
	return c.Run.Run(ctx, execx.Cmd{
		Name:     "gh",
		Args:     args,
		Dir:      c.Dir,
		Env:      env,
		Stdin:    stdin,
		Mutates:  mutates,
		Label:    "gh " + label,
		Expected: expected,
	})
}

// rateLimitJSON is the shape of rateLimit{limit cost remaining used resetAt}.
type rateLimitJSON struct {
	Limit     int       `json:"limit"`
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	ResetAt   time.Time `json:"resetAt"`
}

// rateLimitFields is the selection every query carries.
const rateLimitFields = "rateLimit { limit cost remaining used resetAt }"

// ghHost pins every API command to github.com whatever GH_HOST says.
var ghHost = []string{"--hostname", "github.com"}

// reauthorize runs Reauth after a 401. It reports whether the caller should
// retry; when Reauth itself fails it returns err together with Reauth's error.
func (c *Client) reauthorize(ctx context.Context, err error) (retry bool, out error) {
	if err == nil || c.Reauth == nil || !errors.Is(err, ErrUnauthorized) {
		return false, err
	}
	if rerr := c.Reauth(ctx); rerr != nil {
		return false, fmt.Errorf("%w; reauth: %w", err, rerr)
	}
	return true, nil
}

// graphql runs one query, decodes "data" into out and returns the call's
// rate limit plus the NOT_FOUND errors (their paths are null in data) for the
// caller to map. Any other GraphQL error, or a null data, fails the call.
// After a 401 it calls Reauth and runs the query once more.
func (c *Client) graphql(ctx context.Context, op, query string, vars map[string]any, out any) (RateLimit, []GraphQLError, error) {
	rate, notFound, err := c.graphqlOnce(ctx, op, query, vars, out)
	retry, err := c.reauthorize(ctx, err)
	if retry {
		return c.graphqlOnce(ctx, op, query, vars, out)
	}
	if err != nil {
		return RateLimit{}, nil, err
	}
	return rate, notFound, nil
}

func (c *Client) graphqlOnce(ctx context.Context, op, query string, vars map[string]any, out any) (RateLimit, []GraphQLError, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return RateLimit{}, nil, fmt.Errorf("github %s: encode request: %w", op, err)
	}
	args := append([]string{"api", "graphql"}, ghHost...)
	args = append(args, "--input", "-")
	res, runErr := c.gh(ctx, op, args, body, false, nil)
	var exitErr *execx.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return RateLimit{}, nil, fmt.Errorf("github %s: %w", op, runErr)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(res.Stdout, &env); err != nil {
		if exitErr != nil {
			return RateLimit{}, nil, ghFailure(op, res, exitErr)
		}
		return RateLimit{}, nil, fmt.Errorf("github %s: decode response: %w", op, err)
	}
	if exitErr != nil && len(env.Errors) == 0 {
		// gh failed before GraphQL answered (HTTP 401, 5xx, network).
		return RateLimit{}, nil, ghFailure(op, res, exitErr)
	}
	var notFound []GraphQLError
	fatal := isNull(env.Data)
	for _, e := range env.Errors {
		if e.Type == "NOT_FOUND" {
			notFound = append(notFound, e)
		} else {
			fatal = true
		}
	}
	if fatal {
		return RateLimit{}, nil, &APIError{Op: op, Status: httpStatus(res.Stderr), Errors: env.Errors}
	}
	var rl struct {
		RateLimit *rateLimitJSON `json:"rateLimit"`
	}
	if err := json.Unmarshal(env.Data, &rl); err != nil {
		return RateLimit{}, nil, fmt.Errorf("github %s: decode rateLimit: %w", op, err)
	}
	var rate RateLimit
	if rl.RateLimit != nil {
		rate = RateLimit(*rl.RateLimit)
		c.observe(rate)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return rate, nil, fmt.Errorf("github %s: decode data: %w", op, err)
	}
	return rate, notFound, nil
}

// rest runs one REST call; fields become JSON body fields (gh -f, raw strings).
// out may be nil to ignore the response body. After a 401 it calls Reauth and
// repeats the call once.
func (c *Client) rest(ctx context.Context, op, method, path string, fields [][2]string, mutates bool, out any) error {
	err := c.restOnce(ctx, op, method, path, fields, nil, mutates, out, nil)
	retry, err := c.reauthorize(ctx, err)
	if retry {
		return c.restOnce(ctx, op, method, path, fields, nil, mutates, out, nil)
	}
	return err
}

// restProbe is rest for a GET whose 403 (not a rate limit) or 404 is
// GitHub's answer, not a failure (unanswered): gh exits 1 for it as for a
// 500, so its status decides, and only such an answer is logged at Debug.
func (c *Client) restProbe(ctx context.Context, op, path string, out any) error {
	answer := func(res execx.Result) bool { return unanswered(ghFailure(op, res, &execx.ExitError{Code: res.Code})) }
	err := c.restOnce(ctx, op, "GET", path, nil, nil, false, out, answer)
	retry, err := c.reauthorize(ctx, err)
	if retry {
		return c.restOnce(ctx, op, "GET", path, nil, nil, false, out, answer)
	}
	return err
}

// restInput is rest with the request body sent as JSON on gh's stdin (gh
// api --input -), so none of it reaches argv: a write that carries PR text.
// out may be nil. After a 401 it calls Reauth and repeats the call once.
func (c *Client) restInput(ctx context.Context, op, method, path string, body any, mutates bool, out any) error {
	in, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("github %s: encode request: %w", op, err)
	}
	err = c.restOnce(ctx, op, method, path, nil, in, mutates, out, nil)
	retry, err := c.reauthorize(ctx, err)
	if retry {
		return c.restOnce(ctx, op, method, path, nil, in, mutates, out, nil)
	}
	return err
}

// restOnce runs one REST call with fields as gh -f, or with stdin (when
// non-nil) as its JSON body (--input -); expected as in gh.
func (c *Client) restOnce(ctx context.Context, op, method, path string, fields [][2]string, stdin []byte, mutates bool, out any, expected func(execx.Result) bool) error {
	args := []string{"api"}
	if method != "GET" {
		args = append(args, "-X", method)
	}
	args = append(args, path)
	args = append(args, ghHost...)
	for _, f := range fields {
		args = append(args, "-f", f[0]+"="+f[1])
	}
	if stdin != nil {
		args = append(args, "--input", "-")
	}
	res, err := c.gh(ctx, op, args, stdin, mutates, expected)
	if err != nil {
		var exitErr *execx.ExitError
		if errors.As(err, &exitErr) {
			return ghFailure(op, res, exitErr)
		}
		return fmt.Errorf("github %s: %w", op, err)
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		return fmt.Errorf("github %s: empty response", op)
	}
	if err := json.Unmarshal(res.Stdout, out); err != nil {
		return fmt.Errorf("github %s: decode response: %w", op, err)
	}
	return nil
}

// restRaw runs one REST GET and returns the response body verbatim (it is not
// decoded), asking for media type accept (gh api -H "Accept: ..."), e.g.
// application/vnd.github.raw for a file's content. Errors map as in rest, and
// after a 401 it calls Reauth and repeats the call once. Reads only: it is
// never marked Mutates. A successful call with no output returns an empty,
// non-nil slice.
func (c *Client) restRaw(ctx context.Context, op, path, accept string) ([]byte, error) {
	body, err := c.restRawOnce(ctx, op, path, accept)
	retry, err := c.reauthorize(ctx, err)
	if retry {
		return c.restRawOnce(ctx, op, path, accept)
	}
	return body, err
}

func (c *Client) restRawOnce(ctx context.Context, op, path, accept string) ([]byte, error) {
	args := append([]string{"api", path}, ghHost...)
	args = append(args, "-H", "Accept: "+accept)
	res, err := c.gh(ctx, op, args, nil, false, nil)
	if err != nil {
		var exitErr *execx.ExitError
		if errors.As(err, &exitErr) {
			return nil, ghFailure(op, res, exitErr)
		}
		return nil, fmt.Errorf("github %s: %w", op, err)
	}
	if res.Stdout == nil {
		return []byte{}, nil
	}
	return res.Stdout, nil
}

// httpStatusRe finds the status gh prints for a failed call: "(HTTP 404)"
// after GitHub's message, or a line of its own ("gh: HTTP 502") when the
// answer was not JSON (GitHub's HTML error page).
var httpStatusRe = regexp.MustCompile(`\(HTTP (\d{3})\)|(?m:^(?:gh: )?HTTP (\d{3})[ \t\r]*$)`)

func httpStatus(stderr []byte) int {
	m := httpStatusRe.FindSubmatch(stderr)
	if m == nil {
		return 0
	}
	code := m[1]
	if len(code) == 0 {
		code = m[2]
	}
	n, _ := strconv.Atoi(string(code))
	return n
}

// overloaded reports whether err is GitHub failing to answer a query in
// time: an HTTP 5xx (a query past GitHub's time limit answers 502 or 504),
// a GraphQL error naming a timeout, or the HTTP/2 stream GitHub cancelled.
// A smaller query may succeed where this one did not; a network failure, a
// refused or a malformed query would fail the same way again.
func overloaded(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status >= 500 {
			return true
		}
		for _, g := range apiErr.Errors {
			m := strings.ToLower(g.Message)
			if strings.Contains(m, "timeout") || strings.Contains(m, "timed out") || strings.Contains(m, "timedout") {
				return true
			}
		}
		return false
	}
	var exitErr *execx.ExitError
	return errors.As(err, &exitErr) && strings.Contains(exitErr.Stderr, "CANCEL; received from peer")
}

// minPage is the fewest items a page shrinks to when GitHub could not
// answer a bigger one in time.
const minPage = 10

// retrySmaller runs call with a page of size items; when GitHub could not
// answer it in time (overloaded) it runs it once more with half as many
// (never below minPage, or the size itself when that is smaller) and
// returns that size for the pages after it. The retry's error says the size
// it tried, in what ("repositories a page").
func retrySmaller(size int, what string, call func(size int) error) (int, error) {
	err := call(size)
	if !overloaded(err) {
		return size, err
	}
	size = max(size/2, min(size, minPage))
	if err := call(size); err != nil {
		return size, fmt.Errorf("%w (retried at %d %s)", err, size, what)
	}
	return size, nil
}

// ghFailure turns a non-zero gh exit into an *APIError when GitHub answered
// (status in stderr or a REST error body), else wraps the exit error.
func ghFailure(op string, res execx.Result, exitErr *execx.ExitError) error {
	var body struct {
		Message string            `json:"message"`
		Status  string            `json:"status"`
		Errors  []json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(res.Stdout, &body)
	status := httpStatus(res.Stderr)
	if status == 0 {
		status, _ = strconv.Atoi(body.Status)
	}
	if status == 0 && body.Message == "" {
		return fmt.Errorf("github %s: %w", op, exitErr)
	}
	msg := body.Message
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stderr))
		msg = strings.TrimPrefix(msg, "gh: ")
		msg = strings.TrimSpace(httpStatusRe.ReplaceAllString(msg, ""))
	}
	return &APIError{Op: op, Status: status, Message: execx.Redact(msg), Details: restErrorDetails(body.Errors)}
}

// restErrorDetails reads a REST error body's "errors" entries: a string as
// it is, an object as its field and its message (else its code).
func restErrorDetails(raw []json.RawMessage) []string {
	var out []string
	for _, r := range raw {
		var text string
		if json.Unmarshal(r, &text) != nil {
			var o struct {
				Field   string `json:"field"`
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(r, &o) != nil {
				continue
			}
			text = strings.TrimSpace(o.Field + " " + cmp.Or(o.Message, o.Code))
		}
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, execx.Redact(text))
		}
	}
	return out
}

func isNull(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) == 0 || bytes.Equal(s, []byte("null"))
}

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// checkRepo validates names before they reach a REST path or a query.
func checkRepo(owner, repo string) error {
	if !ownerRe.MatchString(owner) {
		return fmt.Errorf("github: invalid owner %q", owner)
	}
	if !repoRe.MatchString(repo) || repo == "." || repo == ".." {
		return fmt.Errorf("github: invalid repository name %q", repo)
	}
	return nil
}
