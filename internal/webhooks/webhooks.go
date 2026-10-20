// Package webhooks reads the events magnum-relay keeps of the GitHub App's
// webhook deliveries: GET <url>/v1/events, a long poll by cursor, with curl
// through execx. The operator's outbound firewall blocks Go's own HTTP
// client in a rebuilt, unsigned magnum binary, and gh reaches only GitHub,
// so curl, which the firewall lets through, makes the request.
//
// The relay's bearer token is read from a file only its owner can open
// (CheckTokenFile) and goes to curl on stdin, in the config curl reads with
// --config -, never in argv, a log or an error; --disable, the first
// argument, keeps a ~/.curlrc out (one that turns on verbose would print the
// header). An event carries ids and routing only: no title, body, comment,
// patch, user name or URL.
package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// MaxWait is the longest long poll the relay serves (wait=).
const MaxWait = 30 * time.Second

// PageSlack is how much longer than its wait curl gives a page (its
// --max-time): a page answers within wait + PageSlack or fails.
const PageSlack = 10 * time.Second

// MaxLimit is the most events the relay lists in one page.
const MaxLimit = 500

// Event is one delivery as the relay keeps it (relay-api: ids and routing
// only).
type Event struct {
	// Seq is the event's place in the relay's journal.
	Seq int64 `json:"seq"`
	// DeliveryID is the delivery's X-GitHub-Delivery GUID, which a
	// redelivery keeps.
	DeliveryID string `json:"delivery_id"`
	// Event is the X-GitHub-Event name ("pull_request", "check_run") and
	// Action the payload's action ("" when it has none).
	Event  string `json:"event"`
	Action string `json:"action"`
	// InstallationID and RepositoryID are 0 when the payload has none.
	InstallationID int64 `json:"installation_id"`
	RepositoryID   int64 `json:"repository_id"`
	// Repository is "owner/name" ("" when the payload has none).
	Repository string `json:"repository"`
	// PRNumbers are the pull requests the event names: a pull_request*
	// event's, an issue_comment's on a pull request, a check's (none for a
	// fork's, or a status).
	PRNumbers []int `json:"pr_numbers"`
	// HeadSHA is the pull request's head, or the check's or status's commit
	// (40 hex or "").
	HeadSHA string `json:"head_sha"`
	// BaseRef is a pull_request* event's base branch, else "".
	BaseRef string `json:"base_ref"`
	// Check is set only for check_run, check_suite and status.
	Check *Check `json:"check,omitempty"`
	// ReceivedAt is when the relay stored the delivery.
	ReceivedAt time.Time `json:"received_at"`
}

// Check is a CI event's name and conclusion (names are data).
type Check struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

// Name is the event's name with its action: "pull_request.synchronize",
// "status".
func (e Event) Name() string {
	if e.Action == "" {
		return safe(e.Event)
	}
	return safe(e.Event) + "." + safe(e.Action)
}

// Page is one answer of GET /v1/events: the events after the cursor asked
// for, in journal order, and the cursors to go on from. A cursor is
// "<epoch>:<seq>".
type Page struct {
	// Epoch is the journal's id: a journal created again has another, and
	// every cursor of the old one is a gap.
	Epoch  string  `json:"epoch"`
	Events []Event `json:"events"`
	// Next is the cursor to ask after next.
	Next string `json:"next"`
	// HighWater is the cursor of the newest event the journal stored.
	HighWater string `json:"high_water"`
	// Oldest is the cursor before the oldest event the journal keeps.
	Oldest     string    `json:"oldest"`
	ServerTime time.Time `json:"server_time"`
}

// ErrUnauthorized is the relay refusing the token (HTTP 401).
var ErrUnauthorized = errors.New("the relay refused the token (HTTP 401): check webhooks.token_file against the relay's tokens")

// ErrBusy is the relay's HTTP 429: as many long polls wait as it lets wait
// at once; it asks for another try a second later (Retry-After: 1), which
// says nothing of the stream's health.
var ErrBusy = errors.New("the relay is busy (HTTP 429: too many long polls wait)")

// GapError is the relay's HTTP 410: the cursor is another journal's, or
// older than the oldest event it keeps, so the events after it are lost.
type GapError struct {
	// Epoch and Oldest are the relay's journal and the cursor before its
	// oldest kept event.
	Epoch, Oldest string
}

func (e *GapError) Error() string {
	return "the relay no longer holds the events after the cursor (HTTP 410, oldest kept " + safe(e.Oldest) + ")"
}

// StatusError is an answer of the relay that is neither a page nor a 401 or
// 410: a 5xx (Server) or another code.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("the relay answered HTTP %d", e.Code) }

// Server reports whether the relay itself failed (5xx).
func (e *StatusError) Server() bool { return e.Code >= 500 && e.Code <= 599 }

// Client reads the relay's events.
type Client struct {
	Runner execx.Runner
	// URL is the relay's base URL (https; config.Webhooks validates it).
	URL string
	// TokenFile holds the relay's bearer token (CheckTokenFile).
	TokenFile string
}

