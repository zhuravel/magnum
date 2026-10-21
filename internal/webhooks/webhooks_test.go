package webhooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/relay"
)

const testToken = "relay-s3cr3t-token-0123456789abcdef"

// answering is a fake relay behind a fake curl: it answers status with body,
// and records the commands curl was run with.
func answering(t *testing.T, status, body string) (*Client, *execx.Fake) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "relay.token")
	if err := os.WriteFile(token, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Result: execx.Result{Stdout: []byte(body + "\n" + status)}}}}
	return &Client{Runner: fake, URL: "https://relay.example/", TokenFile: token}, fake
}

const pageJSON = `{"epoch":"ep1","events":[
 {"seq":7,"delivery_id":"d-7","event":"pull_request","action":"synchronize","installation_id":1,"repository_id":2,
  "repository":"talkable/talkable","pr_numbers":[45],"head_sha":"aaaa","base_ref":"master","received_at":"2026-10-20T16:00:05Z"},
 {"seq":8,"delivery_id":"d-8","event":"check_run","action":"completed","installation_id":1,"repository_id":2,
  "repository":"talkable/talkable","pr_numbers":[],"head_sha":"bbbb","check":{"name":"rspec","conclusion":"failure"},
  "received_at":"2026-10-20T16:00:06Z"}],
 "next":"ep1:8","high_water":"ep1:8","oldest":"ep1:0","server_time":"2026-10-20T16:00:07Z"}`

// curl runs with a fixed argv that names no token and no URL: --disable
// first (no ~/.curlrc), then the config on stdin, which is exactly these
// directives: quiet but for errors, HTTPS only, curl's time bound (the wait
// and PageSlack), the URL with the cursor and the long poll's wait, the
// headers (the token's among them) and the status code after the body. Any
// other directive (verbose, trace, a redirect followed) fails it.
func TestEventsSendsTheTokenOnStdinNeverInArgv(t *testing.T) {
	c, fake := answering(t, "200", pageJSON)
	if _, err := c.Events(t.Context(), "ep1:6", 200, 25*time.Second); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsWithPrefix("curl")
	if len(calls) != 1 {
		t.Fatalf("curl calls = %d, want 1", len(calls))
	}
	cmd := calls[0]
	if want := []string{"--disable", "--config", "-"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("argv = %q, want %q", cmd.Args, want)
	}
	if strings.Contains(cmd.String(), testToken) || strings.Contains(execx.Redact(cmd.String()), "Bearer") {
		t.Errorf("the command line %q carries the token", cmd.String())
	}
	want := strings.Join([]string{
		`silent`,
		`show-error`,
		`proto = "=https"`,
		`max-time = 35`,
		`url = "https://relay.example/v1/events?after=ep1%3A6&limit=200&wait=25"`,
		`header = "Accept: application/json"`,
		`header = "Authorization: Bearer ` + testToken + `"`,
		`write-out = "\n%{http_code}"`,
	}, "\n") + "\n"
	if stdin := string(cmd.Stdin); stdin != want {
		t.Errorf("curl's config:\n%s\nwant exactly:\n%s", stdin, want)
	}
	if cmd.Timeout <= 35*time.Second || !cmd.Quiet || cmd.Mutates {
		t.Errorf("cmd = timeout %s, quiet %v, mutates %v: want a read past curl's own bound, reported by its caller", cmd.Timeout, cmd.Quiet, cmd.Mutates)
	}
}

