package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// probe401 is real `gh api -H "Authorization: Bearer invalid" /app --include`
// output from gh 2.102.0 (2026-10-03, abridged): the status line ends in LF,
// header lines and the separator in CRLF, the body is GitHub's raw bytes.
// gh exited 1 and printed "gh: Bad credentials (HTTP 401)" on stderr; the
// explicit header won over the user's own gh token (the same call to /user
// answered 401, while `gh api /user` without -H answered 200).
const probe401 = "HTTP/2.0 401 Unauthorized\n" +
	"Access-Control-Allow-Origin: *\r\n" +
	"Access-Control-Expose-Headers: ETag, Link, Location, Retry-After, X-GitHub-OTP\r\n" +
	"Content-Security-Policy: default-src 'none'\r\n" +
	"Content-Type: application/json; charset=utf-8\r\n" +
	"Date: Sat, 03 Oct 2026 13:49:59 GMT\r\n" +
	"Server: github.com\r\n" +
	"Vary: Accept-Encoding, Accept, X-Requested-With\r\n" +
	"X-Github-Media-Type: github.v3; format=json\r\n" +
	"X-Github-Request-Id: CAE7:FE2EA:40597E:404A19:6AC10807\r\n" +
	"\r\n" +
	"{\r\n  \"message\": \"Bad credentials\",\r\n  \"documentation_url\": \"https://docs.github.com/rest\",\r\n  \"status\": \"401\"\r\n}"

const probe401Body = "{\r\n  \"message\": \"Bad credentials\",\r\n  \"documentation_url\": \"https://docs.github.com/rest\",\r\n  \"status\": \"401\"\r\n}"

func newAPIRequest(t *testing.T, ctx context.Context, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, DefaultBaseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestGhTransportArgvHeadersAndStdin(t *testing.T) {
	var got execx.Cmd
	run := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			got = c
			return execx.Result{Stdout: []byte("HTTP/2.0 201 Created\nContent-Type: application/json\r\n\r\n{\"token\":\"x\"}")}, nil
		},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := newAPIRequest(t, ctx, http.MethodPost, "/app/installations/7/access_tokens", strings.NewReader(`{"repositories":["a"]}`))
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.sig")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Accept-Encoding", "gzip") // gh must negotiate encoding itself
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")

	resp, err := (&GhTransport{Run: run, Env: map[string]string{"GH_HOST": "github.com"}}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := []string{
		"api", "--method", "POST", "--include", "--hostname", "github.com", "app/installations/7/access_tokens",
		"-H", "Accept: application/vnd.github+json",
		"-H", "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.e30.sig",
		"-H", "X-Github-Api-Version: 2022-11-28",
		"-H", "X-Multi: one",
		"-H", "X-Multi: two",
		"--input", "-",
	}
	if strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args\n got %q\nwant %q", got.Args, want)
	}
	if string(got.Stdin) != `{"repositories":["a"]}` {
		t.Fatalf("stdin %q", got.Stdin)
	}
	if got.Mutates {
		t.Fatal("gh transport calls must not be Mutates (dry runs mint tokens like the direct path)")
	}
	if got.Timeout <= 0 || got.Timeout > 5*time.Second {
		t.Fatalf("timeout %v, want the request deadline (<= 5s)", got.Timeout)
	}
	for k, v := range map[string]string{"NO_COLOR": "1", "CLICOLOR_FORCE": "", "GH_FORCE_TTY": "", "GH_HOST": "github.com"} {
		if have, ok := got.Env[k]; !ok || have != v {
			t.Errorf("env %s = %q (%v), want %q", k, have, ok, v)
		}
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR"} {
		if _, ok := got.Env[k]; ok {
			t.Errorf("env must keep the user's gh auth, but sets %s", k)
		}
	}
}

func TestGhTransportGetWithQueryHasNoInput(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{Stdout: []byte("HTTP/2.0 200 OK\n\r\n{}")},
	}}}
	req := newAPIRequest(t, context.Background(), http.MethodGet, "/installation/repositories?per_page=100&page=2", nil)
	resp, err := (&GhTransport{Run: run}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(resp.Header) != 0 {
		t.Fatalf("resp %d %v", resp.StatusCode, resp.Header)
	}
	c := run.Calls[0]
	if got := strings.Join(c.Args, " "); got != "api --method GET --include --hostname github.com installation/repositories?per_page=100&page=2" {
		t.Fatalf("args %q", got)
	}
	if c.Stdin != nil {
		t.Fatalf("GET must not send stdin, got %q", c.Stdin)
	}
	if c.Timeout != callTimeout {
		t.Fatalf("timeout without a deadline = %v, want %v", c.Timeout, callTimeout)
	}
}