// Events reads the page of at most limit events after the cursor after
// ("" = from the oldest kept event; limit 0 = the relay's default), waiting
// up to wait for one when there is none yet. curl gets wait + PageSlack.
func (c *Client) Events(ctx context.Context, after string, limit int, wait time.Duration) (Page, error) {
	token, err := ReadToken(c.TokenFile)
	if err != nil {
		return Page{}, err
	}
	u, err := eventsURL(c.URL, after, limit, wait)
	if err != nil {
		return Page{}, err
	}
	maxTime := wait + PageSlack
	conf, err := curlConfig(u, token, maxTime)
	if err != nil {
		return Page{}, err
	}
	res, err := c.Runner.Run(ctx, execx.Cmd{
		Name: "curl", Args: []string{"--disable", "--config", "-"}, Stdin: conf,
		Timeout: maxTime + 5*time.Second, Label: "relay events", Quiet: true,
	})
	if err != nil {
		return Page{}, fmt.Errorf("relay unreachable: %w", err)
	}
	if res.Truncated {
		return Page{}, errors.New("the relay's page is longer than curl's output may be")
	}
	code, body, err := splitStatus(res.Stdout)
	if err != nil {
		return Page{}, err
	}
	switch code {
	case 200:
		return parsePage(body)
	case 401:
		return Page{}, ErrUnauthorized
	case 429:
		return Page{}, ErrBusy
	case 410:
		var g struct{ Epoch, Oldest string }
		_ = json.Unmarshal(body, &g) // a gap without its body is still a gap
		return Page{}, &GapError{Epoch: g.Epoch, Oldest: g.Oldest}
	}
	return Page{}, &StatusError{Code: code}
}

// eventsURL is base's /v1/events with the query.
func eventsURL(base, after string, limit int, wait time.Duration) (string, error) {
	if !strings.HasPrefix(base, "https://") {
		return "", fmt.Errorf("the relay's URL must be https, got %q", base)
	}
	q := url.Values{"after": {after}, "wait": {strconv.Itoa(int(wait / time.Second))}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(min(limit, MaxLimit)))
	}
	return strings.TrimRight(base, "/") + "/v1/events?" + q.Encode(), nil
}

// curlConfig is what curl reads with --config -: HTTPS only, no redirect
// followed, the time bound, the URL, the headers (the token's among them)
// and the status code after the body on a line of its own.
func curlConfig(u, token string, maxTime time.Duration) ([]byte, error) {
	qu, err := quote(u)
	if err != nil {
		return nil, fmt.Errorf("the relay's URL: %w", err)
	}
	auth, err := quote("Authorization: Bearer " + token)
	if err != nil {
		return nil, errors.New("the relay's token holds a control character") // ReadToken refuses one first
	}
	var b bytes.Buffer
	b.WriteString("silent\nshow-error\nproto = \"=https\"\n")
	fmt.Fprintf(&b, "max-time = %s\n", strconv.FormatFloat(maxTime.Seconds(), 'f', -1, 64))
	fmt.Fprintf(&b, "url = %s\n", qu)
	b.WriteString("header = \"Accept: application/json\"\n")
	b.WriteString("header = " + auth + "\n")
	b.WriteString("write-out = \"\\n%{http_code}\"\n")
	return b.Bytes(), nil
}

// quote double-quotes s for curl's config: a backslash and a quote escaped;
// a control character, which curl's config cannot carry, refused.
func quote(s string) (string, error) {
	if strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", errors.New("holds a control character")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

// splitStatus cuts curl's output into the body and the status code that
// --write-out put on its last line.
func splitStatus(out []byte) (int, []byte, error) {
	body, last, ok := bytes.CutLast(out, []byte("\n"))
	if !ok {
		return 0, nil, errors.New("curl wrote no status code")
	}
	code, err := strconv.Atoi(string(bytes.TrimSpace(last)))
	if err != nil || code < 100 || code > 599 {
		return 0, nil, errors.New("curl wrote no status code after the relay's answer")
	}
	return code, body, nil
}

// parsePage decodes a 200's page, which must name its epoch and next cursor.
func parsePage(body []byte) (Page, error) {
	var p Page
	if err := json.Unmarshal(body, &p); err != nil {
		return Page{}, fmt.Errorf("the relay's page does not decode: %w", err)
	}
	if p.Epoch == "" || p.Next == "" {
		return Page{}, errors.New("the relay's page names no epoch or next cursor")
	}
	return p, nil
}

// MinToken and MaxToken bound a token's length, as the relay's
// relay.CheckToken does (MinTokenLength, MaxTokenLength).
const (
	MinToken = 32
	MaxToken = 512
)

// ReadToken reads the relay's bearer token from path: a file CheckTokenFile
// accepts, holding one token the relay can accept (relay.CheckToken: one
// word of MinToken to MaxToken printable ASCII characters), trimmed of
// surrounding white space. Its errors never quote the token.
func ReadToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("webhooks.token_file is not set")
	}
	if err := CheckTokenFile(path); err != nil {
		return "", fmt.Errorf("webhooks.token_file: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("webhooks.token_file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	switch {
	case token == "":
		return "", fmt.Errorf("webhooks.token_file %s is empty", path)
	case len(token) < MinToken || len(token) > MaxToken:
		return "", fmt.Errorf("webhooks.token_file %s holds %d characters; the relay's tokens have %d to %d", path, len(token), MinToken, MaxToken)
	case strings.ContainsFunc(token, func(r rune) bool { return r < '!' || r > '~' }):
		return "", fmt.Errorf("webhooks.token_file %s holds a character a token cannot have (one word of printable ASCII)", path)
	}
	return token, nil
}

// CheckTokenFile checks the file holding the relay's token: a regular file,
// symlinks followed, that neither its group nor others may read or write
// (0600 or narrower). A missing file is fs.ErrNotExist.
func CheckTokenFile(path string) error {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist: %w", path, fs.ErrNotExist)
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s is open to its group or others (mode %04o): chmod 600 it", path, fi.Mode().Perm())
	}
	return nil
}

// safe keeps what the relay sends that reaches a message (an epoch, a
// cursor, an event name) to letters, digits and ._:- and 100 bytes.
func safe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r) {
			return r
		}
		return -1
	}, s)
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}