// A page decodes into its events, ids and routing only, and its cursors.
func TestEventsParsesThePage(t *testing.T) {
	c, _ := answering(t, "200", pageJSON)
	p, err := c.Events(t.Context(), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Epoch != "ep1" || p.Next != "ep1:8" || p.HighWater != "ep1:8" || p.Oldest != "ep1:0" || len(p.Events) != 2 {
		t.Fatalf("page = %+v", p)
	}
	pr, ci := p.Events[0], p.Events[1]
	if pr.Seq != 7 || pr.DeliveryID != "d-7" || pr.Name() != "pull_request.synchronize" || pr.Repository != "talkable/talkable" ||
		!slices.Equal(pr.PRNumbers, []int{45}) || pr.HeadSHA != "aaaa" || pr.BaseRef != "master" ||
		!pr.ReceivedAt.Equal(time.Date(2026, 10, 20, 16, 0, 5, 0, time.UTC)) || pr.Check != nil {
		t.Errorf("pull_request event = %+v", pr)
	}
	if ci.Name() != "check_run.completed" || len(ci.PRNumbers) != 0 || ci.HeadSHA != "bbbb" || ci.Check == nil || ci.Check.Name != "rspec" {
		t.Errorf("check_run event = %+v", ci)
	}
	for _, bad := range []string{`{"events":[]}`, `not json`, `{"epoch":"ep1","events":[]}`} {
		c, _ := answering(t, "200", bad)
		if _, err := c.Events(t.Context(), "", 0, 0); err == nil {
			t.Errorf("page %q decoded", bad)
		}
	}
}

// The relay's refusals map to typed errors: 401 the token, 429 a relay busy
// with long polls (try again), 410 a gap (with the journal's epoch and
// oldest cursor), any other code a StatusError that says whether the relay
// itself failed; none of them carries the token.
func TestEventsMapsTheRelaysRefusals(t *testing.T) {
	c, _ := answering(t, "401", `{"error":"unauthorized"}`)
	_, err := c.Events(t.Context(), "ep1:6", 0, 25*time.Second)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401: err = %v", err)
	}
	c, _ = answering(t, "429", `{"error":"too_many_waiting"}`)
	if _, err := c.Events(t.Context(), "ep1:6", 0, 25*time.Second); !errors.Is(err, ErrBusy) {
		t.Errorf("429: err = %v", err)
	}
	c, _ = answering(t, "410", `{"error":"gap","epoch":"ep2","oldest":"ep2:40"}`)
	_, err = c.Events(t.Context(), "ep1:6", 0, 25*time.Second)
	var gap *GapError
	if !errors.As(err, &gap) || gap.Epoch != "ep2" || gap.Oldest != "ep2:40" {
		t.Errorf("410: err = %v", err)
	}
	for code, server := range map[string]bool{"503": true, "500": true, "400": false, "404": false, "302": false} {
		c, _ := answering(t, code, `{"error":"journal"}`)
		_, err := c.Events(t.Context(), "ep1:6", 0, 25*time.Second)
		var st *StatusError
		if !errors.As(err, &st) || st.Server() != server || !strings.Contains(err.Error(), code) {
			t.Errorf("%s: err = %v, want a StatusError (server %v)", code, err, server)
		}
	}
	fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Result: execx.Result{Code: 7, Stderr: []byte("curl: (7) Failed to connect to relay.example port 443")}}}}
	c.Runner = fake
	_, err = c.Events(t.Context(), "ep1:6", 0, 25*time.Second)
	if err == nil || !strings.Contains(err.Error(), "relay unreachable") || !strings.Contains(err.Error(), "Failed to connect") {
		t.Errorf("curl failing: err = %v", err)
	}
	for _, e := range []error{err, ErrUnauthorized, gap, &StatusError{Code: 503}} {
		if e != nil && strings.Contains(e.Error(), testToken) {
			t.Errorf("error %q carries the token", e)
		}
	}
	c.Runner = &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Result: execx.Result{Stdout: []byte(pageJSON)}}}}
	if _, err := c.Events(t.Context(), "", 0, 0); err == nil {
		t.Error("an answer without its status code decoded")
	}
}