func TestParseGhInclude(t *testing.T) {
	req := newAPIRequest(t, context.Background(), http.MethodGet, "/app", nil)
	resp, err := parseGhInclude([]byte(probe401), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 || resp.Status != "401 Unauthorized" || resp.Proto != "HTTP/2.0" || resp.ProtoMajor != 2 {
		t.Fatalf("status %d %q %q %d", resp.StatusCode, resp.Status, resp.Proto, resp.ProtoMajor)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept-Encoding, Accept, X-Requested-With" {
		t.Fatalf("vary %q", got)
	}
	if got := resp.Header.Get("X-GitHub-Request-Id"); got != "CAE7:FE2EA:40597E:404A19:6AC10807" {
		t.Fatalf("request id %q", got)
	}
	if len(resp.Header) != 9 {
		t.Fatalf("want 9 headers, got %d: %v", len(resp.Header), resp.Header)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != probe401Body || resp.ContentLength != int64(len(probe401Body)) {
		t.Fatalf("body %q (len %d)", body, resp.ContentLength)
	}
	if resp.Request != req {
		t.Fatal("response must point at its request")
	}

	// Repeated names, a folded continuation line and LF-only endings.
	multi := "HTTP/1.1 204 No Content\nLink: <a>; rel=\"next\"\nLink: <b>; rel=\"last\"\nX-Folded: first\n  second\n\n"
	resp, err = parseGhInclude([]byte(multi), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 || resp.ProtoMinor != 1 {
		t.Fatalf("status %d proto %s", resp.StatusCode, resp.Proto)
	}
	if links := resp.Header.Values("Link"); len(links) != 2 || links[1] != `<b>; rel="last"` {
		t.Fatalf("links %q", links)
	}
	if got := resp.Header.Get("X-Folded"); got != "first second" {
		t.Fatalf("folded %q", got)
	}
	if body, _ := io.ReadAll(resp.Body); len(body) != 0 {
		t.Fatalf("204 body %q", body)
	}

	for _, bad := range []string{"", "gh: not found\n", "HTTP/2.0 abc Nope\n\r\n", "To get started with GitHub CLI\n"} {
		if _, err := parseGhInclude([]byte(bad), req); err == nil {
			t.Errorf("parse %q: want error", bad)
		}
	}
}

func TestGhTransportExitOneStillReturnsResponse(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api"},
		Result: execx.Result{Stdout: []byte(probe401), Stderr: []byte("gh: Bad credentials (HTTP 401)\n"), Code: 1},
	}}}
	client := NewGhClient(run, nil)
	req := newAPIRequest(t, context.Background(), http.MethodGet, "/app", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("an HTTP error must be a response, got %v", err)
	}
	defer resp.Body.Close()
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized || msg.Message != "Bad credentials" {
		t.Fatalf("got %d %q", resp.StatusCode, msg.Message)
	}
}

func TestGhTransportErrorsHideSecrets(t *testing.T) {
	const tok = "ghs_secretSecretSecret123"
	cases := []struct {
		name    string
		rule    execx.Rule
		ctx     func() (context.Context, context.CancelFunc)
		want    string
		wantErr error
	}{
		{
			name: "no gh login",
			rule: execx.Rule{Prefix: []string{"gh"}, Result: execx.Result{
				Stderr: []byte("To get started with GitHub CLI, please run:  gh auth login\n"), Code: 4}},
			want: "gh api GET /installation/repositories: gh exited 4: To get started with GitHub CLI, please run:  gh auth login",
		},
		{
			name: "stderr echoes the token",
			rule: execx.Rule{Prefix: []string{"gh"}, Result: execx.Result{
				Stderr: []byte("bad header Authorization: Bearer " + tok), Code: 1}},
			want: "gh exited 1: bad header Authorization: <redacted>",
		},
		{
			name: "gh missing",
			rule: execx.Rule{Prefix: []string{"gh"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				return execx.Result{Code: -1}, &execx.RunError{Cmd: c, Err: errors.New(`exec: "gh": executable file not found in $PATH`)}
			}},
			want: `gh api GET /installation/repositories: exec: "gh": executable file not found in $PATH`,
		},
		{
			name: "start failure echoing a token",
			rule: execx.Rule{Prefix: []string{"gh"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				return execx.Result{Code: -1}, &execx.RunError{Cmd: c, Err: errors.New("fork/exec /opt/gh: bad Authorization: Bearer " + tok)}
			}},
			want: "gh api GET /installation/repositories: fork/exec /opt/gh: bad Authorization: <redacted>",
		},
		{
			name: "runner timeout with partial output",
			rule: execx.Rule{Prefix: []string{"gh"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				partial := []byte("HTTP/2.0 200 OK\nContent-Type: application/json\r\n\r\n{\"repositor")
				return execx.Result{Stdout: partial, Code: -1}, &execx.RunError{Cmd: c, Err: context.DeadlineExceeded}
			}},
			want:    "gh timed out",
			wantErr: context.DeadlineExceeded,
		},
		{
			name: "cancelled request",
			rule: execx.Rule{Prefix: []string{"gh"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				t.Error("gh must not run for a cancelled request")
				return execx.Result{}, nil
			}},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want:    "context canceled",
			wantErr: context.Canceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			req := newAPIRequest(t, ctx, http.MethodGet, "/installation/repositories", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			_, err := (&GhTransport{Run: &execx.Fake{Rules: []execx.Rule{tc.rule}}}).RoundTrip(req)
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), "--include") {
				t.Fatalf("error leaks argv or the token: %q", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err %v does not wrap %v", err, tc.wantErr)
			}
		})
	}
}