// The token file must be a regular file only its owner can open, holding
// one token the relay can accept (relay.CheckToken: one word of 32 to 512
// printable ASCII characters); curl never runs otherwise, and the errors
// name the file, never the token. A quote or a backslash in a token reaches
// the header escaped in curl's config.
func TestReadTokenRefusesWhatItCannotSendSafely(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := ReadToken(write("ok", "  "+testToken+"\n", 0o600)); err != nil || tok != testToken {
		t.Fatalf("ReadToken = %q, %v", tok, err)
	}
	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
		want    string
	}{
		"open":     {testToken, 0o644, "chmod 600"},
		"group":    {testToken, 0o640, "chmod 600"},
		"empty":    {"\n", 0o600, "is empty"},
		"short":    {"short-token", 0o600, "the relay's tokens have 32 to 512"},
		"long":     {strings.Repeat("x", 513), 0o600, "the relay's tokens have 32 to 512"},
		"space":    {"two words-0123456789abcdef0123456789", 0o600, "a character a token cannot have"},
		"newline":  {"tok\nen-0123456789abcdef0123456789", 0o600, "a character a token cannot have"},
		"no ascii": {"tökén-0123456789abcdef0123456789", 0o600, "a character a token cannot have"},
	} {
		_, err := ReadToken(write(name, tc.content, tc.mode))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "webhooks.token_file") {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err != nil && tc.content != "\n" && strings.Contains(err.Error(), tc.content) {
			t.Errorf("%s: the error %q quotes the file's content", name, err)
		}
	}
	if _, err := ReadToken(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: err = %v, want fs.ErrNotExist", err)
	}
	if _, err := ReadToken(""); err == nil {
		t.Error("no token file read")
	}
	c, fake := answering(t, "200", pageJSON)
	c.TokenFile = filepath.Join(dir, "open")
	if _, err := c.Events(t.Context(), "", 0, 0); err == nil || len(fake.Calls) != 0 {
		t.Errorf("an open token file: err %v, %d curl calls", err, len(fake.Calls))
	}

	odd := `q"uote\back-0123456789abcdef0123456789`
	c.TokenFile = write("odd", odd, 0o600)
	if _, err := c.Events(t.Context(), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if stdin := string(fake.Calls[0].Stdin); !strings.Contains(stdin, `header = "Authorization: Bearer q\"uote\\back-0123456789abcdef0123456789"`+"\n") {
		t.Errorf("curl's config carries the token unescaped:\n%s", stdin)
	}

	// The relay's own check agrees on every case.
	for _, tok := range []string{testToken, odd, "short-token", strings.Repeat("x", 512), strings.Repeat("x", 513), "two words-0123456789abcdef0123456789"} {
		_, err := ReadToken(write("cmp", tok, 0o600))
		if got, want := err == nil, relay.CheckToken(tok) == nil; got != want {
			t.Errorf("token of %d characters: ReadToken accepts %v, the relay %v (%v)", len(tok), got, want, err)
		}
	}
}

// pageOf is a page of n status events of talkable/talkable, its cursors
// after the last.
func pageOf(n int) string {
	evs := make([]string, n)
	for i := range evs {
		evs[i] = fmt.Sprintf(`{"seq":%d,"delivery_id":"d-%d","event":"status","repository":"talkable/talkable"}`, i+1, i+1)
	}
	return fmt.Sprintf(`{"epoch":"ep1","events":[%s],"next":"ep1:%d","high_water":"ep1:%d","oldest":"ep1:0"}`, strings.Join(evs, ","), n, n)
}

// A page lists at most the events asked for (the limit, at most MaxLimit,
// or the relay's default of 200 when none is asked): one that lists more is
// refused whole as malformed, never cut, since its next cursor is past the
// events a cut would drop; the error says how many came for how many were
// asked, so the stream's health says what was refused.
func TestEventsRefusesAPageOverTheLimitItAskedFor(t *testing.T) {
	for _, tc := range []struct {
		limit, events int
		want          string // "" = the page is taken
	}{
		{2, 2, ""}, {2, 3, "lists 3 events, more than the 2 asked for"}, {1, 2, "more than the 1 asked for"},
		{0, 200, ""}, {0, 201, "more than the 200 asked for"}, {900, 500, ""}, {900, 501, "more than the 500 asked for"},
	} {
		c, _ := answering(t, "200", pageOf(tc.events))
		p, err := c.Events(t.Context(), "ep1:0", tc.limit, 0)
		switch {
		case tc.want == "" && (err != nil || len(p.Events) != tc.events):
			t.Errorf("limit %d, %d events: %d taken, err %v", tc.limit, tc.events, len(p.Events), err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || len(p.Events) != 0):
			t.Errorf("limit %d, %d events: %d taken, err %v, want %q", tc.limit, tc.events, len(p.Events), err, tc.want)
		}
	}
	if defaultLimit != relay.DefaultLimit || MaxLimit != relay.MaxLimit {
		t.Errorf("the client's limits %d and %d, the relay's %d and %d", defaultLimit, MaxLimit, relay.DefaultLimit, relay.MaxLimit)
	}
}