func TestGhTransportRejectsOtherHosts(t *testing.T) {
	run := &execx.Fake{}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:1234/app", nil)
	_, err := (&GhTransport{Run: run}).RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), `transport = "direct"`) {
		t.Fatalf("err %v", err)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("gh ran: %v", run.Calls)
	}
}

// ghBridge answers `gh api --method ...` calls by replaying the request
// (method, endpoint, -H headers, stdin) against the fake GitHub handler and
// printing the answer the way `gh api --include` does, including exit 1 and
// "gh: <message> (HTTP <code>)" on stderr for non-2xx.
func ghBridge(t *testing.T, f *fakeGitHub) execx.Rule {
	return execx.Rule{
		Prefix: []string{"gh", "api", "--method"},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			a := c.Args
			if len(a) < 7 || a[3] != "--include" || a[4] != "--hostname" || a[5] != "github.com" {
				t.Errorf("unexpected gh args %q", a)
				return execx.Result{Code: 2}, nil
			}
			req := httptest.NewRequest(a[2], DefaultBaseURL+"/"+a[6], bytes.NewReader(c.Stdin))
			if tok := c.Env["GH_TOKEN"]; tok != "" {
				req.Header.Set("Authorization", "token "+tok) // what gh sends for GH_TOKEN
			}
			for i := 7; i < len(a); i++ {
				switch a[i] {
				case "-H":
					name, value, _ := strings.Cut(a[i+1], ": ")
					req.Header.Add(name, value)
					i++
				case "--input":
					i++
				}
			}
			rec := httptest.NewRecorder()
			f.serve(rec, req)
			res := rec.Result()
			body, _ := io.ReadAll(res.Body)
			var out bytes.Buffer
			fmt.Fprintf(&out, "HTTP/2.0 %s\n", res.Status)
			names := make([]string, 0, len(res.Header))
			for name := range res.Header {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&out, "%s: %s\r\n", name, strings.Join(res.Header[name], ", "))
			}
			out.WriteString("\r\n")
			out.Write(body)
			result := execx.Result{Stdout: out.Bytes()}
			if res.StatusCode/100 != 2 {
				var msg struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(body, &msg)
				result.Stderr = fmt.Appendf(nil, "gh: %s (HTTP %d)\n", msg.Message, res.StatusCode)
				result.Code = 1
				return result, &execx.ExitError{Cmd: c, Code: 1, Stderr: string(result.Stderr)}
			}
			return result, nil
		},
	}
}

// newGhTransportApp wires an App whose REST calls go through gh (a Fake
// bridged to the fake GitHub) at the real API root.
func newGhTransportApp(t *testing.T, f *fakeGitHub, clock *fakeClock, key string, opts ...AppOption) (*App, *execx.Fake) {
	t.Helper()
	run := &execx.Fake{Rules: []execx.Rule{ghBridge(t, f)}}
	getenv := func(k string) string {
		if k == "MAGNUM_TEST_APP_KEY" {
			return key
		}
		return ""
	}
	opts = append([]AppOption{WithRunner(run)}, opts...)
	return NewApp(f.id, newLayout(t), NewGhClient(run, nil), getenv, clock.Now, opts...), run
}

func TestAppMintsThroughGhTransport(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, run := newGhTransportApp(t, f, clock, pkcs1PEM(testKey(t)))

	tok, err := app.Token(context.Background())
	if err != nil || tok != "ghs_test1" || f.mintCount() != 1 {
		t.Fatalf("token %q err %v mints %d", tok, err, f.mintCount())
	}
	calls := run.CallsWithPrefix("gh", "api", "--method", "POST", "--include", "--hostname", "github.com", "app/installations/105365229/access_tokens")
	if len(calls) != 1 {
		t.Fatalf("calls %v", run.Calls)
	}
	if !strings.Contains(strings.Join(calls[0].Args, "\n"), "Authorization: Bearer eyJ") {
		t.Fatalf("the JWT must travel as an explicit Authorization header: %q", calls[0].Args)
	}
	if line := execx.Redact(calls[0].String()); strings.Contains(line, "eyJ") || !strings.Contains(line, "Authorization: <redacted>") {
		t.Fatalf("exec log line leaks the JWT: %s", line)
	}
}