// A page's epoch and cursors (next, high_water, oldest) are at most 100
// bytes of letters, digits and ._:-, as the relay's own are: a page with
// any other is refused whole as malformed, so the consumer never keeps,
// writes or quotes it; the error names the field, never its value.
// ValidCursor is the same check.
func TestEventsRefusesAPageWithAMalformedCursor(t *testing.T) {
	long := "ep1:" + strings.Repeat("9", 10<<10)
	for _, field := range []string{"epoch", "next", "high_water", "oldest"} {
		for name, bad := range map[string]string{
			"10 KB": long, "101 bytes": strings.Repeat("e", 101), "a newline": "ep1:\n8", "a NUL": "ep1:\x008",
			"an escape": "ep1:\x1b[2J", "a space": "ep1: 8", "a quote": `ep1:"8`, "a slash": "ep1/8", "not ASCII": "ép1:8",
		} {
			fields := map[string]any{"epoch": "ep1", "events": []any{}, "next": "ep1:8", "high_water": "ep1:8", "oldest": "ep1:0"}
			fields[field] = bad
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			c, _ := answering(t, "200", string(body))
			_, err = c.Events(t.Context(), "ep1:6", 10, 0)
			if err == nil || !strings.Contains(err.Error(), "malformed "+field) {
				t.Errorf("%s with %s: err = %v, want a malformed %s", field, name, err, field)
			} else if strings.Contains(err.Error(), bad) {
				t.Errorf("%s with %s: the error quotes the value", field, name)
			}
			if ValidCursor(bad) {
				t.Errorf("ValidCursor accepts %s", name)
			}
		}
	}
	edge := strings.Repeat("e", 96) + ":123"
	c, _ := answering(t, "200", `{"epoch":"`+edge[:96]+`","events":[],"next":"`+edge+`"}`)
	if p, err := c.Events(t.Context(), "ep1:6", 10, 0); err != nil || p.Next != edge || p.HighWater != "" || p.Oldest != "" {
		t.Errorf("a cursor of 100 bytes and no high water or oldest: %+v, %v", p, err)
	}
	if !ValidCursor(edge) || ValidCursor("") {
		t.Error("ValidCursor: a cursor of 100 bytes refused, or an empty one accepted")
	}
}

// An event names at most 100 pull requests, as the relay keeps them
// (relay.Envelope): one that lists more has the rest cut, the first 100
// kept.
func TestEventsCutsAnEventsPullRequestsAt100(t *testing.T) {
	nums := make([]string, 1000)
	for i := range nums {
		nums[i] = fmt.Sprint(i + 1)
	}
	c, _ := answering(t, "200", `{"epoch":"ep1","events":[{"seq":1,"delivery_id":"d-1","event":"check_run","repository":"talkable/talkable",
 "pr_numbers":[`+strings.Join(nums, ",")+`]}],"next":"ep1:1"}`)
	p, err := c.Events(t.Context(), "ep1:0", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Events[0].PRNumbers; len(got) != maxPRNumbers || got[0] != 1 || got[len(got)-1] != maxPRNumbers {
		t.Errorf("pr_numbers = %d of them (%v ... %v), want the first %d", len(got), got[:1], got[len(got)-1:], maxPRNumbers)
	}
	// The relay keeps as many of a check suite's 150.
	var prs []string
	for i := 1; i <= 150; i++ {
		prs = append(prs, fmt.Sprintf(`{"number":%d}`, i))
	}
	ev, err := relay.Envelope("check_suite", []byte(`{"action":"completed","check_suite":{"pull_requests":[`+strings.Join(prs, ",")+`]}}`))
	if err != nil || len(ev.PRNumbers) != maxPRNumbers {
		t.Errorf("the relay keeps %d of 150 pull requests (%v), the client %d", len(ev.PRNumbers), err, maxPRNumbers)
	}
}