func TestAppCheckThroughGhTransport(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, run := newGhTransportApp(t, f, clock, pkcs1PEM(testKey(t)), WithRepos("talkable/talkable"))
	run.Rules = append(run.Rules, ghRepoRule(t, "talkable/talkable", app.ConfigDir()))

	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Pass {
		t.Fatalf("want pass:\n%s", r)
	}
	assertLines(t, r,
		"PASS app talkable (id 2700610, owner talkable)",
		"PASS installation 105365229 on talkable (repository selection: selected)",
		"PASS permission pull_requests: write",
		"PASS installation token minted, expires 2026-10-03T13:00:00Z (in 1h0m0s)",
		"PASS installation repositories (2): talkable/talkable, talkable/other",
		"PASS gh api repos/talkable/talkable with GH_CONFIG_DIR="+app.ConfigDir(),
	)
	assertNoSecrets(t, r)
	var paths []string
	for _, c := range run.CallsWithPrefix("gh", "api", "--method") {
		paths = append(paths, c.Args[2]+" "+c.Args[6])
	}
	want := "GET app|GET app/installations/105365229|POST app/installations/105365229/access_tokens|GET installation/repositories?per_page=100&page=1"
	if strings.Join(paths, "|") != want {
		t.Fatalf("REST calls\n got %s\nwant %s", strings.Join(paths, "|"), want)
	}
}

func TestAppCheckThroughGhTransportRejectedJWT(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newGhTransportApp(t, f, clock, pkcs8PEM(t, mustOtherKey(t)))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatalf("a 401 is a FAIL line, not a transport error: %v", err)
	}
	assertLines(t, r,
		"FAIL GitHub rejected the App JWT: GET /app: 401 A JSON web token could not be decoded",
	)
	assertNoSecrets(t, r)
}

func TestAppCheckThroughGhTransportWithoutGhLogin(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, run := newGhTransportApp(t, f, clock, pkcs1PEM(testKey(t)))
	run.Rules = []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{
		Stderr: []byte("To get started with GitHub CLI, please run:  gh auth login\n"), Code: 4}}}
	r, err := app.Check(context.Background())
	if err == nil || r.Pass {
		t.Fatalf("want a transport error, got %v:\n%s", err, r)
	}
	assertLines(t, r,
		`FAIL GET /app: GET /app: Get "https://api.github.com/app": gh api GET /app: gh exited 4: To get started with GitHub CLI, please run:  gh auth login`,
	)
	assertNoSecrets(t, r)
}

func TestGhTransportInstallationTokenTravelsInEnvNotArgv(t *testing.T) {
	const tok = "ghs_installationTokenSecret123"
	for _, scheme := range []string{"Bearer ", "token "} {
		t.Run(strings.TrimSpace(scheme), func(t *testing.T) {
			var got execx.Cmd
			run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
				got = c
				return execx.Result{Stdout: []byte("HTTP/2.0 200 OK\n\r\n{}")}, nil
			}}}}
			req := newAPIRequest(t, context.Background(), http.MethodGet, "/installation/repositories", nil)
			req.Header.Set("Authorization", scheme+tok)
			req.Header.Set("Accept", "application/vnd.github+json")
			if _, err := (&GhTransport{Run: run, Env: map[string]string{"GH_TOKEN": "user-level"}}).RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			if line := got.String(); strings.Contains(line, tok) || strings.Contains(line, "Authorization") {
				t.Fatalf("the installation token (or its header) is in argv: %s", line)
			}
			if got.Env["GH_TOKEN"] != tok {
				t.Fatalf("GH_TOKEN = %q, want the installation token (it wins over the transport's Env)", got.Env["GH_TOKEN"])
			}
			if !strings.Contains(strings.Join(got.Args, " "), "-H Accept: application/vnd.github+json") {
				t.Errorf("other headers still travel as -H: %q", got.Args)
			}
		})
	}
}

func TestGhTransportJWTStaysInArgvNotEnv(t *testing.T) {
	var got execx.Cmd
	run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		got = c
		return execx.Result{Stdout: []byte("HTTP/2.0 200 OK\n\r\n{}")}, nil
	}}}}
	req := newAPIRequest(t, context.Background(), http.MethodGet, "/app", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.sig")
	if _, err := (&GhTransport{Run: run}).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Env["GH_TOKEN"]; ok {
		t.Error("a JWT must not replace gh's own login through GH_TOKEN")
	}
	if !slices.Contains(got.Args, "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.e30.sig") {
		t.Errorf("the JWT must travel as -H: %q", got.Args)
	}
}
